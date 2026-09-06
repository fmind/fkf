from __future__ import annotations

import hashlib
import json
import os
from collections.abc import Sequence
from datetime import UTC, datetime
from pathlib import Path
from threading import Event

import pytest

import fkf.lexical as lexical_module
from fkf.base import Base
from fkf.config import load_config
from fkf.errors import CanceledError
from fkf.jsoncodec import dumps
from fkf.lexical import (
    LEXICAL_INDEX_EXTRACTOR_VERSION,
    LEXICAL_INDEX_PATH,
    LEXICAL_INDEX_SCHEMA_VERSION,
    RANKING_VERSION,
    LexicalIndexError,
    LexicalIndexUse,
    LexicalPostingKey,
    build_lexical_index,
    lexical_identifier_subterms,
    lexical_index_status,
    lexical_phrase_supported,
    lexical_posting_shard,
    lexical_trigrams,
    normalize_query_terms,
    query_context_lexical_index,
    query_find_lexical_index,
    read_lexical_index_for_keys,
)
from fkf.listings import TaskListing
from fkf.markdown import Page
from fkf.process import Cancellation
from fkf.query import Window
from fkf.store import Layer

LEXICAL_CONFIG = """\
fkf: 1
name: lexical
schema:
  id: {description: Stable identity., cardinality: one}
  title: {description: Display title., cardinality: optional}
  supersedes: {description: Replaced knowledge., cardinality: many, relation: true}
layers: {events: false, index: false, tasks: false, projects: true, wiki: true}
sources: {}
"""


def _base(tmp_path: Path, *, tasks: bool = False) -> Base:
    root = tmp_path / "base"
    root.mkdir()
    config_text = LEXICAL_CONFIG.replace("tasks: false", "tasks: true") if tasks else LEXICAL_CONFIG
    (root / "fkf.yaml").write_text(config_text, encoding="utf-8")
    config = load_config(root)
    return Base(
        config=config,
        store=config.store(),
        now=lambda: datetime(2026, 9, 6, 8, 9, 10, 456789, tzinfo=UTC),
    )


def _write_page(base: Base, uri: str, text: str) -> None:
    path = base.root / uri
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(text, encoding="utf-8")


def test_versions_and_term_extraction_match_the_go_contract() -> None:
    assert (LEXICAL_INDEX_SCHEMA_VERSION, LEXICAL_INDEX_EXTRACTOR_VERSION, RANKING_VERSION) == (4, 12, 7)
    assert normalize_query_terms("Take my last Retrieval boundary FK-412 and fmind/fkf") == (
        "retrieval",
        "boundary",
        "fk-412",
        "and",
        "fmind/fkf",
    )
    assert lexical_trigrams("AbcA") == ("abc", "bca")
    assert lexical_trigrams("éa") == ()
    assert lexical_identifier_subterms("github.com/fmind/fkf@abcdef") == (
        "fmind/fkf",
        "fmind/fkf@abcdef",
        "github.com/fmind",
        "github.com/fmind/fkf",
        "github.com/fmind/fkf@abcdef",
    )
    assert lexical_identifier_subterms("ordinary") == ()
    assert lexical_phrase_supported("prints fmind/fkf main")
    assert not lexical_phrase_supported("prints **fmind/fkf** main")


def test_lexical_index_use_has_the_compact_public_json_contract() -> None:
    assert dumps(LexicalIndexUse(used=True)) == b'"index/.fkf-index.tsv (used)"'
    assert dumps(LexicalIndexUse(reason="stale")) == b'"index/.fkf-index.tsv (stale)"'


def test_posting_partition_and_lookup_key_are_go_compatible() -> None:
    key = LexicalPostingKey("T", "retrieval")
    digest = hashlib.sha256(b"T\x00retrieval").digest()

    assert lexical_posting_shard(key) == (digest[0] << 4 | digest[1] >> 4)


def test_build_is_deterministic_owner_only_and_classifies_fallbacks(tmp_path: Path) -> None:
    base = _base(tmp_path)
    _write_page(
        base,
        "wiki/retrieval-boundary.md",
        "---\ntype: decision\ntitle: Retrieval boundary\ntags: [retrieval]\n---\n\n"
        "# Retrieval boundary\n\nUse `fmind/fkf main` for FK-412.\n",
    )

    missing = lexical_index_status(base)
    assert (missing.used, missing.reason) == (False, "missing")

    first = build_lexical_index(base)
    rows = (base.root / LEXICAL_INDEX_PATH).read_bytes()
    second = build_lexical_index(base)

    assert (first.entries, first.context_entries, first.mode) == (1, 1, "full")
    # This exact fixture pins semantics JSON, digest framing, TSV rows,
    # delta-varints, and all 4,096 lookup-shard descriptors.
    assert first.meta.semantics_sha256 == "b59a81609bc6c57c31963079df8e037d0026b87552484646c089627e7416d380"
    assert first.meta.inputs_sha256 == "7c834fcf35eaaa32d85b5cf32bd28979f1de3b1ac3896faf250ad2f8cb3e21ba"
    assert first.meta.output_sha256 == "f5ca147ed7f8b9e79fbf3db51f81de91fcfdb53c6fd4d8c2b5a9c40ff77b43bf"
    assert second.meta.output_sha256 == hashlib.sha256(rows).hexdigest()
    assert (base.root / LEXICAL_INDEX_PATH).read_bytes() == rows
    assert (base.root / LEXICAL_INDEX_PATH).stat().st_mode & 0o777 == 0o600
    assert lexical_index_status(base).used

    page = base.root / "wiki/retrieval-boundary.md"
    page.write_text(page.read_text(encoding="utf-8") + "\nChanged.\n", encoding="utf-8")
    stale = lexical_index_status(base)
    assert (stale.used, stale.reason) == (False, "stale")

    build_lexical_index(base)
    path = base.root / LEXICAL_INDEX_PATH
    path.write_bytes(b"x" * path.stat().st_size)
    corrupt = lexical_index_status(base)
    assert (corrupt.used, corrupt.reason) == (False, "corrupt")

    path.unlink()
    missing_again = lexical_index_status(base)
    assert (missing_again.used, missing_again.reason) == (False, "missing")


def test_metadata_is_strict_and_symlink_inputs_fail_closed(tmp_path: Path) -> None:
    base = _base(tmp_path)
    _write_page(base, "wiki/note.md", "# Note\n")
    report = build_lexical_index(base)
    meta_path = base.root / "index/.fkf-index.meta.json"
    meta = json.loads(meta_path.read_bytes())
    meta["unknown"] = True
    meta_path.write_text(json.dumps(meta), encoding="utf-8")

    assert lexical_index_status(base).reason == "corrupt"

    meta_path.unlink()
    (base.root / LEXICAL_INDEX_PATH).unlink()
    target = base.root / "outside.md"
    target.write_text("# Outside\n", encoding="utf-8")
    (base.root / "wiki/note.md").unlink()
    (base.root / "wiki/note.md").symlink_to(target)

    with pytest.raises(LexicalIndexError, match=r"regular file|symlink"):
        build_lexical_index(base)

    assert report.meta.schema_version == 4


def test_decoder_enforces_the_go_line_bound(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    base = _base(tmp_path)
    _write_page(base, "wiki/note.md", "# Note\n")
    build_lexical_index(base)

    monkeypatch.setattr(lexical_module, "MAX_LEXICAL_INDEX_LINE_BYTES", 1)

    assert lexical_index_status(base).reason == "corrupt"


def test_index_document_inventory_ignores_derived_hidden_names(tmp_path: Path) -> None:
    base = _base(tmp_path)
    index = base.root / "index"
    index.mkdir()
    (index / ".fkf-index.tsv").write_text("derived", encoding="utf-8")
    (index / ".fkf-index.meta.json").write_text("{}", encoding="utf-8")

    report = build_lexical_index(base)

    assert report.entries == 0
    assert all(not item.path.startswith("index/.fkf-") for item in report.meta.inputs)


def test_failed_rebuild_does_not_replace_a_previous_generation(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    base = _base(tmp_path)
    _write_page(base, "wiki/note.md", "# Note\n")
    build_lexical_index(base)
    rows_path = base.root / LEXICAL_INDEX_PATH
    meta_path = base.root / "index/.fkf-index.meta.json"
    before = (rows_path.read_bytes(), meta_path.read_bytes())

    original = os.stat
    calls = 0

    def drifting(
        path: os.PathLike[str] | str | int,
        *,
        dir_fd: int | None = None,
        follow_symlinks: bool = True,
    ) -> os.stat_result:
        nonlocal calls
        result = original(path, dir_fd=dir_fd, follow_symlinks=follow_symlinks)
        if not isinstance(path, int) and Path(path).name == "note.md":
            calls += 1
            if calls > 1:
                (base.root / "wiki/note.md").write_text("# Changed during build\n", encoding="utf-8")
        return result

    monkeypatch.setattr(os, "stat", drifting)
    with pytest.raises(LexicalIndexError, match="changed while"):
        build_lexical_index(base)

    assert (rows_path.read_bytes(), meta_path.read_bytes()) == before


@pytest.mark.parametrize("limit", [1, 25, 1_000])
def test_build_cancellation_checkpoints_preserve_the_previous_generation(tmp_path: Path, limit: int) -> None:
    class CancelAfter:
        def __init__(self) -> None:
            self.checks = 0

        def is_set(self) -> bool:
            self.checks += 1
            return self.checks >= limit

    base = _base(tmp_path)
    _write_page(base, "wiki/note.md", "# Note\n\n" + "retrieval boundary " * 50)
    build_lexical_index(base)
    rows_path = base.root / LEXICAL_INDEX_PATH
    meta_path = base.root / "index/.fkf-index.meta.json"
    before = (rows_path.read_bytes(), meta_path.read_bytes())
    cancel = CancelAfter()

    with pytest.raises(CanceledError, match="operation canceled"):
        build_lexical_index(base, cancel=cancel)

    assert cancel.checks == limit
    assert (rows_path.read_bytes(), meta_path.read_bytes()) == before


def test_build_forwards_one_cancellation_event_through_nested_inventory_reads(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    base = _base(tmp_path, tasks=True)
    _write_page(base, "wiki/note.md", "# Note\n")
    _write_page(base, "tasks/2026-09-06/audit/TASKS.md", "# Audit\n\n## Learned\n\n- Preserve the cache.\n")
    build_lexical_index(base)
    rows_path = base.root / LEXICAL_INDEX_PATH
    meta_path = base.root / "index/.fkf-index.meta.json"
    before = (rows_path.read_bytes(), meta_path.read_bytes())
    _write_page(base, "wiki/note.md", "# Changed input\n")

    cancel = Event()
    observed: list[str] = []
    task_calls = 0
    original_graph_inputs = lexical_module.graph_input_uris
    original_identity_load = lexical_module.IdentityResolver.load
    original_pages = lexical_module.load_markdown_layer
    original_tasks = lexical_module.list_tasks

    def observe_graph_inputs(candidate: Base, *, cancel: Cancellation | None = None) -> tuple[str, ...]:
        assert cancel is cancel_event
        observed.append("graph-inputs")
        return original_graph_inputs(candidate, cancel=cancel_event)

    def observe_identity_load(
        _cls: type[lexical_module.IdentityResolver],
        candidate: Base,
        *,
        cancel: Cancellation | None = None,
    ) -> lexical_module.IdentityResolver:
        assert cancel is cancel_event
        observed.append("identities")
        return original_identity_load(candidate, cancel=cancel_event)

    def observe_pages(
        candidate: Base,
        layer: Layer,
        *,
        cancel: Cancellation | None = None,
    ) -> tuple[tuple[Page, ...], tuple[str, ...]]:
        assert cancel is cancel_event
        observed.append(f"pages:{layer}")
        return original_pages(candidate, layer, cancel=cancel_event)

    def observe_tasks(
        candidate: Base,
        window: Window | None = None,
        *,
        limit: int = 0,
        cancel: Cancellation | None = None,
    ) -> TaskListing:
        nonlocal task_calls
        assert cancel is cancel_event
        observed.append("tasks")
        task_calls += 1
        result = original_tasks(candidate, window, limit=limit, cancel=cancel_event)
        if task_calls == 2:
            cancel_event.set()
        return result

    cancel_event = cancel
    monkeypatch.setattr(lexical_module, "graph_input_uris", observe_graph_inputs)
    monkeypatch.setattr(lexical_module.IdentityResolver, "load", classmethod(observe_identity_load))
    monkeypatch.setattr(lexical_module, "load_markdown_layer", observe_pages)
    monkeypatch.setattr(lexical_module, "list_tasks", observe_tasks)

    with pytest.raises(CanceledError, match="operation canceled"):
        build_lexical_index(base, cancel=cancel)

    assert observed.count("graph-inputs") == 1
    assert observed.count("identities") == 1
    assert observed.count("tasks") == 2
    assert {item for item in observed if item.startswith("pages:")} == {"pages:projects", "pages:wiki"}
    assert (rows_path.read_bytes(), meta_path.read_bytes()) == before


def test_cache_readers_preserve_preexisting_and_mid_scan_cancellation(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    base = _base(tmp_path)
    _write_page(base, "wiki/a.md", "# A\n")
    _write_page(base, "wiki/b.md", "# B\n")
    build_lexical_index(base)
    canceled = Event()
    canceled.set()
    key = LexicalPostingKey("T", "note")

    operations = (
        lambda: lexical_module.lexical_index_health(base, cancel=canceled),
        lambda: lexical_module.lexical_index_status(base, cancel=canceled),
        lambda: lexical_module.read_lexical_index(base, cancel=canceled),
        lambda: lexical_module.read_lexical_index_for_keys(base, (key,), cancel=canceled),
        lambda: lexical_module.query_context_lexical_index(base, ("note",), cancel=canceled),
        lambda: lexical_module.query_find_lexical_index(base, ("note",), cancel=canceled),
    )
    for operation in operations:
        with pytest.raises(CanceledError, match="operation canceled"):
            operation()

    mid_scan = Event()
    decoded = 0
    original_decode_entry = lexical_module._decode_entry  # noqa: SLF001

    def cancel_after_first_entry(fields: Sequence[str], expected_id: int) -> lexical_module.LexicalEntry:
        nonlocal decoded
        result = original_decode_entry(fields, expected_id)
        decoded += 1
        if decoded == 1:
            mid_scan.set()
        return result

    monkeypatch.setattr(lexical_module, "_decode_entry", cancel_after_first_entry)
    with pytest.raises(CanceledError, match="operation canceled"):
        lexical_module.read_lexical_index(base, cancel=mid_scan)
    assert decoded == 1


def test_sparse_lookup_authenticates_inclusion_absence_and_only_the_named_shard(tmp_path: Path) -> None:
    base = _base(tmp_path)
    _write_page(
        base,
        "wiki/retrieval-boundary.md",
        "# Retrieval boundary\n\nUse fmind/fkf for the local cache.\n",
    )
    report = build_lexical_index(base)
    present = LexicalPostingKey("T", "retrieval")
    absent = LexicalPostingKey("T", "definitely-absent")

    data, use = read_lexical_index_for_keys(base, (present, absent))

    assert use.used
    assert data is not None
    assert data.postings[present] == frozenset({0})
    assert absent not in data.postings

    requested_shard = lexical_posting_shard(present)
    unrelated = next(
        index for index, shard in enumerate(report.meta.lookup_shards) if shard.rows and index != requested_shard
    )
    rows_path = base.root / LEXICAL_INDEX_PATH
    with rows_path.open("r+b") as handle:
        handle.seek(report.meta.lookup_shards[unrelated].offset)
        original = handle.read(1)
        handle.seek(report.meta.lookup_shards[unrelated].offset)
        handle.write(bytes((original[0] ^ 1,)))

    sparse, sparse_use = read_lexical_index_for_keys(base, (present,))
    assert sparse_use.used
    assert sparse is not None
    assert sparse.postings[present] == frozenset({0})
    assert lexical_index_status(base).reason == "corrupt"

    report = build_lexical_index(base)
    requested = report.meta.lookup_shards[requested_shard]
    with rows_path.open("r+b") as handle:
        handle.seek(requested.offset)
        original = handle.read(1)
        handle.seek(requested.offset)
        handle.write(bytes((original[0] ^ 1,)))

    corrupt, corrupt_use = read_lexical_index_for_keys(base, (present,))
    assert corrupt is None
    assert corrupt_use.reason == "corrupt"


def test_context_and_find_plans_are_conservative_and_fail_soft(tmp_path: Path) -> None:
    base = _base(tmp_path)
    _write_page(
        base,
        "wiki/retrieval-boundary.md",
        "# Retrieval boundary\n\nUse fmind/fkf for the local cache.\n",
    )
    build_lexical_index(base)

    context, context_use = query_context_lexical_index(base, ("fmind/fkf",), query="fmind/fkf", as_of="2026-09-06")
    find, find_use = query_find_lexical_index(base, ("not-present",))
    short, short_use = query_find_lexical_index(base, ("x",))

    assert context_use.used
    assert context is not None
    assert tuple(entry.uri for entry in context.entries) == ("wiki/retrieval-boundary.md",)
    assert context.entries[0].candidate is not None
    # Find never trusts the cache to exclude authored Markdown; durable page scanning is authoritative.
    assert find_use.used
    assert find is not None
    assert find.candidates == frozenset({"wiki/retrieval-boundary.md"})
    assert short is None
    assert short_use.reason == "query-too-short"
