from __future__ import annotations

from dataclasses import dataclass, replace
from datetime import UTC, datetime
from pathlib import Path
from threading import Event

import pytest

import fkf.context as context_module
from fkf.base import Base
from fkf.config import ConfigError, load_config
from fkf.context import (
    RANKING_VERSION,
    ContextBudgetError,
    ContextRequest,
    Reason,
    build_context,
    render_context_bytes,
    render_context_text,
)
from fkf.documents import Document, Record, build_document, day_window, parse_day_in_location
from fkf.graph import build_graph
from fkf.jsoncodec import dumps
from fkf.lexical import build_lexical_index
from fkf.locking import StateDirectoryUnavailableError
from fkf.process import Command, CommandCanceledError, CommandResult
from fkf.query import Window

CONFIG = """\
fkf: 1
name: brain
schema:
  id: {description: Stable identity., cardinality: one}
  time: {description: Event time., cardinality: optional}
  title: {description: Meaningful title., cardinality: optional}
  ticket: {description: Ticket., cardinality: optional, relation: true}
layers: {events: true, index: true, tasks: true, projects: true, wiki: true}
sources:
  synthetic:
    enabled: true
    run: [provider]
    recency: {half_life_days: 7}
    fields: {id: .id, time: .time, title: .title, ticket: .ticket}
  catalog:
    enabled: true
    layer: index
    run: [provider]
    fields: {id: .id, title: .title, ticket: .ticket}
"""


@dataclass
class ExplodingRunner:
    calls: int = 0

    def run(self, command: Command, *, cancel: object | None = None) -> CommandResult:
        del command, cancel
        self.calls += 1
        raise AssertionError("offline context executed a provider command")


def make_base(tmp_path: Path) -> Base:
    root = tmp_path / "brain"
    root.mkdir()
    (root / "fkf.yaml").write_text(CONFIG, encoding="utf-8")
    config = load_config(root)
    return Base(
        config=config,
        store=config.store(),
        runner=ExplodingRunner(),
        now=lambda: datetime(2026, 5, 10, 12, tzinfo=UTC),
    )


def write_event(base: Base, day: str, records: list[Record]) -> None:
    document = build_document(
        base.source("synthetic"),
        records,
        window=day_window(parse_day_in_location(day, UTC)),
        collected_at=base.now(),
    )
    base.write_document(document)


def write_page(base: Base, uri: str, title: str, body: str = "", *, status: str = "") -> None:
    path = base.root / uri
    path.parent.mkdir(parents=True, exist_ok=True)
    frontmatter = f"---\ntitle: {title}\n"
    if status:
        frontmatter += f"status: {status}\n"
    path.write_text(f"{frontmatter}---\n\n# {title}\n\n{body}\n", encoding="utf-8")


def seeded_base(tmp_path: Path) -> Base:
    base = make_base(tmp_path)
    write_event(
        base,
        "2026-04-05",
        [
            {"id": "old1", "time": "2026-04-05T09:00:00Z", "title": "old boundary FK-412", "ticket": "ticket:FK-412"},
            {"id": "noise-old", "time": "2026-04-05T10:00:00Z", "title": "Unrelated archival note"},
        ],
    )
    write_event(
        base,
        "2026-05-09",
        [
            {"id": "fresh1", "time": "2026-05-09T09:00:00Z", "title": "new boundary FK-412", "ticket": "ticket:FK-412"},
            {"id": "noise-new", "time": "2026-05-09T10:00:00Z", "title": "Unrelated fresh note"},
        ],
    )
    write_page(base, "wiki/retrieval-boundary.md", "retrieval boundary", "A durable retrieval boundary.")
    write_page(base, "projects/client.md", "Client migration", "The retrieval boundary is part of the migration.")
    return base


def test_context_scoring_receipt_and_reproducibility(tmp_path: Path) -> None:
    base = make_base(tmp_path)
    write_event(
        base,
        "2026-04-05",
        [
            {"id": "old1", "time": "2026-04-05T09:00:00Z", "title": "old boundary FK-412", "ticket": "ticket:FK-412"},
            {"id": "noise-old", "time": "2026-04-05T10:00:00Z", "title": "Unrelated archival note"},
        ],
    )
    write_event(
        base,
        "2026-05-09",
        [
            {"id": "fresh1", "time": "2026-05-09T09:00:00Z", "title": "new boundary FK-412", "ticket": "ticket:FK-412"},
            {"id": "noise-new", "time": "2026-05-09T10:00:00Z", "title": "Unrelated fresh note"},
        ],
    )
    request = ContextRequest(
        query="FK-412 boundary",
        budget=4096,
        explain=True,
        window=Window("2026-04-01", "2026-05-10"),
    )

    first = build_context(base, request)
    second = build_context(base, request)

    by_id = {item.uri.rpartition("#")[2]: item for item in first.items}
    assert by_id["old1"].score == 150
    assert by_id["fresh1"].score == 164
    assert all(sum(reason.points for reason in item.reasons) == item.score for item in first.items)
    assert first.receipt.ranking_version == RANKING_VERSION == 10
    assert first.receipt.input_digest
    assert first.receipt.encoded_tokens <= first.receipt.budget
    assert dumps(first) == dumps(second)
    assert isinstance(base.runner, ExplodingRunner)
    assert base.runner.calls == 0


def test_input_digest_preserves_the_empty_repeatable_pin_collection(tmp_path: Path) -> None:
    base = seeded_base(tmp_path)
    request = ContextRequest(
        "retrieval",
        budget=1600,
        pins=(),
        delivery_format="json",
        as_of="2026-09-06",
    )

    # Go's public CLI materializes an omitted repeatable --pin as [] rather than null.
    assert (
        context_module._input_digest(  # noqa: SLF001 - exact internal receipt-domain contract
            base, request, (), (), (), "abc"
        )
        == "a449f87e7c709923"
    )


def test_context_pin_is_exact_and_capped(tmp_path: Path) -> None:
    base = seeded_base(tmp_path)
    request = ContextRequest(query="FK-412", budget=900, pins=("wiki/retrieval-boundary.md",), explain=True)
    pack = build_context(base, request)

    pinned = next(item for item in pack.items if item.pinned)
    assert pinned.uri == "wiki/retrieval-boundary.md"
    assert pinned.tokens <= pack.receipt.budget // 3
    with pytest.raises(ConfigError, match="unknown pin"):
        build_context(base, ContextRequest(query="FK-412", pins=("retrieval-boundary",)))


@pytest.mark.parametrize("delivery", ["json", "jsonl", "text"])
def test_context_budget_covers_exact_delivered_bytes(tmp_path: Path, delivery: str) -> None:
    base = seeded_base(tmp_path)
    request = ContextRequest(query="retrieval boundary", budget=700, delivery_format=delivery, explain=True)
    try:
        pack = build_context(base, request)
    except ContextBudgetError as error:
        pack = build_context(base, replace(request, budget=error.minimum))
    encoded = render_context_bytes(pack, delivery)
    if delivery == "text":
        assert encoded == render_context_text(pack).encode()
    else:
        assert encoded == dumps(pack, indent=delivery == "json", newline=True)
    assert (len(encoded) + 3) // 4 == pack.receipt.encoded_tokens
    assert pack.receipt.encoded_tokens <= pack.receipt.budget


def test_context_lexical_index_matches_scan_semantics(tmp_path: Path) -> None:
    base = seeded_base(tmp_path)
    request = ContextRequest(query="retrieval boundary", budget=4096, explain=True)
    scanned = build_context(base, request)
    build_lexical_index(base)
    indexed = build_context(base, request)

    assert [(item.uri, item.score) for item in indexed.items] == [(item.uri, item.score) for item in scanned.items]
    assert indexed.receipt.input_digest == scanned.receipt.input_digest
    assert indexed.receipt.index.used
    assert not scanned.receipt.index.used


def test_context_defers_candidate_semantics_until_snapshot(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    base = make_base(tmp_path)
    write_event(
        base,
        "2026-05-09",
        [
            {"id": "first", "time": "2026-05-09T09:00:00Z", "title": "first needle"},
            {"id": "second", "time": "2026-05-09T10:00:00Z", "title": "second needle"},
        ],
    )
    original = context_module._candidate_semantic_digest  # noqa: SLF001 - performance regression seam
    calls = 0

    def counted(candidate: context_module.ContextItem) -> str:
        nonlocal calls
        calls += 1
        return original(candidate)

    monkeypatch.setattr(context_module, "_candidate_semantic_digest", counted)

    request = ContextRequest(query="needle", budget=4096)
    pack = build_context(base, request)

    assert calls == 0
    assert all(not item.semantic_digest for item in pack.items)

    snapshot = build_context(base, replace(request, save_snapshot=True))

    assert calls == 2
    assert all(not item.semantic_digest for item in snapshot.items)


def test_context_index_hydrates_each_document_once(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    base = make_base(tmp_path)
    write_event(
        base,
        "2026-05-09",
        [
            {"id": "first", "time": "2026-05-09T09:00:00Z", "title": "first needle"},
            {"id": "second", "time": "2026-05-09T10:00:00Z", "title": "second needle"},
        ],
    )
    request = ContextRequest(query="needle", budget=4096, explain=True)
    scanned = build_context(base, request)
    build_lexical_index(base)
    original = Base.read_document
    reads: list[str] = []

    def counted(selected: Base, relative: str) -> Document:
        if selected is base:
            reads.append(relative)
        return original(selected, relative)

    monkeypatch.setattr(Base, "read_document", counted)

    indexed = build_context(base, request)

    assert reads == ["events/2026-05-09/synthetic.json"]
    assert [(item.uri, item.score) for item in indexed.items] == [(item.uri, item.score) for item in scanned.items]
    assert indexed.receipt.input_digest == scanned.receipt.input_digest
    assert indexed.receipt.index.used


def test_context_since_receipt_returns_only_new_or_changed_candidates(tmp_path: Path) -> None:
    base = seeded_base(tmp_path)
    request = ContextRequest(query="retrieval boundary", budget=4096, save_snapshot=True)
    original = build_context(base, request)
    unchanged = build_context(
        base,
        ContextRequest(query="retrieval boundary", budget=4096, since_receipt=original.receipt.input_digest),
    )
    assert unchanged.items == ()
    assert "nothing changed" in unchanged.receipt.warning

    write_page(base, "wiki/retrieval-boundary.md", "retrieval boundary", "Changed durable retrieval boundary.")
    changed = build_context(
        base,
        ContextRequest(query="retrieval boundary", budget=4096, since_receipt=original.receipt.input_digest),
    )
    assert [item.uri for item in changed.items] == ["wiki/retrieval-boundary.md"]


@pytest.mark.parametrize("through_alias", [False, True])
def test_context_receipt_state_must_stay_outside_the_physical_base(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
    through_alias: bool,
) -> None:
    base = seeded_base(tmp_path)
    root = base.root
    state_root = root
    if through_alias:
        state_root = tmp_path / "base-alias"
        state_root.symlink_to(root, target_is_directory=True)
    monkeypatch.setenv("XDG_STATE_HOME", str(state_root / "machine-state"))

    with pytest.raises(StateDirectoryUnavailableError, match="outside the base"):
        build_context(
            base,
            ContextRequest(query="retrieval boundary", budget=4096, save_snapshot=True),
        )

    assert not (root / "machine-state").exists()


def test_context_snapshot_write_retightens_private_state_directories(tmp_path: Path) -> None:
    base = seeded_base(tmp_path)
    request = ContextRequest(query="retrieval boundary", budget=4096, save_snapshot=True)
    build_context(base, request)
    snapshot = next((tmp_path / "home" / "state" / "fkf" / "receipts").glob("*/*.json.gz"))
    directories = (snapshot.parent.parent.parent, snapshot.parent.parent, snapshot.parent)
    for directory in directories:
        directory.chmod(0o755)

    build_context(base, request)

    assert all(directory.stat().st_mode & 0o777 == 0o700 for directory in directories)


def test_context_expands_one_complete_graph_hop(tmp_path: Path) -> None:
    base = seeded_base(tmp_path)
    build_graph(base)

    pack = build_context(base, ContextRequest(query="old1", budget=4096, expand=True, explain=True))

    reached = next(item for item in pack.items if item.uri.endswith("#fresh1"))
    assert reached.score == 20
    assert reached.reasons == (Reason("join-expansion", 20, "one hop through ticket:FK-412"),)
    assert len(pack.graph_generation_sha256) == 64


def test_context_cancellation_and_temporal_query(tmp_path: Path) -> None:
    base = seeded_base(tmp_path)
    canceled = Event()
    canceled.set()
    with pytest.raises(CommandCanceledError, match="canceled"):
        build_context(base, ContextRequest(query="retrieval today"), cancel=canceled)

    pack = build_context(base, ContextRequest(query="show me retrieval today", budget=4096))
    assert pack.query == "retrieval"
    assert pack.receipt.window == Window("2026-05-10", "2026-05-10", "today")


def test_context_forwards_the_invocation_event_to_the_lexical_reader(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    base = seeded_base(tmp_path)
    build_lexical_index(base)
    cancel = Event()

    def cancel_lexical_read(*_args: object, **kwargs: object) -> object:
        assert kwargs["cancel"] is cancel_event
        raise CommandCanceledError("command canceled")

    cancel_event = cancel
    monkeypatch.setattr(context_module, "query_context_lexical_index", cancel_lexical_read)

    with pytest.raises(CommandCanceledError, match="command canceled"):
        build_context(base, ContextRequest(query="retrieval"), cancel=cancel)
