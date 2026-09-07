"""Hermetic contracts for native user-level FKF schedules."""

from __future__ import annotations

import os
import plistlib
from dataclasses import replace
from pathlib import Path

import pytest

from fkf.errors import InvalidUsageError, OperationalError
from fkf.harness import LauncherResolver
from fkf.io import FileTooLargeError
from fkf.process import Cancellation, Command, CommandResult
from fkf.schedule import (
    ScheduleAction,
    ScheduleExecutionState,
    ScheduleFileState,
    ScheduleRequest,
    parse_launchd_execution,
    parse_systemd_execution,
    plan_schedule,
    schedule,
)
from fkf.store import MAX_CONTROL_FILE_BYTES, UnsafePathError


class ScheduleRunner:
    def __init__(self) -> None:
        self.commands: list[Command] = []
        self.active = False
        self.metadata = b""
        self.fail_on = ""

    def run(self, command: Command, *, cancel: Cancellation | None = None) -> CommandResult:
        del cancel
        self.commands.append(command)
        argv = command.argv
        if self.fail_on and self.fail_on in argv:
            raise OperationalError(f"manager refused {self.fail_on}")
        if "is-enabled" in argv or "is-active" in argv or "print" in argv:
            if not self.active:
                raise OperationalError("manager unit is inactive")
            return CommandResult(self.metadata)
        if "show" in argv:
            return CommandResult(self.metadata)
        if "enable" in argv or "bootstrap" in argv:
            self.active = True
        if "disable" in argv or "bootout" in argv:
            self.active = False
        return CommandResult(b"")


def fixture(tmp_path: Path, platform: str = "linux") -> tuple[Path, ScheduleRequest, ScheduleRunner, LauncherResolver]:
    home = tmp_path / "home & owner"
    base = tmp_path / "base & evidence"
    executable = tmp_path / "stable tools" / "fkf"
    home.mkdir()
    base.mkdir()
    executable.parent.mkdir()
    executable.write_text("#!/bin/sh\n", encoding="utf-8")
    executable.chmod(0o700)
    runner = ScheduleRunner()

    def resolver(requested: str, search_path: str) -> Path:
        del requested, search_path
        return executable

    request = ScheduleRequest(
        action=ScheduleAction.INSTALL,
        home=home,
        platform=platform,
        executable=executable,
        path="/usr/bin:/bin:relative:/usr/bin",
        uid=501,
        runner=runner,
    )
    return base, request, runner, resolver


def test_linux_plan_install_status_remove_and_idempotence(tmp_path: Path) -> None:
    base, request, runner, resolver = fixture(tmp_path)
    plan = plan_schedule(base, request, launcher_resolver=resolver)
    assert plan.base == base
    assert plan.path == "/usr/bin:/bin"
    assert len(plan.files) == 2
    service = plan.files[0].content.decode()
    timer = plan.files[1].content.decode()
    assert f'Environment="HOME={request.home}"' in service
    assert 'Environment="PATH=/usr/bin:/bin"' in service
    assert "ExecStart=" in service
    assert '"--format" "text" "sync" "--if-due"' in service
    assert '"--format" "text" "build" "--if-stale"' in service
    assert "/bin/sh" not in service
    assert "OnCalendar=hourly" in timer
    assert "Persistent=true" in timer

    dry = schedule(base, replace(request, dry_run=True), launcher_resolver=resolver)
    assert dry.dry_run is True
    assert dry.changed is True
    assert dry.complete is True
    assert dry.installed is False
    assert runner.commands == []
    assert all(not item.path.exists() for item in dry.files)

    installed = schedule(base, request, launcher_resolver=resolver)
    assert installed.installed is True
    assert installed.active is True
    assert installed.current is True
    assert installed.changed is True
    assert installed.complete is True
    assert [command.argv for command in runner.commands] == [
        ("systemctl", "--user", "daemon-reload"),
        ("systemctl", "--user", "enable", "--now", f"{installed.name}.timer"),
    ]
    for command in runner.commands:
        assert command.environment == {"HOME": os.fspath(request.home), "PATH": "/usr/bin:/bin"}
        assert command.base is None

    runner.commands.clear()
    again = schedule(base, request, launcher_resolver=resolver)
    assert again.changed is False
    assert again.current is True
    assert len(runner.commands) == 3

    runner.commands.clear()
    status = schedule(base, replace(request, action=ScheduleAction.STATUS), launcher_resolver=resolver)
    assert status.changed is False
    assert status.complete is True
    assert status.current is True
    assert [command.argv[2] for command in runner.commands[:2]] == ["is-enabled", "is-active"]

    runner.commands.clear()
    removed = schedule(base, replace(request, action=ScheduleAction.REMOVE), launcher_resolver=resolver)
    assert removed.changed is True
    assert removed.complete is True
    assert removed.installed is False
    assert removed.active is False
    assert all(item.state is ScheduleFileState.MISSING for item in removed.files)
    assert all(not item.path.exists() for item in removed.files)
    assert ("systemctl", "--user", "disable", "--now", f"{removed.name}.timer") in [
        command.argv for command in runner.commands
    ]


def test_darwin_plan_sequences_sync_and_cache_repair_with_opaque_path_arguments(tmp_path: Path) -> None:
    base, request, runner, resolver = fixture(tmp_path, "darwin")
    plan = plan_schedule(base, request, launcher_resolver=resolver)
    assert len(plan.files) == 1
    root = plistlib.loads(plan.files[0].content)
    arguments = root["ProgramArguments"]
    assert arguments == [
        "/bin/sh",
        "-c",
        ('"$1" --base "$2" --format text sync --if-due && exec "$1" --base "$2" --format text build --if-stale'),
        "fkf-schedule",
        os.fspath(request.executable),
        os.fspath(base),
    ]
    assert os.fspath(request.executable) not in arguments[2]
    assert os.fspath(base) not in arguments[2]
    assert root["EnvironmentVariables"] == {"HOME": os.fspath(request.home), "PATH": "/usr/bin:/bin"}
    assert root["WorkingDirectory"] == "/"
    assert root["StartInterval"] == 3600
    assert root["RunAtLoad"] is True

    report = schedule(base, request, launcher_resolver=resolver)
    assert report.current is True
    assert runner.commands[-1].argv == (
        "launchctl",
        "bootstrap",
        "gui/501",
        os.fspath(report.files[0].path),
    )


def test_schedule_rejects_unsafe_runtime_and_launcher_context(tmp_path: Path) -> None:
    base, request, _runner, resolver = fixture(tmp_path)
    invalid = (
        replace(request, action="unknown"),
        replace(request, home=Path("relative")),
        replace(request, platform="windows"),
        replace(request, path="relative"),
        replace(request, path="/usr/bin\n/evil"),
    )
    for candidate in invalid:
        with pytest.raises(InvalidUsageError):
            plan_schedule(base, candidate, launcher_resolver=resolver)

    with pytest.raises(InvalidUsageError):
        plan_schedule(
            base,
            replace(request, executable=base / "fkf"),
            launcher_resolver=lambda requested, _path: Path(requested),
        )

    with pytest.raises(InvalidUsageError, match="uv tool install fkf"):
        plan_schedule(base, replace(request, executable=""), launcher_resolver=None)


def test_inspection_is_bounded_and_rejects_non_regular_files(tmp_path: Path) -> None:
    base, request, _runner, resolver = fixture(tmp_path)
    plan = plan_schedule(base, request, launcher_resolver=resolver)
    path = plan.files[0].path
    path.parent.mkdir(parents=True)
    outside = tmp_path / "outside"
    outside.write_text("outside", encoding="utf-8")
    path.symlink_to(outside)
    with pytest.raises(UnsafePathError):
        schedule(base, replace(request, action=ScheduleAction.STATUS), launcher_resolver=resolver)

    path.unlink()
    path.write_bytes(b"x" * (MAX_CONTROL_FILE_BYTES + 1))
    with pytest.raises(FileTooLargeError):
        schedule(base, replace(request, action=ScheduleAction.STATUS), launcher_resolver=resolver)


def test_native_execution_state_parsers_are_closed_and_conservative() -> None:
    timestamp = "Sat 2026-09-05 08:00:31 CEST"
    never = parse_systemd_execution("Result=success\nExecMainStartTimestamp=\nActiveState=inactive\nSubState=dead\n")
    running = parse_systemd_execution(
        f"Result=success\nExecMainStartTimestamp={timestamp}\nActiveState=activating\nSubState=start\n"
    )
    succeeded = parse_systemd_execution(
        f"Result=success\nExecMainStartTimestamp={timestamp}\nExecMainCode=exited\nExecMainStatus=0\n"
        "ActiveState=inactive\nSubState=dead\n"
    )
    failed = parse_systemd_execution(
        f"Result=exit-code\nExecMainStartTimestamp={timestamp}\nExecMainCode=exited\nExecMainStatus=17\n"
        "ActiveState=failed\nSubState=failed\n"
    )
    duplicate = parse_systemd_execution("Result=success\nResult=success\n")
    assert never.state is ScheduleExecutionState.NEVER
    assert running.state is ScheduleExecutionState.RUNNING
    assert succeeded.state is ScheduleExecutionState.SUCCEEDED
    assert succeeded.exit_code == 0
    assert failed.state is ScheduleExecutionState.FAILED
    assert failed.exit_code == 17
    assert duplicate.state is ScheduleExecutionState.UNKNOWN

    assert parse_launchd_execution("state = running\nruns = 2\n").state is ScheduleExecutionState.RUNNING
    assert parse_launchd_execution("state = waiting\nruns = 0\n").state is ScheduleExecutionState.NEVER
    assert parse_launchd_execution("runs = 2\nlast exit code = 0\n").state is ScheduleExecutionState.SUCCEEDED
    assert parse_launchd_execution("runs = 2\nlast exit code = 9\n").state is ScheduleExecutionState.FAILED


def test_aliases_share_identity_and_status_never_executes_fkf(tmp_path: Path) -> None:
    base, request, runner, resolver = fixture(tmp_path)
    alias = tmp_path / "base-alias"
    alias.symlink_to(base, target_is_directory=True)
    real = plan_schedule(base, request, launcher_resolver=resolver)
    through_alias = plan_schedule(alias, request, launcher_resolver=resolver)
    assert through_alias.base == real.base
    assert through_alias.name == real.name

    status = schedule(base, replace(request, action=ScheduleAction.STATUS), launcher_resolver=resolver)
    assert status.complete is True
    assert all(command.argv[0] in {"systemctl", "launchctl"} for command in runner.commands)


def test_native_manager_cleanup_failures_are_actionable(tmp_path: Path) -> None:
    base, request, runner, resolver = fixture(tmp_path, "darwin")
    plan = plan_schedule(base, request, launcher_resolver=resolver)
    plan.files[0].path.parent.mkdir(parents=True)
    plan.files[0].path.write_text("drift", encoding="utf-8")
    runner.active = True
    runner.fail_on = "bootout"

    with pytest.raises(OperationalError, match="manage darwin schedule"):
        schedule(base, request, launcher_resolver=resolver)
    assert plan.files[0].path.read_text(encoding="utf-8") == "drift"

    linux_root = tmp_path / "linux"
    linux_root.mkdir()
    base, request, runner, resolver = fixture(linux_root, "linux")
    installed = schedule(base, request, launcher_resolver=resolver)
    runner.fail_on = "disable"
    with pytest.raises(OperationalError, match="manage linux schedule"):
        schedule(base, replace(request, action=ScheduleAction.REMOVE), launcher_resolver=resolver)
    assert all(item.path.exists() for item in installed.files)
