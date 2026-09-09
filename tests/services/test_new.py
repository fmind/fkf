from __future__ import annotations

from datetime import UTC, datetime
from pathlib import Path
from threading import Event

import pytest

from fkf.base import Base
from fkf.config import load_config
from fkf.errors import CanceledError
from fkf.new import NewKind, NewRequest, create_new, parse_new_kind
from fkf.pages import read_page
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
    return Base(config=config, store=config.store(), now=lambda: datetime(2026, 9, 6, tzinfo=UTC))


def test_new_task_and_pages_are_valid_and_never_replaced(tmp_path: Path) -> None:
    base = make_base(tmp_path)
    task = create_new(base, NewRequest(NewKind.TASK, "port-python"))
    assert task.uri == "tasks/2026-09-06/port-python/TASKS.md"
    assert "## Learned" in task.path.read_text()

    project = create_new(base, NewRequest(NewKind.PROJECT, "fkf-python", tags=("fkf", "python")))
    assert read_page(base, project.uri).status == "active"
    wiki = create_new(
        base,
        NewRequest(NewKind.WIKI, "runtime-boundary", title="Runtime [Boundary]", tags=("fkf",)),
    )
    assert read_page(base, wiki.uri).title == "Runtime [Boundary]"
    original = wiki.path.read_bytes()
    with pytest.raises(ValueError, match="already exists"):
        create_new(base, NewRequest(NewKind.WIKI, "runtime-boundary", tags=("fkf",)))
    assert wiki.path.read_bytes() == original


def test_new_helper_has_explicit_portable_interpreter_contract(tmp_path: Path) -> None:
    base = make_base(tmp_path)
    shell = create_new(base, NewRequest(NewKind.HELPER, "collect-prs.sh"))
    assert shell.path.read_text().startswith("#!/bin/sh\nset -eu\n")
    assert shell.path.stat().st_mode & 0o777 == 0o700
    assert shell.run == ("collect-prs.sh", "{{start}}", "{{end}}")
    assert shell.requires == ("collect-prs.sh",)
    python = create_new(base, NewRequest(NewKind.HELPER, "collect-prs.py"))
    assert python.path.read_text().startswith("#!/usr/bin/env python3\n")
    assert python.requires == ("collect-prs.py", "python3")


@pytest.mark.parametrize(
    "case",
    [
        NewRequest(NewKind.PROJECT, "nested/slug", tags=("test",)),
        NewRequest(NewKind.PROJECT, "project", title="bad\ntitle", tags=("test",)),
        NewRequest(NewKind.PROJECT, "project", tags=("Bad Tag",)),
        NewRequest(NewKind.WIKI, "wiki"),
        NewRequest(NewKind.WIKI, "wiki", type="bad: type", tags=("test",)),
        NewRequest(NewKind.HELPER, "helper"),
    ],
)
def test_new_rejects_unsafe_metadata_before_any_write(tmp_path: Path, case: NewRequest) -> None:
    base = make_base(tmp_path)
    with pytest.raises(ValueError, match=r"\S"):
        create_new(base, case)
    assert not (base.root / "tasks").exists()
    assert not (base.root / "projects").exists()
    assert not (base.root / "wiki").exists()
    assert not (base.root / "sources").exists()


def test_parse_new_kind_keeps_first_letter_aliases() -> None:
    assert parse_new_kind("t") is NewKind.TASK
    assert parse_new_kind("PROJECT") is NewKind.PROJECT
    with pytest.raises(ValueError, match="expected task"):
        parse_new_kind("unknown")


def test_new_cancellation_before_publish_leaves_no_scaffold(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    from fkf import new

    base = make_base(tmp_path)
    cancel = Event()
    original = new._validate_generated_page  # noqa: SLF001 - cancellation seam regression

    def validate_then_cancel(layer: Layer, uri: str, content: bytes, *, require_status: bool) -> None:
        original(layer, uri, content, require_status=require_status)
        cancel.set()

    monkeypatch.setattr(new, "_validate_generated_page", validate_then_cancel)

    with pytest.raises(CanceledError):
        create_new(
            base,
            NewRequest(NewKind.WIKI, "cancel-me", tags=("test",)),
            cancel=cancel,
        )

    assert not (base.root / "wiki/cancel-me.md").exists()
