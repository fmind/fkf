from __future__ import annotations

import json
from dataclasses import dataclass
from datetime import UTC, datetime
from pathlib import Path
from threading import Event

import pytest

import fkf.find as find_module
from fkf.base import Base
from fkf.bodies import BodyCacheError, cache_body
from fkf.config import ConfigError, load_config
from fkf.documents import Document, Record, build_document, day_window, parse_day_in_location
from fkf.find import (
    DEFAULT_FIND_DAYS,
    DEFAULT_FIND_LIMIT,
    NO_FIND_LIMIT,
    FindFilter,
    FindPosition,
    compact_find_result,
    find,
    find_bounded,
    parse_where,
)
from fkf.jsoncodec import dumps
from fkf.lexical import build_lexical_index
from fkf.process import Command, CommandCanceledError, CommandResult
from fkf.query import Window
from fkf.store import Layer, LayerDisabledError

CONFIG = """\
fkf: 1
name: brain
schema:
  id: {description: Stable identity., cardinality: one}
  time: {description: Event time., cardinality: one}
  title: {description: Meaningful title., cardinality: optional}
  repo: {description: Repository., cardinality: optional, relation: true}
  author: {description: Author., cardinality: optional, relation: true}
  topic: {description: Topic., cardinality: optional}
layers: {events: true, index: true, tasks: true, projects: true, wiki: true}
sources:
  synthetic:
    enabled: true
    run: [provider]
    fields: {id: .id, time: .time, title: .subject, repo: .repo, author: .author, topic: .topic}
  disabled-source:
    run: [provider]
    fields: {id: .id, time: .time, title: .subject, repo: .repo, author: .author, topic: .topic}
  catalog:
    enabled: true
    layer: index
    run: [provider]
    fields: {id: .id, title: .title, topic: .topic}
"""


@dataclass
class ExplodingRunner:
    calls: int = 0

    def run(self, command: Command, *, cancel: object | None = None) -> CommandResult:
        del cancel, command
        self.calls += 1
        raise AssertionError("offline find executed a provider command")


def make_base(tmp_path: Path, *, config_text: str = CONFIG) -> Base:
    root = tmp_path / "brain"
    root.mkdir(parents=True)
    (root / "fkf.yaml").write_text(config_text, encoding="utf-8")
    config = load_config(root)
    return Base(
        config=config,
        store=config.store(),
        runner=ExplodingRunner(),
        now=lambda: datetime(2026, 5, 10, 12, tzinfo=UTC),
    )


def write_event(base: Base, day: str, records: list[Record], *, source_name: str = "synthetic") -> Document:
    source = base.source(source_name)
    document = build_document(
        source,
        records,
        window=day_window(parse_day_in_location(day, UTC)),
        collected_at=base.now(),
    )
    base.write_document(document)
    return document


def write_index(base: Base, records: list[Record]) -> Document:
    source = base.source("catalog")
    document = build_document(source, records, collected_at=base.now())
    base.write_document(document)
    return document


def write_page(base: Base, uri: str, body: str) -> None:
    path = base.root / uri
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(body, encoding="utf-8")


def test_parse_where_uses_the_closed_field_path_grammar() -> None:
    clause = parse_where('.items[].repo="not syntax"')
    assert str(clause.path) == ".items[].repo"
    assert clause.value == '"not syntax"'
    with pytest.raises(ValueError, match="takes <path>=<value>"):
        parse_where("no-equals")
    with pytest.raises(ValueError, match="must start"):
        parse_where("repo=value")


def test_record_search_matches_only_scalar_values_and_where_is_case_insensitive(tmp_path: Path) -> None:
    base = make_base(tmp_path)
    write_event(
        base,
        "2026-05-04",
        [
            {
                "id": "a",
                "time": "2026-05-04T10:00:00Z",
                "subject": "Retrieval boundary",
                "repo": "repo:github.com/acme/ledger",
                "nested": {"label": "Inside"},
                "items": [False, 7],
                "count": 42.5,
            }
        ],
    )

    for term in ("retrieval", "inside", "false", "7", "42.5"):
        result = find(base, FindFilter(grep=(term,)))
        assert result.matched == 1
    for key_or_compound in ("subject", "nested", "label", "items", "null", '{"label"'):
        assert find(base, FindFilter(grep=(key_or_compound,))).matched == 0

    result = find(base, FindFilter(where=(parse_where(".repo=REPO:GITHUB.COM/ACME/LEDGER"),)))
    assert result.matched == 1
    with pytest.raises(ValueError, match="non-whitespace"):
        find(base, FindFilter(grep=("  ",)))


def test_record_projection_normalizes_time_to_the_go_second_precision(tmp_path: Path) -> None:
    base = make_base(tmp_path)
    write_event(
        base,
        "2026-05-04",
        [
            {
                "id": "offset",
                "time": "2026-05-04T11:00:00.987654321+02:00",
                "subject": "Normalized instant",
            }
        ],
    )

    result = find(base, FindFilter(grep=("normalized",)))
    assert result.records[0].time == "2026-05-04T09:00:00Z"


def test_bare_find_is_recent_and_bounded_but_a_question_is_exhaustive(tmp_path: Path) -> None:
    base = make_base(tmp_path)
    for day in range(1, DEFAULT_FIND_DAYS + 2):
        label = f"2026-05-{day:02d}"
        write_event(
            base,
            label,
            [{"id": f"event-{day}", "time": f"{label}T09:00:00Z", "subject": "exhaustive signal"}],
        )
    write_index(base, [{"id": "index-match", "title": "exhaustive signal", "topic": "current"}])

    bare = find(base)
    assert len(bare.days) == DEFAULT_FIND_DAYS
    assert all(not record.uri.startswith("index/") for record in bare.records)
    assert all("2026-05-01" not in record.uri for record in bare.records)

    exhaustive = find(base, FindFilter(grep=("exhaustive signal",)))
    assert exhaustive.matched == DEFAULT_FIND_DAYS + 2
    assert len(exhaustive.records) == DEFAULT_FIND_DAYS + 2
    assert not exhaustive.truncated
    assert exhaustive.records[-1].uri == "index/catalog.json#index-match"

    records: list[Record] = [
        {"id": f"many-{index:03d}", "time": "2026-05-09T09:00:00Z", "subject": "bounded discovery"}
        for index in range(DEFAULT_FIND_LIMIT + 1)
    ]
    write_event(base, "2026-05-09", records)
    bounded = find(base)
    assert len(bounded.records) == DEFAULT_FIND_LIMIT
    assert bounded.truncated


def test_find_is_window_first_and_keeps_disabled_source_evidence(tmp_path: Path) -> None:
    base = make_base(tmp_path)
    corrupt = base.root / "events/2026-01-01/synthetic.json"
    corrupt.parent.mkdir(parents=True)
    corrupt.write_text("not JSON", encoding="utf-8")
    write_event(
        base,
        "2026-05-04",
        [{"id": "retired", "time": "2026-05-04T09:00:00Z", "subject": "Existing evidence"}],
        source_name="disabled-source",
    )

    recent = find(base, FindFilter(window=Window(since="2026-05-01"), sources=("disabled-source",)))
    assert [record.uri for record in recent.records] == ["events/2026-05-04/disabled-source.json#retired"]
    with pytest.raises(ValueError, match="decode"):
        find(base, FindFilter(window=Window(since="2026-01-01")))


def test_find_scans_pages_and_records_with_per_half_limits_and_layer_validation(tmp_path: Path) -> None:
    base = make_base(tmp_path)
    write_event(
        base,
        "2026-05-04",
        [{"id": "event", "time": "2026-05-04T09:00:00Z", "subject": "Needle record"}],
    )
    write_page(base, "wiki/needle.md", "---\ntitle: Needle wiki\n---\n\n# Needle wiki\n")
    write_page(base, "projects/needle.md", "---\ntitle: Needle project\nstatus: active\n---\n\n# Needle project\n")
    write_page(
        base,
        "tasks/2026-05-04/session/TASKS.md",
        "---\ntitle: Needle task\n---\n\n# Needle task\n\nThe trace keeps evidence.\n",
    )

    result = find(base, FindFilter(grep=("needle",), limit=1))
    assert len(result.pages) == 1
    assert len(result.records) == 1
    assert result.records[0].uri.endswith("#event")

    pages_only = find(base, FindFilter(grep=("needle",), layers=(Layer.WIKI,), limit=NO_FIND_LIMIT))
    assert [page.uri for page in pages_only.pages] == ["wiki/needle.md"]
    assert pages_only.records == ()

    record_only = find(base, FindFilter(grep=("needle",), sources=("synthetic",)))
    assert record_only.pages == ()
    with pytest.raises(ConfigError, match="unknown source"):
        find(base, FindFilter(sources=("typo",)))

    disabled = CONFIG.replace("events: true", "events: false").replace("    enabled: true\n", "    enabled: false\n")
    disabled_base = make_base(tmp_path / "disabled", config_text=disabled)
    with pytest.raises(LayerDisabledError):
        find(disabled_base, FindFilter(grep=("needle",), layers=(Layer.EVENTS,)))
    with pytest.raises(ValueError, match="fkf list wiki"):
        find(base, FindFilter(layers=(Layer.WIKI,)))


def test_bounded_find_pages_tasks_and_index_records_across_a_continuation(tmp_path: Path) -> None:
    base = make_base(tmp_path)
    write_event(
        base,
        "2026-05-04",
        [{"id": "event", "time": "2026-05-04T09:00:00Z", "subject": "Needle event record"}],
    )
    write_page(
        base,
        "tasks/2026-05-04/memory/TASKS.md",
        "---\ntitle: Needle task trace\n---\n\n# Needle task trace\n\nBounded retrieval evidence.\n",
    )
    write_page(base, "tasks/2026-05-04/no-trace/README.md", "needle but not TASKS.md\n")
    write_index(
        base,
        [
            {"id": "one", "title": "Needle index record"},
            {"id": "two", "title": "Unrelated index record"},
        ],
    )
    filters = FindFilter(grep=("needle",), layers=(Layer.TASKS, Layer.INDEX))

    first = find_bounded(base, filters, counting=False, limit=1)
    assert [page.uri for page in first.result.pages] == ["tasks/2026-05-04/memory/TASKS.md"]
    assert first.result.records == ()
    assert (first.result.scanned, first.result.matched) == (2, 1)
    assert first.next == FindPosition("page", score=10, uri="tasks/2026-05-04/memory/TASKS.md")

    second = find_bounded(base, filters, counting=False, limit=1, after=first.next)
    assert second.result.pages == ()
    assert [record.uri for record in second.result.records] == ["index/catalog.json#one"]
    assert second.next is None
    assert second.snapshot_sha256 == first.snapshot_sha256
    assert (second.result.scanned, second.result.matched) == (2, 1)

    count_filters = FindFilter(grep=("needle",), layers=(Layer.EVENTS, Layer.INDEX))
    first_count = find_bounded(base, count_filters, counting=True, limit=1)
    assert [(volume.date, volume.total) for volume in first_count.result.volumes] == [("2026-05-04", 1)]
    assert first_count.next == FindPosition("volume", date="2026-05-04")
    assert (first_count.result.scanned, first_count.result.matched) == (3, 2)

    second_count = find_bounded(base, count_filters, counting=True, limit=1, after=first_count.next)
    assert [(volume.date, volume.total) for volume in second_count.result.volumes] == [("", 1)]
    assert second_count.result.volumes[0].sources[0].source == "catalog"
    assert second_count.next is None
    assert second_count.snapshot_sha256 == first_count.snapshot_sha256


def test_count_bodies_and_compaction_remain_exhaustive_and_offline(tmp_path: Path) -> None:
    base = make_base(tmp_path)
    first = write_event(
        base,
        "2026-05-04",
        [{"id": "a", "time": "2026-05-04T09:00:00Z", "subject": "Needle"}],
    )
    write_event(
        base,
        "2026-05-05",
        [{"id": "b", "time": "2026-05-05T09:00:00Z", "subject": "Needle"}],
    )
    write_index(base, [{"id": "c", "title": "Needle"}])

    counted = find(base, FindFilter(grep=("needle",), limit=1), counting=True)
    assert len(counted.volumes) == 1
    assert counted.truncated
    assert (counted.scanned, counted.matched) == (3, 3)
    assert counted.records == ()

    record = first.records[0]
    uri = first.record_uri(record)
    assert uri is not None
    entry = cache_body(base, first, record, uri, "Private cached body marker")
    body_result = find(base, FindFilter(grep=("body marker",), bodies=True))
    assert [record.uri for record in body_result.records] == [uri]
    assert body_result.records[0].body_cached
    assert isinstance(base.runner, ExplodingRunner)
    assert base.runner.calls == 0

    encoded = json.loads(dumps(body_result))
    assert encoded["records"][0]["record"]["id"] == "a"
    assert "raw" not in encoded["records"][0]
    assert "_relation_fields" not in encoded["records"][0]

    compact_find_result(body_result)
    assert body_result.days == ()
    assert body_result.records[0].raw is None
    compact = json.loads(dumps(body_result))
    assert "days" not in compact
    assert "record" not in compact["records"][0]
    with pytest.raises(ValueError, match="requires at least one"):
        find(base, FindFilter(bodies=True))

    (base.root / entry.path).write_text("tampered", encoding="utf-8")
    with pytest.raises(BodyCacheError, match="does not match"):
        find(base, FindFilter(grep=("body",), bodies=True))


def test_lexical_candidates_preserve_scan_results_and_stale_fallback(tmp_path: Path) -> None:
    base = make_base(tmp_path)
    write_event(
        base,
        "2026-05-04",
        [
            {"id": "match", "time": "2026-05-04T09:00:00Z", "subject": "Needle record"},
            {"id": "other", "time": "2026-05-04T10:00:00Z", "subject": "Unrelated"},
        ],
    )
    write_page(base, "wiki/needle.md", "# Needle page\n")
    filters = FindFilter(grep=("needle",))

    fallback = find(base, filters)
    assert fallback.index is not None
    assert (fallback.index.used, fallback.index.reason) == (False, "missing")

    build_lexical_index(base)
    indexed = find(base, filters)
    assert indexed.index is not None
    assert (indexed.index.used, indexed.index.reason) == (True, "")
    assert indexed.pages == fallback.pages
    assert indexed.records == fallback.records
    assert (indexed.scanned, indexed.matched) == (fallback.scanned, fallback.matched)

    write_page(base, "projects/new-needle.md", "# New needle page\n")
    stale = find(base, filters)
    assert stale.index is not None
    assert (stale.index.used, stale.index.reason) == (False, "stale")
    assert [page.uri for page in stale.pages] == ["projects/new-needle.md", "wiki/needle.md"]
    assert stale.records == fallback.records


def test_find_retries_generation_drift_even_when_the_first_scan_errors(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    base = make_base(tmp_path)
    document = write_event(
        base,
        "2026-05-04",
        [{"id": "match", "time": "2026-05-04T09:00:00Z", "subject": "Needle record"}],
    )
    original = Base.read_document
    drifted = False

    def drifting_read(selected: Base, uri: str) -> Document:
        nonlocal drifted
        if not drifted:
            drifted = True
            path = selected.root / document.uri()
            path.write_text(path.read_text(encoding="utf-8") + "\n", encoding="utf-8")
            raise ValueError("generation moved during the first read")
        return original(selected, uri)

    monkeypatch.setattr(Base, "read_document", drifting_read)
    result = find(base, FindFilter(grep=("needle",)))

    assert [record.uri for record in result.records] == [document.record_uri(document.records[0])]
    assert result.index is not None
    assert (result.index.used, result.index.reason) == (False, "stale")


def test_find_honors_pre_cancellation(tmp_path: Path) -> None:
    base = make_base(tmp_path)
    canceled = Event()
    canceled.set()
    with pytest.raises(CommandCanceledError):
        find(base, cancel=canceled)


def test_find_forwards_the_invocation_event_to_the_lexical_reader(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    base = make_base(tmp_path)
    build_lexical_index(base)
    cancel = Event()

    def cancel_lexical_read(*_args: object, **kwargs: object) -> object:
        assert kwargs["cancel"] is cancel_event
        raise CommandCanceledError("command canceled")

    cancel_event = cancel
    monkeypatch.setattr(find_module, "query_find_lexical_index", cancel_lexical_read)

    with pytest.raises(CommandCanceledError, match="command canceled"):
        find(base, FindFilter(grep=("retrieval",)), cancel=cancel)
