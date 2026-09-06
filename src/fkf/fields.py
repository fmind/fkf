"""Typed semantic fields, pure addressing paths, and canonical relation URIs."""

from __future__ import annotations

import json
import posixpath
import re
from collections.abc import Iterator, Mapping, Sequence
from dataclasses import dataclass
from datetime import date
from enum import StrEnum
from types import MappingProxyType
from typing import overload
from urllib.parse import quote_plus, unquote_plus, urlsplit

from fkf.jsoncodec import JsonNumber, format_float_go

CONFIG_VERSION = 1

FIELD_ID = "id"
FIELD_TIME = "time"
FIELD_TITLE = "title"
FIELD_URL = "url"
FIELD_CATEGORY = "category"
FIELD_VISIBILITY = "visibility"

MAX_FIELD_DESCRIPTION_LENGTH = 512
MAX_FIELD_EXAMPLES = 8
MAX_FIELD_EXAMPLE_LENGTH = 512
MAX_FIELD_WEIGHT = 100
DEFAULT_ID_FIELD_WEIGHT = 10
DEFAULT_TITLE_FIELD_WEIGHT = 5
DEFAULT_FIELD_WEIGHT = 1
MAX_FIELD_NAME_LENGTH = 64
MAX_FIELDS = 64
MAX_PATHS_PER_FIELD = 32
MAX_FIELD_PATH_BYTES = 4096
MAX_FIELD_PATH_STEPS = 128

_FIELD_NAME_PATTERN = re.compile(r"[a-z][a-z0-9_-]*\Z")
_ENTITY_REFERENCE_PATTERN = re.compile(r"([a-z][a-z0-9+.-]*):(.+)\Z")
_ENTITY_SCHEME_PATTERN = re.compile(r"[a-z][a-z0-9+.-]*\Z")
_SOURCE_NAME_PATTERN = re.compile(r"[a-z0-9][a-z0-9-]*\Z")
_PAGE_SLUG_PATTERN = re.compile(r"[a-z0-9][a-z0-9._-]*\Z")
_UPPER_HEX = frozenset("0123456789ABCDEF")
_IDENTITY_SAFE = frozenset("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789._:/@+-")
_RESERVED_ENTITY_SCHEMES = frozenset({"file", "external", "http", "https", "ftp", "mailto"})
_WELL_KNOWN_FIELDS = frozenset({FIELD_ID, FIELD_TIME, FIELD_TITLE, FIELD_URL})


class Cardinality(StrEnum):
    """The permitted scalar count for one projected semantic field."""

    ONE = "one"
    OPTIONAL = "optional"
    MANY = "many"

    def allows(self, count: int) -> bool:
        """Return whether a projected scalar count satisfies this declaration."""
        if self is Cardinality.ONE:
            return count == 1
        if self is Cardinality.OPTIONAL:
            return count <= 1
        return count >= 0

    @property
    def max_one(self) -> bool:
        """Return whether consumers may safely request a single scalar."""
        return self in {Cardinality.ONE, Cardinality.OPTIONAL}


@dataclass(frozen=True, slots=True)
class FieldDefinition:
    """One base-chosen semantic field declaration."""

    description: str
    cardinality: Cardinality
    relation: bool = False
    examples: tuple[str, ...] = ()
    weight: int = 0

    def __post_init__(self) -> None:
        object.__setattr__(self, "examples", tuple(self.examples))


class _StepKind(StrEnum):
    KEY = "key"
    INDEX = "index"
    ITERATE = "iterate"


@dataclass(frozen=True, slots=True)
class _FieldStep:
    kind: _StepKind
    key: str = ""
    index: int = 0


@dataclass(frozen=True, slots=True)
class FieldPath:
    """A compiled path in FKF's deliberately small jq addressing subset."""

    raw: str = ""
    _steps: tuple[_FieldStep, ...] = ()

    @classmethod
    def parse(cls, raw: str) -> FieldPath:
        """Compile one path and reject every jq feature beyond addressing."""
        trimmed = raw.strip()
        if not trimmed:
            raise ValueError("field path is empty")
        if _byte_length(trimmed) > MAX_FIELD_PATH_BYTES:
            raise ValueError(
                f"field path is {_byte_length(trimmed)} bytes; expected at most {MAX_FIELD_PATH_BYTES} bytes"
            )
        if not trimmed.startswith("."):
            raise ValueError(f"field path {raw!r} must start with `.` (for example `.id` or `.a.b[0]`)")

        steps: list[_FieldStep] = []
        if trimmed == ".":
            return cls(trimmed, ())
        # The initial dot denotes the root; a bracket may address that root directly.
        rest = trimmed[1:] if trimmed.startswith(".[") else trimmed
        while rest:
            if rest.startswith("."):
                rest = _consume_key(rest[1:], raw, steps)
            elif rest.startswith("["):
                rest = _consume_bracket(rest[1:], raw, steps)
            else:
                raise ValueError(f"field path {raw!r}: expected `.` or `[` at {rest!r}")
        return cls(trimmed, tuple(steps))

    @property
    def is_zero(self) -> bool:
        """Return whether this is the sentinel for an undeclared path."""
        return not self.raw

    def eval(self, value: object) -> list[object]:
        """Evaluate the path as a total, deterministic function over decoded JSON."""
        if self.is_zero:
            return []
        current: list[object] = [value]
        for step in self._steps:
            following: list[object] = []
            for item in current:
                _apply_step(step, item, following)
            if not following:
                return []
            current = following
        return current

    def eval_strings(self, value: object) -> list[str]:
        """Render selected scalars in path order with duplicates removed."""
        rendered: list[str] = []
        seen: set[str] = set()
        for item in self.eval(value):
            text = scalar_string(item)
            if text is None or text in seen:
                continue
            seen.add(text)
            rendered.append(text)
        return rendered

    def eval_string(self, value: object) -> str | None:
        """Return the selected value only when there is exactly one scalar."""
        values = self.eval_strings(value)
        return values[0] if len(values) == 1 else None

    def __str__(self) -> str:
        return self.raw


def parse_field_path(raw: str) -> FieldPath:
    """Compile one field path using the public function-style API."""
    return FieldPath.parse(raw)


def _consume_key(rest: str, raw: str, steps: list[_FieldStep]) -> str:
    if rest.startswith('"'):
        try:
            key, end = json.JSONDecoder().raw_decode(rest)
        except json.JSONDecodeError as error:
            raise ValueError(f"field path {raw!r}: quoted key is not a valid JSON string: {error.msg}") from error
        if not isinstance(key, str):
            raise ValueError(f"field path {raw!r}: quoted key must be a JSON string")
        _append_step(steps, _FieldStep(_StepKind.KEY, key=key), raw)
        return rest[end:]

    ends = [position for marker in (".", "[") if (position := rest.find(marker)) >= 0]
    end = min(ends) if ends else len(rest)
    key = rest[:end]
    if not key:
        raise ValueError(f'field path {raw!r}: empty key; quote it as ."…" if the key really is empty')
    for char in key:
        if not (char == "_" or (char.isascii() and char.isalnum())):
            raise ValueError(f"field path {raw!r}: key {key!r} contains {char!r}; quote it")
    _append_step(steps, _FieldStep(_StepKind.KEY, key=key), raw)
    return rest[end:]


def _consume_bracket(rest: str, raw: str, steps: list[_FieldStep]) -> str:
    end = rest.find("]")
    if end < 0:
        raise ValueError(f"field path {raw!r}: `[` is not closed")
    inner = rest[:end].strip()
    if not inner:
        _append_step(steps, _FieldStep(_StepKind.ITERATE), raw)
        return rest[end + 1 :]
    if re.fullmatch(r"[+-]?[0-9]+", inner) is None:
        raise ValueError(f"field path {raw!r}: `[{inner}]` must be empty or an integer index")
    try:
        index = int(inner)
    except ValueError as error:
        raise ValueError(f"field path {raw!r}: `[{inner}]` must be empty or an integer index") from error
    if index < -(1 << 63) or index > (1 << 63) - 1:
        raise ValueError(f"field path {raw!r}: `[{inner}]` exceeds the supported integer range")
    _append_step(steps, _FieldStep(_StepKind.INDEX, index=index), raw)
    return rest[end + 1 :]


def _append_step(steps: list[_FieldStep], step: _FieldStep, raw: str) -> None:
    if len(steps) >= MAX_FIELD_PATH_STEPS:
        raise ValueError(f"field path {raw!r} has more than {MAX_FIELD_PATH_STEPS} steps")
    steps.append(step)


def _apply_step(step: _FieldStep, item: object, into: list[object]) -> None:
    if step.kind is _StepKind.KEY:
        if isinstance(item, Mapping) and step.key in item:
            selected = item[step.key]
            if selected is not None:
                into.append(selected)
        return
    if step.kind is _StepKind.INDEX:
        if isinstance(item, list):
            index = step.index + len(item) if step.index < 0 else step.index
            if 0 <= index < len(item) and item[index] is not None:
                into.append(item[index])
        return
    if isinstance(item, list):
        into.extend(element for element in item if element is not None)
    elif isinstance(item, Mapping):
        into.extend(item[key] for key in sorted(item) if item[key] is not None)


@dataclass(frozen=True, slots=True)
class FieldPaths(Sequence[FieldPath]):
    """One or more ordered alternative projections for a semantic field."""

    values: tuple[FieldPath, ...]

    def __post_init__(self) -> None:
        object.__setattr__(self, "values", tuple(self.values))

    @classmethod
    def from_json_value(cls, value: object) -> FieldPaths:
        """Parse the compact public string-or-list representation."""
        if isinstance(value, str):
            return cls((FieldPath.parse(value),))
        if not isinstance(value, list):
            raise ValueError("field paths must be a path string or a list of path strings")
        paths: list[FieldPath] = []
        for index, raw in enumerate(value):
            if not isinstance(raw, str):
                raise ValueError(f"field path {index} must be a string")
            try:
                paths.append(FieldPath.parse(raw))
            except ValueError as error:
                raise ValueError(f"field path {index}: {error}") from error
        return cls(tuple(paths))

    def to_json_value(self) -> str | list[str]:
        """Return the compact public representation."""
        if len(self.values) == 1:
            return self.values[0].raw
        return [path.raw for path in self.values]

    @overload
    def __getitem__(self, index: int) -> FieldPath: ...

    @overload
    def __getitem__(self, index: slice) -> Sequence[FieldPath]: ...

    def __getitem__(self, index: int | slice) -> FieldPath | Sequence[FieldPath]:
        return self.values[index]

    def __len__(self) -> int:
        return len(self.values)

    def __iter__(self) -> Iterator[FieldPath]:
        return iter(self.values)


class FieldMap(Mapping[str, FieldPaths]):
    """A source's open map from semantic names to provider paths."""

    def __init__(self, fields: Mapping[str, FieldPaths] | None = None) -> None:
        self._fields = MappingProxyType(dict(fields or {}))

    @classmethod
    def from_json_value(cls, value: object) -> FieldMap:
        """Parse an object holding compact string-or-list path declarations."""
        if not isinstance(value, Mapping):
            raise ValueError("fields must be an object")
        fields: dict[str, FieldPaths] = {}
        for name, paths in value.items():
            if not isinstance(name, str):
                raise ValueError("field names must be strings")
            try:
                fields[name] = FieldPaths.from_json_value(paths)
            except ValueError as error:
                raise ValueError(f"fields.{name}: {error}") from error
        return cls(fields)

    def to_json_value(self) -> dict[str, str | list[str]]:
        """Return the deterministic compact public representation."""
        return {name: self._fields[name].to_json_value() for name in self.names()}

    def names(self) -> list[str]:
        """Return semantic names in deterministic order."""
        return sorted(self._fields)

    def paths(self, name: str) -> FieldPaths:
        """Return every path declared for one field."""
        return self._fields.get(name, FieldPaths(()))

    def path(self, name: str) -> FieldPath:
        """Return the first declared path or the zero path sentinel."""
        paths = self.paths(name)
        return paths[0] if paths else FieldPath()

    def eval_strings(self, name: str, value: object) -> list[str]:
        """Union all declared paths in order and remove duplicate scalars."""
        values: list[str] = []
        seen: set[str] = set()
        for path in self.paths(name):
            for projected in path.eval_strings(value):
                if projected in seen:
                    continue
                seen.add(projected)
                values.append(projected)
        return values

    def eval_string(self, name: str, value: object) -> str | None:
        """Return one scalar only when the complete path union has exactly one."""
        values = self.eval_strings(name, value)
        return values[0] if len(values) == 1 else None

    def eval_field(self, name: str, value: object) -> list[str]:
        """Project a field while rejecting selected objects and arrays."""
        return self._eval_field(name, value, Cardinality.MANY, exact_strings=False)

    def eval_relation(self, name: str, value: object) -> list[str]:
        """Project relation identities without trimming provider strings."""
        return self._eval_field(name, value, Cardinality.MANY, exact_strings=True)

    def eval_declared_field(self, name: str, value: object, definition: FieldDefinition) -> list[str]:
        """Project one field using its schema's empty and relation semantics."""
        return self._eval_field(name, value, definition.cardinality, exact_strings=definition.relation)

    def _eval_field(
        self,
        name: str,
        value: object,
        cardinality: Cardinality,
        *,
        exact_strings: bool,
    ) -> list[str]:
        values: list[str] = []
        seen: set[str] = set()
        for path in self.paths(name):
            for selected in path.eval(value):
                if isinstance(selected, str) and not selected.strip():
                    if cardinality is Cardinality.ONE:
                        raise ValueError(f"path {path} selected an empty identity")
                    continue
                projected = _exact_scalar_string(selected) if exact_strings else scalar_string(selected)
                if projected is None:
                    raise ValueError(
                        f"path {path} selected a {type(selected).__name__}; fields project only JSON scalars"
                    )
                if projected in seen:
                    continue
                seen.add(projected)
                values.append(projected)
        return values

    def __getitem__(self, name: str) -> FieldPaths:
        return self._fields[name]

    def __iter__(self) -> Iterator[str]:
        return iter(self._fields)

    def __len__(self) -> int:
        return len(self._fields)


class FieldSchema(Mapping[str, FieldDefinition]):
    """A base's open semantic dictionary."""

    def __init__(self, definitions: Mapping[str, FieldDefinition] | None = None) -> None:
        self._definitions = MappingProxyType(dict(definitions or {}))

    def names(self) -> list[str]:
        """Return semantic names in deterministic order."""
        return sorted(self._definitions)

    def weight(self, name: str) -> int:
        """Return a configured lexical weight or the stable field-name default."""
        definition = self._definitions.get(name)
        if definition is not None and definition.weight > 0:
            return definition.weight
        if name == FIELD_ID:
            return DEFAULT_ID_FIELD_WEIGHT
        if name == FIELD_TITLE:
            return DEFAULT_TITLE_FIELD_WEIGHT
        return DEFAULT_FIELD_WEIGHT

    def select(self, fields: FieldMap) -> FieldSchema:
        """Copy the semantic definitions used by one source."""
        return FieldSchema({name: self._definitions[name] for name in fields.names()})

    def __getitem__(self, name: str) -> FieldDefinition:
        return self._definitions[name]

    def __iter__(self) -> Iterator[str]:
        return iter(self._definitions)

    def __len__(self) -> int:
        return len(self._definitions)


def scalar_string(value: object) -> str | None:
    """Render a decoded JSON scalar and refuse objects, arrays, and absence."""
    if isinstance(value, str):
        trimmed = value.strip()
        return trimmed or None
    if isinstance(value, bool):
        return "true" if value else "false"
    if isinstance(value, JsonNumber):
        return value.raw
    if isinstance(value, float):
        return format_float_go(value, fixed=True)
    return None


def _exact_scalar_string(value: object) -> str | None:
    return value if isinstance(value, str) else scalar_string(value)


def is_well_known_field(name: str) -> bool:
    """Return whether FKF gives this suggested name built-in semantics."""
    return name in _WELL_KNOWN_FIELDS


def validate_field_map(fields: FieldMap, *, event: bool) -> None:
    """Validate the open projection map's small structural contract."""
    if len(fields) > MAX_FIELDS:
        raise ValueError(f"declares {len(fields)} fields; expected at most {MAX_FIELDS}")
    for name in fields.names():
        paths = fields.paths(name)
        if _FIELD_NAME_PATTERN.fullmatch(name) is None:
            raise ValueError(
                f"field name {name!r} must start with a lowercase letter and contain only "
                "lowercase letters, digits, hyphens, or underscores"
            )
        if _byte_length(name) > MAX_FIELD_NAME_LENGTH:
            raise ValueError(
                f"field name {name!r} is {_byte_length(name)} bytes; expected at most {MAX_FIELD_NAME_LENGTH}"
            )
        if not paths:
            raise ValueError(f"fields.{name} must declare at least one path")
        if len(paths) > MAX_PATHS_PER_FIELD:
            raise ValueError(f"fields.{name} declares {len(paths)} paths; expected at most {MAX_PATHS_PER_FIELD}")
        for index, path in enumerate(paths):
            if path.is_zero:
                raise ValueError(f"fields.{name}[{index}] is empty")
    if fields.path(FIELD_ID).is_zero:
        raise ValueError("fields.id is required: a record with no declared identity cannot be addressed by a URI")
    if event and fields.path(FIELD_TIME).is_zero:
        raise ValueError("fields.time is required for an events source: a dated document needs a per-record timestamp")


def validate_field_schema(schema: FieldSchema) -> None:
    """Validate the semantic contract shared by config, evidence, graph, and retrieval."""
    if not schema:
        raise ValueError("schema is required and must declare at least id")
    if len(schema) > MAX_FIELDS:
        raise ValueError(f"schema declares {len(schema)} fields; expected at most {MAX_FIELDS}")
    for name in schema.names():
        definition = schema[name]
        if _FIELD_NAME_PATTERN.fullmatch(name) is None:
            raise ValueError(
                f"schema field name {name!r} must start with a lowercase letter and contain only "
                "lowercase letters, digits, hyphens, or underscores"
            )
        if _byte_length(name) > MAX_FIELD_NAME_LENGTH:
            raise ValueError(
                f"schema field name {name!r} is {_byte_length(name)} bytes; expected at most {MAX_FIELD_NAME_LENGTH}"
            )
        if not definition.description.strip():
            raise ValueError(f"schema.{name}.description is required")
        if _byte_length(definition.description) > MAX_FIELD_DESCRIPTION_LENGTH:
            raise ValueError(
                f"schema.{name}.description is {_byte_length(definition.description)} bytes; "
                f"expected at most {MAX_FIELD_DESCRIPTION_LENGTH}"
            )
        if len(definition.examples) > MAX_FIELD_EXAMPLES:
            raise ValueError(
                f"schema.{name}.examples has {len(definition.examples)} entries; expected at most {MAX_FIELD_EXAMPLES}"
            )
        if definition.weight < 0 or definition.weight > MAX_FIELD_WEIGHT:
            raise ValueError(
                f"schema.{name}.weight is {definition.weight}; expected 1..{MAX_FIELD_WEIGHT} when declared"
            )
        if not isinstance(definition.cardinality, Cardinality):
            raise ValueError(f"schema.{name}.cardinality {definition.cardinality!r} must be one, optional, or many")
        for index, example in enumerate(definition.examples):
            if _byte_length(example) > MAX_FIELD_EXAMPLE_LENGTH:
                raise ValueError(
                    f"schema.{name}.examples[{index}] is {_byte_length(example)} bytes; "
                    f"expected at most {MAX_FIELD_EXAMPLE_LENGTH}"
                )
            if definition.relation:
                try:
                    validate_relation_value(example)
                except ValueError as error:
                    raise ValueError(f"schema.{name}.examples[{index}] {example!r}: {error}") from error

    identity = schema.get(FIELD_ID)
    if identity is None:
        raise ValueError("schema.id is required")
    if identity.cardinality is not Cardinality.ONE:
        raise ValueError("schema.id.cardinality must be one")
    if identity.relation:
        raise ValueError("schema.id must not be a relation")
    event_time = schema.get(FIELD_TIME)
    if event_time is not None and event_time.relation:
        raise ValueError("schema.time must not be a relation")
    for name in (FIELD_TIME, FIELD_TITLE, FIELD_URL, FIELD_CATEGORY, FIELD_VISIBILITY):
        definition = schema.get(name)
        if definition is not None and not definition.cardinality.max_one:
            raise ValueError(f"schema.{name}.cardinality must be one or optional because fkf consumes one scalar")


def validate_entity_scheme(value: str) -> None:
    """Validate an open, lowercase, non-reserved entity namespace."""
    if _ENTITY_SCHEME_PATTERN.fullmatch(value) is None:
        raise ValueError("must start with a lowercase letter and contain only lowercase letters, digits, +, ., or -")
    if value in _RESERVED_ENTITY_SCHEMES:
        raise ValueError(f"scheme {value!r} is reserved and cannot name an entity")


def validate_entity_uri(value: str) -> None:
    """Validate a canonical entity URI, excluding files and external URLs."""
    if not value or value.strip() != value or any(char in value for char in " \t\r\n"):
        raise ValueError("must be a non-empty canonical entity URI with no whitespace")
    match = _ENTITY_REFERENCE_PATTERN.fullmatch(value)
    if match is None:
        raise ValueError("must be an entity URI of the form scheme:identity")
    validate_entity_scheme(match.group(1))
    try:
        _validate_canonical_entity_identity(match.group(2))
    except ValueError as error:
        raise ValueError(f"entity identity: {error}") from error


def validate_relation_value(value: str) -> None:
    """Validate a canonical entity, HTTPS, or published base-file URI."""
    if not value or value.strip() != value:
        raise ValueError("must be a non-empty canonical URI with no surrounding whitespace")
    if any(char in value for char in " \t\r\n"):
        raise ValueError("must be a canonical URI; whitespace must be percent-encoded")
    if value.lower().startswith("https://"):
        _validate_https_relation(value)
        return
    if "://" in _file_relation_head(value):
        raise ValueError("external URIs must use https")
    entity = _ENTITY_REFERENCE_PATTERN.fullmatch(value)
    if entity is not None:
        validate_entity_scheme(entity.group(1))
        try:
            _validate_canonical_entity_identity(entity.group(2))
        except ValueError as error:
            raise ValueError(f"entity identity: {error}") from error
        return
    _validate_file_relation(value)


def _validate_canonical_entity_identity(identity: str) -> None:
    decoded = bytearray()
    index = 0
    while index < len(identity):
        char = identity[index]
        if char in _IDENTITY_SAFE:
            decoded.append(ord(char))
            index += 1
            continue
        if (
            char != "%"
            or index + 2 >= len(identity)
            or identity[index + 1] not in _UPPER_HEX
            or identity[index + 2] not in _UPPER_HEX
        ):
            raise ValueError("must use uppercase percent escapes for bytes outside A-Z a-z 0-9 . _ : / @ + -")
        escaped = identity[index + 1 : index + 3]
        byte = int(escaped, 16)
        if chr(byte) in _IDENTITY_SAFE:
            raise ValueError(f"percent escape %{escaped} is not canonical; write {chr(byte)!r} directly")
        decoded.append(byte)
        index += 3
    try:
        text = decoded.decode("utf-8")
    except UnicodeDecodeError as error:
        raise ValueError("percent-decoded value is not valid UTF-8") from error
    if not text or text.strip() != text:
        raise ValueError("must decode to a non-empty value with no surrounding whitespace")


def _validate_https_relation(value: str) -> None:
    try:
        parsed = urlsplit(value)
    except ValueError as error:
        raise ValueError(f"must be an absolute HTTPS URI: {error}") from error
    if parsed.scheme.lower() != "https" or not parsed.netloc:
        raise ValueError("must be an absolute HTTPS URI with a host")


def _file_relation_head(value: str) -> str:
    head = value.rsplit("#", maxsplit=1)[0]
    return head.split("?", maxsplit=1)[0]


def _validate_file_relation(value: str) -> None:
    path_value = value
    has_fragment = "#" in path_value
    if has_fragment:
        path_value, fragment = path_value.rsplit("#", maxsplit=1)
        try:
            _validate_canonical_entity_identity(fragment)
        except ValueError as error:
            raise ValueError(f"file URI fragment: {error}") from error
    has_jq = "?" in path_value
    if has_jq:
        path_value, raw_query = path_value.split("?", maxsplit=1)
        _validate_file_relation_query(raw_query)
    cleaned = _clean_relative(path_value)
    if cleaned != path_value:
        raise ValueError(f"base-relative relation path is not canonical; want {cleaned!r}")
    fragment_capable, jq_capable, addressable = _relation_file_path(cleaned)
    if not addressable:
        raise ValueError("base-relative relation must name a published file, not a listing or private path")
    if has_fragment and not fragment_capable:
        raise ValueError(f"base-relative relation {cleaned!r} does not address records or Markdown headings")
    if has_jq and not jq_capable:
        raise ValueError(f"base-relative relation {cleaned!r} is not a selectable JSON document")


def _validate_file_relation_query(raw_query: str) -> None:
    if not raw_query.startswith("jq="):
        raise ValueError("the only supported file URI query is ?jq=<expression>")
    raw_expression = raw_query.removeprefix("jq=")
    if re.search(r"%(?![0-9A-Fa-f]{2})", raw_expression):
        raise ValueError("file URI jq expression has an invalid percent escape")
    expression = unquote_plus(raw_expression)
    if not expression.strip():
        raise ValueError("file URI ?jq= must name an expression")
    canonical = quote_plus(expression, safe="")
    if canonical != raw_expression:
        raise ValueError(f"file URI jq expression is not canonical; want {canonical!r}")


def _clean_relative(relative: str) -> str:
    candidate = relative.strip()
    if not candidate:
        raise ValueError("path escapes the base: path is empty")
    if "\x00" in candidate or "\\" in candidate:
        raise ValueError(f"path escapes the base: {candidate!r} contains an unsafe character")
    if posixpath.isabs(candidate):
        raise ValueError(f"path escapes the base: {candidate!r} is absolute")
    if candidate.startswith("~"):
        raise ValueError(f"path escapes the base: {candidate!r} is home-relative")
    trailing_slash = candidate.endswith("/")
    cleaned = posixpath.normpath(candidate)
    if cleaned == ".." or cleaned.startswith("../"):
        raise ValueError(f"path escapes the base: {candidate!r}")
    if cleaned == ".":
        return "."
    if trailing_slash:
        cleaned += "/"
    return cleaned


def _relation_file_path(relative: str) -> tuple[bool, bool, bool]:
    if relative in {"fkf.yaml", "graph.tsv", "graph.dst.tsv", "graph.offsets.tsv"}:
        return False, False, True
    if relative == "AGENTS.md":
        return True, False, True
    if relative in {"graph.meta.json", "graph.generation.json"}:
        return False, True, True

    parts = relative.split("/")
    if len(parts) == 3 and parts[0] == "events" and _valid_date(parts[1]) and _source_document(parts[2]):
        return True, True, True
    if len(parts) == 2 and parts[0] == "index" and _source_document(parts[1]):
        return True, True, True
    if (
        len(parts) == 4
        and parts[0] == "tasks"
        and _valid_date(parts[1])
        and _PAGE_SLUG_PATTERN.fullmatch(parts[2]) is not None
        and parts[3] == "TASKS.md"
    ):
        return True, False, True
    if len(parts) == 2 and parts[0] in {"projects", "wiki"} and parts[1].endswith(".md"):
        return (
            _PAGE_SLUG_PATTERN.fullmatch(parts[1][:-3]) is not None,
            False,
            _PAGE_SLUG_PATTERN.fullmatch(parts[1][:-3]) is not None,
        )
    return False, False, False


def _valid_date(value: str) -> bool:
    if re.fullmatch(r"[0-9]{4}-[0-9]{2}-[0-9]{2}", value) is None:
        return False
    try:
        return date.fromisoformat(value).isoformat() == value
    except ValueError:
        return False


def _source_document(value: str) -> bool:
    return value.endswith(".json") and _SOURCE_NAME_PATTERN.fullmatch(value[:-5]) is not None


def _byte_length(value: str) -> int:
    return len(value.encode("utf-8"))


__all__ = [
    "CONFIG_VERSION",
    "DEFAULT_FIELD_WEIGHT",
    "DEFAULT_ID_FIELD_WEIGHT",
    "DEFAULT_TITLE_FIELD_WEIGHT",
    "FIELD_CATEGORY",
    "FIELD_ID",
    "FIELD_TIME",
    "FIELD_TITLE",
    "FIELD_URL",
    "FIELD_VISIBILITY",
    "MAX_FIELDS",
    "MAX_FIELD_DESCRIPTION_LENGTH",
    "MAX_FIELD_EXAMPLES",
    "MAX_FIELD_EXAMPLE_LENGTH",
    "MAX_FIELD_NAME_LENGTH",
    "MAX_FIELD_PATH_BYTES",
    "MAX_FIELD_PATH_STEPS",
    "MAX_FIELD_WEIGHT",
    "MAX_PATHS_PER_FIELD",
    "Cardinality",
    "FieldDefinition",
    "FieldMap",
    "FieldPath",
    "FieldPaths",
    "FieldSchema",
    "is_well_known_field",
    "parse_field_path",
    "scalar_string",
    "validate_entity_scheme",
    "validate_entity_uri",
    "validate_field_map",
    "validate_field_schema",
    "validate_relation_value",
]
