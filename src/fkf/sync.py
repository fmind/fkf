"""Deterministic missing-day planning and bounded source collection."""

from __future__ import annotations

import os
import threading
from collections.abc import Sequence
from concurrent.futures import ThreadPoolExecutor
from contextlib import suppress
from dataclasses import dataclass, field, replace
from datetime import datetime, timedelta, tzinfo
from enum import StrEnum
from typing import Any, Protocol

from fkf.base import Base
from fkf.bodies import (
    BodyManifest,
    body_provider_modified_at,
    cache_body,
    fetch_body,
    load_body_manifest,
    read_cached_body_from_manifest,
    write_body_manifest,
)
from fkf.collection import collect, collect_window
from fkf.config import MAX_COMMAND_TIMEOUT, BodyPolicy, Source
from fkf.documents import (
    Document,
    Window,
    day_window,
    event_document_uri,
    index_document_uri,
    parse_day_in_location,
)
from fkf.errors import CanceledError, OperationalError
from fkf.fields import FIELD_TIME, FIELD_TITLE, FIELD_URL, is_well_known_field
from fkf.process import (
    Cancellation,
    CommandCanceledError,
    CommandFailureError,
    display_argv,
)
from fkf.source_runtime import Pacer, PacingRunner, PolicyRunner, build_auth_command, build_run_command
from fkf.store import Layer
from fkf.timeutil import DurationNS, Instant, format_rfc3339, parse_record_time, parse_rfc3339
from fkf.trust import require_trust


class SyncOutcome(StrEnum):
    """What happened to one planned source unit."""

    WRITTEN = "written"
    SKIPPED_EXISTING = "skipped-existing"
    SKIPPED_FRESH = "skipped-fresh"
    FAILED = "failed"
    PLANNED = "planned"
    AUTH_REQUIRED = "auth-required"


@dataclass(frozen=True, slots=True)
class SyncWindow:
    """Inclusive completed-day bounds selected for one sync."""

    since: str = field(default="", metadata={"json": "since,omitempty"})
    until: str = field(default="", metadata={"json": "until,omitempty"})


@dataclass(frozen=True, slots=True)
class SyncRequest:
    """One collection request."""

    targets: tuple[str, ...] = ()
    days: int = 0
    date: str = ""
    force: bool = False
    dry_run: bool = False
    no_graph: bool = False
    preview: bool = False
    if_due: bool = False

    def __post_init__(self) -> None:
        object.__setattr__(self, "targets", tuple(self.targets))


@dataclass(frozen=True, slots=True, kw_only=True)
class SyncUnit:
    """One source/day result; index units have no date."""

    source: str
    kind: Layer
    date: str = field(default="", metadata={"json": "date,omitempty"})
    uri: str
    outcome: SyncOutcome = SyncOutcome.FAILED
    count: int = field(default=0, metadata={"json": "count,omitempty"})
    command: str = field(default="", metadata={"json": "command,omitempty"})
    error: str = field(default="", metadata={"json": "error,omitempty"})
    elapsed: str = field(default="", metadata={"json": "elapsed,omitempty"})
    attempts: int = field(default=0, metadata={"json": "attempts,omitempty"})
    bodies_cached: int = field(default=0, metadata={"json": "bodies_cached,omitempty"})
    body_failures: int = field(default=0, metadata={"json": "body_failures,omitempty"})
    body_error: str = field(default="", metadata={"json": "body_error,omitempty"})
    body_auth_required: bool = field(default=False, metadata={"json": "body_auth_required,omitempty"})


@dataclass(frozen=True, slots=True)
class PreviewRecord:
    """One compact projected record from a non-persistent preview."""

    uri: str
    source: str
    date: str = ""
    time: str = ""
    title: str = ""
    url: str = ""
    fields: dict[str, tuple[str, ...]] | None = None


@dataclass(frozen=True, slots=True, kw_only=True)
class SyncPreview:
    """At most three validated records from exactly one source invocation."""

    source: str
    kind: Layer
    date: str = field(default="", metadata={"json": "date,omitempty"})
    count: int
    sample: tuple[PreviewRecord, ...] = ()


class Rebuild(Protocol):
    """One injected derived-cache rebuild seam."""

    def __call__(self, base: Base, /) -> object | None: ...


@dataclass(frozen=True, slots=True)
class RebuildHooks:
    """Derived builders in their required dependency order."""

    wiki: Rebuild | None = None
    graph: Rebuild | None = None
    lexical: Rebuild | None = None


@dataclass(slots=True, kw_only=True)
class SyncReport:
    """Complete deterministic result of one sync attempt."""

    base: str
    dry_run: bool = field(default=False, metadata={"json": "dry_run,omitempty"})
    preview: SyncPreview | None = field(default=None, metadata={"json": "preview,omitempty"})
    window: SyncWindow
    units: tuple[SyncUnit, ...] = ()
    written: int = 0
    skipped: int = 0
    failed: int = 0
    auth_required: tuple[str, ...] = field(default=(), metadata={"json": "auth_required,omitempty"})
    records: int = 0
    bodies_cached: int = field(default=0, metadata={"json": "bodies_cached,omitempty"})
    body_failed: int = field(default=0, metadata={"json": "body_failed,omitempty"})
    wiki: object | None = field(default=None, metadata={"json": "-"})
    graph: object | None = field(default=None, metadata={"json": "graph,omitempty"})
    index: object | None = field(default=None, metadata={"json": "index,omitempty"})
    elapsed: str = "0s"
    complete: bool = False
    nothing_due: bool = field(default=False, metadata={"json": "nothing_due,omitempty"})

    def failure_summary(self) -> str:
        """Render safe source diagnostics without provider output."""
        lines: list[str] = []
        for unit in self.units:
            if unit.outcome is not SyncOutcome.FAILED and unit.body_failures == 0:
                continue
            label = f"{unit.source} {unit.date}".rstrip()
            diagnostic = unit.body_error if unit.body_failures else unit.error
            line = f"  {label}: {diagnostic}"
            if unit.command and unit.outcome is SyncOutcome.FAILED:
                line += f"\n    command: {unit.command}"
            lines.append(line)
        return "\n".join(lines)


@dataclass(frozen=True, slots=True)
class SyncPreflight:
    """Read-only answer used before acquiring the caller-owned writer lock."""

    base: str
    window: SyncWindow
    due: bool
    due_sources: tuple[str, ...] = ()
    elapsed: str = "0s"

    def report(self) -> SyncReport:
        """Return the lock-free successful no-op report."""
        return SyncReport(
            base=self.base,
            window=self.window,
            elapsed=self.elapsed,
            complete=True,
            nothing_due=True,
        )


class DerivedRebuildError(OperationalError):
    """Durable documents succeeded but a rebuildable cache did not."""

    def __init__(self, report: SyncReport, error: BaseException) -> None:
        self.report = report
        super().__init__(
            "source documents are complete but derived rebuild failed; run `fkf build` to retry it: " + str(error),
            cause=error,
        )


@dataclass(frozen=True, slots=True)
class _SyncWork:
    source: Source
    unit: SyncUnit | None = None
    dates: tuple[str, ...] = ()


@dataclass(slots=True)
class _AuthResult:
    event: threading.Event
    ready: bool = False
    error: BaseException | None = None


def _format_elapsed(started: datetime, ended: datetime) -> str:
    milliseconds = round((ended - started).total_seconds() * 1000)
    if milliseconds == 0:
        return "0s"
    return f"{milliseconds}ms" if milliseconds < 1000 else f"{milliseconds / 1000:g}s"


def _raise_if_canceled(cancel: Cancellation | None) -> None:
    if cancel is not None and cancel.is_set():
        raise CommandCanceledError("command canceled")


def _zone(now: datetime) -> tzinfo:
    if now.tzinfo is None or now.utcoffset() is None:
        raise ValueError("sync clock must have an explicit timezone")
    return now.tzinfo


def _validate_request(request: SyncRequest) -> None:
    if request.if_due and (request.force or request.dry_run or request.preview):
        raise ValueError("--if-due cannot be combined with --force, --dry-run, or --preview")


def resolve_targets(base: Base, names: Sequence[str], *, allow_disabled: bool) -> tuple[Source, ...]:
    """Resolve explicit targets or the stable enabled-source default."""
    if not names:
        enabled = base.config.enabled_sources()
        if enabled:
            return enabled
        if allow_disabled and base.config.sources:
            return tuple(base.config.sources[name] for name in base.config.source_names())
        raise ValueError(f"no source is enabled in {base.config.path}; set `enabled: true` on the sources you want")

    targets: list[Source] = []
    seen: set[str] = set()
    for name in names:
        if name in seen:
            raise ValueError(f"duplicate source {name!r}; name each sync target once")
        seen.add(name)
        source = base.source(name)
        if not source.enabled and not allow_disabled:
            raise ValueError(
                f"source {name} is disabled; set `sources.{name}.enabled: true` in {base.config.path} to collect it"
            )
        targets.append(source)
    return tuple(targets)


def previous_completed_days(now: datetime, count: int) -> tuple[datetime, ...]:
    """Enumerate existing local calendar labels, excluding today."""
    zone = _zone(now)
    cursor = now.date() - timedelta(days=1)
    days: list[datetime] = []
    while len(days) < count:
        label = cursor.isoformat()
        try:
            days.append(parse_day_in_location(label, zone))
        except ValueError as error:
            if "civil date does not exist" not in str(error):
                raise
        cursor -= timedelta(days=1)
    days.reverse()
    return tuple(days)


def plan_days(base: Base, request: SyncRequest) -> tuple[tuple[datetime, ...], SyncWindow]:
    """Resolve completed local days and their inclusive report window."""
    now = base.now()
    zone = _zone(now)
    if request.date:
        day = parse_day_in_location(request.date, zone)
        if day.date().isoformat() >= now.date().isoformat():
            raise ValueError(f"{request.date} is today or later; fkf collects completed local days only")
        return (day,), SyncWindow(request.date, request.date)

    count = request.days if request.days > 0 else base.config.sync.days
    if count < 1 or count > 366:
        raise ValueError(f"--days is {count}; expected 1..366")
    days = previous_completed_days(now, count)
    return days, SyncWindow(days[0].date().isoformat(), days[-1].date().isoformat())


def _plan_units(targets: Sequence[Source], days: Sequence[datetime]) -> tuple[_SyncWork, ...]:
    dates = tuple(day.date().isoformat() for day in days)
    work: list[_SyncWork] = []
    for source in targets:
        if source.layer is Layer.INDEX:
            work.append(
                _SyncWork(
                    source,
                    SyncUnit(source=source.name, kind=source.layer, uri=index_document_uri(source.name)),
                )
            )
        elif source.window and dates:
            work.append(_SyncWork(source, dates=dates))
        else:
            work.extend(
                _SyncWork(
                    source,
                    SyncUnit(
                        source=source.name,
                        kind=source.layer,
                        date=label,
                        uri=event_document_uri(label, source.name),
                    ),
                )
                for label in dates
            )
    return tuple(work)


def _source_collection_window(source: Source, label: str, now: datetime) -> Window:
    if not label:
        if source.layer is not Layer.INDEX:
            raise ValueError(f"events source {source.name} has no collection date")
        label = now.date().isoformat()
    return day_window(parse_day_in_location(label, _zone(now)))


def _should_skip(base: Base, source: Source, unit: SyncUnit, request: SyncRequest) -> SyncOutcome | None:
    try:
        destination = base.store.resolve(unit.uri)
        destination.stat()
    except FileNotFoundError:
        return None
    except Exception as error:
        raise OperationalError(f"inspect collection destination {unit.uri}: {error}", cause=error) from error
    if request.force:
        return None
    if source.layer is Layer.EVENTS:
        return SyncOutcome.SKIPPED_EXISTING
    try:
        document = base.read_document(unit.uri)
        collected_at = parse_rfc3339(document.collected_at)
    except Exception as error:
        raise OperationalError(
            f"inspect existing index snapshot {unit.uri}: {error}; use --force to replace it", cause=error
        ) from error
    age = Instant.from_datetime(base.now()).unix_nanoseconds - collected_at.unix_nanoseconds
    max_age = source.effective_max_age_hours(base.config.sync.index_max_age_hours) * 3_600_000_000_000
    return SyncOutcome.SKIPPED_FRESH if 0 <= age < max_age else None


def _body_cache_current(
    base: Base,
    manifest: BodyManifest,
    document: Document,
    record: dict[str, Any],
) -> bool:
    uri = document.record_uri(record)
    if uri is None:
        raise OperationalError(f"document {document.uri()} has a record without its declared identity")
    _body, entry, found = read_cached_body_from_manifest(base, manifest, uri)
    return bool(
        found and entry is not None and entry.provider_modified_at == body_provider_modified_at(document, record)
    )


def _event_body_restore_pending(manifest: BodyManifest, source: str) -> bool:
    attempted = manifest.event_attempts.get(source)
    if attempted is not None:
        return not attempted
    return not any(entry.source == source for entry in manifest.entries.values())


def _body_document_due(base: Base, source: Source, uri: str) -> bool:
    """Inspect whether a sync-policy cache needs one bounded repair pass."""
    if source.bodies is not BodyPolicy.SYNC:
        return False
    manifest = load_body_manifest(base)
    if source.layer is Layer.EVENTS:
        return _event_body_restore_pending(manifest, source.name)
    if source.layer is not Layer.INDEX:
        return False
    document = base.read_document(uri)
    return any(not _body_cache_current(base, manifest, document, record) for record in document.records)


def _work_due(base: Base, item: _SyncWork, request: SyncRequest) -> bool:
    if item.unit is not None:
        outcome = _should_skip(base, item.source, item.unit, request)
        return outcome is None or _body_document_due(base, item.source, item.unit.uri)
    for label in item.dates:
        unit = SyncUnit(
            source=item.source.name,
            kind=item.source.layer,
            date=label,
            uri=event_document_uri(label, item.source.name),
        )
        outcome = _should_skip(base, item.source, unit, request)
        if outcome is None or _body_document_due(base, item.source, unit.uri):
            return True
    return False


def _due_sources(base: Base, work: Sequence[_SyncWork], request: SyncRequest) -> tuple[str, ...]:
    return tuple(sorted({item.source.name for item in work if _work_due(base, item, request)}))


def preflight_sync(base: Base, request: SyncRequest) -> SyncPreflight:
    """Inspect due work without executing, writing, or acquiring a lock."""
    _validate_request(request)
    started = base.now()
    targets = resolve_targets(base, request.targets, allow_disabled=False)
    days, window = plan_days(base, request)
    due_sources = _due_sources(base, _plan_units(targets, days), request)
    return SyncPreflight(
        base=os.fspath(base.root),
        window=window,
        due=bool(due_sources),
        due_sources=due_sources,
        elapsed=_format_elapsed(started, base.now()),
    )


class _AuthCache:
    def __init__(self, base: Base) -> None:
        self._base = base
        self._lock = threading.Lock()
        self._results: dict[str, _AuthResult] = {}

    def ready(self, source: Source, cancel: Cancellation | None) -> bool:
        if not source.auth:
            return True
        with self._lock:
            result = self._results.get(source.name)
            owner = result is None
            if result is None:
                result = _AuthResult(threading.Event())
                self._results[source.name] = result
        if owner:
            try:
                result.ready = _run_auth_probe(self._base, source, cancel)
            except BaseException as error:
                result.error = error
            finally:
                result.event.set()
        else:
            while not result.event.wait(0.05):
                _raise_if_canceled(cancel)
        if result.error is not None:
            raise result.error
        return result.ready


def _run_auth_probe(base: Base, source: Source, cancel: Cancellation | None) -> bool:
    if not source.auth:
        return True
    command = build_auth_command(source, base.environment, base.config.sync.timeout)
    try:
        base.runner.run(command, cancel=cancel)
    except CommandFailureError as error:
        if error.provider_exit_code is not None:
            return False
        raise
    return True


def _failed(unit: SyncUnit, error: BaseException, started: datetime, base: Base, *, command: str = "") -> SyncUnit:
    return replace(
        unit,
        outcome=SyncOutcome.FAILED,
        error=str(error),
        command=command or unit.command,
        elapsed=_format_elapsed(started, base.now()),
    )


def _collect_unit(
    base: Base,
    item: _SyncWork,
    request: SyncRequest,
    pacer: Pacer,
    auth: _AuthCache,
    cancel: Cancellation | None,
) -> tuple[SyncUnit, ...]:
    if item.unit is None:
        raise RuntimeError("non-window sync work has no unit")
    unit = item.unit
    started = base.now()
    try:
        window = _source_collection_window(item.source, unit.date, started)
        command = build_run_command(item.source, base.environment, window, base.config.sync.timeout)
        unit = replace(unit, command=display_argv(command.argv))
        outcome = _should_skip(base, item.source, unit, request)
    except Exception as error:
        return (_failed(unit, error, started, base),)
    if outcome is not None:
        return (replace(unit, outcome=outcome),)
    if request.dry_run:
        return (replace(unit, outcome=SyncOutcome.PLANNED),)
    try:
        if not auth.ready(item.source, cancel):
            return (replace(unit, outcome=SyncOutcome.AUTH_REQUIRED, command=""),)
    except CommandCanceledError:
        raise
    except Exception as error:
        return (_failed(unit, error, started, base),)

    runner = PolicyRunner(PacingRunner(base.runner, pacer, item.source), item.source)
    try:
        document = collect(
            runner,
            item.source,
            base.environment,
            window,
            base.config.sync.timeout,
            base.now(),
            cancel=cancel,
        )
        base.write_document(document)
    except CommandCanceledError:
        raise
    except Exception as error:
        failed = _failed(unit, error, started, base)
        return (replace(failed, attempts=runner.attempts if runner.attempts > 1 else 0),)
    return (
        replace(
            unit,
            outcome=SyncOutcome.WRITTEN,
            count=document.count,
            attempts=runner.attempts if runner.attempts > 1 else 0,
            elapsed=_format_elapsed(started, base.now()),
        ),
    )


def contiguous_day_spans(dates: Sequence[str], zone: tzinfo) -> tuple[tuple[str, ...], ...]:
    """Split labels at genuine gaps while treating skipped civil labels as adjacent."""
    if not dates:
        return ()
    previous = day_window(parse_day_in_location(dates[0], zone))
    spans: list[list[str]] = [[dates[0]]]
    for label in dates[1:]:
        current = day_window(parse_day_in_location(label, zone))
        if previous.next != label:
            spans.append([])
        spans[-1].append(label)
        previous = current
    return tuple(tuple(span) for span in spans)


def window_spanning(dates: Sequence[str], zone: tzinfo) -> Window:
    """Build exact DST-safe boundaries around one non-empty contiguous span."""
    if not dates:
        raise ValueError("cannot build a collection window for no dates")
    first = day_window(parse_day_in_location(dates[0], zone))
    last = day_window(parse_day_in_location(dates[-1], zone))
    return Window(date=first.date, next=last.next, start=first.start, end=last.end)


def _span_source(source: Source, fallback: DurationNS, days: int) -> Source:
    effective = source.timeout if source.timeout > 0 else fallback
    scaled = min(int(MAX_COMMAND_TIMEOUT), int(effective) * max(1, days))
    return replace(source, timeout=DurationNS(scaled))


def _range_units(source: Source, dates: Sequence[str]) -> list[SyncUnit]:
    return [
        SyncUnit(
            source=source.name,
            kind=source.layer,
            date=label,
            uri=event_document_uri(label, source.name),
        )
        for label in dates
    ]


def _collect_range_span(
    base: Base,
    source: Source,
    dates: tuple[str, ...],
    request: SyncRequest,
    pacer: Pacer,
    started: datetime,
    cancel: Cancellation | None,
) -> tuple[SyncUnit, ...]:
    empty_units = _range_units(source, dates)
    try:
        range_window = window_spanning(dates, _zone(started))
        span_source = _span_source(source, base.config.sync.timeout, len(dates))
        command = build_run_command(span_source, base.environment, range_window, base.config.sync.timeout)
        displayed = display_argv(command.argv)
    except Exception as error:
        return tuple(_failed(unit, error, started, base) for unit in empty_units)
    if request.dry_run:
        return tuple(replace(unit, outcome=SyncOutcome.PLANNED, command=displayed) for unit in empty_units)

    runner = PolicyRunner(PacingRunner(base.runner, pacer, span_source), span_source)
    try:
        documents = collect_window(
            runner,
            span_source,
            base.environment,
            range_window,
            dates,
            _zone(started),
            base.config.sync.timeout,
            base.now(),
            cancel=cancel,
        )
    except CommandCanceledError:
        raise
    except Exception as error:
        attempts = runner.attempts if runner.attempts > 1 else 0
        return tuple(
            replace(_failed(unit, error, started, base, command=displayed), attempts=attempts) for unit in empty_units
        )

    units: list[SyncUnit] = []
    attempts = runner.attempts if runner.attempts > 1 else 0
    for label in dates:
        document = documents[label]
        unit = SyncUnit(source=source.name, kind=source.layer, date=label, uri=document.uri(), command=displayed)
        try:
            base.write_document(document)
        except Exception as error:
            units.append(replace(_failed(unit, error, started, base), attempts=attempts))
            continue
        units.append(
            replace(
                unit,
                outcome=SyncOutcome.WRITTEN,
                count=document.count,
                attempts=attempts,
                elapsed=_format_elapsed(started, base.now()),
            )
        )
    return tuple(units)


def _collect_range(
    base: Base,
    item: _SyncWork,
    request: SyncRequest,
    pacer: Pacer,
    auth: _AuthCache,
    cancel: Cancellation | None,
) -> tuple[SyncUnit, ...]:
    started = base.now()
    units: list[SyncUnit] = []
    needed: list[str] = []
    for unit in _range_units(item.source, item.dates):
        try:
            outcome = _should_skip(base, item.source, unit, request)
        except Exception as error:
            units.append(_failed(unit, error, started, base))
            continue
        if outcome is None:
            needed.append(unit.date)
        else:
            units.append(replace(unit, outcome=outcome))
    if not needed:
        return tuple(units)
    if not request.dry_run:
        try:
            ready = auth.ready(item.source, cancel)
        except CommandCanceledError:
            raise
        except Exception as error:
            units.extend(_failed(unit, error, started, base) for unit in _range_units(item.source, needed))
            return tuple(units)
        if not ready:
            units.extend(replace(unit, outcome=SyncOutcome.AUTH_REQUIRED) for unit in _range_units(item.source, needed))
            return tuple(units)
    try:
        spans = contiguous_day_spans(needed, _zone(started))
    except Exception as error:
        units.extend(_failed(unit, error, started, base) for unit in _range_units(item.source, needed))
        return tuple(units)
    for span in spans:
        units.extend(_collect_range_span(base, item.source, span, request, pacer, started, cancel))
    return tuple(units)


def _source_reads_base(source: Source) -> bool:
    return any("{{base}}" in argument for argument in source.run)


def _run_phase(
    base: Base,
    work: Sequence[_SyncWork],
    request: SyncRequest,
    pacer: Pacer,
    auth: _AuthCache,
    cancel: Cancellation | None,
) -> list[SyncUnit]:
    if not work:
        return []
    _raise_if_canceled(cancel)
    workers = min(4, max(1, base.config.sync.concurrency), len(work))

    def run(item: _SyncWork) -> tuple[SyncUnit, ...]:
        _raise_if_canceled(cancel)
        if item.dates:
            return _collect_range(base, item, request, pacer, auth, cancel)
        return _collect_unit(base, item, request, pacer, auth, cancel)

    with ThreadPoolExecutor(max_workers=workers, thread_name_prefix="fkf-sync") as pool:
        grouped = pool.map(run, work, buffersize=workers)
        return [unit for group in grouped for unit in group]


def _run_units(
    base: Base,
    work: Sequence[_SyncWork],
    request: SyncRequest,
    cancel: Cancellation | None,
) -> list[SyncUnit]:
    # A single run-owned pacer coordinates every retry and both dependency phases.
    pacer = Pacer()
    auth = _AuthCache(base)
    ordinary = tuple(item for item in work if not _source_reads_base(item.source))
    base_readers = tuple(item for item in work if _source_reads_base(item.source))
    units = _run_phase(base, ordinary, request, pacer, auth, cancel)
    units.extend(_run_phase(base, base_readers, request, pacer, auth, cancel))
    _raise_if_canceled(cancel)
    return units


def _mark_event_body_attempt(base: Base, source: str, attempted: bool) -> None:
    manifest = load_body_manifest(base)
    manifest.event_attempts[source] = attempted
    write_body_manifest(base, manifest)


def _missing_cached_body_records(base: Base, document: Document) -> tuple[dict[str, Any], ...]:
    manifest = load_body_manifest(base)
    return tuple(record for record in document.records if not _body_cache_current(base, manifest, document, record))


def _event_body_restore_candidates(base: Base, units: Sequence[SyncUnit]) -> dict[str, str]:
    manifest = load_body_manifest(base)
    candidates: dict[str, str] = {}
    for unit in units:
        if unit.kind is not Layer.EVENTS or unit.outcome is not SyncOutcome.SKIPPED_EXISTING:
            continue
        source = base.source(unit.source)
        if source.bodies is not BodyPolicy.SYNC or not _event_body_restore_pending(manifest, source.name):
            continue
        current = candidates.get(source.name, "")
        if unit.uri > current:
            candidates[source.name] = unit.uri
    return candidates


def _cache_missing_bodies(
    base: Base,
    document: Document,
    records: Sequence[dict[str, Any]],
    unit: SyncUnit,
    cancel: Cancellation | None,
) -> SyncUnit:
    cached = 0
    failures = 0
    diagnostics: list[str] = []
    for record in records:
        _raise_if_canceled(cancel)
        uri = document.record_uri(record)
        if uri is None:
            raise OperationalError(f"document {document.uri()} has a record without its declared identity")
        try:
            body = fetch_body(base, document, record, cancel=cancel)
            cache_body(base, document, record, uri, body)
        except CommandCanceledError:
            raise
        except Exception as error:
            failures += 1
            if len(diagnostics) < 3:
                diagnostics.append(f"{uri}: {error}")
            continue
        cached += 1
    diagnostic = ""
    if failures:
        diagnostic = f"{failures} body fetch(es) failed: {'; '.join(diagnostics)}"
    return replace(unit, bodies_cached=cached, body_failures=failures, body_error=diagnostic)


def _sync_unit_bodies(
    base: Base,
    auth: _AuthCache,
    unit: SyncUnit,
    *,
    restore_event: bool,
    cancel: Cancellation | None,
) -> SyncUnit:
    # Newly written evidence, a fresh index snapshot, and the one selected event restore are
    # the only units allowed to trigger provider body reads.
    if unit.outcome not in {SyncOutcome.WRITTEN, SyncOutcome.SKIPPED_FRESH} and not restore_event:
        return unit
    source = base.source(unit.source)
    if source.bodies is not BodyPolicy.SYNC:
        return unit
    document = base.read_document(unit.uri)
    missing = _missing_cached_body_records(base, document)
    attempt_event = source.layer is Layer.EVENTS and (unit.outcome is SyncOutcome.WRITTEN or restore_event)
    if attempt_event:
        # False arms one later retry for a newly collected document. A restore attempt closes
        # the marker even after failure so vanished historical objects never become perpetual.
        _mark_event_body_attempt(base, source.name, False)
    if not missing:
        if attempt_event:
            _mark_event_body_attempt(base, source.name, True)
        return unit
    if not auth.ready(source, cancel):
        return replace(unit, body_auth_required=True)
    updated = _cache_missing_bodies(base, document, missing, unit, cancel)
    if attempt_event and (updated.body_failures == 0 or restore_event):
        _mark_event_body_attempt(base, source.name, True)
    return updated


def _sync_requested_bodies(
    base: Base,
    units: Sequence[SyncUnit],
    cancel: Cancellation | None,
) -> tuple[SyncUnit, ...]:
    auth = _AuthCache(base)
    restore = _event_body_restore_candidates(base, units)
    updated: list[SyncUnit] = []
    for unit in units:
        _raise_if_canceled(cancel)
        updated.append(
            _sync_unit_bodies(
                base,
                auth,
                unit,
                restore_event=restore.get(unit.source) == unit.uri,
                cancel=cancel,
            )
        )
    return tuple(updated)


def _project_preview(document: Document, record: dict[str, Any]) -> PreviewRecord:
    uri = document.record_uri(record) or document.uri()
    time_value = ""
    if raw := document.fields.eval_string(FIELD_TIME, record):
        with suppress(ValueError):
            time_value = format_rfc3339(parse_record_time(raw))
    projected: dict[str, tuple[str, ...]] = {}
    for name in document.fields.names():
        if is_well_known_field(name):
            continue
        values = tuple(document.fields.eval_strings(name, record))
        if values:
            projected[name] = values
    return PreviewRecord(
        uri=uri,
        source=document.source,
        date=document.date,
        time=time_value,
        title=document.fields.eval_string(FIELD_TITLE, record) or "",
        url=document.fields.eval_string(FIELD_URL, record) or "",
        fields=projected or None,
    )


def _preview_sync(base: Base, request: SyncRequest, cancel: Cancellation | None) -> SyncReport:
    if len(request.targets) != 1:
        raise ValueError("--preview requires exactly one source")
    if request.days != 0 or request.force or request.dry_run or request.no_graph:
        raise ValueError("--preview may be combined only with --date")
    source = resolve_targets(base, request.targets, allow_disabled=False)[0]
    started = base.now()
    report_window = SyncWindow()
    label = ""
    if source.layer is Layer.EVENTS:
        label = request.date or previous_completed_days(started, 1)[0].date().isoformat()
        _days, report_window = plan_days(base, SyncRequest(date=label))
    elif request.date:
        raise ValueError("--date applies only to an events source")
    window = _source_collection_window(source, label, started)
    if not _run_auth_probe(base, source, cancel):
        uri = (
            event_document_uri(label, source.name) if source.layer is Layer.EVENTS else index_document_uri(source.name)
        )
        return SyncReport(
            base=os.fspath(base.root),
            window=report_window,
            units=(
                SyncUnit(
                    source=source.name,
                    kind=source.layer,
                    date=label,
                    uri=uri,
                    outcome=SyncOutcome.AUTH_REQUIRED,
                ),
            ),
            auth_required=(source.name,),
            elapsed=_format_elapsed(started, base.now()),
            complete=True,
        )
    runner = PolicyRunner(PacingRunner(base.runner, Pacer(), source), source)
    document = collect(
        runner,
        source,
        base.environment,
        window,
        base.config.sync.timeout,
        base.now(),
        cancel=cancel,
    )
    preview = SyncPreview(
        source=source.name,
        kind=source.layer,
        date=label,
        count=document.count,
        sample=tuple(_project_preview(document, record) for record in document.records[:3]),
    )
    return SyncReport(
        base=os.fspath(base.root),
        window=report_window,
        preview=preview,
        records=document.count,
        elapsed=_format_elapsed(started, base.now()),
        complete=True,
    )


def _summarize(base: Base, request: SyncRequest, window: SyncWindow, units: Sequence[SyncUnit]) -> SyncReport:
    ordered = tuple(sorted(units, key=lambda unit: (unit.date, unit.source)))
    written = sum(unit.outcome is SyncOutcome.WRITTEN for unit in ordered)
    skipped = sum(unit.outcome in {SyncOutcome.SKIPPED_EXISTING, SyncOutcome.SKIPPED_FRESH} for unit in ordered)
    failed = sum(unit.outcome is SyncOutcome.FAILED for unit in ordered)
    auth_required = tuple(
        sorted(
            {unit.source for unit in ordered if unit.outcome is SyncOutcome.AUTH_REQUIRED or unit.body_auth_required}
        )
    )
    records = sum(unit.count for unit in ordered if unit.outcome is SyncOutcome.WRITTEN)
    bodies_cached = sum(unit.bodies_cached for unit in ordered)
    body_failed = sum(unit.body_failures for unit in ordered)
    nothing_due = bool(
        request.if_due and not written and not failed and not bodies_cached and not body_failed and not auth_required
    )
    return SyncReport(
        base=os.fspath(base.root),
        window=window,
        units=() if nothing_due else ordered,
        dry_run=request.dry_run,
        written=written,
        skipped=0 if nothing_due else skipped,
        failed=failed,
        auth_required=auth_required,
        records=records,
        bodies_cached=bodies_cached,
        body_failed=body_failed,
        complete=failed == 0 and body_failed == 0,
        nothing_due=nothing_due,
    )


def _rebuild(
    base: Base,
    report: SyncReport,
    request: SyncRequest,
    hooks: RebuildHooks,
    cancel: Cancellation | None,
) -> None:
    if report.written > 0:
        _raise_if_canceled(cancel)
        if hooks.wiki is not None:
            report.wiki = hooks.wiki(base)
        _raise_if_canceled(cancel)
        if not request.no_graph and hooks.graph is not None:
            report.graph = hooks.graph(base)
        _raise_if_canceled(cancel)
    if (report.written > 0 or report.bodies_cached > 0) and hooks.lexical is not None:
        report.index = hooks.lexical(base)
    _raise_if_canceled(cancel)


def sync(
    base: Base,
    request: SyncRequest,
    *,
    rebuild: RebuildHooks | None = None,
    cancel: Cancellation | None = None,
) -> SyncReport:
    """Collect due evidence; the mutating caller must hold ``WriterLock``.

    Dry runs, previews, and preflight inspection are deliberately lock-free. The service does
    not acquire a second lock because command handlers compose several mutating services under
    one physical-base lock.
    """
    _validate_request(request)
    _raise_if_canceled(cancel)
    # Dry runs only disclose the plan. Every other path can execute trusted provider
    # argv or persist its output, so reject stale approval before either can happen.
    if not request.dry_run:
        require_trust(base.config, cancel=cancel)
    if request.preview:
        return _preview_sync(base, request, cancel)
    targets = resolve_targets(base, request.targets, allow_disabled=request.dry_run)
    days, window = plan_days(base, request)
    work = _plan_units(targets, days)
    if request.if_due and not _due_sources(base, work, request):
        return SyncPreflight(os.fspath(base.root), window, False).report()
    started = base.now()
    units: Sequence[SyncUnit] = _run_units(base, work, request, cancel)
    if not request.dry_run:
        units = _sync_requested_bodies(base, units, cancel)
    report = _summarize(base, request, window, units)
    if not request.dry_run and (report.written > 0 or report.bodies_cached > 0) and rebuild is not None:
        try:
            _rebuild(base, report, request, rebuild, cancel)
        except CanceledError:
            raise
        except Exception as error:
            report.elapsed = _format_elapsed(started, base.now())
            raise DerivedRebuildError(report, error) from error
    report.elapsed = _format_elapsed(started, base.now())
    return report


__all__ = [
    "DerivedRebuildError",
    "PreviewRecord",
    "RebuildHooks",
    "SyncOutcome",
    "SyncPreflight",
    "SyncPreview",
    "SyncReport",
    "SyncRequest",
    "SyncUnit",
    "SyncWindow",
    "contiguous_day_spans",
    "plan_days",
    "preflight_sync",
    "previous_completed_days",
    "resolve_targets",
    "sync",
    "window_spanning",
]
