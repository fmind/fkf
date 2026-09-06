"""A bounded jq-shaped selector that can reach no process or filesystem state."""

from __future__ import annotations

import math
from dataclasses import dataclass
from typing import Final

from fkf.errors import InvalidUsageError
from fkf.fields import FieldPath
from fkf.io import FileTooLargeError
from fkf.jsoncodec import JsonNumber, dumps

MAX_SELECTOR_BYTES: Final = 4096
MAX_SELECTOR_STEPS: Final = 128
MAX_SELECTOR_OUTPUT_BYTES: Final = 4 << 20


class SelectorError(InvalidUsageError):
    """A selector is malformed or asks for an unsupported operation."""


@dataclass(frozen=True, slots=True)
class Selector:
    """One pure path plus an optional terminal length operation."""

    path: FieldPath
    length: bool = False


def _outside_string_pipes(expression: str) -> list[int]:
    pipes: list[int] = []
    quoted = False
    escaped = False
    for index, character in enumerate(expression):
        if quoted:
            if escaped:
                escaped = False
            elif character == "\\":
                escaped = True
            elif character == '"':
                quoted = False
            continue
        if character == '"':
            quoted = True
        elif character == "|":
            pipes.append(index)
    return pipes


def _step_count(path: str) -> int:
    """Count path operators without treating punctuation in JSON keys as syntax."""
    count = 0
    quoted = False
    escaped = False
    index = 0
    while index < len(path):
        character = path[index]
        if quoted:
            if escaped:
                escaped = False
            elif character == "\\":
                escaped = True
            elif character == '"':
                quoted = False
            index += 1
            continue
        if character == '"':
            quoted = True
        elif character == ".":
            root_before_bracket = index == 0 and (len(path) == 1 or path[1] == "[")
            if not root_before_bracket:
                count += 1
        elif character == "[":
            count += 1
        index += 1
    return count


def parse_selector(expression: str) -> Selector:
    """Parse the complete closed selector grammar."""
    size = len(expression.encode("utf-8"))
    if size > MAX_SELECTOR_BYTES:
        raise SelectorError(f"invalid JSON selector: expression is {size} bytes; maximum is {MAX_SELECTOR_BYTES}")
    trimmed = expression.strip()
    pipes = _outside_string_pipes(trimmed)
    if len(pipes) > 1:
        raise SelectorError("invalid JSON selector: only one terminal `| length` operation is supported")
    length = False
    path_text = trimmed
    if pipes:
        position = pipes[0]
        path_text = trimmed[:position].strip()
        operation = trimmed[position + 1 :].strip()
        if operation != "length":
            raise SelectorError("invalid JSON selector: the only supported pipe operation is `length`")
        length = True
    try:
        path = FieldPath.parse(path_text)
    except ValueError as error:
        raise SelectorError(f"invalid JSON selector: {error}", cause=error) from error
    steps = _step_count(path_text)
    if steps > MAX_SELECTOR_STEPS:
        raise SelectorError(f"invalid JSON selector: path has {steps} steps; maximum is {MAX_SELECTOR_STEPS}")
    return Selector(path=path, length=length)


def _length(value: object) -> object:
    if value is None:
        return 0
    if isinstance(value, bool):
        raise SelectorError("invalid JSON selector result: length is undefined for a boolean")
    if isinstance(value, JsonNumber):
        raw = str(value)
        if raw.startswith("-"):
            return JsonNumber(raw[1:])
        return value
    if isinstance(value, int):
        return abs(value)
    if isinstance(value, float):
        if not math.isfinite(value):
            raise SelectorError("invalid JSON selector result: length is undefined for a non-finite number")
        return abs(value)
    if isinstance(value, str | list | dict):
        return len(value)
    raise SelectorError(f"invalid JSON selector result: length is undefined for {type(value).__name__}")


def apply_selector(
    expression: str,
    payload: object,
    *,
    max_output_bytes: int = MAX_SELECTOR_OUTPUT_BYTES,
) -> bytes:
    """Select and encode one bounded JSON result document."""
    if max_output_bytes <= 0:
        raise ValueError("selector output limit must be positive")
    selector = parse_selector(expression)
    values = selector.path.eval(payload)
    if selector.length:
        values = [_length(value) for value in values]
    if not values:
        return b"null"

    encoded: list[bytes] = []
    produced = 0
    for value in values:
        item = dumps(value)
        if not encoded:
            produced = len(item)
        elif len(encoded) == 1:
            produced += len(item) + 3
        else:
            produced += len(item) + 1
        if produced > max_output_bytes:
            raise FileTooLargeError(
                f"file exceeds size limit: selector produced more than {max_output_bytes} bytes; narrow the selector"
            )
        encoded.append(item)
    return encoded[0] if len(encoded) == 1 else b"[" + b",".join(encoded) + b"]"


__all__ = [
    "MAX_SELECTOR_BYTES",
    "MAX_SELECTOR_OUTPUT_BYTES",
    "MAX_SELECTOR_STEPS",
    "Selector",
    "SelectorError",
    "apply_selector",
    "parse_selector",
]
