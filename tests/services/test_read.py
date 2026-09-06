from __future__ import annotations

import json
from datetime import UTC, datetime
from pathlib import Path
from threading import Event

import pytest

from fkf.base import Base
from fkf.config import load_config
from fkf.documents import Document, Record, fields_of, schema_of
from fkf.errors import InvalidUsageError
from fkf.jsoncodec import JsonNumber, dumps
from fkf.process import Cancellation, Command, CommandResult
from fkf.read import ReadOptions, ReadResult, read, suggest_uris
from fkf.source_runtime import Environment
from fkf.store import Layer
from fkf.trust import write_trust
from fkf.uri import Scheme, parse_uri

CONFIG = """\
fkf: 1
name: brain
schema:
  id: {description: Stable identity., cardinality: one}
  time: {description: Event time., cardinality: one}
  title: {description: Meaningful title., cardinality: optional}
layers: {events: true, index: true, tasks: true, projects: true, wiki: true}
sources:
  journal:
    enabled: true
    layer: events
    run: [provider]
    fields: {id: .id, time: .time, title: .title}
    body: [provider, body, "{{id}}"]
BODY_POLICY  snapshot:
    enabled: true
    layer: index
    run: [provider]
    fields: {id: .id, title: .title}
"""


class FakeRunner:
    def __init__(self, output: bytes = b"full body") -> None:
        self.output = output
        self.commands: list[Command] = []
        self.cancellations: list[Cancellation | None] = []

    def run(self, command: Command, *, cancel: Cancellation | None = None) -> CommandResult:
        self.commands.append(command)
        self.cancellations.append(cancel)
        return CommandResult(self.output)


class ExplodingRunner:
    def run(self, command: Command, *, cancel: Cancellation | None = None) -> CommandResult:
        del command, cancel
        raise AssertionError("an offline read executed a command")


def make_base(tmp_path: Path, *, bodies: str = "none", runner: FakeRunner | ExplodingRunner | None = None) -> Base:
    root = tmp_path / "brain"
    root.mkdir()
    body_policy = "" if bodies == "none" else f"    bodies: {bodies}\n"
    (root / "fkf.yaml").write_text(CONFIG.replace("BODY_POLICY", body_policy), encoding="utf-8")
    config = load_config(root)
    base = Base(
        config=config,
        store=config.store(),
        environment=Environment.from_config(config),
        runner=runner or ExplodingRunner(),
        now=lambda: datetime(2026, 9, 6, 12, tzinfo=UTC),
    )
    event_source = config.sources["journal"]
    event_records: list[Record] = [
        {"id": "a1", "time": "2026-09-05T09:00:00Z", "title": "First"},
        {"id": "b2", "time": "2026-09-05T10:00:00Z", "title": "Second"},
    ]
    base.write_document(
        Document(
            source="journal",
            layer=Layer.EVENTS,
            date="2026-09-05",
            window_start="2026-09-05T00:00:00Z",
            window_end="2026-09-06T00:00:00Z",
            collected_at="2026-09-06T11:00:00Z",
            schema=schema_of(event_source),
            fields=fields_of(event_source),
            body=True,
            count=len(event_records),
            records=event_records,
        )
    )
    base.write_document(
        Document(
            source="other",
            layer=Layer.EVENTS,
            date="2026-09-05",
            window_start="2026-09-05T00:00:00Z",
            window_end="2026-09-06T00:00:00Z",
            collected_at="2026-09-06T11:00:00Z",
            schema=schema_of(event_source),
            fields=fields_of(event_source),
            count=0,
            records=[],
        )
    )
    index_source = config.sources["snapshot"]
    base.write_document(
        Document(
            source="snapshot",
            layer=Layer.INDEX,
            collected_at="2026-09-06T11:00:00Z",
            schema=schema_of(index_source),
            fields=fields_of(index_source),
            count=1,
            records=[{"id": "snapshot", "title": "Snapshot"}],
        )
    )
    wiki = root / "wiki"
    wiki.mkdir()
    (wiki / "retrieval-boundary.md").write_text(
        "---\ntype: decision\ntitle: Retrieval boundary\ntags: [retrieval]\n---\n\n"
        "# Retrieval boundary\n\nOverview.\n\n"
        "## Decision\n\nRead stored evidence only. [Alice](person:alice) "
        "and [source](https://example.test/evidence).\n\n"
        "### Detail\n\nNested detail.\n\n"
        "## Consequences\n\nNo ambient execution.\n",
        encoding="utf-8",
    )
    (root / "AGENTS.md").write_text("# Base instructions\n", encoding="utf-8")
    return base


def primary_payloads(result: ReadResult) -> list[str]:
    names = ("document", "record", "page", "text", "entries", "selection", "entity")
    return [name for name in names if getattr(result, name) is not None]


def test_read_resolves_published_shapes_offline_with_the_go_payload_contract(tmp_path: Path) -> None:
    base = make_base(tmp_path)
    cases = (
        ("events/2026-09-05/", ReadOptions(offset=1, limit=1), "directory", "entries"),
        ("events/2026-09-05/journal.json", ReadOptions(), "document", "document"),
        ("events/2026-09-05/journal.json#a1", ReadOptions(), "record", "record"),
        ("events/2026-09-05/journal.json?jq=.records[].id", ReadOptions(), "selection", "selection"),
        ("wiki/retrieval-boundary.md", ReadOptions(), "page", "page,text"),
        ("wiki/retrieval-boundary.md#decision", ReadOptions(), "section", "page,text"),
        ("fkf.yaml", ReadOptions(), "file", "text"),
    )

    results = [read(base, uri, options) for uri, options, _kind, _payload in cases]

    for result, (uri, _options, kind, payload) in zip(results, cases, strict=True):
        assert result.uri == str(parse_uri(uri))
        assert result.kind == kind
        assert primary_payloads(result) == payload.split(",")
    assert results[0].entries == ("events/2026-09-05/other.json",)
    assert results[1].document is not None
    assert results[1].document.count == 2
    assert results[2].record is not None
    assert results[2].record["id"] == "a1"
    assert results[3].selection == ["a1", "b2"]
    assert results[4].page is not None
    assert results[4].page.type == "decision"
    assert results[4].text == results[4].page.body
    assert results[5].page is results[4].page or results[5].page == results[4].page
    assert results[5].text == (
        "## Decision\n\nRead stored evidence only. [Alice](person:alice) and "
        "[source](https://example.test/evidence).\n\n### Detail\n\nNested detail."
    )
    assert "name: brain" in (results[6].text or "")

    encoded_document = dumps(results[1], indent=True).decode()
    assert '"fields": {\n      "id": ".id"' in encoded_document
    assert '"relation": false' not in encoded_document
    assert '"_steps"' not in encoded_document

    length = read(base, "events/2026-09-05/journal.json?jq=.records%7Clength")
    assert length.selection == JsonNumber("2")


def test_read_normalizes_markdown_timestamps_before_json_serialization(tmp_path: Path) -> None:
    base = make_base(tmp_path)
    (base.root / "wiki" / "timestamps.md").write_text(
        "---\ndate: 2026-09-06\nreviewed: 2026-09-06T14:34:56+02:00\n---\n# Timestamps\n",
        encoding="utf-8",
    )

    result = read(base, "wiki/timestamps.md")

    assert result.page is not None
    assert result.page.date == "2026-09-06"
    assert result.page.frontmatter == {
        "date": "2026-09-06T00:00:00Z",
        "reviewed": "2026-09-06T14:34:56+02:00",
    }
    assert json.loads(dumps(result))["page"]["frontmatter"] == result.page.frontmatter


def test_read_rejects_invalid_cross_shape_options_without_execution(tmp_path: Path) -> None:
    base = make_base(tmp_path)
    for uri, message in (
        ("events/2026-09-05/", "directory"),
        ("events/2026-09-05/journal.json", "add #<id>"),
        ("wiki/retrieval-boundary.md", "not a stored document"),
        ("person:alice", "entity"),
        ("https://example.test/evidence", "external graph node"),
    ):
        with pytest.raises(InvalidUsageError, match=message):
            read(base, uri, ReadOptions(body=True))
    with pytest.raises(InvalidUsageError, match="does not support fragments"):
        read(base, "fkf.yaml#anything")
    with pytest.raises(InvalidUsageError, match="applies to a JSON document"):
        read(base, "wiki/retrieval-boundary.md?jq=.title")
    with pytest.raises(InvalidUsageError, match="non-negative"):
        read(base, "events/2026-09-05/", ReadOptions(offset=-1))


def test_body_flag_is_the_only_execution_path_and_reports_cache_states(tmp_path: Path) -> None:
    runner = FakeRunner(b"body text")
    base = make_base(tmp_path, bodies="cache", runner=runner)
    write_trust(base.config, base.now())
    uri = "events/2026-09-05/journal.json#a1"

    first = read(base, uri, ReadOptions(body=True))
    second = read(base, uri, ReadOptions(body=True))

    assert first.record is not None
    assert first.body == "body text"
    assert first.body_state == "fetched-and-cached"
    assert second.body == "body text"
    assert second.body_state == "cached"
    assert len(runner.commands) == 1


def test_body_execution_receives_the_callers_cancellation(tmp_path: Path) -> None:
    runner = FakeRunner(b"body text")
    base = make_base(tmp_path, runner=runner)
    write_trust(base.config, base.now())
    cancel = Event()

    result = read(base, "events/2026-09-05/journal.json#a1", ReadOptions(body=True), cancel=cancel)

    assert result.body == "body text"
    assert runner.cancellations == [cancel]


def test_entities_and_graph_artifacts_use_one_validated_offline_generation(tmp_path: Path) -> None:
    base = make_base(tmp_path)
    from fkf.graph import build_graph

    build_graph(base)
    entity = read(base, "person:alice", ReadOptions(limit=1))
    external = read(base, "https://example.test/evidence")
    graph = read(base, "graph.tsv")
    meta = read(base, 'graph.meta.json?jq=.sha256.outputs."graph.tsv"')
    generation = read(base, "graph.generation.json?jq=.state")

    assert entity.entity is not None
    assert entity.entity.neighbours
    assert entity.entity.neighbours_truncated is False
    assert len(entity.snapshot_sha256) == 64
    assert external.entity is not None
    assert external.entity.scheme is Scheme.EXTERNAL
    assert external.entity.neighbours
    assert graph.kind == "file"
    assert graph.text
    assert meta.kind == "selection"
    assert isinstance(meta.selection, str)
    assert len(meta.selection) == 64
    assert generation.selection == "current"


def test_missing_graph_is_empty_but_a_corrupt_graph_is_not_hidden(tmp_path: Path) -> None:
    base = make_base(tmp_path)
    missing = read(base, "person:absent")
    assert missing.entity is not None
    assert missing.entity.neighbours == ()
    (base.root / "graph.tsv").mkdir()
    with pytest.raises(ValueError, match="neighbourhood"):
        read(base, "person:absent")


def test_suggestions_are_exact_bounded_and_remain_in_the_published_grammar(tmp_path: Path) -> None:
    base = make_base(tmp_path)
    suggestions = suggest_uris(base, "retrieval")
    assert suggestions[0] == "wiki/retrieval-boundary.md"
    assert len(suggestions) <= 5
    for uri in suggestions:
        parsed = parse_uri(uri)
        assert parsed.scheme is Scheme.FILE
        base.store.resolve(parsed.path)

    with pytest.raises(OSError, match="did you mean:") as suggested:
        read(base, "wiki/retrieval.md")
    assert "did you mean:" in str(suggested.value)
    assert "wiki/retrieval-boundary.md" in str(suggested.value)
    with pytest.raises(InvalidUsageError) as caught:
        read(base, "zzzznotathinganywhere")
    assert "did you mean" not in str(caught.value)
