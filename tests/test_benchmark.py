from __future__ import annotations

from pathlib import Path

from scripts.benchmark_scale import SCALE_NOW, create_scale_corpus

from fkf.base import open_base
from fkf.context import ContextRequest, build_context
from fkf.find import NO_FIND_LIMIT, FindFilter, find
from fkf.graph import Direction, GraphQuery, neighbours


def test_scale_corpus_exercises_the_supported_operations(tmp_path: Path) -> None:
    corpus = create_scale_corpus(tmp_path, records=10, relations_per_record=5)
    base = open_base(str(corpus.root))
    base.now = lambda: SCALE_NOW

    assert (corpus.records, corpus.edges) == (10, 50)
    found = find(
        base,
        FindFilter(
            sources=("scale",),
            window=corpus.window,
            grep=("benchmark",),
            limit=NO_FIND_LIMIT,
        ),
    )
    assert (found.scanned, found.matched, len(found.records)) == (10, 10, 10)

    pack = build_context(
        base,
        ContextRequest("record-000009", window=corpus.window, budget=2048),
    )
    assert pack.items
    assert pack.receipt.encoded_tokens <= 2048

    graph = neighbours(base, GraphQuery(corpus.first_record_uri, Direction.OUT, limit=100))
    assert len(graph.edges) == 5
