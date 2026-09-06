"""Exhaustive offline search across authored pages and durable evidence."""

from __future__ import annotations

import hashlib
import os
import struct
from collections.abc import Callable, Iterator, Mapping
from contextlib import suppress
from dataclasses import dataclass, field, replace
from datetime import datetime
from functools import cmp_to_key
from typing import Final, Protocol

from fkf.base import Base
from fkf.bodies import BodyManifest, load_body_manifest, read_cached_body_from_manifest
from fkf.config import ConfigError
from fkf.documents import Document, Record, event_document_uri, index_document_uri
from fkf.errors import OperationalError
from fkf.fields import FIELD_TIME, FIELD_TITLE, FIELD_URL, FieldPath, is_well_known_field, scalar_string
from fkf.graph import IdentityResolver
from fkf.jsoncodec import dumps
from fkf.lexical import (
    LEXICAL_INDEX_META_PATH,
    LEXICAL_INDEX_PATH,
    LexicalIndexUse,
    lexical_inputs,
    lexical_inputs_match,
    query_find_lexical_index,
)
from fkf.markdown import Page
from fkf.pages import normalize_terms, read_page, require_known
from fkf.process import Cancellation, CommandCanceledError
from fkf.query import Window
from fkf.store import MARKDOWN_EXTENSION, TASK_TRACE_FILE, Layer, LayerDisabledError
from fkf.timeutil import parse_record_time

DEFAULT_FIND_DAYS: Final = 7
DEFAULT_FIND_LIMIT: Final = 200
NO_FIND_LIMIT: Final = -1
MAX_FIND_PAGE_LIMIT: Final = 100
FIND_PHASE_PAGE: Final = "page"
FIND_PHASE_RECORD: Final = "record"
FIND_PHASE_VOLUME: Final = "volume"


@dataclass(frozen=True, slots=True)
class Where:
    """One exact equality over the closed field-path grammar."""

    path: FieldPath
    value: str


@dataclass(frozen=True, slots=True)
class FindFilter:
    """The complete exhaustive-find request."""

    sources: tuple[str, ...] = ()
    layers: tuple[Layer, ...] = ()
    window: Window = field(default_factory=Window)
    grep: tuple[str, ...] = ()
    where: tuple[Where, ...] = ()
    limit: int = 0
    bodies: bool = False

    def __post_init__(self) -> None:
        object.__setattr__(self, "sources", tuple(self.sources))
        object.__setattr__(self, "layers", tuple(self.layers))
        object.__setattr__(self, "grep", tuple(self.grep))
        object.__setattr__(self, "where", tuple(self.where))

    def selects(self) -> bool:
        """Return whether this is a question rather than bare discovery."""
        return bool(self.sources or self.grep or self.where)

    def record_only(self) -> bool:
        """Return whether authored pages cannot satisfy the filter."""
        return bool(self.sources or self.where)

    def wants(self, layer: Layer) -> bool:
        return not self.layers or layer in self.layers

    def result_limit(self) -> int:
        if self.limit != 0:
            return self.limit
        return NO_FIND_LIMIT if self.selects() else DEFAULT_FIND_LIMIT


@dataclass(frozen=True, slots=True)
class PageHit:
    """One matching authored Markdown page."""

    uri: str
    layer: Layer = field(metadata={"json": "layer,omitempty"})
    slug: str
    title: str = field(default="", metadata={"json": "title,omitempty"})
    type: str = field(default="", metadata={"json": "type,omitempty"})
    date: str = field(default="", metadata={"json": "date,omitempty"})
    tags: tuple[str, ...] = field(default=(), metadata={"json": "tags,omitempty"})
    score: int = 0
    matched: tuple[str, ...] = ()
    excerpt: str = field(default="", metadata={"json": "excerpt,omitempty"})


@dataclass(frozen=True, slots=True)
class RecordHit:
    """One matching record stamped with a stable citation URI."""

    uri: str
    source: str
    date: str = field(default="", metadata={"json": "date,omitempty"})
    time: str = field(default="", metadata={"json": "time,omitempty"})
    title: str = field(default="", metadata={"json": "title,omitempty"})
    url: str = field(default="", metadata={"json": "url,omitempty"})
    fields: dict[str, tuple[str, ...]] | None = field(default=None, metadata={"json": "fields,omitempty"})
    body: bool = field(default=False, metadata={"json": "body,omitempty"})
    body_cached: bool = field(default=False, metadata={"json": "body_cached,omitempty"})
    raw: Record | None = field(default=None, metadata={"json": "record,omitempty"})
    _relation_fields: frozenset[str] = field(
        default_factory=frozenset,
        repr=False,
        compare=False,
        metadata={"json": "-"},
    )

    def canonicalized(self, resolver: IdentityResolver) -> RecordHit:
        """Canonicalize only fields the stored schema marked as relations."""
        if not self.fields or not self._relation_fields:
            return self
        fields = dict(self.fields)
        for name in self._relation_fields:
            if name in fields:
                fields[name] = tuple(resolver.canonical(value) for value in fields[name])
        return replace(self, fields=fields)


@dataclass(frozen=True, slots=True)
class SourceCount:
    source: str
    count: int


@dataclass(frozen=True, slots=True)
class Volume:
    """Matched record volume for one event day or the undated index."""

    date: str
    total: int
    sources: tuple[SourceCount, ...]


@dataclass(slots=True)
class FindResult:
    """One complete scan with independently bounded pages and records."""

    window: Window
    days: tuple[str, ...] = field(default=(), metadata={"json": "days,omitempty"})
    pages: tuple[PageHit, ...] = field(default=(), metadata={"json": "pages,omitempty"})
    records: tuple[RecordHit, ...] = field(default=(), metadata={"json": "records,omitempty"})
    volumes: tuple[Volume, ...] = field(default=(), metadata={"json": "volumes,omitempty"})
    scanned: int = 0
    matched: int = 0
    truncated: bool = field(default=False, metadata={"json": "truncated,omitempty"})
    index: LexicalIndexUse | None = field(default=None, metadata={"json": "index,omitempty"})


@dataclass(frozen=True, slots=True)
class FindPosition:
    """The last semantic sort key returned by one bounded find page."""

    phase: str = ""
    score: int = 0
    time: str = ""
    uri: str = ""
    date: str = ""

    def as_json(self) -> dict[str, object]:
        value: dict[str, object] = {"phase": self.phase}
        for name in ("score", "time", "uri", "date"):
            item = getattr(self, name)
            if item:
                value[name] = item
        return value


@dataclass(frozen=True, slots=True)
class BoundedFindResult:
    """One bounded page plus the digest and position needed to continue it."""

    result: FindResult
    snapshot_sha256: str
    next: FindPosition | None = None


@dataclass(frozen=True, slots=True)
class _LexicalPlan:
    candidates: frozenset[str] | None
    diagnostic: LexicalIndexUse | None
    current: Callable[[], bool] | None = None


@dataclass(frozen=True, slots=True)
class _Prepared:
    filter: FindFilter
    terms: tuple[str, ...]
    dates: tuple[str, ...]
    resolver: IdentityResolver
    manifest: BodyManifest | None
    lexical: _LexicalPlan


@dataclass(slots=True)
class _Scan:
    pages: list[PageHit] = field(default_factory=list)
    records: list[RecordHit] = field(default_factory=list)
    volumes: list[Volume] = field(default_factory=list)
    scanned: int = 0
    matched: int = 0
    truncated: bool = False


class _Digest(Protocol):
    def update(self, data: bytes, /) -> None: ...

    def hexdigest(self) -> str: ...


def parse_where(argument: str) -> Where:
    """Parse one ``path=value`` equality without expanding the selector grammar."""
    raw, separator, value = argument.partition("=")
    if not separator:
        raise ValueError(f"--where takes <path>=<value>, for example --where .state=MERGED (got {argument!r})")
    try:
        path = FieldPath.parse(raw.strip())
    except ValueError as error:
        raise ValueError(f"--where: {error}") from error
    return Where(path, value.strip())


def compact_find_result(result: FindResult) -> None:
    """Remove internal selected-day metadata and full provider payloads in place."""
    result.days = ()
    result.records = tuple(replace(record, raw=None) for record in result.records)


def _check_canceled(cancel: Cancellation | None) -> None:
    if cancel is not None and cancel.is_set():
        raise CommandCanceledError("command canceled")


def _validate(base: Base, filters: FindFilter) -> None:
    if filters.limit < NO_FIND_LIMIT:
        raise ValueError(f"limit must be {NO_FIND_LIMIT} or greater")
    if filters.bodies and not filters.grep:
        raise ValueError("--bodies requires at least one search term")
    if any(not term.strip() for term in filters.grep):
        raise ValueError("--grep terms must contain non-whitespace text")
    for layer in filters.layers:
        if not base.store.enabled(layer):
            raise LayerDisabledError(layer)
    if filters.layers and not filters.selects():
        windowed_events = bool((filters.window.since or filters.window.until) and Layer.EVENTS in filters.layers)
        if not windowed_events:
            raise ValueError(
                "--layer narrows a find question but none was given; add terms or a record predicate, "
                f"or use `fkf list {filters.layers[0]}` to browse the layer"
            )
    require_known("source", filters.sources, base.config.source_names())


def _select_dates(base: Base, filters: FindFilter) -> tuple[str, ...]:
    if not filters.wants(Layer.EVENTS) or not base.store.enabled(Layer.EVENTS):
        return ()
    selected = tuple(value for value in base.event_dates() if filters.window.contains(value))
    if not filters.window.since and not filters.window.until and not filters.selects():
        return selected[-DEFAULT_FIND_DAYS:]
    return selected


def _prepare_lexical(
    base: Base,
    filters: FindFilter,
    terms: tuple[str, ...],
    cancel: Cancellation | None,
    *,
    forced_reason: str = "",
) -> _LexicalPlan:
    if not terms:
        return _LexicalPlan(None, None)

    def fallback(reason: str) -> _LexicalPlan:
        inputs, _semantics, digest = lexical_inputs(base, cancel=cancel)
        return _LexicalPlan(
            None,
            LexicalIndexUse(reason=reason),
            lambda: lexical_inputs_match(base, inputs, digest, cancel=cancel),
        )

    if forced_reason:
        return fallback(forced_reason)
    # Missing derived files are the common fallback and do not need the relatively heavy
    # lexical decoder imported merely to rediscover their absence.
    if not (base.root / LEXICAL_INDEX_PATH).exists() or not (base.root / LEXICAL_INDEX_META_PATH).exists():
        return fallback("missing")

    plan, use = query_find_lexical_index(
        base,
        terms,
        bodies=filters.bodies,
        layers=filters.layers,
        sources=filters.sources,
        window=filters.window,
        record_only=filters.record_only(),
        cancel=cancel,
    )
    diagnostic = use
    if plan is None or not diagnostic.used:
        fallback_plan = fallback(diagnostic.reason)
        return replace(fallback_plan, diagnostic=diagnostic)
    return _LexicalPlan(
        plan.candidates,
        diagnostic,
        lambda: lexical_inputs_match(base, plan.inputs, plan.inputs_sha256, cancel=cancel),
    )


def _prepare(
    base: Base,
    filters: FindFilter,
    cancel: Cancellation | None,
    *,
    forced_index_reason: str = "",
) -> _Prepared:
    _check_canceled(cancel)
    _validate(base, filters)
    terms = normalize_terms(filters.grep)
    resolver = IdentityResolver.load(base, cancel=cancel)
    manifest = load_body_manifest(base) if filters.bodies else None
    lexical = _prepare_lexical(base, filters, terms, cancel, forced_reason=forced_index_reason)
    return _Prepared(filters, terms, _select_dates(base, filters), resolver, manifest, lexical)


def _excerpt_around(body: str, term: str) -> str:
    folded = body.lower()
    index = folded.find(term)
    if index < 0:
        return ""
    radius = 90
    start = max(0, index - radius)
    end = min(len(body), index + len(term) + radius)
    excerpt = " ".join(body[start:end].split())
    if start:
        excerpt = "…" + excerpt
    if end < len(body):
        excerpt += "…"
    return excerpt


def _score_page(page: Page, layer: Layer, terms: tuple[str, ...]) -> PageHit | None:
    title = " ".join((page.title, page.slug, page.description, *page.aliases)).lower()
    tags = " ".join(page.tags).lower()
    body = page.body.lower()
    score = 0
    excerpt = ""
    for term in terms:
        if term in title:
            points = 10
        elif term in tags:
            points = 6
        elif term in body:
            points = 2
        else:
            return None
        score += points
        if not excerpt:
            excerpt = _excerpt_around(page.body, term)
    return PageHit(
        uri=page.uri,
        layer=layer,
        slug=page.slug,
        title=page.title,
        type=page.type,
        date=page.date,
        tags=page.tags,
        score=score,
        matched=terms,
        excerpt=excerpt,
    )


def _admitted(uri: str, candidates: frozenset[str] | None) -> bool:
    return candidates is None or uri in candidates


def _directory_names(
    directory: str | os.PathLike[str],
    *,
    directories: bool,
    cancel: Cancellation | None = None,
) -> tuple[str, ...]:
    try:
        iterator = os.scandir(directory)
    except FileNotFoundError:
        return ()
    except OSError as error:
        raise OSError(f"list {directory}: {error}") from error
    names: list[str] = []
    with iterator:
        for entry in iterator:
            _check_canceled(cancel)
            if entry.is_dir(follow_symlinks=False) is directories:
                names.append(entry.name)
    return tuple(sorted(names))


def _date_directories(directory: str | os.PathLike[str], cancel: Cancellation | None = None) -> tuple[str, ...]:
    dates: list[str] = []
    for name in _directory_names(directory, directories=True, cancel=cancel):
        _check_canceled(cancel)
        try:
            parsed = datetime.strptime(name, "%Y-%m-%d").date()
        except ValueError:
            continue
        if parsed.isoformat() == name:
            dates.append(name)
    return tuple(dates)


def _scan_pages(
    base: Base,
    prepared: _Prepared,
    on_page: Callable[[PageHit], None],
    cancel: Cancellation | None,
) -> None:
    filters = prepared.filter
    if not prepared.terms or filters.record_only():
        return
    for layer in (Layer.WIKI, Layer.PROJECTS):
        if not filters.wants(layer) or not base.store.enabled(layer):
            continue
        for name in _directory_names(base.store.directory(layer), directories=False, cancel=cancel):
            _check_canceled(cancel)
            if not name.endswith(MARKDOWN_EXTENSION):
                continue
            hit = _score_page(read_page(base, f"{layer}/{name}", cancel=cancel), layer, prepared.terms)
            if hit is None or not _admitted(hit.uri, prepared.lexical.candidates):
                continue
            # Flat-page search historically leaves the optional authored date out of SearchHit.
            on_page(replace(hit, date=""))
    if filters.wants(Layer.TASKS) and base.store.enabled(Layer.TASKS):
        directory = base.store.directory(Layer.TASKS)
        for date in reversed(_date_directories(directory, cancel)):
            if not filters.window.contains(date):
                continue
            for slug in _directory_names(directory / date, directories=True, cancel=cancel):
                _check_canceled(cancel)
                uri = f"tasks/{date}/{slug}/{TASK_TRACE_FILE}"
                if not base.exists(uri):
                    continue
                hit = _score_page(read_page(base, uri, cancel=cancel), Layer.TASKS, prepared.terms)
                if hit is None or not _admitted(hit.uri, prepared.lexical.candidates):
                    continue
                on_page(replace(hit, date=date, slug=slug))


def _walk_scalar_leaves(value: object) -> Iterator[str]:
    if isinstance(value, Mapping):
        for key in sorted(value):
            yield from _walk_scalar_leaves(value[key])
        return
    if isinstance(value, list):
        for item in value:
            yield from _walk_scalar_leaves(item)
        return
    if text := scalar_string(value):
        yield text


def _same_identity(left: str, right: str, resolver: IdentityResolver) -> bool:
    identity = resolver.exact(right)
    return identity is not None and resolver.canonical(left) == identity.canonical


def _grep_record(value: Record, term: str, resolver: IdentityResolver) -> bool:
    needle = term.lower()
    identity = resolver.exact(term)
    canonical = identity.canonical if identity is not None else ""
    return any(
        needle in text.lower() or bool(canonical and resolver.canonical(text) == canonical)
        for text in _walk_scalar_leaves(value)
    )


def _matches(value: Record, body: str, prepared: _Prepared) -> bool:
    for clause in prepared.filter.where:
        if not any(
            selected.casefold() == clause.value.casefold() or _same_identity(selected, clause.value, prepared.resolver)
            for selected in clause.path.eval_strings(value)
        ):
            return False
    folded_body = body.casefold()
    return all(
        _grep_record(value, term, prepared.resolver) or bool(prepared.filter.bodies and term.casefold() in folded_body)
        for term in prepared.terms
    )


def _record_hit(document: Document, record: Record) -> RecordHit:
    uri = document.record_uri(record)
    if uri is None:
        raise ConfigError(f"record in {document.uri()} has no stable identity")
    canonical_time = ""
    if raw_time := document.fields.eval_string(FIELD_TIME, record):
        with suppress(ValueError):
            instant = parse_record_time(raw_time).to_datetime()
            # Find's stable ordering key follows Go's fixed RFC 3339 seconds layout;
            # durable evidence retains any finer provider precision in ``raw``.
            canonical_time = (
                f"{instant.year:04d}-{instant.month:02d}-{instant.day:02d}T"
                f"{instant.hour:02d}:{instant.minute:02d}:{instant.second:02d}Z"
            )
    projected: dict[str, tuple[str, ...]] = {}
    relation_fields: set[str] = set()
    for name in document.fields.names():
        definition = document.schema.get(name)
        if definition is not None and definition.relation:
            relation_fields.add(name)
        if is_well_known_field(name):
            continue
        values = tuple(document.fields.eval_strings(name, record))
        if values:
            projected[name] = values
    return RecordHit(
        uri=uri,
        source=document.source,
        date=document.date,
        time=canonical_time,
        title=document.fields.eval_string(FIELD_TITLE, record) or "",
        url=document.fields.eval_string(FIELD_URL, record) or "",
        fields=projected or None,
        body=document.body,
        raw=record,
        _relation_fields=frozenset(relation_fields),
    )


def _canonicalize(hit: RecordHit, resolver: IdentityResolver) -> RecordHit:
    return hit.canonicalized(resolver)


def _scan_document(
    base: Base,
    prepared: _Prepared,
    scan: _Scan,
    uri: str,
    *,
    counting: bool,
    on_record: Callable[[RecordHit], None],
    cancel: Cancellation | None,
) -> int:
    document = base.read_document(uri)
    scan.scanned += document.count
    matched = 0
    for record in document.records:
        _check_canceled(cancel)
        projected = _record_hit(document, record)
        if not _admitted(projected.uri, prepared.lexical.candidates):
            continue
        body = ""
        found = False
        if prepared.manifest is not None:
            body, _entry, found = read_cached_body_from_manifest(base, prepared.manifest, projected.uri)
        if not _matches(record, body, prepared):
            continue
        matched += 1
        scan.matched += 1
        if counting:
            continue
        projected = _canonicalize(replace(projected, body_cached=found), prepared.resolver)
        on_record(projected)
    return matched


def _scan_event_records(
    base: Base,
    prepared: _Prepared,
    scan: _Scan,
    *,
    counting: bool,
    on_record: Callable[[RecordHit], None],
    on_volume: Callable[[Volume], None],
    cancel: Cancellation | None,
) -> None:
    for day in reversed(prepared.dates):
        sources: list[SourceCount] = []
        total = 0
        for name in base.day_documents(day):
            _check_canceled(cancel)
            if prepared.filter.sources and name not in prepared.filter.sources:
                continue
            matched = _scan_document(
                base,
                prepared,
                scan,
                event_document_uri(day, name),
                counting=counting,
                on_record=on_record,
                cancel=cancel,
            )
            if matched:
                total += matched
                sources.append(SourceCount(name, matched))
        if counting and total:
            on_volume(Volume(day, total, tuple(sources)))


def _scan_index_records(
    base: Base,
    prepared: _Prepared,
    scan: _Scan,
    *,
    counting: bool,
    on_record: Callable[[RecordHit], None],
    on_volume: Callable[[Volume], None],
    cancel: Cancellation | None,
) -> None:
    filters = prepared.filter
    if not filters.selects() or not filters.wants(Layer.INDEX) or not base.store.enabled(Layer.INDEX):
        return
    sources: list[SourceCount] = []
    total = 0
    for name in base.index_documents():
        _check_canceled(cancel)
        if filters.sources and name not in filters.sources:
            continue
        matched = _scan_document(
            base,
            prepared,
            scan,
            index_document_uri(name),
            counting=counting,
            on_record=on_record,
            cancel=cancel,
        )
        if matched:
            total += matched
            sources.append(SourceCount(name, matched))
    if counting and total:
        on_volume(Volume("", total, tuple(sources)))


def _traverse(
    base: Base,
    prepared: _Prepared,
    scan: _Scan,
    *,
    counting: bool,
    on_page: Callable[[PageHit], None],
    on_record: Callable[[RecordHit], None],
    on_volume: Callable[[Volume], None],
    cancel: Cancellation | None,
) -> None:
    if not counting:
        _scan_pages(base, prepared, on_page, cancel)
    _scan_event_records(
        base,
        prepared,
        scan,
        counting=counting,
        on_record=on_record,
        on_volume=on_volume,
        cancel=cancel,
    )
    _scan_index_records(
        base,
        prepared,
        scan,
        counting=counting,
        on_record=on_record,
        on_volume=on_volume,
        cancel=cancel,
    )


def _scan(
    base: Base,
    prepared: _Prepared,
    *,
    counting: bool,
    cancel: Cancellation | None,
) -> FindResult:
    scan = _Scan()
    limit = prepared.filter.limit if counting else prepared.filter.result_limit()

    def add_record(hit: RecordHit) -> None:
        if limit > 0 and len(scan.records) >= limit:
            scan.truncated = True
            return
        scan.records.append(hit)

    def add_volume(volume: Volume) -> None:
        if limit > 0 and len(scan.volumes) >= limit:
            scan.truncated = True
            return
        scan.volumes.append(volume)

    _traverse(
        base,
        prepared,
        scan,
        counting=counting,
        on_page=scan.pages.append,
        on_record=add_record,
        on_volume=add_volume,
        cancel=cancel,
    )
    scan.pages.sort(key=lambda hit: (-hit.score, hit.uri))
    if limit > 0 and len(scan.pages) > limit:
        del scan.pages[limit:]
        scan.truncated = True
    scan.records.sort(key=lambda hit: hit.uri)
    scan.records.sort(key=lambda hit: hit.time, reverse=True)
    return FindResult(
        window=prepared.filter.window,
        days=prepared.dates,
        pages=tuple(scan.pages),
        records=tuple(scan.records),
        volumes=tuple(scan.volumes),
        scanned=scan.scanned,
        matched=scan.matched,
        truncated=scan.truncated,
        index=prepared.lexical.diagnostic,
    )


def _validate_find_position(position: FindPosition, *, counting: bool) -> None:
    if position == FindPosition():
        return
    if counting:
        if (
            position.phase != FIND_PHASE_VOLUME
            or not position.date
            or position.score != 0
            or position.time
            or position.uri
        ):
            raise ValueError("invalid find continuation position")
        try:
            parsed = datetime.strptime(position.date, "%Y-%m-%d").date()
        except ValueError as error:
            raise ValueError("invalid find continuation position") from error
        if parsed.isoformat() != position.date:
            raise ValueError("invalid find continuation position")
        return
    if position.phase == FIND_PHASE_PAGE:
        valid = position.score > 0 and bool(position.uri) and not position.time and not position.date
    elif position.phase == FIND_PHASE_RECORD:
        valid = position.score == 0 and bool(position.uri) and not position.date
    else:
        valid = False
    if not valid:
        raise ValueError("invalid find continuation position")


def _page_before(left: PageHit, right: PageHit) -> bool:
    return left.score > right.score or (left.score == right.score and left.uri < right.uri)


def _page_after(hit: PageHit, position: FindPosition) -> bool:
    return hit.score < position.score or (hit.score == position.score and hit.uri > position.uri)


def _record_before(left: RecordHit, right: RecordHit) -> bool:
    return left.time > right.time or (left.time == right.time and left.uri < right.uri)


def _record_after(hit: RecordHit, position: FindPosition) -> bool:
    return hit.time < position.time or (hit.time == position.time and hit.uri > position.uri)


def _volume_before(left: Volume, right: Volume) -> bool:
    if not left.date:
        return False
    if not right.date:
        return True
    return left.date > right.date


def _volume_after(volume: Volume, position: FindPosition) -> bool:
    return not volume.date or volume.date < position.date


def _retain_bounded[T](
    values: list[T],
    value: T,
    capacity: int,
    before: Callable[[T, T], bool],
) -> list[T]:
    """Retain only the earliest values under one deterministic semantic ordering."""

    def compare(left: T, right: T) -> int:
        if before(left, right):
            return -1
        if before(right, left):
            return 1
        return 0

    values.append(value)
    values.sort(key=cmp_to_key(compare))
    del values[capacity:]
    return values


def _write_digest_value(digest: _Digest, value: bytes) -> None:
    digest.update(struct.pack(">Q", len(value)))
    digest.update(value)


@dataclass(slots=True)
class _BoundedScan:
    counting: bool
    after: FindPosition
    capacity: int
    state: _Scan = field(default_factory=_Scan)
    pages: list[PageHit] = field(default_factory=list)
    records: list[RecordHit] = field(default_factory=list)
    volumes: list[Volume] = field(default_factory=list)
    digest: _Digest = field(default_factory=hashlib.sha256)

    def hash_value(self, kind: str, value: object) -> None:
        _write_digest_value(self.digest, kind.encode())
        _write_digest_value(self.digest, dumps(value))

    def add_page(self, hit: PageHit) -> None:
        self.hash_value(FIND_PHASE_PAGE, hit)
        if self.after.phase == FIND_PHASE_RECORD or (
            self.after.phase == FIND_PHASE_PAGE and not _page_after(hit, self.after)
        ):
            return
        _retain_bounded(self.pages, hit, self.capacity, _page_before)

    def add_record(self, hit: RecordHit) -> None:
        self.hash_value(FIND_PHASE_RECORD, hit)
        if self.after.phase == FIND_PHASE_RECORD and not _record_after(hit, self.after):
            return
        _retain_bounded(self.records, hit, self.capacity, _record_before)

    def add_volume(self, volume: Volume) -> None:
        self.hash_value(FIND_PHASE_VOLUME, volume)
        if self.after.phase == FIND_PHASE_VOLUME and not _volume_after(volume, self.after):
            return
        _retain_bounded(self.volumes, volume, self.capacity, _volume_before)

    def compose(self, prepared: _Prepared, limit: int) -> BoundedFindResult:
        pages: tuple[PageHit, ...] = ()
        records: tuple[RecordHit, ...] = ()
        volumes: tuple[Volume, ...] = ()
        position: FindPosition | None = None
        if self.counting:
            volumes = tuple(self.volumes[:limit])
            if len(self.volumes) > len(volumes):
                position = FindPosition(FIND_PHASE_VOLUME, date=volumes[-1].date)
        else:
            pages = tuple(self.pages[:limit])
            remaining = limit - len(pages)
            records = tuple(self.records[:remaining])
            more = len(self.pages) > len(pages) or len(self.records) > len(records)
            if more and records:
                position = FindPosition(FIND_PHASE_RECORD, time=records[-1].time, uri=records[-1].uri)
            elif more:
                position = FindPosition(FIND_PHASE_PAGE, score=pages[-1].score, uri=pages[-1].uri)
        result = FindResult(
            window=prepared.filter.window,
            pages=pages,
            records=records,
            volumes=volumes,
            scanned=self.state.scanned,
            matched=self.state.matched,
            truncated=position is not None,
            index=prepared.lexical.diagnostic,
        )
        return BoundedFindResult(result, self.digest.hexdigest(), position)


def find_bounded(
    base: Base,
    filters: FindFilter,
    *,
    counting: bool,
    limit: int,
    after: FindPosition | None = None,
    cancel: Cancellation | None = None,
) -> BoundedFindResult:
    """Scan exhaustively while retaining only ``limit + 1`` candidates per phase."""

    if limit <= 0 or limit > MAX_FIND_PAGE_LIMIT:
        raise ValueError(f"bounded find limit must be between 1 and {MAX_FIND_PAGE_LIMIT}")
    position = after or FindPosition()
    _validate_find_position(position, counting=counting)
    forced_reason = ""
    for attempt in range(3):
        prepared = _prepare(base, filters, cancel, forced_index_reason=forced_reason)
        bounded = _BoundedScan(counting, position, limit + 1)
        bounded.digest.update(b"fkf-bounded-find-v1\x00")
        bounded.hash_value("window", filters.window)
        scan_error: Exception | None = None
        try:
            _traverse(
                base,
                prepared,
                bounded.state,
                counting=counting,
                on_page=bounded.add_page,
                on_record=bounded.add_record,
                on_volume=bounded.add_volume,
                cancel=cancel,
            )
        except Exception as error:
            scan_error = error
        if prepared.lexical.current is None or prepared.lexical.current():
            if scan_error is not None:
                raise scan_error
            _check_canceled(cancel)
            bounded.hash_value(
                "counters",
                {"scanned": bounded.state.scanned, "matched": bounded.state.matched},
            )
            return bounded.compose(prepared, limit)
        forced_reason = "stale"
        if attempt == 2:
            break
    raise OperationalError("find inputs kept changing while they were read; retry after the writer finishes")


def find(
    base: Base,
    filters: FindFilter | None = None,
    *,
    counting: bool = False,
    cancel: Cancellation | None = None,
) -> FindResult:
    """Scan every admitted document and page without executing any source command."""
    selected = filters or FindFilter()
    forced_reason = ""
    for attempt in range(3):
        prepared = _prepare(base, selected, cancel, forced_index_reason=forced_reason)
        result: FindResult | None = None
        scan_error: Exception | None = None
        try:
            result = _scan(base, prepared, counting=counting, cancel=cancel)
        except Exception as error:
            scan_error = error
        if prepared.lexical.current is None or prepared.lexical.current():
            if scan_error is not None:
                raise scan_error
            _check_canceled(cancel)
            if result is None:
                raise RuntimeError("find scan completed without a result")
            return result
        # Candidate bytes or fallback inputs moved during the scan. Re-run exhaustively;
        # derived state may optimize a read but can never decide its semantic answer. Check
        # generation before surfacing a scan error because that error may be a torn read.
        forced_reason = "stale"
        if attempt == 2:
            break
    raise OperationalError("find inputs kept changing while they were read; retry after the writer finishes")


WhereClause = Where
DayVolume = Volume
FindRecord = RecordHit
IndexDiagnostic = LexicalIndexUse

__all__ = [
    "DEFAULT_FIND_DAYS",
    "DEFAULT_FIND_LIMIT",
    "FIND_PHASE_PAGE",
    "FIND_PHASE_RECORD",
    "FIND_PHASE_VOLUME",
    "MAX_FIND_PAGE_LIMIT",
    "NO_FIND_LIMIT",
    "BoundedFindResult",
    "DayVolume",
    "FindFilter",
    "FindPosition",
    "FindRecord",
    "FindResult",
    "IndexDiagnostic",
    "PageHit",
    "RecordHit",
    "SourceCount",
    "Volume",
    "Where",
    "WhereClause",
    "compact_find_result",
    "find",
    "find_bounded",
    "parse_where",
]
