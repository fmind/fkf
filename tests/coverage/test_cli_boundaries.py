from __future__ import annotations

import io
import json
import os
from pathlib import Path

import pytest
from tests.services.test_context import seeded_base, write_page
from tests.services.test_harness import make_base as make_harness_base
from tests.services.test_read import make_base as make_read_base

import fkf.cli_integrate as cli_integrate
from fkf.cli import app
from fkf.cli_support import run_app
from fkf.graph import build_graph
from fkf.harness import Cancellation, HarnessInstallReport, HarnessInstallRequest, HarnessPlan
from fkf.schedule import ScheduleAction, ScheduleExecution, ScheduleExecutionState, ScheduleReport, ScheduleRequest


def invoke(*arguments: str) -> tuple[int, str, str]:
    stdout, stderr = io.StringIO(), io.StringIO()
    code = run_app(app, arguments, stdout=stdout, stderr=stderr)
    return code, stdout.getvalue(), stderr.getvalue()


def with_base(base: Path, *arguments: str, output: str = "text") -> tuple[int, str, str]:
    return invoke(*arguments, "--base", os.fspath(base), "--format", output)


def test_ask_commands_cover_each_published_read_shape_and_graph_mode(tmp_path: Path) -> None:
    base = make_read_base(tmp_path)
    build_graph(base)

    reads = {
        "events/2026-09-05/": "other.json",
        "events/2026-09-05/journal.json": "journal.json#a1",
        "events/2026-09-05/journal.json#a1": "id     a1",
        "events/2026-09-05/journal.json?jq=.records[].id": "a1",
        "wiki/retrieval-boundary.md": "Overview.",
        "person:alice": "link       ",
    }
    for uri, expected in reads.items():
        code, stdout, stderr = with_base(base.root, "read", uri)
        assert code == 0
        assert expected in stdout
        assert stderr == ""

    code, stdout, stderr = with_base(base.root, "find", "First")
    assert code == 0
    assert "journal.json#a1" in stdout
    assert "record(s) scanned" in stdout
    assert stderr == ""

    code, stdout, stderr = with_base(base.root, "find", "--count", output="jsonl")
    assert code == 0
    assert '"date":"2026-09-05"' in stdout
    assert stderr == ""

    code, stdout, stderr = with_base(base.root, "find", "Retrieval", "--raw", output="json")
    assert code == 0
    assert json.loads(stdout)["pages"][0]["uri"] == "wiki/retrieval-boundary.md"
    assert stderr == ""

    for arguments, message in (
        (("find", "x", "--limit", "-1"), "expected zero or a positive"),
        (("find", "x", "--layer", "unknown"), "unknown layer"),
        (("find", "x", "--where", "invalid"), "takes <path>=<value>"),
        (("find", "x", "--since", "nope"), "not a window"),
        (("read", "wiki/retrieval-boundary.md", "--limit", "-1"), "expected zero or a positive"),
        (("read", "events/2026-09-05/journal.json#a1", "--body"), "not trusted"),
    ):
        code, _stdout, stderr = with_base(base.root, *arguments)
        assert code in {2, 3}
        assert message in stderr

    graph_cases = (
        (("graph",), "graph.tsv"),
        (("graph", "--verify"), "graph.tsv"),
        (("graph", "person:alice", "--in"), "link       "),
        (("graph", "person:alice", "--out"), "edge(s)"),
        (("graph", "person:alice", "--both"), "link       "),
        (("graph", "nodes", "--limit", "2"), "wiki/retrieval-boundary.md"),
    )
    for arguments, expected in graph_cases:
        code, stdout, stderr = with_base(base.root, *arguments)
        assert code == 0
        assert expected in stdout
        assert stderr == ""

    for arguments, message in (
        (("graph", "person:alice", "--depth", "0"), "expected 1.."),
        (("graph", "person:alice", "--in", "--out"), "choose one"),
        (("graph", "--verify", "person:alice"), "accepts no URI"),
        (("graph", "--verify", "nodes"), "accepts no subcommand"),
        (("graph", "nodes", "--limit", "-1"), "expected zero or a positive"),
    ):
        code, _stdout, stderr = with_base(base.root, *arguments)
        assert code == 2
        assert message in stderr


def test_browse_commands_cover_list_validation_and_tag_surfaces(tmp_path: Path) -> None:
    base = seeded_base(tmp_path)
    write_page(base, "projects/active.md", "Active project", "Project body.", status="active")

    code, stdout, stderr = with_base(base.root, "list")
    assert code == 2
    assert "name a subcommand" in stderr
    assert "List stored evidence" in stdout

    commands = (
        ("list", "events"),
        ("list", "events", "--source", "synthetic", "--limit", "1"),
        ("list", "index"),
        ("list", "tasks"),
        ("list", "tasks", "learned", "--unharvested"),
        ("list", "projects", "--status", "active"),
        ("list", "wiki", "--type", "decision"),
        ("tags",),
        ("tags", "wiki"),
        ("tags", "projects"),
        ("validate", "wiki"),
        ("validate", "projects"),
        ("validate", "records"),
        ("validate", "--lint"),
    )
    for arguments in commands:
        code, _stdout, stderr = with_base(base.root, *arguments, output="text")
        assert code in {0, 1}
        if code == 0:
            assert stderr == ""

    for arguments, message in (
        (("list", "events", "--limit", "-1"), "expected zero or a positive"),
        (("list", "events", "--since", "not-a-window"), "not a window"),
        (("list", "tasks", "--until", "not-a-window"), "not a window"),
        (("list", "tasks", "learned", "--since", "not-a-window"), "not a window"),
        (("list", "projects", "--limit", "-1"), "expected zero or a positive"),
        (("list", "wiki", "--limit", "-1"), "expected zero or a positive"),
        (("validate", "--stale-days", "0"), "expected a positive"),
        (("validate", "--stale-days", "5"), "requires --lint"),
    ):
        code, _stdout, stderr = with_base(base.root, *arguments)
        assert code == 2
        assert message in stderr


def test_operate_commands_cover_checks_mutations_and_short_circuits(tmp_path: Path) -> None:
    base = make_read_base(tmp_path)

    for hours in ("-1", "87601"):
        code, _stdout, stderr = with_base(base.root, "status", "--max-age-hours", hours)
        assert code == 2
        assert "expected 1.." in stderr

    code, stdout, stderr = with_base(base.root, "status")
    assert code in {0, 1}
    assert "brain" in stdout
    if code:
        assert "older than" in stderr

    code, stdout, stderr = with_base(base.root, "test")
    assert code == 0
    assert stdout == "\n"
    assert stderr == ""

    code, _stdout, stderr = with_base(base.root, "trust", "--check")
    assert code == 3
    assert "not trusted" in stderr
    code, _stdout, stderr = with_base(base.root, "trust", "--all")
    assert code == 0
    assert stderr == ""
    code, stdout, stderr = with_base(base.root, "trust", "--check")
    assert code == 0
    assert "trusted since" in stdout
    assert stderr == ""

    for arguments, message in (
        (("sync", "--days", "367"), "expected 1..366"),
        (("sync", "--if-due", "--force"), "cannot be combined"),
        (("build", "all", "--check", "--if-stale"), "cannot be combined"),
        (("build", "all", "--older-than", "invalid"), "duration"),
        (("build", "all", "--older-than=-1s"), "must not be negative"),
    ):
        code, _stdout, stderr = with_base(base.root, *arguments)
        assert code == 2
        assert message in stderr

    code, stdout, stderr = with_base(base.root, "sync", "--date", "2026-09-04", "--dry-run")
    assert code == 0
    assert "planned" in stdout
    assert stderr == ""

    code, _stdout, stderr = with_base(base.root, "build", "all", "--check")
    assert code == 1
    assert "derived artifacts are stale" in stderr
    code, stdout, stderr = with_base(base.root, "build", "all")
    assert code == 0
    assert "page(s)" in stdout
    assert "graph" in stdout
    assert "index" in stdout
    assert stderr == ""
    code, stdout, stderr = with_base(base.root, "build", "all", "--if-stale")
    assert code == 0
    assert "nothing stale" in stdout
    assert stderr == ""
    code, _stdout, stderr = with_base(base.root, "build", "all", "--check")
    assert code == 0
    assert stderr == ""

    for arguments in (("config",), ("config", "schema")):
        code, stdout, stderr = with_base(base.root, *arguments, output="json")
        assert code == 0
        assert '"fkf"' in stdout
        assert stderr == ""

    for arguments in (
        ("new", "helper", "collector.sh"),
        ("new", "task", "follow-up", "--title", "Follow up"),
        ("new", "project", "migration", "--tag", "retrieval", "--title", "Migration"),
        ("new", "wiki", "policy", "--tag", "retrieval", "--type", "decision", "--title", "Policy"),
    ):
        code, _stdout, stderr = with_base(base.root, *arguments)
        assert code == 0, (arguments, stderr)
        assert stderr == ""


def test_integration_commands_are_safe_in_dry_run_check_and_status_modes(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    base = make_harness_base(tmp_path)
    executable = tmp_path / "tools" / "fkf"
    executable.parent.mkdir()
    executable.write_text("#!/bin/sh\n", encoding="utf-8")
    executable.chmod(0o700)
    monkeypatch.setenv("PATH", f"{executable.parent}:/usr/bin:/bin")
    workspace = tmp_path / "workspace"
    workspace.mkdir()

    original_plan = cli_integrate.harness_plan_for
    original_install = cli_integrate.install_harnesses

    def plan(
        root: Path,
        name: str,
        *,
        workspace: str = "",
        path: str = "",
    ) -> HarnessPlan:
        return original_plan(
            root,
            name,
            workspace=workspace,
            path=path,
            launcher_resolver=lambda _requested, _path: executable,
        )

    def install(
        root: Path,
        request: HarnessInstallRequest,
        *,
        cancel: Cancellation | None = None,
    ) -> HarnessInstallReport:
        return original_install(
            root,
            request,
            launcher_resolver=lambda _requested, _path: executable,
            cancel=cancel,
        )

    monkeypatch.setattr(cli_integrate, "harness_plan_for", plan)
    monkeypatch.setattr(cli_integrate, "install_harnesses", install)

    code, stdout, stderr = with_base(base, "harness", "print", "codex", "--workspace", os.fspath(workspace))
    assert code == 0
    assert "# Base: brain" in stdout
    assert "# Workspace:" in stdout
    assert stderr == ""

    code, stdout, stderr = with_base(base, "harness", "install", "codex", "--dry-run")
    assert code == 0
    assert "create" in stdout
    assert stderr == ""
    code, stdout, stderr = with_base(base, "harness", "install", "codex", "--check")
    assert code == 1
    assert "change(s) required" in stderr
    assert "create" in stdout
    code, _stdout, stderr = with_base(base, "harness", "install", "codex")
    assert code == 0
    assert stderr == ""

    for action in ("install", "remove"):
        code, stdout, stderr = with_base(
            base,
            "schedule",
            action,
            "--dry-run",
            "--executable",
            os.fspath(executable),
        )
        assert code == 0
        assert "schedule dry-run" in stdout
        assert stderr == ""

    def fake_schedule(
        root: Path,
        request: ScheduleRequest,
        *,
        cancel: Cancellation | None = None,
    ) -> ScheduleReport:
        del cancel
        action = request.action
        return ScheduleReport(
            root,
            ScheduleAction(action),
            "linux",
            "fkf-test",
            (),
            ScheduleExecution(ScheduleExecutionState.NEVER),
            False,
            False,
            False,
            False,
            False,
            True,
        )

    monkeypatch.setattr(cli_integrate, "schedule", fake_schedule)
    code, stdout, stderr = with_base(base, "schedule", "status", "--executable", os.fspath(executable))
    assert code == 0
    assert "schedule linux fkf-test: missing" in stdout
    assert stderr == ""
