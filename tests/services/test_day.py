from __future__ import annotations

import os
from datetime import UTC, datetime
from pathlib import Path
from threading import Event
from typing import Any

import pytest

import fkf.day as day_module
import fkf.status as status_module
from fkf.base import Base
from fkf.config import ConfigError, load_config
from fkf.day import (
    DIGEST_DELIVERY_COMPACT_JSON,
    DIGEST_DELIVERY_JSON,
    DIGEST_DELIVERY_JSONL,
    DIGEST_DELIVERY_TEXT,
    BriefBudgetError,
    BriefRequest,
    DayRequest,
    DigestBudgetError,
    TimelineRequest,
    WhoReport,
    brief,
    day,
    encode_brief_json,
    encode_timeline_delivery,
    parse_temporal_query,
    render_brief_text,
    render_timeline_text,
    timeline,
    who,
)
from fkf.documents import Document, Record, day_window, fields_of, parse_day_in_location, schema_of
from fkf.graph import build_graph
from fkf.query import Window
from fkf.scan import ScanGuard
from fkf.store import Layer
from fkf.timeutil import parse_duration

CONFIG = """\
fkf: 1
name: temporal
schema:
  id: {description: Stable identity., cardinality: one}
  time: {description: Event time., cardinality: one}
  title: {description: Display title., cardinality: optional}
  participant: {description: People., cardinality: many, relation: true}
  repository: {description: Repository., cardinality: optional, relation: true}
  related: {description: Related record., cardinality: optional, relation: true}
identities:
  maxime:
    canonical: person:email/maxime@example.test
    aliases: [actor:github.com/maxime]
    kind: person
  fkf:
    canonical: repo:github.com/fmind/fkf
    aliases: [repository:github.com/fmind/fkf]
    kind: repository
layers: {events: true, index: false, tasks: true, projects: true, wiki: true}
sources:
  activity:
    enabled: true
    run: [provider, activity]
    fields: {id: .id, time: .time, title: .title, participant: [".people[]"], repository: .repo, related: .related}
  meetings:
    enabled: true
    run: [provider, meetings]
    fields: {id: .id, time: .time, title: .title, participant: [".people[]"], repository: .repo, related: .related}
"""


def make_base(tmp_path: Path, *, now: datetime | None = None) -> Base:
    root = tmp_path / "base"
    root.mkdir()
    (root / "fkf.yaml").write_text(CONFIG, encoding="utf-8")
    config = load_config(root)
    return Base(config=config, store=config.store(), now=lambda: now or datetime(2026, 5, 10, 12, tzinfo=UTC))


def write_events(base: Base, source_name: str, value: str, records: list[Record]) -> None:
    source = base.config.sources[source_name]
    bounds = day_window(parse_day_in_location(value, UTC))
    base.write_document(
        Document(
            source=source_name,
            layer=Layer.EVENTS,
            date=value,
            window_start=bounds.start,
            window_end=bounds.end,
            collected_at=bounds.end,
            schema=schema_of(source),
            fields=fields_of(source),
            count=len(records),
            records=records,
        )
    )


def populate_timeline(base: Base) -> None:
    write_events(
        base,
        "meetings",
        "2026-05-09",
        [
            {
                "id": "early",
                "time": "2026-05-09T08:00:00Z",
                "title": "Planning",
                "people": ["actor:github.com/maxime"],
                "repo": "repository:github.com/fmind/fkf",
            },
            {
                "id": "repeat",
                "time": "2026-05-09T09:00:00Z",
                "title": "Planning",
                "people": ["actor:github.com/maxime"],
                "repo": "repository:github.com/fmind/fkf",
            },
            {"id": "later", "time": "2026-05-09T11:00:00Z", "title": "Review"},
        ],
    )
    write_events(
        base,
        "activity",
        "2026-05-09",
        [
            {"id": f"activity-{index}", "time": f"2026-05-09T{12 + index:02d}:00:00Z", "title": f"Activity {index}"}
            for index in range(6)
        ],
    )


def write_page(base: Base, uri: str, text: str, *, modified: datetime | None = None) -> None:
    path = base.root / uri
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(text, encoding="utf-8")
    if modified is not None:
        timestamp = modified.timestamp()
        os.utime(path, (timestamp, timestamp))


@pytest.mark.parametrize(
    ("query", "clean", "since", "until", "derived", "newest"),
    [
        ("yesterday release work", "release work", "2026-05-09", "2026-05-09", "yesterday", False),
        ("release work this week", "release work", "2026-05-04", "2026-05-10", "this week", False),
        ("last friday release work", "release work", "2026-05-08", "2026-05-08", "last friday", False),
        ("2026-04 release work", "release work", "2026-04-01", "2026-04-30", "2026-04", False),
        ("last meeting notes", "meeting notes", "", "", "last", True),
    ],
)
def test_temporal_query_uses_the_closed_boundary_grammar(
    query: str,
    clean: str,
    since: str,
    until: str,
    derived: str,
    newest: bool,
) -> None:
    parsed = parse_temporal_query(query, datetime(2026, 5, 10, 12, tzinfo=UTC))

    assert (parsed.query, parsed.window.since, parsed.window.until, parsed.window.derived_from, parsed.newest) == (
        clean,
        since,
        until,
        derived,
        newest,
    )


def test_day_groups_chronologically_collapses_titles_and_summarizes_noisy_sources(tmp_path: Path) -> None:
    base = make_base(tmp_path)
    populate_timeline(base)

    report = day(base, DayRequest(date="yesterday", budget=2_000))

    assert report.receipt.window == Window("2026-05-09", "2026-05-09", "yesterday")
    assert report.receipt.records == 9
    assert [(group.source, group.count, group.summarized) for group in report.groups] == [
        ("meetings", 3, False),
        ("activity", 6, True),
    ]
    assert [(item.title, item.count) for item in report.groups[0].items] == [("Planning", 2), ("Review", 0)]
    assert report.people == ["person:email/maxime@example.test"]
    assert report.repositories == ["repo:github.com/fmind/fkf"]

    expanded = day(base, DayRequest(date="2026-05-09", budget=4_000, all=True))
    assert len(expanded.groups[1].items) == 6


def test_timeline_filters_relations_and_uses_exact_around_bounds(tmp_path: Path) -> None:
    base = make_base(tmp_path)
    populate_timeline(base)

    filtered = timeline(
        base,
        TimelineRequest(
            window=Window("2026-05-09", "2026-05-09"),
            sources=("meetings",),
            repository="repository:github.com/fmind/fkf",
            person="actor:github.com/maxime",
            budget=2_000,
            all=True,
        ),
    )

    assert filtered.receipt.records == 2
    assert filtered.receipt.repository == "repo:github.com/fmind/fkf"
    assert filtered.receipt.person == "person:email/maxime@example.test"

    around = timeline(
        base,
        TimelineRequest(
            around_uri="events/2026-05-09/meetings.json#repeat",
            around=parse_duration("1h"),
            budget=2_000,
            all=True,
        ),
    )
    assert around.receipt.around_window == "1h0m0s"
    assert [item.title for group in around.groups for item in group.items] == ["Planning"]
    assert around.groups[0].items[0].count == 2


@pytest.mark.parametrize(
    "delivery",
    [DIGEST_DELIVERY_JSON, DIGEST_DELIVERY_JSONL, DIGEST_DELIVERY_COMPACT_JSON, DIGEST_DELIVERY_TEXT],
)
def test_timeline_budget_accounts_for_the_exact_delivery(tmp_path: Path, delivery: str) -> None:
    base = make_base(tmp_path)
    populate_timeline(base)

    with pytest.raises(DigestBudgetError) as raised:
        day(base, DayRequest(date="yesterday", budget=1, all=True, delivery_format=delivery))
    report = day(
        base,
        DayRequest(date="yesterday", budget=raised.value.minimum, all=True, delivery_format=delivery),
    )
    encoded = encode_timeline_delivery(report)

    assert len(encoded) <= report.receipt.budget * 4
    assert report.receipt.used_tokens == (len(encoded) + 3) // 4
    repeated = day(base, DayRequest(date="yesterday", budget=raised.value.minimum, all=True, delivery_format=delivery))
    assert len(report.receipt.input_digest) == 64
    assert int(report.receipt.input_digest, 16) >= 0
    assert repeated.receipt.input_digest == report.receipt.input_digest
    if delivery == DIGEST_DELIVERY_TEXT:
        assert encoded.decode() == render_timeline_text(report)


def test_timeline_rejects_unknown_sources_and_delivery_formats(tmp_path: Path) -> None:
    base = make_base(tmp_path)

    with pytest.raises(ConfigError, match="absent"):
        timeline(base, TimelineRequest(window=Window("2026-05-09", "2026-05-09"), sources=("absent",)))
    with pytest.raises(ConfigError, match="delivery format"):
        day(base, DayRequest(date="yesterday", delivery_format="yaml"))


def test_brief_composes_attention_evidence_tasks_projects_and_exact_budget(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    base = make_base(tmp_path)
    write_events(
        base,
        "meetings",
        "2026-05-10",
        [{"id": "today", "time": "2026-05-10T09:00:00Z", "title": "Daily planning"}],
    )
    write_events(
        base,
        "meetings",
        "2026-05-09",
        [{"id": "yesterday", "time": "2026-05-09T10:00:00Z", "title": "Bounded delivery"}],
    )
    write_page(
        base,
        "tasks/2026-05-10/delta/TASKS.md",
        "---\ntitle: Finish the daily brief\nstatus: active\ndue: '2026-05-10'\n---\n\n"
        "# Finish the daily brief\n\n## Learned\n\n- Keep one receipt.\n",
    )
    write_page(
        base,
        "projects/fkf.md",
        "---\ntype: project\ntitle: FKF\nstatus: active\nreviewed: '2026-05-08'\nnext_action: Verify retrieval\n---\n\n# FKF\n",
        modified=datetime(2026, 5, 8, 12, tzinfo=UTC),
    )
    cancel = Event()
    forwarded: set[str] = set()
    original_load = day_module.IdentityResolver.load
    original_status = day_module.status_report
    original_find = day_module.find
    original_list_tasks = status_module.list_tasks
    original_load_layer = status_module.list_pages

    def load(_cls: object, selected: Base, *, cancel: object) -> Any:
        assert cancel is cancel_event
        forwarded.add("identity")
        return original_load(selected, cancel=cancel_event)

    def status(selected: Base, request: Any, *, cancel: object) -> Any:
        assert cancel is cancel_event
        forwarded.add("status")
        return original_status(selected, request, cancel=cancel_event)

    def find(selected: Base, filters: Any, *, cancel: object) -> Any:
        assert cancel is cancel_event
        forwarded.add("find")
        return original_find(selected, filters, cancel=cancel_event)

    def list_tasks(selected: Base, window: Any = None, *, cancel: object, scan: ScanGuard | None = None) -> Any:
        assert cancel is cancel_event
        forwarded.add("tasks")
        return original_list_tasks(selected, window, cancel=cancel_event, scan=scan)

    def load_layer(selected: Base, layer: Layer, filters: Any, *, cancel: object, scan: ScanGuard | None = None) -> Any:
        assert cancel is cancel_event
        forwarded.add("pages")
        return original_load_layer(selected, layer, filters, cancel=cancel_event, scan=scan)

    cancel_event = cancel
    monkeypatch.setattr(day_module.IdentityResolver, "load", classmethod(load))
    monkeypatch.setattr(day_module, "status_report", status)
    monkeypatch.setattr(day_module, "find", find)
    monkeypatch.setattr(status_module, "list_tasks", list_tasks)
    monkeypatch.setattr(status_module, "list_pages", load_layer)

    with pytest.raises(BriefBudgetError) as raised:
        brief(base, BriefRequest(budget=1), cancel=cancel)
    floor = brief(base, BriefRequest(budget=raised.value.minimum), cancel=cancel)
    report = brief(base, BriefRequest(budget=4_096), cancel=cancel)

    sections = {section.name: section for section in report.sections}
    assert set(sections) == {"attention", "today", "tasks_due", "yesterday", "active_projects"}
    assert sections["today"].total == 1
    assert sections["tasks_due"].items[0].title == "Finish the daily brief"
    assert sections["active_projects"].items[0].detail == "Verify retrieval · reviewed 2026-05-08"
    assert report.receipt.unharvested == 1
    assert report.receipt.selected + report.receipt.dropped == report.receipt.candidates
    assert floor.receipt.used_tokens <= raised.value.minimum
    assert len(encode_brief_json(report)) <= report.receipt.budget * 4
    assert len(render_brief_text(report).encode()) <= report.receipt.budget * 4
    assert forwarded == {"identity", "status", "find", "tasks", "pages"}


def test_brief_uses_project_commitments_not_file_modification_dates(tmp_path: Path) -> None:
    base = make_base(tmp_path)
    write_page(
        base,
        "projects/commitment.md",
        "---\ntype: project\ntitle: Commitment\nstatus: active\n"
        "next_action: Run the recovery drill\ndue: '2026-05-09'\n"
        "reviewed: '2026-04-01'\nblocker: Await access\n---\n\n# Commitment\n",
        modified=datetime(2020, 1, 1, tzinfo=UTC),
    )
    sections = {section.name: section for section in brief(base, BriefRequest(budget=4096)).sections}
    assert sections["tasks_due"].items[0].uri == "projects/commitment.md"
    detail = sections["active_projects"].items[0].detail
    assert "Run the recovery drill" in detail
    assert "reviewed 2026-04-01" in detail
    assert "blocked: Await access" in detail


def test_who_joins_identity_pages_graph_neighbours_and_recent_records(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    base = make_base(tmp_path)
    direct_uri = "events/2026-05-09/meetings.json#direct"
    linked_uri = "events/2026-05-09/activity.json#notes"
    second_hop_uri = "events/2026-05-09/activity.json#follow-up"
    write_page(
        base,
        "wiki/maxime.md",
        "---\ntype: person\ntitle: Maxime Cordy\naliases: [actor:github.com/maxime]\n---\n\n# Maxime Cordy\n",
    )
    write_events(
        base,
        "meetings",
        "2026-05-09",
        [
            {
                "id": "direct",
                "time": "2026-05-09T12:00:00Z",
                "title": "Planning with Maxime",
                "people": ["actor:github.com/maxime"],
                "related": linked_uri,
            }
        ],
    )
    write_events(
        base,
        "activity",
        "2026-05-09",
        [
            {
                "id": "notes",
                "time": "2026-05-09T12:05:00Z",
                "title": "Planning notes",
                "related": direct_uri,
            },
            {
                "id": "follow-up",
                "time": "2026-05-09T12:10:00Z",
                "title": "Second hop",
                "related": linked_uri,
            },
        ],
    )
    build_graph(base)
    cancel = Event()
    forwarded: set[str] = set()
    original_load = day_module.IdentityResolver.load
    original_find = day_module.find
    original_read_page = day_module.read_page
    original_open_graph = day_module.open_validated_graph_cache
    original_neighbours = day_module.neighbours_from_cache

    def load(_cls: object, selected: Base, *, cancel: object) -> Any:
        assert cancel is cancel_event
        forwarded.add("identity")
        return original_load(selected, cancel=cancel_event)

    def find(selected: Base, filters: Any, *, cancel: object) -> Any:
        assert cancel is cancel_event
        forwarded.add("find")
        return original_find(selected, filters, cancel=cancel_event)

    def read_page(selected: Base, uri: str, *, cancel: object) -> Any:
        assert cancel is cancel_event
        forwarded.add("page")
        return original_read_page(selected, uri, cancel=cancel_event)

    def open_graph(selected: Base, *, cancel: object) -> Any:
        assert cancel is cancel_event
        forwarded.add("graph-open")
        return original_open_graph(selected, cancel=cancel_event)

    def neighbours(cache: Any, query: Any, *, cancel: object) -> Any:
        assert cancel is cancel_event
        forwarded.add("graph-walk")
        return original_neighbours(cache, query, cancel=cancel_event)

    cancel_event = cancel
    monkeypatch.setattr(day_module.IdentityResolver, "load", classmethod(load))
    monkeypatch.setattr(day_module, "find", find)
    monkeypatch.setattr(day_module, "read_page", read_page)
    monkeypatch.setattr(day_module, "open_validated_graph_cache", open_graph)
    monkeypatch.setattr(day_module, "neighbours_from_cache", neighbours)

    report = who(base, "Maxime Cordy", cancel=cancel)

    assert isinstance(report, WhoReport)
    assert len(report.matches) == 1
    match = report.matches[0]
    assert match.canonical == "person:email/maxime@example.test"
    assert [page.uri for page in match.pages] == ["wiki/maxime.md"]
    assert match.total == 2
    assert [record.uri for record in match.recent] == [linked_uri, direct_uri]
    assert [(count.source, count.count) for count in match.counts] == [("activity", 1), ("meetings", 1)]
    assert second_hop_uri not in {record.uri for record in match.recent}
    assert forwarded == {"identity", "find", "page", "graph-open", "graph-walk"}


def test_who_rejects_an_empty_query(tmp_path: Path) -> None:
    with pytest.raises(ConfigError, match="name or URI"):
        who(make_base(tmp_path), "  ")


def test_brief_reuses_status_tasks_for_due_items(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    from collections import Counter

    import fkf.pages as pages

    base = make_base(tmp_path)
    write_page(base, "tasks/2026-05-10/once/TASKS.md", "---\ntitle: One read\ndue: '2026-05-10'\n---\n# One read\n")
    write_page(
        base,
        "projects/once.md",
        "---\ntype: project\ntitle: One project\nstatus: active\nnext_action: Verify once\n---\n# One project\n",
    )
    calls: Counter[str] = Counter()
    original = pages.parse_page

    def parse(uri: str, *args: Any, **kwargs: Any) -> Any:
        calls[uri] += 1
        return original(uri, *args, **kwargs)

    monkeypatch.setattr(pages, "parse_page", parse)
    result = brief(base, BriefRequest(budget=4096))
    assert calls["tasks/2026-05-10/once/TASKS.md"] == 1
    assert any(item.title == "One read" for section in result.sections for item in section.items)
    assert any(
        item.detail == "Verify once · review not recorded" for section in result.sections for item in section.items
    )
