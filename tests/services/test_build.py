from __future__ import annotations

import json
from dataclasses import replace
from pathlib import Path
from threading import Event

import pytest

from fkf.base import Base
from fkf.build import BuildCheck, BuildOptions, BuildTarget, build, build_if_stale, build_stale, parse_build_target
from fkf.config import Config, SyncConfig
from fkf.errors import CanceledError, InvalidUsageError
from fkf.fields import FieldSchema
from fkf.graph import EdgeScanStats, GraphSummary
from fkf.jsoncodec import dumps
from fkf.lexical import LexicalIndexBuild, LexicalIndexUse
from fkf.store import Layer, Store
from fkf.wiki_index import WikiIndexReport


def _base(tmp_path: Path, *, wiki: bool = True) -> Base:
    path = tmp_path / "fkf.yaml"
    path.write_text("name: build\n")
    layers = dict.fromkeys(Layer, True)
    layers[Layer.WIKI] = wiki
    config = Config(1, "build", FieldSchema(), layers, {}, {}, SyncConfig(), (), path)
    return Base(config, Store(tmp_path, layers))


def _summary() -> GraphSummary:
    return GraphSummary("graph.tsv", "2026-09-06T00:00:00Z", 2, 3, (), (), (), EdgeScanStats())


def test_build_target_vocabulary_is_closed() -> None:
    assert parse_build_target("") is BuildTarget.ALL
    with pytest.raises(InvalidUsageError, match="expected bodies, graph"):
        parse_build_target("everything")


def test_build_all_preserves_wiki_graph_index_order(monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> None:
    calls: list[str] = []
    base = _base(tmp_path)
    wiki = WikiIndexReport("wiki/index.md", 1, 1, 1, changed=True)
    graph = BuildCheck("graph.tsv", False)
    index = BuildCheck("index/.fkf-index.tsv", False)
    monkeypatch.setattr("fkf.build.build_wiki_index", lambda _base, **_options: calls.append("wiki") or wiki)
    monkeypatch.setattr("fkf.build.build_graph", lambda _base, **_kwargs: calls.append("graph") or graph)
    monkeypatch.setattr("fkf.build.build_lexical_index", lambda _base, **_kwargs: calls.append("index") or index)
    report = build(base)
    assert calls == ["wiki", "graph", "index"]
    assert (report.wiki, report.graph, report.index) == (wiki, graph, index)


def test_build_stale_short_circuits_in_dependency_order(monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> None:
    base = _base(tmp_path)
    monkeypatch.setattr(
        "fkf.build.build_wiki_index",
        lambda _base, **_options: WikiIndexReport("wiki/index.md", 0, 0, 0, changed=True, stale=True),
    )
    monkeypatch.setattr(
        "fkf.build.summarize_graph", lambda _base, **_kwargs: pytest.fail("graph must not be inspected")
    )
    assert build_stale(base)


def test_build_stale_prioritizes_cancellation_over_a_stale_phase(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    base = _base(tmp_path)
    cancel = Event()

    def stale_wiki(_base: Base, *, write: bool, cancel: object) -> WikiIndexReport:
        assert not write
        assert cancel is cancel_event
        cancel_event.set()
        return WikiIndexReport("wiki/index.md", 0, 0, 0, stale=True)

    cancel_event = cancel
    monkeypatch.setattr("fkf.build.build_wiki_index", stale_wiki)

    with pytest.raises(CanceledError):
        build_stale(base, cancel=cancel)


def test_build_if_stale_returns_explicit_noop(monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> None:
    base = _base(tmp_path, wiki=False)
    monkeypatch.setattr("fkf.build.summarize_graph", lambda _base, **_kwargs: _summary())
    monkeypatch.setattr("fkf.build.lexical_index_health", lambda _base, **_kwargs: LexicalIndexUse(used=True))
    assert build_if_stale(base).nothing_stale


def test_check_reports_each_cache_without_writing(monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> None:
    base = _base(tmp_path)
    monkeypatch.setattr(
        "fkf.build.build_wiki_index",
        lambda _base, write, **_kwargs: WikiIndexReport("wiki/index.md", 0, 0, 0, stale=not write),
    )
    monkeypatch.setattr("fkf.build.summarize_graph", lambda _base, **_kwargs: _summary())
    monkeypatch.setattr("fkf.build.lexical_index_health", lambda _base, **_kwargs: LexicalIndexUse(reason="stale"))
    report = build(base, BuildOptions(check=True))
    from fkf.graph import GraphBuild

    assert isinstance(report.graph, GraphBuild)
    assert not report.graph.stale
    assert isinstance(report.index, LexicalIndexBuild)
    assert report.index.stale
    assert report.stale
    encoded = json.loads(dumps(report))
    assert isinstance(encoded, dict)
    assert encoded["graph"]["edges"] == 2
    assert encoded["graph"]["meta"]["columns"] is None
    assert encoded["index"]["meta_uri"] == ""
    assert encoded["index"]["meta"]["lookup_shards"] is None


def test_body_flags_are_confined_to_explicit_pruning(tmp_path: Path) -> None:
    base = _base(tmp_path)
    with pytest.raises(InvalidUsageError, match="requires --prune"):
        build(base, BuildOptions(target=BuildTarget.BODIES))
    with pytest.raises(InvalidUsageError, match="only"):
        build(base, BuildOptions(target=BuildTarget.GRAPH, prune=True))
    with pytest.raises(InvalidUsageError, match="cannot be combined"):
        build(base, replace(BuildOptions(check=True), prune=True))


def test_build_passes_one_cancellation_event_through_ordered_phases(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    base = _base(tmp_path)
    cancel = Event()
    calls: list[str] = []

    def wiki(_base: Base, *, write: bool, cancel: object) -> WikiIndexReport:
        assert write
        assert cancel is cancel_event
        calls.append("wiki")
        return WikiIndexReport("wiki/index.md", 0, 0, 0)

    def graph(_base: Base, *, cancel: object) -> BuildCheck:
        assert cancel is cancel_event
        calls.append("graph")
        cancel_event.set()
        return BuildCheck("graph.tsv", False)

    cancel_event = cancel
    monkeypatch.setattr("fkf.build.build_wiki_index", wiki)
    monkeypatch.setattr("fkf.build.build_graph", graph)
    monkeypatch.setattr("fkf.build.build_lexical_index", lambda _base: pytest.fail("canceled build reached index"))

    with pytest.raises(CanceledError):
        build(base, cancel=cancel)

    assert calls == ["wiki", "graph"]


def test_build_passes_cancellation_to_body_pruning(monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> None:
    base = _base(tmp_path)
    cancel = Event()

    def prune(_base: Base, *, source: str, cancel: object, **kwargs: object) -> object:
        assert source == "source"
        assert cancel is cancel_event
        assert "older_than" in kwargs
        raise CanceledError("operation canceled")

    cancel_event = cancel
    monkeypatch.setattr("fkf.build.prune_bodies", prune)

    with pytest.raises(CanceledError):
        build(
            base,
            BuildOptions(target=BuildTarget.BODIES, prune=True, source="source"),
            cancel=cancel,
        )
