from __future__ import annotations

import os
from datetime import UTC, date, datetime
from pathlib import Path
from threading import Event

import pytest

from fkf.base import Base
from fkf.config import load_config
from fkf.errors import CanceledError
from fkf.markdown import Severity
from fkf.store import Layer, Store
from fkf.validation import (
    _date_like,
    _frontmatter_scalar,
    _relative_date,
    validate_all,
    validate_knowledge_lint,
    validate_markdown_layer,
    validate_record_titles,
)

CONFIG = """\
fkf: 1
name: brain
schema:
  id: {description: Stable identity., cardinality: one}
  title: {description: Subject., cardinality: optional}
  note: {description: Plain note., cardinality: optional}
  related: {description: Related URI., cardinality: optional, relation: true}
  supersedes: {description: Superseded URI., cardinality: many, relation: true}
layers: {events: false, index: true, tasks: false, projects: true, wiki: true}
sources: {}
"""


def make_base(tmp_path: Path) -> Base:
    root = tmp_path / "brain"
    root.mkdir()
    (root / "fkf.yaml").write_text(CONFIG, encoding="utf-8")
    config = load_config(root)
    return Base(config=config, store=config.store(), now=lambda: datetime(2026, 9, 6, 12, tzinfo=UTC))


def write(base: Base, uri: str, text: str) -> Path:
    target = base.root / uri
    target.parent.mkdir(parents=True, exist_ok=True)
    target.write_text(text, encoding="utf-8")
    return target


def test_markdown_validation_reports_relation_cardinality_and_link_addressability(tmp_path: Path) -> None:
    base = make_base(tmp_path)
    write(base, "wiki/index.md", "# Wiki\n")
    write(base, "wiki/target.md", "---\ntype: insight\ntitle: Target\ntags: [test]\n---\n\n# Target\n")
    write(
        base,
        "wiki/source.md",
        """\
---
type: decision
title: Source
tags: [test]
relations:
  unknown: [target.md]
  note: [target.md]
  related: [target.md, absent.md]
  supersedes: ["https://example.com/old", "target.md#target", absent.md]
---

# Source

[external](https://example.com) [missing](missing.md) [bad anchor](target.md#absent) [target](target.md)
""",
    )

    report = validate_markdown_layer(base, Layer.WIKI, strict=True)
    messages = [issue.message for issue in report.issues]

    assert report.ok is False
    assert report.errors == len(report.issues)
    assert any("is not declared in fkf.yaml schema" in message for message in messages)
    assert any("is not declared as a relation" in message for message in messages)
    assert any("cardinality optional" in message for message in messages)
    assert any("points at wiki/missing.md, which does not exist" in message for message in messages)
    assert any("target.md#absent" in message and "not addressable" in message for message in messages)


def test_knowledge_lint_covers_supersession_project_freshness_and_inbound_edges(tmp_path: Path) -> None:
    base = make_base(tmp_path)
    write(base, "wiki/index.md", "# Wiki\n\n[Current](current.md)\n")
    write(
        base,
        "wiki/current.md",
        """\
---
type: decision
title: Current
tags: [test]
due: next week
relations:
  supersedes: ["https://example.com/old", "old.md#old", missing.md]
---

# Current

[Old](old.md) [External](mailto:owner@example.com)
""",
    )
    write(base, "wiki/old.md", "---\ntype: insight\ntitle: Old\ntags: [test]\n---\n\n# Old\n")
    project = write(
        base,
        "projects/stale.md",
        """\
---
type: project
title: Stale
tags: [work]
status: active
valid_until: 2026-01-01
---

# Stale
""",
    )
    old = datetime(2025, 1, 1, tzinfo=UTC).timestamp()
    os.utime(project, (old, old))

    report = validate_knowledge_lint(base, strict=False, stale_days=90)
    messages = [issue.message for issue in report.issues]

    assert report.ok is True
    assert report.errors == 0
    assert all(issue.severity is Severity.WARNING for issue in report.issues)
    assert any("relative date" in message for message in messages)
    assert any("must be a wiki or project page URI" in message for message in messages)
    assert any("must name a whole page" in message for message in messages)
    assert any("supersedes target 'missing.md' does not exist" in message for message in messages)
    assert any("active project valid_until" in message for message in messages)
    assert any("project page is untouched" in message for message in messages)
    assert any(issue.uri == "projects/stale.md" and "orphan" not in issue.message for issue in report.issues)
    assert not any(issue.uri == "wiki/old.md" and "orphan" in issue.message for issue in report.issues)

    strict = validate_knowledge_lint(base, strict=True, stale_days=90)
    assert strict.errors == len(strict.issues)
    assert strict.ok is False


def test_lint_date_primitives_and_disabled_layers_are_total(tmp_path: Path) -> None:
    assert _frontmatter_scalar(datetime(2026, 9, 6, 12, 30)) == "2026-09-06T12:30:00"
    assert _frontmatter_scalar(date(2026, 9, 6)) == "2026-09-06"
    assert _frontmatter_scalar(7) == "7"
    assert _frontmatter_scalar(True) == "true"
    assert _frontmatter_scalar({"nested": True}) is None
    for name in ("date", "due", "start", "end", "valid_from", "valid_until", "created_at", "review_date"):
        assert _date_like(name)
    assert not _date_like("candidate")
    for value in ("today", "YESTERDAY", "next week", "two days ago", "a month from now"):
        assert _relative_date(value)
    assert not _relative_date("2026-09-06")

    base = make_base(tmp_path)
    with pytest.raises(ValueError, match="must be positive"):
        validate_knowledge_lint(base, stale_days=0)

    base.store = Store(base.root)
    bundle = validate_all(base, lint=False)
    assert bundle.wiki is None
    assert bundle.projects is None
    assert bundle.lint is None
    assert bundle.ok is True


def test_validation_entrypoints_honor_preexisting_cancellation(tmp_path: Path) -> None:
    base = make_base(tmp_path)
    cancel = Event()
    cancel.set()

    operations = (
        lambda: validate_markdown_layer(base, Layer.WIKI, cancel=cancel),
        lambda: validate_record_titles(base, cancel=cancel),
        lambda: validate_knowledge_lint(base, cancel=cancel),
        lambda: validate_all(base, lint=True, cancel=cancel),
    )
    for operation in operations:
        with pytest.raises(CanceledError, match="operation canceled"):
            operation()
