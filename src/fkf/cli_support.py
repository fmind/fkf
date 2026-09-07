"""Small, testable adapters between Typer/Click and FKF's public CLI contract."""

from __future__ import annotations

import contextlib
import signal
import sys
from collections.abc import Sequence
from contextvars import ContextVar
from dataclasses import dataclass
from threading import Event, current_thread, main_thread
from typing import NoReturn, TextIO

import typer
from typer import _click as click
from typer._click.exceptions import BadParameter, ClickException, UsageError
from typer.core import TyperGroup

from fkf.base import Base, open_base
from fkf.errors import FKFError, exit_code_for
from fkf.output import OutputFormat, default_format, encode, parse_format

_ACTIVE_CANCELLATION: ContextVar[Event | None] = ContextVar("fkf_active_cancellation", default=None)

_ROOT_ALIASES: dict[str, frozenset[str]] = {
    "help": frozenset({"h"}),
    "context": frozenset({"c"}),
    "day": frozenset({"d"}),
    "who": frozenset({"w"}),
    "find": frozenset({"f"}),
    "read": frozenset({"r"}),
    "graph": frozenset({"g"}),
    "list": frozenset({"l"}),
    "validate": frozenset({"v"}),
    "tags": frozenset({"t"}),
    "eval": frozenset({"e"}),
    "sync": frozenset({"s"}),
    "build": frozenset({"b"}),
    "new": frozenset({"n"}),
    "mcp": frozenset({"m"}),
}
_CHILD_ALIASES: dict[str, frozenset[str]] = {
    "list": frozenset({"l"}),
    "events": frozenset({"e"}),
    "index": frozenset({"i"}),
    "tasks": frozenset({"t"}),
    "learned": frozenset({"l"}),
    "projects": frozenset({"p"}),
    "wiki": frozenset({"w"}),
    "records": frozenset({"r"}),
    "nodes": frozenset({"n"}),
    "task": frozenset({"t"}),
    "project": frozenset({"p"}),
    "helper": frozenset({"h"}),
    "helpers": frozenset({"h"}),
    "schema": frozenset({"s"}),
    "serve": frozenset({"s"}),
    "instructions": frozenset({"i"}),
    "print": frozenset({"p"}),
    "install": frozenset({"i"}),
    "propose": frozenset({"p"}),
    "review": frozenset({"v"}),
    "apply": frozenset({"a"}),
    "reject": frozenset({"r"}),
    "status": frozenset({"s"}),
    "remove": frozenset({"r"}),
}
_GLOBAL_VALUE_OPTIONS = frozenset({"--base", "-b", "--format", "-f"})


def normalize_global_options(arguments: Sequence[str]) -> list[str]:
    """Hoist root value options so they remain valid after any subcommand."""
    leading: list[str] = []
    remaining: list[str] = []
    index = 0
    while index < len(arguments):
        argument = arguments[index]
        if argument == "--":
            remaining.extend(arguments[index:])
            break
        if argument in _GLOBAL_VALUE_OPTIONS:
            leading.append(argument)
            if index + 1 < len(arguments):
                leading.append(arguments[index + 1])
                index += 2
                continue
            index += 1
            continue
        if argument.startswith(("--base=", "--format=")):
            leading.append(argument)
            index += 1
            continue
        if argument.startswith(("-b", "-f")) and len(argument) > 2:
            leading.append(argument)
            index += 1
            continue
        remaining.append(argument)
        index += 1
    return [*leading, *remaining]


class FKFGroup(TyperGroup):
    """Resolve the documented aliases and normalize root options once."""

    def parse_args(self, ctx: click.Context, args: list[str]) -> list[str]:
        if ctx.parent is None:
            args = normalize_global_options(args)
        return super().parse_args(ctx, args)

    def get_command(self, ctx: click.Context, cmd_name: str) -> click.Command | None:
        command = super().get_command(ctx, cmd_name)
        if command is not None:
            return command
        aliases = _ROOT_ALIASES if ctx.parent is None else _CHILD_ALIASES
        matches = [name for name in self.commands if cmd_name in aliases.get(name, frozenset())]
        return super().get_command(ctx, matches[0]) if len(matches) == 1 else None


@dataclass(slots=True)
class CLIState:
    """One invocation's lazily opened base and output selection."""

    base_argument: str
    output_format: OutputFormat
    stdout: TextIO
    stderr: TextIO
    cancel: Event
    _base: Base | None = None

    def base(self) -> Base:
        if self._base is None:
            self._base = open_base(self.base_argument)
        return self._base

    def emit(self, result: object) -> None:
        data, native = encode(result, self.output_format)
        if not native:
            self.stderr.write("fkf: no text rendering for this result; showing JSON\n")
        _write_bytes(self.stdout, data)

    def write(self, data: bytes) -> None:
        """Write pre-encoded public bytes without a second serialization pass."""
        _write_bytes(self.stdout, data)


def state(ctx: typer.Context) -> CLIState:
    """Return the root invocation state from any nested callback."""
    root = ctx.find_root()
    value = root.obj
    if not isinstance(value, CLIState):
        raise RuntimeError("FKF CLI state was not initialized")
    return value


def _write_bytes(stream: TextIO, data: bytes) -> None:
    buffer = getattr(stream, "buffer", None)
    if buffer is not None:
        buffer.write(data)
        buffer.flush()
        return
    stream.write(data.decode("utf-8"))
    stream.flush()


def initialize_state(ctx: typer.Context, base: str, format_name: str | None) -> CLIState:
    """Create root state after validating the public output vocabulary."""
    try:
        output_format = default_format(sys.stdout) if format_name is None else parse_format(format_name)
    except ValueError as error:
        raise BadParameter(str(error), param_hint="--format") from error
    cancel = _ACTIVE_CANCELLATION.get() or Event()
    value = CLIState(base, output_format, sys.stdout, sys.stderr, cancel)
    ctx.obj = value
    return value


def parent_without_command(ctx: typer.Context, reason: str) -> None:
    """Print a container's help, then fail with the stable usage category."""
    if ctx.invoked_subcommand is None:
        click.echo(ctx.get_help())
        raise UsageError(reason, ctx)


@contextlib.contextmanager
def _cancel_on_signals(cancel: Event):
    """Turn the first termination signal into cooperative cancellation."""
    if current_thread() is not main_thread():
        yield
        return
    signals = (signal.SIGINT, signal.SIGTERM)
    previous = {number: signal.getsignal(number) for number in signals}

    def request_cancel(_signum: int, _frame: object) -> None:
        if cancel.is_set():
            raise KeyboardInterrupt
        cancel.set()

    for number in signals:
        signal.signal(number, request_cancel)
    try:
        yield
    finally:
        for number in signals:
            if signal.getsignal(number) is request_cancel:
                signal.signal(number, previous[number])


def run_app(
    app: typer.Typer,
    arguments: Sequence[str] | None = None,
    *,
    stdout: TextIO | None = None,
    stderr: TextIO | None = None,
) -> int:
    """Run an app without framework-owned exits or provider-detail diagnostics."""
    command = typer.main.get_command(app)
    output = sys.stdout if stdout is None else stdout
    errors = sys.stderr if stderr is None else stderr
    argv = list(sys.argv[1:] if arguments is None else arguments)
    cancel = Event()
    token = _ACTIVE_CANCELLATION.set(cancel)
    try:
        with _cancel_on_signals(cancel), contextlib.redirect_stdout(output), contextlib.redirect_stderr(errors):
            command.main(args=argv, prog_name="fkf", standalone_mode=False)
    except ClickException as error:
        errors.write(f"fkf: {error.format_message()}\n")
        return 2 if isinstance(error, (UsageError, BadParameter)) else error.exit_code
    except (KeyboardInterrupt, SystemExit) as error:
        if isinstance(error, SystemExit) and isinstance(error.code, int):
            return error.code
        return 130
    except FKFError as error:
        errors.write(f"fkf: {error}\n")
        return error.exit_code
    except BrokenPipeError:
        return 1
    except Exception as error:
        errors.write(f"fkf: {error}\n")
        return exit_code_for(error)
    finally:
        _ACTIVE_CANCELLATION.reset(token)
    return 0


def exit_main(app: typer.Typer) -> NoReturn:
    raise SystemExit(run_app(app))


__all__ = [
    "CLIState",
    "FKFGroup",
    "exit_main",
    "initialize_state",
    "normalize_global_options",
    "parent_without_command",
    "run_app",
    "state",
]
