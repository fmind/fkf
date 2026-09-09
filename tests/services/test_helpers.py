"""Exact-byte contracts for FKF's bundled collection helpers."""

from __future__ import annotations

from pathlib import Path
from threading import Event

import pytest

from fkf.base import Base
from fkf.config import load_config
from fkf.errors import CanceledError
from fkf.helpers import HelperState, inspect_helpers


def make_base(tmp_path: Path) -> Base:
    root = tmp_path / "brain"
    root.mkdir()
    (root / "fkf.yaml").write_text(
        """\
fkf: 1
name: brain
schema:
  id: {description: Stable identity., cardinality: one}
  time: {description: Event time., cardinality: one}
  title: {description: Meaningful title., cardinality: optional}
layers: {events: true}
sources:
  commits:
    enabled: true
    layer: events
    requires: [git-log-json.py]
    run: [git-log-json.py, \"{{date}}\"]
    fields: {id: .id, time: .time, title: .title}
""",
        encoding="utf-8",
    )
    config = load_config(root)
    return Base(config=config, store=config.store())


def test_helpers_report_refresh_and_ignore_custom_files(tmp_path: Path) -> None:
    base = make_base(tmp_path)
    (base.root / "sources").mkdir()
    hook = base.root / "sources" / "fkf-hook.py"
    hook.write_text("#!/bin/sh\necho edited\n", encoding="utf-8")
    hook.chmod(0o700)
    custom = base.root / "sources" / "owner-helper"
    custom.write_text("owner\n", encoding="utf-8")

    before = inspect_helpers(base)
    assert [(item.name, item.state) for item in before.helpers] == [
        ("fkf-hook.py", HelperState.DRIFTED),
        ("git-log-json.py", HelperState.MISSING),
    ]
    assert (before.current, before.drifted, before.missing, before.refreshed) == (0, 1, 1, 0)

    after = inspect_helpers(base, refresh=True)
    assert (after.current, after.drifted, after.missing, after.refreshed) == (2, 0, 0, 2)
    assert all(item.refreshed for item in after.helpers)
    assert (base.root / "sources" / "git-log-json.py").stat().st_mode & 0o777 == 0o700
    assert custom.read_text(encoding="utf-8") == "owner\n"


def test_helper_mode_is_not_content_drift(tmp_path: Path) -> None:
    base = make_base(tmp_path)
    inspect_helpers(base, refresh=True)
    hook = base.root / "sources" / "fkf-hook.py"
    hook.chmod(0o600)

    report = inspect_helpers(base)

    status = next(item for item in report.helpers if item.name == "fkf-hook.py")
    assert status.state is HelperState.CURRENT
    assert report.drifted == 0


def test_helpers_preflight_all_targets_before_refresh(tmp_path: Path) -> None:
    base = make_base(tmp_path)
    (base.root / "sources").mkdir()
    hook = base.root / "sources" / "fkf-hook.py"
    hook.write_text("edited\n", encoding="utf-8")
    outside = tmp_path / "outside"
    (base.root / "sources" / "git-log-json.py").symlink_to(outside)

    with pytest.raises(Exception, match="symlink"):
        inspect_helpers(base, refresh=True)

    assert hook.read_text(encoding="utf-8") == "edited\n"


def test_helpers_fail_closed_on_bin_symlink_and_cancellation(tmp_path: Path) -> None:
    base = make_base(tmp_path)
    outside = tmp_path / "outside"
    outside.mkdir()
    (base.root / "sources").symlink_to(outside, target_is_directory=True)
    with pytest.raises(Exception, match="symlink"):
        inspect_helpers(base)

    (base.root / "sources").unlink()
    canceled = Event()
    canceled.set()
    with pytest.raises(CanceledError):
        inspect_helpers(base, cancel=canceled)
