from __future__ import annotations

import json
from datetime import UTC, datetime, timedelta, timezone

import pytest

from fkf.jsoncodec import dumps
from fkf.markdown import (
    PROJECT_STATUSES,
    Heading,
    MarkdownError,
    Severity,
    find_invisible,
    markdown_code_text,
    markdown_literal_text,
    parse_page,
    validate_pages,
)

MODIFIED = datetime(2026, 5, 10, 12, 0, tzinfo=UTC)


def test_parse_page_preserves_unknown_frontmatter_and_extracts_real_links() -> None:
    page = parse_page(
        "wiki/a.md",
        b"""---
type: decision
title: A decision
tags: [architecture, retrieval]
invented_by_a_teammate: keep me
---

# A decision

Body text with a [link](../projects/p.md) and an ![image](img.png).
""",
        MODIFIED,
    )

    assert (page.type, page.title, page.slug) == ("decision", "A decision", "a")
    assert page.tags == ("architecture", "retrieval")
    assert page.frontmatter["invented_by_a_teammate"] == "keep me"
    assert [link.target for link in page.links] == ["../projects/p.md", "img.png"]
    assert page.updated == "2026-05-10T12:00:00Z"


def test_frontmatter_rejects_recursive_aliases() -> None:
    recursive = b"---\nx: &x [*x]\n---\n# A\n"
    with pytest.raises(MarkdownError, match="recursive YAML alias"):
        parse_page("wiki/recursive.md", recursive)


def test_frontmatter_rejects_excessively_expanded_aliases() -> None:
    lines = ["a0: &a0 [x]"]
    lines.extend(f"a{depth}: &a{depth} [" + ",".join([f"*a{depth - 1}"] * 10) + "]" for depth in range(1, 6))
    expanded = ("---\n" + "\n".join(lines) + "\nx: *a5\n---\n# A\n").encode()
    with pytest.raises(MarkdownError, match="YAML alias expansion exceeds"):
        parse_page("wiki/expanded.md", expanded)


def test_frontmatter_bounds_repeated_large_scalar_aliases() -> None:
    value = "x" * 5000
    aliases = ",".join(["*shared"] * 1000)
    expanded = f"---\nshared: &shared {value}\nitems: [{aliases}]\n---\n# A\n".encode()
    with pytest.raises(MarkdownError, match="byte scalar limit"):
        parse_page("wiki/scalar-expanded.md", expanded)


def test_frontmatter_rejects_excessive_nesting() -> None:
    nested = "[" * 130 + "x" + "]" * 130
    with pytest.raises(MarkdownError, match="128-level depth limit"):
        parse_page("wiki/deep.md", f"---\nx: {nested}\n---\n# A\n".encode())


def test_frontmatter_wraps_yaml_parser_and_constructor_limits() -> None:
    parser_deep = "[" * 1000 + "x" + "]" * 1000
    with pytest.raises(MarkdownError, match="nesting exceeds the parser limit"):
        parse_page("wiki/parser-deep.md", f"---\nx: {parser_deep}\n---\n# A\n".encode())
    with pytest.raises(MarkdownError, match="parse YAML frontmatter"):
        parse_page("wiki/invalid-date.md", b"---\nx: 2026-99-99\n---\n# A\n")


def test_frontmatter_keeps_bounded_ordinary_aliases() -> None:
    page = parse_page(
        "wiki/aliases.md",
        b"---\nshared: &shared [one, two]\nfirst: *shared\nsecond: *shared\n---\n# Aliases\n",
    )

    assert page.frontmatter["first"] == ["one", "two"]
    assert page.frontmatter["second"] == ["one", "two"]
    assert b'"first":["one","two"]' in dumps(page)


@pytest.mark.parametrize(
    "frontmatter",
    [
        "title: First\ntitle: Second",
        "metadata:\n  owner: first\n  owner: second",
    ],
)
def test_frontmatter_rejects_duplicate_mapping_keys(frontmatter: str) -> None:
    with pytest.raises(MarkdownError, match="duplicate YAML mapping key"):
        parse_page("wiki/duplicate.md", f"---\n{frontmatter}\n---\n# Duplicate\n".encode())


def test_frontmatter_normalizes_yaml_timestamps_at_the_json_boundary() -> None:
    page = parse_page(
        "wiki/timestamps.md",
        b"""---
published: 2026-09-06
reviewed: 2026-09-06T12:34:56Z
localized: 2026-09-06T14:34:56+02:00
fractional: 2026-09-06T12:34:56.120000Z
history: [2026-09-05, 2026-09-06 09:00:00]
---
# Timestamps
""",
    )

    encoded = json.loads(dumps(page))
    assert page.frontmatter == {
        "published": "2026-09-06T00:00:00Z",
        "reviewed": "2026-09-06T12:34:56Z",
        "localized": "2026-09-06T14:34:56+02:00",
        "fractional": "2026-09-06T12:34:56.12Z",
        "history": ["2026-09-05T00:00:00Z", "2026-09-06T09:00:00Z"],
    }
    assert encoded["frontmatter"] == page.frontmatter


def test_frontmatter_rejects_yaml_values_outside_the_json_boundary() -> None:
    with pytest.raises(MarkdownError, match=r"frontmatter\.labels.*set"):
        parse_page("wiki/set.md", b"---\nlabels: !!set {one: null, two: null}\n---\n# Set\n")


def test_parse_page_normalizes_modified_time_to_utc() -> None:
    modified = datetime(2026, 5, 10, 14, 30, 59, 999999, tzinfo=timezone(timedelta(hours=2)))

    page = parse_page("wiki/a.md", b"# A\n", modified)

    assert page.updated == "2026-05-10T12:30:59Z"


def test_parse_page_renders_yaml_scalars_and_carries_validity_relations() -> None:
    page = parse_page(
        "wiki/2026.md",
        b"""---
type: insight
title: 2026
description: 20.26
date: 2026-08-28
valid_from: 2026-05-01
valid_until: 2026-06-01
tags: [2026, retrieval]
relations:
  supersedes: [wiki/old.md]
---

# Fallback title
""",
        MODIFIED,
    )

    assert page.title == "2026"
    assert page.description == "20.26"
    assert page.date == "2026-08-28"
    assert page.frontmatter["date"] == "2026-08-28T00:00:00Z"
    assert page.tags == ("2026", "retrieval")
    assert page.valid_from == "2026-05-01"
    assert page.valid_until == "2026-06-01"
    assert page.relations == {"supersedes": ("wiki/old.md",)}
    assert page.valid_at("2026-05-01")
    assert page.valid_at("2026-06-01")
    assert not page.valid_at("2026-06-02")


def test_yaml_12_words_remain_text_and_timestamps_render_deterministically() -> None:
    page = parse_page(
        "wiki/scalars.md",
        b"""---
type: yes
title: true
description: 2026-08-28T12:34:56+02:00
tags: "on, off false"
---
# Scalars
""",
        MODIFIED,
    )

    assert page.type == "yes"
    assert page.title == "true"
    assert page.description == "2026-08-28T12:34:56+02:00"
    assert page.tags == ("on", "off", "false")


def test_links_and_headings_in_code_or_raw_html_are_skipped() -> None:
    page = parse_page(
        "wiki/a.md",
        b"""# Real

A real [link](real.md).

```markdown
[fenced](fenced.md)
# Fenced heading
```

Inline `[code](code.md)` stays out.

    [indented](indented.md)
    # Indented heading

<!-- [comment](comment.md) -->
<pre>
[raw HTML](raw-html.md)
</pre>
""",
        MODIFIED,
    )

    assert [heading.text for heading in page.headings] == ["Real"]
    assert [link.target for link in page.links] == ["real.md"]


def test_every_commonmark_code_form_stays_literal() -> None:
    page = parse_page(
        "wiki/a.md",
        b"# A\n\n"
        b"A real [link](real.md).\n\n"
        b"```markdown\n[fenced](fenced.md)\n```\n\n"
        b"~~~\n[tilde-fenced](tilde.md)\n~~~\n\n"
        b"Inline `[code](code.md)` stays out too.\n"
        b"A double-backtick span ``[double](double.md)`` is literal too.\n"
        b"A double-backtick span `` `[nested](nested.md)` `` is literal too.\n"
        b"An escaped opening bracket \\[escaped](escaped.md) is not a link.\n\n"
        b"    [indented code](indented.md)\n\n"
        b"   \t[tab-expanded code](tab-expanded.md)\n\n"
        b"````markdown\n[four-backtick fence](four.md)\n``` trailing text\n"
        b"[still fenced](still-fenced.md)\n````\n\n"
        b"A multiline `code span starts\n[multiline code](multiline.md)\nand ends here`.\n\n"
        b"<!--\n[HTML comment](comment.md)\n-->\n\n"
        b"<pre>\n[raw HTML](raw-html.md)\n</pre>\n",
        MODIFIED,
    )

    assert [link.target for link in page.links] == ["real.md"]


def test_setext_and_duplicate_headings_carry_visible_text_anchors_and_source_lines() -> None:
    page = parse_page(
        "wiki/a.md",
        b"""Rendered *title*
==================

## Key Outcomes

### [Sub](target.md)

## Key Outcomes

## Key Outcomes-1
""",
        MODIFIED,
    )

    assert page.title == "Rendered title"
    assert page.headings == (
        Heading(level=1, text="Rendered title", anchor="rendered-title", line=1),
        Heading(level=2, text="Key Outcomes", anchor="key-outcomes", line=4),
        Heading(level=3, text="Sub", anchor="sub", line=6),
        Heading(level=2, text="Key Outcomes", anchor="key-outcomes-1", line=8),
        Heading(level=2, text="Key Outcomes-1", anchor="key-outcomes-1-1", line=10),
    )


def test_inline_link_titles_are_metadata_and_destinations_preserve_commonmark_escaping() -> None:
    page = parse_page(
        "projects/p.md",
        rb"""# P

[Ticket](https://acme/browse/FK-412 "../events/x.json#FK-412")
[Balanced](https://en.wikipedia.org/wiki/Foo_(bar))
[Escaped](https://example.test/Foo_\(bar\))
""",
        MODIFIED,
    )

    assert [(link.target, link.title, link.via) for link in page.links] == [
        ("https://acme/browse/FK-412", "../events/x.json#FK-412", "markdown-inline"),
        ("https://en.wikipedia.org/wiki/Foo_(bar)", "", "markdown-inline"),
        ("https://example.test/Foo_(bar)", "", "markdown-inline"),
    ]


def test_reference_definition_is_the_only_reference_edge_and_uses_its_line() -> None:
    page = parse_page(
        "wiki/a.md",
        b"# A\n\nSee [the page][ref] twice [again][ref].\n\n[ref]: https://example.test/Foo_\\(bar\\)?a=1&amp;b=2\n",
        MODIFIED,
    )

    assert len(page.links) == 1
    assert page.links[0].target == "https://example.test/Foo_(bar)?a=1&b=2"
    assert page.links[0].via == "markdown-reference"
    assert page.links[0].line == 5


def test_url_autolinks_are_extracted_without_inventing_email_uris() -> None:
    page = parse_page(
        "wiki/a.md",
        b"# A\n\n<https://example.test/Foo_\\(bar\\)?a=1&amp;b=2> <person@example.test>\n",
        MODIFIED,
    )

    assert [(link.target, link.via, link.line) for link in page.links] == [
        ("https://example.test/Foo_(bar)?a=1&b=2", "markdown-autolink", 3)
    ]


def test_inline_links_use_their_own_source_line_inside_a_multiline_paragraph() -> None:
    page = parse_page(
        "wiki/a.md",
        b"# A\n\nfirst line\nsecond [target](target.md)\nthird ![image](image.png)\n",
        MODIFIED,
    )

    assert [(link.target, link.line) for link in page.links] == [("target.md", 4), ("image.png", 5)]


@pytest.mark.parametrize(
    ("frontmatter", "message"),
    [
        ("relations: []", "relations must be a mapping"),
        ("relations:\n  related: one", "relations.related must be a URI list"),
        ("relations:\n  related: [wiki/a.md, {}]", "relations.related must contain only non-empty URI strings"),
    ],
)
def test_parse_page_rejects_invalid_relation_shapes(frontmatter: str, message: str) -> None:
    with pytest.raises(MarkdownError, match=message):
        parse_page("wiki/a.md", f"---\n{frontmatter}\n---\n# A\n".encode(), MODIFIED)


def test_parse_page_rejects_unclosed_or_non_mapping_frontmatter() -> None:
    with pytest.raises(MarkdownError, match="closing delimiter"):
        parse_page("wiki/a.md", b"---\ntype: decision\n\n# A\n", MODIFIED)
    with pytest.raises(MarkdownError, match="frontmatter must be a mapping"):
        parse_page("wiki/a.md", b"---\n- one\n- two\n---\n# A\n", MODIFIED)


def test_frontmatter_split_preserves_crlf_body_bytes_and_lines() -> None:
    page = parse_page("wiki/a.md", b"---\r\ntitle: A\r\n---\r\n\r\n# A\r\n", MODIFIED)

    assert page.body == "\r\n# A\r\n"
    assert page.headings == (Heading(level=1, text="A", anchor="a", line=5),)


def test_find_invisible_and_literal_rendering_fail_visible() -> None:
    assert find_invisible("ordinary text") is None
    assert find_invisible("hidden\u200binstruction") == ("\u200b", "zero-width space")
    assert markdown_literal_text("# [unsafe](x) <b> & value") == (
        "&#35; &#91;unsafe&#93;&#40;x&#41; &#60;b&#62; &#38; value"
    )
    assert markdown_code_text("safe-tag") == "safe-tag"
    assert markdown_code_text("bad`tag") == "bad&#96;tag"


def test_validate_pages_applies_local_wiki_and_project_rules() -> None:
    pages = [
        parse_page("wiki/index.md", b"Just prose.\n", MODIFIED),
        parse_page("wiki/log.md", b"# Log\n\n## 2026-05-09\n\n## 2026-05-10\n\n## 2026-05-10\n", MODIFIED),
        parse_page(
            "wiki/Bad Slug.md",
            "---\ntitle: Hidden\u200b title\ntags: [Bad_Tag]\n---\n\n## !!!\n".encode(),
            MODIFIED,
        ),
    ]

    report = validate_pages(pages, layer="wiki", require_status=False, strict=False, nested=("wiki/nested/deep.md",))

    assert not report.ok
    messages = "\n".join(issue.message for issue in report.issues)
    for expected in (
        "level-one heading",
        "newest first",
        "repeats the date",
        "slug",
        "invisible character",
        "no addressable anchor",
        "layer is flat",
    ):
        assert expected in messages
    assert any(issue.severity is Severity.WARNING for issue in report.issues)

    strict = validate_pages(pages, layer="wiki", require_status=False, strict=True)
    assert strict.warnings == 0
    assert strict.errors >= report.errors


def test_validate_projects_requires_the_closed_status_vocabulary() -> None:
    assert PROJECT_STATUSES == ("active", "paused", "done")
    pages = [
        parse_page(
            "projects/ok.md", b"---\ntype: project\ntitle: OK\nstatus: active\ntags: [x]\n---\n# OK\n", MODIFIED
        ),
        parse_page("projects/missing.md", b"---\ntype: project\ntitle: Missing\ntags: [x]\n---\n# Missing\n", MODIFIED),
        parse_page(
            "projects/bad.md", b"---\ntype: project\ntitle: Bad\nstatus: someday\ntags: [x]\n---\n# Bad\n", MODIFIED
        ),
    ]

    report = validate_pages(pages, layer="projects", require_status=True, strict=False)

    assert report.errors == 2
    assert all("active, paused, or done" in issue.message for issue in report.issues)
