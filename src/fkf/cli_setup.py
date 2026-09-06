"""CLI adapters for base initialization and bundled-helper maintenance."""

from __future__ import annotations

from typing import Annotated

import typer
from typer import _click as click

from fkf.cli_support import state
from fkf.errors import InvalidUsageError
from fkf.helpers import HelperReport, HelperState, inspect_helpers
from fkf.init import PRESET_MINIMAL, InitReport, InitRequest, init_base
from fkf.locking import WriterLock
from fkf.output import register_text


def _init_text(report: InitReport) -> str:
    verb = "created" if report.created else "refreshed"
    lines = [f"{verb} {report.base}"]
    for step in report.steps:
        marker = "+" if step.changed else " "
        lines.append(f" {marker} {step.item:<18} {step.detail}")
    if report.next:
        lines.extend(("", "next", *(f"  {index}. {item}" for index, item in enumerate(report.next, start=1))))
    return "\n".join(lines)


def _short(digest: str) -> str:
    return digest[:12]


def _helpers_text(report: HelperReport) -> str:
    lines: list[str] = []
    for helper in report.helpers:
        details = [str(helper.state)]
        if helper.required:
            details.append("required")
        if helper.refreshed:
            details.append("refreshed")
        lines.append(f"{helper.path:<28} {', '.join(details)}")
        if helper.state is not HelperState.CURRENT:
            lines.extend(
                (
                    f"  current: {_short(helper.current_sha256) or '-'}",
                    f"  shipped: {_short(helper.shipped_sha256)}",
                )
            )
    lines.extend(
        (
            "",
            (
                f"{report.current} current, {report.drifted} drifted, {report.missing} missing, "
                f"{report.refreshed} refreshed"
            ),
        )
    )
    return "\n".join(lines)


register_text(InitReport, _init_text)
register_text(HelperReport, _helpers_text)


def _specified(ctx: typer.Context, name: str) -> bool:
    return ctx.get_parameter_source(name) is click.core.ParameterSource.COMMANDLINE


def register_setup_commands(app: typer.Typer, config_app: typer.Typer) -> None:
    """Register setup operations while extending the one existing config group."""

    @app.command(
        "init",
        help=(
            "Create a base, or refresh the parts of one that fkf owns. On a new path this writes "
            "the chosen preset's fkf.yaml, enabled layers, managed git blocks, AGENTS.md, bundled "
            "skills, helpers required by enabled sources, and agent bridges. It records trust only "
            "when no execution input predated init. On an existing base it refreshes FKF-owned "
            "skills and managed blocks, creates missing agent bridges, and preserves fkf.yaml, "
            "AGENTS.md, and helpers."
        ),
        short_help="Create a base, or refresh the parts of one that fkf owns.",
    )
    def init_command(
        ctx: typer.Context,
        path: Annotated[str | None, typer.Argument(help="Base path; defaults to the root --base value.")] = None,
        preset: Annotated[
            str | None,
            typer.Option("--preset", help="Initial preset: minimal (default), personal, or team."),
        ] = None,
        name: Annotated[str, typer.Option("--name", help="Base name; defaults to the directory name.")] = "",
        track_collected: Annotated[
            bool,
            typer.Option("--track-collected", help="Track events/ and index/ in append-only Git history."),
        ] = False,
        demo: Annotated[
            int,
            typer.Option("--demo", help="Fill an empty base with 1 to 366 days of synthetic documents."),
        ] = 0,
        skip_git: Annotated[bool, typer.Option("--skip-git", help="Do not initialize a Git repository.")] = False,
        skip_validate: Annotated[
            bool,
            typer.Option("--skip-validate", help="Skip the final scaffold validation."),
        ] = False,
    ) -> None:
        if _specified(ctx, "demo") and not 1 <= demo <= 366:
            raise InvalidUsageError(f"--demo is {demo}; expected 1..366")
        invocation = state(ctx)
        target = path or invocation.base_argument
        if not target.strip():
            raise InvalidUsageError("`fkf init` needs a path, for example `fkf init ~/brain`")
        selected_preset = preset if preset is not None else "" if demo else PRESET_MINIMAL
        request = InitRequest(
            path=target,
            preset=selected_preset,
            name=name,
            track_collected=track_collected,
            demo=demo,
            skip_git=skip_git,
            skip_validate=skip_validate,
        )
        with WriterLock.acquire(target):
            invocation.emit(init_base(request, cancel=invocation.cancel))

    @config_app.command(
        "helpers",
        help="Compare official helpers with this package, or explicitly refresh them.",
    )
    def config_helpers(
        ctx: typer.Context,
        refresh: Annotated[
            bool,
            typer.Option(
                "--refresh",
                help="Atomically restore drifted and missing official helpers required by this base.",
            ),
        ] = False,
    ) -> None:
        invocation = state(ctx)
        base = invocation.base()
        if refresh:
            with WriterLock.acquire(base.root):
                invocation.emit(inspect_helpers(base, refresh=True, cancel=invocation.cancel))
            return
        invocation.emit(inspect_helpers(base, cancel=invocation.cancel))


__all__ = ["register_setup_commands"]
