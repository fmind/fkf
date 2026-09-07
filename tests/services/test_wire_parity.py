from __future__ import annotations

from datetime import UTC, datetime
from pathlib import Path

from fkf.base import Base
from fkf.config import load_config
from fkf.context import ContextRequest, build_context
from fkf.day import TimelineRequest, _input_digest
from fkf.documents import build_document, day_window, parse_day_in_location
from fkf.find import RecordHit
from fkf.graph import build_graph
from fkf.query import Window

_CONTEXT_CONFIG = """\
fkf: 1
name: brain
schema:
  id: {description: Stable identity., cardinality: one}
  time: {description: Event time., cardinality: optional}
  title: {description: Meaningful title., cardinality: optional}
  ticket: {description: Tickets., cardinality: many, relation: true}
layers: {events: true, index: false, tasks: false, projects: false, wiki: false}
sources:
  synthetic:
    enabled: true
    run: [provider]
    fields: {id: .id, time: .time, title: .title, ticket: '.tickets[]'}
"""


def test_timeline_input_digest_matches_go_html_escaping() -> None:
    window = Window("2026-09-05", "2026-09-05", "day")
    record = RecordHit(
        uri="events/2026-09-05/demo.json#a&b<c>",
        source="demo",
        time="2026-09-05T12:00:00Z",
        title="A&B<C>",
        fields={"detail": ("x&y<z>",)},
        _relation_fields=frozenset({"detail"}),
    )
    request = TimelineRequest(
        window=window,
        budget=600,
        delivery_format="json",
        _base_name="brain",
    )

    digest = _input_digest(
        [record],
        request,
        window,
        "2026-09-06",
        [],
        [],
    )

    assert digest == "2d626961ea22f030589f1139ac91c7360a6d93082d1917b9634aa42e2efc2bf2"


def test_context_expansion_digest_appends_new_candidates_in_go_order(tmp_path: Path) -> None:
    root = tmp_path / "brain"
    root.mkdir()
    (root / "fkf.yaml").write_text(_CONTEXT_CONFIG, encoding="utf-8")
    config = load_config(root)
    base = Base(
        config=config,
        store=config.store(),
        now=lambda: datetime(2026, 9, 6, 12, tzinfo=UTC),
    )
    document = build_document(
        base.source("synthetic"),
        [
            {
                "id": "seed",
                "time": "2026-09-05T09:00:00Z",
                "title": "needle seed",
                "tickets": ["ticket:a", "ticket:z"],
            },
            {
                "id": "zzzz",
                "time": "2026-09-05T10:00:00Z",
                "title": "alpha relation",
                "tickets": ["ticket:a"],
            },
            {
                "id": "aaaa",
                "time": "2026-09-05T11:00:00Z",
                "title": "zeta relation",
                "tickets": ["ticket:z"],
            },
        ],
        window=day_window(parse_day_in_location("2026-09-05", UTC)),
        collected_at=base.now(),
    )
    base.write_document(document)
    build_graph(base)

    pack = build_context(
        base,
        ContextRequest(
            query="needle",
            budget=4096,
            expand=True,
            explain=True,
            delivery_format="json",
        ),
    )

    assert pack.receipt.input_digest == "dc6ebb92372bd692"
