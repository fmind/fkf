"""Read-only base health, readiness, freshness, and integrity reporting."""

from __future__ import annotations

import fnmatch
import hashlib
import os
import posixpath
import stat
import struct
from collections.abc import Sequence
from dataclasses import dataclass, field, replace
from datetime import UTC, date, datetime
from pathlib import Path, PurePosixPath
from typing import TYPE_CHECKING, Final, Protocol, cast

from fkf.assets import BUNDLED_SKILLS, HOOK_SCRIPT, shipped_helpers, skill_digest
from fkf.auth import probe_source_auth
from fkf.base import Base
from fkf.documents import (
    Document,
    day_window,
    decode_document,
    event_document_uri,
    index_document_uri,
    parse_day,
    verify_document,
)
from fkf.errors import CanceledError
from fkf.graph import DerivedGraphMissingError, GraphSummary, summarize_graph
from fkf.harness import HarnessRegistration, inspect_harnesses
from fkf.io import read_file_limited
from fkf.learned import cited_task_traces, learned_bullets
from fkf.lexical import (
    LEXICAL_INDEX_FALLBACK_CORRUPT,
    LEXICAL_INDEX_FALLBACK_MISSING,
    LEXICAL_INDEX_FALLBACK_STALE,
    LEXICAL_INDEX_PATH,
    LexicalIndexUse,
    lexical_index_health,
)
from fkf.listings import TaskTrace, list_tasks
from fkf.markdown import Page, Severity
from fkf.marked_block import MarkedBlockMarkers, parse_marked_block_region
from fkf.pages import PageFilter, list_pages
from fkf.process import Cancellation, Command, SubprocessRunner, check_cancel, sanitize_path
from fkf.source_runtime import Environment
from fkf.store import (
    BASE_CLIENTS_DIR,
    BASE_DIR_MODE,
    BASE_FILE_MODE,
    BASE_SKILLS_DIR,
    BASE_SOURCES_DIR,
    BASE_TESTS_DIR,
    GRAPH_DST_FILE,
    GRAPH_FILE,
    GRAPH_META_FILE,
    GRAPH_OFFSETS_FILE,
    LAYERS,
    LOCAL_CONFIG_NAME,
    MAX_CONTROL_FILE_BYTES,
    MAX_SOURCE_DOCUMENT_BYTES,
    Layer,
    UnsafePathError,
    validate_within_root,
)
from fkf.sync import previous_completed_days
from fkf.timeutil import DurationNS, Instant, format_rfc3339, parse_rfc3339
from fkf.trust import TrustState, read_trust

if TYPE_CHECKING:
    from fkf.scan import ScanGuard

QUIET_WINDOW: Final = 14
QUIET_ARMING_DAYS: Final = 7
QUIET_RATIO_PERCENT: Final = 20
GIT_TIMEOUT: Final = DurationNS(15_000_000_000)


_MANAGED_BEGIN: Final = "# >>> fkf managed block — do not edit between the markers"
_MANAGED_BEGIN_PREFIX: Final = "# >>> fkf managed block"
_MANAGED_END: Final = "# <<< fkf managed block"
_MANAGED_END_PREFIX: Final = "# <<< fkf managed block"
_COLLECTED_LAYERS: Final = ("events/", "index/")
_CONFLICT_MARKERS: Final = (b"<<<<<<< ", b"=======\n", b">>>>>>> ")

_CREDENTIAL_PATTERNS: Final = (
    ".env",
    ".env.*",
    "*.env",
    ".envrc",
    "*.key",
    "*.pem",
    "*.p12",
    "*.pfx",
    "*.p8",
    "*.asc",
    "*.jks",
    "*.keystore",
    "*.kdbx",
    "id_rsa",
    "id_dsa",
    "id_ecdsa",
    "id_ecdsa_sk",
    "id_ed25519",
    "id_ed25519_sk",
    "*.ppk",
    ".ssh/",
    "credentials.json",
    "token.json",
    "application_default_credentials.json",
    "service-account*.json",
    ".aws/",
    ".netrc",
    "_netrc",
    ".git-credentials",
    ".npmrc",
    ".pypirc",
    "PRIVATE.md",
    "feeds.private.opml",
    LOCAL_CONFIG_NAME,
)


class DocumentReader(Protocol):
    """Injected bounded durable-document read seam used by snapshot tests."""

    def __call__(self, uri: str, limit: int, /) -> bytes: ...


class HarnessInspector(Protocol):
    """User-scope harness inspection seam; offline reports never invoke it."""

    def __call__(
        self,
        root: Path,
        *,
        executable: Path | str = "",
        cancel: Cancellation | None = None,
    ) -> Sequence[HarnessRegistration]: ...


class LexicalHealth(Protocol):
    """Small cache-health seam shared with the lexical implementation."""

    def __call__(self, base: Base, /) -> LexicalIndexUse: ...


@dataclass(frozen=True, slots=True)
class Finding:
    """One actionable health or integrity issue."""

    check: str
    severity: Severity
    message: str
    paths: tuple[str, ...] = field(default=(), metadata={"json": "paths,omitempty"})
    fix: str = field(default="", metadata={"json": "fix,omitempty"})


@dataclass(frozen=True, slots=True)
class LayerOverview:
    """One durable or authored layer summary."""

    layer: Layer
    enabled: bool
    uri: str
    count: int = 0
    unit: str = ""
    since: str = field(default="", metadata={"json": "since,omitempty"})
    until: str = field(default="", metadata={"json": "until,omitempty"})
    note: str = field(default="", metadata={"json": "note,omitempty"})


@dataclass(frozen=True, slots=True)
class RequirementStatus:
    """One executable explicitly declared by a source."""

    name: str
    on_path: bool


@dataclass(frozen=True, slots=True)
class SourceStatus:
    """One source's readiness, freshness, and recent volume."""

    name: str
    enabled: bool
    kind: Layer
    requires: tuple[RequirementStatus, ...] = field(default=(), metadata={"json": "requires,omitempty"})
    install: str = field(default="", metadata={"json": "install,omitempty"})
    test: RequirementStatus | None = field(default=None, metadata={"json": "test,omitempty"})
    body: bool = False
    auth: bool = False
    auth_required: bool = field(default=False, metadata={"json": "auth_required,omitempty"})
    undeclared: bool = field(default=False, metadata={"json": "undeclared,omitempty"})
    last_date: str = field(default="", metadata={"json": "last_date,omitempty"})
    last_collected_at: str = field(default="", metadata={"json": "last_collected_at,omitempty"})
    lag_hours: int = field(default=0, metadata={"json": "lag_hours,omitempty"})
    stale: bool = field(default=False, metadata={"json": "stale,omitempty"})
    missing_dates: tuple[str, ...] = field(default=(), metadata={"json": "missing_dates,omitempty"})
    last_count: int = field(default=0, metadata={"json": "last_count,omitempty"})
    median: int = field(default=0, metadata={"json": "median,omitempty"})
    days: int = field(default=0, metadata={"json": "days,omitempty"})
    quiet: bool = field(default=False, metadata={"json": "quiet,omitempty"})
    quiet_reason: str = field(default="", metadata={"json": "quiet_reason,omitempty"})
    last_collected: Instant | None = field(default=None, repr=False, compare=False, metadata={"json": "-"})


@dataclass(frozen=True, slots=True)
class StatusRequest:
    """Options and injectable read-only boundaries for one report."""

    max_age_hours: int = 0
    executable: str = ""
    evaluation_time: datetime | None = field(default=None, repr=False, compare=False, metadata={"json": "-"})
    live: bool = False
    skip_git_audit: bool = False
    inspect_harnesses: HarnessInspector | None = field(
        default=None,
        repr=False,
        compare=False,
        metadata={"json": "-"},
    )
    document_reader: DocumentReader | None = field(
        default=None,
        repr=False,
        compare=False,
        metadata={"json": "-"},
    )
    lexical_health: LexicalHealth | None = field(
        default=None,
        repr=False,
        compare=False,
        metadata={"json": "-"},
    )
    scan: ScanGuard | None = field(default=None, repr=False, compare=False, metadata={"json": "-"})


@dataclass(frozen=True, slots=True)
class Status:
    """Unified read-only status of one FKF base."""

    base: str
    name: str
    base_origin: str
    trust: TrustState
    versioned: bool
    track_collected: bool
    layers: tuple[LayerOverview, ...]
    sources: tuple[SourceStatus, ...]
    harnesses: tuple[HarnessRegistration, ...] = field(default=(), metadata={"json": "harnesses,omitempty"})
    auth_required: tuple[str, ...] = field(default=(), metadata={"json": "auth_required,omitempty"})
    findings: tuple[Finding, ...] = ()
    graph: GraphSummary | None = field(default=None, metadata={"json": "graph,omitempty"})
    unharvested: int = field(default=0, metadata={"json": "unharvested,omitempty"})
    enabled: int = 0
    missing_requirements: int = field(default=0, metadata={"json": "missing_requirements"})
    missing_test_hooks: int = field(default=0, metadata={"json": "missing_test_hooks"})
    quiet: int = 0
    errors: int = 0
    warnings: int = 0
    ok: bool = False
    stale: bool = False
    last_sync: str = field(default="", metadata={"json": "last_sync,omitempty"})
    stale_days: int = field(default=0, metadata={"json": "stale_days,omitempty"})
    max_age_hours: int = field(default=0, metadata={"json": "max_age_hours,omitempty"})
    next: tuple[str, ...] = ()
    # Reuse the verified narrative inventory in composed offline views. Never serialize page bodies here.
    task_pages: tuple[Page, ...] = field(default=(), repr=False, compare=False, metadata={"json": "-"})
    project_pages: tuple[Page, ...] = field(default=(), repr=False, compare=False, metadata={"json": "-"})


@dataclass(frozen=True, slots=True)
class _StatusDocumentEntry:
    uri: str
    data: bytes = b""
    document: Document | None = None
    error: BaseException | None = None


@dataclass(frozen=True, slots=True)
class _DayVolume:
    date: str
    count: int
    last_collected: Instant


@dataclass(frozen=True, slots=True)
class _KnowledgeInventory:
    tasks: tuple[TaskTrace, ...]
    projects: tuple[Page, ...]
    wiki: tuple[Page, ...]


class _StatusDocuments:
    """One durable-document generation reused by every status concern."""

    def __init__(self, entries: Sequence[_StatusDocumentEntry]) -> None:
        self.entries = tuple(entries)
        self.by_uri = {entry.uri: entry for entry in entries}

    def volume_history(self) -> dict[str, list[_DayVolume]]:
        history: dict[str, list[_DayVolume]] = {}
        for entry in self.entries:
            document = entry.document
            if entry.error is not None or document is None or not entry.uri.startswith("events/"):
                continue
            boundary = _event_collection_boundary(document)
            history.setdefault(document.source, []).append(_DayVolume(document.date, document.count, boundary))
        for days in history.values():
            days.sort(key=lambda item: item.date)
        return history

    def index_names(self) -> tuple[str, ...]:
        return tuple(
            PurePosixPath(entry.uri).name.removesuffix(".json")
            for entry in self.entries
            if entry.uri.startswith("index/")
        )

    def index_status(self, name: str) -> tuple[int, str, Instant] | None:
        entry = self.by_uri.get(index_document_uri(name))
        if entry is None or entry.error is not None or entry.document is None:
            return None
        collected = parse_rfc3339(entry.document.collected_at)
        local_day = collected.to_datetime().astimezone().date().isoformat()
        return entry.document.count, local_day, collected

    def index_overview(self, now: Instant) -> tuple[int, str]:
        ages: list[int] = []
        valid = 0
        for entry in self.entries:
            document = entry.document
            if entry.error is not None or document is None or not entry.uri.startswith("index/"):
                continue
            collected = parse_rfc3339(document.collected_at)
            valid += 1
            ages.append(max(0, (now.unix_nanoseconds - collected.unix_nanoseconds) // 3_600_000_000_000))
        oldest = max(ages, default=0)
        return valid, f"oldest refreshed {oldest}h ago" if oldest else ""

    def conflicted(self) -> tuple[str, ...]:
        return tuple(entry.uri for entry in self.entries if any(marker in entry.data for marker in _CONFLICT_MARKERS))


def _aware_now(value: datetime) -> datetime:
    return value.replace(tzinfo=UTC) if value.tzinfo is None else value


def _instant(value: datetime) -> Instant:
    return Instant.from_datetime(_aware_now(value))


def _event_collection_boundary(document: Document) -> Instant:
    if document.window_end:
        return parse_rfc3339(document.window_end)
    return parse_rfc3339(day_window(parse_day(document.date)).end)


def _load_status_documents(
    base: Base,
    reader: DocumentReader,
    cancel: Cancellation | None,
    scan: ScanGuard | None,
) -> _StatusDocuments:
    uris: list[str] = []
    if base.store.enabled(Layer.EVENTS):
        for day in base.event_dates(scan=scan):
            uris.extend(event_document_uri(day, name) for name in base.day_documents(day, scan=scan))
    if base.store.enabled(Layer.INDEX):
        uris.extend(index_document_uri(name) for name in base.index_documents(scan=scan))

    entries: list[_StatusDocumentEntry] = []
    for uri in uris:
        check_cancel(cancel)
        data = b""
        try:
            absolute = base.store.resolve(uri)
            before = absolute.lstat()
            data = reader(uri, MAX_SOURCE_DOCUMENT_BYTES)
        except CanceledError:
            raise
        except Exception as error:
            entries.append(_StatusDocumentEntry(uri, data=data, error=error))
            continue
        if scan is not None:
            # Charge the actual bounded read before JSON decoding retains its object graph.
            scan.consume(len(data))
            scan.retain(uri)
        try:
            check_cancel(cancel)
            after = absolute.lstat()
            if (
                before.st_dev,
                before.st_ino,
                before.st_size,
                before.st_mtime_ns,
            ) != (
                after.st_dev,
                after.st_ino,
                after.st_size,
                after.st_mtime_ns,
            ):
                raise OSError(f"stored document {uri} changed while status read it; retry")
            document = decode_document(data, uri)
            requested = PurePosixPath(uri).as_posix()
            if document.uri() != requested:
                raise ValueError(
                    f"stored document {uri} mints {document.uri()} from its source, layer, and date metadata; re-collect it"
                )
            verify_document(document)
            entries.append(_StatusDocumentEntry(uri, data, document))
        except CanceledError:
            raise
        except Exception as error:
            entries.append(_StatusDocumentEntry(uri, data=data, error=error))
    return _StatusDocuments(entries)


def _load_knowledge(base: Base, cancel: Cancellation | None, scan: ScanGuard | None) -> _KnowledgeInventory:
    tasks: tuple[TaskTrace, ...] = ()
    projects: tuple[Page, ...] = ()
    wiki: tuple[Page, ...] = ()
    check_cancel(cancel)
    if base.store.enabled(Layer.TASKS):
        tasks = tuple(list_tasks(base, cancel=cancel, scan=scan).traces)
    check_cancel(cancel)
    if base.store.enabled(Layer.PROJECTS):
        projects = list_pages(base, Layer.PROJECTS, PageFilter(), cancel=cancel, scan=scan).pages
    check_cancel(cancel)
    if base.store.enabled(Layer.WIKI):
        wiki = list_pages(base, Layer.WIKI, PageFilter(), cancel=cancel, scan=scan).pages
    return _KnowledgeInventory(tasks, projects, wiki)


def _layer_overviews(
    base: Base,
    documents: _StatusDocuments,
    knowledge: _KnowledgeInventory,
    now: datetime,
    cancel: Cancellation | None,
) -> tuple[LayerOverview, ...]:
    summaries: list[LayerOverview] = []
    for layer in LAYERS:
        check_cancel(cancel)
        enabled = base.store.enabled(layer)
        summary = LayerOverview(layer, enabled, f"{layer}/")
        if not enabled:
            summaries.append(summary)
            continue
        if layer is Layer.EVENTS:
            dates = base.event_dates()
            summary = replace(
                summary,
                count=len(dates),
                unit="day",
                since=dates[0] if dates else "",
                until=dates[-1] if dates else "",
            )
        elif layer is Layer.INDEX:
            count, note = documents.index_overview(_instant(now))
            summary = replace(summary, count=count, unit="document", note=note)
        elif layer is Layer.TASKS:
            summary = replace(
                summary,
                count=len(knowledge.tasks),
                unit="trace",
                until=knowledge.tasks[0].date if knowledge.tasks else "",
            )
        elif layer is Layer.PROJECTS:
            counts: dict[str, int] = {}
            for page in knowledge.projects:
                if page.status:
                    counts[page.status] = counts.get(page.status, 0) + 1
            note = ", ".join(f"{count} {name}" for name, count in sorted(counts.items()))
            summary = replace(summary, count=len(knowledge.projects), unit="page", note=note)
        elif layer is Layer.WIKI:
            tags = {tag.strip().lower() for page in knowledge.wiki for tag in page.tags}
            untagged = sum(not page.tags for page in knowledge.wiki)
            note = ""
            if knowledge.wiki:
                note = f"{len(tags)} tags"
                if untagged:
                    note += f", {untagged} untagged"
            summary = replace(summary, count=len(knowledge.wiki), unit="page", note=note)
        summaries.append(summary)
    return tuple(summaries)


def _environment(base: Base) -> Environment:
    if isinstance(base.environment, Environment):
        return base.environment
    return Environment.from_config(base.config)


def _apply_volume(entry: SourceStatus, days: Sequence[_DayVolume]) -> SourceStatus:
    if not days:
        return entry
    latest = days[-1]
    updated = replace(
        entry,
        last_date=latest.date,
        last_count=latest.count,
        days=len(days),
        last_collected=latest.last_collected,
    )
    if len(days) < QUIET_ARMING_DAYS:
        return updated

    latest_day = date.fromisoformat(latest.date)
    weekend = latest_day.weekday() >= 5
    baseline: list[int] = []
    for previous in reversed(days[:-1]):
        if len(baseline) >= QUIET_WINDOW:
            break
        if weekend:
            if date.fromisoformat(previous.date).weekday() != latest_day.weekday():
                continue
            baseline.append(previous.count)
        elif previous.count > 0:
            baseline.append(previous.count)
    if len(baseline) < QUIET_ARMING_DAYS:
        return updated
    baseline.sort()
    median = baseline[len(baseline) // 2]
    updated = replace(updated, median=median)
    if median == 0:
        return updated
    if latest.count == 0:
        return replace(
            updated,
            quiet=True,
            quiet_reason=f"{latest.date} returned nothing while its recent median is {median}",
        )
    if latest.count * 100 < median * QUIET_RATIO_PERCENT:
        return replace(
            updated,
            quiet=True,
            quiet_reason=(
                f"{latest.date} returned {latest.count}, under {QUIET_RATIO_PERCENT}% of its recent median {median}"
            ),
        )
    return updated


def _observe_freshness(entry: SourceStatus, now: Instant, max_age_hours: int) -> SourceStatus:
    if entry.last_collected is None:
        return replace(entry, stale=entry.enabled and max_age_hours > 0)
    age = now.unix_nanoseconds - entry.last_collected.unix_nanoseconds
    lag_hours = max(0, age // 3_600_000_000_000)
    stale = entry.enabled and max_age_hours > 0 and (age < 0 or age >= max_age_hours * 3_600_000_000_000)
    return replace(
        entry,
        last_collected_at=format_rfc3339(entry.last_collected),
        lag_hours=lag_hours,
        stale=stale,
    )


def _source_statuses(
    base: Base,
    documents: _StatusDocuments,
    request: StatusRequest,
    now: datetime,
    cancel: Cancellation | None,
) -> tuple[tuple[SourceStatus, ...], int, int, int, int, bool, str]:
    history = documents.volume_history()
    undeclared = {name: days for name, days in history.items() if name not in base.config.sources}
    environment = _environment(base)
    entries: list[SourceStatus] = []
    missing: set[str] = set()
    enabled = 0
    missing_tests = 0
    now_instant = _instant(now)
    expected_dates = tuple(day.date().isoformat() for day in previous_completed_days(now, base.config.sync.days))

    for name in base.config.source_names():
        check_cancel(cancel)
        source = base.config.sources[name]
        undeclared.pop(name, None)
        requirements = tuple(
            RequirementStatus(requirement, environment.look_path(requirement) is not None)
            for requirement in source.requires
        )
        test = None
        if source.test:
            test = RequirementStatus(source.test[0], environment.look_test_path(source.test[0]) is not None)
        entry = SourceStatus(
            name,
            source.enabled,
            source.layer,
            requires=requirements,
            install=source.install,
            test=test,
            body=source.has_body(),
            auth=bool(source.auth),
        )
        if source.layer is Layer.INDEX:
            indexed = documents.index_status(name)
            if indexed is not None:
                count, last_date, collected = indexed
                entry = replace(entry, last_count=count, days=1, last_date=last_date, last_collected=collected)
        else:
            entry = _apply_volume(entry, history.get(name, ()))
        max_age = request.max_age_hours
        if max_age == 0 and source.layer is Layer.INDEX:
            max_age = source.effective_max_age_hours(base.config.sync.index_max_age_hours)
        entry = _observe_freshness(entry, now_instant, max_age)
        if source.enabled and source.layer is Layer.EVENTS and base.store.enabled(Layer.EVENTS):
            collected_dates = {day.date for day in history.get(name, ())}
            missing_dates = tuple(day for day in expected_dates if day not in collected_dates)
            entry = replace(entry, missing_dates=missing_dates, stale=entry.stale or bool(missing_dates))
        if source.enabled:
            enabled += 1
            if test is not None and not test.on_path:
                missing_tests += 1
            missing.update(requirement.name for requirement in requirements if not requirement.on_path)
        entries.append(entry)

    for name in sorted(undeclared):
        check_cancel(cancel)
        entry = SourceStatus(name, False, Layer.EVENTS, undeclared=True)
        entry = _observe_freshness(_apply_volume(entry, undeclared[name]), now_instant, 0)
        entries.append(entry)
    if base.store.enabled(Layer.INDEX):
        for name in documents.index_names():
            check_cancel(cancel)
            if name in base.config.sources:
                continue
            entry = SourceStatus(name, False, Layer.INDEX, undeclared=True)
            indexed = documents.index_status(name)
            if indexed is not None:
                count, last_date, collected = indexed
                entry = replace(entry, last_count=count, days=1, last_date=last_date, last_collected=collected)
            entries.append(_observe_freshness(entry, now_instant, 0))

    quiet = sum(entry.quiet for entry in entries)
    stale = any(entry.stale for entry in entries)
    last_sync = max((entry.last_date for entry in entries), default="")
    return tuple(entries), enabled, len(missing), missing_tests, quiet, stale, last_sync


def _collection_stale_days(base: Base, now: datetime) -> int:
    if not base.store.enabled(Layer.EVENTS):
        return 0
    dates = base.event_dates()
    if not dates:
        return 0
    return max(0, (_aware_now(now).date() - date.fromisoformat(dates[-1])).days)


def _shell_arg(value: str | os.PathLike[str]) -> str:
    return "'" + os.fspath(value).replace("'", "'\"'\"'") + "'"


def _base_command(base: Base, arguments: str) -> str:
    return f"fkf --base {_shell_arg(base.root)} {arguments}"


def _safe_problem(base: Base, error: BaseException) -> str:
    message = " ".join(str(error).replace("\r", " ").replace("\n", " ").split())
    roots: list[tuple[str, str]] = [(os.fspath(base.root), ".")]
    try:
        physical = os.fspath(base.root.resolve(strict=True))
    except OSError:
        physical = ""
    if physical:
        roots.append((physical, "."))
    home = os.environ.get("HOME", "")
    if home:
        roots.append((home, "~"))
    state = os.environ.get("XDG_STATE_HOME", "")
    if state:
        roots.append((state, "<state>"))
    for prefix, replacement in sorted(set(roots), key=lambda item: len(item[0]), reverse=True):
        message = message.replace(prefix + os.sep, replacement + "/")
        message = message.replace(prefix, replacement)
    return message


def _tracks_collected(base: Base) -> bool:
    path = base.root / ".gitignore"
    validate_within_root(base.root, path)
    try:
        path.lstat()
    except FileNotFoundError:
        return False
    data = read_file_limited(path, MAX_CONTROL_FILE_BYTES)
    content = data.decode("utf-8", errors="replace")
    markers = MarkedBlockMarkers(
        begin=_MANAGED_BEGIN,
        begin_prefix=_MANAGED_BEGIN_PREFIX,
        end=_MANAGED_END,
        end_prefix=_MANAGED_END_PREFIX,
    )
    region = parse_marked_block_region(content, markers)
    if not region.present:
        return False
    block = content[region.begin : region.end]
    lines = {line.strip() for line in block.split("\n")}
    return not any(layer in lines for layer in _COLLECTED_LAYERS)


def _tracked_paths(base: Base, cancel: Cancellation | None) -> tuple[str, ...]:
    path_value = sanitize_path(os.environ.get("PATH", ""), base.root)
    command = Command(
        argv=(
            "git",
            "-C",
            os.fspath(base.root),
            "--no-pager",
            "--no-optional-locks",
            "-c",
            "core.fsmonitor=false",
            "ls-files",
        ),
        timeout=GIT_TIMEOUT,
        environment={"PATH": path_value},
        max_output_bytes=64 << 20,
    )
    try:
        result = SubprocessRunner().run(command, cancel=cancel)
    except CanceledError:
        raise
    except Exception as error:
        raise OSError(f"ask git what {base.root} tracks: {_safe_problem(base, error)}") from error
    paths = {line.strip() for line in result.stdout.decode("utf-8", errors="replace").splitlines() if line.strip()}
    return tuple(sorted(paths))


def _matches_credential(entry: str) -> bool:
    name = posixpath.basename(entry)
    for pattern in _CREDENTIAL_PATTERNS:
        if pattern.endswith("/"):
            if entry.startswith(pattern) or f"/{pattern}" in entry:
                return True
        elif fnmatch.fnmatchcase(name, pattern):
            return True
    return False


def _git_findings(base: Base, track_collected: bool, cancel: Cancellation | None) -> list[Finding]:
    if not base.store.versioned:
        return [
            Finding(
                "git",
                Severity.WARNING,
                "this base is not a git working tree, so nothing versions the wiki, the projects, or the task traces",
                fix=f"git init {_shell_arg(base.root)}",
            )
        ]
    tracked = _tracked_paths(base, cancel)
    if not tracked:
        quoted = _shell_arg(base.root)
        return [
            Finding(
                "uncommitted",
                Severity.WARNING,
                "this base is a git tree with no commit, so nothing here is versioned and every audit of what git tracks passes by having nothing to look at",
                fix=f"git -C {quoted} add -A && git -C {quoted} commit -m 'chore: first snapshot'",
            )
        ]
    findings: list[Finding] = []
    credentials = tuple(entry for entry in tracked if _matches_credential(entry))
    if credentials:
        findings.append(
            Finding(
                "tracked-credentials",
                Severity.ERROR,
                "git tracks files whose whole purpose is to hold a secret; adding a pattern to .gitignore does not untrack them",
                credentials,
                "git rm --cached <path> and rotate the credential",
            )
        )
    collected = tuple(
        entry
        for entry in tracked
        if not track_collected and any(entry.startswith(layer) for layer in _COLLECTED_LAYERS)
    )
    if collected:
        findings.append(
            Finding(
                "tracked-collected",
                Severity.ERROR,
                "collected content is ignored by the managed block but is still tracked, so it keeps entering history",
                collected,
                "git rm -r --cached events index",
            )
        )
    return findings


def _tree_digest(directory: Path, cancel: Cancellation | None) -> str | None:
    check_cancel(cancel)
    try:
        root_info = directory.lstat()
    except FileNotFoundError:
        return None
    if stat.S_ISLNK(root_info.st_mode) or not stat.S_ISDIR(root_info.st_mode):
        raise UnsafePathError(f"unsafe filesystem path: owned skill {directory} must be a real directory")
    files: list[tuple[str, Path]] = []

    def walk(current: Path) -> None:
        check_cancel(cancel)
        with os.scandir(current) as iterator:
            entries = sorted(iterator, key=lambda entry: entry.name)
        for entry in entries:
            check_cancel(cancel)
            path = current / entry.name
            info = path.lstat()
            if stat.S_ISLNK(info.st_mode):
                raise UnsafePathError(f"unsafe filesystem path: owned skill entry {path} is a symlink")
            if stat.S_ISDIR(info.st_mode):
                walk(path)
            elif stat.S_ISREG(info.st_mode):
                files.append((path.relative_to(directory).as_posix(), path))
            else:
                raise UnsafePathError(f"unsafe filesystem path: owned skill entry {path} is not a regular file")

    walk(directory)
    digest = hashlib.sha256()
    for relative, path in sorted(files):
        check_cancel(cancel)
        encoded = relative.encode()
        data = read_file_limited(path, MAX_CONTROL_FILE_BYTES)
        digest.update(b"P")
        digest.update(struct.pack(">Q", len(encoded)))
        digest.update(encoded)
        digest.update(b"C")
        digest.update(struct.pack(">Q", len(data)))
        digest.update(data)
    return digest.hexdigest()


def _skill_findings(base: Base, cancel: Cancellation | None) -> list[Finding]:
    missing: list[str] = []
    drifted: list[str] = []
    for name in BUNDLED_SKILLS:
        check_cancel(cancel)
        relative = f"{BASE_SKILLS_DIR}/{name}"
        installed_root = base.root / relative
        validate_within_root(base.root, installed_root)
        installed = _tree_digest(installed_root, cancel)
        if installed is None:
            missing.append(relative)
            continue
        if installed != skill_digest(name):
            drifted.append(relative)
    findings: list[Finding] = []
    if missing:
        findings.append(
            Finding(
                "skills",
                Severity.WARNING,
                "fkf-owned skills are missing from this base",
                tuple(missing),
                f"fkf init {_shell_arg(base.root)}",
            )
        )
    if drifted:
        findings.append(
            Finding(
                "skills",
                Severity.WARNING,
                "fkf-owned skills differ from the installed package's copy; they are rewritten by init, so local edits are lost",
                tuple(drifted),
                f"fkf init {_shell_arg(base.root)}",
            )
        )
    return findings


def _helper_findings(base: Base, cancel: Cancellation | None) -> list[Finding]:
    helpers = shipped_helpers()
    shipped_names = set(helpers)
    required = {HOOK_SCRIPT}
    for source in base.config.enabled_sources():
        required.update(name for name in source.requires if name in shipped_names)
    missing: list[str] = []
    drifted: list[str] = []
    for name in sorted(required):
        check_cancel(cancel)
        relative = f"{BASE_SOURCES_DIR}/{name}"
        target = base.root / relative
        validate_within_root(base.root, target)
        try:
            info = target.lstat()
        except FileNotFoundError:
            missing.append(relative)
            continue
        if stat.S_ISLNK(info.st_mode) or not stat.S_ISREG(info.st_mode):
            raise UnsafePathError(f"unsafe filesystem path: helper {relative} must be a regular non-symlink file")
        current = read_file_limited(target, MAX_CONTROL_FILE_BYTES)
        if current != helpers[name]:
            drifted.append(relative)
    findings: list[Finding] = []
    if missing:
        findings.append(
            Finding(
                "helpers",
                Severity.WARNING,
                "official helpers required by this base are missing",
                tuple(missing),
                _base_command(base, "config helpers --refresh"),
            )
        )
    if drifted:
        findings.append(
            Finding(
                "helpers",
                Severity.WARNING,
                "official helpers differ from this binary's copy",
                tuple(drifted),
                _base_command(base, "config helpers --refresh"),
            )
        )
    return findings


def _permission_repair_command(root: Path) -> str:
    quoted_root = _shell_arg(root)
    quoted_git = _shell_arg(root / ".git")
    quoted_bin = _shell_arg(root / BASE_SOURCES_DIR)
    quoted_tests = _shell_arg(root / BASE_TESTS_DIR)
    quoted_clients = _shell_arg(root / BASE_CLIENTS_DIR)
    preserve = (
        ' -type f -exec sh -c \'for file do if [ -x "$file" ]; then chmod 700 "$file"; '
        'else chmod 600 "$file"; fi; done\' sh {} +; fi'
    )
    return (
        f"chmod 700 {quoted_root}"
        f" && find {quoted_root} -path {quoted_git} -prune -o -type d -exec chmod 700 {{}} +"
        f" && find {quoted_root} -path {quoted_git} -prune -o -path {quoted_bin} -prune -o -path {quoted_tests}"
        f" -prune -o -path {quoted_clients} -prune -o -type f -exec chmod 600 {{}} +"
        f" && if [ -d {quoted_bin} ]; then find {quoted_bin}{preserve}"
        f" && if [ -d {quoted_tests} ]; then find {quoted_tests}{preserve}"
        f" && if [ -d {quoted_clients} ]; then find {quoted_clients}{preserve}"
    )


def _permission_finding(base: Base, cancel: Cancellation | None) -> Finding | None:
    check_cancel(cancel)
    try:
        root = base.root.resolve(strict=True)
    except OSError as error:
        raise OSError(f"resolve base for permission audit: {error}") from error
    wrong: list[str] = []

    def inspect(relative: str, info: os.stat_result) -> None:
        is_directory = stat.S_ISDIR(info.st_mode)
        desired = BASE_DIR_MODE if is_directory else BASE_FILE_MODE
        parts = PurePosixPath(relative).parts
        if (
            not is_directory
            and len(parts) > 1
            and parts[0] in {BASE_SOURCES_DIR, BASE_TESTS_DIR, BASE_CLIENTS_DIR}
            and stat.S_IMODE(info.st_mode) & 0o111
        ):
            desired = 0o700
        if stat.S_IMODE(info.st_mode) != desired:
            wrong.append(relative)

    inspect(".", root.lstat())

    def walk(directory: Path) -> None:
        check_cancel(cancel)
        with os.scandir(directory) as iterator:
            entries = sorted(iterator, key=lambda entry: entry.name)
        for entry in entries:
            check_cancel(cancel)
            if entry.name == ".git" and entry.is_dir(follow_symlinks=False):
                continue
            current = directory / entry.name
            info = current.lstat()
            if stat.S_ISLNK(info.st_mode):
                continue
            relative = current.relative_to(root).as_posix()
            inspect(relative, info)
            if stat.S_ISDIR(info.st_mode):
                walk(current)

    walk(root)
    if not wrong:
        return None
    return Finding(
        "permissions",
        Severity.WARNING,
        "files or directories are not owner-only; a base can hold mail and shell-activity metadata",
        tuple(sorted(wrong)),
        _permission_repair_command(root),
    )


def _derived_findings(
    base: Base,
    request: StatusRequest,
    *,
    graph_missing: bool,
    cancel: Cancellation | None,
) -> list[Finding]:
    findings: list[Finding] = []
    if graph_missing:
        findings.append(
            Finding(
                "derived",
                Severity.WARNING,
                "the graph cache is absent, so `graph <uri>` and `context --expand` have nothing to read",
                (GRAPH_FILE,),
                _base_command(base, "build graph"),
            )
        )
    try:
        check_cancel(cancel)
        use = (
            request.lexical_health(base)
            if request.lexical_health is not None
            else lexical_index_health(base, cancel=cancel)
        )
        check_cancel(cancel)
    except CanceledError:
        raise
    except Exception as error:
        findings.append(
            Finding(
                "derived",
                Severity.ERROR,
                f"the lexical index cache could not be validated against its inputs: {_safe_problem(base, error)}",
                (LEXICAL_INDEX_PATH,),
                f"repair the input named in this message, then rebuild with `{_base_command(base, 'build index')}`",
            )
        )
        return findings
    if use.used:
        return findings
    if use.reason == LEXICAL_INDEX_FALLBACK_MISSING:
        findings.append(
            Finding(
                "derived",
                Severity.WARNING,
                "the lexical index cache is absent, so `context` and `find` must scan documents directly",
                (LEXICAL_INDEX_PATH,),
                _base_command(base, "build index"),
            )
        )
    elif use.reason == LEXICAL_INDEX_FALLBACK_STALE:
        findings.append(
            Finding(
                "derived",
                Severity.WARNING,
                "the lexical index cache is stale, so `context` and `find` must scan documents directly",
                (LEXICAL_INDEX_PATH,),
                _base_command(base, "build index"),
            )
        )
    elif use.reason == LEXICAL_INDEX_FALLBACK_CORRUPT:
        findings.append(
            Finding(
                "derived",
                Severity.ERROR,
                "the lexical index cache is corrupt: rebuild it with `fkf build index`",
                (LEXICAL_INDEX_PATH,),
                _base_command(base, "build index"),
            )
        )
    return findings


def _cited_traces(knowledge: _KnowledgeInventory, cancel: Cancellation | None) -> set[str]:
    cited: set[str] = set()
    for page in (*knowledge.wiki, *knowledge.projects):
        check_cancel(cancel)
        cited.update(cited_task_traces(page, cancel=cancel))
    return cited


def _unharvested(knowledge: _KnowledgeInventory, cancel: Cancellation | None) -> int:
    cited = _cited_traces(knowledge, cancel)
    total = 0
    for trace in knowledge.tasks:
        check_cancel(cancel)
        uri = trace.uri
        if uri in cited:
            continue
        page = cast(Page, trace.page)
        total += len(learned_bullets(page, cancel=cancel))
    return total


def _suggest_next(status: Status) -> tuple[str, ...]:
    root = status.base

    def command(arguments: str) -> str:
        return f"fkf --base {_shell_arg(root)} {arguments}"

    items: list[str] = []
    if not status.trust.trusted:
        items.append(command("trust") + "  read the commands this base declares, then record them")
    events = next((item for item in status.layers if item.layer is Layer.EVENTS), None)
    if events is not None and events.count == 0:
        items.append(command("sync --days 7") + "  collect the last seven completed days")
    elif status.stale_days > 1:
        days = min(status.stale_days, 30)
        items.append(command(f"sync --days {days}") + f"  the newest day here is {status.stale_days} day(s) old")
    if status.graph is None:
        items.append(command("build graph") + "  derive the edge list the graph and --expand read")
    if status.unharvested:
        items.append(
            command("list tasks learned --unharvested")
            + f"  {status.unharvested} bullet(s) from uncited traces; review only durable findings"
        )
    items.extend(
        (
            command('context "<terms>"') + "  the evidence pack you hand an agent",
            command("find <term>") + "  every match, in every layer",
            command("graph <uri>") + "  what is connected to one thing",
        )
    )
    return tuple(items)


def report(
    base: Base,
    request: StatusRequest | None = None,
    *,
    cancel: Cancellation | None = None,
) -> Status:
    """Compile the complete diagnostic without collecting or mutating the base."""

    request = request or StatusRequest()
    if request.max_age_hours < 0:
        raise ValueError("max_age_hours must not be negative")
    check_cancel(cancel)
    now = _aware_now(request.evaluation_time or base.now())
    trust = read_trust(base.config, cancel=cancel)
    reader = request.document_reader or base.read_file
    documents = _load_status_documents(base, reader, cancel, request.scan)
    check_cancel(cancel)
    track_collected = _tracks_collected(base)
    knowledge = _load_knowledge(base, cancel, request.scan)
    layers = _layer_overviews(base, documents, knowledge, now, cancel)
    sources, enabled, missing, missing_tests, quiet, stale, last_sync = _source_statuses(
        base,
        documents,
        request,
        now,
        cancel,
    )

    auth_required: tuple[str, ...] = ()
    harnesses: tuple[HarnessRegistration, ...] = ()
    if request.live:
        if trust.trusted:
            auth_required = tuple(
                sorted(
                    set(
                        probe_source_auth(
                            base,
                            base.config.enabled_sources(),
                            live=True,
                            cancel=cancel,
                        )
                    )
                )
            )
            required = set(auth_required)
            sources = tuple(replace(source, auth_required=source.name in required) for source in sources)
        inspector = request.inspect_harnesses or inspect_harnesses
        harnesses = tuple(
            sorted(
                inspector(base.root, executable=request.executable, cancel=cancel),
                key=lambda item: item.name,
            )
        )

    findings: list[Finding] = []
    graph: GraphSummary | None = None
    graph_missing = False
    try:
        graph = summarize_graph(base, cancel=cancel)
    except DerivedGraphMissingError:
        graph_missing = True
    except CanceledError:
        raise
    except Exception as error:
        findings.append(
            Finding(
                "derived",
                Severity.ERROR,
                f"the graph cache is invalid: {_safe_problem(base, error)}",
                (GRAPH_FILE, GRAPH_DST_FILE, GRAPH_OFFSETS_FILE, GRAPH_META_FILE),
                _base_command(base, "build graph"),
            )
        )

    if not trust.trusted:
        findings.append(
            Finding(
                "trust",
                Severity.WARNING,
                "this base's configuration is not trusted on this machine, so `fkf sync` will refuse to run its commands",
                fix=_base_command(base, "trust"),
            )
        )
    if track_collected:
        findings.append(
            Finding(
                "history",
                Severity.WARNING,
                "this base commits events/ and index/; git history is append-only, so anything collected is permanent",
                fix="start a new base if that was not intended",
            )
        )
    if not request.skip_git_audit:
        findings.extend(_git_findings(base, track_collected, cancel))
    findings.extend(_skill_findings(base, cancel))
    findings.extend(_helper_findings(base, cancel))

    conflicted = documents.conflicted()
    if conflicted:
        findings.append(
            Finding(
                "conflict-markers",
                Severity.ERROR,
                "collected JSON documents contain unresolved git merge conflicts",
                conflicted,
                f"resolve the conflict or re-collect the day with `{_base_command(base, 'sync --force')}`",
            )
        )
    permission = _permission_finding(base, cancel)
    if permission is not None:
        findings.append(permission)
    findings.extend(_derived_findings(base, request, graph_missing=graph_missing, cancel=cancel))

    unharvested = _unharvested(knowledge, cancel) if base.store.enabled(Layer.TASKS) else 0
    if unharvested:
        findings.append(
            Finding(
                "learned",
                Severity.WARNING,
                f'{unharvested} "## Learned" bullet(s) belong to traces not cited by wiki or project pages; citation counts are not lesson validation',
                fix=_base_command(base, "list tasks learned --unharvested"),
            )
        )
    for entry in documents.entries:
        check_cancel(cancel)
        if entry.error is None:
            continue
        problem = _safe_problem(base, entry.error)
        findings.append(
            Finding(
                "documents",
                Severity.ERROR,
                f"{entry.uri}: {problem}",
                (entry.uri,),
                "re-collect the day or fix the document JSON",
            )
        )

    findings.sort(key=lambda item: item.check)
    errors = sum(finding.severity is Severity.ERROR for finding in findings)
    status = Status(
        base=os.fspath(base.root),
        name=base.config.name,
        base_origin=base.origin,
        trust=trust,
        versioned=base.store.versioned,
        track_collected=track_collected,
        layers=layers,
        sources=sources,
        harnesses=harnesses,
        auth_required=auth_required,
        findings=tuple(findings),
        graph=graph,
        unharvested=unharvested,
        enabled=enabled,
        missing_requirements=missing,
        missing_test_hooks=missing_tests,
        quiet=quiet,
        errors=errors,
        warnings=len(findings) - errors,
        ok=errors == 0,
        stale=stale,
        last_sync=last_sync,
        stale_days=_collection_stale_days(base, now),
        max_age_hours=request.max_age_hours,
        task_pages=tuple(cast(Page, trace.page) for trace in knowledge.tasks),
        project_pages=knowledge.projects,
    )
    return replace(status, next=_suggest_next(status))


__all__ = [
    "DocumentReader",
    "Finding",
    "HarnessInspector",
    "HarnessRegistration",
    "LayerOverview",
    "LexicalHealth",
    "RequirementStatus",
    "SourceStatus",
    "Status",
    "StatusRequest",
    "report",
]
