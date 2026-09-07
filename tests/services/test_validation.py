from __future__ import annotations

from datetime import UTC, datetime
from pathlib import Path

from fkf.base import Base
from fkf.config import load_config
from fkf.documents import build_document
from fkf.markdown import Severity
from fkf.store import Layer
from fkf.validation import validate_all, validate_knowledge_lint, validate_markdown_layer, validate_record_titles

CONFIG = """\
fkf: 1
name: brain
schema:
  id: {description: Stable identity., cardinality: one}
  title: {description: Subject., cardinality: optional}
  related: {description: Related URI., cardinality: many, relation: true}
  note: {description: Plain note., cardinality: optional}
layers: {events: false, index: true, tasks: false, projects: true, wiki: true}
sources:
  snapshot:
    enabled: true
    layer: index
    run: [provider]
    fields: {id: .id, title: .title}
"""


def make_base(tmp_path: Path) -> Base:
    root = tmp_path / "brain"
    root.mkdir()
    (root / "fkf.yaml").write_text(CONFIG)
    config = load_config(root)
    return Base(config=config, store=config.store(), now=lambda: datetime(2026, 9, 6, 12, tzinfo=UTC))


def write(base: Base, uri: str, text: str) -> None:
    target = base.root / uri
    target.parent.mkdir(parents=True, exist_ok=True)
    target.write_text(text)


def test_markdown_validation_adds_relation_and_addressable_link_checks(tmp_path: Path) -> None:
    base = make_base(tmp_path)
    write(base, "wiki/index.md", "# Wiki\n")
    write(
        base,
        "wiki/source.md",
        "---\ntype: decision\ntitle: Source\ntags: [test]\nrelations:\n"
        "  note: [missing.md]\n  related: [target.md]\n---\n\n# Source\n\n[bad](target.md#missing)\n",
    )
    write(base, "wiki/target.md", "---\ntype: insight\ntitle: Target\ntags: [test]\n---\n\n# Target\n")
    report = validate_markdown_layer(base, Layer.WIKI)
    assert report.ok is False
    assert any("not declared as a relation" in issue.message for issue in report.issues)
    assert any("is not addressable" in issue.message for issue in report.issues)


def test_record_title_validation_only_lints_current_projection(tmp_path: Path) -> None:
    base = make_base(tmp_path)
    document = build_document(
        base.source("snapshot"),
        [{"id": "a", "title": "Same"}, {"id": "b", "title": "Same"}, {"id": "c", "title": "Other"}],
        collected_at=base.now(),
    )
    base.write_document(document)
    permissive = validate_record_titles(base)
    assert permissive.warnings == 1
    assert permissive.ok is True
    strict = validate_record_titles(base, strict=True)
    assert strict.errors == 1
    assert strict.issues[0].severity is Severity.ERROR
    assert strict.ok is False


def test_knowledge_lint_ignores_generated_index_links_and_finds_orphans(tmp_path: Path) -> None:
    base = make_base(tmp_path)
    write(
        base,
        "wiki/index.md",
        "# Wiki\n\n<!-- >>> fkf managed block — regenerate with `fkf build wiki`; edits between the markers are lost -->\n"
        "\n- [Generated](orphan.md)\n\n<!-- <<< fkf managed block -->\n",
    )
    write(
        base,
        "wiki/orphan.md",
        "---\ntype: insight\ntitle: Orphan\ntags: [test]\ndue: tomorrow\n---\n\n# Orphan\n",
    )
    report = validate_knowledge_lint(base)
    messages = [issue.message for issue in report.issues]
    assert any("relative date" in message for message in messages)
    assert any("orphan wiki page" in message for message in messages)


def test_validate_all_returns_one_typed_envelope(tmp_path: Path) -> None:
    base = make_base(tmp_path)
    write(base, "wiki/index.md", "# Wiki\n")
    write(
        base,
        "projects/work.md",
        "---\ntype: project\ntitle: Work\ntags: [work]\nstatus: active\n---\n\n# Work\n",
    )
    report = validate_all(base, lint=True)
    assert report.wiki is not None
    assert report.projects is not None
    assert report.records is not None
