"""CLI adapters for the read-only MCP server."""

from __future__ import annotations

import asyncio
from contextlib import suppress
from typing import TYPE_CHECKING

import typer

from fkf.cli_support import FKFGroup, parent_without_command, state
from fkf.errors import CanceledError, InvalidUsageError
from fkf.process import Cancellation

if TYPE_CHECKING:
    from mcp.server.mcpserver import MCPServer

    from fkf.base import Base


def create_server(base: Base, *, cancel: Cancellation) -> MCPServer[None]:
    """Load the sizeable MCP SDK only for the command that actually serves it."""
    from fkf.mcp_server import create_server as create

    return create(base, cancel=cancel)


def instructions(base: Base) -> str:
    """Keep ordinary CLI startup independent from the MCP SDK import graph."""
    from fkf.mcp_server import instructions as render

    return render(base)


async def _serve_stdio(server: MCPServer[None], cancel: Cancellation) -> None:
    """Run the SDK's public async stdio boundary until it ends or FKF is canceled."""
    task = asyncio.create_task(server.run_stdio_async())
    try:
        while not task.done() and not cancel.is_set():
            try:
                await asyncio.wait_for(asyncio.shield(task), timeout=0.05)
            except TimeoutError:
                continue
        if cancel.is_set():
            task.cancel()
            with suppress(asyncio.CancelledError):
                await task
            raise CanceledError("MCP server canceled")
        await task
    finally:
        if not task.done():
            task.cancel()
            with suppress(asyncio.CancelledError):
                await task


def _run_stdio(server: MCPServer[None], cancel: Cancellation) -> None:
    """Run stdio while bridging FKF's process-level cancellation event."""
    asyncio.run(_serve_stdio(server, cancel))


def register_mcp_commands(app: typer.Typer) -> None:
    """Register inspection and stdio serving for one base."""

    mcp_app = typer.Typer(
        cls=FKFGroup,
        invoke_without_command=True,
        no_args_is_help=False,
        help="Serve this base to an agent over MCP, read-only.",
        rich_markup_mode=None,
    )
    app.add_typer(mcp_app, name="mcp")

    @mcp_app.callback()
    def mcp_parent(ctx: typer.Context) -> None:
        parent_without_command(ctx, "name a subcommand")

    @mcp_app.command(
        "serve",
        help=(
            "Run the read-only stdio server; --base is required. Exposes context, find, day, timeline, list, read, "
            "and graph."
        ),
    )
    def mcp_serve(ctx: typer.Context) -> None:
        invocation = state(ctx)
        if not invocation.base_argument:
            raise InvalidUsageError("fkf mcp serve requires an explicit --base")
        _run_stdio(create_server(invocation.base(), cancel=invocation.cancel), invocation.cancel)

    @mcp_app.command("instructions", help="Print the instructions this base would send to a connecting client.")
    def mcp_instructions(ctx: typer.Context) -> None:
        invocation = state(ctx)
        invocation.write(instructions(invocation.base()).encode())


__all__ = ["register_mcp_commands"]
