from __future__ import annotations

import io
from pathlib import Path
from threading import Event

import pytest

import fkf.cli_integrate as cli_integrate
from fkf.cli import app
from fkf.cli_integrate import HarnessList, _harness_install_text, _schedule_text
from fkf.cli_support import run_app
from fkf.errors import CanceledError, OperationalError
from fkf.harness import HarnessChange, HarnessInstallReport, HarnessPlan
from fkf.harness import harness_plan_for as build_harness_plan
from fkf.schedule import (
    ScheduleAction,
    ScheduleExecution,
    ScheduleExecutionState,
    ScheduleFile,
    ScheduleFileState,
    ScheduleReport,
)
from tests.services.test_context import seeded_base


def invoke(*arguments: str) -> tuple[int, str, str]:
    stdout, stderr = io.StringIO(), io.StringIO()
    code = run_app(app, arguments, stdout=stdout, stderr=stderr)
    return code, stdout.getvalue(), stderr.getvalue()


def test_harness_list_has_stable_json_and_text_envelopes() -> None:
    code, stdout, stderr = invoke("harness", "list", "--format", "json")
    assert code == 0
    assert '"harnesses": [' in stdout
    assert '"claude"' in stdout
    assert stderr == ""

    code, stdout, stderr = invoke("harness", "list", "--format", "jsonl")
    assert code == 0
    assert stdout.count("\n") == 1
    assert '"harnesses"' in stdout
    assert stderr == ""

    code, stdout, stderr = invoke("harness", "list", "--format", "text")
    assert code == 0
    assert stdout.splitlines()[0] == "claude"
    assert stdout.splitlines()[-1] == "cline"
    assert stderr == ""


def test_harness_install_rejects_invalid_selection_before_opening_a_base() -> None:
    for arguments, message in (
        (("harness", "install"), "install <name>... | --all"),
        (("harness", "install", "claude", "--all"), "cannot be combined"),
        (("harness", "install", "claude", "--check", "--dry-run"), "cannot be combined"),
    ):
        code, stdout, stderr = invoke(*arguments)
        assert code == 2
        assert stdout == ""
        assert message in stderr


def test_harness_install_forwards_the_invocation_cancellation(monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> None:
    base = seeded_base(tmp_path)
    observed: list[object] = []

    def stop(*_args: object, cancel: object = None, **_kwargs: object) -> None:
        observed.append(cancel)
        raise OperationalError("stopped after observing cancellation")

    monkeypatch.setattr("fkf.cli_integrate.install_harnesses", stop)
    code, stdout, stderr = invoke(
        "harness",
        "install",
        "codex",
        "--check",
        "--base",
        str(base.root),
    )

    assert code == 1
    assert stdout == ""
    assert "stopped after observing cancellation" in stderr
    assert len(observed) == 1
    assert isinstance(observed[0], Event)


def test_harness_print_maps_missing_workspace_to_operational_exit(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    base = seeded_base(tmp_path)
    launcher = tmp_path / "stable" / "fkf"

    def plan(
        base_root: Path | str,
        name: str,
        *,
        executable: Path | str = "",
        workspace: Path | str = "",
        path: str = "",
        launcher_resolver: object = None,
    ) -> HarnessPlan:
        del launcher_resolver
        return build_harness_plan(
            base_root,
            name,
            executable=executable,
            workspace=workspace,
            path=path,
            launcher_resolver=lambda _requested, _path: launcher,
        )

    monkeypatch.setattr(cli_integrate, "harness_plan_for", plan)
    code, stdout, stderr = invoke(
        "harness",
        "print",
        "codex",
        "--workspace",
        str(tmp_path / "missing"),
        "--base",
        str(base.root),
    )

    assert code == 1
    assert stdout == ""
    assert "inspect harness workspace" in stderr


def test_schedule_forwards_the_invocation_cancellation(monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> None:
    base = seeded_base(tmp_path)
    observed: list[object] = []

    def stop(*_args: object, cancel: object = None, **_kwargs: object) -> None:
        observed.append(cancel)
        raise CanceledError("stopped after observing schedule cancellation")

    monkeypatch.setattr(cli_integrate, "schedule", stop)
    code, stdout, stderr = invoke("schedule", "status", "--base", str(base.root))

    assert code == 130
    assert stdout == ""
    assert "stopped after observing schedule cancellation" in stderr
    assert len(observed) == 1
    assert isinstance(observed[0], Event)


def test_integration_text_reports_name_current_changes_and_execution() -> None:
    current = HarnessInstallReport(Path("/base"), "brain", "check", ("codex",), True, ())
    assert _harness_install_text(current) == "harness check for brain (/base): current"
    changed = HarnessInstallReport(
        Path("/base"),
        "brain",
        "install",
        ("codex",),
        True,
        (HarnessChange("codex", "update", Path("/home/.codex/config.toml"), Path("/home/backup")),),
    )
    assert _harness_install_text(changed).splitlines() == [
        "backup /home/.codex/config.toml -> /home/backup",
        "update /home/.codex/config.toml [codex]",
    ]

    report = ScheduleReport(
        Path("/base"),
        ScheduleAction.STATUS,
        "linux",
        "fkf-123",
        (ScheduleFile(Path("/home/timer"), ScheduleFileState.DRIFTED),),
        ScheduleExecution(ScheduleExecutionState.FAILED, "today", 7),
        False,
        False,
        True,
        True,
        False,
        True,
    )
    assert _schedule_text(report).splitlines() == [
        "schedule linux fkf-123: drifted",
        "last execution: failed (exit 7) at today",
        "drifted: /home/timer",
    ]


def test_harness_list_dataclass_preserves_closed_tuple() -> None:
    assert HarnessList(("codex",)).harnesses == ("codex",)
