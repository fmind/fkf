"""Deterministic command output with explicit JSONL and text registries."""

from __future__ import annotations

import io
import os
from collections.abc import Callable, Iterable
from enum import StrEnum
from typing import BinaryIO, TextIO, cast

from fkf.jsoncodec import dumps
from fkf.markdown import find_invisible


class OutputFormat(StrEnum):
    JSON = "json"
    JSONL = "jsonl"
    TEXT = "text"


type _JSONLSelector = Callable[[object], Iterable[object]]
type _TextRenderer = Callable[[object], str]

_JSONL_REGISTRY: dict[type[object], _JSONLSelector] = {}
_TEXT_REGISTRY: dict[type[object], _TextRenderer] = {}


def register_jsonl[T](value_type: type[T], selector: Callable[[T], Iterable[object]]) -> None:
    """Declare the natural JSONL collection for one public result type."""

    def erased(value: object) -> Iterable[object]:
        return selector(cast("T", value))

    _JSONL_REGISTRY[value_type] = erased


def register_text[T](value_type: type[T], renderer: Callable[[T], str]) -> None:
    """Declare the complete human renderer for one public result type."""

    def erased(value: object) -> str:
        return renderer(cast("T", value))

    _TEXT_REGISTRY[value_type] = erased


def default_format(stream: TextIO | BinaryIO) -> OutputFormat:
    """Choose text only for a real character device; pipes receive JSON."""
    try:
        descriptor = stream.fileno()
        return OutputFormat.TEXT if os.isatty(descriptor) else OutputFormat.JSON
    except AttributeError, io.UnsupportedOperation, OSError, ValueError:
        return OutputFormat.JSON


def parse_format(value: str) -> OutputFormat:
    try:
        return OutputFormat(value)
    except ValueError as error:
        raise ValueError(f"invalid format {value!r}; expected json, jsonl, or text") from error


def json_bytes(value: object) -> bytes:
    """Encode one indented JSON result and its required final newline."""
    return dumps(value, indent=True, newline=True)


def jsonl_bytes(value: object) -> bytes:
    """Encode the explicitly registered natural collection, or one object."""
    selector = _JSONL_REGISTRY.get(type(value))
    if selector is None:
        items: Iterable[object] = value if isinstance(value, list | tuple) else (value,)
    else:
        items = selector(value)
    return b"".join(dumps(item, newline=True) for item in items)


def _is_terminal_active(char: str) -> bool:
    codepoint = ord(char)
    if (codepoint < 0x20 and char not in "\n\t") or codepoint == 0x7F or 0x80 <= codepoint <= 0x9F:
        return True
    return find_invisible(char) is not None


def inline(value: str) -> str:
    """Flatten untrusted free text onto one terminal-safe line."""
    return "".join(" " if char in "\n\t" or _is_terminal_active(char) else char for char in value)


def block(value: str) -> str:
    """Preserve line layout while neutralizing terminal controls and bidi text."""
    return "".join(" " if _is_terminal_active(char) else char for char in value)


def text_bytes(value: object) -> tuple[bytes, bool]:
    """Render a registered result safely, naming whether a renderer exists."""
    renderer = _TEXT_REGISTRY.get(type(value))
    if renderer is None:
        return b"", False
    rendered = block(renderer(value))
    if not rendered.endswith("\n"):
        rendered += "\n"
    return rendered.encode("utf-8"), True


def encode(value: object, output_format: OutputFormat) -> tuple[bytes, bool]:
    """Return encoded bytes and whether requested text had a native renderer."""
    if output_format is OutputFormat.JSONL:
        return jsonl_bytes(value), True
    if output_format is OutputFormat.TEXT:
        rendered, exists = text_bytes(value)
        return (rendered, True) if exists else (json_bytes(value), False)
    return json_bytes(value), True


__all__ = [
    "OutputFormat",
    "block",
    "default_format",
    "encode",
    "inline",
    "json_bytes",
    "jsonl_bytes",
    "parse_format",
    "register_jsonl",
    "register_text",
    "text_bytes",
]
