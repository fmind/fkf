"""Boundary coverage for offline URI resolution and explicit body reads."""

from __future__ import annotations

from datetime import UTC, datetime
from pathlib import Path
from typing import Any

import pytest

from fkf.base import Base
from fkf.config import load_config
from fkf.documents import Document, Record, fields_of, schema_of
from fkf.errors import InvalidUsageError, OperationalError
from fkf.graph import EdgeValidationError
from fkf.markdown import Page
from fkf.process import Cancellation, Command, CommandResult
from fkf.read import ReadOptions, ReadResult, anchors_of, read, section_of, suggest_uris
from fkf.source_runtime import Environment
from fkf.store import Layer, UnsafePathError
from fkf.trust import write_trust

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


class RecordingRunner:
    def __init__(self, output: bytes = b"full body") -> None:
        self.output = output
        self.commands: list[Command] = []

    def run(self, command: Command, *, cancel: Cancellation | None = None) -> CommandResult:
        del cancel
        self.commands.append(command)
        return CommandResult(self.output)


class NoExecuteRunner:
    def __init__(self) -> None:
        self.commands: list[Command] = []

    def run(self, command: Command, *, cancel: Cancellation | None = None) -> CommandResult:
        del cancel
        self.commands.append(command)
        raise AssertionError(f"offline read executed {command.argv!r}")


def _make_base(
    tmp_path: Path,
    *,
    name: str = "brain",
    bodies: str = "none",
    runner: RecordingRunner | NoExecuteRunner | None = None,
) -> Base:
    root = tmp_path / name
    root.mkdir()
    policy = "" if bodies == "none" else f"    bodies: {bodies}\n"
    (root / "fkf.yaml").write_text(CONFIG.replace("BODY_POLICY", policy), encoding="utf-8")
    config = load_config(root)
    base = Base(
        config=config,
        store=config.store(),
        environment=Environment.from_config(config),
        runner=runner or NoExecuteRunner(),
        now=lambda: datetime(2026, 9, 6, 12, tzinfo=UTC),
    )

    event_source = config.sources["journal"]
    records: list[Record] = [
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
            count=len(records),
            records=records,
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
    (root / "wiki").mkdir()
    (root / "projects").mkdir()
    (root / "wiki" / "retrieval-boundary.md").write_text(
        "---\ntype: decision\ntitle: Retrieval boundary\ntags: [retrieval]\n---\n\n"
        "# Retrieval boundary\n\nOverview.\n\n"
        "## Decision\n\nRead stored evidence only.\n\n"
        "### Detail\n\nNested detail.\n\n"
        "## Consequences\n\nNo ambient execution.\n",
        encoding="utf-8",
    )
    (root / "AGENTS.md").write_text("# Base instructions\n", encoding="utf-8")
    return base


def test_read_result_rejects_ambiguous_or_incomplete_payloads() -> None:
    page = Page(uri="wiki/page.md", slug="page", body="# Page")
    cases: tuple[tuple[dict[str, Any], str], ...] = (
        ({"uri": "wiki/page.md", "kind": "page"}, "both page metadata and rendered text"),
        (
            {"uri": "wiki/page.md", "kind": "page", "page": page, "text": page.body, "record": {}},
            "cannot carry another primary payload",
        ),
        (
            {"uri": "wiki/page.md", "kind": "page", "page": page, "text": page.body, "selection": {}},
            "cannot carry a selection payload",
        ),
        ({"uri": "fkf.yaml", "kind": "file"}, "exactly one primary payload"),
        (
            {"uri": "fkf.yaml", "kind": "file", "text": "config", "entries": ()},
            "exactly one primary payload",
        ),
        (
            {"uri": "fkf.yaml", "kind": "file", "text": "config", "selection": {}},
            "non-selection read result",
        ),
        (
            {"uri": "index/snapshot.json#id", "kind": "record", "record": {}, "body_state": "cached"},
            "body_state must carry body text",
        ),
    )
    for arguments, message in cases:
        with pytest.raises(ValueError, match=message):
            ReadResult(**arguments)

    # JSON null remains a present selector result; ``kind`` disambiguates it from no payload.
    assert ReadResult(uri="index/snapshot.json?jq=null", kind="selection", selection=None).selection is None


def test_resolver_reports_record_heading_utf8_and_option_errors_without_execution(tmp_path: Path) -> None:
    runner = NoExecuteRunner()
    base = _make_base(tmp_path, runner=runner)

    cases: tuple[tuple[str, ReadOptions, type[Exception], str], ...] = (
        ("events/2026-09-05/journal.json#missing", ReadOptions(), OperationalError, "holds no record"),
        ("wiki/retrieval-boundary.md#missing", ReadOptions(), OperationalError, "its anchors are"),
        ("events/2026-09-05/", ReadOptions(limit=-1), InvalidUsageError, "limit must be non-negative"),
        ("graph.tsv", ReadOptions(body=True), InvalidUsageError, "is derived"),
        ("graph.tsv#anything", ReadOptions(), InvalidUsageError, "does not support fragments"),
        ("graph.tsv?jq=.state", ReadOptions(), InvalidUsageError, "applies to a JSON document"),
    )
    for uri, options, error_type, message in cases:
        with pytest.raises(error_type, match=message):
            read(base, uri, options)

    (base.root / "fkf.yaml").write_bytes(b"\xff")
    with pytest.raises(OperationalError, match="not valid UTF-8"):
        read(base, "fkf.yaml")
    assert runner.commands == []


def test_directory_listing_exposes_only_resolvable_children_and_paginates(tmp_path: Path) -> None:
    base = _make_base(tmp_path)
    projects = base.root / "projects"
    for name in ("alpha.md", "beta.md"):
        (projects / name).write_text(f"# {name}\n", encoding="utf-8")
    (projects / ".env").write_text("private", encoding="utf-8")
    (projects / "backup.key").write_text("private", encoding="utf-8")
    (projects / "nested").mkdir()
    (projects / "linked.md").symlink_to(projects / "alpha.md")

    result = read(base, "projects/", ReadOptions(offset=1, limit=1))

    assert result.entries == ("projects/beta.md",)
    assert read(base, "projects/", ReadOptions(offset=2)).entries == ()


def test_directory_errors_keep_the_requested_uri(monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> None:
    base = _make_base(tmp_path)

    def refuse_scandir(_path: object) -> object:
        raise PermissionError("denied")

    monkeypatch.setattr("fkf.read.os.scandir", refuse_scandir)
    with pytest.raises(OSError, match="list projects/: denied"):
        read(base, "projects/")


def test_symlinked_page_is_never_read_or_used_to_fetch(tmp_path: Path) -> None:
    runner = NoExecuteRunner()
    base = _make_base(tmp_path, runner=runner)
    outside = tmp_path / "outside.md"
    outside.write_text("private", encoding="utf-8")
    (base.root / "wiki" / "linked.md").symlink_to(outside)

    with pytest.raises(UnsafePathError, match="symlink inside the base"):
        read(base, "wiki/linked.md")
    assert runner.commands == []


def test_cached_body_survives_trust_drift_without_refetching_or_mutating_evidence(tmp_path: Path) -> None:
    runner = RecordingRunner(b"cached body")
    base = _make_base(tmp_path, bodies="cache", runner=runner)
    write_trust(base.config, base.now())
    uri = "events/2026-09-05/journal.json#a1"

    first = read(base, uri, ReadOptions(body=True))
    (base.root / "fkf.yaml").write_text((base.root / "fkf.yaml").read_text() + "\n# trust drift\n")
    no_execute = NoExecuteRunner()
    base.runner = no_execute
    second = read(base, uri, ReadOptions(body=True))
    stored = base.read_document("events/2026-09-05/journal.json")

    assert first.body_state == "fetched-and-cached"
    assert second.body_state == "cached"
    assert second.body == "cached body"
    assert len(runner.commands) == 1
    assert no_execute.commands == []
    assert "body" not in stored.records[0]


def test_suggestions_bound_ambiguous_matches_and_preserve_error_types(tmp_path: Path) -> None:
    base = _make_base(tmp_path)
    for layer in ("wiki", "projects"):
        (base.root / layer / "retrieval.md").write_text(
            "---\ntype: insight\ntitle: Retrieval\n---\n\n# Retrieval\n\n## Evidence\n\nStored.\n",
            encoding="utf-8",
        )
    for index in range(8):
        (base.root / "wiki" / f"topic-{index}.md").write_text(
            f"---\ntype: insight\ntitle: Topic {index}\n---\n\n# Topic {index}\n",
            encoding="utf-8",
        )

    ambiguous = suggest_uris(base, "retrieval")
    bounded = suggest_uris(base, "topic")

    assert ambiguous[:2] == ("wiki/retrieval.md", "projects/retrieval.md")
    assert len(bounded) == 5
    assert suggest_uris(base, "   ") == ()
    with pytest.raises(InvalidUsageError, match="did you mean:") as caught:
        read(base, "retrieval")
    assert "wiki/retrieval.md" in str(caught.value)


def test_suggestions_fail_closed_when_optional_surfaces_are_unreadable(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    base = _make_base(tmp_path)

    def broken_pages(
        candidate: Base,
        layer: Layer,
        *,
        cancel: object = None,
    ) -> tuple[tuple[Page, ...], tuple[str, ...]]:
        del candidate, cancel
        if layer is Layer.PROJECTS:
            raise OSError("unreadable projects")
        return (), ()

    def broken_indexes(candidate: Base) -> tuple[str, ...]:
        del candidate
        raise OSError("unreadable index")

    def broken_events(candidate: Base) -> tuple[str, ...]:
        del candidate
        raise ValueError("invalid event directory")

    monkeypatch.setattr("fkf.read.load_markdown_layer", broken_pages)
    monkeypatch.setattr(Base, "index_documents", broken_indexes)
    monkeypatch.setattr(Base, "event_dates", broken_events)

    assert suggest_uris(base, "anything") == ()


def test_graph_artifact_boundaries_distinguish_json_null_and_corruption(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    base = _make_base(tmp_path)
    (base.root / "graph.generation.json").write_text('{"state":null}', encoding="utf-8")

    generation = read(base, "graph.generation.json")
    selected = read(base, "graph.generation.json?jq=.state")

    assert generation.kind == "index"
    assert generation.selection == {"state": None}
    assert selected.kind == "selection"
    assert selected.selection is None

    (base.root / "graph.generation.json").write_text("not-json", encoding="utf-8")
    with pytest.raises(EdgeValidationError, match="not one JSON document"):
        read(base, "graph.generation.json")

    def invalid_utf8(candidate: Base, path: str, limit: int, *, cancel: object = None) -> bytes:
        del candidate, path, limit, cancel
        return b"\xff"

    monkeypatch.setattr("fkf.graph.read_validated_graph_artifact", invalid_utf8)
    with pytest.raises(EdgeValidationError, match="not valid UTF-8"):
        read(base, "graph.tsv")


def test_sections_stop_at_peer_headings_and_expose_canonical_anchors(tmp_path: Path) -> None:
    base = _make_base(tmp_path)
    result = read(base, "wiki/retrieval-boundary.md")
    assert result.page is not None

    assert anchors_of(result.page) == ("retrieval-boundary", "decision", "detail", "consequences")
    assert section_of(result.page, "decision") == (
        "## Decision\n\nRead stored evidence only.\n\n### Detail\n\nNested detail."
    )
    assert section_of(result.page, "consequences") == "## Consequences\n\nNo ambient execution."
    assert section_of(result.page, "absent") is None
