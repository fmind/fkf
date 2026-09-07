from __future__ import annotations

from pathlib import Path

import pytest

from fkf.base import Base
from fkf.config import ConfigError, load_config
from fkf.pages import PageFilter, build_tag_vocabulary, list_pages, search_pages
from fkf.store import Layer

CONFIG = """\
fkf: 1
name: brain
schema:
  id: {description: Stable identity., cardinality: one}
  time: {description: Event time., cardinality: one}
  title: {description: Meaningful title., cardinality: optional}
layers: {events: true, index: true, tasks: true, projects: true, wiki: true}
sources: {}
"""


def make_base(tmp_path: Path) -> Base:
    root = tmp_path / "brain"
    root.mkdir()
    (root / "fkf.yaml").write_text(CONFIG)
    config = load_config(root)
    return Base(config=config, store=config.store())


def write_page(base: Base, name: str, *, title: str, tags: str, body: str, status: str = "") -> None:
    directory = base.root / "wiki"
    directory.mkdir(exist_ok=True)
    status_line = f"status: {status}\n" if status else ""
    (directory / name).write_text(
        f"---\ntype: insight\ntitle: {title}\ntags: [{tags}]\n{status_line}---\n\n# {title}\n\n{body}\n"
    )


def test_page_listing_filters_conjunctively_and_strips_search_body(tmp_path: Path) -> None:
    base = make_base(tmp_path)
    write_page(base, "retrieval.md", title="Retrieval boundary", tags="decision, retrieval", body="Exact proof.")
    write_page(base, "other.md", title="Other", tags="other", body="Retrieval mentioned in prose.")

    listing = list_pages(base, Layer.WIKI)
    assert listing.total == 2
    assert [page.slug for page in listing.pages] == ["other", "retrieval"]
    assert all(not page.body and not page.links and not page.headings for page in listing.pages)

    both = list_pages(base, Layer.WIKI, PageFilter(tags=("Decision", "retrieval")))
    disjoint = list_pages(base, Layer.WIKI, PageFilter(tags=("decision", "other")))
    assert [page.slug for page in both.pages] == ["retrieval"]
    assert disjoint.total == 0


def test_page_listing_refuses_unknown_closed_values(tmp_path: Path) -> None:
    base = make_base(tmp_path)
    write_page(base, "one.md", title="One", tags="real", body="Body")
    with pytest.raises(ConfigError, match=r"unknown tag.*absent.*real"):
        list_pages(base, Layer.WIKI, PageFilter(tags=("absent",)))
    with pytest.raises(ConfigError, match=r"unknown status.*wibble.*active"):
        list_pages(base, Layer.WIKI, PageFilter(status="wibble"))


def test_tag_vocabulary_is_usage_then_name_ordered(tmp_path: Path) -> None:
    base = make_base(tmp_path)
    write_page(base, "a.md", title="A", tags="shared, alpha", body="")
    write_page(base, "b.md", title="B", tags="shared, beta", body="")
    write_page(base, "c.md", title="C", tags="", body="")
    vocabulary = build_tag_vocabulary(base, Layer.WIKI)
    assert [(tag.tag, tag.count, tag.pages) for tag in vocabulary.tags] == [
        ("shared", 2, ("a", "b")),
        ("alpha", 1, ("a",)),
        ("beta", 1, ("b",)),
    ]


def test_search_requires_every_term_and_ranks_title_before_body(tmp_path: Path) -> None:
    base = make_base(tmp_path)
    write_page(base, "title.md", title="Alpha beta retrieval", tags="test", body="Body")
    write_page(base, "body.md", title="Something else", tags="test", body="Mentions alpha beta retrieval.")
    write_page(base, "partial.md", title="Alpha only", tags="test", body="No second term")

    result = search_pages(base, Layer.WIKI, ["alpha", "beta"])
    assert [hit.slug for hit in result.hits] == ["title", "body"]
    assert result.hits[0].score > result.hits[1].score
    assert "Mentions alpha beta retrieval." in result.hits[1].excerpt
    with pytest.raises(ValueError, match="at least one term"):
        search_pages(base, Layer.WIKI, [])


def test_nested_pages_are_not_silently_loaded(tmp_path: Path) -> None:
    base = make_base(tmp_path)
    nested = base.root / "wiki" / "nested"
    nested.mkdir(parents=True)
    (nested / "hidden.md").write_text("# Hidden\n")
    from fkf.pages import load_markdown_layer

    pages, directories = load_markdown_layer(base, Layer.WIKI)
    assert pages == ()
    assert directories == ("wiki/nested/",)
