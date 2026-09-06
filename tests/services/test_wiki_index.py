from __future__ import annotations

from pathlib import Path
from threading import Event

import pytest

from fkf.base import Base
from fkf.config import Config, SyncConfig
from fkf.errors import CanceledError
from fkf.fields import FieldSchema
from fkf.markdown import Page
from fkf.marked_block import MarkedBlockError
from fkf.store import Layer, Store
from fkf.wiki_index import INDEX_BLOCK_BEGIN, build_wiki_index


def _base(tmp_path: Path) -> Base:
    path = tmp_path / "fkf.yaml"
    path.write_text("name: wiki\n")
    layers = dict.fromkeys(Layer, True)
    config = Config(1, "wiki", FieldSchema(), layers, {}, {}, SyncConfig(), (), path)
    base = Base(config, Store(tmp_path, layers))
    (tmp_path / "wiki").mkdir()
    (tmp_path / "wiki" / "retrieval.md").write_text(
        "---\ntype: decision\ntitle: Retrieval boundary\n"
        "description: Why retrieval is lexical.\ntags: [retrieval, decision]\n---\n\n# Retrieval boundary\n"
    )
    (tmp_path / "wiki" / "sources.md").write_text(
        "---\ntype: pattern\ntitle: Declarative sources\ntags: [collection]\n---\n\n# Declarative sources\n"
    )
    (tmp_path / "wiki" / "log.md").write_text("# Log\n")
    return base


def test_wiki_index_groups_concepts_and_excludes_structural_pages(tmp_path: Path) -> None:
    base = _base(tmp_path)
    report = build_wiki_index(base, write=True)
    assert (report.pages, report.types, report.tags, report.created, report.changed) == (2, 2, 3, True, True)
    content = (tmp_path / "wiki" / "index.md").read_text()
    assert "### decision" in content
    assert "[Retrieval boundary](retrieval.md) — Why retrieval is lexical&#46;" in content
    assert "`collection` · `decision` · `retrieval`" in content
    assert "(log.md)" not in content


def test_wiki_index_preserves_curated_bytes_and_is_idempotent(tmp_path: Path) -> None:
    base = _base(tmp_path)
    curated = "# Wiki\n\nStart [here](retrieval.md).\n"
    (tmp_path / "wiki" / "index.md").write_text(curated)
    first = build_wiki_index(base, write=True)
    settled = (tmp_path / "wiki" / "index.md").read_text()
    second = build_wiki_index(base, write=True)
    assert first.changed
    assert not second.changed
    assert settled.startswith(curated)
    assert settled.count(INDEX_BLOCK_BEGIN) == 1


def test_check_reports_stale_without_writing(tmp_path: Path) -> None:
    base = _base(tmp_path)
    report = build_wiki_index(base, write=False)
    assert report.stale
    assert report.changed
    assert not report.created
    assert not (tmp_path / "wiki" / "index.md").exists()


def test_ambiguous_existing_marker_fails_closed(tmp_path: Path) -> None:
    base = _base(tmp_path)
    original = f"# Wiki\n\n{INDEX_BLOCK_BEGIN}\nnever closed\n"
    (tmp_path / "wiki" / "index.md").write_text(original)
    with pytest.raises(MarkedBlockError, match="no matching end marker"):
        build_wiki_index(base, write=True)
    assert (tmp_path / "wiki" / "index.md").read_text() == original


def test_quoted_marker_is_neutralized_inside_generated_metadata(tmp_path: Path) -> None:
    base = _base(tmp_path)
    (tmp_path / "wiki" / "quoted.md").write_text(
        '---\ntype: concept\ntitle: "Note <!-- <<< fkf managed block -->"\ntags: [x]\n---\n\n# Note\n'
    )
    build_wiki_index(base, write=True)
    first = (tmp_path / "wiki" / "index.md").read_text()
    build_wiki_index(base, write=True)
    assert (tmp_path / "wiki" / "index.md").read_text() == first
    assert first.count("<!-- <<< fkf managed block -->") == 1


def test_wiki_index_cancellation_before_publish_preserves_curated_bytes(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    from fkf import wiki_index

    base = _base(tmp_path)
    path = tmp_path / "wiki" / "index.md"
    path.write_text("# Curated\n", encoding="utf-8")
    original = wiki_index.load_markdown_layer
    cancel = Event()

    def cancel_after_load(selected: Base, layer: Layer, *, cancel: object) -> tuple[tuple[Page, ...], tuple[str, ...]]:
        assert cancel is cancel_event
        result = original(selected, layer, cancel=cancel_event)
        cancel_event.set()
        return result

    cancel_event = cancel
    monkeypatch.setattr(wiki_index, "load_markdown_layer", cancel_after_load)

    with pytest.raises(CanceledError):
        build_wiki_index(base, write=True, cancel=cancel)

    assert path.read_text(encoding="utf-8") == "# Curated\n"
