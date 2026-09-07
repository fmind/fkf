"""Realistic provider-shape fixtures for every declared bundled source."""

from __future__ import annotations

from datetime import UTC, datetime
from pathlib import Path

import pytest

from fkf.assets import PRESETS
from fkf.documents import build_document, day_window, decode_records, parse_day_in_location

from .conftest import SOURCE_FIXTURES, load_preset

SOURCE_NAMES = tuple(sorted(path.stem for path in SOURCE_FIXTURES.glob("*.json")))


@pytest.mark.parametrize("source_name", SOURCE_NAMES)
def test_every_declared_source_fixture_produces_addressable_records(tmp_path: Path, source_name: str) -> None:
    sources = {}
    for preset in PRESETS:
        sources.update(load_preset(tmp_path, preset).sources)
    source = sources[source_name]
    records = decode_records(source, (SOURCE_FIXTURES / f"{source_name}.json").read_bytes())
    window = day_window(parse_day_in_location("2026-05-04", UTC)) if source.layer.value == "events" else None
    document = build_document(
        source,
        records,
        window=window,
        collected_at=datetime(2026, 5, 10, 12, tzinfo=UTC),
    )
    assert document.count > 0
    assert all(document.record_uri(record) is not None for record in document.records)
