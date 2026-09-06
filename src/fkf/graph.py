"""Deterministic, integrity-bound graph extraction and offline neighbourhood reads."""

from __future__ import annotations

import hashlib
import json
import os
import stat
import struct
import unicodedata
from collections.abc import Callable, Iterable, Sequence
from dataclasses import dataclass, field, replace
from datetime import UTC, date, datetime
from enum import StrEnum
from pathlib import Path
from typing import TYPE_CHECKING, BinaryIO, Final, Protocol

from fkf.config import IdentityKind, validate_identity_alias
from fkf.documents import Document, Record, event_document_uri, index_document_uri
from fkf.fields import FIELD_TIME, validate_entity_uri, validate_relation_value
from fkf.io import FileTooLargeError, atomic_write, open_regular_file, read_file_limited, write_json
from fkf.markdown import Page
from fkf.pages import load_markdown_layer, read_page
from fkf.process import Cancellation, check_cancel
from fkf.source_runtime import normalize_github_noreply_actor
from fkf.store import (
    BASE_FILE_MODE,
    GRAPH_DST_FILE,
    GRAPH_FILE,
    GRAPH_GENERATION_FILE,
    GRAPH_META_FILE,
    GRAPH_OFFSETS_FILE,
    MARKDOWN_EXTENSION,
    MAX_NARRATIVE_BYTES,
    MAX_SOURCE_DOCUMENT_BYTES,
    TASK_TRACE_FILE,
    Layer,
    UnsafePathError,
)
from fkf.timeutil import parse_record_time
from fkf.uri import URI, Scheme, encode_fragment, parse_uri, resolve_link

if TYPE_CHECKING:
    from fkf.base import Base

EDGE_SCHEMA_VERSION: Final = 3
GRAPH_EXTRACTOR_VERSION: Final = 2
EDGE_FIELD_SEPARATOR: Final = "\t"
EDGE_FIELD_COUNT: Final = 6
EDGE_COLUMNS: Final[tuple[str, ...]] = ("src", "dst", "kind", "at", "via", "indexed")
MAX_EDGE_LINE_BYTES: Final = 64 << 10
MAX_EDGE_LIST_ROWS: Final = 5_000_000
MAX_GRAPH_GENERATION_BYTES: Final = 512


class EdgeValidationError(ValueError):
    """An edge or encoded edge list violates its durable storage contract."""


@dataclass(frozen=True, slots=True)
class Edge:
    """One provenance-bearing relationship in durable TSV column order."""

    src: str
    dst: str
    kind: str
    at: str = field(default="", metadata={"json": "at,omitempty"})
    via: str = ""
    indexed: str = field(default="", metadata={"json": "indexed,omitempty"})

    def fields(self) -> tuple[str, str, str, str, str, str]:
        return (self.src, self.dst, self.kind, self.at, self.via, self.indexed)

    def sort_key(self) -> tuple[str, str, str, str, str]:
        """Return the canonical key, deliberately excluding build time."""

        return (self.src, self.dst, self.kind, self.at, self.via)

    def encoded_row_bytes(self) -> int:
        return sum(len(value.encode()) for value in self.fields()) + EDGE_FIELD_COUNT

    def validate(self) -> None:
        """Reject rows that cannot be audited and decoded without escaping."""

        required = {"src": self.src, "dst": self.dst, "kind": self.kind, "via": self.via}
        for name in ("src", "dst", "kind", "via"):
            if not required[name].strip():
                raise EdgeValidationError(f"edge is missing a required field: {name}")
        for name, value in zip(EDGE_COLUMNS, self.fields(), strict=True):
            if any(separator in value for separator in "\t\n\r"):
                raise EdgeValidationError(f"edge field contains a separator byte: {name}")
            for character in value:
                if unicodedata.category(character) in {"Cc", "Cf"}:
                    raise EdgeValidationError(
                        f"edge field contains a control or invisible character: {name} contains U+{ord(character):04X}"
                    )
        if self.at and not _valid_edge_fact_time(self.at):
            raise EdgeValidationError(f"edge timestamp is not canonical: at {self.at!r}")
        if self.indexed and not _valid_canonical_edge_time(self.indexed):
            raise EdgeValidationError(f"edge timestamp is not canonical: indexed {self.indexed!r}")


def _valid_edge_fact_time(value: str) -> bool:
    try:
        return date.fromisoformat(value).isoformat() == value
    except ValueError:
        return _valid_canonical_edge_time(value)


def _valid_canonical_edge_time(value: str) -> bool:
    if not value.endswith("Z"):
        return False
    try:
        parsed = datetime.fromisoformat(value.removesuffix("Z") + "+00:00")
    except ValueError:
        return False
    return parsed.microsecond == 0 and parsed.astimezone(UTC).strftime("%Y-%m-%dT%H:%M:%SZ") == value


def sort_edges(edges: Iterable[Edge]) -> list[Edge]:
    """Return rows in canonical source-first order without mutating the caller."""

    return sorted(edges, key=Edge.sort_key)


def dedupe_edges(edges: Iterable[Edge]) -> list[Edge]:
    """Keep the first row for each fact key, ignoring only indexed build time."""

    seen: set[tuple[str, str, str, str, str]] = set()
    unique: list[Edge] = []
    for edge in edges:
        key = edge.sort_key()
        if key in seen:
            continue
        seen.add(key)
        unique.append(edge)
    return unique


def _canonical_edges(edges: Iterable[Edge]) -> list[Edge]:
    rows = dedupe_edges(sort_edges(edges))
    if len(rows) > MAX_EDGE_LIST_ROWS:
        raise EdgeValidationError(
            f"edge list exceeds the row limit: {len(rows)} rows exceeds the maximum of {MAX_EDGE_LIST_ROWS}"
        )
    for index, edge in enumerate(rows):
        try:
            edge.validate()
        except EdgeValidationError as error:
            raise EdgeValidationError(f"edge row {index}: {error}") from error
        size = edge.encoded_row_bytes()
        if size > MAX_EDGE_LINE_BYTES:
            raise EdgeValidationError(
                f"edge line exceeds the record size limit: edge row {index} encodes to {size} bytes "
                f"including its newline; maximum is {MAX_EDGE_LINE_BYTES}"
            )
    return rows


def _encode_ordered_edges(rows: Sequence[Edge]) -> bytes:
    return b"".join((EDGE_FIELD_SEPARATOR.join(edge.fields()) + "\n").encode() for edge in rows)


def encode_edges(edges: Iterable[Edge]) -> bytes:
    """Preflight, sort, deduplicate, and encode canonical durable TSV bytes."""

    return _encode_ordered_edges(_canonical_edges(edges))


def decode_edge(line: bytes) -> Edge | None:
    """Decode one valid row, returning ``None`` for malformed input."""

    fields = line.split(b"\t")
    if len(fields) != EDGE_FIELD_COUNT:
        return None
    try:
        values = tuple(field.decode() for field in fields)
        edge = Edge(*values)
        edge.validate()
    except UnicodeDecodeError, EdgeValidationError:
        return None
    return edge


@dataclass(frozen=True, slots=True)
class EdgeQuery:
    """Exact optional selectors over the first three edge columns."""

    src: str = ""
    dst: str = ""
    kind: str = ""

    def matches(self, edge: Edge) -> bool:
        return (
            (not self.src or edge.src == self.src)
            and (not self.dst or edge.dst == self.dst)
            and (not self.kind or edge.kind == self.kind)
        )


@dataclass(slots=True)
class EdgeScanStats:
    """Bounded streaming-scan accounting."""

    lines: int = 0
    matched: int = 0
    malformed: int = 0

    def add(self, other: EdgeScanStats) -> None:
        self.lines += other.lines
        self.matched += other.matched
        self.malformed += other.malformed


class _LineReader(Protocol):
    def readline(self, size: int = -1, /) -> bytes: ...


def scan_edges(
    reader: _LineReader,
    query: EdgeQuery,
    visit: Callable[[Edge], None] | None = None,
    *,
    cancel: Cancellation | None = None,
) -> tuple[list[Edge], EdgeScanStats]:
    """Stream bounded rows; collect only when no visitor is supplied."""

    found: list[Edge] = []
    stats = EdgeScanStats()
    prefix = (query.src + EDGE_FIELD_SEPARATOR).encode() if query.src else b""
    contains = [f"\t{value}\t".encode() for value in (query.dst, query.kind) if value]
    while True:
        check_cancel(cancel)
        raw = reader.readline(MAX_EDGE_LINE_BYTES + 1)
        if not raw:
            break
        if len(raw) > MAX_EDGE_LINE_BYTES:
            raise EdgeValidationError(f"edge line exceeds the record size limit: line {stats.lines + 1}")
        stats.lines += 1
        if stats.lines > MAX_EDGE_LIST_ROWS:
            raise EdgeValidationError("edge list exceeds the row limit")
        line = (raw[:-1] if raw.endswith(b"\n") else raw).rstrip(b"\r")
        if not line:
            continue
        if prefix and not line.startswith(prefix):
            continue
        if any(token not in line for token in contains):
            continue
        edge = decode_edge(line)
        if edge is None:
            stats.malformed += 1
            continue
        if not query.matches(edge):
            continue
        stats.matched += 1
        if visit is None:
            found.append(edge)
        else:
            visit(edge)
    return found, stats


@dataclass(frozen=True, slots=True)
class GraphArtifacts:
    """The source-sorted rows, destination twin, and binary-search offset table."""

    src: bytes
    dst: bytes
    offsets: bytes


@dataclass(frozen=True, slots=True)
class GraphOffset:
    direction: str
    node: str
    start: int
    bytes: int

    def key(self) -> str:
        return f"{self.direction}\t{self.node}"


def _graph_offsets_for_rows(rows: Sequence[Edge], direction: str, node_of: Callable[[Edge], str]) -> list[GraphOffset]:
    offsets: list[GraphOffset] = []
    position = 0
    index = 0
    while index < len(rows):
        node = node_of(rows[index])
        start = position
        while index < len(rows) and node_of(rows[index]) == node:
            position += rows[index].encoded_row_bytes()
            index += 1
        offsets.append(GraphOffset(direction=direction, node=node, start=start, bytes=position - start))
    return offsets


def _encode_graph_offsets(offsets: Iterable[GraphOffset]) -> bytes:
    ordered = sorted(offsets, key=GraphOffset.key)
    previous = ""
    output: list[str] = []
    for offset in ordered:
        if (
            offset.key() <= previous
            or offset.direction not in {"src", "dst"}
            or offset.start < 0
            or offset.bytes <= 0
            or not offset.node
            or any(character in offset.node for character in "\t\n\r")
        ):
            raise EdgeValidationError(f"invalid graph offset for {offset.node!r}")
        previous = offset.key()
        output.append(f"{offset.direction}\t{offset.node}\t{offset.start}\t{offset.bytes}\n")
    return "".join(output).encode()


def decode_graph_offset(line: bytes) -> GraphOffset:
    """Decode one strict offset row."""

    fields = line.split(b"\t")
    if len(fields) != 4:
        raise EdgeValidationError("offset row has the wrong field count")
    try:
        direction, node = fields[0].decode(), fields[1].decode()
        start, length = int(fields[2]), int(fields[3])
    except (UnicodeDecodeError, ValueError) as error:
        raise EdgeValidationError("offset row has an invalid value") from error
    if direction not in {"src", "dst"} or not node:
        raise EdgeValidationError("offset row has an invalid key")
    if start < 0:
        raise EdgeValidationError("offset row has an invalid start")
    if length <= 0:
        raise EdgeValidationError("offset row has an invalid byte length")
    return GraphOffset(direction=direction, node=node, start=start, bytes=length)


def encode_graph_artifacts(edges: Iterable[Edge]) -> GraphArtifacts:
    """Build all three canonical seek artifacts from one edge sequence."""

    rows = _canonical_edges(edges)
    src = _encode_ordered_edges(rows)
    src_offsets = _graph_offsets_for_rows(rows, "src", lambda edge: edge.src)
    dst_rows = sorted(rows, key=lambda edge: (edge.dst, edge.src, edge.kind, edge.at, edge.via))
    dst = _encode_ordered_edges(dst_rows)
    dst_offsets = _graph_offsets_for_rows(dst_rows, "dst", lambda edge: edge.dst)
    return GraphArtifacts(src=src, dst=dst, offsets=_encode_graph_offsets((*src_offsets, *dst_offsets)))


@dataclass(frozen=True, slots=True)
class GraphFileManifest:
    uri: str
    bytes: int
    modified_unix_nano: int
    sha256: str


@dataclass(frozen=True, slots=True)
class GraphInputSHA256:
    aggregate: str = field(metadata={"json": "AGGREGATE"})
    events: str = ""
    index: str = ""
    projects: str = ""
    tasks: str = ""
    wiki: str = ""
    schema: str = ""

    def named(self, name: str) -> str:
        return {
            "events": self.events,
            "index": self.index,
            "projects": self.projects,
            "tasks": self.tasks,
            "wiki": self.wiki,
            "schema": self.schema,
        }.get(name, "")


@dataclass(frozen=True, slots=True)
class GraphOutputSHA256:
    graph_tsv: str = field(metadata={"json": GRAPH_FILE})
    graph_dst_tsv: str = field(metadata={"json": GRAPH_DST_FILE})
    graph_offsets_tsv: str = field(metadata={"json": GRAPH_OFFSETS_FILE})


@dataclass(frozen=True, slots=True)
class GraphSHA256Manifest:
    inputs: GraphInputSHA256
    outputs: GraphOutputSHA256


@dataclass(frozen=True, slots=True)
class EdgeListMeta:
    schema_version: int
    extractor_version: int
    columns: tuple[str, ...]
    separator: str
    generated_at: str
    edges: int
    extractors: tuple[str, ...]
    bytes: int
    kinds: tuple[str, ...]
    inputs: tuple[GraphFileManifest, ...]
    outputs: tuple[GraphFileManifest, ...]
    sha256: GraphSHA256Manifest


_GRAPH_INPUT_NAMES: Final[tuple[str, ...]] = ("events", "index", "projects", "tasks", "wiki", "schema")


class _Digest(Protocol):
    def update(self, data: bytes, /) -> None: ...


def _write_digest_value(digest: _Digest, value: str) -> None:
    encoded = value.encode()
    digest.update(struct.pack(">Q", len(encoded)))
    digest.update(encoded)


def _canonical_sha256(value: str) -> bool:
    if len(value) != 64 or value != value.lower():
        return False
    try:
        bytes.fromhex(value)
    except ValueError:
        return False
    return True


def _aggregate_graph_inputs_sha256(inputs: GraphInputSHA256) -> str:
    digest = hashlib.sha256(b"fkf-graph-inputs-v3\x00")
    _write_digest_value(digest, "extractor_version")
    _write_digest_value(digest, str(GRAPH_EXTRACTOR_VERSION))
    for name in _GRAPH_INPUT_NAMES:
        _write_digest_value(digest, name)
        _write_digest_value(digest, inputs.named(name))
    return digest.hexdigest()


def _graph_input_digest_problems(inputs: GraphInputSHA256) -> list[str]:
    problems = [
        f"{name} input digest {inputs.named(name)!r} is not a lowercase SHA-256 digest"
        for name in _GRAPH_INPUT_NAMES
        if not _canonical_sha256(inputs.named(name))
    ]
    if not _canonical_sha256(inputs.aggregate):
        problems.append(f"AGGREGATE input digest {inputs.aggregate!r} is not a lowercase SHA-256 digest")
    elif inputs.aggregate != _aggregate_graph_inputs_sha256(inputs):
        problems.append("AGGREGATE input digest does not match extractor_version and named inputs")
    return problems


def new_graph_input_sha256(
    events: str,
    index: str,
    projects: str,
    tasks: str,
    wiki: str,
    schema: str,
) -> GraphInputSHA256:
    """Validate the closed logical-input vocabulary and derive its aggregate."""

    inputs = GraphInputSHA256(
        aggregate="", events=events, index=index, projects=projects, tasks=tasks, wiki=wiki, schema=schema
    )
    for name in _GRAPH_INPUT_NAMES:
        if not _canonical_sha256(inputs.named(name)):
            raise EdgeValidationError(f"{name} input digest {inputs.named(name)!r} is not a lowercase SHA-256 digest")
    return replace(inputs, aggregate=_aggregate_graph_inputs_sha256(inputs))


def _canonical_generated_at(value: datetime) -> tuple[datetime, str]:
    if value.tzinfo is None:
        value = value.replace(tzinfo=UTC)
    canonical = value.astimezone(UTC).replace(microsecond=0)
    return canonical, canonical.strftime("%Y-%m-%dT%H:%M:%SZ")


def _graph_output_manifests(artifacts: GraphArtifacts, generated_at: datetime) -> tuple[GraphFileManifest, ...]:
    canonical, _ = _canonical_generated_at(generated_at)
    modified = int(canonical.timestamp()) * 1_000_000_000
    values = (
        (GRAPH_DST_FILE, artifacts.dst),
        (GRAPH_OFFSETS_FILE, artifacts.offsets),
        (GRAPH_FILE, artifacts.src),
    )
    return tuple(
        GraphFileManifest(
            uri=uri,
            bytes=len(data),
            modified_unix_nano=modified,
            sha256=hashlib.sha256(data).hexdigest(),
        )
        for uri, data in values
    )


def _manifest_by_uri(files: Sequence[GraphFileManifest], uri: str) -> GraphFileManifest | None:
    return next((item for item in files if item.uri == uri), None)


def _graph_file_manifest_problems(files: Sequence[GraphFileManifest], label: str) -> list[str]:
    problems: list[str] = []
    previous = ""
    for item in files:
        if not item.uri or item.uri <= previous:
            problems.append(f"metadata {label} manifest is not strictly URI-sorted")
            break
        previous = item.uri
        if item.bytes < 0:
            problems.append(f"metadata {label} {item.uri} has a negative byte count")
        if not _canonical_sha256(item.sha256):
            problems.append(f"metadata {label} {item.uri} has an invalid SHA-256")
    return problems


def new_edge_list_meta(
    edges: Iterable[Edge],
    generated_at: datetime,
    inputs: GraphInputSHA256,
    *input_files: GraphFileManifest,
) -> EdgeListMeta:
    """Derive the complete sidecar from canonical artifacts and exact inputs."""

    rows = list(edges)
    artifacts = encode_graph_artifacts(rows)
    problems = _graph_input_digest_problems(inputs)
    problems.extend(_graph_file_manifest_problems(input_files, "input"))
    if problems:
        raise EdgeValidationError("; ".join(problems))
    _, generated_text = _canonical_generated_at(generated_at)
    output_files = _graph_output_manifests(artifacts, generated_at)
    output = {item.uri: item.sha256 for item in output_files}
    return EdgeListMeta(
        schema_version=EDGE_SCHEMA_VERSION,
        extractor_version=GRAPH_EXTRACTOR_VERSION,
        columns=EDGE_COLUMNS,
        separator="\\t",
        generated_at=generated_text,
        edges=len(dedupe_edges(rows)),
        extractors=tuple(sorted({edge.via for edge in rows if edge.via})),
        bytes=len(artifacts.src),
        kinds=tuple(sorted({edge.kind for edge in rows})),
        inputs=tuple(input_files),
        outputs=output_files,
        sha256=GraphSHA256Manifest(
            inputs=inputs,
            outputs=GraphOutputSHA256(
                graph_tsv=output[GRAPH_FILE],
                graph_dst_tsv=output[GRAPH_DST_FILE],
                graph_offsets_tsv=output[GRAPH_OFFSETS_FILE],
            ),
        ),
    )


def graph_generation_sha256(meta: EdgeListMeta) -> str:
    """Bind one publication marker to logical inputs and all artifact bytes."""

    digest = hashlib.sha256(b"fkf-graph-generation-v1\x00")
    for value in (
        meta.sha256.inputs.aggregate,
        meta.sha256.outputs.graph_tsv,
        meta.sha256.outputs.graph_dst_tsv,
        meta.sha256.outputs.graph_offsets_tsv,
    ):
        _write_digest_value(digest, value)
    return digest.hexdigest()


@dataclass(frozen=True, slots=True)
class _GraphGenerationState:
    state: str
    generation: str


def _write_graph_generation_state(path: Path, state: str, generation: str) -> None:
    if state not in {"building", "current"}:
        raise EdgeValidationError(f"invalid graph generation state {state!r}")
    if not _canonical_sha256(generation):
        raise EdgeValidationError(f"invalid graph generation digest {generation!r}")
    write_json(_GraphGenerationState(state=state, generation=generation), path)


def _decode_one_json(data: bytes, label: str) -> object:
    try:
        text = data.decode()
        decoder = json.JSONDecoder()
        value, end = decoder.raw_decode(text)
    except (UnicodeDecodeError, json.JSONDecodeError) as error:
        raise EdgeValidationError(f"invalid derived graph cache: decode {label} JSON: {error}") from error
    if text[end:].strip():
        raise EdgeValidationError(f"invalid derived graph cache: {label} holds more than one JSON document")
    return value


def read_current_graph_generation(root: Path) -> str:
    """Read the small strict marker that closes multi-file publication."""

    path = root / GRAPH_GENERATION_FILE
    try:
        value = _decode_one_json(read_file_limited(path, MAX_GRAPH_GENERATION_BYTES), GRAPH_GENERATION_FILE)
    except FileNotFoundError as error:
        raise EdgeValidationError(
            f"invalid derived graph cache: {GRAPH_GENERATION_FILE} does not exist; run `fkf build graph`"
        ) from error
    if not isinstance(value, dict):
        raise EdgeValidationError(f"invalid derived graph cache: {GRAPH_GENERATION_FILE} must hold an object")
    unknown = set(value) - {"state", "generation"}
    if unknown:
        raise EdgeValidationError(
            f"invalid derived graph cache: {GRAPH_GENERATION_FILE} has unknown field {min(unknown)!r}"
        )
    state, generation = value.get("state"), value.get("generation")
    if state != "current":
        raise EdgeValidationError(f"invalid derived graph cache: graph generation is {state!r}, not current")
    if not isinstance(generation, str) or not _canonical_sha256(generation):
        raise EdgeValidationError(
            f"invalid derived graph cache: {GRAPH_GENERATION_FILE} has an invalid generation digest"
        )
    return generation


def _write_graph_artifact(path: Path, data: bytes, generated_at: datetime) -> None:
    atomic_write(path, data, mode=BASE_FILE_MODE)
    canonical, _ = _canonical_generated_at(generated_at)
    modified = int(canonical.timestamp()) * 1_000_000_000
    os.utime(path, ns=(modified, modified))


def write_edge_list(
    path: Path,
    meta_path: Path,
    edges: Sequence[Edge],
    meta: EdgeListMeta,
) -> None:
    """Fail closed while publishing three artifacts, sidecar, then current marker."""

    artifacts = encode_graph_artifacts(edges)
    if not _valid_canonical_edge_time(meta.generated_at):
        raise EdgeValidationError(f"edge-list metadata generated_at {meta.generated_at!r} is not canonical UTC RFC3339")
    problems = _graph_input_digest_problems(meta.sha256.inputs)
    if problems:
        raise EdgeValidationError(f"edge-list metadata {'; '.join(problems)}")
    for index, edge in enumerate(edges):
        if edge.indexed != meta.generated_at:
            raise EdgeValidationError(
                f"edge row {index} indexed {edge.indexed!r} does not match metadata generated_at {meta.generated_at!r}"
            )
    generated = datetime.fromisoformat(meta.generated_at.removesuffix("Z") + "+00:00")
    expected = new_edge_list_meta(edges, generated, meta.sha256.inputs, *meta.inputs)
    if meta != expected:
        raise EdgeValidationError("edge-list metadata does not describe the exact canonical encoded rows")

    generation_path = meta_path.parent / GRAPH_GENERATION_FILE
    generation = graph_generation_sha256(meta)
    _write_graph_generation_state(generation_path, "building", generation)
    _write_graph_artifact(path, artifacts.src, generated)
    _write_graph_artifact(path.parent / GRAPH_DST_FILE, artifacts.dst, generated)
    _write_graph_artifact(path.parent / GRAPH_OFFSETS_FILE, artifacts.offsets, generated)
    write_json(meta, meta_path)
    _write_graph_generation_state(generation_path, "current", generation)


# --- exact graph inputs and extraction -----------------------------------------------------


@dataclass(frozen=True, slots=True)
class GraphInputState:
    """One exact, URI-sorted snapshot of every edge-relevant durable input."""

    sha256: GraphInputSHA256
    files: tuple[GraphFileManifest, ...]


def _graph_input_layer(uri: str) -> Layer | None:
    try:
        return Layer(uri.partition("/")[0])
    except ValueError:
        return None


def _task_graph_input_uris(base: Base, cancel: Cancellation | None = None) -> list[str]:
    root = base.store.directory(Layer.TASKS)
    try:
        days = sorted(
            (entry for entry in os.scandir(root) if entry.is_dir(follow_symlinks=False)),
            key=lambda entry: entry.name,
        )
    except FileNotFoundError:
        return []
    uris: list[str] = []
    for day in days:
        check_cancel(cancel)
        try:
            if date.fromisoformat(day.name).isoformat() != day.name:
                continue
        except ValueError:
            continue
        try:
            slugs = sorted(
                (entry for entry in os.scandir(day.path) if entry.is_dir(follow_symlinks=False)),
                key=lambda entry: entry.name,
            )
        except FileNotFoundError:
            continue
        for slug in slugs:
            check_cancel(cancel)
            uri = f"tasks/{day.name}/{slug.name}/{TASK_TRACE_FILE}"
            if base.exists(uri):
                uris.append(uri)
    return uris


def _authored_graph_input_uris(base: Base, layer: Layer, cancel: Cancellation | None = None) -> list[str]:
    if not base.store.enabled(layer):
        return []
    if layer is Layer.TASKS:
        return _task_graph_input_uris(base, cancel)
    root = base.store.directory(layer)
    try:
        entries = sorted(os.scandir(root), key=lambda entry: entry.name)
    except FileNotFoundError:
        return []
    return [
        f"{layer}/{entry.name}"
        for entry in entries
        if not entry.is_dir(follow_symlinks=False) and entry.name.endswith(MARKDOWN_EXTENSION)
    ]


def graph_input_uris(base: Base, *, cancel: Cancellation | None = None) -> tuple[str, ...]:
    """List the closed set of files whose bytes can affect extracted edges."""

    uris: list[str] = []
    check_cancel(cancel)
    if base.store.enabled(Layer.EVENTS):
        for day in base.event_dates():
            check_cancel(cancel)
            uris.extend(event_document_uri(day, name) for name in base.day_documents(day))
    if base.store.enabled(Layer.INDEX):
        uris.extend(index_document_uri(name) for name in base.index_documents())
    for layer in (Layer.PROJECTS, Layer.TASKS, Layer.WIKI):
        check_cancel(cancel)
        uris.extend(_authored_graph_input_uris(base, layer, cancel))
    return tuple(sorted(uris))


def hash_graph_input(base: Base, uri: str, *, cancel: Cancellation | None = None) -> GraphFileManifest:
    """Hash one descriptor-bound regular graph input and prove it stayed stable."""

    absolute = base.store.resolve(uri)
    check_cancel(cancel)
    limit = MAX_SOURCE_DOCUMENT_BYTES if _graph_input_layer(uri) in {Layer.EVENTS, Layer.INDEX} else MAX_NARRATIVE_BYTES
    try:
        handle = open_regular_file(absolute)
    except (OSError, UnsafePathError) as error:
        raise OSError(f"open graph input {uri}: {error}") from error
    with handle:
        before = os.fstat(handle.fileno())
        if before.st_size > limit:
            raise FileTooLargeError(f"graph input {uri} exceeds {limit} bytes")
        digest = hashlib.sha256()
        total = 0
        while chunk := handle.read(min(1 << 20, limit + 1 - total)):
            check_cancel(cancel)
            total += len(chunk)
            if total > limit:
                raise FileTooLargeError(f"graph input {uri} exceeds {limit} bytes")
            digest.update(chunk)
        after = os.fstat(handle.fileno())
    check_cancel(cancel)
    if (before.st_size, before.st_mtime_ns) != (after.st_size, after.st_mtime_ns):
        raise EdgeValidationError(f"graph input {uri} changed while it was being hashed; retry")
    return GraphFileManifest(
        uri=uri,
        bytes=after.st_size,
        modified_unix_nano=after.st_mtime_ns,
        sha256=digest.hexdigest(),
    )


def _graph_layer_input_sha256(
    layer: Layer,
    files: Sequence[GraphFileManifest],
    cancel: Cancellation | None = None,
) -> str:
    digest = hashlib.sha256(f"fkf-graph-input-{layer}-v2\0".encode())
    for item in files:
        check_cancel(cancel)
        if _graph_input_layer(item.uri) is not layer:
            continue
        _write_digest_value(digest, item.uri)
        _write_digest_value(digest, item.sha256)
    return digest.hexdigest()


def graph_schema_input_sha256(base: Base, *, cancel: Cancellation | None = None) -> str:
    """Digest only schema and identity semantics that influence edge extraction."""

    digest = hashlib.sha256(b"fkf-graph-input-schema-v2\x00")
    for name in base.config.schema.names():
        check_cancel(cancel)
        definition = base.config.schema[name]
        _write_digest_value(digest, name)
        _write_digest_value(digest, str(definition.cardinality))
        _write_digest_value(digest, "relation" if definition.relation else "value")
    for name in sorted(base.config.identities):
        check_cancel(cancel)
        identity = base.config.identities[name]
        _write_digest_value(digest, f"identity:{name}")
        _write_digest_value(digest, identity.canonical)
        effective = identity.effective_kind()
        _write_digest_value(digest, "" if effective is None else str(effective))
        if identity.owner:
            _write_digest_value(digest, "owner")
        for alias in identity.aliases:
            _write_digest_value(digest, alias)
    return digest.hexdigest()


def _graph_input_sha256_from_files(
    files: Sequence[GraphFileManifest],
    schema: str,
    cancel: Cancellation | None = None,
) -> GraphInputSHA256:
    digests = {layer: _graph_layer_input_sha256(layer, files, cancel) for layer in Layer}
    return new_graph_input_sha256(
        digests[Layer.EVENTS],
        digests[Layer.INDEX],
        digests[Layer.PROJECTS],
        digests[Layer.TASKS],
        digests[Layer.WIKI],
        schema,
    )


def graph_input_state(base: Base, *, cancel: Cancellation | None = None) -> GraphInputState:
    """Read the complete graph input snapshot used before and after a build."""

    files: list[GraphFileManifest] = []
    for uri in graph_input_uris(base, cancel=cancel):
        check_cancel(cancel)
        files.append(hash_graph_input(base, uri, cancel=cancel))
    return GraphInputState(
        _graph_input_sha256_from_files(files, graph_schema_input_sha256(base, cancel=cancel), cancel),
        tuple(files),
    )


@dataclass(frozen=True, slots=True)
class ExtractCounts:
    documents: int = 0
    pages: int = 0


def _record_edge_time(document: Document, record: Record) -> str:
    raw = document.fields.eval_string(FIELD_TIME, record)
    if raw is None:
        return ""
    try:
        instant = parse_record_time(raw)
    except ValueError:
        return ""
    # Go's time.RFC3339 layout deliberately omits fractional seconds.
    return instant.to_datetime().astimezone(UTC).strftime("%Y-%m-%dT%H:%M:%SZ")


def _validate_existing_relation_child(
    base: Base,
    uri: URI,
    cancel: Cancellation | None = None,
) -> None:
    check_cancel(cancel)
    if uri.scheme != Scheme.FILE or not uri.fragment or not base.exists(uri.path):
        return
    try:
        if uri.path.endswith(".json"):
            if base.read_document(uri.path).find_record(uri.fragment) is None:
                raise ValueError(f"record {uri.fragment!r} does not exist")
            check_cancel(cancel)
            return
        if uri.path.endswith(MARKDOWN_EXTENSION):
            page = read_page(base, uri.path, cancel=cancel)
            if not any(heading.anchor == uri.fragment for heading in page.headings):
                raise ValueError(f"heading {uri.fragment!r} does not exist")
            return
        raise ValueError("the file type has no addressable children")
    except (OSError, ValueError) as error:
        raise EdgeValidationError(f"fragment does not name an addressable child: {error}") from error


def _resolved_graph_destination(
    base: Base,
    destination: str,
    src: str,
    via: str,
    cancel: Cancellation | None = None,
) -> str:
    try:
        validate_relation_value(destination)
        parsed = parse_uri(destination)
    except ValueError as error:
        raise EdgeValidationError(f"derive {via} from {src}: destination {destination!r}: {error}") from error
    if str(parsed) != destination:
        raise EdgeValidationError(
            f"derive {via} from {src}: destination {destination!r} is not canonical; want {str(parsed)!r}"
        )
    if parsed.scheme == Scheme.FILE:
        try:
            base.store.resolve(parsed.path)
            _validate_existing_relation_child(base, parsed, cancel)
        except ValueError as error:
            raise EdgeValidationError(f"derive {via} from {src}: destination {destination!r}: {error}") from error
    return parsed.node_uri()


def _document_edges(base: Base, document: Document, cancel: Cancellation | None = None) -> list[Edge]:
    edges: list[Edge] = []
    for record in document.records:
        check_cancel(cancel)
        src = document.record_uri(record)
        if src is None:
            raise EdgeValidationError(f"{document.uri()} has a record with no addressable identity")
        try:
            parsed_src = parse_uri(src)
        except ValueError as error:
            raise EdgeValidationError(f"record URI {src!r}: {error}") from error
        if str(parsed_src) != src:
            raise EdgeValidationError(f"record URI {src!r} is not canonical")
        at = _record_edge_time(document, record)
        for name in document.schema.names():
            check_cancel(cancel)
            definition = document.schema[name]
            if not definition.relation:
                continue
            try:
                values = document.fields.eval_relation(name, record)
            except ValueError as error:
                raise EdgeValidationError(f"derive field:{name} from {src}: {error}") from error
            if not definition.cardinality.allows(len(values)):
                raise EdgeValidationError(
                    f"derive field:{name} from {src}: selected {len(values)} values, "
                    f"cardinality {definition.cardinality} does not allow that count"
                )
            for destination in values:
                via = f"field:{name}"
                edge = Edge(
                    src=src,
                    dst=_resolved_graph_destination(base, destination, src, via, cancel),
                    kind=name,
                    at=at,
                    via=via,
                )
                edge.validate()
                edges.append(edge)
    return edges


def _resolve_addressable_page_link(base: Base, page_uri: str, target: str) -> URI:
    trimmed = target.strip()
    parsed = parse_uri(page_uri + trimmed) if trimmed.startswith("#") else resolve_link(page_uri, trimmed)
    if parsed.scheme == Scheme.FILE:
        base.store.resolve(parsed.path)
    return parsed


def _page_edges(base: Base, page: Page, cancel: Cancellation | None = None) -> list[Edge]:
    edges: list[Edge] = []

    def add(dst: str, kind: str, via: str) -> None:
        edge = Edge(src=page.uri, dst=dst, kind=kind, at=page.date, via=via)
        edge.validate()
        edges.append(edge)

    for tag in page.tags:
        check_cancel(cancel)
        add(str(parse_uri(f"tag:{encode_fragment(tag)}")), "tag", "frontmatter:tags")
    for link in page.links:
        check_cancel(cancel)
        target = link.target.strip()
        if not target or target.startswith("#"):
            continue
        try:
            resolved = _resolve_addressable_page_link(base, page.uri, link.target)
            _validate_existing_relation_child(base, resolved, cancel)
        except ValueError as error:
            raise EdgeValidationError(f"{page.uri}:{link.line}: link {link.target!r}: {error}") from error
        add(resolved.node_uri(), "link", link.via)
    for name in sorted(page.relations):
        check_cancel(cancel)
        definition = base.config.schema.get(name)
        if definition is None:
            raise EdgeValidationError(f"{page.uri}: frontmatter relations.{name} is not declared in fkf.yaml schema")
        if not definition.relation:
            raise EdgeValidationError(f"{page.uri}: frontmatter relations.{name} is not declared as a relation")
        values = page.relations[name]
        if not definition.cardinality.allows(len(values)):
            raise EdgeValidationError(
                f"{page.uri}: frontmatter relations.{name} has {len(values)} values; "
                f"cardinality {definition.cardinality} does not allow that count"
            )
        for candidate in values:
            try:
                resolved = _resolve_addressable_page_link(base, page.uri, candidate)
                validate_relation_value(resolved.node_uri())
                _validate_existing_relation_child(base, resolved, cancel)
            except ValueError as error:
                raise EdgeValidationError(
                    f"{page.uri}: frontmatter relations.{name} URI {candidate!r}: {error}"
                ) from error
            add(resolved.node_uri(), name, f"frontmatter:relations.{name}")
    return sorted(edges, key=lambda edge: (edge.dst, edge.via))


def _task_pages(base: Base, cancel: Cancellation | None = None) -> tuple[Page, ...]:
    pages: list[Page] = []
    for uri in _task_graph_input_uris(base, cancel):
        check_cancel(cancel)
        pages.append(read_page(base, uri, cancel=cancel))
    return tuple(pages)


@dataclass(frozen=True, slots=True)
class ResolvedIdentity:
    canonical: str
    kind: IdentityKind | None = None
    owner: bool = False
    aliases: tuple[str, ...] = ()
    names: tuple[str, ...] = ()
    pages: tuple[str, ...] = ()


@dataclass(frozen=True, slots=True)
class IdentityAlias:
    alias: str
    canonical: str
    via: str


@dataclass(slots=True)
class _IdentityDeclaration:
    canonical: str
    kind: IdentityKind | None
    owner: bool = False
    aliases: tuple[str, ...] = ()
    names: tuple[str, ...] = ()
    pages: tuple[str, ...] = ()
    root: str = ""
    via: dict[str, str] = field(default_factory=dict)


def _normalize_identity_key(value: str) -> str:
    return value.strip().lower()


def _identity_tokens(declaration: _IdentityDeclaration) -> tuple[str, ...]:
    return (declaration.canonical, *declaration.aliases, *declaration.pages)


def _is_graph_identity_alias(value: str) -> bool:
    try:
        validate_entity_uri(value)
        return True
    except ValueError:
        pass
    try:
        parsed = parse_uri(value)
    except ValueError:
        return False
    return (
        parsed.scheme == Scheme.FILE
        and not parsed.fragment
        and not parsed.jq
        and not parsed.directory
        and parsed.path.partition("/")[0] in {str(Layer.WIKI), str(Layer.PROJECTS)}
    )


class IdentityResolver:
    """Immutable, base-local exact identity components and graph aliases."""

    def __init__(self) -> None:
        self._by_canonical: dict[str, ResolvedIdentity] = {}
        self._aliases: dict[str, str] = {}
        self._names: dict[str, list[str]] = {}
        self._graph: list[IdentityAlias] = []

    @classmethod
    def load(cls, base: Base, *, cancel: Cancellation | None = None) -> IdentityResolver:
        declarations: list[_IdentityDeclaration] = []
        for name in sorted(base.config.identities):
            check_cancel(cancel)
            identity = base.config.identities[name]
            declarations.append(
                _IdentityDeclaration(
                    canonical=identity.canonical,
                    kind=identity.effective_kind(),
                    owner=identity.owner,
                    aliases=identity.aliases,
                    root=name,
                    via={_normalize_identity_key(alias): f"identities.{name}.aliases" for alias in identity.aliases},
                )
            )
        for layer in (Layer.PROJECTS, Layer.WIKI):
            if not base.store.enabled(layer):
                continue
            pages, _ = load_markdown_layer(base, layer, cancel=cancel)
            for page in pages:
                check_cancel(cancel)
                kind = IdentityKind(page.type.strip()) if page.type.strip() in {"person", "organization"} else None
                if page.aliases and kind is None:
                    raise EdgeValidationError(f"{page.uri}: frontmatter aliases require type person or organization")
                if kind is None:
                    continue
                for alias in page.aliases:
                    try:
                        validate_identity_alias(alias)
                    except ValueError as error:
                        raise EdgeValidationError(f"{page.uri}: frontmatter alias {alias!r}: {error}") from error
                via = {_normalize_identity_key(page.uri): "frontmatter:aliases"}
                via.update({_normalize_identity_key(alias): "frontmatter:aliases" for alias in page.aliases})
                declarations.append(
                    _IdentityDeclaration(
                        canonical=page.uri,
                        kind=kind,
                        aliases=page.aliases,
                        names=(page.title,) if page.title else (),
                        pages=(page.uri,),
                        via=via,
                    )
                )
        resolver = cls()
        resolver._resolve(declarations, cancel)
        return resolver

    def _resolve(
        self,
        declarations: Sequence[_IdentityDeclaration],
        cancel: Cancellation | None,
    ) -> None:
        parents = list(range(len(declarations)))

        def find(index: int) -> int:
            while parents[index] != index:
                check_cancel(cancel)
                parents[index] = parents[parents[index]]
                index = parents[index]
            return index

        def union(left: int, right: int) -> None:
            left_root, right_root = find(left), find(right)
            if left_root != right_root:
                parents[right_root] = left_root

        claimed: dict[str, int] = {}
        for index, declaration in enumerate(declarations):
            check_cancel(cancel)
            for value in _identity_tokens(declaration):
                check_cancel(cancel)
                key = _normalize_identity_key(value)
                if key in claimed:
                    union(index, claimed[key])
                else:
                    claimed[key] = index
        components: dict[int, list[_IdentityDeclaration]] = {}
        for index, declaration in enumerate(declarations):
            check_cancel(cancel)
            components.setdefault(find(index), []).append(declaration)
        for root in sorted(components):
            check_cancel(cancel)
            self._add_component(components[root], cancel)
        self._graph.sort(key=lambda alias: (alias.alias, alias.canonical))

    def _add_component(
        self,
        component: Sequence[_IdentityDeclaration],
        cancel: Cancellation | None,
    ) -> None:
        root_canonical = ""
        kind: IdentityKind | None = None
        owner = False
        aliases: dict[str, str] = {}
        names: dict[str, str] = {}
        pages: set[str] = set()
        vias: dict[str, str] = {}
        canonical_candidates: list[str] = []
        for declaration in component:
            check_cancel(cancel)
            if declaration.root:
                if root_canonical and root_canonical != declaration.canonical:
                    raise EdgeValidationError(
                        "identity aliases transitively join canonical declarations "
                        f"{root_canonical!r} and {declaration.canonical!r}"
                    )
                root_canonical = declaration.canonical
            canonical_candidates.append(declaration.canonical)
            if kind is None:
                kind = declaration.kind
            elif declaration.kind is not None and declaration.kind is not kind:
                raise EdgeValidationError(f"identity aliases transitively join kinds {kind!r} and {declaration.kind!r}")
            owner = owner or declaration.owner
            for value in _identity_tokens(declaration):
                aliases[_normalize_identity_key(value)] = value
            for value in declaration.names:
                names[_normalize_identity_key(value)] = value
            pages.update(declaration.pages)
            vias.update(declaration.via)
        check_cancel(cancel)
        canonical = root_canonical or min(canonical_candidates)
        aliases.pop(_normalize_identity_key(canonical), None)
        ordered_aliases = tuple(sorted(aliases.values(), key=lambda value: (_normalize_identity_key(value), value)))
        ordered_names = tuple(sorted(names.values(), key=lambda value: (_normalize_identity_key(value), value)))
        identity = ResolvedIdentity(canonical, kind, owner, ordered_aliases, ordered_names, tuple(sorted(pages)))
        self._by_canonical[canonical] = identity
        self._aliases[_normalize_identity_key(canonical)] = canonical
        for alias in ordered_aliases:
            check_cancel(cancel)
            key = _normalize_identity_key(alias)
            self._aliases[key] = canonical
            if _is_graph_identity_alias(alias):
                self._graph.append(IdentityAlias(alias, canonical, vias.get(key, "")))
        for name in ordered_names:
            check_cancel(cancel)
            values = self._names.setdefault(_normalize_identity_key(name), [])
            values.append(canonical)
            values.sort()

    def canonical(self, value: str) -> str:
        key = _normalize_identity_key(value)
        if key in self._aliases:
            return self._aliases[key]
        names = self._names.get(key, [])
        if len(names) == 1:
            return names[0]
        actor = normalize_github_noreply_actor(value)
        if actor is not None:
            return self._aliases.get(_normalize_identity_key(actor), actor)
        return value

    def exact(self, value: str) -> ResolvedIdentity | None:
        return self._by_canonical.get(self.canonical(value))

    def identities(self) -> tuple[ResolvedIdentity, ...]:
        return tuple(self._by_canonical[key] for key in sorted(self._by_canonical))

    def graph_aliases(self) -> tuple[IdentityAlias, ...]:
        return tuple(self._graph)

    def kind(self, value: str) -> IdentityKind | None:
        identity = self.exact(value)
        return identity.kind if identity is not None else None

    def is_owner(self, value: str) -> bool:
        identity = self.exact(value)
        return identity is not None and identity.owner


def extract_edges(base: Base, *, cancel: Cancellation | None = None) -> tuple[list[Edge], ExtractCounts]:
    """Transcribe only declared relations, authored links/tags, and exact aliases."""

    edges: list[Edge] = []
    documents = 0
    check_cancel(cancel)
    if base.store.enabled(Layer.EVENTS):
        for day in base.event_dates():
            check_cancel(cancel)
            for name in base.day_documents(day):
                check_cancel(cancel)
                edges.extend(_document_edges(base, base.read_document(event_document_uri(day, name)), cancel))
                documents += 1
    if base.store.enabled(Layer.INDEX):
        for name in base.index_documents():
            check_cancel(cancel)
            edges.extend(_document_edges(base, base.read_document(index_document_uri(name)), cancel))
            documents += 1
    pages: list[Page] = []
    for layer in (Layer.WIKI, Layer.PROJECTS):
        check_cancel(cancel)
        if base.store.enabled(layer):
            loaded, _ = load_markdown_layer(base, layer, cancel=cancel)
            pages.extend(loaded)
    if base.store.enabled(Layer.TASKS):
        pages.extend(_task_pages(base, cancel))
    for page in pages:
        check_cancel(cancel)
        edges.extend(_page_edges(base, page, cancel))
    check_cancel(cancel)
    resolver = IdentityResolver.load(base, cancel=cancel)
    check_cancel(cancel)
    canonical: list[Edge] = []
    for edge in edges:
        check_cancel(cancel)
        canonical.append(replace(edge, src=resolver.canonical(edge.src), dst=resolver.canonical(edge.dst)))
    for alias in resolver.graph_aliases():
        check_cancel(cancel)
        if alias.alias != alias.canonical:
            canonical.append(Edge(alias.alias, alias.canonical, "same-as", via=alias.via))
    return canonical, ExtractCounts(documents=documents, pages=len(pages))


@dataclass(frozen=True, slots=True)
class GraphBuild:
    uri: str
    edges: int
    documents: int
    pages: int
    mode: str
    elapsed: str
    meta: EdgeListMeta
    stale: bool = field(default=False, metadata={"json": "stale,omitempty"})


def _format_elapsed(started: datetime, ended: datetime) -> str:
    milliseconds = round((ended - started).total_seconds() * 1000)
    if milliseconds == 0:
        return "0s"
    return f"{milliseconds}ms" if milliseconds < 1000 else f"{milliseconds / 1000:g}s"


def build_graph(base: Base, *, cancel: Cancellation | None = None) -> GraphBuild:
    """Rebuild the graph from one stable input generation and publish it atomically."""

    started = base.now()
    check_cancel(cancel)
    inputs = graph_input_state(base, cancel=cancel)
    edges, counts = extract_edges(base, cancel=cancel)
    check_cancel(cancel)
    confirmed = graph_input_state(base, cancel=cancel)
    if confirmed != inputs:
        raise EdgeValidationError("graph inputs changed while the derived caches were being built; retry")
    _, indexed = _canonical_generated_at(started)
    indexed_edges = [replace(edge, indexed=indexed) for edge in edges]
    rows_path = base.store.resolve(GRAPH_FILE)
    meta_path = base.store.resolve(GRAPH_META_FILE)
    metadata = new_edge_list_meta(indexed_edges, started, inputs.sha256, *inputs.files)
    # Publish a complete generation after the last cooperative checkpoint.
    check_cancel(cancel)
    write_edge_list(rows_path, meta_path, indexed_edges, metadata)
    return GraphBuild(
        uri=GRAPH_FILE,
        edges=len(dedupe_edges(indexed_edges)),
        documents=counts.documents,
        pages=counts.pages,
        mode="full",
        elapsed=_format_elapsed(started, base.now()),
        meta=metadata,
    )


# --- integrity-bound reads and seek traversal ----------------------------------------------


class DerivedGraphMissingError(EdgeValidationError):
    """The benign fresh-clone case: no derived graph has been built yet."""


def _strict_object(value: object, fields: set[str], label: str) -> dict[str, object]:
    if not isinstance(value, dict) or any(not isinstance(key, str) for key in value):
        raise EdgeValidationError(f"decode {label}: expected an object")
    unknown = set(value) - fields
    if unknown:
        raise EdgeValidationError(f"decode {label}: unknown field {min(unknown)!r}")
    missing = fields - set(value)
    if missing:
        raise EdgeValidationError(f"decode {label}: missing field {min(missing)!r}")
    return value


def _strict_string(value: object, label: str) -> str:
    if not isinstance(value, str):
        raise EdgeValidationError(f"decode {GRAPH_META_FILE}: {label} must be a string")
    return value


def _strict_int(value: object, label: str) -> int:
    if isinstance(value, bool) or not isinstance(value, int):
        raise EdgeValidationError(f"decode {GRAPH_META_FILE}: {label} must be an integer")
    return value


def _strict_strings(value: object, label: str) -> tuple[str, ...]:
    if not isinstance(value, list) or any(not isinstance(item, str) for item in value):
        raise EdgeValidationError(f"decode {GRAPH_META_FILE}: {label} must be a string array")
    return tuple(value)


def _decode_file_manifest(value: object, label: str) -> GraphFileManifest:
    fields = {"uri", "bytes", "modified_unix_nano", "sha256"}
    item = _strict_object(value, fields, f"{GRAPH_META_FILE} {label}")
    return GraphFileManifest(
        uri=_strict_string(item["uri"], f"{label}.uri"),
        bytes=_strict_int(item["bytes"], f"{label}.bytes"),
        modified_unix_nano=_strict_int(item["modified_unix_nano"], f"{label}.modified_unix_nano"),
        sha256=_strict_string(item["sha256"], f"{label}.sha256"),
    )


def _decode_manifests(value: object, label: str) -> tuple[GraphFileManifest, ...]:
    if not isinstance(value, list):
        raise EdgeValidationError(f"decode {GRAPH_META_FILE}: {label} must be an array")
    return tuple(_decode_file_manifest(item, f"{label}[{index}]") for index, item in enumerate(value))


def _decode_graph_input_sha256(value: object) -> GraphInputSHA256:
    fields = {"AGGREGATE", "events", "index", "projects", "tasks", "wiki", "schema"}
    item = _strict_object(value, fields, f"{GRAPH_META_FILE} sha256.inputs")
    return GraphInputSHA256(
        aggregate=_strict_string(item["AGGREGATE"], "sha256.inputs.AGGREGATE"),
        events=_strict_string(item["events"], "sha256.inputs.events"),
        index=_strict_string(item["index"], "sha256.inputs.index"),
        projects=_strict_string(item["projects"], "sha256.inputs.projects"),
        tasks=_strict_string(item["tasks"], "sha256.inputs.tasks"),
        wiki=_strict_string(item["wiki"], "sha256.inputs.wiki"),
        schema=_strict_string(item["schema"], "sha256.inputs.schema"),
    )


def _decode_graph_output_sha256(value: object) -> GraphOutputSHA256:
    fields = {GRAPH_FILE, GRAPH_DST_FILE, GRAPH_OFFSETS_FILE}
    item = _strict_object(value, fields, f"{GRAPH_META_FILE} sha256.outputs")
    return GraphOutputSHA256(
        graph_tsv=_strict_string(item[GRAPH_FILE], f"sha256.outputs.{GRAPH_FILE}"),
        graph_dst_tsv=_strict_string(item[GRAPH_DST_FILE], f"sha256.outputs.{GRAPH_DST_FILE}"),
        graph_offsets_tsv=_strict_string(item[GRAPH_OFFSETS_FILE], f"sha256.outputs.{GRAPH_OFFSETS_FILE}"),
    )


def decode_edge_list_meta(data: bytes) -> EdgeListMeta:
    """Decode the sidecar with Go's unknown-field and single-document closure."""

    value = _decode_one_json(data, GRAPH_META_FILE)
    fields = {
        "schema_version",
        "extractor_version",
        "columns",
        "separator",
        "generated_at",
        "edges",
        "extractors",
        "bytes",
        "kinds",
        "inputs",
        "outputs",
        "sha256",
    }
    item = _strict_object(value, fields, GRAPH_META_FILE)
    sha = _strict_object(item["sha256"], {"inputs", "outputs"}, f"{GRAPH_META_FILE} sha256")
    return EdgeListMeta(
        schema_version=_strict_int(item["schema_version"], "schema_version"),
        extractor_version=_strict_int(item["extractor_version"], "extractor_version"),
        columns=_strict_strings(item["columns"], "columns"),
        separator=_strict_string(item["separator"], "separator"),
        generated_at=_strict_string(item["generated_at"], "generated_at"),
        edges=_strict_int(item["edges"], "edges"),
        extractors=_strict_strings(item["extractors"], "extractors"),
        bytes=_strict_int(item["bytes"], "bytes"),
        kinds=_strict_strings(item["kinds"], "kinds"),
        inputs=_decode_manifests(item["inputs"], "inputs"),
        outputs=_decode_manifests(item["outputs"], "outputs"),
        sha256=GraphSHA256Manifest(
            inputs=_decode_graph_input_sha256(sha["inputs"]),
            outputs=_decode_graph_output_sha256(sha["outputs"]),
        ),
    )


def read_graph_meta(base: Base) -> EdgeListMeta:
    return decode_edge_list_meta(read_file_limited(base.store.resolve(GRAPH_META_FILE), MAX_SOURCE_DOCUMENT_BYTES))


def _strictly_sorted_unique(values: Sequence[str]) -> bool:
    return all(value and (index == 0 or value > values[index - 1]) for index, value in enumerate(values))


def _graph_input_manifest_problems(meta: EdgeListMeta) -> list[str]:
    problems = _graph_file_manifest_problems(meta.inputs, "input")
    for item in meta.inputs:
        if _graph_input_layer(item.uri) is None:
            problems.append(f"metadata input URI {item.uri!r} is not a graph input")
    try:
        derived = _graph_input_sha256_from_files(meta.inputs, meta.sha256.inputs.schema)
    except EdgeValidationError as error:
        problems.append(f"metadata input manifest cannot be digested: {error}")
    else:
        if derived != meta.sha256.inputs:
            problems.append("metadata input manifest does not match sha256.inputs")
    return problems


def _graph_output_manifest_problems(meta: EdgeListMeta, generated: datetime | None) -> list[str]:
    problems = _graph_file_manifest_problems(meta.outputs, "output")
    wanted = (GRAPH_DST_FILE, GRAPH_OFFSETS_FILE, GRAPH_FILE)
    got = tuple(item.uri for item in meta.outputs)
    if got != wanted:
        problems.append(f"metadata output URIs are {got}, want {wanted}")
    for item in meta.outputs:
        if generated is not None and item.modified_unix_nano != int(generated.timestamp()) * 1_000_000_000:
            problems.append(f"metadata output {item.uri} mtime does not match generated_at")
    primary = _manifest_by_uri(meta.outputs, GRAPH_FILE)
    if primary is not None and meta.bytes != primary.bytes:
        problems.append(f"metadata bytes does not match the {GRAPH_FILE} output manifest")
    for uri, digest in (
        (GRAPH_DST_FILE, meta.sha256.outputs.graph_dst_tsv),
        (GRAPH_OFFSETS_FILE, meta.sha256.outputs.graph_offsets_tsv),
        (GRAPH_FILE, meta.sha256.outputs.graph_tsv),
    ):
        manifest = _manifest_by_uri(meta.outputs, uri)
        if manifest is None or manifest.sha256 != digest:
            problems.append(f"metadata sha256.outputs[{uri!r}] does not match its output manifest")
    return problems


def graph_meta_static_problems(meta: EdgeListMeta) -> list[str]:
    """Return every closed-vocabulary metadata defect without touching its files."""

    problems: list[str] = []
    if meta.schema_version != EDGE_SCHEMA_VERSION:
        problems.append(f"metadata schema_version is {meta.schema_version}, want {EDGE_SCHEMA_VERSION}")
    if meta.extractor_version != GRAPH_EXTRACTOR_VERSION:
        problems.append(f"metadata extractor_version is {meta.extractor_version}, want {GRAPH_EXTRACTOR_VERSION}")
    if meta.columns != EDGE_COLUMNS:
        problems.append(f"metadata columns are {meta.columns}, want {EDGE_COLUMNS}")
    if meta.separator != "\\t":
        problems.append(f"metadata separator is {meta.separator!r}, want '\\\\t'")
    if meta.edges < 0 or meta.bytes < 0:
        problems.append("metadata edge and byte counts must be non-negative")
    generated: datetime | None = None
    if _valid_canonical_edge_time(meta.generated_at):
        generated = datetime.fromisoformat(meta.generated_at.removesuffix("Z") + "+00:00")
    else:
        problems.append(f"metadata generated_at {meta.generated_at!r} is not canonical UTC RFC3339")
    problems.extend(_graph_input_digest_problems(meta.sha256.inputs))
    problems.extend(_graph_input_manifest_problems(meta))
    problems.extend(_graph_output_manifest_problems(meta, generated))
    if not _strictly_sorted_unique(meta.extractors):
        problems.append("metadata extractors are not strictly sorted and unique")
    if not _strictly_sorted_unique(meta.kinds):
        problems.append("metadata kinds are not strictly sorted and unique")
    return problems


def _current_graph_input_problems(
    base: Base,
    meta: EdgeListMeta,
    *,
    full: bool,
    cancel: Cancellation | None = None,
) -> list[str]:
    try:
        current = graph_input_uris(base, cancel=cancel)
    except (OSError, ValueError) as error:
        return [f"cannot list current graph inputs: {error}"]
    expected = {item.uri: item for item in meta.inputs}
    changed: set[str] = set()
    for uri in set(current) | set(expected):
        check_cancel(cancel)
        layer = _graph_input_layer(uri)
        if uri not in expected or uri not in current:
            if layer is not None:
                changed.add(str(layer))
            continue
        manifest = expected[uri]
        try:
            absolute = base.store.resolve(uri)
            info = absolute.lstat()
            if stat.S_ISLNK(info.st_mode) or not stat.S_ISREG(info.st_mode):
                raise EdgeValidationError(f"graph input {uri} is not a regular file")
            if (
                (full or (info.st_size, info.st_mtime_ns) != (manifest.bytes, manifest.modified_unix_nano))
                and hash_graph_input(base, uri, cancel=cancel).sha256 != manifest.sha256
                and layer is not None
            ):
                changed.add(str(layer))
        except FileNotFoundError:
            if layer is not None:
                changed.add(str(layer))
        except (OSError, ValueError) as error:
            return [f"cannot validate current graph inputs: {error}"]
    if graph_schema_input_sha256(base, cancel=cancel) != meta.sha256.inputs.schema:
        changed.add("schema")
    return [f"{name} input changed" for name in _GRAPH_INPUT_NAMES if name in changed]


@dataclass(frozen=True, slots=True)
class _GraphFileSnapshot:
    uri: str
    handle: BinaryIO
    bytes: int
    modified_unix_nano: int


def _open_graph_artifact(base: Base, uri: str, *, primary: bool = False) -> BinaryIO:
    path = base.store.resolve(uri)
    try:
        return open_regular_file(path)
    except (OSError, UnsafePathError) as error:
        caused_by_missing = any(isinstance(item, FileNotFoundError) for item in _exception_chain(error))
        if primary and caused_by_missing:
            raise DerivedGraphMissingError(
                f"derived file not built: {GRAPH_FILE} does not exist; run `fkf build graph`"
            ) from error
        if caused_by_missing:
            raise EdgeValidationError(
                f"invalid derived graph cache: {uri} does not exist; run `fkf build graph`"
            ) from error
        raise EdgeValidationError(f"open {uri}: {error}") from error


def _exception_chain(error: BaseException) -> Iterable[BaseException]:
    current: BaseException | None = error
    while current is not None:
        yield current
        current = current.__cause__ or current.__context__


def _artifact_max_bytes(uri: str) -> int:
    if uri == GRAPH_OFFSETS_FILE:
        return (MAX_EDGE_LINE_BYTES + 128) * MAX_EDGE_LIST_ROWS * 2
    return MAX_EDGE_LINE_BYTES * MAX_EDGE_LIST_ROWS


def _hash_open_artifact(
    handle: BinaryIO,
    uri: str,
    before: os.stat_result,
    cancel: Cancellation | None = None,
) -> tuple[str, os.stat_result]:
    if before.st_size > _artifact_max_bytes(uri):
        raise FileTooLargeError(f"{uri} exceeds {_artifact_max_bytes(uri)} bytes")
    digest = hashlib.sha256()
    position = 0
    while position < before.st_size:
        check_cancel(cancel)
        chunk = os.pread(handle.fileno(), min(1 << 20, before.st_size - position), position)
        if not chunk:
            break
        digest.update(chunk)
        position += len(chunk)
    after = os.fstat(handle.fileno())
    if position != before.st_size or before.st_size != after.st_size or before.st_mtime_ns != after.st_mtime_ns:
        raise EdgeValidationError(f"{uri} changed while it was being hashed; retry")
    return digest.hexdigest(), after


@dataclass(slots=True)
class ValidatedGraphCache:
    """Open descriptors proven to belong to one complete graph publication."""

    file: BinaryIO
    dst: BinaryIO
    offsets: BinaryIO
    meta: EdgeListMeta
    files: tuple[_GraphFileSnapshot, ...] = ()

    def close(self) -> None:
        for handle in (self.file, self.dst, self.offsets):
            handle.close()

    def __enter__(self) -> ValidatedGraphCache:
        return self

    def __exit__(self, *_args: object) -> None:
        self.close()

    def revalidate_bytes(self) -> None:
        for snapshot in self.files:
            info = os.fstat(snapshot.handle.fileno())
            if (info.st_size, info.st_mtime_ns) != (snapshot.bytes, snapshot.modified_unix_nano):
                raise EdgeValidationError(
                    f"invalid derived graph cache: {snapshot.uri} changed during the read; run `fkf build graph`"
                )

    def scan(
        self,
        query: EdgeQuery,
        visit: Callable[[Edge], None] | None = None,
        *,
        cancel: Cancellation | None = None,
    ) -> tuple[list[Edge], EdgeScanStats]:
        if query.src:
            return self._scan_range(self.file, "src", query.src, query, visit, cancel)
        if query.dst:
            return self._scan_range(self.dst, "dst", query.dst, query, visit, cancel)
        self.file.seek(0)
        return scan_edges(self.file, query, visit, cancel=cancel)

    def _scan_range(
        self,
        handle: BinaryIO,
        direction: str,
        node: str,
        query: EdgeQuery,
        visit: Callable[[Edge], None] | None,
        cancel: Cancellation | None,
    ) -> tuple[list[Edge], EdgeScanStats]:
        offset = self.find_offset(direction, node, cancel)
        if offset is None:
            return [], EdgeScanStats()
        size = os.fstat(handle.fileno()).st_size
        if offset.start > size or offset.bytes > size - offset.start:
            raise EdgeValidationError(
                f"invalid derived graph cache: {direction} range for {node} exceeds its edge list; "
                "run `fkf build graph`"
            )
        reader = _PreadSection(handle.fileno(), offset.start, offset.bytes)
        return scan_edges(reader, query, visit, cancel=cancel)

    def find_offset(
        self,
        direction: str,
        node: str,
        cancel: Cancellation | None = None,
    ) -> GraphOffset | None:
        size = os.fstat(self.offsets.fileno()).st_size
        target = f"{direction}\t{node}"
        low, high = 0, size
        while low < high:
            check_cancel(cancel)
            middle = low + (high - low) // 2
            found = _read_graph_offset_at_or_after(self.offsets, size, middle)
            if found is None or found[1] >= high:
                high = middle
                continue
            offset, start, end = found
            if offset.key() < target:
                low = end
            else:
                high = start
        found = _read_graph_offset_at_or_after(self.offsets, size, low)
        if found is None or found[0].key() != target:
            return None
        return found[0]


class _PreadSection:
    def __init__(self, descriptor: int, start: int, length: int) -> None:
        self._descriptor = descriptor
        self._position = start
        self._end = start + length

    def readline(self, size: int = -1) -> bytes:
        remaining = self._end - self._position
        if remaining <= 0:
            return b""
        amount = remaining if size < 0 else min(remaining, size)
        data = os.pread(self._descriptor, amount, self._position)
        newline = data.find(b"\n")
        if newline >= 0:
            data = data[: newline + 1]
        self._position += len(data)
        return data


def _read_offset_line(handle: BinaryIO, size: int, start: int) -> bytes:
    maximum = MAX_EDGE_LINE_BYTES + 128
    data = os.pread(handle.fileno(), min(maximum + 1, size - start), start)
    newline = data.find(b"\n")
    if newline < 0:
        if len(data) > maximum:
            raise EdgeValidationError(f"{GRAPH_OFFSETS_FILE} line exceeds {maximum} bytes")
        raise EdgeValidationError(f"{GRAPH_OFFSETS_FILE} does not end with a newline")
    line = data[: newline + 1]
    if b"\r" in line:
        raise EdgeValidationError(f"{GRAPH_OFFSETS_FILE} contains a carriage return")
    return line


def _read_graph_offset_at_or_after(handle: BinaryIO, size: int, position: int) -> tuple[GraphOffset, int, int] | None:
    if position >= size:
        return None
    start = position
    if position > 0 and os.pread(handle.fileno(), 1, position - 1) != b"\n":
        partial = _read_offset_line(handle, size, position)
        start += len(partial)
    if start >= size:
        return None
    line = _read_offset_line(handle, size, start)
    try:
        offset = decode_graph_offset(line[:-1])
    except EdgeValidationError as error:
        raise EdgeValidationError(f"decode {GRAPH_OFFSETS_FILE} at byte {start}: {error}") from error
    return offset, start, start + len(line)


def _validate_graph_outputs(
    cache: ValidatedGraphCache,
    *,
    full: bool,
    cancel: Cancellation | None = None,
) -> list[str]:
    handles = {GRAPH_FILE: cache.file, GRAPH_DST_FILE: cache.dst, GRAPH_OFFSETS_FILE: cache.offsets}
    problems: list[str] = []
    snapshots: list[_GraphFileSnapshot] = []
    for expected in cache.meta.outputs:
        check_cancel(cancel)
        handle = handles.get(expected.uri)
        if handle is None:
            continue
        info = os.fstat(handle.fileno())
        changed = (info.st_size, info.st_mtime_ns) != (expected.bytes, expected.modified_unix_nano)
        if full or changed:
            digest, info = _hash_open_artifact(handle, expected.uri, info, cancel)
            if digest != expected.sha256:
                problems.append(f"{expected.uri} bytes do not match metadata sha256.outputs")
        snapshots.append(_GraphFileSnapshot(expected.uri, handle, info.st_size, info.st_mtime_ns))
    cache.files = tuple(snapshots)
    return problems


def _validate_cached_edge(base: Base, edge: Edge) -> None:
    for field_name, value in (("src", edge.src), ("dst", edge.dst)):
        try:
            parsed = parse_uri(value)
        except ValueError as error:
            raise EdgeValidationError(f"graph row {field_name} {value!r} is not a URI: {error}") from error
        if str(parsed) != value:
            raise EdgeValidationError(f"graph row {field_name} {value!r} is not canonical; want {str(parsed)!r}")
        if parsed.scheme != Scheme.FILE:
            continue
        if parsed.directory or not parsed.path or parsed.jq:
            raise EdgeValidationError(f"graph row {field_name} {value!r} is not an addressable file or record node")
        try:
            base.store.resolve(parsed.path)
        except ValueError as error:
            raise EdgeValidationError(
                f"graph row {field_name} {value!r} is outside the published base grammar: {error}"
            ) from error


def scan_graph_rows(
    base: Base,
    cache: ValidatedGraphCache,
    visit: Callable[[Edge], None] | None = None,
    *,
    cancel: Cancellation | None = None,
) -> EdgeScanStats:
    """Fully validate canonical primary rows and sidecar row vocabulary."""

    cache.file.seek(0)
    edges = 0
    indexed: set[str] = set()
    vias: set[str] = set()
    kinds: set[str] = set()
    previous: tuple[str, str, str, str, str] | None = None
    semantic_problem = ""

    def inspect(edge: Edge) -> None:
        nonlocal edges, previous, semantic_problem
        check_cancel(cancel)
        edges += 1
        indexed.add(edge.indexed)
        vias.add(edge.via)
        kinds.add(edge.kind)
        if not semantic_problem:
            try:
                _validate_cached_edge(base, edge)
            except EdgeValidationError as error:
                semantic_problem = str(error)
        key = edge.sort_key()
        if not semantic_problem and previous is not None:
            if key == previous:
                semantic_problem = "graph rows contain a duplicate canonical edge"
            elif key < previous:
                semantic_problem = "graph rows are not in canonical sort order"
        previous = key
        if visit is not None:
            visit(edge)

    _, stats = scan_edges(cache.file, EdgeQuery(), inspect, cancel=cancel)
    byte_count = cache.file.tell()
    problems: list[str] = []
    if cache.meta.edges != edges:
        problems.append(f"metadata edges is {cache.meta.edges}, but {GRAPH_FILE} holds {edges} valid row(s)")
    if cache.meta.bytes != byte_count:
        problems.append(f"metadata bytes is {cache.meta.bytes}, but {GRAPH_FILE} holds {byte_count} byte(s)")
    if cache.meta.extractors != tuple(sorted(vias)):
        problems.append(f"metadata extractors are {cache.meta.extractors}, but rows use {tuple(sorted(vias))}")
    if cache.meta.kinds != tuple(sorted(kinds)):
        problems.append(f"metadata kinds are {cache.meta.kinds}, but rows use {tuple(sorted(kinds))}")
    if any(value != cache.meta.generated_at for value in indexed):
        problems.append(f"metadata generated_at {cache.meta.generated_at!r} does not match every indexed column")
    if stats.malformed:
        problems.append(f"{GRAPH_FILE} has {stats.malformed} malformed row(s)")
    if semantic_problem:
        problems.append(semantic_problem)
    if problems:
        raise EdgeValidationError(f"invalid derived graph cache: {'; '.join(problems)}; run `fkf build graph`")
    return stats


def open_validated_graph_cache(
    base: Base,
    *,
    full_inputs: bool = False,
    full_outputs: bool = False,
    cancel: Cancellation | None = None,
) -> ValidatedGraphCache:
    """Open, bracket, and validate one immutable derived graph generation."""

    check_cancel(cancel)
    probe = _open_graph_artifact(base, GRAPH_FILE, primary=True)
    probe.close()
    generation = read_current_graph_generation(base.root)
    primary = _open_graph_artifact(base, GRAPH_FILE, primary=True)
    dst: BinaryIO | None = None
    offsets: BinaryIO | None = None
    try:
        try:
            meta = read_graph_meta(base)
        except (OSError, ValueError) as error:
            raise EdgeValidationError(f"invalid derived graph cache: {error}; run `fkf build graph`") from error
        problems = graph_meta_static_problems(meta)
        if generation != graph_generation_sha256(meta):
            problems.append("graph generation marker does not match metadata")
        problems.extend(_current_graph_input_problems(base, meta, full=full_inputs, cancel=cancel))
        dst = _open_graph_artifact(base, GRAPH_DST_FILE)
        offsets = _open_graph_artifact(base, GRAPH_OFFSETS_FILE)
        cache = ValidatedGraphCache(primary, dst, offsets, meta)
        output_problems = _validate_graph_outputs(cache, full=full_outputs, cancel=cancel)
        if any(problem.startswith(f"{GRAPH_FILE} bytes ") for problem in output_problems):
            scan_graph_rows(base, cache, cancel=cancel)
        problems.extend(output_problems)
        if read_current_graph_generation(base.root) != generation:
            problems.append("graph generation changed while its artifacts were opened; retry")
        if problems:
            raise EdgeValidationError(f"invalid derived graph cache: {'; '.join(problems)}; run `fkf build graph`")
        return cache
    except BaseException:
        primary.close()
        if dst is not None:
            dst.close()
        if offsets is not None:
            offsets.close()
        raise


class Direction(StrEnum):
    OUT = "out"
    IN = "in"
    BOTH = "both"


def parse_direction(value: str) -> Direction:
    candidate = value.strip().lower()
    if not candidate:
        return Direction.BOTH
    try:
        return Direction(candidate)
    except ValueError as error:
        raise ValueError(f"direction {value!r} must be in, out, or both") from error


MAX_GRAPH_DEPTH: Final = 3


@dataclass(frozen=True, slots=True)
class GraphQuery:
    uri: str
    direction: Direction = Direction.BOTH
    kind: str = ""
    depth: int = 1
    offset: int = 0
    limit: int = 0


@dataclass(frozen=True, slots=True)
class NeighbourEdge:
    src: str
    dst: str
    kind: str
    at: str = field(default="", metadata={"json": "at,omitempty"})
    via: str = ""
    indexed: str = field(default="", metadata={"json": "indexed,omitempty"})
    hop: int = 0

    @classmethod
    def from_edge(cls, edge: Edge, hop: int) -> NeighbourEdge:
        return cls(*edge.fields(), hop=hop)

    @property
    def edge(self) -> Edge:
        """Return the durable edge shape for callers that embed hop metadata separately."""

        return Edge(*self.src_dst_fields)

    @property
    def src_dst_fields(self) -> tuple[str, str, str, str, str, str]:
        return (self.src, self.dst, self.kind, self.at, self.via, self.indexed)


@dataclass(frozen=True, slots=True)
class Neighbourhood:
    uri: str
    direction: Direction
    depth: int
    edges: tuple[NeighbourEdge, ...]
    nodes: tuple[str, ...]
    truncated: bool = field(metadata={"json": "truncated,omitempty"})
    stats: EdgeScanStats
    snapshot_sha256: str = field(default="", repr=False, compare=False, metadata={"json": "-"})
    skipped: int = field(default=0, repr=False, compare=False, metadata={"json": "-"})


def _prepare_graph_query(query: GraphQuery) -> tuple[GraphQuery, URI]:
    depth = max(1, query.depth)
    if query.offset < 0:
        raise ValueError(f"graph offset {query.offset} must be non-negative")
    if depth > MAX_GRAPH_DEPTH:
        raise ValueError(f"depth {depth} exceeds the maximum of {MAX_GRAPH_DEPTH}; walk in steps and read what matters")
    prepared = replace(query, depth=depth, kind=query.kind.strip())
    return prepared, parse_uri(prepared.uri)


def _scan_neighbours(
    cache: ValidatedGraphCache,
    node: str,
    query: GraphQuery,
    cancel: Cancellation | None,
) -> tuple[list[Edge], EdgeScanStats]:
    selectors: tuple[EdgeQuery, ...]
    if query.direction is Direction.OUT:
        selectors = (EdgeQuery(src=node, kind=query.kind),)
    elif query.direction is Direction.IN:
        selectors = (EdgeQuery(dst=node, kind=query.kind),)
    else:
        selectors = (EdgeQuery(src=node, kind=query.kind), EdgeQuery(dst=node, kind=query.kind))
    found: list[Edge] = []
    total = EdgeScanStats()
    for selector in selectors:
        check_cancel(cancel)
        rows, stats = cache.scan(selector, cancel=cancel)
        found.extend(rows)
        total.add(stats)
    return sort_edges(found), total


def _walk_neighbourhood(
    cache: ValidatedGraphCache,
    query: GraphQuery,
    start: URI,
    cancel: Cancellation | None,
) -> Neighbourhood:
    if query.kind and query.kind not in cache.meta.kinds:
        vocabulary = ", ".join(cache.meta.kinds) if cache.meta.kinds else "none"
        raise ValueError(f"unknown edge kind {query.kind!r}; this base declares {vocabulary}")
    start_node = start.node_uri()
    frontier = [start_node]
    visited = {start_node}
    seen: set[tuple[str, str, str, str, str]] = set()
    reported: list[NeighbourEdge] = []
    nodes: list[str] = []
    total = EdgeScanStats()
    skipped = 0
    truncated = False
    for hop in range(1, query.depth + 1):
        check_cancel(cancel)
        if not frontier:
            break
        next_frontier: list[str] = []
        stop = False
        for node in frontier:
            check_cancel(cancel)
            edges, stats = _scan_neighbours(cache, node, query, cancel)
            total.add(stats)
            for edge in edges:
                check_cancel(cancel)
                key = edge.sort_key()
                if key in seen:
                    continue
                seen.add(key)
                report = skipped >= query.offset
                if not report:
                    skipped += 1
                elif query.limit > 0 and len(reported) >= query.limit:
                    truncated = True
                    stop = True
                    break
                elif report:
                    reported.append(NeighbourEdge.from_edge(edge, hop))
                for side in (edge.src, edge.dst):
                    if side in visited:
                        continue
                    visited.add(side)
                    next_frontier.append(side)
                    if report:
                        nodes.append(side)
            if stop:
                break
        frontier = next_frontier
        if stop:
            break
    return Neighbourhood(
        uri=str(start),
        direction=query.direction,
        depth=query.depth,
        edges=tuple(reported),
        nodes=tuple(sorted(nodes)),
        truncated=truncated,
        stats=total,
        snapshot_sha256=cache.meta.sha256.outputs.graph_tsv,
        skipped=skipped,
    )


def neighbours(
    base: Base,
    query: GraphQuery,
    *,
    cancel: Cancellation | None = None,
) -> Neighbourhood:
    resolver = IdentityResolver.load(base, cancel=cancel)
    prepared, start = _prepare_graph_query(replace(query, uri=resolver.canonical(query.uri)))
    with open_validated_graph_cache(base, cancel=cancel) as cache:
        result = _walk_neighbourhood(cache, prepared, start, cancel)
        check_cancel(cancel)
        cache.revalidate_bytes()
        return result


def neighbours_from_cache(
    cache: ValidatedGraphCache,
    query: GraphQuery,
    *,
    cancel: Cancellation | None = None,
) -> Neighbourhood:
    """Walk a prepared URI while reusing a caller-owned validated generation."""

    prepared, start = _prepare_graph_query(query)
    return _walk_neighbourhood(cache, prepared, start, cancel)


def read_validated_graph_meta(base: Base, *, cancel: Cancellation | None = None) -> EdgeListMeta:
    """Return metadata only after its marker, inputs, and open artifacts validate."""

    with open_validated_graph_cache(base, cancel=cancel) as cache:
        meta = cache.meta
        check_cancel(cancel)
        cache.revalidate_bytes()
        return meta


def read_validated_graph_artifact(
    base: Base,
    uri: str,
    limit: int,
    *,
    cancel: Cancellation | None = None,
) -> bytes:
    """Read one bounded artifact from the exact descriptors its sidecar validates."""

    if uri not in {GRAPH_FILE, GRAPH_DST_FILE, GRAPH_OFFSETS_FILE}:
        raise ValueError(f"{uri!r} is not a graph row artifact")
    if limit <= 0:
        raise ValueError("graph artifact read limit must be positive")
    with open_validated_graph_cache(base, cancel=cancel) as cache:
        handles = {GRAPH_FILE: cache.file, GRAPH_DST_FILE: cache.dst, GRAPH_OFFSETS_FILE: cache.offsets}
        manifest = _manifest_by_uri(cache.meta.outputs, uri)
        if manifest is None:
            raise EdgeValidationError(f"invalid derived graph cache: metadata omits {uri}; run `fkf build graph`")
        if manifest.bytes > limit:
            raise FileTooLargeError(f"read {uri}: file exceeds {limit}-byte limit")
        handle = handles[uri]
        check_cancel(cancel)
        data = os.pread(handle.fileno(), limit + 1, 0)
        check_cancel(cancel)
        if len(data) != manifest.bytes:
            raise EdgeValidationError(
                f"invalid derived graph cache: {uri} changed during the read; run `fkf build graph`"
            )
        cache.revalidate_bytes()
        return data


@dataclass(frozen=True, slots=True)
class NodeCount:
    uri: str
    kind: str
    out: int
    in_: int = field(metadata={"json": "in"})
    total: int = 0


@dataclass(frozen=True, slots=True)
class NodeListing:
    kind: str = field(metadata={"json": "kind,omitempty"})
    nodes: tuple[NodeCount, ...]
    total: int
    stats: EdgeScanStats


def node_kind(uri: str) -> str:
    try:
        parsed = parse_uri(uri)
    except ValueError:
        parsed = None
    if parsed is not None:
        if parsed.scheme == Scheme.EXTERNAL:
            return "url"
        if parsed.is_entity():
            return str(parsed.scheme)
    if uri in {GRAPH_FILE, GRAPH_DST_FILE, GRAPH_OFFSETS_FILE, GRAPH_META_FILE, GRAPH_GENERATION_FILE}:
        return "derived"
    return {
        "events": "event",
        "index": "index",
        "tasks": "task",
        "projects": "project",
        "wiki": "wiki",
    }.get(uri.partition("/")[0], "file")


def _scan_validated_graph_cache(
    base: Base,
    visit: Callable[[Edge], None],
    cancel: Cancellation | None = None,
) -> EdgeScanStats:
    with open_validated_graph_cache(base, cancel=cancel) as cache:
        stats = scan_graph_rows(base, cache, visit, cancel=cancel)
        check_cancel(cancel)
        cache.revalidate_bytes()
        return stats


def list_nodes(
    base: Base,
    kind: str = "",
    limit: int = 0,
    *,
    cancel: Cancellation | None = None,
) -> NodeListing:
    check_cancel(cancel)
    kind = kind.strip()
    resolver = IdentityResolver.load(base, cancel=cancel)
    counts: dict[str, list[int | str]] = {}

    def record(uri: str, outgoing: bool) -> None:
        classified = node_kind(uri)
        identity_kind = resolver.kind(uri)
        if identity_kind is not None:
            classified = str(identity_kind)
            if kind == classified:
                uri = resolver.canonical(uri)
        if kind and classified != kind:
            return
        values = counts.setdefault(uri, [classified, 0, 0])
        values[1 if outgoing else 2] = int(values[1 if outgoing else 2]) + 1

    def inspect(edge: Edge) -> None:
        record(edge.src, True)
        record(edge.dst, False)

    stats = _scan_validated_graph_cache(base, inspect, cancel)
    nodes = []
    for uri, values in counts.items():
        check_cancel(cancel)
        nodes.append(NodeCount(uri, str(values[0]), int(values[1]), int(values[2]), int(values[1]) + int(values[2])))
    nodes.sort(key=lambda item: (-item.total, item.uri))
    total = len(nodes)
    if limit > 0:
        nodes = nodes[:limit]
    return NodeListing(kind, tuple(nodes), total, stats)


@dataclass(frozen=True, slots=True)
class KindCount:
    kind: str
    count: int


@dataclass(frozen=True, slots=True)
class GraphSummary:
    uri: str
    generated_at: str = field(metadata={"json": "generated_at,omitempty"})
    edges: int
    nodes: int
    edge_kinds: tuple[KindCount, ...]
    node_kinds: tuple[KindCount, ...]
    extractors: tuple[str, ...] = field(metadata={"json": "extractors,omitempty"})
    stats: EdgeScanStats


def _sorted_kind_counts(counts: dict[str, int]) -> tuple[KindCount, ...]:
    return tuple(KindCount(kind, count) for kind, count in sorted(counts.items(), key=lambda item: (-item[1], item[0])))


def _summarize_graph(base: Base, *, full: bool, cancel: Cancellation | None = None) -> GraphSummary:
    edge_kinds: dict[str, int] = {}
    node_kinds: dict[str, int] = {}
    nodes: set[str] = set()

    def inspect(edge: Edge) -> None:
        check_cancel(cancel)
        edge_kinds[edge.kind] = edge_kinds.get(edge.kind, 0) + 1
        for uri in (edge.src, edge.dst):
            if uri in nodes:
                continue
            nodes.add(uri)
            kind = node_kind(uri)
            node_kinds[kind] = node_kinds.get(kind, 0) + 1

    with open_validated_graph_cache(base, full_inputs=full, full_outputs=full, cancel=cancel) as cache:
        stats = scan_graph_rows(base, cache, inspect, cancel=cancel)
        summary = GraphSummary(
            uri=GRAPH_FILE,
            generated_at=cache.meta.generated_at,
            edges=sum(edge_kinds.values()),
            nodes=len(nodes),
            edge_kinds=_sorted_kind_counts(edge_kinds),
            node_kinds=_sorted_kind_counts(node_kinds),
            extractors=cache.meta.extractors,
            stats=stats,
        )
        cache.revalidate_bytes()
        return summary


def summarize_graph(base: Base, *, cancel: Cancellation | None = None) -> GraphSummary:
    return _summarize_graph(base, full=False, cancel=cancel)


def verify_graph(base: Base, *, cancel: Cancellation | None = None) -> GraphSummary:
    return _summarize_graph(base, full=True, cancel=cancel)


__all__ = [
    "EDGE_COLUMNS",
    "EDGE_FIELD_COUNT",
    "EDGE_FIELD_SEPARATOR",
    "EDGE_SCHEMA_VERSION",
    "GRAPH_EXTRACTOR_VERSION",
    "MAX_EDGE_LINE_BYTES",
    "MAX_EDGE_LIST_ROWS",
    "MAX_GRAPH_DEPTH",
    "DerivedGraphMissingError",
    "Direction",
    "Edge",
    "EdgeListMeta",
    "EdgeQuery",
    "EdgeScanStats",
    "EdgeValidationError",
    "ExtractCounts",
    "GraphArtifacts",
    "GraphBuild",
    "GraphFileManifest",
    "GraphInputSHA256",
    "GraphInputState",
    "GraphOffset",
    "GraphOutputSHA256",
    "GraphQuery",
    "GraphSHA256Manifest",
    "GraphSummary",
    "IdentityAlias",
    "IdentityResolver",
    "KindCount",
    "NeighbourEdge",
    "Neighbourhood",
    "NodeCount",
    "NodeListing",
    "ResolvedIdentity",
    "ValidatedGraphCache",
    "build_graph",
    "decode_edge",
    "decode_edge_list_meta",
    "decode_graph_offset",
    "dedupe_edges",
    "encode_edges",
    "encode_graph_artifacts",
    "extract_edges",
    "graph_generation_sha256",
    "graph_input_state",
    "graph_input_uris",
    "graph_meta_static_problems",
    "graph_schema_input_sha256",
    "hash_graph_input",
    "list_nodes",
    "neighbours",
    "neighbours_from_cache",
    "new_edge_list_meta",
    "new_graph_input_sha256",
    "node_kind",
    "open_validated_graph_cache",
    "parse_direction",
    "read_current_graph_generation",
    "read_graph_meta",
    "read_validated_graph_artifact",
    "read_validated_graph_meta",
    "scan_edges",
    "scan_graph_rows",
    "sort_edges",
    "summarize_graph",
    "verify_graph",
    "write_edge_list",
]
