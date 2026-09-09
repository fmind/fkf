"""Answer-bearing retrieval and normalization regressions, without providers."""

from pathlib import Path

import pytest

from fkf.context import ContextRequest, _body_excerpt, build_context, render_context_text
from fkf.documents import Record
from fkf.text import lower, terms
from tests.services.test_context import make_base, write_event, write_page


@pytest.mark.parametrize("indexed", [False, True])
@pytest.mark.parametrize(("query", "answer"), [("cerulean", "cerulean rollout"), ("amber", "amber approval")])
def test_project_commitments_are_searchable(tmp_path: Path, indexed: bool, query: str, answer: str) -> None:
    from fkf.lexical import build_lexical_index

    base = make_base(tmp_path)
    write_page(base, "projects/orion.md", "Orion", "Use transactional persistence.", status="active")
    path = base.root / "projects/orion.md"
    path.write_text(
        path.read_text().replace(
            "status: active", "status: active\nnext_action: Verify cerulean rollout.\nblocker: Await amber approval."
        )
    )
    if indexed:
        build_lexical_index(base)
    pack = build_context(base, ContextRequest(query=query, budget=600, delivery_format="text"))
    assert pack.items[0].uri == "projects/orion.md"
    assert answer in render_context_text(pack)
    from fkf.find import FindFilter, find

    matches = find(base, FindFilter(grep=(query,)))
    assert len(matches.pages) == 1
    assert answer in matches.pages[0].excerpt


@pytest.mark.parametrize("indexed", [False, True])
def test_conjunction_in_title_does_not_outrank_a_specific_commitment(tmp_path: Path, indexed: bool) -> None:
    from fkf.lexical import build_lexical_index

    base = make_base(tmp_path)
    write_page(base, "wiki/recovery.md", "Retention and recovery", "Validate the recovery archive.")
    write_page(base, "projects/orion.md", "Orion", "Maintain the workspace.", status="active")
    path = base.root / "projects/orion.md"
    path.write_text(
        path.read_text().replace(
            "status: active",
            "status: active\nnext_action: Validate the current toolkit and shared catalog without disturbing unrelated edits.",
        )
    )
    for number in range(8):
        write_page(base, f"wiki/neutral-{number}.md", f"Neutral {number}", "Unrelated topic.")
    if indexed:
        build_lexical_index(base)
    pack = build_context(
        base,
        ContextRequest(query="Validate the current toolkit and shared catalog", budget=850, delivery_format="text"),
    )
    assert pack.items[0].uri == "projects/orion.md"
    assert "without disturbing unrelated edits" in pack.items[0].excerpt


@pytest.mark.parametrize("indexed", [False, True])
def test_commitment_metadata_cannot_push_a_body_answer_out_of_the_excerpt(tmp_path: Path, indexed: bool) -> None:
    from fkf.lexical import build_lexical_index

    base = make_base(tmp_path)
    write_page(
        base,
        "projects/orion.md",
        "Orion Open Source Course",
        "# Orion Open Source Course\n\n## Intent and accepted decisions\n\nThe Orion course is now Python-first. Preserve learner outcomes and tested safety boundaries.",
        status="active",
    )
    path = base.root / "projects/orion.md"
    path.write_text(
        path.read_text().replace(
            "status: active",
            "status: active\nnext_action: Review the parity work order against the current learner path and select its first bounded implementation slice.\nblocker: The enhancement plan is not implementation or release evidence.\nreviewed: 2026-05-09",
        )
    )
    if indexed:
        build_lexical_index(base)
    pack = build_context(base, ContextRequest(query="What decisions did I make about the Orion course?", budget=1500))
    assert pack.items[0].uri == "projects/orion.md"
    assert "Python-first" in pack.items[0].excerpt


@pytest.mark.parametrize("indexed", [False, True])
@pytest.mark.parametrize("project", ["orion", "atlas"])
def test_small_identity_pack_preserves_active_handoff(tmp_path: Path, indexed: bool, project: str) -> None:
    from fkf.lexical import build_lexical_index

    base = make_base(tmp_path)
    identity = f"ticket:org/{project}"
    records: list[Record] = [
        {
            "id": f"commit-{index}",
            "time": "2026-05-09T12:00:00Z",
            "title": "Recent implementation change",
            "ticket": identity,
        }
        for index in range(8)
    ]
    records.append({"id": identity, "time": "2026-05-09T12:00:00Z", "title": "Repository setup facts"})
    write_event(base, "2026-05-09", records)
    uri = f"projects/{project}.md"
    write_page(base, uri, project.title(), "Decision: use transactional persistence.", status="active")
    path = base.root / uri
    path.write_text(
        path.read_text().replace(
            "status: active",
            f"status: active\nnext_action: Verify recovery before release.\nrelations: {{ticket: [{identity}]}}",
        )
    )
    write_page(base, "projects/aaa-overview.md", "Overview", "Related projects.", status="active")
    overview = base.root / "projects/aaa-overview.md"
    overview.write_text(
        overview.read_text().replace(
            "status: active",
            f"status: active\nnext_action: Review the portfolio.\nrelations: {{ticket: [{identity}, ticket:org/other]}}",
        )
    )
    if indexed:
        build_lexical_index(base)
    pack = build_context(base, ContextRequest(query=identity, budget=600, delivery_format="text"))
    assert pack.items[0].uri == uri
    assert "Verify recovery before release" in render_context_text(pack)
    assert pack.receipt.encoded_tokens <= 600
    facts = build_context(base, ContextRequest(query=f"{identity} setup facts"))
    assert facts.items[0].uri.startswith("events/")
    assert facts.items[0].title == "Repository setup facts"


@pytest.mark.parametrize("cited", [False, True])
def test_learning_receipt_matches_listing_for_nested_lessons_and_fragments(tmp_path: Path, cited: bool) -> None:
    from fkf.learned import list_learned
    from fkf.lexical import build_lexical_index

    base = make_base(tmp_path)
    trace = "tasks/2026-05-09/orion/TASKS.md"
    write_page(base, trace, "Orion", "## Learned\n\n- Verify recovery.\n  - Preserve history.\n")
    write_page(base, "wiki/orion.md", "Orion", "Durable recovery knowledge.")
    if cited:
        path = base.root / "wiki/orion.md"
        path.write_text(path.read_text().replace("title: Orion", f"title: Orion\nsources: [../{trace}#learned]"))
    expected = list_learned(base).unharvested
    assert expected == (0 if cited else 2)
    from fkf.status import StatusRequest, report

    assert report(base, StatusRequest(skip_git_audit=True)).unharvested == expected
    fallback = build_context(base, ContextRequest(query="orion"))
    build_lexical_index(base)
    indexed = build_context(base, ContextRequest(query="orion"))
    assert indexed.receipt.unharvested_bullets == fallback.receipt.unharvested_bullets == expected


def test_fallback_reads_each_task_once_for_selection_and_global_backlog(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    from collections import Counter

    from fkf import listings
    from fkf.base import Base
    from fkf.markdown import Page
    from fkf.process import Cancellation
    from fkf.query import Window
    from fkf.scan import ScanGuard

    base = make_base(tmp_path)
    old = "tasks/2026-05-01/older/TASKS.md"
    current = "tasks/2026-05-09/current/TASKS.md"
    for uri in (old, current):
        write_page(base, uri, "Orion", "## Learned\n\n- Verify recovery.\n")
    calls: Counter[str] = Counter()
    original = listings.read_page

    def tracked_read(
        selected: Base, uri: str, *, cancel: Cancellation | None = None, scan: ScanGuard | None = None
    ) -> Page:
        calls[uri] += 1
        return original(selected, uri, cancel=cancel, scan=scan)

    monkeypatch.setattr(listings, "read_page", tracked_read)
    pack = build_context(base, ContextRequest(query="orion", window=Window("2026-05-09", "2026-05-09")))
    assert pack.receipt.unharvested_bullets == 2
    assert current in {item.uri for item in pack.items}
    assert old not in {item.uri for item in pack.items}
    assert calls == {old: 1, current: 1}


@pytest.mark.parametrize("indexed", [False, True])
def test_question_words_do_not_promote_unrelated_tool_identity(tmp_path: Path, indexed: bool) -> None:
    from fkf.lexical import build_lexical_index

    base = make_base(tmp_path)
    neutral: list[Record] = [
        {"id": f"unrelated-{index}", "time": "2026-05-09T12:00:00Z", "title": "Unrelated catalog entry"}
        for index in range(6)
    ]
    write_event(
        base,
        "2026-05-09",
        [
            {"id": "make", "time": "2026-05-09T12:00:00Z", "title": "A build automation tool"},
            # Keep signal terms below the corpus-wide common-word cutoff.
            *neutral,
        ],
    )
    write_page(
        base, "projects/agentops-open-source.md", "AgentOps Open Course", "Decisions: use Python for the course labs."
    )
    write_page(
        base,
        "wiki/old-memory.md",
        "Historical working notes",
        "We make decisions about many topics. AgentOps course notes mention an old Go upgrade; " * 10,
    )
    if indexed:
        build_lexical_index(base)
    pack = build_context(base, ContextRequest(query="What decisions did I make about the AgentOps course?"))
    assert pack.items[0].uri == "projects/agentops-open-source.md"
    assert "Decisions: use Python" in render_context_text(pack)


def test_excerpt_prefers_answer_bearing_multi_term_passage() -> None:
    body = "AgentOps course overview. " + "Background material. " * 40
    body += "Decisions for the AgentOps course: Python labs replace Go to reuse the teaching stack."
    assert "Python labs replace Go" in _body_excerpt(body, ("agentops", "course", "decisions"))


@pytest.mark.parametrize("indexed", [False, True])
@pytest.mark.parametrize("authored", [False, True])
def test_named_subject_identity_beats_incidental_query_coverage(tmp_path: Path, indexed: bool, authored: bool) -> None:
    from fkf.lexical import build_lexical_index

    base = make_base(tmp_path)
    records: list[Record] = [
        {"id": f"neutral-{index}", "time": "2026-05-09T12:00:00Z", "title": "Unrelated catalog entry"}
        for index in range(6)
    ]
    if authored:
        expected = "projects/orion.md"
        write_page(base, expected, "Orion", "Use transactional persistence.")
    else:
        expected = "events/2026-05-09/synthetic.json#commit-1"
        records.append(
            {
                "id": "commit-1",
                "time": "2026-05-09T12:00:00Z",
                "title": "Improve transaction handling",
                "ticket": "ticket:org/orion",
            }
        )
    write_event(base, "2026-05-09", records)
    write_page(base, "wiki/changelog.md", "Historical notes", "Orion changes mentioned in unrelated working notes.")
    if indexed:
        build_lexical_index(base)
    pack = build_context(base, ContextRequest(query="orion changes"))
    assert pack.items[0].uri == expected


@pytest.mark.parametrize("indexed", [False, True])
def test_ambiguous_relation_leaf_is_not_an_exact_subject(tmp_path: Path, indexed: bool) -> None:
    from fkf.lexical import build_lexical_index

    base = make_base(tmp_path)
    records: list[Record] = [
        {"id": f"neutral-{index}", "time": "2026-05-09T12:00:00Z", "title": "Unrelated catalog entry"}
        for index in range(6)
    ]
    records.extend(
        {"id": owner, "time": "2026-05-09T12:00:00Z", "title": f"{owner} repository", "ticket": f"ticket:{owner}/orion"}
        for owner in ("one", "two")
    )
    write_event(base, "2026-05-09", records)
    expected = "wiki/implementation-rationale.md"
    write_page(base, expected, "Implementation rationale", "Orion decisions favor transactional persistence.")
    if indexed:
        build_lexical_index(base)
    pack = build_context(base, ContextRequest(query="orion decisions"))
    assert pack.items[0].uri == expected
    explicit = build_context(base, ContextRequest(query="ticket:one/orion"))
    assert explicit.items[0].uri.endswith("#one")


def test_unicode_normalization_preserves_single_codepoint_contract() -> None:
    value = "".join(chr(code) for code in range(0x10000))
    expected = "".join(character.lower() if len(character.lower()) == 1 else character for character in value)
    assert lower(value) == expected
    expected_tokens: list[str] = []
    current = ""
    for character in expected:
        if character in "-_.@/:#%" or character.isalpha() or character.isnumeric():
            current += character
        elif current:
            expected_tokens.append(current)
            current = ""
    if current:
        expected_tokens.append(current)
    assert terms(value) == tuple(expected_tokens)
    assert lower("A\u03a3\u03a3 \u0130") == "a\u03c3\u03c3 \u0130"


def test_canonical_fragment_validator_preserves_byte_grammar() -> None:
    from fkf.lexical import _valid_lexical_fragment

    safe = frozenset("._:/@+-")
    for value in range(256):
        character = chr(value)
        literal = value < 128 and (character.isalnum() or character in safe)
        assert _valid_lexical_fragment(character) == literal
        assert _valid_lexical_fragment(f"%{value:02X}") == (not literal)
    for fragment in ("%", "%0", "%ff", "a%FG", "a%F0%9F%98%80%00", "abc-12:/@+._"):
        assert _valid_lexical_fragment(fragment) == (fragment in {"a%F0%9F%98%80%00", "abc-12:/@+._"})
