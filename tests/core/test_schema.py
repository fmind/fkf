from __future__ import annotations

import json
from pathlib import Path
from typing import cast

from fkf.config import MAX_FRESHNESS_AGE_HOURS, MAX_SOURCE_NAME_LENGTH, MAX_SYNC_CONCURRENCY
from fkf.schema import SCHEMA_URL, config_schema, encode_config_schema


def _object(value: object) -> dict[str, object]:
    assert isinstance(value, dict)
    return cast(dict[str, object], value)


def test_config_schema_matches_the_closed_loader_surface() -> None:
    schema = config_schema()
    properties = _object(schema["properties"])

    assert schema["$id"] == SCHEMA_URL
    assert schema["additionalProperties"] is False
    assert set(properties) == {"fkf", "name", "schema", "layers", "identities", "sources", "sync", "bin", "clients"}
    sources = _object(properties["sources"])
    sync = _object(_object(properties["sync"])["properties"])
    assert _object(sources["propertyNames"])["maxLength"] == MAX_SOURCE_NAME_LENGTH
    assert _object(sync["index_max_age_hours"])["maximum"] == MAX_FRESHNESS_AGE_HOURS
    assert _object(sync["concurrency"])["maximum"] == MAX_SYNC_CONCURRENCY

    source = _object(sources["additionalProperties"])
    source_properties = _object(source["properties"])
    assert source["required"] == ["run"]
    assert source["allOf"]
    assert _object(source_properties["requires"])["uniqueItems"] is True
    body_description = _object(source_properties["body"])["description"]
    assert isinstance(body_description, str)
    assert "{{id}}" in body_description
    run_description = _object(source_properties["run"])["description"]
    assert isinstance(run_description, str)
    for placeholder in ("date", "next_date", "start", "end", "base", "home"):
        assert f"{{{{{placeholder}}}}}" in run_description
    test_description = _object(source_properties["test"])["description"]
    assert isinstance(test_description, str)
    assert "{{base}}" in test_description
    assert "{{home}}" in test_description
    assert "env" not in properties
    assert "env" not in source_properties
    assert "lookup" not in source_properties


def test_encoded_schema_is_stable_json_and_matches_the_published_bytes() -> None:
    encoded = encode_config_schema()

    assert encoded.endswith(b"}\n")
    assert json.loads(encoded) == config_schema()
    assert encode_config_schema() == encoded
    assert Path("docs/fkf.schema.json").read_bytes() == encoded
