from __future__ import annotations

import re

import pytest

from fkf.assets import read_asset
from fkf.uri import Scheme, URIError, anchor_slug, parse_uri, relative_link, resolve_link


def test_parse_uri_separates_path_fragment_and_jq() -> None:
    uri = parse_uri("events/2026-05-04/google-gmail-emails.json?jq=.payload.headers#18c2a9f")

    assert uri.scheme is Scheme.FILE
    assert uri.path == "events/2026-05-04/google-gmail-emails.json"
    assert uri.fragment == "18c2a9f"
    assert uri.jq == ".payload.headers"
    assert uri.node_uri() == "events/2026-05-04/google-gmail-emails.json#18c2a9f"
    assert uri.file_uri() == "events/2026-05-04/google-gmail-emails.json"


def _skill_uri_examples() -> list[str]:
    skill = read_asset("skills/fkf-use/SKILL.md").decode()
    section = skill.split("\n## URIs\n", maxsplit=1)[1].split("\n## ", maxsplit=1)[0]
    candidates = (match.strip() for match in re.findall(r"`([^`\n]+)`", section))
    examples = [candidate for candidate in candidates if _looks_addressable(candidate)]
    assert len(examples) >= 15, "the skill's URI table is the public parser contract"
    return examples


def _looks_addressable(candidate: str) -> bool:
    if len(candidate) < 3 or not candidate[0].islower() or any(character in candidate for character in "<>[] "):
        return False
    if candidate.startswith("--"):
        return False
    return (
        bool(re.fullmatch(r"[a-z][a-z0-9+.-]*:.+", candidate))
        or any(marker in candidate for marker in ("://", ".json", ".md", ".yaml", ".tsv"))
        or candidate.endswith("/")
    )


def test_every_documented_uri_form_parses_and_round_trips() -> None:
    for raw in _skill_uri_examples():
        first = parse_uri(raw)
        second = parse_uri(str(first))

        assert str(second) == str(first), raw
        assert second == first


@pytest.mark.parametrize(
    ("raw", "canonical", "value"),
    [
        ("person:Ops%25Team@Example.Test", "person:Ops%25Team@Example.Test", "Ops%Team@Example.Test"),
        ("tag:line%0Abreak", "tag:line%0Abreak", "line\nbreak"),
        ("repo:owner/control%01name", "repo:owner/control%01name", "owner/control\x01name"),
    ],
)
def test_entity_round_trip_encodes_literal_percent_and_controls(raw: str, canonical: str, value: str) -> None:
    first = parse_uri(raw)

    assert str(first) == canonical
    assert first.value == value
    assert first.is_entity()
    assert parse_uri(str(first)) == first
    assert not any(character in str(first) for character in "\n\r\t\x01")


@pytest.mark.parametrize(
    ("raw", "message"),
    [
        ("", "empty"),
        ("../etc/passwd", "escapes the base"),
        ("events/../../etc/passwd", "escapes the base"),
        ("/etc/passwd", "is absolute"),
        ("~/secrets", "home-relative"),
        ("events/x.json?limit=10", "the only supported query is ?jq="),
        ("events/x.json?jq=", "names no expression"),
        ("wiki/x.md#", "names no fragment"),
        ("events/2026-05-04/#id", "a directory has no fragment"),
        ("person:", "names no person"),
        ("file:wiki/x.md", "reserved"),
        ("external:example.test", "reserved"),
        (".", "base root is not addressable"),
        ("./", "base root is not addressable"),
    ],
)
def test_parse_uri_rejects_invalid_or_unpublished_addresses(raw: str, message: str) -> None:
    with pytest.raises(URIError, match=re.escape(message)):
        parse_uri(raw)


@pytest.mark.parametrize("raw", ["http://example.test", "mailto:person@example.test", "ftp://example.test/file"])
def test_parse_uri_rejects_unpublished_external_schemes(raw: str) -> None:
    with pytest.raises(URIError):
        parse_uri(raw)

    assert str(parse_uri("https://example.test/path")) == "https://example.test/path"


@pytest.mark.parametrize(
    ("source", "target", "expected"),
    [
        ("wiki/a.md", "b.md", "wiki/b.md"),
        ("wiki/a.md", "../projects/p.md", "projects/p.md"),
        ("wiki/a.md", "../events/2026-05-04/x.json#a1", "events/2026-05-04/x.json#a1"),
        ("tasks/2026-05-04/review/TASKS.md", "../../../projects/fkf.md#decisions", "projects/fkf.md#decisions"),
        ("wiki/a.md", "/wiki/b.md", "wiki/b.md"),
        ("wiki/a.md", "ticket:FK-412", "ticket:FK-412"),
        ("wiki/a.md", "https://example.test/x", "https://example.test/x"),
    ],
)
def test_resolve_link_follows_the_linking_file(source: str, target: str, expected: str) -> None:
    assert resolve_link(source, target).node_uri() == expected


def test_resolve_link_rejects_escape_and_preserves_url_shaped_fragment() -> None:
    with pytest.raises(URIError, match="escapes the base"):
        resolve_link("wiki/a.md", "../../../etc/passwd")

    resolved = resolve_link("wiki/a.md", "../events/2026-08-22/rss.json#https://example.test/post")
    assert resolved.path == "events/2026-08-22/rss.json"
    assert resolved.fragment == "https://example.test/post"


@pytest.mark.parametrize(
    ("source", "target", "expected"),
    [
        ("wiki/a.md", "wiki/b.md", "b.md"),
        ("wiki/a.md", "projects/p.md", "../projects/p.md"),
        ("tasks/2026-05-04/review/TASKS.md", "projects/fkf.md#decisions", "../../../projects/fkf.md#decisions"),
    ],
)
def test_relative_link_is_the_inverse(source: str, target: str, expected: str) -> None:
    rendered = relative_link(source, target)

    assert rendered == expected
    assert resolve_link(source, rendered).node_uri() == target


@pytest.mark.parametrize(
    ("heading", "expected"),
    [
        ("Verification", "verification"),
        ("Key Outcomes", "key-outcomes"),
        ("What it protects, and what it does not", "what-it-protects-and-what-it-does-not"),
        ("URIs and the graph", "uris-and-the-graph"),
        ("`fkf init`", "fkf-init"),
        ("[Deployment guide](https://example.test)", "deployment-guide"),
        ("![Launch diagram](diagram.png)", "launch-diagram"),
        ("<span>Inline</span> **markup**", "inline-markup"),
        ("Deploy <!-- internal note -->", "deploy"),
        ("Fix FK&#45;412", "fix-fk-412"),
        ("1&#46; Scope", "1-scope"),
        ("Nebula&#58; Northport&#39;s", "nebula-northports"),
        ("Decision \N{EN DASH} launch", "decision--launch"),
        ("🚀 Launch", "-launch"),
        ("Привет non-latin 你好", "привет-non-latin-你好"),
        ("Cafe\u0301", "cafe\u0301"),
    ],
)
def test_anchor_slug_follows_github_rules(heading: str, expected: str) -> None:
    assert anchor_slug(heading) == expected
