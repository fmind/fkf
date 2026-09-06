"""Closed jq-shaped JSON selector contracts."""

from __future__ import annotations

import pytest

from fkf.io import FileTooLargeError
from fkf.jsoncodec import JsonNumber, loads
from fkf.selector import MAX_SELECTOR_BYTES, SelectorError, apply_selector, parse_selector


@pytest.mark.parametrize(
    ("expression", "payload", "expected"),
    [
        (".", {"value": 1}, b'{"value":1}'),
        (".records[].id", {"records": [{"id": "a"}, {"id": "b"}]}, b'["a","b"]'),
        (".records[0].id", {"records": [{"id": "a"}]}, b'"a"'),
        (".records[-1].id", {"records": [{"id": "a"}, {"id": "b"}]}, b'"b"'),
        ('."odd key"', {"odd key": "value"}, b'"value"'),
        (r'."odd\"key"', {'odd"key': "value"}, b'"value"'),
        (".records | length", {"records": [1, 2, 3]}, b"3"),
        (".records[] | length", {"records": ["é", [1, 2], {"a": 1}]}, b"[1,2,1]"),
        (".missing", {"secret": "never echoed"}, b"null"),
        (".big", loads(b'{"big":9007199254740993}'), b"9007199254740993"),
    ],
)
def test_selector_is_total_deterministic_and_lossless(expression: str, payload: object, expected: bytes) -> None:
    assert apply_selector(expression, payload) == expected


def test_object_iteration_is_sorted() -> None:
    assert apply_selector(".labels[]", {"labels": {"z": "zulu", "a": "alpha"}}) == b'["alpha","zulu"]'


@pytest.mark.parametrize(
    "expression",
    [
        "$ENV",
        "env",
        "input",
        "inputs",
        'include "x"',
        'import "x" as x',
        "modulemeta",
        "range(3)",
        "range(50000000)",
        "halt",
        "halt_error",
        ".,0",
        ".items[0:2]",
        ".title|tonumber",
        ".a | length | length",
    ],
)
def test_selector_rejects_every_expression_feature_outside_path_and_length(expression: str) -> None:
    with pytest.raises(SelectorError, match="invalid JSON selector"):
        parse_selector(expression)


def test_selector_accepts_halt_error_as_data_key() -> None:
    assert apply_selector('."halt_error"', {"halt_error": "data"}) == b'"data"'


def test_selector_length_handles_null_and_numbers_without_accepting_bool() -> None:
    assert apply_selector(".|length", None) == b"0"
    assert apply_selector(".value|length", {"value": JsonNumber("-12.50")}) == b"12.50"
    with pytest.raises(SelectorError, match="boolean") as caught:
        apply_selector(".|length", True)
    assert "True" not in str(caught.value)


def test_selector_has_bounded_expression_and_output_bytes() -> None:
    with pytest.raises(SelectorError, match=str(MAX_SELECTOR_BYTES)):
        parse_selector("." + "a" * MAX_SELECTOR_BYTES)

    limit = 128
    exact = ["a" * (limit - 6), 0]
    assert len(apply_selector(".[]", exact, max_output_bytes=limit)) == limit
    with pytest.raises(FileTooLargeError, match="narrow the selector"):
        apply_selector(".[]", ["a" * (limit - 5), 0], max_output_bytes=limit)


def test_selector_step_count_is_bounded() -> None:
    with pytest.raises(SelectorError, match="128"):
        parse_selector("." + ".".join("a" for _ in range(129)))
