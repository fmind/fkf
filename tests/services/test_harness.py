"""Hermetic contracts for generated and installed coding-harness integrations."""

from __future__ import annotations

import json
import os
from dataclasses import replace
from pathlib import Path
from threading import Event

import pytest

from fkf.errors import CanceledError, InvalidUsageError, OperationalError
from fkf.harness import (
    HarnessConflictError,
    HarnessInstallRequest,
    harness_names,
    harness_plan_for,
    inspect_harnesses,
    install_harnesses,
    resolve_persistent_launcher,
)

HARNESSES = (
    "claude",
    "codex",
    "gemini",
    "copilot",
    "antigravity",
    "opencode",
    "grok",
    "cursor",
    "kiro",
    "cline",
)
HOOK_HARNESSES = {"claude", "codex", "gemini", "kiro"}


def make_base(tmp_path: Path, name: str = "brain") -> Path:
    root = tmp_path / f"{name}-base"
    (root / "sources").mkdir(parents=True)
    (root / "fkf.yaml").write_text(
        f"fkf: 1\nname: {name}\nschema:\n  id: {{description: Stable identity., cardinality: one}}\nlayers: {{}}\n",
        encoding="utf-8",
    )
    hook = root / "sources" / "fkf-hook.py"
    hook.write_text("#!/usr/bin/env python3\n", encoding="utf-8")
    hook.chmod(0o700)
    return root


def fake_launcher(path: Path):
    def resolve(requested: str, search_path: str) -> Path:
        del requested, search_path
        return path

    return resolve


def test_registry_and_vendor_fragments_bind_one_base_with_workspace_hooks(tmp_path: Path) -> None:
    base = make_base(tmp_path)
    workspace = tmp_path / "workspace"
    workspace.mkdir()
    executable = tmp_path / "stable" / "fkf"
    executable.parent.mkdir()
    resolver = fake_launcher(executable)

    assert harness_names() == HARNESSES
    for name in harness_names():
        plan = harness_plan_for(base, name, workspace=workspace, launcher_resolver=resolver)
        rendered = "\n".join(fragment.content for fragment in plan.fragments)
        assert plan.name == name
        assert plan.base == base
        assert plan.base_name == "brain"
        assert os.fspath(executable) in rendered
        assert "mcp" in rendered
        assert "serve" in rendered
        assert "--base" in rendered
        assert os.fspath(base) in rendered
        assert ("fkf-hook.py" in rendered) is (name in HOOK_HARNESSES)
        if name in HOOK_HARNESSES:
            assert "trust" in rendered
            assert "--check" in rendered
            assert os.fspath(workspace) in rendered

    with pytest.raises(InvalidUsageError, match="unknown harness"):
        harness_plan_for(base, "devin", launcher_resolver=resolver)


def test_persistent_launcher_is_explicit_or_actionably_global(tmp_path: Path) -> None:
    assert resolve_persistent_launcher("/bin/sh", "") == Path("/bin/sh")
    with pytest.raises(InvalidUsageError, match="uv tool install fkf"):
        resolve_persistent_launcher("", os.fspath(tmp_path / "empty-path"))
    cached = tmp_path / ".cache" / "uv" / "archive-v0" / "bin" / "fkf"
    cached.parent.mkdir(parents=True)
    cached.write_text("#!/bin/sh\n", encoding="utf-8")
    cached.chmod(0o700)
    with pytest.raises(InvalidUsageError, match=r"temporary|cache|uvx"):
        resolve_persistent_launcher("", os.fspath(cached.parent))
    assert resolve_persistent_launcher(os.fspath(cached), "") == cached


def test_install_dry_run_check_repair_backup_and_idempotence(tmp_path: Path) -> None:
    base = make_base(tmp_path)
    home = tmp_path / "harness-home"
    home.mkdir()
    executable = tmp_path / "tools" / "fkf"
    resolver = fake_launcher(executable)
    request = HarnessInstallRequest(names=("claude", "codex"), home=home)

    dry = install_harnesses(base, replace(request, dry_run=True), launcher_resolver=resolver)
    assert dry.mode == "dry-run"
    assert dry.complete is False
    assert len(dry.changes) == 2
    assert tuple(home.iterdir()) == ()

    (home / ".claude.json").write_text('{"unrelated":{"keep":true}}\n', encoding="utf-8")
    (home / ".codex").mkdir()
    (home / ".codex" / "config.toml").write_text('user_setting = "keep"\n', encoding="utf-8")
    installed = install_harnesses(base, request, launcher_resolver=resolver)
    assert installed.complete is True
    assert len(installed.changes) == 2
    assert (home / ".claude.json.fkf.bak").is_file()
    assert (home / ".codex" / "config.toml.fkf.bak").is_file()
    assert json.loads((home / ".claude.json").read_text())["unrelated"] == {"keep": True}
    assert 'user_setting = "keep"' in (home / ".codex" / "config.toml").read_text()

    again = install_harnesses(base, request, launcher_resolver=resolver)
    assert again.complete is True
    assert again.changes == ()

    config = home / ".claude.json"
    drifted = config.read_text().replace('"env": {}', '"env": {"DRIFT": "1"}')
    config.write_text(drifted, encoding="utf-8")
    checked = install_harnesses(
        base,
        HarnessInstallRequest(names=("claude",), home=home, check=True),
        launcher_resolver=resolver,
    )
    assert checked.mode == "check"
    assert checked.complete is False
    assert len(checked.changes) == 1
    assert config.read_text() == drifted

    repaired = install_harnesses(
        base,
        HarnessInstallRequest(names=("claude",), home=home),
        launcher_resolver=resolver,
    )
    assert repaired.complete is True
    assert (home / ".claude.json.fkf.bak").read_text() == drifted
    assert "DRIFT" not in config.read_text()


def test_install_all_combines_shared_targets_once(tmp_path: Path) -> None:
    base = make_base(tmp_path)
    home = tmp_path / "harness-home"
    home.mkdir()
    resolver = fake_launcher(tmp_path / "tools" / "fkf")
    request = HarnessInstallRequest(all=True, home=home)

    installed = install_harnesses(base, request, launcher_resolver=resolver)
    changed_paths = tuple(change.path for change in installed.changes)
    assert installed.harnesses == HARNESSES
    assert len(changed_paths) == len(set(changed_paths))
    assert installed.complete is True

    checked = install_harnesses(base, replace(request, check=True), launcher_resolver=resolver)
    assert checked.complete is True
    assert checked.changes == ()


def test_preflight_conflicts_and_overlapping_workspaces_write_nothing(tmp_path: Path) -> None:
    base = make_base(tmp_path / "first", "shared")
    other = make_base(tmp_path / "second", "other")
    home = tmp_path / "harness-home"
    home.mkdir()
    executable = tmp_path / "tools" / "fkf"
    resolver = fake_launcher(executable)
    conflict = home / ".claude.json"
    conflict.write_text('{"mcpServers":{"fkf-shared":{"command":"other"}}}\n', encoding="utf-8")

    with pytest.raises(HarnessConflictError):
        install_harnesses(
            base,
            HarnessInstallRequest(names=("codex", "claude"), home=home),
            launcher_resolver=resolver,
        )
    assert not (home / ".codex").exists()

    conflict.unlink()
    workspace = tmp_path / "workspace"
    nested = workspace / "nested"
    nested.mkdir(parents=True)
    install_harnesses(
        base,
        HarnessInstallRequest(names=("claude",), home=home, workspace=workspace),
        launcher_resolver=resolver,
    )
    with pytest.raises(HarnessConflictError, match="overlapping"):
        install_harnesses(
            other,
            HarnessInstallRequest(names=("claude",), home=home, workspace=nested),
            launcher_resolver=resolver,
        )


def test_status_is_read_only_and_reports_drift_and_legacy_cleanup(tmp_path: Path) -> None:
    base = make_base(tmp_path)
    home = tmp_path / "harness-home"
    home.mkdir()
    executable = tmp_path / "tools" / "fkf"
    resolver = fake_launcher(executable)
    legacy = home / ".claude.json"
    original = '{"mcpServers":{"fkf":{"command":"/usr/local/bin/fkf","args":["mcp","serve","--base","/old"]}}}\n'
    legacy.write_text(original, encoding="utf-8")

    before = inspect_harnesses(base, home=home, launcher_resolver=resolver)
    claude = next(item for item in before if item.name == "claude")
    assert claude.registered is False
    assert claude.changes == 1
    assert claude.manual_cleanup == ("~/.claude.json#mcpServers.fkf",)
    assert legacy.read_text() == original

    install_harnesses(
        base,
        HarnessInstallRequest(names=("claude",), home=home),
        launcher_resolver=resolver,
    )
    after = inspect_harnesses(base, home=home, launcher_resolver=resolver)
    claude = next(item for item in after if item.name == "claude")
    assert claude.registered is True
    assert claude.changes == 0
    assert claude.error == ""

    named = json.loads((home / ".claude.json").read_text())
    named["mcpServers"]["fkf-brain"] = {"command": "other"}
    (home / ".claude.json").write_text(json.dumps(named), encoding="utf-8")
    conflicted = inspect_harnesses(base, home=home, launcher_resolver=resolver)
    claude = next(item for item in conflicted if item.name == "claude")
    assert claude.registered is False
    assert claude.error


def test_selection_unsafe_targets_and_cancellation_fail_closed(tmp_path: Path) -> None:
    base = make_base(tmp_path)
    home = tmp_path / "harness-home"
    home.mkdir()
    resolver = fake_launcher(tmp_path / "tools" / "fkf")
    invalid = (
        HarnessInstallRequest(home=home),
        HarnessInstallRequest(names=("claude",), all=True, home=home),
        HarnessInstallRequest(names=("claude",), dry_run=True, check=True, home=home),
        HarnessInstallRequest(names=("claude", "claude"), home=home),
    )
    for request in invalid:
        with pytest.raises(InvalidUsageError):
            install_harnesses(base, request, launcher_resolver=resolver)

    outside = tmp_path / "outside"
    outside.write_text("{}", encoding="utf-8")
    (home / ".claude.json").symlink_to(outside)
    with pytest.raises(HarnessConflictError, match="symlink"):
        install_harnesses(
            base,
            HarnessInstallRequest(names=("claude",), home=home),
            launcher_resolver=resolver,
        )

    canceled = Event()
    canceled.set()
    with pytest.raises(CanceledError):
        inspect_harnesses(base, home=home, launcher_resolver=resolver, cancel=canceled)


def test_workspace_syntax_is_usage_but_missing_directory_is_operational(tmp_path: Path) -> None:
    base = make_base(tmp_path)
    resolver = fake_launcher(tmp_path / "tools" / "fkf")

    with pytest.raises(InvalidUsageError, match="must be an absolute path"):
        harness_plan_for(base, "codex", workspace="relative", launcher_resolver=resolver)
    with pytest.raises(OperationalError, match="inspect harness workspace"):
        harness_plan_for(base, "codex", workspace=tmp_path / "missing", launcher_resolver=resolver)
