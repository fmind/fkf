from __future__ import annotations

from dataclasses import dataclass, field
from io import BytesIO
from pathlib import Path

import pytest
from pydantic import BaseModel, ConfigDict, Field

from fkf.jsoncodec import JsonNumber, dump, dumps, iter_ndjson, load, loads


def test_loads_keeps_numeric_lexemes_losslessly() -> None:
    value = loads(b'{"big":9007199254740993,"decimal":1.2300e+04,"negative_zero":-0}')

    assert value == {
        "big": JsonNumber("9007199254740993"),
        "decimal": JsonNumber("1.2300e+04"),
        "negative_zero": JsonNumber("-0"),
    }
    assert dumps(value) == b'{"big":9007199254740993,"decimal":1.2300e+04,"negative_zero":-0}'


@pytest.mark.parametrize("raw", [b"NaN", b"Infinity", b"-Infinity", b"{}{}", b"{} trailing"])
def test_loads_refuses_non_json_constants_and_trailing_content(raw: bytes) -> None:
    with pytest.raises(ValueError, match="invalid JSON"):
        loads(raw)


@pytest.mark.parametrize("raw", ["01", "+1", ".5", "1.", "nan"])
def test_json_number_accepts_only_the_json_number_grammar(raw: str) -> None:
    with pytest.raises(ValueError, match="invalid JSON number"):
        JsonNumber(raw)


def test_dumps_is_deterministic_and_matches_go_string_and_byte_rules() -> None:
    value = {
        "z": "<script>&\u2028next\u2029",
        "a": b"\x00\xff",
        "floats": [1.0, 0.000001, 1e-7, 1e21, -0.0],
    }

    assert dumps(value) == (b'{"a":"AP8=","floats":[1,0.000001,1e-7,1e+21,-0],"z":"<script>&\\u2028next\\u2029"}')
    assert dumps(value, indent=True, newline=True).endswith(b"\n")
    assert dumps(value, indent=True).startswith(b'{\n  "a": "AP8=",')


@dataclass(frozen=True)
class _Report:
    source: str
    count: int
    detail: str | None = field(default=None, metadata={"json": "detail,omitempty"})


class _AliasedModel(BaseModel):
    model_config = ConfigDict(populate_by_name=True)

    source_name: str = Field(alias="source")
    count: int


@dataclass(frozen=True)
class _ProjectedValue:
    internal: str

    def __json_value__(self) -> str:
        return f"public:{self.internal}"


@pytest.mark.parametrize(
    "value",
    [_Report(source="github", count=2), _AliasedModel(source="jira", count=3)],
)
def test_dumps_accepts_dataclasses_and_pydantic_models(value: object) -> None:
    assert dumps(value) in {b'{"source":"github","count":2}', b'{"source":"jira","count":3}'}


def test_typed_objects_keep_declaration_order_while_dynamic_maps_sort_keys() -> None:
    assert dumps(_Report(source="github", count=2)) == b'{"source":"github","count":2}'
    assert dumps({"source": "github", "count": 2}) == b'{"count":2,"source":"github"}'


def test_typed_objects_can_define_a_public_json_projection() -> None:
    assert dumps({"value": _ProjectedValue("detail")}) == b'{"value":"public:detail"}'


def test_binary_stream_helpers_and_ndjson_share_the_lossless_decoder() -> None:
    stream = BytesIO()
    dump({"n": JsonNumber("1e3")}, stream, newline=True)
    stream.seek(0)

    assert load(stream) == {"n": JsonNumber("1e3")}
    assert list(iter_ndjson(b'\n{"n":9007199254740993}\n{"n":2}\n')) == [
        {"n": JsonNumber("9007199254740993")},
        {"n": JsonNumber("2")},
    ]


def test_ndjson_errors_name_the_physical_line() -> None:
    with pytest.raises(ValueError, match="line 3"):
        list(iter_ndjson(b'{"ok":true}\n\nnot-json\n'))


def test_ndjson_splits_only_on_lf_records() -> None:
    # Go's durable format treats vertical whitespace as JSON content, not as a
    # record delimiter. Accepting Python's wider splitlines vocabulary would
    # silently turn malformed provider output into two complete records.
    with pytest.raises(ValueError, match="line 1"):
        list(iter_ndjson(b'{"first":1}\v{"second":2}\n'))


def test_dumps_refuses_values_outside_the_json_boundary() -> None:
    with pytest.raises(TypeError):
        dumps({"set": {"not", "json"}})
    with pytest.raises(TypeError):
        dumps({1: "object keys must be strings"})
    with pytest.raises(ValueError, match="non-finite"):
        dumps(float("nan"))


def test_pathlike_values_encode_as_public_strings() -> None:
    assert dumps({"path": Path("relative/file")}) == b'{"path":"relative/file"}'


def test_unicode_strings_keep_content_and_replace_each_unpaired_surrogate() -> None:
    assert dumps("café Σİ\ud800\udfff\u2028") == '"café Σİ��\\u2028"'.encode()
