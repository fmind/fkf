from __future__ import annotations

import io
from dataclasses import dataclass

import pytest

from fkf.output import (
    OutputFormat,
    block,
    default_format,
    encode,
    inline,
    jsonl_bytes,
    parse_format,
    register_jsonl,
    register_text,
)


@dataclass(frozen=True, slots=True)
class _Report:
    label: str
    items: tuple[int, ...]


def test_non_file_stream_defaults_to_json() -> None:
    assert default_format(io.StringIO()) is OutputFormat.JSON


def test_format_vocabulary_is_closed() -> None:
    assert parse_format("jsonl") is OutputFormat.JSONL
    with pytest.raises(ValueError, match="expected json, jsonl, or text"):
        parse_format("yaml")


def test_jsonl_registry_selects_the_natural_collection() -> None:
    register_jsonl(_Report, lambda report: report.items)
    assert jsonl_bytes(_Report("x", (1, 2))) == b"1\n2\n"


def test_unregistered_report_remains_one_jsonl_object() -> None:
    @dataclass(frozen=True, slots=True)
    class Receipt:
        value: int

    assert jsonl_bytes(Receipt(7)) == b'{"value":7}\n'


def test_text_registry_and_fallback_are_explicit() -> None:
    register_text(_Report, lambda report: f"{report.label}: {len(report.items)}")
    assert encode(_Report("safe", (1,)), OutputFormat.TEXT) == (b"safe: 1\n", True)

    @dataclass(frozen=True, slots=True)
    class Unknown:
        value: int

    data, native = encode(Unknown(2), OutputFormat.TEXT)
    assert data == b'{\n  "value": 2\n}\n'
    assert not native


def test_terminal_safety_preserves_blocks_but_flattens_inline_text() -> None:
    assert inline("one\ntwo\tthree\x1b") == "one two three "
    assert block("one\ntwo\tthree\x1b") == "one\ntwo\tthree "
    assert inline("left\u202eright") == "left right"
