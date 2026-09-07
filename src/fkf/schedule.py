"""Native user-level schedules with exact base binding and bounded inspection."""

from __future__ import annotations

import hashlib
import os
import plistlib
from contextlib import suppress
from dataclasses import dataclass, field, replace
from enum import StrEnum
from pathlib import Path
from typing import Final

from fkf.errors import CanceledError, InvalidUsageError, OperationalError
from fkf.harness import LauncherResolver, resolve_persistent_launcher
from fkf.io import atomic_write, read_file_limited
from fkf.process import Cancellation, Command, Runner, SubprocessRunner, check_cancel, sanitize_path
from fkf.store import BASE_FILE_MODE, MAX_CONTROL_FILE_BYTES, resolve_physical_path
from fkf.timeutil import parse_duration

_MANAGER_TIMEOUT: Final = parse_duration("30s")
_MAX_PATH_BYTES: Final = 16 << 10


class ScheduleAction(StrEnum):
    INSTALL = "install"
    STATUS = "status"
    REMOVE = "remove"


class ScheduleFileState(StrEnum):
    MISSING = "missing"
    CURRENT = "current"
    DRIFTED = "drifted"


class ScheduleExecutionState(StrEnum):
    UNKNOWN = "unknown"
    NEVER = "never-run"
    RUNNING = "running"
    SUCCEEDED = "succeeded"
    FAILED = "failed"


@dataclass(frozen=True, slots=True)
class ScheduleExecution:
    state: ScheduleExecutionState
    timestamp: str = ""
    exit_code: int | None = None


@dataclass(frozen=True, slots=True)
class ScheduleFile:
    path: Path
    state: ScheduleFileState


@dataclass(frozen=True, slots=True)
class ScheduledFile:
    path: Path
    content: bytes = field(repr=False)


@dataclass(frozen=True, slots=True)
class ScheduleRequest:
    action: ScheduleAction | str
    home: Path | str
    platform: str
    path: str
    executable: Path | str = ""
    uid: int = 0
    dry_run: bool = False
    runner: Runner | None = field(default=None, repr=False, compare=False, metadata={"json": "-"})


@dataclass(frozen=True, slots=True)
class SchedulePlan:
    base: Path
    home: Path
    path: str
    platform: str
    executable: Path
    name: str
    uid: int
    files: tuple[ScheduledFile, ...]


@dataclass(frozen=True, slots=True)
class ScheduleReport:
    base: Path
    action: ScheduleAction
    platform: str
    name: str
    files: tuple[ScheduleFile, ...]
    last_execution: ScheduleExecution
    dry_run: bool
    changed: bool
    installed: bool
    active: bool
    current: bool
    complete: bool


def _absolute(value: Path | str, label: str) -> Path:
    rendered = os.fspath(value)
    if not rendered or not Path(rendered).is_absolute():
        raise InvalidUsageError(f"{label} must be an explicit absolute path")
    if any(character in rendered for character in "\x00\r\n"):
        raise InvalidUsageError(f"{label} may not contain NUL or newlines")
    if len(rendered.encode()) > _MAX_PATH_BYTES:
        raise InvalidUsageError(f"{label} exceeds {_MAX_PATH_BYTES} bytes")
    return Path(os.path.normpath(rendered))


def _within(root: Path, candidate: Path) -> bool:
    try:
        candidate.relative_to(root)
    except ValueError:
        return False
    return True


def _schedule_action(value: ScheduleAction | str) -> ScheduleAction:
    try:
        return ScheduleAction(value)
    except ValueError as error:
        raise InvalidUsageError(f"unknown schedule action {value!r}; expected install, status, or remove") from error


def plan_schedule(
    base: Path | str,
    request: ScheduleRequest,
    *,
    launcher_resolver: LauncherResolver | None = resolve_persistent_launcher,
) -> SchedulePlan:
    """Build the complete native-unit plan without reading or mutating scheduler state."""

    _schedule_action(request.action)
    selected_base = _absolute(base, "schedule base")
    try:
        root = resolve_physical_path(selected_base)
        if not root.is_dir():
            raise OSError("not a directory")
    except OSError as error:
        raise InvalidUsageError(f"resolve schedule base: {error}") from error
    home = _absolute(request.home, "schedule HOME")
    if _within(root, home.resolve(strict=False)):
        raise InvalidUsageError("schedule HOME must be outside the base")
    resolver = launcher_resolver or resolve_persistent_launcher
    executable = _absolute(resolver(os.fspath(request.executable), request.path), "schedule executable")
    try:
        physical_executable = executable.resolve(strict=True)
    except OSError as error:
        raise InvalidUsageError(f"resolve schedule executable: {error}") from error
    if _within(root, physical_executable):
        raise InvalidUsageError("schedule executable must be outside the base")
    closed_path = sanitize_path(request.path, root)
    if not closed_path:
        raise InvalidUsageError("schedule requires a non-empty PATH with an absolute directory outside the base")
    if any(character in closed_path for character in "\x00\r\n"):
        raise InvalidUsageError("schedule PATH may not contain NUL or newlines")
    if len(closed_path.encode()) > _MAX_PATH_BYTES:
        raise InvalidUsageError(f"schedule PATH exceeds {_MAX_PATH_BYTES} bytes")
    if request.platform not in {"linux", "darwin"}:
        raise InvalidUsageError(f"schedule is supported only on linux and darwin, not {request.platform!r}")
    if request.uid < 0:
        raise InvalidUsageError("schedule UID must be non-negative")
    name = f"fkf-{hashlib.sha256(os.fsencode(root)).hexdigest()[:12]}"
    if request.platform == "linux":
        directory = home / ".config" / "systemd" / "user"
        files = (
            ScheduledFile(directory / f"{name}.service", _systemd_service(root, home, closed_path, executable, name)),
            ScheduledFile(directory / f"{name}.timer", _systemd_timer(name)),
        )
    else:
        path = home / "Library" / "LaunchAgents" / f"com.fmind.{name}.plist"
        files = (ScheduledFile(path, _launchd_agent(root, home, closed_path, executable, name)),)
    return SchedulePlan(root, home, closed_path, request.platform, executable, name, request.uid, files)


def _systemd_quote(value: str) -> str:
    if any(character in value for character in "\x00\r\n"):
        raise InvalidUsageError("schedule paths may not contain NUL or newlines")
    return value.replace("\\", "\\\\").replace('"', '\\"').replace("%", "%%")


def _systemd_command(arguments: tuple[str, ...]) -> str:
    return " ".join(f'"{_systemd_quote(argument)}"' for argument in arguments)


def _systemd_service(base: Path, home: Path, path: str, executable: Path, name: str) -> bytes:
    prefix = (os.fspath(executable), "--base", os.fspath(base), "--format", "text")
    sync = _systemd_command((*prefix, "sync", "--if-due"))
    build = _systemd_command((*prefix, "build", "--if-stale"))
    return (
        f"[Unit]\nDescription=FKF opportunistic sync for {name}\n\n"
        "[Service]\nType=oneshot\n"
        f'Environment="HOME={_systemd_quote(os.fspath(home))}"\n'
        f'Environment="PATH={_systemd_quote(path)}"\n'
        f"ExecStart={sync}\nExecStart={build}\n"
    ).encode()


def _systemd_timer(name: str) -> bytes:
    return (
        f"[Unit]\nDescription=Run {name} hourly\n\n"
        f"[Timer]\nOnCalendar=hourly\nPersistent=true\nUnit={name}.service\n\n"
        "[Install]\nWantedBy=timers.target\n"
    ).encode()


def _launchd_agent(base: Path, home: Path, path: str, executable: Path, name: str) -> bytes:
    # launchd has no sequential multi-command primitive. The constant script receives both
    # paths as opaque positional arguments: this preserves cache repair after a prior rebuild
    # failure without interpolating base-owned or machine-local text into shell syntax.
    script = '"$1" --base "$2" --format text sync --if-due && exec "$1" --base "$2" --format text build --if-stale'
    payload = {
        "Label": f"com.fmind.{name}",
        "ProgramArguments": [
            "/bin/sh",
            "-c",
            script,
            "fkf-schedule",
            os.fspath(executable),
            os.fspath(base),
        ],
        "EnvironmentVariables": {"HOME": os.fspath(home), "PATH": path},
        "WorkingDirectory": "/",
        "StartInterval": 3600,
        "RunAtLoad": True,
    }
    return plistlib.dumps(payload, fmt=plistlib.FMT_XML, sort_keys=False)


def _manager_command(plan: SchedulePlan, argv: tuple[str, ...]) -> Command:
    # PATH is already stripped of relative/base-resolving entries. ``base=None`` prevents the
    # generic provider runner from prepending <base>/bin ahead of systemctl or launchctl.
    return Command(
        argv,
        _MANAGER_TIMEOUT,
        environment={"HOME": os.fspath(plan.home), "PATH": plan.path},
        max_output_bytes=MAX_CONTROL_FILE_BYTES,
    )


def _run_manager(
    plan: SchedulePlan,
    runner: Runner,
    argv: tuple[str, ...],
    cancel: Cancellation | None,
) -> bytes:
    check_cancel(cancel)
    return runner.run(_manager_command(plan, argv), cancel=cancel).stdout


def parse_systemd_execution(output: str) -> ScheduleExecution:
    values: dict[str, str] = {}
    for line in output.splitlines():
        key, separator, value = line.partition("=")
        if not separator or not key:
            continue
        if key in values:
            return ScheduleExecution(ScheduleExecutionState.UNKNOWN)
        values[key] = value.strip()
    if not values:
        return ScheduleExecution(ScheduleExecutionState.UNKNOWN)
    timestamp = values.get("ExecMainStartTimestamp", "")
    if values.get("ActiveState") == "activating" or values.get("SubState") in {"start", "running"}:
        return ScheduleExecution(ScheduleExecutionState.RUNNING, timestamp)
    if not timestamp:
        return ScheduleExecution(ScheduleExecutionState.NEVER)
    exit_code: int | None = None
    if values.get("ExecMainCode") in {"1", "exited"}:
        with suppress(ValueError):
            exit_code = int(values.get("ExecMainStatus", ""))
    if (
        (exit_code is not None and exit_code != 0)
        or values.get("Result", "") not in {"", "success"}
        or values.get("ActiveState") == "failed"
    ):
        return ScheduleExecution(ScheduleExecutionState.FAILED, timestamp, exit_code)
    if exit_code == 0 and values.get("Result") == "success":
        return ScheduleExecution(ScheduleExecutionState.SUCCEEDED, timestamp, exit_code)
    return ScheduleExecution(ScheduleExecutionState.UNKNOWN, timestamp, exit_code)


def parse_launchd_execution(output: str) -> ScheduleExecution:
    state = ScheduleExecutionState.UNKNOWN
    runs = -1
    exit_code: int | None = None
    for line in output.splitlines():
        key, separator, value = line.strip().partition(" = ")
        if not separator:
            continue
        if key == "state" and value == "running":
            state = ScheduleExecutionState.RUNNING
        elif key == "runs":
            with suppress(ValueError):
                runs = int(value)
        elif key == "last exit code":
            with suppress(ValueError):
                exit_code = int(value)
    if state is ScheduleExecutionState.RUNNING:
        return ScheduleExecution(state)
    if runs == 0:
        return ScheduleExecution(ScheduleExecutionState.NEVER)
    if exit_code is not None:
        state = ScheduleExecutionState.SUCCEEDED if exit_code == 0 else ScheduleExecutionState.FAILED
    return ScheduleExecution(state, exit_code=exit_code)


def _manager_state(
    plan: SchedulePlan,
    runner: Runner,
    cancel: Cancellation | None,
) -> tuple[bool, ScheduleExecution]:
    unknown = ScheduleExecution(ScheduleExecutionState.UNKNOWN)
    if plan.platform == "darwin":
        try:
            output = _run_manager(
                plan,
                runner,
                ("launchctl", "print", f"gui/{plan.uid}/com.fmind.{plan.name}"),
                cancel,
            )
        except CanceledError:
            raise
        except Exception:
            return False, unknown
        return True, parse_launchd_execution(output.decode(errors="replace"))
    for argv in (
        ("systemctl", "--user", "is-enabled", "--quiet", f"{plan.name}.timer"),
        ("systemctl", "--user", "is-active", "--quiet", f"{plan.name}.timer"),
    ):
        try:
            _run_manager(plan, runner, argv, cancel)
        except CanceledError:
            raise
        except Exception:
            return False, unknown
    try:
        output = _run_manager(
            plan,
            runner,
            (
                "systemctl",
                "--user",
                "show",
                f"{plan.name}.service",
                "--property=ActiveState,SubState,Result,ExecMainCode,ExecMainStatus,ExecMainStartTimestamp",
            ),
            cancel,
        )
    except CanceledError:
        raise
    except Exception:
        return True, unknown
    return True, parse_systemd_execution(output.decode(errors="replace"))


def _inspect(
    plan: SchedulePlan,
    request: ScheduleRequest,
    runner: Runner,
    cancel: Cancellation | None,
) -> tuple[ScheduleReport, bool]:
    files: list[ScheduleFile] = []
    all_exist = True
    all_current = True
    any_exist = False
    for managed in plan.files:
        check_cancel(cancel)
        try:
            managed.path.lstat()
        except FileNotFoundError:
            all_exist = False
            all_current = False
            state = ScheduleFileState.MISSING
        except OSError as error:
            raise OperationalError(f"inspect schedule file {managed.path}: {error}") from error
        else:
            body = read_file_limited(managed.path, MAX_CONTROL_FILE_BYTES)
            any_exist = True
            if body == managed.content:
                state = ScheduleFileState.CURRENT
            else:
                state = ScheduleFileState.DRIFTED
                all_current = False
        files.append(ScheduleFile(managed.path, state))
    active = False
    execution = ScheduleExecution(ScheduleExecutionState.UNKNOWN)
    action = _schedule_action(request.action)
    if (
        (all_exist and all_current)
        or (plan.platform == "darwin" and any_exist)
        or action in {ScheduleAction.STATUS, ScheduleAction.REMOVE}
    ):
        active, execution = _manager_state(plan, runner, cancel)
    installed = all_exist
    current = all_exist and all_current and active
    report = ScheduleReport(
        plan.base,
        action,
        plan.platform,
        plan.name,
        tuple(files),
        execution,
        request.dry_run,
        False,
        installed,
        active,
        current,
        False,
    )
    return report, any_exist or active


def _manage(plan: SchedulePlan, runner: Runner, argv: tuple[str, ...], cancel: Cancellation | None) -> None:
    try:
        _run_manager(plan, runner, argv, cancel)
    except CanceledError:
        raise
    except Exception as error:
        raise OperationalError(f"manage {plan.platform} schedule: {error}") from error


def _install(plan: SchedulePlan, active: bool, runner: Runner, cancel: Cancellation | None) -> None:
    if plan.platform == "darwin" and active:
        _manage(
            plan,
            runner,
            ("launchctl", "bootout", f"gui/{plan.uid}", os.fspath(plan.files[0].path)),
            cancel,
        )
    for managed in plan.files:
        check_cancel(cancel)
        atomic_write(managed.path, managed.content, mode=BASE_FILE_MODE)
    if plan.platform == "linux":
        _manage(plan, runner, ("systemctl", "--user", "daemon-reload"), cancel)
        _manage(plan, runner, ("systemctl", "--user", "enable", "--now", f"{plan.name}.timer"), cancel)
    else:
        _manage(
            plan,
            runner,
            ("launchctl", "bootstrap", f"gui/{plan.uid}", os.fspath(plan.files[0].path)),
            cancel,
        )


def _remove(plan: SchedulePlan, active: bool, runner: Runner, cancel: Cancellation | None) -> None:
    if plan.platform == "linux":
        _manage(plan, runner, ("systemctl", "--user", "disable", "--now", f"{plan.name}.timer"), cancel)
    elif active:
        _manage(
            plan,
            runner,
            ("launchctl", "bootout", f"gui/{plan.uid}", os.fspath(plan.files[0].path)),
            cancel,
        )
    for managed in plan.files:
        check_cancel(cancel)
        try:
            managed.path.unlink()
        except FileNotFoundError:
            pass
        except OSError as error:
            raise OperationalError(f"remove schedule file {managed.path}: {error}") from error
    if plan.platform == "linux":
        _manage(plan, runner, ("systemctl", "--user", "daemon-reload"), cancel)


def schedule(
    base: Path | str,
    request: ScheduleRequest,
    *,
    launcher_resolver: LauncherResolver | None = resolve_persistent_launcher,
    cancel: Cancellation | None = None,
) -> ScheduleReport:
    """Install, inspect, or remove the one hourly native schedule for a base."""

    check_cancel(cancel)
    plan = plan_schedule(base, request, launcher_resolver=launcher_resolver)
    runner = request.runner or SubprocessRunner()
    report, exists = _inspect(plan, request, runner, cancel)
    if report.action is ScheduleAction.STATUS:
        return replace(report, complete=True)
    changed = not report.current if report.action is ScheduleAction.INSTALL else exists
    report = replace(report, changed=changed)
    if request.dry_run or not changed:
        return replace(report, complete=True)
    check_cancel(cancel)
    if report.action is ScheduleAction.INSTALL:
        _install(plan, report.active, runner, cancel)
        files = tuple(replace(item, state=ScheduleFileState.CURRENT) for item in report.files)
        return replace(report, files=files, installed=True, active=True, current=True, complete=True)
    _remove(plan, report.active, runner, cancel)
    files = tuple(replace(item, state=ScheduleFileState.MISSING) for item in report.files)
    return replace(report, files=files, installed=False, active=False, current=False, complete=True)


__all__ = [
    "ScheduleAction",
    "ScheduleExecution",
    "ScheduleExecutionState",
    "ScheduleFile",
    "ScheduleFileState",
    "SchedulePlan",
    "ScheduleReport",
    "ScheduleRequest",
    "ScheduledFile",
    "parse_launchd_execution",
    "parse_systemd_execution",
    "plan_schedule",
    "schedule",
]
