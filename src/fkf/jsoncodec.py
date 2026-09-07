"""Deterministic JSON with lossless number lexemes and Go-compatible bytes."""

from __future__ import annotations

import base64
import dataclasses
import json
import math
import os
import re
from collections.abc import Iterable, Iterator, Mapping, Sequence
from decimal import Decimal
from enum import Enum
from typing import BinaryIO, Protocol, runtime_checkable

_SURROGATES = re.compile(r"[\ud800-\udfff]")
_NUMBER_PATTERN = re.compile(r"-?(?:0|[1-9][0-9]*)(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?\Z")


@dataclasses.dataclass(frozen=True, slots=True)
class JsonNumber:
    """A validated JSON number that keeps the exact source lexeme."""

    raw: str

    def __post_init__(self) -> None:
        if _NUMBER_PATTERN.fullmatch(self.raw) is None:
            raise ValueError(f"invalid JSON number {self.raw!r}")

    def __str__(self) -> str:
        return self.raw


type JsonScalar = bool | str | int | float | JsonNumber | None
type JsonValue = JsonScalar | list[JsonValue] | dict[str, JsonValue]


@runtime_checkable
class _PydanticModel(Protocol):
    def model_dump(self, *, mode: str, by_alias: bool) -> object: ...


@runtime_checkable
class _JsonValueProvider(Protocol):
    def __json_value__(self) -> object: ...


@dataclasses.dataclass(frozen=True, slots=True)
class _OrderedObject:
    entries: tuple[tuple[str, object], ...]


def _reject_constant(value: str) -> JsonNumber:
    raise ValueError(f"invalid JSON constant {value!r}")


def loads(data: str | bytes | bytearray | memoryview) -> JsonValue:
    """Decode exactly one JSON document while retaining every number lexeme."""
    if isinstance(data, memoryview):
        data = data.tobytes()
    try:
        value = json.loads(
            data,
            parse_int=JsonNumber,
            parse_float=JsonNumber,
            parse_constant=_reject_constant,
        )
    except (json.JSONDecodeError, UnicodeDecodeError) as error:
        raise ValueError(f"invalid JSON: {error}") from error
    return value


def load(stream: BinaryIO) -> JsonValue:
    """Decode exactly one JSON document from a binary stream."""
    return loads(stream.read())


def iter_ndjson(data: str | bytes | bytearray | memoryview) -> Iterator[JsonValue]:
    """Decode non-blank NDJSON lines and name the physical line on failure."""
    if isinstance(data, str):
        lines: Iterable[str | bytes] = data.split("\n")
    else:
        raw = data.tobytes() if isinstance(data, memoryview) else bytes(data)
        lines = raw.split(b"\n")
    for line_number, line in enumerate(lines, start=1):
        if not line.strip():
            continue
        try:
            yield loads(line)
        except ValueError as error:
            raise ValueError(f"line {line_number}: {error}") from error


def _model_value(value: object) -> object:
    if isinstance(value, _JsonValueProvider):
        # A few typed runtime models have a deliberately smaller wire shape than their
        # in-process state. Project before dataclass discovery so internals never leak.
        return _model_value(value.__json_value__())
    if dataclasses.is_dataclass(value) and not isinstance(value, type):
        entries: list[tuple[str, object]] = []
        for field in dataclasses.fields(value):
            item = getattr(value, field.name)
            tag = field.metadata.get("json", field.name)
            if not isinstance(tag, str):
                raise TypeError(f"dataclass field {field.name!r} has a non-string JSON tag")
            name, *options = tag.split(",")
            if name == "-":
                continue
            if not name:
                name = field.name
            if any(option in {"omitempty", "omitzero"} for option in options) and _is_empty(item):
                continue
            entries.append((name, item))
        return _OrderedObject(tuple(entries))
    if isinstance(value, _PydanticModel):
        # Python mode preserves bytes for the same base64 rule as Go's []byte encoder.
        dumped = value.model_dump(mode="python", by_alias=True)
        if not isinstance(dumped, Mapping):
            return dumped
        return _OrderedObject(tuple(dumped.items()))
    return value


def _is_empty(value: object) -> bool:
    if value is None or value is False:
        return True
    if isinstance(value, int | float) and value == 0:
        return True
    return isinstance(value, str | bytes | bytearray | memoryview | Sequence | Mapping) and len(value) == 0


def _quote_string(value: str) -> str:
    # Go replaces invalid UTF-8 in strings and always escapes the two JavaScript line separators.
    clean = _SURROGATES.sub("\ufffd", value)
    return json.dumps(clean, ensure_ascii=False).replace("\u2028", "\\u2028").replace("\u2029", "\\u2029")


def _strip_fixed_zeros(value: str) -> str:
    if "." not in value:
        return value
    stripped = value.rstrip("0").rstrip(".")
    return stripped if stripped not in {"", "-"} else "0"


def format_float_go(value: float, *, fixed: bool = False) -> str:
    """Render a finite float like Go's shortest ``strconv.FormatFloat`` forms."""
    if not math.isfinite(value):
        raise ValueError("JSON cannot encode a non-finite float")
    if value == 0:
        return "-0" if math.copysign(1.0, value) < 0 else "0"

    decimal = Decimal(repr(value))
    absolute = abs(value)
    if fixed or 1e-6 <= absolute < 1e21:
        return _strip_fixed_zeros(format(decimal, "f"))

    mantissa, exponent = format(decimal, "e").split("e", maxsplit=1)
    mantissa = _strip_fixed_zeros(mantissa)
    exponent_value = int(exponent)
    sign = "+" if exponent_value >= 0 else ""
    return f"{mantissa}e{sign}{exponent_value}"


def _encode(value: object, *, pretty: bool, level: int) -> str:
    if isinstance(value, JsonNumber):
        return value.raw
    # Durable records contain built-in scalars; avoid structural model inspection for each one.
    if value is not None and type(value) not in (str, int, float, bool):
        value = _model_value(value)
    if value is None:
        return "null"
    if value is True:
        return "true"
    if value is False:
        return "false"
    if isinstance(value, Mapping):
        sorted_entries: list[tuple[str, object]] = []
        for key, item in value.items():
            if not isinstance(key, str):
                raise TypeError(f"JSON object key {key!r} is not a string")
            sorted_entries.append((key, item))
        sorted_entries.sort(key=lambda entry: entry[0])
        value = _OrderedObject(tuple(sorted_entries))
    if isinstance(value, _OrderedObject):
        entries = value.entries
        if not entries:
            return "{}"
        if not pretty:
            return (
                "{"
                + ",".join(
                    f"{_quote_string(key)}:{_encode(item, pretty=False, level=level + 1)}" for key, item in entries
                )
                + "}"
            )
        indentation = "  " * (level + 1)
        closing = "  " * level
        body = ",\n".join(
            f"{indentation}{_quote_string(key)}: {_encode(item, pretty=True, level=level + 1)}" for key, item in entries
        )
        return f"{{\n{body}\n{closing}}}"
    if isinstance(value, str):
        return _quote_string(value)
    if isinstance(value, os.PathLike):
        return _quote_string(os.fspath(value))
    if isinstance(value, bytes | bytearray | memoryview):
        return _quote_string(base64.b64encode(bytes(value)).decode("ascii"))
    if isinstance(value, int):
        return str(value)
    if isinstance(value, float):
        return format_float_go(value)
    if isinstance(value, Enum):
        return _encode(value.value, pretty=pretty, level=level)
    if isinstance(value, Sequence):
        if not value:
            return "[]"
        if not pretty:
            return "[" + ",".join(_encode(item, pretty=False, level=level + 1) for item in value) + "]"
        indentation = "  " * (level + 1)
        closing = "  " * level
        body = ",\n".join(f"{indentation}{_encode(item, pretty=True, level=level + 1)}" for item in value)
        return f"[\n{body}\n{closing}]"
    raise TypeError(f"value of type {type(value).__name__} is outside the JSON boundary")


def dumps(value: object, *, indent: bool = False, newline: bool = False) -> bytes:
    """Encode a supported value as deterministic UTF-8 JSON bytes."""
    encoded = _encode(value, pretty=indent, level=0).encode("utf-8")
    return encoded + (b"\n" if newline else b"")


def dump(value: object, stream: BinaryIO, *, indent: bool = False, newline: bool = False) -> None:
    """Encode a supported value to a binary stream."""
    stream.write(dumps(value, indent=indent, newline=newline))


__all__ = [
    "JsonNumber",
    "JsonScalar",
    "JsonValue",
    "dump",
    "dumps",
    "format_float_go",
    "iter_ndjson",
    "load",
    "loads",
]
