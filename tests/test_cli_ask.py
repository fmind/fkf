from __future__ import annotations

import io
import json
from pathlib import Path
from threading import Event

import pytest

from fkf.cli import app
from fkf.cli_support import run_app
from fkf.errors import OperationalError
from fkf.eval import EVAL_PATH
from fkf.find import FindResult, SourceCount, Volume
from fkf.graph import (
    Direction,
    EdgeScanStats,
    GraphSummary,
    KindCount,
    NeighbourEdge,
    Neighbourhood,
    NodeCount,
    NodeListing,
    build_graph,
)
from fkf.locking import WriterLock
from fkf.output import text_bytes
from fkf.query import Window
from tests.services.test_context import seeded_base
from tests.services.test_read import make_base as make_read_base


def invoke(*arguments: str) -> tuple[int, str, str]:
    stdout, stderr = io.StringIO(), io.StringIO()
    code = run_app(app, arguments, stdout=stdout, stderr=stderr)
    return code, stdout.getvalue(), stderr.getvalue()


def test_context_cli_selects_delivery_and_persists_a_receipt_snapshot(tmp_path: Path) -> None:
    home = tmp_path / "home"
    base = seeded_base(tmp_path)
    code, stdout, stderr = invoke(
        "context",
        "--save-receipt",
        "retrieval",
        "boundary",
        "--since",
        "2026-04-01",
        "--until",
        "2026-05-10",
        "--budget",
        "1500",
        "--base",
        str(base.root),
        "--format",
        "json",
    )

    assert code == 0
    result = json.loads(stdout)
    assert result["query"] == "retrieval boundary"
    assert result["receipt"]["format"] == "json"
    assert result["receipt"]["encoded_tokens"] <= 1500
    assert result["receipt"]["input_digest"]
    assert any(item["uri"] == "wiki/retrieval-boundary.md" for item in result["items"])
    assert stderr == ""
    snapshots = tuple((home / "state" / "fkf" / "receipts").glob("*/*.json.gz"))
    assert len(snapshots) == 1


def test_context_cli_rejects_missing_terms_before_opening_a_base() -> None:
    code, stdout, stderr = invoke("context", "--format", "json")
    assert code == 2
    assert stdout == ""
    assert "takes the terms" in stderr


def test_context_cli_takes_the_physical_base_writer_lock(tmp_path: Path) -> None:
    base = seeded_base(tmp_path)
    alias = tmp_path / "base-alias"
    alias.symlink_to(base.root, target_is_directory=True)

    with WriterLock.acquire(base.root):
        code, stdout, stderr = invoke("context", "retrieval", "--save-receipt", "--base", str(alias))

    assert code == 1
    assert stdout == ""
    assert "active writer" in stderr


def test_context_default_reads_while_writer_is_locked_without_saving(tmp_path: Path) -> None:
    base = seeded_base(tmp_path)
    with WriterLock.acquire(base.root):
        code, stdout, stderr = invoke("context", "retrieval", "--base", str(base.root))
    assert code == 0, stderr
    assert json.loads(stdout)["items"]
    assert not tuple((tmp_path / "home" / "state" / "fkf" / "receipts").glob("*/*.json.gz"))


@pytest.mark.parametrize(
    ("service", "arguments"),
    [
        ("build_context", ("context", "needle")),
        ("find", ("find", "needle")),
        ("read", ("read", "wiki/retrieval-boundary.md", "--body")),
        ("evaluate", ("eval",)),
        ("verify_graph", ("graph", "--verify")),
        ("summarize_graph", ("graph",)),
        ("neighbours", ("graph", "fkf://sample/wiki/example.md")),
        ("list_nodes", ("graph", "nodes")),
    ],
)
def test_ask_commands_forward_the_invocation_cancellation(
    monkeypatch: pytest.MonkeyPatch,
    tmp_path: Path,
    service: str,
    arguments: tuple[str, ...],
) -> None:
    base = seeded_base(tmp_path)
    observed: list[object] = []

    def stop(*_args: object, cancel: object = None, **_kwargs: object) -> None:
        observed.append(cancel)
        raise OperationalError("stopped after observing cancellation")

    monkeypatch.setattr(f"fkf.cli_ask.{service}", stop)
    code, stdout, stderr = invoke(*arguments, "--base", str(base.root))

    assert code == 1
    assert stdout == ""
    assert "stopped after observing cancellation" in stderr
    assert len(observed) == 1
    assert isinstance(observed[0], Event)


def test_eval_cli_maps_threshold_failure_to_exit_one(tmp_path: Path) -> None:
    base = seeded_base(tmp_path)
    path = base.root / EVAL_PATH
    path.parent.mkdir(parents=True)
    path.write_text(
        """\
fkf: 1
k: 1
recall_threshold: 1
queries:
  - name: miss
    question: retrieval boundary
    expected_uris: [events/2026-04-05/synthetic.json#old1]
""",
        encoding="utf-8",
    )

    code, stdout, stderr = invoke("eval", "--base", str(base.root), "--format", "json")
    assert code == 1
    assert json.loads(stdout)["failed_queries"] == 1
    assert "retrieval evaluation(s) failed" in stderr


def test_find_count_text_aligns_to_the_widest_source_in_the_result() -> None:
    result = FindResult(
        Window("2026-09-04", "2026-09-05"),
        days=("2026-09-04", "2026-09-05"),
        volumes=(Volume("2026-09-04", 4, (SourceCount("short", 1), SourceCount("longer-source", 3))),),
        matched=4,
    )

    assert text_bytes(result)[0] == (
        b"2026-09-04 .. 2026-09-05  2 day(s)\n\n"
        b"longer-source      3\n"
        b"short              1\n\n"
        b"4 record(s) across 1 day(s)\n"
    )


def test_graph_cli_disambiguates_a_bare_uri_from_nodes_options(tmp_path: Path) -> None:
    base = seeded_base(tmp_path)
    build_graph(base)

    code, stdout, stderr = invoke("graph", "nodes", "--limit", "1", "--base", str(base.root), "--format", "json")
    assert code == 0
    assert len(json.loads(stdout)["nodes"]) == 1
    assert stderr == ""

    code, stdout, stderr = invoke(
        "graph",
        "wiki/retrieval-boundary.md",
        "--depth",
        "1",
        "--base",
        str(base.root),
        "--format",
        "json",
    )
    assert code == 0
    assert json.loads(stdout)["uri"] == "wiki/retrieval-boundary.md"
    assert stderr == ""


def test_read_text_matches_the_native_page_record_document_and_entity_layout(tmp_path: Path) -> None:
    base = make_read_base(tmp_path)

    code, page, stderr = invoke("read", "wiki/retrieval-boundary.md", "--base", str(base.root), "--format", "text")
    assert code == 0
    assert stderr == ""
    assert page.startswith("wiki/retrieval-boundary.md  [page]\n\n")
    assert page.endswith("No ambient execution.\n\n")

    code, record, stderr = invoke(
        "read", "events/2026-09-05/journal.json#a1", "--base", str(base.root), "--format", "text"
    )
    assert code == 0
    assert stderr == ""
    assert record.splitlines()[2:] == [
        "id     a1",
        "time   2026-09-05T09:00:00Z",
        "title  First",
    ]

    code, document, stderr = invoke(
        "read", "events/2026-09-05/journal.json", "--base", str(base.root), "--format", "text"
    )
    assert code == 0
    assert stderr == ""
    assert "events/2026-09-05/journal.json  journal  2 record(s)  collected 2026-09-06T11:00:00Z" in document
    assert "events/2026-09-05/journal.json#a1\n    First" in document


def test_graph_text_renderers_match_the_public_summary_walk_and_node_layout() -> None:
    summary = GraphSummary(
        "graph.tsv",
        "2026-09-06T08:00:00Z",
        2,
        3,
        (KindCount("link", 2),),
        (KindCount("wiki", 3),),
        ("markdown-inline",),
        EdgeScanStats(lines=2, matched=2),
    )
    expected_summary = """\
graph.tsv  2 edge(s), 3 node(s)  built 2026-09-06T08:00:00Z
edges   link 2
nodes   wiki 3
from    markdown-inline
"""
    assert text_bytes(summary) == (expected_summary.encode(), True)

    neighbourhood = Neighbourhood(
        "wiki/a.md",
        Direction.BOTH,
        1,
        (NeighbourEdge("wiki/a.md", "tag:a", "tag", via="frontmatter:tags", hop=1),),
        ("tag:a",),
        False,
        EdgeScanStats(lines=4, matched=1),
    )
    assert text_bytes(neighbourhood) == (
        b"1  tag        wiki/a.md -> tag:a  (frontmatter:tags)\n\n1 edge(s), 1 node(s), 4 row(s) scanned\n",
        True,
    )

    nodes = NodeListing("wiki", (NodeCount("wiki/a.md", "wiki", 2, 1, 3),), 1, EdgeScanStats(lines=4))
    assert text_bytes(nodes) == (b"    3  wiki     wiki/a.md  (in 1, out 2)\n\n1 node(s)\n", True)
