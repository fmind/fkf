from __future__ import annotations

from datetime import UTC, datetime
from pathlib import Path

import pytest

from fkf.base import Base
from fkf.config import ConfigError, load_config
from fkf.documents import Document, day_window, fields_of, parse_day_in_location, schema_of
from fkf.jsoncodec import dumps
from fkf.listings import list_events, list_index, list_tasks
from fkf.query import Window
from fkf.store import Layer

CONFIG = """\
fkf: 1
name: brain
schema:
  id: {description: Stable identity., cardinality: one}
  time: {description: Event time., cardinality: one}
  title: {description: Meaningful title., cardinality: optional}
layers: {events: true, index: true, tasks: true, projects: true, wiki: true}
sync: {index_max_age_hours: 24}
sources:
  events:
    enabled: true
    layer: events
    run: [provider]
    fields: {id: .id, time: .time, title: .title}
  snapshot:
    enabled: true
    layer: index
    max_age_hours: 2
    run: [provider]
    fields: {id: .id, title: .title}
"""


def make_base(tmp_path: Path) -> Base:
    root = tmp_path / "brain"
    root.mkdir()
    (root / "fkf.yaml").write_text(CONFIG)
    config = load_config(root)
    return Base(config=config, store=config.store(), now=lambda: datetime(2026, 9, 6, 12, tzinfo=UTC))


def empty_document(base: Base, *, source: str, layer: Layer, day: str = "", collected: str) -> Document:
    window = day_window(parse_day_in_location(day, UTC)) if day else None
    definition = base.config.sources[source]
    return Document(
        source=source,
        layer=layer,
        date=day,
        window_start=window.start if window else "",
        window_end=window.end if window else "",
        collected_at=collected,
        schema=schema_of(definition),
        fields=fields_of(definition),
        count=0,
        records=[],
    )


def test_list_events_walks_dates_first_and_filters_known_source(tmp_path: Path) -> None:
    base = make_base(tmp_path)
    for day, count in (("2026-09-04", 0), ("2026-09-05", 0)):
        document = empty_document(
            base,
            source="events",
            layer=Layer.EVENTS,
            day=day,
            collected="2026-09-06T10:00:00Z",
        )
        document.count = count
        base.write_document(document)
    listing = list_events(base, Window("2026-09-05", "2026-09-05"), source="events")
    assert [day.date for day in listing.days] == ["2026-09-05"]
    assert listing.days[0].sources[0].uri == "events/2026-09-05/events.json"
    with pytest.raises(ConfigError, match="unknown source"):
        list_events(base, source="typo")


def test_list_index_uses_collected_time_and_source_freshness(tmp_path: Path) -> None:
    base = make_base(tmp_path)
    document = empty_document(
        base,
        source="snapshot",
        layer=Layer.INDEX,
        collected="2026-09-06T09:00:00Z",
    )
    base.write_document(document)
    listing = list_index(base)
    assert listing.total == 1
    assert listing.entries[0].age_hours == 3
    assert listing.entries[0].stale is True
    assert listing.entries[0].bytes > 0


def test_list_index_omits_zero_count_and_fresh_marker_from_json(tmp_path: Path) -> None:
    base = make_base(tmp_path)
    base.write_document(
        empty_document(
            base,
            source="snapshot",
            layer=Layer.INDEX,
            collected="2026-09-06T12:00:00Z",
        )
    )

    encoded = dumps(list_index(base))

    assert b'"count"' not in encoded
    assert b'"stale"' not in encoded


def test_list_tasks_is_newest_first_and_requires_trace_leaf(tmp_path: Path) -> None:
    base = make_base(tmp_path)
    for day, slug, title in (
        ("2026-09-04", "first", "First"),
        ("2026-09-05", "alpha", "Alpha"),
        ("2026-09-05", "zeta", "Zeta"),
    ):
        directory = base.root / "tasks" / day / slug
        directory.mkdir(parents=True)
        (directory / "TASKS.md").write_text(f"# {title}\n")
    missing = base.root / "tasks" / "2026-09-06" / "missing"
    missing.mkdir(parents=True)

    listing = list_tasks(base, limit=2)
    assert [(trace.date, trace.slug, trace.title) for trace in listing.traces] == [
        ("2026-09-05", "alpha", "Alpha"),
        ("2026-09-05", "zeta", "Zeta"),
    ]
