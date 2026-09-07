"""Boundary coverage for permanent evidence documents and collection decoding."""

from __future__ import annotations

from dataclasses import replace
from datetime import UTC, datetime
from zoneinfo import ZoneInfo

import pytest

from fkf.config import OutputFormat, Source
from fkf.documents import (
    Document,
    IncompleteCollectionError,
    UnknownSchemaError,
    Window,
    build_document,
    build_window_documents,
    decode_document,
    decode_records,
    encode_document,
    verify_document,
)
from fkf.fields import Cardinality, FieldDefinition, FieldMap, FieldSchema, parse_field_path
from fkf.jsoncodec import JsonNumber, JsonValue, dumps, loads
from fkf.store import Layer

COLLECTED_AT = datetime(2026, 5, 5, 8, tzinfo=UTC)
UTC_WINDOW = Window("2026-05-04", "2026-05-05", "2026-05-04T00:00:00Z", "2026-05-05T00:00:00Z")


def _field_map(**paths: str | list[str]) -> FieldMap:
    return FieldMap.from_json_value(paths)


def _index_source(*, output: OutputFormat = OutputFormat.JSON, records: str = "") -> Source:
    return Source(
        name="snapshot",
        layer=Layer.INDEX,
        format=output,
        records=parse_field_path(records) if records else None,
        fields=_field_map(id=".id"),
        schema=FieldSchema({"id": FieldDefinition("Stable identity.", Cardinality.ONE)}),
    )


def _event_source(
    *,
    fields: FieldMap | None = None,
    schema: FieldSchema | None = None,
    output: OutputFormat = OutputFormat.JSON,
    records: str = "",
) -> Source:
    return Source(
        name="journal",
        layer=Layer.EVENTS,
        format=output,
        records=parse_field_path(records) if records else None,
        fields=fields or _field_map(id=".id", time=".time", title=".title"),
        schema=schema
        or FieldSchema(
            {
                "id": FieldDefinition("Stable identity.", Cardinality.ONE),
                "time": FieldDefinition("Event time.", Cardinality.ONE),
                "title": FieldDefinition("Human label.", Cardinality.OPTIONAL),
            }
        ),
    )


def _index_document() -> Document:
    source = _index_source()
    return build_document(source, [{"id": "fmind/fkf"}], collected_at=COLLECTED_AT)


def _event_document() -> Document:
    return build_document(
        _event_source(),
        [{"id": "a", "time": "2026-05-04T09:00:00Z", "title": "Record"}],
        window=UTC_WINDOW,
        collected_at=COLLECTED_AT,
    )


@pytest.mark.parametrize(
    ("payload", "message"),
    [
        (b"not-json", "invalid JSON"),
        (b"[]", "document must be a JSON object"),
        (b'{"fkf":1.0,"layer":"index"}', "field fkf must be an integer"),
        (b'{"fkf":1,"layer":"index","source":7}', "field source must be a string"),
        (b'{"fkf":1,"layer":"index","schema":[]}', "field schema must be an object"),
        (b'{"fkf":1,"layer":"index","schema":{"id":[]}}', "schema.id must be an object"),
        (
            b'{"fkf":1,"layer":"index","schema":{"id":{"description":1,"cardinality":"one"}}}',
            "schema.id.description must be a string",
        ),
        (
            b'{"fkf":1,"layer":"index","schema":{"id":{"description":"id","cardinality":1}}}',
            "schema.id.cardinality must be a string",
        ),
        (
            b'{"fkf":1,"layer":"index","schema":{"id":{"description":"id","cardinality":"one","relation":0}}}',
            "schema.id.relation must be a boolean",
        ),
        (
            b'{"fkf":1,"layer":"index","schema":{"id":{"description":"id","cardinality":"one","examples":{}}}}',
            "schema.id.examples must be an array of strings",
        ),
        (
            b'{"fkf":1,"layer":"index","schema":{"id":{"description":"id","cardinality":"one","examples":[1]}}}',
            "schema.id.examples must be an array of strings",
        ),
        (
            b'{"fkf":1,"layer":"index","schema":{"id":{"description":"id","cardinality":"one","weight":0.5}}}',
            "schema.id.weight must be an integer",
        ),
        (b'{"fkf":1,"layer":"index","fields":[]}', "fields must be an object"),
        (b'{"fkf":1,"layer":"index","body":0}', "field body must be a boolean"),
        (b'{"fkf":1,"layer":"index","count":true}', "field count must be an integer"),
        (b'{"fkf":1,"layer":"index","records":{}}', "field records must be an array"),
        (b'{"fkf":1,"layer":"index","records":[[]]}', "record 0 must be a JSON object"),
    ],
)
def test_decode_document_rejects_corrupt_envelope_types(payload: bytes, message: str) -> None:
    with pytest.raises(ValueError, match=message) as caught:
        decode_document(payload, "broken.json")
    assert str(caught.value).startswith("decode broken.json:")


def test_decode_document_preserves_lossless_numbers_defaults_and_additive_fields() -> None:
    raw = b"""{
      "fkf": 1,
      "source": "snapshot",
      "layer": "index",
      "collected_at": "2026-05-05T08:00:00Z",
      "schema": {"id": {"description": "Stable identity.", "cardinality": "one"}},
      "fields": {"id": ".id"},
      "count": 1,
      "records": [{"id": "9007199254740993123", "number": 9007199254740993123}],
      "future": {"ignored": true}
    }"""

    document = decode_document(memoryview(raw), "index/snapshot.json")

    assert document.body is False
    assert document.date == ""
    assert document.records[0]["number"] == JsonNumber("9007199254740993123")
    assert document.record_uri(document.records[0]) == "index/snapshot.json#9007199254740993123"
    assert document.record_uri({"other": "missing id"}) is None
    assert document.find_record("absent") is None
    encoded = loads(encode_document(document))
    projected = loads(dumps(document))
    assert isinstance(encoded, dict)
    assert isinstance(projected, dict)
    assert encoded["records"] == document.records
    assert projected["records"] == document.records


@pytest.mark.parametrize(
    ("candidate", "error_type", "message"),
    [
        (replace(_index_document(), fkf=2), UnknownSchemaError, "unsupported evidence envelope"),
        (replace(_index_document(), layer=Layer.WIKI), UnknownSchemaError, "unsupported layer"),
        (replace(_index_document(), count=2), ValueError, "count 2 does not match"),
        (replace(_index_document(), source=" "), ValueError, "declares no source"),
        (replace(_index_document(), source="other/../snapshot"), ValueError, "document source"),
        (replace(_index_document(), collected_at="yesterday"), ValueError, "collected_at"),
        (replace(_index_document(), fields=FieldMap()), ValueError, "document field map"),
        (replace(_index_document(), schema=FieldSchema()), ValueError, "schema is required"),
        (
            replace(
                _index_document(),
                schema=FieldSchema(
                    {
                        "id": FieldDefinition("Stable identity.", Cardinality.ONE),
                        "other": FieldDefinition("Other.", Cardinality.OPTIONAL),
                    }
                ),
                fields=_field_map(id=".id", title=".title"),
            ),
            ValueError,
            "field title is not declared",
        ),
        (replace(_index_document(), records=[{}]), ValueError, "field id projects 0 values"),
        (
            replace(_index_document(), records=[{"id": "same"}, {"id": "same"}], count=2),
            ValueError,
            "share the id",
        ),
        (replace(_index_document(), date="2026-05-04"), ValueError, "index document declares date"),
        (
            replace(_index_document(), window_start=UTC_WINDOW.start, window_end=UTC_WINDOW.end),
            ValueError,
            "index document declares an event collection window",
        ),
        (replace(_event_document(), records=[{"id": "a"}]), ValueError, "field time projects 0 values"),
        (
            replace(_event_document(), records=[{"id": "a", "time": "last tuesday"}]),
            ValueError,
            "record 0",
        ),
        (
            replace(_event_document(), records=[{"id": "a", "time": UTC_WINDOW.end}]),
            ValueError,
            "outside the requested window",
        ),
    ],
)
def test_verify_document_fails_closed_on_stored_invariants(
    candidate: Document, error_type: type[Exception], message: str
) -> None:
    with pytest.raises(error_type, match=message):
        verify_document(candidate, zone=ZoneInfo("UTC"))


@pytest.mark.parametrize(
    ("start", "end", "message"),
    [
        ("not-a-time", UTC_WINDOW.end, "window_start"),
        (UTC_WINDOW.start, "not-a-time", "window_end"),
        (UTC_WINDOW.end, UTC_WINDOW.start, "empty or reversed"),
        (UTC_WINDOW.start, "2026-05-04T01:00:00Z", "civil day must span"),
        ("2026-06-04T00:00:00Z", "2026-06-05T00:00:00Z", "not aligned"),
    ],
)
def test_verify_document_rejects_corrupt_stored_windows(start: str, end: str, message: str) -> None:
    with pytest.raises(ValueError, match=message):
        verify_document(replace(_event_document(), window_start=start, window_end=end))


def test_build_document_enforces_collection_cardinality_relations_and_windows() -> None:
    fields = _field_map(id=".ids[]", time=".times[]", title=".titles[]", author=".authors[]")
    schema = FieldSchema(
        {
            "id": FieldDefinition("Stable identity.", Cardinality.ONE),
            "time": FieldDefinition("Event time.", Cardinality.ONE),
            "title": FieldDefinition("Human label.", Cardinality.OPTIONAL),
            "author": FieldDefinition("Canonical authors.", Cardinality.MANY, relation=True),
        }
    )
    source = _event_source(fields=fields, schema=schema)
    cases: tuple[tuple[dict[str, JsonValue], str], ...] = (
        (
            {"ids": ["a", "b"], "times": ["2026-05-04T09:00:00Z"], "titles": ["Record"]},
            "field id projects 2 values",
        ),
        (
            {"ids": ["a"], "times": ["2026-05-04T09:00:00Z"], "titles": ["one", "two"]},
            "field title projects 2 values",
        ),
        (
            {
                "ids": ["a"],
                "times": ["2026-05-04T09:00:00Z"],
                "titles": ["Record"],
                "authors": ["actor:alice", "not-a-uri"],
            },
            "not a canonical relation URI",
        ),
    )
    for record, message in cases:
        with pytest.raises(IncompleteCollectionError, match=message):
            build_document(source, [record], window=UTC_WINDOW, collected_at=COLLECTED_AT)

    with pytest.raises(IncompleteCollectionError, match="events source requires a collection window"):
        build_document(_event_source(), [], collected_at=COLLECTED_AT)
    with pytest.raises(IncompleteCollectionError, match="index source cannot declare an event collection window"):
        build_document(_index_source(), [], window=UTC_WINDOW, collected_at=COLLECTED_AT)
    with pytest.raises(IncompleteCollectionError, match="explicit timezone"):
        build_document(_index_source(), [], collected_at=datetime(2026, 5, 5, 8))


@pytest.mark.parametrize(
    ("payload", "message"),
    [
        ("[null]", "got null"),
        ("[true]", "got boolean"),
        ("[1]", "got number"),
        ('["record"]', "got str"),
        ("[[]]", "got array"),
        ("null", "got null"),
    ],
)
def test_decode_records_names_every_invalid_json_shape(payload: str, message: str) -> None:
    with pytest.raises(IncompleteCollectionError, match=message):
        decode_records(_index_source(), payload)


def test_decode_records_reports_ndjson_line_and_selected_scalar() -> None:
    ndjson = _index_source(output=OutputFormat.NDJSON)
    with pytest.raises(IncompleteCollectionError, match="line 3: output is not valid JSON"):
        decode_records(ndjson, '{"id":"a"}\n\nnot-json\n')

    selected = _index_source(output=OutputFormat.NDJSON, records=".items")
    with pytest.raises(IncompleteCollectionError, match="selected a number"):
        decode_records(selected, '{"items":1}\n')


def test_build_window_documents_refuses_ambiguous_or_incomplete_ranges() -> None:
    source = _event_source()
    cases: tuple[tuple[Source, list[dict[str, JsonValue]], list[str], str], ...] = (
        (_index_source(), [], ["2026-05-04"], "requires an events source"),
        (source, [], [], "requires at least one date"),
        (source, [], ["2026-05-04", "2026-05-06"], "ascending and contiguous"),
        (source, [{"id": "a", "title": "Missing time"}], ["2026-05-04"], "has no value"),
        (
            source,
            [{"id": "a", "time": "last tuesday", "title": "Bad time"}],
            ["2026-05-04"],
            "record 0",
        ),
        (
            source,
            [{"id": "a", "time": "2026-05-05", "title": "Wrong civil date"}],
            ["2026-05-04"],
            "falls outside the requested window",
        ),
    )
    for candidate_source, records, dates, message in cases:
        with pytest.raises(IncompleteCollectionError, match=message):
            build_window_documents(candidate_source, records, dates, ZoneInfo("UTC"), collected_at=COLLECTED_AT)

    fields = _field_map(id=".id", time=[".created", ".updated"], title=".title")
    schema = FieldSchema(
        {
            "id": FieldDefinition("Stable identity.", Cardinality.ONE),
            "time": FieldDefinition("Event time.", Cardinality.ONE),
            "title": FieldDefinition("Human label.", Cardinality.OPTIONAL),
        }
    )
    with pytest.raises(IncompleteCollectionError, match="field time projects 2 values"):
        build_window_documents(
            _event_source(fields=fields, schema=schema),
            [
                {
                    "id": "a",
                    "created": "2026-05-04T09:00:00Z",
                    "updated": "2026-05-04T10:00:00Z",
                    "title": "Edited",
                }
            ],
            ["2026-05-04"],
            ZoneInfo("UTC"),
            collected_at=COLLECTED_AT,
        )
