from __future__ import annotations

from datetime import datetime, timedelta, timezone
from pathlib import Path
from threading import Event

import pytest

import fkf.context as context_module
import fkf.lexical as lexical_module
from fkf.config import ConfigError
from fkf.errors import CanceledError
from fkf.eval import EVAL_PATH, MAX_EVAL_BUDGET, evaluate
from fkf.lexical import build_lexical_index
from fkf.output import text_bytes
from fkf.store import UnsafePathError
from tests.services.test_context import ExplodingRunner, seeded_base


def write_suite(base_root: Path, content: str) -> None:
    path = base_root / EVAL_PATH
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(content, encoding="utf-8")


def test_eval_measures_the_exact_final_delivery_and_one_clock(tmp_path: Path) -> None:
    base = seeded_base(tmp_path)
    reads = 0
    original = base.now

    def clock():
        nonlocal reads
        reads += 1
        return original()

    base.now = clock
    write_suite(
        base.root,
        """\
fkf: 1
k: 3
budget: 1500
delivery: text
recall_threshold: 1
queries:
  - name: text
    question: Retrieval boundary
    expected_uris: [wiki/retrieval-boundary.md]
  - name: json
    question: Retrieval boundary
    delivery: json
    expected_uris: [wiki/retrieval-boundary.md]
  - name: jsonl
    question: Retrieval boundary
    delivery: jsonl
    expected_uris: [wiki/retrieval-boundary.md]
""",
    )

    report = evaluate(base)

    assert reads == 1
    assert report.passed
    assert report.passed_queries == 3
    assert report.failed == 0
    assert report.evaluation_time == "2026-05-10T12:00:00Z"
    assert [query.delivery for query in report.queries] == ["text", "json", "jsonl"]
    for query in report.queries:
        assert query.passed
        assert query.recall == 1
        assert query.expected_ranks[0].rank <= query.k
        assert query.delivered_bytes > 0
        assert query.delivered_tokens == (query.delivered_bytes + 3) // 4
        assert query.delivered_tokens <= query.budget
        assert query.input_digest
        assert query.ranking_version == 7
    assert isinstance(base.runner, ExplodingRunner)
    assert base.runner.calls == 0


def test_eval_reuses_one_authenticated_index_generation(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    base = seeded_base(tmp_path)
    write_suite(
        base.root,
        """\
fkf: 1
k: 3
budget: 1500
recall_threshold: 1
queries:
  - name: first
    question: Retrieval
    expected_uris: [wiki/retrieval-boundary.md]
  - name: second
    question: Retrieval
    expected_uris: [wiki/retrieval-boundary.md]
  - name: empty
    question: zzz-no-answer-zzz
    expect_empty: true
""",
    )
    scanned = evaluate(base)
    build_lexical_index(base)
    original_decode = lexical_module._decode_entries  # noqa: SLF001 - authenticated-prefix regression seam
    original_match = context_module.lexical_inputs_match
    original_analyze = context_module._analyze_segment  # noqa: SLF001 - cached-score regression seam
    decoded = 0
    revalidated = 0
    analyzed = 0

    def count_decode(*args, **kwargs):
        nonlocal decoded
        decoded += 1
        return original_decode(*args, **kwargs)

    def count_match(*args, **kwargs):
        nonlocal revalidated
        revalidated += 1
        return original_match(*args, **kwargs)

    def count_analyze(*args, **kwargs):
        nonlocal analyzed
        analyzed += 1
        return original_analyze(*args, **kwargs)

    monkeypatch.setattr(lexical_module, "_decode_entries", count_decode)
    monkeypatch.setattr(context_module, "lexical_inputs_match", count_match)
    monkeypatch.setattr(context_module, "_analyze_segment", count_analyze)

    report = evaluate(base)

    assert report.passed
    assert decoded == 1
    assert revalidated == 1
    assert analyzed == 0
    assert all(query.index.used for query in report.queries)
    assert [
        (query.delivered_uris, query.expected_ranks, query.recall, query.input_digest, query.passed)
        for query in report.queries
    ] == [
        (query.delivered_uris, query.expected_ranks, query.recall, query.input_digest, query.passed)
        for query in scanned.queries
    ]


def test_eval_cached_scores_preserve_last_query_non_body_ordering(tmp_path: Path) -> None:
    base = seeded_base(tmp_path)
    (base.root / "wiki" / "a-body.md").write_text(
        "---\ntitle: Alpha\n---\n\n# Alpha\n\nneedle\n",
        encoding="utf-8",
    )
    (base.root / "wiki" / "z-description.md").write_text(
        "---\ntitle: Zulu\ndescription: needle\n---\n\n# Zulu\n\nneedle\n",
        encoding="utf-8",
    )
    write_suite(
        base.root,
        "fkf: 1\nk: 1\nbudget: 1500\nrecall_threshold: 1\nqueries:\n"
        "  - name: newest-direct-match\n"
        "    question: last needle\n"
        "    expected_uris: [wiki/z-description.md]\n",
    )

    scanned = evaluate(base)
    build_lexical_index(base)
    indexed = evaluate(base)

    assert scanned.passed
    assert indexed.passed
    assert indexed.queries[0].delivered_uris == scanned.queries[0].delivered_uris


def test_eval_retries_the_whole_batch_on_generation_drift(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    base = seeded_base(tmp_path)
    build_lexical_index(base)
    write_suite(
        base.root,
        """\
fkf: 1
k: 3
budget: 1500
recall_threshold: 1
queries:
  - name: first
    question: Retrieval boundary
    expected_uris: [wiki/retrieval-boundary.md]
  - name: second
    question: Retrieval boundary
    expected_uris: [wiki/retrieval-boundary.md]
""",
    )
    outcomes = iter((False, True))
    calls = 0

    def changed_once(*_args, **_kwargs):
        nonlocal calls
        calls += 1
        return next(outcomes)

    monkeypatch.setattr(context_module, "lexical_inputs_match", changed_once)

    report = evaluate(base)

    assert report.passed
    assert calls == 2
    assert all(not query.index.used and query.index.reason == "stale" for query in report.queries)


def test_eval_preserves_the_clock_offset_and_text_uses_whole_seconds(tmp_path: Path) -> None:
    base = seeded_base(tmp_path)
    base.now = lambda: datetime(2026, 9, 6, 12, 34, 56, 789123, tzinfo=timezone(timedelta(hours=2)))
    write_suite(
        base.root,
        "fkf: 1\nk: 1\nrecall_threshold: 1\nqueries:\n"
        "  - name: empty\n    question: zzz-no-answer-zzz\n    expect_empty: true\n",
    )

    report = evaluate(base)

    assert report.evaluation_time == "2026-09-06T12:34:56.789123+02:00"
    assert b"evaluated 2026-09-06T12:34:56+02:00" in text_bytes(report)[0]


def test_eval_fails_recall_for_misses_forbidden_hits_and_false_empty(tmp_path: Path) -> None:
    base = seeded_base(tmp_path)
    write_suite(
        base.root,
        """\
fkf: 1
k: 1
recall_threshold: 1
queries:
  - name: miss-and-forbidden
    question: Retrieval boundary
    expected_uris: [events/2026-04-05/synthetic.json#old1]
    forbidden_uris: [wiki/retrieval-boundary.md]
  - name: true-empty
    question: zzz-no-answer-zzz
    expect_empty: true
""",
    )

    report = evaluate(base)

    assert not report.passed
    assert report.failed == 1
    failed, empty = report.queries
    assert not failed.passed
    assert failed.missing_expected == ("events/2026-04-05/synthetic.json#old1",)
    assert failed.forbidden_found == ("wiki/retrieval-boundary.md",)
    assert empty.passed
    assert empty.recall == 1
    assert empty.delivered_uris == ()


@pytest.mark.parametrize(
    ("declaration", "message"),
    [
        ("unknown: true\n", "field not found"),
        ("budget: 0\n", "budget"),
        (f"budget: {MAX_EVAL_BUDGET + 1}\n", "budget"),
        ("delivery: html\n", "delivery"),
        ("recall_threshold: 0\n", "recall_threshold"),
        ("", "expected_uris"),
    ],
)
def test_eval_suite_is_strict_and_fail_closed(tmp_path: Path, declaration: str, message: str) -> None:
    base = seeded_base(tmp_path)
    query = (
        "  - name: invalid\n    question: query\n"
        if not declaration
        else "  - name: valid\n    question: query\n    expect_empty: true\n"
    )
    write_suite(
        base.root,
        f"fkf: 1\nk: 3\n{declaration}recall_threshold: 1\nqueries:\n{query}",
    )

    with pytest.raises(ConfigError, match=message):
        evaluate(base)


def test_eval_rejects_duplicate_uris_symlinks_and_cancellation(tmp_path: Path) -> None:
    base = seeded_base(tmp_path)
    body = """\
fkf: 1
k: 3
recall_threshold: 1
queries:
  - name: duplicate
    question: Retrieval boundary
    expected_uris: [wiki/retrieval-boundary.md, wiki/retrieval-boundary.md]
"""
    write_suite(base.root, body)
    with pytest.raises(ConfigError, match="duplicated"):
        evaluate(base)

    canceled = Event()
    canceled.set()
    write_suite(base.root, body.replace(", wiki/retrieval-boundary.md", ""))
    with pytest.raises(CanceledError):
        evaluate(base, cancel=canceled)

    evals = base.root / "evals"
    suite = evals / "queries.yaml"
    suite.unlink()
    evals.rmdir()
    outside = tmp_path / "outside"
    outside.mkdir()
    (outside / "queries.yaml").write_text(body, encoding="utf-8")
    evals.symlink_to(outside, target_is_directory=True)
    with pytest.raises(UnsafePathError, match="symlink"):
        evaluate(base)
