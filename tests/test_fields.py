from __future__ import annotations

import pytest

from fkf.fields import (
    DEFAULT_FIELD_WEIGHT,
    DEFAULT_ID_FIELD_WEIGHT,
    DEFAULT_TITLE_FIELD_WEIGHT,
    Cardinality,
    FieldDefinition,
    FieldMap,
    FieldPath,
    FieldPaths,
    FieldSchema,
    is_well_known_field,
    scalar_string,
    validate_entity_scheme,
    validate_entity_uri,
    validate_field_map,
    validate_field_schema,
    validate_relation_value,
)
from fkf.jsoncodec import JsonNumber, dumps, loads


@pytest.mark.parametrize(
    "raw",
    [
        ".id",
        ".a.b",
        ".items[0]",
        ".items[]",
        ".[0]",
        ".[]",
        '."odd key"',
        '.a."odd key".b',
        ".items[-1]",
        ".",
    ],
)
def test_field_path_accepts_only_the_addressing_subset(raw: str) -> None:
    assert str(FieldPath.parse(raw)) == raw


@pytest.mark.parametrize(
    ("raw", "document", "want"),
    [
        (r'."odd\"key"', {'odd"key': "quoted"}, "quoted"),
        (r'."snowman \u2603"', {"snowman ☃": "unicode"}, "unicode"),
        (r'."slash\\key"', {r"slash\key": "escaped"}, "escaped"),
    ],
)
def test_quoted_field_keys_use_json_string_escapes(raw: str, document: dict[str, str], want: str) -> None:
    path = FieldPath.parse(raw)

    assert str(path) == raw
    assert path.eval_string(document) == want


@pytest.mark.parametrize(
    "raw",
    [
        "id",
        ".a | .b",
        '.a[] | select(.x=="y")',
        ".a[1:2]",
        ".a..b",
        ".a.",
        "..",
        ".a[",
        '."unclosed',
        ".a[x]",
        "",
        "   ",
        ".a+.b",
    ],
)
def test_field_path_refuses_an_expression_language(raw: str) -> None:
    with pytest.raises(ValueError, match="field path"):
        FieldPath.parse(raw)


@pytest.mark.parametrize("raw", [r'."bad\q"', '."raw\ncontrol"', '."key"trailing'])
def test_quoted_field_keys_refuse_invalid_json_and_trailing_syntax(raw: str) -> None:
    with pytest.raises(ValueError, match="field path"):
        FieldPath.parse(raw)


def test_field_path_has_a_small_parse_bound() -> None:
    with pytest.raises(ValueError, match="4096 bytes"):
        FieldPath.parse("." + "a" * 4096)
    with pytest.raises(ValueError, match="128 steps"):
        FieldPath.parse(".a" * 129)


def test_field_path_evaluates_arrays_objects_nulls_and_lossless_numbers() -> None:
    document = loads(
        b'{"number":412,"to":[{"value":"a@x.test"},{"value":"b@x.test"}],'
        b'"labels":{"z":"zulu","a":"alpha","m":"mike"},"empty":null,"big":9007199254740993}'
    )

    assert FieldPath.parse(".number").eval_strings(document) == ["412"]
    assert FieldPath.parse(".to[].value").eval_strings(document) == ["a@x.test", "b@x.test"]
    assert FieldPath.parse(".to[-1].value").eval_string(document) == "b@x.test"
    assert FieldPath.parse(".labels[]").eval_strings(document) == ["alpha", "mike", "zulu"]
    assert FieldPath.parse(".empty").eval(document) == []
    assert FieldPath.parse(".missing.deeper").eval(document) == []
    assert FieldPath.parse(".big").eval_string(document) == "9007199254740993"
    assert FieldPath.parse(".[0]").eval_string(["root-first"]) == "root-first"
    assert FieldPath.parse(".[]").eval_strings(["a", "b"]) == ["a", "b"]


def test_field_path_eval_string_requires_exactly_one_scalar() -> None:
    path = FieldPath.parse(".to[]")

    assert path.eval_string({"to": ["first", "second"]}) is None
    assert path.eval_string({"to": [{"a": JsonNumber("1")}]}) is None
    assert path.eval_string({"to": ["only"]}) == "only"


def test_field_paths_and_map_keep_the_compact_scalar_or_list_shape() -> None:
    fields = FieldMap.from_json_value({"id": ".id", "project": [".project.key", ".fallback_project"]})

    assert fields.eval_string("project", {"fallback_project": "knowledge"}) == "knowledge"
    assert fields.to_json_value() == {"id": ".id", "project": [".project.key", ".fallback_project"]}
    assert dumps(fields.to_json_value()) == b'{"id":".id","project":[".project.key",".fallback_project"]}'
    assert FieldMap.from_json_value(loads(dumps(fields.to_json_value()))).to_json_value() == fields.to_json_value()


def test_field_map_unions_paths_in_order_and_deduplicates() -> None:
    fields = FieldMap(
        {
            "topic": FieldPaths((FieldPath.parse(".first[]"), FieldPath.parse(".second[]"))),
        }
    )

    assert fields.eval_strings("topic", {"first": ["a", "b"], "second": ["b", "c"]}) == ["a", "b", "c"]
    assert fields.eval_string("topic", {"first": ["a"], "second": ["b"]}) is None


def test_declared_field_distinguishes_empty_identity_and_exact_relation_text() -> None:
    fields = FieldMap.from_json_value({"id": ".id", "related": ".related[]"})

    with pytest.raises(ValueError, match="empty identity"):
        fields.eval_declared_field("id", {"id": "  "}, FieldDefinition("ID", Cardinality.ONE))
    assert fields.eval_declared_field(
        "related",
        {"related": [" repo:one ", "repo:one"]},
        FieldDefinition("Related", Cardinality.MANY, relation=True),
    ) == [" repo:one ", "repo:one"]


@pytest.mark.parametrize(
    ("value", "want"),
    [
        ("  text  ", "text"),
        (True, "true"),
        (412.0, "412"),
        (1.5, "1.5"),
        (JsonNumber("9007199254740993"), "9007199254740993"),
    ],
)
def test_scalar_string_matches_decoded_json_scalars(value: object, want: str) -> None:
    assert scalar_string(value) == want


@pytest.mark.parametrize("value", ["", "   ", None, {}, [], 412])
def test_scalar_string_refuses_absent_non_scalars_and_non_decoded_ints(value: object) -> None:
    assert scalar_string(value) is None


def test_cardinality_contract() -> None:
    assert Cardinality.ONE.allows(1)
    assert not Cardinality.ONE.allows(0)
    assert Cardinality.OPTIONAL.allows(0)
    assert Cardinality.OPTIONAL.allows(1)
    assert not Cardinality.OPTIONAL.allows(2)
    assert Cardinality.MANY.allows(0)
    assert Cardinality.MANY.allows(100)
    assert Cardinality.ONE.max_one
    assert Cardinality.OPTIONAL.max_one
    assert not Cardinality.MANY.max_one


def test_field_map_validation_requires_only_structural_fields() -> None:
    valid = FieldMap.from_json_value({"id": [".legacy_id", ".id"], "time": ".updated", "topic": ".topic"})
    validate_field_map(valid, event=True)

    with pytest.raises(ValueError, match=r"fields\.id is required"):
        validate_field_map(FieldMap.from_json_value({"time": ".updated"}), event=True)
    with pytest.raises(ValueError, match=r"fields\.time is required"):
        validate_field_map(FieldMap.from_json_value({"id": ".id"}), event=True)
    with pytest.raises(ValueError, match="field name"):
        validate_field_map(FieldMap.from_json_value({"id": ".id", "Project.Key": ".topic"}), event=False)
    with pytest.raises(ValueError, match="at least one path"):
        validate_field_map(
            FieldMap({"id": FieldPaths((FieldPath.parse(".id"),)), "topic": FieldPaths(())}), event=False
        )


def test_field_schema_validation_selection_and_weights() -> None:
    schema = FieldSchema(
        {
            "id": FieldDefinition("Stable identity.", Cardinality.ONE),
            "title": FieldDefinition("Display title.", Cardinality.OPTIONAL),
            "related": FieldDefinition(
                "Related records.", Cardinality.MANY, relation=True, examples=("repo:github.com/fmind/fkf",)
            ),
        }
    )
    validate_field_schema(schema)

    assert schema.names() == ["id", "related", "title"]
    assert schema.weight("id") == DEFAULT_ID_FIELD_WEIGHT
    assert schema.weight("title") == DEFAULT_TITLE_FIELD_WEIGHT
    assert schema.weight("related") == DEFAULT_FIELD_WEIGHT
    assert schema.select(FieldMap.from_json_value({"id": ".id", "related": ".related[]"})).names() == ["id", "related"]

    with pytest.raises(ValueError, match=r"schema\.id\.cardinality"):
        validate_field_schema(FieldSchema({"id": FieldDefinition("ID", Cardinality.MANY)}))
    with pytest.raises(ValueError, match=r"schema\.time must not be a relation"):
        validate_field_schema(
            FieldSchema(
                {
                    "id": FieldDefinition("ID", Cardinality.ONE),
                    "time": FieldDefinition("Time", Cardinality.ONE, relation=True),
                }
            )
        )


@pytest.mark.parametrize("scheme", ["person", "actor", "repo", "google+chat", "custom.v1"])
def test_entity_scheme_accepts_open_non_reserved_namespaces(scheme: str) -> None:
    validate_entity_scheme(scheme)


@pytest.mark.parametrize("scheme", ["", "Person", "http", "https", "ftp", "mailto", "file", "external", "bad_name"])
def test_entity_scheme_refuses_malformed_or_reserved_namespaces(scheme: str) -> None:
    with pytest.raises(ValueError, match=r"must|reserved"):
        validate_entity_scheme(scheme)


@pytest.mark.parametrize(
    "uri",
    ["person:email/marc@example.test", "actor:github.com/Marc", "tag:line%0Abreak", "repo:github.com/fmind/fkf"],
)
def test_entity_uri_requires_canonical_identity_text(uri: str) -> None:
    validate_entity_uri(uri)


@pytest.mark.parametrize(
    "uri",
    ["person:", "person: bad", "person:bad%escape", "person:line%0abreak", "person:raw?query", "https://example.test"],
)
def test_entity_uri_refuses_noncanonical_identity_text(uri: str) -> None:
    with pytest.raises(ValueError, match=r"must|entity|scheme"):
        validate_entity_uri(uri)


@pytest.mark.parametrize(
    "value",
    [
        "https://example.test/post",
        "events/2026-08-22/rss.json#https://example.test/post",
        "index/github-repositories.json#fmind/fkf",
        "tasks/2026-08-22/review/TASKS.md#verification",
        "projects/fkf.md#decisions",
        "wiki/retrieval-boundary.md#decision",
        "graph.tsv",
        "graph.meta.json?jq=.edges",
        "graph.generation.json?jq=.state",
        "AGENTS.md#invariants",
        "fkf.yaml",
    ],
)
def test_relation_value_accepts_only_published_canonical_uris(value: str) -> None:
    validate_relation_value(value)


@pytest.mark.parametrize(
    "value",
    [
        "http://example.test",
        "mailto:marc@example.test",
        "file:wiki/page.md",
        "wiki/../projects/p.md",
        "./wiki/x.md",
        "wiki//x.md",
        "wiki/x.md?evil=1",
        "wiki/x.md#",
        "wiki/nested/page.md",
        "events/not-a-date",
        "events/2026-08-22/not@source.json",
        "tasks/2026-08-22/review/notes.md",
        "wiki/diagram.png",
        "wiki/",
        "wiki/page.md?jq=.title",
        "fkf.yaml#config",
        "graph.tsv?jq=.",
    ],
)
def test_relation_value_refuses_unpublished_or_noncanonical_uris(value: str) -> None:
    with pytest.raises(ValueError, match=r".+"):
        validate_relation_value(value)


def test_well_known_field_vocabulary_is_closed() -> None:
    assert all(is_well_known_field(name) for name in ("id", "time", "title", "url"))
    assert not is_well_known_field("category")
