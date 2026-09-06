"""Permanent evidence documents and pure collection-output validation."""

from __future__ import annotations

import os
import posixpath
import re
import unicodedata
from collections.abc import Mapping, Sequence
from dataclasses import dataclass, field
from datetime import UTC, date, datetime, timedelta, tzinfo
from pathlib import Path
from typing import TYPE_CHECKING, Final, Protocol, cast
from zoneinfo import ZoneInfo, ZoneInfoNotFoundError

from fkf.errors import OperationalError
from fkf.fields import (
    FIELD_ID,
    FIELD_TIME,
    FIELD_TITLE,
    Cardinality,
    FieldDefinition,
    FieldMap,
    FieldPath,
    FieldSchema,
    validate_field_map,
    validate_field_schema,
    validate_relation_value,
)
from fkf.io import FileTooLargeError, atomic_write, read_file_limited
from fkf.jsoncodec import JsonNumber, JsonValue, dumps, loads
from fkf.store import BASE_FILE_MODE, MAX_SOURCE_DOCUMENT_BYTES, Layer
from fkf.timeutil import Instant, format_rfc3339, parse_record_time, parse_rfc3339

if TYPE_CHECKING:
    from fkf.config import Source

SCHEMA_VERSION: Final = 1

_FRAGMENT_SAFE = frozenset(b"ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789._:/@+-")
_INTEGER_PATTERN = re.compile(r"-?(?:0|[1-9][0-9]*)\Z")
_CANONICAL_UTC_SECOND_PATTERN = re.compile(r"[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z\Z")
_NANOSECONDS_PER_SECOND = 1_000_000_000

type Record = dict[str, JsonValue]


class UnknownSchemaError(OperationalError):
    """A stored evidence envelope needs a different reader."""


class IncompleteCollectionError(OperationalError):
    """Provider output cannot represent one complete requested collection."""


class _DocumentSource(Protocol):
    name: str
    layer: Layer
    format: object
    records: FieldPath | None
    fields: FieldMap
    schema: FieldSchema
    body: tuple[str, ...]

    def has_body(self) -> bool: ...


@dataclass(frozen=True, slots=True)
class Window:
    """One local civil day and its half-open UTC boundaries."""

    date: str
    next: str
    start: str
    end: str


@dataclass(slots=True)
class Document:
    """One self-describing, complete source snapshot or event day."""

    fkf: int = SCHEMA_VERSION
    source: str = ""
    layer: Layer = Layer.EVENTS
    date: str = ""
    window_start: str = ""
    window_end: str = ""
    collected_at: str = ""
    schema: FieldSchema = field(default_factory=FieldSchema)
    fields: FieldMap = field(default_factory=FieldMap)
    body: bool = False
    count: int = 0
    records: list[Record] = field(default_factory=list)

    def uri(self) -> str:
        """Return this document's base-relative address."""
        if self.layer is Layer.INDEX:
            return index_document_uri(self.source)
        return event_document_uri(self.date, self.source)

    def record_uri(self, record: Record) -> str | None:
        """Address one record by its declared identity."""
        identity = self.fields.eval_string(FIELD_ID, record)
        return f"{self.uri()}#{encode_fragment(identity)}" if identity is not None else None

    def find_record(self, identity: str) -> Record | None:
        """Return the record with one exact declared identity."""
        return next(
            (record for record in self.records if self.fields.eval_string(FIELD_ID, record) == identity),
            None,
        )

    def __json_value__(self) -> _StoredDocument:
        """Project runtime field objects onto the permanent evidence envelope."""

        return _stored_document(self)


@dataclass(frozen=True, slots=True)
class _StoredFieldDefinition:
    description: str
    cardinality: Cardinality | str
    relation: bool = field(default=False, metadata={"json": "relation,omitempty"})
    examples: tuple[str, ...] = field(default=(), metadata={"json": "examples,omitempty"})
    weight: int = field(default=0, metadata={"json": "weight,omitempty"})


@dataclass(frozen=True, slots=True)
class _StoredDocument:
    fkf: int
    source: str
    layer: Layer
    date: str = field(default="", metadata={"json": "date,omitempty"})
    window_start: str = field(default="", metadata={"json": "window_start,omitempty"})
    window_end: str = field(default="", metadata={"json": "window_end,omitempty"})
    collected_at: str = ""
    schema: Mapping[str, _StoredFieldDefinition] = field(default_factory=dict)
    fields: Mapping[str, str | list[str]] = field(default_factory=dict)
    body: bool = False
    count: int = 0
    records: Sequence[Record] = ()


def fields_of(source: Source | _DocumentSource) -> FieldMap:
    """Copy the provider paths carried by one source into durable evidence."""
    return FieldMap({name: source.fields.paths(name) for name in source.fields.names()})


def schema_of(source: Source | _DocumentSource) -> FieldSchema:
    """Copy the semantic definitions used by one source."""
    return FieldSchema({name: source.schema[name] for name in source.fields.names()})


def event_document_uri(day: str, source: str) -> str:
    """Return the storage URI of one events source day."""
    return posixpath.normpath(f"events/{day}/{source}.json")


def index_document_uri(source: str) -> str:
    """Return the storage URI of one index source snapshot."""
    return posixpath.normpath(f"index/{source}.json")


def encode_fragment(identity: str) -> str:
    """Percent-encode one UTF-8 record identity with FKF's readable safe set."""
    encoded = [chr(byte) if byte in _FRAGMENT_SAFE else f"%{byte:02X}" for byte in identity.encode("utf-8")]
    return "".join(encoded)


def decode_fragment(fragment: str) -> str:
    """Decode an FKF record fragment and reject malformed UTF-8 or escapes."""
    decoded = bytearray()
    index = 0
    while index < len(fragment):
        char = fragment[index]
        if char != "%":
            decoded.extend(char.encode("utf-8"))
            index += 1
            continue
        if index + 2 >= len(fragment):
            raise ValueError(f"fragment {fragment!r} ends in a truncated percent escape")
        escape = fragment[index + 1 : index + 3]
        try:
            decoded.append(int(escape, 16))
        except ValueError as error:
            raise ValueError(f"fragment {fragment!r} holds an invalid percent escape") from error
        index += 3
    try:
        return decoded.decode("utf-8")
    except UnicodeDecodeError as error:
        raise ValueError(f"fragment {fragment!r} does not decode to valid UTF-8") from error


def _stored_document(document: Document) -> _StoredDocument:
    schema = {
        name: _StoredFieldDefinition(
            description=document.schema[name].description,
            cardinality=document.schema[name].cardinality,
            relation=document.schema[name].relation,
            examples=document.schema[name].examples,
            weight=document.schema[name].weight,
        )
        for name in document.schema.names()
    }
    return _StoredDocument(
        fkf=document.fkf,
        source=document.source,
        layer=document.layer,
        date=document.date,
        window_start=document.window_start,
        window_end=document.window_end,
        collected_at=document.collected_at,
        schema=schema,
        fields=document.fields.to_json_value(),
        body=document.body,
        count=document.count,
        records=document.records,
    )


def encode_document(document: Document) -> bytes:
    """Encode a document as deterministic two-space JSON with one final newline."""
    try:
        return dumps(_stored_document(document), indent=True, newline=True)
    except (TypeError, ValueError) as error:
        raise ValueError(f"encode document {document.uri()}: {error}") from error


def _json_integer(value: object, name: str, *, default: int = 0) -> int:
    if value is None:
        return default
    if not isinstance(value, JsonNumber) or _INTEGER_PATTERN.fullmatch(value.raw) is None:
        raise ValueError(f"field {name} must be an integer")
    return int(value.raw)


def _json_string(value: object, name: str, *, default: str = "") -> str:
    if value is None:
        return default
    if not isinstance(value, str):
        raise ValueError(f"field {name} must be a string")
    return value


def _json_boolean(value: object, name: str, *, default: bool = False) -> bool:
    if value is None:
        return default
    if not isinstance(value, bool):
        raise ValueError(f"field {name} must be a boolean")
    return value


def _field_definition(value: object, name: str) -> FieldDefinition:
    if not isinstance(value, Mapping):
        raise ValueError(f"schema.{name} must be an object")
    description = _json_string(value.get("description"), f"schema.{name}.description")
    cardinality_raw = _json_string(value.get("cardinality"), f"schema.{name}.cardinality")
    try:
        cardinality: Cardinality | str = Cardinality(cardinality_raw)
    except ValueError:
        cardinality = cardinality_raw
    relation = _json_boolean(value.get("relation"), f"schema.{name}.relation")
    raw_examples = value.get("examples", [])
    if not isinstance(raw_examples, list) or any(not isinstance(example, str) for example in raw_examples):
        raise ValueError(f"schema.{name}.examples must be an array of strings")
    weight = _json_integer(value.get("weight"), f"schema.{name}.weight")
    return FieldDefinition(
        description=description,
        cardinality=cast(Cardinality, cardinality),
        relation=relation,
        examples=tuple(cast(list[str], raw_examples)),
        weight=weight,
    )


def _field_schema(value: object) -> FieldSchema:
    if value is None:
        return FieldSchema()
    if not isinstance(value, Mapping):
        raise ValueError("field schema must be an object")
    definitions: dict[str, FieldDefinition] = {}
    for name, raw_definition in value.items():
        if not isinstance(name, str):
            raise ValueError("schema field names must be strings")
        definitions[name] = _field_definition(raw_definition, name)
    return FieldSchema(definitions)


def _records(value: object) -> list[Record]:
    if value is None:
        return []
    if not isinstance(value, list):
        raise ValueError("field records must be an array")
    records: list[Record] = []
    for index, raw_record in enumerate(value):
        if not isinstance(raw_record, Mapping):
            raise ValueError(f"record {index} must be a JSON object")
        records.append(dict(cast(Mapping[str, JsonValue], raw_record)))
    return records


def decode_document(data: str | bytes | bytearray | memoryview, path: str = "document") -> Document:
    """Decode one additive v1 evidence envelope and reject trailing JSON."""
    try:
        value = loads(data)
    except ValueError as error:
        label = "invalid trailing JSON" if "Extra data" in str(error) else "invalid JSON"
        raise ValueError(f"decode {path}: {label}: {error}") from error
    if not isinstance(value, Mapping):
        raise ValueError(f"decode {path}: document must be a JSON object")
    try:
        marker = _json_integer(value.get("fkf"), "fkf")
        if marker != SCHEMA_VERSION:
            raise UnknownSchemaError(
                f"{path} declares unsupported evidence envelope fkf {marker}; use a build that reads marker {marker}"
            )
        raw_layer = _json_string(value.get("layer"), "layer")
        try:
            layer = Layer(raw_layer)
        except ValueError as error:
            raise UnknownSchemaError(
                f"{path} declares layer {raw_layer!r}; a stored document is filed under events or index"
            ) from error
        if layer not in {Layer.EVENTS, Layer.INDEX}:
            raise UnknownSchemaError(
                f"{path} declares layer {raw_layer!r}; a stored document is filed under events or index"
            )
        raw_fields = value.get("fields")
        fields = FieldMap() if raw_fields is None else FieldMap.from_json_value(raw_fields)
        return Document(
            fkf=marker,
            source=_json_string(value.get("source"), "source"),
            layer=layer,
            date=_json_string(value.get("date"), "date"),
            window_start=_json_string(value.get("window_start"), "window_start"),
            window_end=_json_string(value.get("window_end"), "window_end"),
            collected_at=_json_string(value.get("collected_at"), "collected_at"),
            schema=_field_schema(value.get("schema")),
            fields=fields,
            body=_json_boolean(value.get("body"), "body"),
            count=_json_integer(value.get("count"), "count"),
            records=_records(value.get("records")),
        )
    except UnknownSchemaError:
        raise
    except (TypeError, ValueError) as error:
        raise ValueError(f"decode {path}: {error}") from error


def read_document(path: str | os.PathLike[str]) -> Document:
    """Read and decode one bounded stored document."""
    candidate = Path(path)
    return decode_document(read_file_limited(candidate, MAX_SOURCE_DOCUMENT_BYTES), str(candidate))


def write_document(path: str | os.PathLike[str], document: Document) -> None:
    """Atomically replace one document, refusing bytes its reader cannot accept."""
    candidate = Path(path)
    encoded = encode_document(document)
    if len(encoded) > MAX_SOURCE_DOCUMENT_BYTES:
        raise FileTooLargeError(
            f"encoded document {candidate} is {len(encoded)} bytes (limit {MAX_SOURCE_DOCUMENT_BYTES})"
        )
    atomic_write(candidate, encoded, mode=BASE_FILE_MODE)


def _strict_date(value: str, label: str) -> date:
    try:
        parsed = date.fromisoformat(value)
    except ValueError as error:
        raise ValueError(f"{label} {value!r} is not YYYY-MM-DD") from error
    if parsed.isoformat() != value:
        raise ValueError(f"{label} {value!r} is not YYYY-MM-DD")
    return parsed


def _canonical_utc_bound(name: str, value: str) -> Instant:
    if _CANONICAL_UTC_SECOND_PATTERN.fullmatch(value) is None:
        try:
            parsed = parse_rfc3339(value)
        except ValueError as error:
            raise ValueError(f"event document {name} {value!r} is not RFC 3339: {error}") from error
        raise ValueError(f"event document {name} {value!r} is not canonical UTC; want {format_rfc3339(parsed)!r}")
    try:
        return parse_rfc3339(value)
    except ValueError as error:
        raise ValueError(f"event document {name} {value!r} is not RFC 3339: {error}") from error


def _local_timezone() -> tzinfo:
    configured = os.environ.get("TZ", "").strip()
    if configured:
        try:
            return ZoneInfo(configured)
        except ZoneInfoNotFoundError:
            pass
    # ``astimezone().tzinfo`` is commonly only today's fixed offset. The system zonefile
    # retains the historical DST rules needed by additive v1 documents without stored bounds.
    try:
        with Path("/etc/localtime").open("rb") as zonefile:
            return ZoneInfo.from_file(zonefile, key="local")
    except OSError, ValueError:
        pass
    return datetime.now().astimezone().tzinfo or UTC


def _event_window(document: Document, zone: tzinfo | None) -> Window:
    if not document.window_start and not document.window_end:
        return day_window(parse_day_in_location(document.date, zone or _local_timezone()))
    if not document.window_start or not document.window_end:
        raise ValueError("event document must declare both window_start and window_end or neither")
    start = _canonical_utc_bound("window_start", document.window_start)
    end = _canonical_utc_bound("window_end", document.window_end)
    if start >= end:
        raise ValueError(f"event document window [{document.window_start}, {document.window_end}) is empty or reversed")
    span = end.unix_nanoseconds - start.unix_nanoseconds
    if span < 20 * 3600 * _NANOSECONDS_PER_SECOND or span > 48 * 3600 * _NANOSECONDS_PER_SECOND:
        raise ValueError(
            f"event document window spans {span / _NANOSECONDS_PER_SECOND:g}s; a civil day must span 20h..48h"
        )

    document_day = _strict_date(document.date, "event document date")
    utc_start = Instant.from_datetime(datetime.combine(document_day, datetime.min.time(), UTC))
    utc_end = Instant.from_datetime(datetime.combine(document_day + timedelta(days=1), datetime.min.time(), UTC))
    maximum = 18 * 3600 * _NANOSECONDS_PER_SECOND
    if (
        abs(start.unix_nanoseconds - utc_start.unix_nanoseconds) > maximum
        or abs(end.unix_nanoseconds - utc_end.unix_nanoseconds) > maximum
    ):
        raise ValueError(
            f"event document window [{document.window_start}, {document.window_end}) "
            f"is not aligned with civil date {document.date}"
        )
    return Window(
        document.date, (document_day + timedelta(days=1)).isoformat(), document.window_start, document.window_end
    )


def _civil_date_value(raw: str) -> str | None:
    value = raw.strip()
    try:
        parsed = date.fromisoformat(value)
    except ValueError:
        return None
    return value if len(value) == 10 and parsed.isoformat() == value else None


def _verify_record(document: Document, record: Record, index: int) -> str:
    for name in document.fields.names():
        definition = document.schema[name]
        try:
            projected = document.fields.eval_declared_field(name, record, definition)
        except ValueError as error:
            raise ValueError(f"record {index} field {name}: {error}") from error
        if not definition.cardinality.allows(len(projected)):
            raise ValueError(
                f"record {index} field {name} projects {len(projected)} values; "
                f"schema cardinality {definition.cardinality}"
            )
        if definition.relation:
            for value in projected:
                try:
                    validate_relation_value(value)
                except ValueError as error:
                    raise ValueError(
                        f"record {index}: field {name} value {value!r} is not a canonical relation URI: {error}"
                    ) from error
    identity = document.fields.eval_string(FIELD_ID, record)
    if identity is None:
        raise ValueError(
            f"record {index} has no value at the declared fields.id paths "
            f"{[str(path) for path in document.fields.paths(FIELD_ID)]}"
        )
    return identity


def _verify_records_within_window(document: Document, window: Window) -> None:
    start = parse_rfc3339(window.start)
    end = parse_rfc3339(window.end)
    for index, record in enumerate(document.records):
        raw = document.fields.eval_string(FIELD_TIME, record)
        if raw is None:
            raise ValueError(
                f"record {index} has no value at the declared fields.time paths "
                f"{[str(path) for path in document.fields.paths(FIELD_TIME)]}"
            )
        civil = _civil_date_value(raw)
        if civil is not None:
            if civil != window.date:
                raise ValueError(
                    f"record {index} has civil date {civil} outside the requested window [{window.start}, {window.end})"
                )
            continue
        try:
            instant = parse_record_time(raw)
        except ValueError as error:
            raise ValueError(f"record {index}: {error}") from error
        if instant < start or instant >= end:
            raise ValueError(
                f"record {index} has time {format_rfc3339(instant)} "
                f"outside the requested window [{window.start}, {window.end})"
            )


def verify_document(document: Document, *, zone: tzinfo | None = None) -> None:
    """Validate a stored document's definition, identities, and event membership."""
    if document.fkf != SCHEMA_VERSION:
        raise UnknownSchemaError(f"unsupported evidence envelope fkf {document.fkf}")
    if document.layer not in {Layer.EVENTS, Layer.INDEX}:
        raise UnknownSchemaError(f"stored document declares unsupported layer {document.layer!r}")
    if document.count != len(document.records):
        raise ValueError(f"document count {document.count} does not match {len(document.records)} records")
    if not document.source.strip():
        raise ValueError("document declares no source")
    # Import lazily so the evidence reader does not pull the configuration loader into offline reads.
    from fkf.config import validate_source_name

    try:
        validate_source_name(document.source)
    except ValueError as error:
        raise ValueError(f"document source {document.source!r}: {error}") from error
    try:
        parse_rfc3339(document.collected_at)
    except ValueError as error:
        raise ValueError(f"document collected_at {document.collected_at!r} is not RFC 3339: {error}") from error
    try:
        validate_field_map(document.fields, event=document.layer is Layer.EVENTS)
    except ValueError as error:
        raise ValueError(f"document field map: {error}") from error
    try:
        validate_field_schema(document.schema)
    except ValueError as error:
        raise ValueError(f"document schema: {error}") from error
    if len(document.schema) != len(document.fields):
        raise ValueError(
            f"document schema declares {len(document.schema)} fields but the field map uses {len(document.fields)}"
        )
    for name in document.fields.names():
        if name not in document.schema:
            raise ValueError(f"document field {name} is not declared in its schema")

    if document.layer is Layer.EVENTS:
        _strict_date(document.date, "event document date")
    elif document.date:
        raise ValueError(f"index document declares date {document.date!r}; an index is a point-in-time snapshot")
    elif document.window_start or document.window_end:
        raise ValueError("index document declares an event collection window")

    seen: dict[str, int] = {}
    for index, record in enumerate(document.records):
        identity = _verify_record(document, record, index)
        if identity in seen:
            raise ValueError(
                f"records {seen[identity]} and {index} share the id {identity!r} at fields.id paths "
                f"{[str(path) for path in document.fields.paths(FIELD_ID)]}; "
                "a record URI must name exactly one record"
            )
        seen[identity] = index
    if document.layer is Layer.EVENTS:
        _verify_records_within_window(document, _event_window(document, zone))


def _format_collection_time(value: datetime | None) -> str:
    current = value or datetime.now(UTC)
    if current.tzinfo is None or current.utcoffset() is None:
        raise ValueError("collected_at must have an explicit timezone")
    return current.astimezone(UTC).replace(microsecond=0).isoformat().replace("+00:00", "Z")


def _verify_collected_titles(source: Source | _DocumentSource, records: Sequence[Record]) -> None:
    if source.fields.path(FIELD_TITLE).is_zero:
        return
    for index, record in enumerate(records):
        title = source.fields.eval_string(FIELD_TITLE, record)
        if title is None or not title.strip():
            raise ValueError(
                f"record {index} has no meaningful title at the declared fields.title paths "
                f"{[str(path) for path in source.fields.paths(FIELD_TITLE)]}"
            )
        for char in title:
            if unicodedata.category(char) in {"Cc", "Cf"}:
                raise ValueError(f"record {index} title contains control or invisible character U+{ord(char):04X}")


def _new_document(
    source: Source | _DocumentSource,
    records: Sequence[Record],
    window: Window | None,
    collected_at: datetime | None,
) -> Document:
    if source.layer is Layer.EVENTS and window is None:
        raise ValueError("an events source requires a collection window")
    if source.layer is Layer.INDEX and window is not None:
        raise ValueError("an index source cannot declare an event collection window")
    document = Document(
        source=source.name,
        layer=source.layer,
        collected_at=_format_collection_time(collected_at),
        schema=schema_of(source),
        fields=fields_of(source),
        body=source.has_body(),
        count=len(records),
        records=list(records),
    )
    if window is not None:
        document.date = window.date
        document.window_start = window.start
        document.window_end = window.end
    return document


def _incomplete(source: Source | _DocumentSource, error: BaseException) -> IncompleteCollectionError:
    return IncompleteCollectionError(f"source {source.name}: {error}", cause=error)


def build_document(
    source: Source | _DocumentSource,
    records: Sequence[Record],
    *,
    window: Window | None = None,
    collected_at: datetime | None = None,
) -> Document:
    """Construct and validate one complete document without executing or writing."""
    try:
        document = _new_document(source, records, window, collected_at)
        verify_document(document)
        _verify_collected_titles(source, records)
    except (UnknownSchemaError, ValueError) as error:
        raise _incomplete(source, error) from error
    return document


def _valid_local_candidates(local: datetime, zone: tzinfo) -> list[datetime]:
    candidates: dict[datetime, datetime] = {}
    for fold in (0, 1):
        candidate = local.replace(tzinfo=zone, fold=fold)
        utc = candidate.astimezone(UTC)
        if utc.astimezone(zone).replace(tzinfo=None) == local:
            candidates[utc] = candidate
    return [candidates[key] for key in sorted(candidates)]


def parse_day_in_location(value: str, zone: tzinfo) -> datetime:
    """Parse an existing civil date at a transition-safe local noon."""
    label = value.strip()
    parsed = _strict_date(label, "date")
    candidates = _valid_local_candidates(datetime.combine(parsed, datetime.min.time()).replace(hour=12), zone)
    if not candidates:
        raise ValueError(f"civil date does not exist: {label} in {zone}")
    return candidates[0]


def parse_day(value: str) -> datetime:
    """Parse an existing civil date in the process's local timezone."""
    return parse_day_in_location(value, _local_timezone())


def _start_of_civil_day(day: date, zone: tzinfo) -> datetime:
    midnight = datetime.combine(day, datetime.min.time())
    for minutes in range(4 * 60 + 1):
        candidates = _valid_local_candidates(midnight + timedelta(minutes=minutes), zone)
        if candidates:
            return candidates[0]
    raise ValueError(f"civil date {day.isoformat()} in {zone} has no boundary within four hours")


def _next_civil_date(day: date, zone: tzinfo) -> date:
    # Date-line changes can remove a label entirely; a missing label is not a zero-length day.
    candidate = day + timedelta(days=1)
    for _ in range(3):
        if _valid_local_candidates(datetime.combine(candidate, datetime.min.time()).replace(hour=12), zone):
            return candidate
        candidate += timedelta(days=1)
    raise ValueError(f"no civil date follows {day.isoformat()} in {zone} within three labels")


def day_window(day: datetime) -> Window:
    """Build the honest half-open UTC window for one local civil day."""
    if day.tzinfo is None or day.utcoffset() is None:
        raise ValueError("day must have an explicit timezone")
    zone = day.tzinfo
    label = day.date()
    start = _start_of_civil_day(label, zone)
    next_day = _next_civil_date(label, zone)
    end = _start_of_civil_day(next_day, zone)
    return Window(
        date=label.isoformat(),
        next=next_day.isoformat(),
        start=format_rfc3339(Instant.from_datetime(start)),
        end=format_rfc3339(Instant.from_datetime(end)),
    )


def _format_name(value: object) -> str:
    if isinstance(value, Mapping):
        return "object"
    if isinstance(value, list):
        return "array"
    if value is None:
        return "null"
    if isinstance(value, bool):
        return "boolean"
    if isinstance(value, JsonNumber):
        return "number"
    return type(value).__name__


def _object_records(values: Sequence[object], origin: str) -> list[Record]:
    records: list[Record] = []
    for value in values:
        if not isinstance(value, Mapping):
            raise ValueError(f"{origin}: a record must be a JSON object, got {_format_name(value)}")
        records.append(dict(cast(Mapping[str, JsonValue], value)))
    return records


def _records_from(
    source: Source | _DocumentSource, value: object, origin: str, *, whole_document: bool
) -> list[Record]:
    path = source.records
    if path is None or path.is_zero:
        if isinstance(value, list):
            return _object_records(value, origin)
        if isinstance(value, Mapping):
            if whole_document:
                raise ValueError(
                    f"{origin}: expected a JSON array of records, got an object "
                    "(declare `records:` when the command wraps them in an envelope)"
                )
            return [dict(cast(Mapping[str, JsonValue], value))]
        raise ValueError(f"{origin}: expected a JSON object or array, got {_format_name(value)}")

    selected = path.eval(value)
    if not selected:
        if whole_document:
            raise ValueError(f"{origin}: declared records path {path} selected nothing")
        return []
    records: list[Record] = []
    for item in selected:
        if isinstance(item, list):
            records.extend(_object_records(item, origin))
        elif isinstance(item, Mapping):
            records.append(dict(cast(Mapping[str, JsonValue], item)))
        else:
            raise ValueError(f"{origin}: {path} selected a {_format_name(item)}; a record must be a JSON object")
    return records


def _decode_output_value(text: str | bytes) -> JsonValue:
    try:
        return loads(text)
    except ValueError as error:
        if "Extra data" in str(error):
            raise ValueError(
                "output holds more than one JSON document: set `format: ndjson` if the command prints one per line, "
                "or `records: <path>` if it prints a paginated envelope"
            ) from error
        raise ValueError(f"output is not valid JSON: {error}") from error


def _output_format(source: Source | _DocumentSource) -> str:
    value = source.format
    return cast(str, getattr(value, "value", value))


def decode_records(source: Source | _DocumentSource, stdout: str | bytes) -> list[Record]:
    """Decode one command's complete JSON or NDJSON standard output."""
    try:
        if not stdout.strip():
            if _output_format(source) == "ndjson":
                return []
            raise ValueError(
                "command exited zero but printed nothing; a CLI emitting json prints [] for an empty result"
            )
        if _output_format(source) != "ndjson":
            return _records_from(source, _decode_output_value(stdout), "output", whole_document=True)

        records: list[Record] = []
        lines = stdout.splitlines()
        for number, line in enumerate(lines, start=1):
            if not line.strip():
                continue
            try:
                value = _decode_output_value(line)
            except ValueError as error:
                raise ValueError(f"line {number}: {error}") from error
            records.extend(_records_from(source, value, f"line {number}", whole_document=False))
        return records
    except ValueError as error:
        raise _incomplete(source, error) from error


@dataclass(frozen=True, slots=True)
class _DayBound:
    date: str
    start: Instant
    end: Instant
    window: Window


def _collection_bounds(dates: Sequence[str], zone: tzinfo) -> list[_DayBound]:
    if not dates:
        raise ValueError("a windowed collection requires at least one date")
    bounds: list[_DayBound] = []
    previous: date | None = None
    for label in dates:
        parsed = _strict_date(label, "requested day")
        if previous is not None and parsed != _next_civil_date(previous, zone):
            raise ValueError("requested dates must be ascending and contiguous")
        window = day_window(parse_day_in_location(label, zone))
        bounds.append(_DayBound(label, parse_rfc3339(window.start), parse_rfc3339(window.end), window))
        previous = parsed
    return bounds


def _record_collection_date(
    source: Source | _DocumentSource,
    record: Record,
    index: int,
    bounds: Sequence[_DayBound],
) -> str:
    values = source.fields.eval_strings(FIELD_TIME, record)
    if not values:
        raise ValueError(
            f"record {index} has no value at the declared fields.time paths "
            f"{[str(path) for path in source.fields.paths(FIELD_TIME)]}"
        )
    if len(values) > 1:
        definition = source.schema[FIELD_TIME]
        raise ValueError(
            f"record {index} field time projects {len(values)} values; schema cardinality {definition.cardinality}"
        )
    raw = values[0]
    civil = _civil_date_value(raw)
    if civil is not None:
        if any(bound.date == civil for bound in bounds):
            return civil
        raise ValueError(
            f"record {index} has civil date {civil}, which falls outside the requested window "
            f"{bounds[0].date}..{bounds[-1].date}"
        )
    try:
        instant = parse_record_time(raw)
    except ValueError as error:
        raise ValueError(f"record {index}: {error}") from error
    for bound in bounds:
        if bound.start <= instant < bound.end:
            return bound.date
    raise ValueError(
        f"record {index} has time {format_rfc3339(instant)}, which falls outside the requested window "
        f"{bounds[0].date}..{bounds[-1].date}"
    )


def build_window_documents(
    source: Source | _DocumentSource,
    records: Sequence[Record],
    dates: Sequence[str],
    zone: tzinfo,
    *,
    collected_at: datetime | None = None,
) -> dict[str, Document]:
    """Bucket one range result into a complete document for every requested day."""
    try:
        if source.layer is not Layer.EVENTS:
            raise ValueError("windowed collection requires an events source")
        bounds = _collection_bounds(dates, zone)
        buckets: dict[str, list[Record]] = {bound.date: [] for bound in bounds}
        for index, record in enumerate(records):
            buckets[_record_collection_date(source, record, index, bounds)].append(record)
        documents: dict[str, Document] = {}
        for bound in bounds:
            day_records = buckets[bound.date]
            document = _new_document(source, day_records, bound.window, collected_at)
            verify_document(document)
            _verify_collected_titles(source, day_records)
            documents[bound.date] = document
        return documents
    except (UnknownSchemaError, ValueError) as error:
        raise _incomplete(source, error) from error


verify_records = verify_document

__all__ = [
    "SCHEMA_VERSION",
    "Document",
    "IncompleteCollectionError",
    "Record",
    "UnknownSchemaError",
    "Window",
    "build_document",
    "build_window_documents",
    "day_window",
    "decode_document",
    "decode_fragment",
    "decode_records",
    "encode_document",
    "encode_fragment",
    "event_document_uri",
    "fields_of",
    "index_document_uri",
    "parse_day",
    "parse_day_in_location",
    "read_document",
    "schema_of",
    "verify_document",
    "verify_records",
    "write_document",
]
