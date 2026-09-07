"""Permanent evidence-envelope, record decoding, and civil-window contracts."""

from __future__ import annotations

from dataclasses import replace
from datetime import UTC, datetime
from pathlib import Path
from zoneinfo import ZoneInfo

import pytest

from fkf.config import OutputFormat, Source
from fkf.documents import (
    Document,
    IncompleteCollectionError,
    Record,
    UnknownSchemaError,
    Window,
    build_document,
    build_window_documents,
    day_window,
    decode_document,
    decode_fragment,
    decode_records,
    encode_document,
    encode_fragment,
    event_document_uri,
    index_document_uri,
    parse_day_in_location,
    read_document,
    verify_document,
    write_document,
)
from fkf.fields import Cardinality, FieldDefinition, FieldMap, FieldSchema, parse_field_path
from fkf.io import FileTooLargeError
from fkf.jsoncodec import JsonNumber
from fkf.store import MAX_SOURCE_DOCUMENT_BYTES, Layer


def field_map(**paths: str | list[str]) -> FieldMap:
    """Build the compact public field shape used by fixtures."""
    return FieldMap.from_json_value(paths)


def event_source(*, output: OutputFormat = OutputFormat.JSON, records: str = "") -> Source:
    """Return a validated-shape event source without loading YAML."""
    fields = field_map(id=".id", time=".time", title=".title", author=".author")
    schema = FieldSchema(
        {
            "id": FieldDefinition("Stable identity.", Cardinality.ONE),
            "time": FieldDefinition("Event time.", Cardinality.ONE),
            "title": FieldDefinition("Human label.", Cardinality.OPTIONAL),
            "author": FieldDefinition("Actor.", Cardinality.OPTIONAL, relation=True),
        }
    )
    return Source(
        name="events-source",
        layer=Layer.EVENTS,
        format=output,
        records=parse_field_path(records) if records else None,
        fields=fields,
        schema=schema,
        body=("provider", "{{id}}"),
    )


def index_source(*, output: OutputFormat = OutputFormat.JSON, records: str = "") -> Source:
    """Return a minimal index source."""
    return Source(
        name="repositories",
        layer=Layer.INDEX,
        format=output,
        records=parse_field_path(records) if records else None,
        fields=field_map(id=".id"),
        schema=FieldSchema({"id": FieldDefinition("Stable identity.", Cardinality.ONE)}),
    )


def collected_at() -> datetime:
    return datetime(2026, 5, 5, 8, tzinfo=UTC)


def test_fragment_and_document_uris_are_canonical() -> None:
    assert event_document_uri("2026-05-04", "google-emails") == "events/2026-05-04/google-emails.json"
    assert index_document_uri("github-repositories") == "index/github-repositories.json"
    assert index_document_uri("other/../repositories") == "index/repositories.json"

    for identity in ("fmind/fkf", "marc@example.test", "has space", "has#hash", "héllo", "a\nb"):
        encoded = encode_fragment(identity)
        assert not any(delimiter in encoded for delimiter in " #?")
        assert decode_fragment(encoded) == identity
    assert encode_fragment("fmind/fkf") == "fmind/fkf"
    assert encode_fragment("a\nb") == "a%0Ab"
    with pytest.raises(ValueError, match="invalid percent escape"):
        decode_fragment("bad%zz")
    with pytest.raises(ValueError, match="truncated percent escape"):
        decode_fragment("truncated%")
    with pytest.raises(ValueError, match="valid UTF-8"):
        decode_fragment("%FF")


def test_document_round_trip_is_additive_deterministic_and_lossless(tmp_path: Path) -> None:
    source = index_source()
    document = build_document(
        source,
        [{"id": "fmind/fkf", "big": JsonNumber("9007199254740993")}],
        collected_at=collected_at(),
    )
    encoded = encode_document(document)
    assert encoded.endswith(b"\n")
    assert b'"relation"' not in encoded
    assert b'"examples"' not in encoded
    assert b'"weight"' not in encoded

    additive = encoded.rstrip()[:-1] + b',"future":{"ignored":true}}\n'
    decoded = decode_document(additive, "index/repositories.json")
    assert encode_document(decoded) == encoded
    assert decoded.records[0]["big"] == JsonNumber("9007199254740993")
    assert decoded.record_uri(decoded.records[0]) == "index/repositories.json#fmind/fkf"
    assert decoded.find_record("fmind/fkf") == decoded.records[0]

    path = tmp_path / "repositories.json"
    write_document(path, decoded)
    assert path.stat().st_mode & 0o777 == 0o600
    assert encode_document(read_document(path)) == encoded


def test_document_decode_refuses_unknown_generation_layer_and_trailing_json() -> None:
    with pytest.raises(UnknownSchemaError, match="unsupported evidence envelope"):
        decode_document(b'{"fkf":99,"source":"s","records":[]}', "x.json")
    with pytest.raises(UnknownSchemaError, match="inventory"):
        decode_document(b'{"fkf":1,"source":"s","layer":"inventory","records":[]}', "x.json")

    valid = b'{"fkf":1,"source":"s","layer":"events","records":[]}'
    for suffix in (b'{"fkf":1}', b"{"):
        with pytest.raises(ValueError, match="trailing JSON"):
            decode_document(valid + suffix, "x.json")


def test_write_document_refuses_bytes_its_reader_cannot_accept(tmp_path: Path) -> None:
    document = Document(records=[{"payload": "x" * MAX_SOURCE_DOCUMENT_BYTES}])
    path = tmp_path / "oversized.json"
    with pytest.raises(FileTooLargeError):
        write_document(path, document)
    assert not path.exists()


def test_decode_records_distinguishes_json_ndjson_and_envelopes() -> None:
    source = index_source()
    records = decode_records(source, '[{"id":"a","big":9007199254740993}]')
    assert records == [{"id": "a", "big": JsonNumber("9007199254740993")}]
    with pytest.raises(IncompleteCollectionError, match=r"prints \[\]"):
        decode_records(source, "  \n")
    with pytest.raises(IncompleteCollectionError, match="array of records"):
        decode_records(source, '{"id":"a"}')
    with pytest.raises(IncompleteCollectionError, match="more than one JSON document"):
        decode_records(source, "[] []")

    ndjson = index_source(output=OutputFormat.NDJSON)
    assert decode_records(ndjson, "") == []
    assert decode_records(ndjson, '{"id":"a"}\n[{"id":"b"}]\n') == [{"id": "a"}, {"id": "b"}]

    wrapped = index_source(output=OutputFormat.NDJSON, records=".items")
    output = '{"items":[{"id":"a"}]}\n{"items":[{"id":"b"}]}\n{"next":null}\n'
    assert decode_records(wrapped, output) == [{"id": "a"}, {"id": "b"}]
    whole = index_source(records=".items")
    with pytest.raises(IncompleteCollectionError, match="selected nothing"):
        decode_records(whole, '{"next":null}')


def test_verify_document_enforces_definition_records_and_stored_windows() -> None:
    source = event_source()
    window = Window(
        date="2026-05-04",
        next="2026-05-05",
        start="2026-05-04T00:00:00Z",
        end="2026-05-05T00:00:00Z",
    )
    document = build_document(
        source,
        [{"id": "a", "time": "2026-05-04T09:00:00Z", "title": "Record", "author": "actor:alice"}],
        window=window,
        collected_at=collected_at(),
    )
    verify_document(document)

    failures = (
        (replace(document, count=2), "count"),
        (replace(document, source="other/../events-source"), "document source"),
        (replace(document, records=[document.records[0], document.records[0]], count=2), "share the id"),
        (replace(document, window_end=""), "both window_start and window_end"),
        (replace(document, window_start="2026-05-04T02:00:00+02:00"), "canonical UTC"),
        (replace(document, window_end="2026-05-04T01:00:00Z"), "civil day must span"),
        (replace(document, date="2026-06-04"), "aligned with civil date"),
        (replace(document, records=[{"id": "a", "time": "2026-05-05"}], count=1), "outside"),
        (
            replace(
                document,
                records=[{"id": "a", "time": "2026-05-04T09:00:00Z", "author": "alice"}],
                count=1,
            ),
            "canonical relation URI",
        ),
    )
    for candidate, message in failures:
        with pytest.raises(ValueError, match=message):
            verify_document(candidate)


def test_build_document_applies_the_new_title_contract_only_at_collection() -> None:
    source = event_source()
    window = day_window(parse_day_in_location("2026-05-04", ZoneInfo("UTC")))
    for title in ("", "unsafe\nheading", "invisible\u200bheading"):
        with pytest.raises(IncompleteCollectionError, match="title"):
            build_document(
                source,
                [{"id": "a", "time": "2026-05-04T09:00:00Z", "title": title}],
                window=window,
                collected_at=collected_at(),
            )

    historical = build_document(
        source,
        [{"id": "a", "time": "2026-05-04T09:00:00Z", "title": "was present"}],
        window=window,
        collected_at=collected_at(),
    )
    historical.records[0].pop("title")
    verify_document(historical)


def test_civil_windows_handle_dst_and_skipped_dates() -> None:
    ordinary = day_window(parse_day_in_location("2026-05-04", ZoneInfo("UTC")))
    assert ordinary == Window("2026-05-04", "2026-05-05", "2026-05-04T00:00:00Z", "2026-05-05T00:00:00Z")

    short = day_window(parse_day_in_location("2015-10-18", ZoneInfo("America/Sao_Paulo")))
    assert short.start == "2015-10-18T03:00:00Z"
    assert short.end == "2015-10-19T02:00:00Z"

    long = day_window(parse_day_in_location("2018-02-17", ZoneInfo("America/Sao_Paulo")))
    start = datetime.fromisoformat(long.start)
    end = datetime.fromisoformat(long.end)
    assert (end - start).total_seconds() == pytest.approx(25 * 60 * 60)

    with pytest.raises(ValueError, match="does not exist"):
        parse_day_in_location("2011-12-30", ZoneInfo("Pacific/Apia"))
    before_skip = day_window(parse_day_in_location("2011-12-29", ZoneInfo("Pacific/Apia")))
    assert before_skip.next == "2011-12-31"
    assert before_skip.end == "2011-12-30T10:00:00Z"


def test_build_window_documents_buckets_half_open_instants_and_empty_days() -> None:
    source = event_source()
    records: list[Record] = [
        {"id": "a", "time": "2026-05-04T23:59:59Z", "title": "First"},
        {"id": "b", "time": "2026-05-05T00:00:00Z", "title": "Second"},
        {"id": "c", "time": "2026-05-05", "title": "All day"},
    ]
    documents = build_window_documents(
        source,
        records,
        ["2026-05-04", "2026-05-05", "2026-05-06"],
        ZoneInfo("UTC"),
        collected_at=collected_at(),
    )
    assert [record["id"] for record in documents["2026-05-04"].records] == ["a"]
    assert [record["id"] for record in documents["2026-05-05"].records] == ["b", "c"]
    assert documents["2026-05-06"].records == []

    with pytest.raises(IncompleteCollectionError, match="outside the requested window"):
        build_window_documents(
            source,
            [{"id": "late", "time": "2026-06-01T00:00:00Z", "title": "Late"}],
            ["2026-05-04"],
            ZoneInfo("UTC"),
            collected_at=collected_at(),
        )
