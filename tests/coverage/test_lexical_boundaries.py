from __future__ import annotations

# This module deliberately exercises the cache's private wire decoder boundary.
# ruff: noqa: SLF001
import base64
import json
import os
from dataclasses import replace
from datetime import UTC, datetime
from pathlib import Path

import pytest

import fkf.lexical as lexical
from fkf.base import Base
from fkf.bodies import cache_body
from fkf.config import load_config
from fkf.documents import Document, Record, build_document, day_window, parse_day_in_location
from fkf.find import FindFilter, find
from fkf.markdown import Page
from fkf.process import Cancellation, Command, CommandResult
from fkf.query import Window
from fkf.store import Layer

CONFIG = """\
fkf: 1
name: lexical-boundaries
schema:
  id: {description: Stable identity., cardinality: one, weight: 10}
  time: {description: Event time., cardinality: one}
  title: {description: Display title., cardinality: optional, weight: 5}
  url: {description: Provider URL., cardinality: optional}
  repo: {description: Repository identity., cardinality: optional, relation: true}
  category: {description: Evidence direction., cardinality: optional}
  visibility: {description: Evidence visibility., cardinality: optional}
  topic: {description: Search topic., cardinality: optional}
  supersedes: {description: Replaced knowledge., cardinality: many, relation: true}
layers: {events: true, index: true, tasks: true, projects: true, wiki: true}
identities:
  acme:
    canonical: org:github.com/acme
    aliases: [acme, org:legacy/acme]
    kind: organization
sources:
  activity:
    enabled: true
    run: [provider]
    fields:
      id: .id
      time: .time
      title: .title
      url: .url
      repo: .repo
      category: .category
      visibility: .visibility
      topic: .topic
    body: [provider, body, "{{id}}"]
    bodies: cache
  catalog:
    enabled: true
    layer: index
    run: [provider]
    fields: {id: .id, title: .title, repo: .repo, topic: .topic}
"""


class OfflineRunner:
    def run(self, command: Command, *, cancel: Cancellation | None = None) -> CommandResult:
        del command, cancel
        raise AssertionError("lexical reads must not execute provider commands")


def make_base(tmp_path: Path) -> Base:
    root = tmp_path / "base"
    root.mkdir(parents=True)
    (root / "fkf.yaml").write_text(CONFIG, encoding="utf-8")
    config = load_config(root)
    return Base(
        config=config,
        store=config.store(),
        runner=OfflineRunner(),
        now=lambda: datetime(2026, 9, 6, 8, 9, 10, 456789, tzinfo=UTC),
    )


def write_event(base: Base, day: str, records: list[Record]) -> Document:
    document = build_document(
        base.source("activity"),
        records,
        window=day_window(parse_day_in_location(day, UTC)),
        collected_at=base.now(),
    )
    base.write_document(document)
    return document


def write_index(base: Base, records: list[Record]) -> Document:
    document = build_document(base.source("catalog"), records, collected_at=base.now())
    base.write_document(document)
    return document


def write_page(base: Base, uri: str, body: str) -> None:
    path = base.root / uri
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(body, encoding="utf-8")


def candidate(*, identifier: str = "prefixabc/defsuffix") -> lexical.LexicalCandidate:
    value = lexical.LexicalCandidate(uri="wiki/page.md", kind="wiki", title="Boundary")
    value.add_segment("id", identifier, 10)
    value.add_segment("repo", "abc/def", 1)
    value.add_identifier("wiki/page.md")
    value.add_related_identifier("org:github.com/acme")
    value.add_entity_identifier("person:email/alice@example.test")
    return value


def test_page_candidate_resolves_authored_relation_targets(tmp_path: Path) -> None:
    base = make_base(tmp_path)
    page = Page(
        uri="projects/client.md",
        slug="client",
        title="Client",
        relations={"supersedes": ("../wiki/old.md",)},
        frontmatter={"relations": {}},
    )
    projected = lexical._page_candidate(page, Layer.PROJECTS, base.config.schema)

    assert "../wiki/old.md" in projected.identifier_keys
    assert "wiki/old.md" not in projected.identifier_keys
    assert projected.supersedes == ("../wiki/old.md",)


def test_candidate_terms_bounds_and_receipt_parsing_cover_identifier_edges() -> None:
    value = candidate()
    value.add_segment("ignored", "   ", 0)
    value.add_identifier("  ")
    value.add_entity_identifier("not an entity")
    value.identity_terms.add("org:github.com/acme")

    scores = lexical.lexical_term_scores(value)
    score = scores["abc/def"]
    value.identifier_bounds = lexical.lexical_candidate_identifier_bounds(value)

    assert score.analysis.matched
    assert score.analysis.max_weight == 1
    assert score.analysis.segments[0].field == "repo"
    assert not lexical.lexical_term_score_is_complete(value, "abc/def", score.analysis)

    unrelated = candidate(identifier="unrelated/identifier")
    unrelated_score = lexical.lexical_term_scores(unrelated)["abc/def"]
    unrelated.identifier_bounds = lexical.lexical_candidate_identifier_bounds(unrelated)
    assert lexical.lexical_term_score_is_complete(unrelated, "abc/def", unrelated_score.analysis)
    assert lexical.candidate_semantic_digest(value) != lexical.candidate_semantic_digest(unrelated)

    assert lexical.LexicalIndexUse.parse("") == lexical.LexicalIndexUse(path="")
    assert lexical.LexicalIndexUse.parse("cache (used)") == lexical.LexicalIndexUse(path="cache", used=True)
    assert lexical.LexicalIndexUse.parse("cache (stale)") == lexical.LexicalIndexUse(path="cache", reason="stale")
    for invalid in ("cache", "cache used)"):
        with pytest.raises(ValueError, match="expected"):
            lexical.LexicalIndexUse.parse(invalid)

    assert lexical.lexical_phrase_words("prints `fmind/fkf main`).") == ("prints", "fmind/fkf", "main")
    assert lexical.lexical_phrase_supported("fmind/fkf main")
    assert lexical.normalize_query_terms("the THE x #a useful useful") == ("useful",)


def test_rich_corpus_builds_records_pages_tasks_bodies_and_supersessions(tmp_path: Path) -> None:
    base = make_base(tmp_path)
    old = write_event(
        base,
        "2026-09-04",
        [
            {
                "id": "old",
                "time": "2026-09-04T10:00:00.123Z",
                "title": "Repeated run",
                "url": "https://example.test/old",
                "repo": "org:legacy/acme",
                "category": "received",
                "visibility": "private",
                "topic": "retrieval",
                "nested": {"owner": "acme"},
            },
            {
                "id": "untitled",
                "time": "2026-09-04",
                "title": "Archive note",
                "topic": "archive",
            },
        ],
    )
    fresh = write_event(
        base,
        "2026-09-05",
        [
            {
                "id": "new",
                "time": "2026-09-05T10:00:00Z",
                "title": "Repeated run",
                "repo": "org:github.com/acme",
                "topic": "retrieval",
            }
        ],
    )
    write_index(
        base,
        [
            {
                "id": "inventory",
                "title": "Catalog entry",
                "repo": "org:legacy/acme",
                "topic": "inventory",
            }
        ],
    )
    body_uri = old.record_uri(old.records[0])
    assert body_uri is not None
    cache_body(base, old, old.records[0], body_uri, "The unique-body-needle appears only here.")

    write_page(
        base,
        "wiki/old.md",
        "---\ntitle: Old guidance\ndate: 2026-09-01\nvalid_until: 2026-09-06\n"
        "tags: [retrieval]\nrelations: {repo: [org:legacy/acme]}\n---\n\n# Old guidance\n",
    )
    write_page(
        base,
        "wiki/new.md",
        "---\ntitle: New guidance\ndate: 2026-09-05\nvalid_from: 2026-09-05\n"
        "tags: [retrieval]\nrelations: {supersedes: [wiki/old.md]}\n---\n\n"
        "# New guidance\n\nprints `fmind/fkf main`).\n",
    )
    write_page(
        base,
        "projects/acme.md",
        "---\ntitle: Acme project\ntype: organization\nstatus: active\naliases: [acme]\n"
        "relations: {repo: [org:github.com/acme]}\n---\n\n# Acme project\n",
    )
    write_page(
        base,
        "tasks/2026-09-05/audit/TASKS.md",
        "---\ntitle: Retrieval audit\n---\n\n# Retrieval audit\n\n## Learned\n\n- Preserve parity.\n",
    )

    report = lexical.build_lexical_index(base)
    data, use = lexical.read_lexical_index(base)

    assert use.used
    assert data is not None
    assert report.entries == 8
    assert report.context_entries == 7
    assert report.meta.unharvested_bullets == 1
    assert any(item.path.startswith("bodies/") for item in report.meta.inputs)
    assert lexical.lexical_inputs_match(base, report.meta.inputs, report.meta.inputs_sha256)

    by_uri = {entry.uri: entry for entry in data.entries}
    newest_uri = fresh.record_uri(fresh.records[0])
    assert newest_uri is not None
    assert by_uri[newest_uri].context
    assert by_uri[newest_uri].count == 2
    assert by_uri[newest_uri].collapsed == tuple(sorted((body_uri, newest_uri)))
    assert not by_uri[body_uri].context
    assert by_uri[body_uri].body_cached

    phrase, phrase_use = lexical.query_context_lexical_index(
        base,
        ("fmind/fkf", "main"),
        query="fmind/fkf main",
        as_of="2026-09-06",
        summarize=True,
    )
    assert phrase_use.used
    assert phrase is not None
    assert "wiki/new.md" in {entry.uri for entry in phrase.entries}
    assert phrase.supersessions["wiki/old.md"].by == "wiki/new.md"

    hydrated, hydrated_use = lexical.query_context_lexical_index(
        base,
        ("org:github.com/acme",),
        pins=("projects/acme.md",),
        window=Window("2026-09-05", "2026-09-05"),
        as_of="2026-09-06",
    )
    assert hydrated_use.used
    assert hydrated is not None
    assert "projects/acme.md" in {entry.uri for entry in hydrated.entries}
    assert hydrated.consulted_bodies == ()
    assert "wiki/new.md" in hydrated.pinnable

    body_plan, body_use = lexical.query_find_lexical_index(
        base,
        ("unique-body-needle",),
        bodies=True,
        layers=(Layer.EVENTS,),
        sources=("activity",),
        window=Window("2026-09-04", "2026-09-04"),
        record_only=True,
    )
    assert body_use.used
    assert body_plan is not None
    assert body_plan.candidates == frozenset({body_uri})

    # The durable scan remains authoritative; the index only narrows record candidates.
    indexed_find = find(base, FindFilter(grep=("unique-body-needle",), bodies=True))
    assert [item.uri for item in indexed_find.records] == [body_uri]
    assert indexed_find.index is not None
    assert indexed_find.index.used


@pytest.mark.parametrize(
    "value",
    ["", "-1", "01", "+1", "A", "!", "z!"],
)
def test_integer_and_base64_codecs_reject_noncanonical_wire_values(value: str) -> None:
    with pytest.raises(lexical._LexicalIndexCorrupt):
        lexical._canonical_int(value, base=36 if any(character.isalpha() for character in value) else 10)

    with pytest.raises(lexical._LexicalIndexCorrupt):
        lexical._raw_url_decode(value or "=", "test value")


def test_varint_and_posting_payload_boundaries_are_strict() -> None:
    for value in (0, 1, 127, 128, 1 << 32, (1 << 64) - 1):
        encoded = lexical._encode_uvarint(value)
        assert lexical._consume_uvarint(encoded, 0) == (value, len(encoded))
    for value in (-1, 1 << 64):
        with pytest.raises(lexical.LexicalIndexError, match="uint64"):
            lexical._encode_uvarint(value)
    for encoded, message in (
        (b"\x80", "truncated"),
        (b"\x80\x00", "canonical"),
        (b"\xff" * 10, "overflows"),
        (b"\xff" * 9 + b"\x02", "overflows"),
    ):
        with pytest.raises(lexical._LexicalIndexCorrupt, match=message):
            lexical._consume_uvarint(encoded, 0)

    ordinary = lexical.LexicalPostingKey(lexical.LEXICAL_FIND_TRIGRAM, "abc")
    assert lexical._decode_posting_payload(b"\x01\x01", ordinary, 2, ()) == (
        frozenset({0, 1}),
        {},
        2,
    )
    for payload in (b"", b"\x00", b"\x03"):
        with pytest.raises(lexical._LexicalIndexCorrupt):
            lexical._decode_posting_payload(payload, ordinary, 2, ())

    token = lexical.LexicalPostingKey(lexical.LEXICAL_CONTEXT_TOKEN, "term")
    score_payload = b"\x01\x02\x03\x04\x01\x05\x01"
    identifiers, scores, pairs = lexical._decode_posting_payload(b"\x01" + score_payload, token, 1, ("id",))
    assert identifiers == frozenset({0})
    assert pairs == 1
    assert scores[0].analysis.segments == (lexical.LexicalTermSegment("id", 3, 4),)
    assert scores[0].analysis.non_body_match
    assert scores[0].excerpt_bytes == 5

    for invalid in (
        b"\x03\x00\x00\x00\x00\x00\x00",
        b"\x00\x00\x01\x00\x01\x00\x00",
        b"\x00\x00\x01\x01\x02\x00\x00",
        b"\x00\x00\x00\x00\x00\x00\x02",
        b"\x00\x00\x01\x01\x01\x00\x00",
        b"\x00\x00\x00\x00\x00\x00\x01",
    ):
        with pytest.raises(lexical._LexicalIndexCorrupt, match="term score"):
            lexical._decode_term_score(invalid, 0, ("id",))


@pytest.mark.parametrize(
    "uri",
    [
        "",
        " wiki/a.md",
        "wiki/a.md ",
        "wiki/a.md#",
        "wiki/a.md#bad%2f",
        "wiki/a.md?query=x",
        "https://example.test/a",
        "wiki/",
        "../wiki/a.md",
        ".",
    ],
)
def test_cache_entry_uri_grammar_rejects_ambiguous_addresses(uri: str) -> None:
    assert not lexical._valid_lexical_uri(uri)


def test_context_candidate_codec_round_trips_and_rejects_malleability() -> None:
    value = candidate()
    value.body = "Body evidence"
    value.add_segment("body", value.body, 1)
    value.tags = ("one", "two")
    value.fields = {"repo": ("org:github.com/acme",)}
    value.supersedes = ("wiki/old.md",)
    value.created_evidence = True
    encoded = lexical._encode_context_candidate(value)
    entry = lexical.LexicalEntry(0, value.uri, value.kind)

    decoded = lexical._decode_context_candidate(entry, encoded)
    assert decoded.body == value.body
    assert decoded.segments == value.segments

    raw = json.loads(encoded)
    mutations: list[dict[str, object] | str] = [
        "not-json",
        {**raw, "unknown": True},
        {key: item for key, item in raw.items() if key != "title"},
        {**raw, "identifiers": ["z", "a"]},
        {**raw, "direct_identifiers": [""]},
        {**raw, "segments": "not-an-array"},
        {**raw, "segments": [{"field": "body", "weight": 1, "body": True, "text": "both"}]},
        {**raw, "segments": [{"field": "", "weight": 1, "text": "x"}]},
        {**raw, "segments": [{"field": "body", "weight": True, "text": "x"}]},
        {**raw, "segments": [{"field": "body", "weight": 1, "text": " x"}]},
        {**raw, "segments": [{"field": "body", "weight": 1, "body": True}] * 2},
        {**raw, "segments": [], "body": "still present"},
        {**raw, "created_evidence": "true"},
        {**raw, "fields": []},
        {**raw, "tags": [1]},
    ]
    for mutation in mutations:
        candidate_bytes = mutation if isinstance(mutation, str) else json.dumps(mutation, separators=(",", ":"))
        with pytest.raises(lexical._LexicalIndexCorrupt):
            lexical._decode_context_candidate(entry, candidate_bytes)

    with pytest.raises(lexical._LexicalIndexCorrupt, match="no candidate"):
        lexical._decode_context_candidate(lexical.LexicalEntry(0, "index/a.json#x", "index"), encoded)
    with pytest.raises(lexical.LexicalIndexError, match="record"):
        lexical._encode_context_candidate(lexical.LexicalCandidate("index/a.json#x", "record"))


def test_rank_candidate_and_identifier_bound_codecs_fail_closed() -> None:
    value = candidate()
    encoded = lexical._encode_rank_candidate(value)
    entry = lexical.LexicalEntry(0, value.uri, value.kind, rank=encoded)
    decoded = lexical._decode_rank_candidate(entry)
    assert decoded.semantic_digest == lexical.candidate_semantic_digest(value)
    assert decoded.identifier_bounds

    raw = json.loads(encoded)
    for mutation in (
        {},
        {**raw, "unknown": True},
        {**raw, "semantic_digest": "bad"},
        {**raw, "body_available": 1},
        {**raw, "title": None},
        {**raw, "fields": []},
        {**raw, "supersedes": [1]},
    ):
        entry.rank = json.dumps(mutation, separators=(",", ":"))
        with pytest.raises(lexical._LexicalIndexCorrupt):
            lexical._decode_rank_candidate(entry)
    entry.rank = ""
    with pytest.raises(lexical._LexicalIndexCorrupt, match="no rank"):
        lexical._decode_rank_candidate(entry)

    good_bloom = base64.b64encode(b"\x01" + bytes(31)).decode()
    bad_bounds: tuple[object, ...] = (
        "not-an-array",
        [None],
        [{"w": 1, "p": 10}],
        [{"w": True, "p": 10, "b": good_bloom}],
        [{"w": 1, "p": 10, "b": "!"}],
        [{"w": 0, "p": 10, "b": good_bloom}],
        [{"w": 1, "p": 9, "b": good_bloom}],
        [{"w": 1, "p": 10, "b": base64.b64encode(bytes(32)).decode()}],
        [
            {"w": 1, "p": 10, "b": good_bloom},
            {"w": 1, "p": 10, "b": good_bloom},
        ],
    )
    assert lexical._decode_identifier_bounds(None) == ()
    for bounds in bad_bounds:
        with pytest.raises(lexical._LexicalIndexCorrupt, match=r"identifier[_ ]bounds"):
            lexical._decode_identifier_bounds(bounds)


def test_metadata_validator_classifies_each_generation_boundary(tmp_path: Path) -> None:
    base = make_base(tmp_path)
    write_page(base, "wiki/note.md", "# Note\n")
    report = lexical.build_lexical_index(base)
    meta = report.meta

    stale = replace(meta, extractor_version=meta.extractor_version + 1)
    with pytest.raises(lexical._LexicalIndexStale):
        lexical._validate_lexical_index_meta(stale)

    invalid = (
        replace(meta, schema_version=99),
        replace(meta, format="other"),
        replace(meta, entries=-1),
        replace(meta, context_entries=meta.entries + 1),
        replace(meta, posting_rows=meta.postings + 1),
        replace(meta, bytes=-1),
        replace(meta, postings_offset=meta.lookup_offset + 1),
        replace(meta, entries_sha256="bad"),
        replace(meta, lookup_shards=()),
        replace(meta, generated_at="yesterday"),
        replace(meta, inputs=(*meta.inputs, meta.inputs[0])) if meta.inputs else replace(meta, generated_at="bad"),
    )
    for changed in invalid:
        with pytest.raises(lexical._LexicalIndexCorrupt):
            lexical._validate_lexical_index_meta(changed)

    sidecar = base.root / lexical.LEXICAL_INDEX_META_PATH
    for payload in (b"not-json", b"[]", b"{}"):
        sidecar.write_bytes(payload)
        assert lexical.lexical_index_status(base).reason == lexical.LEXICAL_INDEX_FALLBACK_CORRUPT


def test_public_readers_preserve_cancellation_and_classify_artifact_failures(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    base = make_base(tmp_path)
    write_page(base, "wiki/note.md", "# Note\n")
    report = lexical.build_lexical_index(base)
    path = base.root / lexical.LEXICAL_INDEX_PATH

    original = lexical._decode_lexical_index

    def canceled(*_args: object, **_kwargs: object) -> object:
        raise KeyboardInterrupt

    monkeypatch.setattr(lexical, "_decode_lexical_index", canceled)
    with pytest.raises(KeyboardInterrupt):
        lexical.read_lexical_index(base)
    monkeypatch.setattr(lexical, "_decode_lexical_index", original)

    path.unlink()
    assert lexical.lexical_index_health(base).reason == lexical.LEXICAL_INDEX_FALLBACK_MISSING
    path.write_bytes(b"x" * report.meta.bytes)
    assert lexical.lexical_index_health(base).used
    assert lexical.lexical_index_status(base).reason == lexical.LEXICAL_INDEX_FALLBACK_CORRUPT

    path.write_bytes(b"too short")
    assert lexical.lexical_index_health(base).reason == lexical.LEXICAL_INDEX_FALLBACK_CORRUPT

    with pytest.raises(lexical.LexicalIndexError, match="TSV separator"):
        lexical._write_row(("safe", "bad\nfield"))
    monkeypatch.setattr(lexical, "MAX_LEXICAL_INDEX_LINE_BYTES", 2)
    with pytest.raises(lexical.LexicalIndexError, match="maximum"):
        lexical._write_row(("safe",))


def test_stat_fingerprint_reuses_digest_and_rehashes_only_changed_inputs(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    base = make_base(tmp_path)
    write_page(base, "wiki/note.md", "# Note\n")
    prior, _semantics, digest = lexical.lexical_inputs(base)
    calls = 0
    original = lexical._hash_lexical_file

    def counted(path: Path, cancel: Cancellation | None = None) -> tuple[int, str]:
        nonlocal calls
        calls += 1
        return original(path, cancel)

    monkeypatch.setattr(lexical, "_hash_lexical_file", counted)
    assert lexical.lexical_inputs(base, prior)[2] == digest
    assert calls == 0

    page = base.root / "wiki/note.md"
    info = page.stat()
    os.utime(page, ns=(info.st_atime_ns, info.st_mtime_ns + 1))
    assert lexical.lexical_inputs(base, prior)[2] == digest
    assert calls == 1
