"""Public FKF command tree; callbacks stay thin over typed services."""

from __future__ import annotations

from typing import Annotated

import typer
from typer import _click as click
from typer._click.exceptions import UsageError
from typer._completion_classes import completion_init
from typer.core import TyperGroup

from fkf import DISPLAY_VERSION
from fkf.cli_ask import register_ask_commands
from fkf.cli_browse import register_browse_commands
from fkf.cli_integrate import register_integration_commands
from fkf.cli_learn import register_learn_commands
from fkf.cli_mcp import register_mcp_commands
from fkf.cli_operate import register_operate_commands
from fkf.cli_setup import register_setup_commands
from fkf.cli_support import FKFGroup, initialize_state
from fkf.cli_temporal import register_temporal_commands

completion_init()

app = typer.Typer(
    cls=FKFGroup,
    add_completion=False,
    invoke_without_command=True,
    help="Fmind Knowledge Framework — a local, offline record of your work, for your agent.",
    no_args_is_help=False,
    epilog=(
        "Exit codes are stable: 0 success, 1 partial or operational failure, "
        "2 invalid configuration or usage, 3 untrusted base, 130 cancellation."
    ),
    pretty_exceptions_enable=False,
    rich_markup_mode=None,
    suggest_commands=False,
    context_settings={"help_option_names": ["-h", "--help"]},
)


@app.callback()
def root(
    ctx: typer.Context,
    base: Annotated[
        str,
        typer.Option(
            "--base",
            "-b",
            help="Base directory; then $FKF_BASE; then the nearest ancestor holding fkf.yaml.",
        ),
    ] = "",
    format_name: Annotated[
        str | None,
        typer.Option("--format", "-f", help="Output format: json, jsonl, or text."),
    ] = None,
    version: Annotated[bool, typer.Option("--version", "-v", help="Print the FKF version and exit.")] = False,
) -> None:
    """Initialize one lazy base boundary for the selected command."""
    initialize_state(ctx, base, format_name)
    if version:
        typer.echo(f"fkf version {DISPLAY_VERSION}")
        raise typer.Exit
    if ctx.invoked_subcommand is None:
        typer.echo(ctx.get_help())


@app.command("help", help="Show root help or help for one command (alias: h).")
def help_command(
    ctx: typer.Context,
    topics: Annotated[list[str] | None, typer.Argument(help="Command whose help to show.")] = None,
) -> None:
    """Render help without opening a base; extra topics use the first topic only."""
    root_context = ctx.find_root()
    if not topics:
        typer.echo(root_context.get_help())
        return
    group = root_context.command
    if not isinstance(group, TyperGroup):
        raise RuntimeError("FKF root command is not a command group")
    topic = topics[0]
    command = group.get_command(root_context, topic)
    if command is None:
        raise UsageError(f"No help topic for {topic!r}", root_context)
    help_context = click.Context(command, info_name=command.name or topic, parent=root_context)
    typer.echo(command.get_help(help_context))


register_ask_commands(app)
register_temporal_commands(app)
register_browse_commands(app)
config_app = register_operate_commands(app)
register_setup_commands(app, config_app)
register_mcp_commands(app)
register_integration_commands(app)
register_learn_commands(app)

__all__ = ["app"]
