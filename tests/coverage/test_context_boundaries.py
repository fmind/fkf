from __future__ import annotations

import gzip
from collections.abc import Callable
from dataclasses import replace
from datetime import UTC, datetime
from pathlib import Path
from typing import Any

import pytest
from tests.services.test_context import seeded_base, write_event

import fkf.context as context_module
from fkf.context import ContextItem, ContextPack, ContextRequest, DroppedItem, Reason, Receipt
from fkf.errors import OperationalError
from fkf.graph import IdentityResolver
from fkf.jsoncodec import dumps
from fkf.lexical import LexicalIndexUse, LexicalSupersession, LexicalTermAnalysis, LexicalTermSegment
from fkf.markdown import Page
from fkf.query import Window
from fkf.store import Layer


class _PrivateContext:
    def __getattr__(self, name: str) -> Any:
        return vars(context_module)[f"_{name}"]


CONTEXT_INTERNAL = _PrivateContext()


def make_pack(*, budget: int = 800, candidates: int = 0, terms: tuple[str, ...] = ()) -> ContextPack:
    receipt = Receipt(
        base="brain",
        query="needle",
        window=Window("2026-05-01", "2026-05-10"),
        budget=budget,
        format="json",
        candidates=candidates,
        terms=terms,
        as_of="2026-05-10",
    )
    return ContextPack("needle", (), receipt)


def item(uri: str, **changes: object) -> ContextItem:
    candidate = ContextItem(uri=uri, kind="record", title=uri, score=20)
    for name, value in changes.items():
        setattr(candidate, name, value)
    return candidate


def snapshot_payload(
    *, physical: str, digest: str, entries: list[dict[str, object]] | None = None
) -> dict[str, object]:
    return {
        "version": 3,
        "base": physical,
        "input_digest": digest,
        "request_key": "a" * 64,
        "query": "needle",
        "window": {"since": "2026-05-01", "until": "2026-05-10", "derived_from": ""},
        "as_of": "2026-05-10",
        "entries": entries or [],
    }


def compressed(value: object) -> bytes:
    return gzip.compress(dumps(value), mtime=0)


def test_request_normalization_owns_defaults_windows_and_ambiguity(tmp_path: Path) -> None:
    base = seeded_base(tmp_path)

    normalized, now = CONTEXT_INTERNAL.normalize_request(
        base,
        ContextRequest(query="Please show me retrieval", delivery_format=""),
    )
    assert normalized.query == "retrieval"
    assert normalized.budget == context_module.DEFAULT_BUDGET
    assert normalized.delivery_format == "json"
    assert normalized.window == Window("2026-04-05", "2026-05-10")
    assert normalized.as_of == "2026-05-10"
    assert now == base.now()

    until, _ = CONTEXT_INTERNAL.normalize_request(
        base,
        ContextRequest(query="needle", window=Window(until="yesterday")),
    )
    assert until.window == Window("2026-04-10", "2026-05-09", "--until")

    since, _ = CONTEXT_INTERNAL.normalize_request(
        base,
        ContextRequest(query="needle", window=Window(since="today")),
    )
    assert since.window == Window("2026-05-10", "")

    newest, _ = CONTEXT_INTERNAL.normalize_request(base, ContextRequest(query="needle last"))
    assert newest.query == "needle"
    assert newest.newest

    with pytest.raises(ValueError, match="ambiguous temporal inputs"):
        CONTEXT_INTERNAL.normalize_request(
            base,
            ContextRequest(query="needle today", window=Window(since="2026-05-01")),
        )
    with pytest.raises(ValueError, match="needs a query"):
        CONTEXT_INTERNAL.normalize_request(base, ContextRequest(query="please show me"))
    with pytest.raises(ValueError, match="is not json, jsonl, or text"):
        CONTEXT_INTERNAL.normalize_request(base, ContextRequest(query="needle", delivery_format="yaml"))


def test_lexical_and_temporal_primitives_are_closed_and_deterministic() -> None:
    error = context_module.ContextBudgetError(1, 42)
    assert (error.requested, error.minimum) == (1, 42)
    assert "42" in str(error)

    assert CONTEXT_INTERNAL.lower("Aİ") == "aİ"
    assert CONTEXT_INTERNAL.terms(" Alpha,FK-412 / repo:x! ") == ("alpha", "fk-412", "/", "repo:x")
    assert CONTEXT_INTERNAL.trim_query_scaffolding("Please tell me the last incident") == "last incident"
    assert CONTEXT_INTERNAL.truncate("  tiny  ", 10) == "tiny"
    assert CONTEXT_INTERNAL.truncate("abcdefgh", 4) == "abcd…"
    assert CONTEXT_INTERNAL.canonical_time("2026-05-10T14:00:00+02:00") == "2026-05-10T12:00:00Z"

    assert CONTEXT_INTERNAL.rarity(0, 1) == 0
    assert CONTEXT_INTERNAL.rarity(1, 1) == 1
    assert CONTEXT_INTERNAL.rarity(10, 6) == 0
    assert CONTEXT_INTERNAL.rarity(10_000_000, 1) == context_module.MAX_RARITY_FACTOR
    assert CONTEXT_INTERNAL.recency_bonus("", datetime(2026, 5, 10, tzinfo=UTC), 7) == (0, -1)
    assert CONTEXT_INTERNAL.recency_bonus("invalid", datetime(2026, 5, 10, tzinfo=UTC), 7) == (0, -1)
    assert CONTEXT_INTERNAL.recency_bonus("2026-05-11", datetime(2026, 5, 10, tzinfo=UTC), 7) == (0, -1)
    assert CONTEXT_INTERNAL.recency_bonus("2026-05-10", datetime(2026, 5, 10, tzinfo=UTC), 7) == (15, 0)

    assert CONTEXT_INTERNAL.kind_for_uri("wiki/x.md") == "wiki"
    assert CONTEXT_INTERNAL.kind_for_uri("events/2026-05-10/x.json") == "events"
    assert CONTEXT_INTERNAL.kind_for_uri("foreign/x") == "index"
    assert CONTEXT_INTERNAL.date_for_task_uri("tasks/2026-05-10/focus/x.md") == "2026-05-10"
    assert CONTEXT_INTERNAL.date_for_task_uri("tasks/x.md") == ""
    assert CONTEXT_INTERNAL.render_window(Window()) == "all"
    assert CONTEXT_INTERNAL.render_window(Window(since="2026-05-01")) == "2026-05-01.."
    assert CONTEXT_INTERNAL.render_window(Window(until="2026-05-10")) == "..2026-05-10"
    assert CONTEXT_INTERNAL.qualified("brain", "wiki/x.md") == "fkf://brain/wiki/x.md"
    assert CONTEXT_INTERNAL.qualified("brain", "fkf://other/wiki/x.md") == "fkf://other/wiki/x.md"


def test_page_and_record_candidates_preserve_relations_and_reject_missing_identity(tmp_path: Path) -> None:
    base = seeded_base(tmp_path)
    page = Page(
        uri="wiki/policy.md",
        slug="policy",
        type="decision",
        title="Policy",
        description="Retrieval rules",
        status="active",
        date="2026-05-09",
        valid_from="2026-05-01",
        tags=("retrieval",),
        relations={
            "ticket": ("ticket:FK-412", "../wiki/retrieval-boundary.md"),
            "unknown": ("repo:github.com/fmind/fkf",),
        },
        frontmatter={"tags": ["retrieval"], "relations": {}},
        body="Durable policy body.",
    )
    candidate = CONTEXT_INTERNAL.page_candidate(page, Layer.WIKI, base.config.schema)
    assert candidate.tags == ("retrieval",)
    assert candidate.fields == page.relations
    assert candidate.relation_fields == frozenset({"ticket"})
    assert "ticket:fk-412" in candidate.identifier_keys
    assert "../wiki/retrieval-boundary.md" in candidate.identifier_keys
    assert candidate.validity_rank == "2026-05-01"
    assert candidate.supersedes == ()
    assert candidate.semantic_digest == ""
    assert candidate.tokens == 0

    write_event(
        base,
        "2026-05-10",
        [{"id": "odd", "time": "2026-05-10T10:00:00Z", "title": "Odd", "ticket": "repo:x/y"}],
    )
    document = base.read_document("events/2026-05-10/synthetic.json")
    document.records[0]["time"] = "not-time"
    document.records[0]["title"] = ""
    record = CONTEXT_INTERNAL.record_candidate(document, document.records[0], base.source("synthetic").schema)
    assert record.time == ""
    assert "y" in record.identifier_keys
    assert record.semantic_digest == ""
    assert record.tokens == 0

    candidate.semantic_digest = "stale"
    candidate.tokens = 1
    record.semantic_digest = "stale"
    record.tokens = 1
    CONTEXT_INTERNAL.canonicalize((candidate, record), IdentityResolver.load(base))
    assert candidate.semantic_digest == ""
    assert candidate.tokens == 0
    assert record.semantic_digest == ""
    assert record.tokens == 0

    assert CONTEXT_INTERNAL.candidate_digest(candidate)
    assert candidate.semantic_digest == ""

    missing = dict(document.records[0])
    missing.pop("id")
    with pytest.raises(OperationalError, match="has no identity URI"):
        CONTEXT_INTERNAL.record_candidate(document, missing, base.source("synthetic").schema)


def test_candidate_collapsing_prefers_complete_recent_evidence() -> None:
    page = item("wiki/x.md", kind="wiki")
    untitled = item("events/a.json#untitled", source="", title="")
    old = item(
        "events/a.json#old",
        source="provider",
        title="Same Run",
        time="bad-time",
        segments=(("title", "needle", 1),),
    )
    fresh = item(
        "events/b.json#fresh",
        source="provider",
        title="same run",
        time="2026-05-10T10:00:00Z",
        segments=(("title", "needle", 1),),
    )
    assert CONTEXT_INTERNAL.chronology(old) == "bad-time"
    old.time = "2026-05-01T10:00:00Z"
    runs = CONTEXT_INTERNAL.collapse_runs((page, untitled, old, fresh))
    kept = next(candidate for candidate in runs if candidate.source == "provider")
    assert kept.uri.endswith("#fresh")
    assert kept.collapsed_uris == ("events/a.json#old", "events/b.json#fresh")
    assert kept.count == 2

    other = item("wiki/other.md", kind="wiki")
    sparse = item("index/a.json#z", url="https://example.test/x", date="2026-05-01")
    complete = item(
        "index/b.json#a",
        url="https://example.test/x",
        date="2026-05-02",
        body_available=True,
        fields={"topic": ("needle",)},
        segments=(("topic", "needle", 1),),
    )
    resources = CONTEXT_INTERNAL.collapse_resources((other, sparse, complete), ("needle",))
    resource = next(candidate for candidate in resources if candidate.kind == "record")
    assert resource.uri == "index/b.json#a"
    assert resource.count == 2
    assert resource.collapsed_uris == ("index/a.json#z", "index/b.json#a")


def test_supersession_selects_one_stable_winner() -> None:
    original = item("wiki/original.md", kind="wiki", validity_rank="2026-01-01")
    older = item(
        "wiki/z-older.md",
        kind="wiki",
        validity_rank="2026-04-01",
        supersedes=("wiki/original.md",),
    )
    winner = item(
        "wiki/a-winner.md",
        kind="wiki",
        validity_rank="2026-05-01",
        supersedes=("wiki/original.md", "wiki/a-winner.md"),
    )
    tie = item(
        "wiki/b-tie.md",
        kind="wiki",
        validity_rank="2026-05-01",
        supersedes=("wiki/original.md",),
    )
    CONTEXT_INTERNAL.apply_supersedes((original, older, winner, tie))
    assert original.superseded_by == winner.uri
    assert older.superseded_by == winner.uri
    assert tie.superseded_by == winner.uri

    CONTEXT_INTERNAL.apply_indexed_supersessions(
        (original, tie),
        {original.uri: LexicalSupersession(by="wiki/new.md", rank="2026-06-01")},
    )
    assert (original.superseded_by, original.superseded_rank) == ("wiki/new.md", "2026-06-01")


def test_scoring_covers_identifiers_weighting_policy_recency_and_penalties(tmp_path: Path) -> None:
    base = seeded_base(tmp_path)
    exact = item(
        "wiki/index.md",
        kind="wiki",
        source="synthetic",
        date="2026-05-10",
        title="alpha boundary",
        status="deprecated",
        created_evidence=True,
        direct_identifiers=frozenset({"alpha"}),
        identifier_keys=frozenset({"boundary"}),
        segments=(("title", "alpha boundary", 3),),
        body="x" * 100 + " alpha body evidence " + "y" * 100,
    )
    weighted = item(
        "wiki/weighted.md",
        kind="wiki",
        title="Weighted",
        segments=(("title", "alpha", 4), ("body", "alpha", 1)),
        default_excluded="category:received",
    )
    superseded = item(
        "wiki/old.md",
        kind="wiki",
        title="alpha",
        segments=(("title", "alpha", 1),),
        superseded_by="wiki/new.md",
    )
    CONTEXT_INTERNAL.score_candidates(
        base,
        (exact, weighted, superseded),
        "alpha boundary",
        ("alpha", "boundary"),
        10,
        base.now(),
    )

    names = {reason.reason for reason in exact.reasons}
    assert {"exact-identifier", "exact-phrase", "created-evidence", "recency", "superseded", "navigation-page"} <= names
    assert exact.excerpt.startswith("…")
    assert exact.excerpt.endswith("…")
    assert any(reason.reason == "term" for reason in weighted.reasons)
    assert any(reason.reason == "superseded" for reason in superseded.reasons)
    assert not CONTEXT_INTERNAL.candidate_allowed(weighted, ("alpha",))
    assert CONTEXT_INTERNAL.candidate_allowed(weighted, ("received",))
    assert CONTEXT_INTERNAL.policy_explicit(weighted, ("category:received",))

    analysis = LexicalTermAnalysis(True, 0, 4, (LexicalTermSegment("title", 4, 1),))
    assert CONTEXT_INTERNAL.weighted_points("alpha", 2, analysis)[0] == 80
    assert CONTEXT_INTERNAL.weighted_points("alpha", 0, LexicalTermAnalysis()) == (0, "")
    assert CONTEXT_INTERNAL.body_excerpt("no match", ("needle",)) == ""


@pytest.mark.parametrize(
    ("attribute", "left", "right", "newest"),
    [
        ("date", "2026-05-10", "", True),
        ("explicit_identity", True, False, True),
        ("direct_identity", True, False, True),
        ("matched_identity", True, False, True),
        ("match_weight", 2, 1, True),
        ("matched_terms", 2, 1, True),
        ("direct_identity", True, False, False),
        ("matched_identity", True, False, False),
        ("match_weight", 2, 1, False),
        ("matched_terms", 2, 1, False),
        ("score", 30, 20, False),
        ("time", "2026-05-10T12:00:00Z", "", False),
    ],
)
def test_candidate_ordering_applies_each_tie_break(attribute: str, left: object, right: object, newest: bool) -> None:
    first = item("record:a")
    second = item("record:b")
    setattr(first, attribute, left)
    setattr(second, attribute, right)
    assert CONTEXT_INTERNAL.candidate_cmp(first, second, newest=newest) < 0


def test_selection_reports_policy_floor_source_and_budget_drops() -> None:
    pack = make_pack(budget=900, candidates=10, terms=("needle",))
    candidates = [item(f"index/source.json#{index}", source="same", score=30) for index in range(7)]
    candidates.extend(
        [
            item("wiki/page.md", kind="wiki", score=25),
            item("index/low.json#x", source="other", score=1),
            item("index/private.json#x", source="other", score=30, default_excluded="visibility:private"),
        ]
    )
    request = ContextRequest(query="needle", budget=900, delivery_format="json")
    CONTEXT_INTERNAL.select(pack, candidates, request)
    reasons = {drop.reason for drop in pack.receipt.dropped}
    assert "wiki/page.md" in {candidate.uri for candidate in pack.items}
    assert {"below-floor", "default-excluded"} <= reasons
    assert len([candidate for candidate in pack.items if candidate.source == "same"]) <= 4

    rejected = make_pack(budget=200, candidates=1)
    huge_pin = item("wiki/huge.md", kind="wiki", excerpt="x" * 5_000)
    CONTEXT_INTERNAL.select(
        rejected,
        (huge_pin,),
        ContextRequest(query="needle", budget=200, pins=(huge_pin.uri,), delivery_format="json"),
    )
    assert rejected.items == ()
    assert rejected.receipt.rejected_pins == (huge_pin.uri,)
    assert rejected.receipt.dropped[0].pinned
    assert "budget" in rejected.receipt.warning

    empty = make_pack(budget=200)
    CONTEXT_INTERNAL.select(empty, (), ContextRequest(query="needle", budget=200))
    assert "no candidates" in empty.receipt.warning


def test_selected_page_mix_source_share_and_omitted_receipts() -> None:
    records = [item(f"record:{index}", source="same", score=100 - index) for index in range(7)]
    page = item("wiki/page.md", kind="wiki", score=1)
    selected = CONTEXT_INTERNAL.sort_selected((*records, page), newest=False)
    assert page in selected[:5]

    drops: list[DroppedItem] = []
    CONTEXT_INTERNAL.enforce_source_share(selected, drops)
    assert drops
    assert all(drop.reason == "source-cap" for drop in drops)

    pack = make_pack(budget=400, candidates=20)
    pack.receipt.dropped = (DroppedItem("already", "budget"),)
    CONTEXT_INTERNAL.append_omitted(pack, tuple(f"omitted:{index}" for index in range(20)))
    assert pack.receipt.dropped_total == 21
    assert len(pack.receipt.dropped) == CONTEXT_INTERNAL.dropped_cap(400)
    unchanged = pack.receipt.dropped
    CONTEXT_INTERNAL.append_omitted(pack, ())
    assert pack.receipt.dropped == unchanged


def test_text_rendering_exposes_receipt_provenance_and_structured_fields() -> None:
    assert context_module.render_context_text(None) == ""
    pack = make_pack(budget=800, candidates=2)
    pack.receipt.format = "text"
    pack.receipt.encoded_tokens = 100
    pack.receipt.newest_event_day = "2026-05-10"
    pack.receipt.stale_days = 2
    pack.receipt.input_digest = "a" * 16
    pack.receipt.dropped_total = 3
    pack.receipt.index = LexicalIndexUse(path="index/cache", reason="stale")
    pack.receipt.since_receipt = "b" * 16
    pack.receipt.changed = 1
    pack.receipt.unharvested_bullets = 2
    pack.items = (
        item(
            "wiki/page.md",
            kind="wiki",
            date="",
            title="",
            source="provider",
            status="active",
            tags=("one", "two"),
            fields={"ticket": ("ticket:1",)},
            pinned=True,
            count=2,
            reasons=(Reason("term", 20, "needle"),),
        ),
    )
    pack.receipt.selected = 1

    rendered = context_module.render_context_text(pack)
    assert "fkf://brain/wiki/page.md" in rendered
    assert "source=provider" in rendered
    assert "why=term:+20(needle)" in rendered
    assert "index index/cache fallback=stale" in rendered
    assert "delta since" in rendered
    assert "unharvested" in rendered
    assert context_module.render_context_bytes(pack, "text") == rendered.encode()
    with pytest.raises(ValueError, match="is not json, jsonl, or text"):
        context_module.render_context_bytes(pack, "yaml")


@pytest.mark.parametrize(
    "mutate",
    [
        lambda _value: [],
        lambda value: {**value, "extra": True},
        lambda value: {**value, "window": [], "entries": []},
        lambda value: {**value, "window": {"unknown": "x"}},
        lambda value: {**value, "entries": ["bad"]},
        lambda value: {**value, "entries": [{"uri": 1, "sha256": "a" * 64}]},
        lambda value: {**value, "query": 1},
        lambda value: {**value, "version": 2},
        lambda value: {**value, "entries": [{"uri": "", "sha256": "a" * 64}]},
        lambda value: {**value, "entries": [{"uri": "wiki/x.md", "sha256": "A" * 64}]},
        lambda value: {
            **value,
            "entries": [
                {"uri": "wiki/x.md", "sha256": "a" * 64},
                {"uri": "wiki/x.md", "sha256": "b" * 64},
            ],
        },
    ],
)
def test_snapshot_decoder_rejects_ambiguous_manifests(
    mutate: Callable[[dict[str, object]], object],
) -> None:
    physical = "/base"
    digest = "c" * 16
    value = snapshot_payload(physical=physical, digest=digest)
    changed = mutate(value)
    with pytest.raises(ValueError, match="snapshot"):
        CONTEXT_INTERNAL.decode_snapshot(compressed(changed), physical, digest)


def test_snapshot_decoder_rejects_transport_limits_and_accepts_canonical_state(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    with pytest.raises(ValueError, match="invalid gzip"):
        CONTEXT_INTERNAL.decode_snapshot(b"not-gzip", "/base", "c" * 16)

    value = snapshot_payload(
        physical="/base",
        digest="c" * 16,
        entries=[{"uri": "wiki/x.md", "sha256": "a" * 64}],
    )
    monkeypatch.setattr(
        gzip,
        "decompress",
        lambda _data: (_ for _ in ()).throw(AssertionError("receipt decoding must be bounded while inflating")),
    )
    snapshot = CONTEXT_INTERNAL.decode_snapshot(compressed(value), "/base", "c" * 16)
    assert snapshot.entries[0].uri == "wiki/x.md"

    monkeypatch.setattr(context_module, "_MAX_SNAPSHOT_DECODED", 1)
    with pytest.raises(ValueError, match="decoded manifest exceeds"):
        CONTEXT_INTERNAL.decode_snapshot(compressed(value), "/base", "c" * 16)


def test_snapshot_storage_is_bounded_idempotent_and_symlink_safe(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setenv("XDG_STATE_HOME", str(tmp_path / "state"))
    (tmp_path / "base").mkdir()
    base = seeded_base(tmp_path / "base")
    request = ContextRequest(
        query="needle",
        window=Window("2026-05-01", "2026-05-10"),
        budget=800,
        as_of="2026-05-10",
    )
    candidate = item("wiki/x.md", kind="wiki")

    with pytest.raises(ValueError, match="invalid context input digest"):
        CONTEXT_INTERNAL.snapshot_path(base, "INVALID")
    with pytest.raises(ValueError, match="not available"):
        CONTEXT_INTERNAL.load_snapshot(base, "a" * 16)

    CONTEXT_INTERNAL.store_snapshot(base, request, "a" * 16, (candidate,))
    loaded = CONTEXT_INTERNAL.load_snapshot(base, "a" * 16)
    assert loaded.entries[0].uri == candidate.uri
    CONTEXT_INTERNAL.store_snapshot(base, request, "a" * 16, (candidate,))

    duplicate = (candidate, replace(candidate))
    with pytest.raises(OperationalError, match="duplicate candidate URI"):
        CONTEXT_INTERNAL.store_snapshot(base, request, "b" * 16, duplicate)

    monkeypatch.setattr(context_module, "_SNAPSHOT_RETENTION", 1)
    CONTEXT_INTERNAL.store_snapshot(base, request, "b" * 16, (item("wiki/y.md", kind="wiki"),))
    directory, _physical = CONTEXT_INTERNAL.snapshot_directory(base)
    assert len(tuple(directory.glob("*.json.gz"))) == 1

    symlink_path, _ = CONTEXT_INTERNAL.snapshot_path(base, "c" * 16)
    symlink_path.symlink_to(directory / "missing")
    with pytest.raises(OperationalError, match="symbolic link"):
        CONTEXT_INTERNAL.store_snapshot(base, request, "c" * 16, (candidate,))


def test_snapshot_delta_binds_query_and_candidate_content(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("XDG_STATE_HOME", str(tmp_path / "state"))
    (tmp_path / "base").mkdir()
    base = seeded_base(tmp_path / "base")
    original = ContextRequest(
        query="needle",
        window=Window("2026-05-01", "2026-05-10"),
        budget=800,
        as_of="2026-05-10",
    )
    candidate = item("wiki/x.md", kind="wiki")
    CONTEXT_INTERNAL.store_snapshot(base, original, "a" * 16, (candidate,))

    with pytest.raises(ValueError, match="lowercase 16-character"):
        CONTEXT_INTERNAL.delta_candidates(base, replace(original, since_receipt="BAD"), (candidate,))
    with pytest.raises(ValueError, match="belongs to context query"):
        CONTEXT_INTERNAL.delta_candidates(
            base,
            replace(original, query="different", since_receipt="a" * 16),
            (candidate,),
        )
    assert (
        CONTEXT_INTERNAL.delta_candidates(
            base,
            replace(original, since_receipt="a" * 16),
            (candidate,),
        )
        == []
    )
    changed = replace(candidate, title="changed")
    assert CONTEXT_INTERNAL.delta_candidates(
        base,
        replace(original, since_receipt="a" * 16),
        (changed,),
    ) == [changed]


def test_context_retries_a_changing_generation_then_fails_closed(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    base = seeded_base(tmp_path)
    monkeypatch.setattr(context_module, "lexical_inputs_match", lambda *_args, **_kwargs: False)
    with pytest.raises(OperationalError, match="inputs kept changing"):
        context_module.build_context(base, ContextRequest(query="retrieval", budget=1_500))
