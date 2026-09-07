"""Universal bounded URI resolution with one explicit record-body execution path."""

from __future__ import annotations

import os
import posixpath
from dataclasses import dataclass, field
from typing import TYPE_CHECKING, Final

from fkf.base import Base
from fkf.documents import Document, Record, encode_document
from fkf.errors import InvalidUsageError, OperationalError
from fkf.jsoncodec import JsonValue, dumps, loads
from fkf.markdown import Page, parse_page
from fkf.pages import load_markdown_layer, read_page
from fkf.process import Cancellation, check_cancel
from fkf.selector import apply_selector
from fkf.store import (
    GRAPH_DST_FILE,
    GRAPH_FILE,
    GRAPH_GENERATION_FILE,
    GRAPH_META_FILE,
    GRAPH_OFFSETS_FILE,
    MARKDOWN_EXTENSION,
    MAX_NARRATIVE_BYTES,
    Layer,
    NotAddressableError,
    UnsafePathError,
)
from fkf.uri import URI, Scheme, URIError, anchor_slug, parse_uri

if TYPE_CHECKING:
    from fkf.graph import Edge
    from fkf.scan import ScanGuard

MAX_SUGGESTIONS: Final = 5
MAX_SUGGESTION_SCAN: Final = 2000
DEFAULT_NEIGHBOUR_LIMIT: Final = 100
_GRAPH_ARTIFACTS: Final = frozenset(
    {GRAPH_FILE, GRAPH_DST_FILE, GRAPH_OFFSETS_FILE, GRAPH_META_FILE, GRAPH_GENERATION_FILE}
)


@dataclass(frozen=True, slots=True)
class ReadOptions:
    """Tune bounded listing, entity continuation, and explicit body fetching."""

    body: bool = False
    limit: int = 0
    offset: int = 0


@dataclass(frozen=True, slots=True)
class EntityView:
    """The validated graph evidence held locally for one entity or external URL."""

    uri: str
    scheme: Scheme | str
    value: str
    neighbours: tuple[Edge, ...] = ()
    neighbours_truncated: bool = field(default=False, metadata={"json": "neighbours_truncated,omitempty"})


@dataclass(frozen=True, slots=True)
class ReadResult:
    """One resolved URI with its Go-compatible payload and optional body evidence."""

    uri: str
    kind: str
    source: str = field(default="", metadata={"json": "source,omitempty"})
    date: str = field(default="", metadata={"json": "date,omitempty"})
    document: Document | None = field(default=None, metadata={"json": "document,omitempty"})
    record: Record | None = field(default=None, metadata={"json": "record,omitempty"})
    page: Page | None = field(default=None, metadata={"json": "page,omitempty"})
    text: str | None = field(default=None, metadata={"json": "text,omitempty"})
    entries: tuple[str, ...] | None = field(default=None, metadata={"json": "entries,omitempty"})
    selection: JsonValue | None = field(default=None, metadata={"json": "selection,omitempty"})
    entity: EntityView | None = field(default=None, metadata={"json": "entity,omitempty"})
    body: str | None = field(default=None, metadata={"json": "body,omitempty"})
    body_state: str = field(default="", metadata={"json": "body_state,omitempty"})
    snapshot_sha256: str = field(default="", repr=False, compare=False, metadata={"json": "-"})

    def __post_init__(self) -> None:
        if self.kind in {"page", "section"}:
            if self.page is None or self.text is None:
                raise ValueError("a Markdown read result must carry both page metadata and rendered text")
            if any(payload is not None for payload in (self.document, self.record, self.entries, self.entity)):
                raise ValueError("a Markdown read result cannot carry another primary payload")
            if self.selection is not None:
                raise ValueError("a Markdown read result cannot carry a selection payload")
            return
        # JSON null is a valid selector result, so ``kind`` records presence for that one
        # payload while ``None`` continues to mean absence for every other result shape.
        payloads = (self.document, self.record, self.page, self.text, self.entries, self.entity)
        selection_present = self.kind in {"index", "selection"}
        if sum(payload is not None for payload in payloads) + selection_present != 1:
            raise ValueError("a read result must carry exactly one primary payload")
        if not selection_present and self.selection is not None:
            raise ValueError("a non-selection read result cannot carry a selection payload")
        if self.body_state and self.body is None:
            raise ValueError("a read result with body_state must carry body text")


def read(
    base: Base,
    raw: str,
    options: ReadOptions | None = None,
    *,
    cancel: Cancellation | None = None,
    scan: ScanGuard | None = None,
) -> ReadResult:
    """Resolve any published FKF URI and decorate only genuine misses with suggestions."""

    check_cancel(cancel)
    options = options or ReadOptions()
    if options.limit < 0:
        raise InvalidUsageError("read limit must be non-negative")
    if options.offset < 0:
        raise InvalidUsageError("read offset must be non-negative")
    try:
        return _resolve_read(base, parse_uri(raw), options, cancel, scan)
    except Exception as error:
        if not _names_nothing(error):
            raise
        suggestions = suggest_uris(base, raw, cancel=cancel)
        if not suggestions:
            raise
        message = f"{error}\ndid you mean:\n  {'\n  '.join(suggestions)}\n(every listing prints the URI to pass here)"
        if isinstance(error, InvalidUsageError):
            raise InvalidUsageError(message, cause=error) from error
        raise OSError(message) from error


def _resolve_read(
    base: Base,
    uri: URI,
    options: ReadOptions,
    cancel: Cancellation | None,
    scan: ScanGuard | None,
) -> ReadResult:
    if uri.scheme == Scheme.EXTERNAL:
        if options.body:
            raise InvalidUsageError(f"--body fetches a collected record; {uri} is an external graph node")
        return _read_entity(base, uri, options, cancel)
    if uri.is_entity():
        if options.body:
            raise InvalidUsageError(f"--body fetches a collected record; {uri} is an entity")
        return _read_entity(base, uri, options, cancel)
    if uri.directory:
        if options.body:
            raise InvalidUsageError(f"--body fetches one record; {uri} names a directory")
        return _read_directory(base, uri, options, cancel, scan)
    if uri.path in _GRAPH_ARTIFACTS:
        if options.body:
            raise InvalidUsageError(f"--body fetches a collected record; {uri} is derived")
        return _read_graph_artifact(base, uri, cancel)
    if uri.path.endswith(".json"):
        return _read_json(base, uri, options, cancel)
    if options.body:
        raise InvalidUsageError(f"--body fetches a collected record; {uri} is not a stored document")
    return _read_text(base, uri, cancel)


def _read_directory(
    base: Base,
    uri: URI,
    options: ReadOptions,
    cancel: Cancellation | None,
    scan: ScanGuard | None,
) -> ReadResult:
    check_cancel(cancel)
    absolute = base.store.resolve(uri.path)
    try:
        iterator = os.scandir(absolute)
    except OSError as error:
        raise OSError(f"list {uri}: {error}") from error
    entries: list[str] = []
    with iterator:
        for entry in iterator:
            check_cancel(cancel)
            if scan is not None:
                scan.visit()
            if entry.name.startswith("."):
                continue
            child = posixpath.join(uri.path, entry.name)
            try:
                base.store.resolve(child)
            except OSError, ValueError, NotAddressableError, UnsafePathError:
                continue
            if entry.is_dir(follow_symlinks=False):
                child += "/"
            if scan is not None:
                scan.retain(child)
            entries.append(child)
    entries.sort()
    selected = entries[options.offset :]
    if options.limit:
        selected = selected[: options.limit]
    return ReadResult(uri=str(uri), kind="directory", entries=tuple(selected))


def _read_json(base: Base, uri: URI, options: ReadOptions, cancel: Cancellation | None) -> ReadResult:
    check_cancel(cancel)
    document = base.read_document(uri.path)
    check_cancel(cancel)
    record: Record | None = None
    payload: JsonValue
    if uri.fragment:
        record = document.find_record(uri.fragment)
        if record is None:
            raise OperationalError(
                f"{uri.path} holds no record with id {uri.fragment!r} at its declared fields.id paths "
                f"{document.fields.paths('id')}"
            )
        payload = record
    else:
        if options.body:
            raise InvalidUsageError(f"--body fetches one record; add #<id> to name it (for example {uri.path}#<id>)")
        decoded = loads(encode_document(document))
        payload = decoded

    body: str | None = None
    body_state = ""
    if options.body:
        if record is None:
            raise RuntimeError("body resolution reached a document without a record")
        body, body_state = _read_body(base, document, record, str(uri), cancel)

    if uri.jq:
        selection = loads(apply_selector(uri.jq, payload, max_output_bytes=MAX_NARRATIVE_BYTES))
        check_cancel(cancel)
        return ReadResult(
            uri=str(uri),
            kind="selection",
            source=document.source,
            date=document.date,
            selection=selection,
            body=body,
            body_state=body_state,
        )
    if record is not None:
        return ReadResult(
            uri=str(uri),
            kind="record",
            source=document.source,
            date=document.date,
            record=record,
            body=body,
            body_state=body_state,
        )
    return ReadResult(uri=str(uri), kind="document", source=document.source, date=document.date, document=document)


def _read_body(
    base: Base,
    document: Document,
    record: Record,
    uri: str,
    cancel: Cancellation | None,
) -> tuple[str, str]:
    # Keeping this import on the explicit flag path makes the offline execution boundary
    # mechanically visible: no ordinary read even imports body-fetch orchestration.
    from fkf.bodies import read_or_fetch_body

    body, state = read_or_fetch_body(base, document, record, uri, cancel=cancel)
    source = base.source(document.source)
    if state == "fetched" and source.caches_bodies():
        state = "fetched-and-cached"
    return body, state


def _read_text(base: Base, uri: URI, cancel: Cancellation | None) -> ReadResult:
    check_cancel(cancel)
    if uri.jq:
        raise InvalidUsageError(f"?jq= applies to a JSON document; {uri.path} is not one")
    if uri.path.endswith(MARKDOWN_EXTENSION):
        page = read_page(base, uri.path, cancel=cancel)
        if not uri.fragment:
            return ReadResult(uri=str(uri), kind="page", page=page, text=page.body)
        section = section_of(page, uri.fragment, cancel=cancel)
        if section is None:
            anchors = ", ".join(anchors_of(page, cancel=cancel))
            raise OperationalError(f"{uri.path} has no heading anchored {uri.fragment!r}; its anchors are {anchors}")
        return ReadResult(uri=str(uri), kind="section", page=page, text=section)
    if uri.fragment:
        raise InvalidUsageError(f"{uri.path} does not support fragments")
    data = base.read_file(uri.path, MAX_NARRATIVE_BYTES)
    check_cancel(cancel)
    try:
        text = data.decode()
    except UnicodeDecodeError as error:
        raise OperationalError(f"{uri.path} is not valid UTF-8", cause=error) from error
    return ReadResult(uri=str(uri), kind="file", text=text)


def section_of(page: Page, anchor: str, *, cancel: Cancellation | None = None) -> str | None:
    """Return one heading through the next same-or-higher heading."""

    lines = page.body.split("\n")
    relative = parse_page(page.uri, page.body.encode()).headings
    for index, heading in enumerate(relative):
        check_cancel(cancel)
        if heading.anchor != anchor:
            continue
        start = min(max(0, heading.line - 1), len(lines))
        end = len(lines)
        for later in relative[index + 1 :]:
            check_cancel(cancel)
            if later.level <= heading.level:
                end = min(max(0, later.line - 1), len(lines))
                break
        return "\n".join(lines[start:end]).rstrip("\n")
    return None


def anchors_of(page: Page, *, cancel: Cancellation | None = None) -> tuple[str, ...]:
    anchors = []
    for heading in page.headings:
        check_cancel(cancel)
        anchors.append(heading.anchor)
    return tuple(anchors)


def _read_entity(base: Base, uri: URI, options: ReadOptions, cancel: Cancellation | None) -> ReadResult:
    # Graph is intentionally lazy: the offline resolver does not create an import cycle with
    # graph extraction, which itself consumes pages and document URIs.
    from fkf import graph

    limit = options.limit or DEFAULT_NEIGHBOUR_LIMIT
    try:
        neighbourhood = graph.neighbours(
            base,
            graph.GraphQuery(
                uri=str(uri),
                direction=graph.Direction.BOTH,
                depth=1,
                offset=options.offset,
                limit=limit,
            ),
            cancel=cancel,
        )
    except graph.DerivedGraphMissingError:
        edges: tuple[Edge, ...] = ()
        truncated = False
        snapshot = ""
    except graph.EdgeValidationError as error:
        raise graph.EdgeValidationError(f"read the neighbourhood of {uri}: {error}") from error
    else:
        if neighbourhood.stats.malformed:
            raise graph.EdgeValidationError(
                f"read the neighbourhood of {uri}: invalid derived graph cache: "
                f"{GRAPH_FILE} has {neighbourhood.stats.malformed} malformed matching row(s); "
                "run `fkf build graph`"
            )
        edges = tuple(item.edge for item in neighbourhood.edges)
        truncated = neighbourhood.truncated
        snapshot = neighbourhood.snapshot_sha256
    entity = EntityView(str(uri), uri.scheme, uri.value, edges, truncated)
    return ReadResult(uri=str(uri), kind="entity", entity=entity, snapshot_sha256=snapshot)


def _read_graph_artifact(base: Base, uri: URI, cancel: Cancellation | None) -> ReadResult:
    from fkf import graph

    check_cancel(cancel)
    if uri.fragment:
        raise InvalidUsageError(f"{uri.path} does not support fragments")
    if uri.path == GRAPH_GENERATION_FILE:
        try:
            payload = loads(base.read_file(uri.path, graph.MAX_GRAPH_GENERATION_BYTES).strip())
        except (OSError, ValueError) as error:
            raise graph.EdgeValidationError(
                f"invalid derived graph cache: {GRAPH_GENERATION_FILE} is not one JSON document; run `fkf build graph`"
            ) from error
        return _json_selection(uri, payload)
    if uri.path == GRAPH_META_FILE:
        payload = loads(dumps(graph.read_validated_graph_meta(base, cancel=cancel)))
        return _json_selection(uri, payload)
    if uri.jq:
        raise InvalidUsageError(f"?jq= applies to a JSON document; {uri.path} is not one")
    data = graph.read_validated_graph_artifact(base, uri.path, MAX_NARRATIVE_BYTES, cancel=cancel)
    check_cancel(cancel)
    try:
        text = data.decode()
    except UnicodeDecodeError as error:
        raise graph.EdgeValidationError(f"invalid derived graph cache: {uri.path} is not valid UTF-8") from error
    return ReadResult(uri=str(uri), kind="file", text=text)


def _json_selection(uri: URI, payload: JsonValue) -> ReadResult:
    if not uri.jq:
        return ReadResult(uri=str(uri), kind="index", selection=payload)
    selected = loads(apply_selector(uri.jq, payload, max_output_bytes=MAX_NARRATIVE_BYTES))
    return ReadResult(uri=str(uri), kind="selection", selection=selected)


@dataclass(frozen=True, slots=True)
class _Suggestion:
    uri: str
    exact: bool


def suggest_uris(base: Base, raw: str, *, cancel: Cancellation | None = None) -> tuple[str, ...]:
    """Return at most five substring near-misses drawn only from published URIs."""

    needle = raw.strip().lower().partition("#")[0]
    if not needle:
        return ()
    stem = _stem_of(needle)
    found: list[_Suggestion] = []

    def consider(uri: str) -> None:
        check_cancel(cancel)
        if len(found) >= MAX_SUGGESTION_SCAN:
            return
        lowered = uri.lower()
        if needle not in lowered and stem not in lowered:
            return
        found.append(_Suggestion(uri, _stem_of(lowered) == stem))

    for layer in (Layer.WIKI, Layer.PROJECTS):
        if not base.store.enabled(layer):
            continue
        try:
            pages, _nested = load_markdown_layer(base, layer, cancel=cancel)
        except OSError, ValueError:
            continue
        for page in pages:
            check_cancel(cancel)
            consider(page.uri)
            for heading in page.headings:
                check_cancel(cancel)
                if not anchor_slug(heading.text) or heading.anchor == _stem_of(page.uri):
                    continue
                consider(f"{page.uri}#{heading.anchor}")
    if base.store.enabled(Layer.INDEX):
        try:
            names = base.index_documents()
        except OSError, ValueError:
            names = ()
        for name in names:
            check_cancel(cancel)
            consider(f"index/{name}.json")
    if base.store.enabled(Layer.EVENTS):
        try:
            dates = base.event_dates()
        except OSError, ValueError:
            dates = ()
        for value in dates:
            check_cancel(cancel)
            consider(f"events/{value}/")
    found.sort(key=lambda item: (not item.exact, len(item.uri), item.uri))
    return tuple(item.uri for item in found[:MAX_SUGGESTIONS])


def _names_nothing(error: BaseException) -> bool:
    seen: set[int] = set()
    current: BaseException | None = error
    while current is not None and id(current) not in seen:
        seen.add(id(current))
        if isinstance(current, (FileNotFoundError, URIError, NotAddressableError)):
            return True
        current = current.__cause__ or current.__context__
    return False


def _stem_of(uri: str) -> str:
    trimmed = uri.removesuffix("/").rsplit("/", 1)[-1]
    head, separator, _suffix = trimmed.rpartition(".")
    return head if separator and head else trimmed


__all__ = [
    "DEFAULT_NEIGHBOUR_LIMIT",
    "MAX_SUGGESTIONS",
    "MAX_SUGGESTION_SCAN",
    "EntityView",
    "ReadOptions",
    "ReadResult",
    "anchors_of",
    "read",
    "section_of",
    "suggest_uris",
]
