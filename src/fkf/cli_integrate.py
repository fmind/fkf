"""CLI adapters for persistent harness and native schedule integrations."""

from __future__ import annotations

import os
import sys
from dataclasses import dataclass
from pathlib import Path
from typing import Annotated

import typer

from fkf.cli_support import FKFGroup, parent_without_command, state
from fkf.errors import InvalidUsageError, OperationalError
from fkf.harness import (
    HarnessInstallReport,
    HarnessInstallRequest,
    HarnessPlan,
    harness_names,
    harness_plan_for,
    install_harnesses,
)
from fkf.locking import WriterLock
from fkf.output import register_jsonl, register_text
from fkf.schedule import ScheduleAction, ScheduleReport, ScheduleRequest, schedule


@dataclass(frozen=True, slots=True)
class HarnessList:
    """Stable public envelope for the closed harness vocabulary."""

    harnesses: tuple[str, ...]


register_jsonl(HarnessList, lambda result: (result,))
register_text(HarnessList, lambda result: "\n".join(result.harnesses))


def _harness_plan_text(plan: HarnessPlan) -> str:
    lines = [f"# Base: {plan.base_name} ({plan.base})"]
    if plan.workspace is not None:
        lines.append(f"# Workspace: {plan.workspace}")
    for fragment in plan.fragments:
        heading = f"# {fragment.path} ({fragment.kind}"
        if fragment.selector:
            heading += f": {fragment.selector}"
        lines.extend(("", heading + ")", fragment.content))
    lines.extend(f"\n# Note: {note}" for note in plan.notes)
    return "\n".join(lines)


def _harness_install_text(report: HarnessInstallReport) -> str:
    if not report.changes:
        return f"harness {report.mode} for {report.base_name} ({report.base}): current"
    lines: list[str] = []
    for change in report.changes:
        if change.backup is not None:
            lines.append(f"backup {change.path} -> {change.backup}")
        lines.append(f"{change.action} {change.path} [{change.harness}]")
    return "\n".join(lines)


def _schedule_text(report: ScheduleReport) -> str:
    if report.current:
        condition = "current"
    elif report.active and not report.installed:
        condition = "active-with-missing-files"
    elif report.installed:
        condition = "drifted"
    else:
        condition = "missing"
    prefix = "schedule dry-run" if report.dry_run else "schedule"
    execution = f"last execution: {report.last_execution.state}"
    if report.last_execution.exit_code is not None:
        execution += f" (exit {report.last_execution.exit_code})"
    if report.last_execution.timestamp:
        execution += f" at {report.last_execution.timestamp}"
    files = (f"{item.state}: {item.path}" for item in report.files)
    return "\n".join((f"{prefix} {report.platform} {report.name}: {condition}", execution, *files))


register_text(HarnessPlan, _harness_plan_text)
register_text(HarnessInstallReport, _harness_install_text)
register_jsonl(HarnessInstallReport, lambda report: report.changes)
register_text(ScheduleReport, _schedule_text)
register_jsonl(ScheduleReport, lambda report: report.files)


def register_integration_commands(app: typer.Typer) -> None:
    """Register harness and schedule command groups."""

    harness_app = typer.Typer(
        cls=FKFGroup,
        invoke_without_command=True,
        no_args_is_help=False,
        help="Print or install one base's MCP, context-hook, and skill integrations.",
        rich_markup_mode=None,
    )
    app.add_typer(harness_app, name="harness")

    @harness_app.callback()
    def harness_parent(ctx: typer.Context) -> None:
        parent_without_command(ctx, "name a subcommand")

    @harness_app.command("list", help="List the closed supported harness vocabulary.")
    def harness_list(ctx: typer.Context) -> None:
        state(ctx).emit(HarnessList(harness_names()))

    @harness_app.command("print", help="Print the exact managed fragments for a dotfile template.")
    def harness_print(
        ctx: typer.Context,
        name: Annotated[str, typer.Argument()],
        workspace: Annotated[str, typer.Option("--workspace")] = "",
        executable: Annotated[
            str, typer.Option("--executable", help="Persistent FKF launcher; defaults to PATH.")
        ] = "",
    ) -> None:
        invocation = state(ctx)
        invocation.emit(
            harness_plan_for(
                invocation.base().root,
                name,
                executable=executable,
                workspace=workspace,
                path=os.environ.get("PATH", ""),
            )
        )

    @harness_app.command("install", help="Install or verify managed user-scope integrations.")
    def harness_install(
        ctx: typer.Context,
        names: Annotated[list[str] | None, typer.Argument()] = None,
        all_harnesses: Annotated[bool, typer.Option("--all")] = False,
        dry_run: Annotated[bool, typer.Option("--dry-run")] = False,
        check: Annotated[bool, typer.Option("--check")] = False,
        workspace: Annotated[str, typer.Option("--workspace")] = "",
        executable: Annotated[
            str, typer.Option("--executable", help="Persistent FKF launcher; defaults to PATH.")
        ] = "",
    ) -> None:
        selected = tuple(names or ())
        if all_harnesses and selected:
            raise InvalidUsageError("fkf harness install --all cannot be combined with harness names")
        if not all_harnesses and not selected:
            raise InvalidUsageError("usage: fkf harness install <name>... | --all")
        if check and dry_run:
            raise InvalidUsageError("fkf harness install --check cannot be combined with --dry-run")
        invocation = state(ctx)
        base = invocation.base()
        request = HarnessInstallRequest(
            selected,
            all_harnesses,
            dry_run,
            check,
            Path.home(),
            executable,
            workspace,
            os.environ.get("PATH", ""),
        )

        def execute() -> None:
            report = install_harnesses(base.root, request, cancel=invocation.cancel)
            invocation.emit(report)
            if check and not report.complete:
                raise OperationalError(f"{len(report.changes)} harness integration change(s) required")

        if check or dry_run:
            execute()
        else:
            with WriterLock.acquire(base.root):
                execute()

    schedule_app = typer.Typer(
        cls=FKFGroup,
        invoke_without_command=True,
        no_args_is_help=False,
        help="Install, inspect, or remove this base's hourly user schedule.",
        rich_markup_mode=None,
    )
    app.add_typer(schedule_app, name="schedule")

    @schedule_app.callback()
    def schedule_parent(ctx: typer.Context) -> None:
        parent_without_command(ctx, "name a subcommand")

    def execute_schedule(ctx: typer.Context, action: ScheduleAction, executable: str, dry_run: bool) -> None:
        invocation = state(ctx)
        base = invocation.base()
        request = ScheduleRequest(
            action,
            Path.home(),
            "darwin" if sys.platform == "darwin" else "linux" if sys.platform.startswith("linux") else sys.platform,
            os.environ.get("PATH", ""),
            executable,
            os.getuid(),
            dry_run,
        )

        def run() -> None:
            invocation.emit(schedule(base.root, request, cancel=invocation.cancel))

        if action is ScheduleAction.STATUS or dry_run:
            run()
        else:
            with WriterLock.acquire(base.root):
                run()

    @schedule_app.command("install", help="Install and activate the managed hourly user schedule.")
    def schedule_install(
        ctx: typer.Context,
        executable: Annotated[str, typer.Option("--executable")] = "",
        dry_run: Annotated[bool, typer.Option("--dry-run")] = False,
    ) -> None:
        execute_schedule(ctx, ScheduleAction.INSTALL, executable, dry_run)

    @schedule_app.command("status", help="Report whether the managed hourly user schedule is installed and current.")
    def schedule_status(
        ctx: typer.Context,
        executable: Annotated[str, typer.Option("--executable")] = "",
    ) -> None:
        execute_schedule(ctx, ScheduleAction.STATUS, executable, False)

    @schedule_app.command("remove", help="Deactivate and remove the managed hourly user schedule.")
    def schedule_remove(
        ctx: typer.Context,
        executable: Annotated[str, typer.Option("--executable")] = "",
        dry_run: Annotated[bool, typer.Option("--dry-run")] = False,
    ) -> None:
        execute_schedule(ctx, ScheduleAction.REMOVE, executable, dry_run)


__all__ = ["HarnessList", "register_integration_commands"]
