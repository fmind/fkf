from __future__ import annotations

import contextlib
import io
import os
import signal
import subprocess
import sys
import threading
import time
from pathlib import Path

import typer

from fkf.cli import app as fkf_app
from fkf.cli_support import FKFGroup, initialize_state, normalize_global_options, run_app, state
from fkf.process import Command, CommandFailureError, SubprocessRunner
from fkf.timeutil import DurationNS


def test_global_options_are_hoisted_without_crossing_double_dash() -> None:
    assert normalize_global_options(["find", "term", "--format", "jsonl", "-b/tmp/base"]) == [
        "--format",
        "jsonl",
        "-b/tmp/base",
        "find",
        "term",
    ]
    assert normalize_global_options(["find", "--", "--format", "text"]) == [
        "find",
        "--",
        "--format",
        "text",
    ]


def _application() -> typer.Typer:
    app = typer.Typer(cls=FKFGroup, invoke_without_command=True, no_args_is_help=False)

    @app.callback()
    def root(
        ctx: typer.Context,
        base: str = typer.Option("", "--base", "-b"),
        format_name: str | None = typer.Option(None, "--format", "-f"),
    ) -> None:
        value = initialize_state(ctx, base, format_name)
        if ctx.invoked_subcommand is None:
            value.emit({"root": True})

    @app.command("context")
    def context(ctx: typer.Context, term: str) -> None:
        ctx.find_root().obj.emit({"term": term})

    return app


def test_alias_and_trailing_global_option_work_together() -> None:
    stdout, stderr = io.StringIO(), io.StringIO()
    assert run_app(_application(), ["c", "needle", "--format", "json"], stdout=stdout, stderr=stderr) == 0
    assert stdout.getvalue() == '{\n  "term": "needle"\n}\n'
    assert stderr.getvalue() == ""


def test_root_and_nested_aliases_are_scoped_to_their_command_group() -> None:
    app = typer.Typer(cls=FKFGroup)
    schedule = typer.Typer(cls=FKFGroup)

    @app.command("sync")
    def sync() -> None:
        typer.echo("sync")

    @app.command("status")
    def root_status() -> None:
        typer.echo("root status")

    @schedule.command("status")
    def schedule_status() -> None:
        typer.echo("schedule status")

    app.add_typer(schedule, name="schedule")

    stdout, stderr = io.StringIO(), io.StringIO()
    assert run_app(app, ["s"], stdout=stdout, stderr=stderr) == 0
    assert stdout.getvalue() == "sync\n"
    assert stderr.getvalue() == ""

    stdout, stderr = io.StringIO(), io.StringIO()
    assert run_app(app, ["schedule", "s"], stdout=stdout, stderr=stderr) == 0
    assert stdout.getvalue() == "schedule status\n"
    assert stderr.getvalue() == ""


def test_colliding_child_aliases_resolve_within_their_own_group() -> None:
    app = typer.Typer(cls=FKFGroup)
    config = typer.Typer(cls=FKFGroup)
    new = typer.Typer(cls=FKFGroup)
    harness = typer.Typer(cls=FKFGroup)

    @config.command("helpers")
    def config_helpers() -> None:
        typer.echo("config helpers")

    @new.command("helper")
    def new_helper() -> None:
        typer.echo("new helper")

    @harness.command("list")
    def harness_list() -> None:
        typer.echo("harness list")

    app.add_typer(config, name="config")
    app.add_typer(new, name="new")
    app.add_typer(harness, name="harness")

    for arguments, expected in (
        (["config", "h"], "config helpers\n"),
        (["new", "h"], "new helper\n"),
        (["harness", "l"], "harness list\n"),
    ):
        stdout, stderr = io.StringIO(), io.StringIO()
        assert run_app(app, arguments, stdout=stdout, stderr=stderr) == 0
        assert stdout.getvalue() == expected
        assert stderr.getvalue() == ""


def test_unknown_command_is_stable_usage_failure() -> None:
    stdout, stderr = io.StringIO(), io.StringIO()
    assert run_app(_application(), ["bogus"], stdout=stdout, stderr=stderr) == 2
    assert stdout.getvalue() == ""
    assert stderr.getvalue().startswith("fkf: No such command 'bogus'")


def test_help_command_and_root_alias_render_without_opening_a_base() -> None:
    rendered: list[str] = []
    for topic in ("help", "h"):
        stdout, stderr = io.StringIO(), io.StringIO()
        assert run_app(fkf_app, [topic], stdout=stdout, stderr=stderr) == 0
        assert stderr.getvalue() == ""
        assert "Commands:" in stdout.getvalue()
        assert "(alias: h)" in stdout.getvalue()
        rendered.append(stdout.getvalue())
    assert rendered[0] == rendered[1]

    command_help: list[str] = []
    for arguments in (("help", "read"), ("h", "r"), ("help", "read", "ignored")):
        stdout, stderr = io.StringIO(), io.StringIO()
        assert run_app(fkf_app, arguments, stdout=stdout, stderr=stderr) == 0
        assert stderr.getvalue() == ""
        assert stdout.getvalue().startswith("Usage: fkf read ")
        command_help.append(stdout.getvalue())
    assert len(set(command_help)) == 1

    stdout, stderr = io.StringIO(), io.StringIO()
    assert run_app(fkf_app, ["help", "not-a-command"], stdout=stdout, stderr=stderr) == 2
    assert stdout.getvalue() == ""
    assert stderr.getvalue() == "fkf: No help topic for 'not-a-command'\n"


def test_invalid_format_is_usage_failure() -> None:
    stdout, stderr = io.StringIO(), io.StringIO()
    assert run_app(_application(), ["--format", "yaml"], stdout=stdout, stderr=stderr) == 2
    assert "expected json, jsonl, or text" in stderr.getvalue()


def _provider_failure_app(returncode: int) -> typer.Typer:
    app = typer.Typer()

    @app.command("fail")
    def fail() -> None:
        raise CommandFailureError(returncode, b"private-provider-stderr")

    return app


def test_provider_exit_status_remains_a_stable_private_operational_failure() -> None:
    for returncode in (7, -signal.SIGTERM):
        app = _provider_failure_app(returncode)

        stdout, stderr = io.StringIO(), io.StringIO()
        assert run_app(app, [], stdout=stdout, stderr=stderr) == 1
        assert stdout.getvalue() == ""
        assert "private-provider-stderr" not in stderr.getvalue()


def test_sigint_cancels_the_active_process_group_and_restores_the_handler(tmp_path: Path) -> None:
    ready = tmp_path / "ready"
    escaped = tmp_path / "escaped"
    app = typer.Typer()

    @app.command("wait")
    def wait(ctx: typer.Context) -> None:
        script = (
            "import pathlib,subprocess,sys,time;"
            "subprocess.Popen([sys.executable,'-c',"
            "'import pathlib,time;time.sleep(0.6);pathlib.Path(sys.argv[1]).write_text(\\\"escaped\\\")',"
            "sys.argv[2]]);"
            "pathlib.Path(sys.argv[1]).write_text('ready');"
            "time.sleep(30)"
        )
        SubprocessRunner().run(
            Command(
                (sys.executable, "-c", script, os.fspath(ready), os.fspath(escaped)),
                DurationNS(30_000_000_000),
            ),
            cancel=state(ctx).cancel,
        )

    @app.callback()
    def root(ctx: typer.Context) -> None:
        initialize_state(ctx, "", "json")

    def interrupt_when_ready() -> None:
        deadline = time.monotonic() + 5
        while not ready.exists() and time.monotonic() < deadline:
            time.sleep(0.01)
        if ready.exists():
            os.kill(os.getpid(), signal.SIGINT)

    previous = signal.getsignal(signal.SIGINT)
    interrupter = threading.Thread(target=interrupt_when_ready)
    interrupter.start()
    started = time.monotonic()
    stdout, stderr = io.StringIO(), io.StringIO()
    code = run_app(app, ["wait"], stdout=stdout, stderr=stderr)
    elapsed = time.monotonic() - started
    interrupter.join(timeout=5)

    assert code == 130
    assert elapsed < 2
    assert ready.read_text() == "ready"
    time.sleep(0.7)
    assert not escaped.exists()
    assert signal.getsignal(signal.SIGINT) is previous


def test_sigterm_cancels_the_active_process_group(tmp_path: Path) -> None:
    ready = tmp_path / "ready"
    escaped = tmp_path / "escaped"
    grandchild = tmp_path / "grandchild.py"
    provider = tmp_path / "provider.py"
    driver = tmp_path / "driver.py"
    grandchild.write_text(
        "import pathlib,sys,time\ntime.sleep(0.6)\npathlib.Path(sys.argv[1]).write_text('escaped')\n",
        encoding="utf-8",
    )
    provider.write_text(
        "import os,pathlib,subprocess,sys,time\n"
        "subprocess.Popen([sys.executable, sys.argv[3], sys.argv[2]])\n"
        "pathlib.Path(sys.argv[1]).write_text(str(os.getpid()))\n"
        "time.sleep(30)\n",
        encoding="utf-8",
    )
    driver.write_text(
        "import sys,typer\n"
        "from fkf.cli_support import initialize_state,run_app,state\n"
        "from fkf.process import Command,SubprocessRunner\n"
        "from fkf.timeutil import DurationNS\n"
        "app=typer.Typer()\n"
        "@app.callback()\n"
        "def root(ctx: typer.Context): initialize_state(ctx, '', 'json')\n"
        "@app.command('wait')\n"
        "def wait(ctx: typer.Context):\n"
        " SubprocessRunner().run(Command((sys.executable,sys.argv[1],sys.argv[2],sys.argv[3],sys.argv[4]),DurationNS(30_000_000_000)),cancel=state(ctx).cancel)\n"
        "raise SystemExit(run_app(app,['wait']))\n",
        encoding="utf-8",
    )

    process = subprocess.Popen(  # noqa: S603 - the test owns every script and path.
        [
            sys.executable,
            os.fspath(driver),
            os.fspath(provider),
            os.fspath(ready),
            os.fspath(escaped),
            os.fspath(grandchild),
        ],
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
    )
    provider_pid: int | None = None
    try:
        deadline = time.monotonic() + 5
        while not ready.exists() and time.monotonic() < deadline:
            time.sleep(0.01)
        assert ready.exists()
        provider_pid = int(ready.read_text())
        started = time.monotonic()
        process.send_signal(signal.SIGTERM)
        code = process.wait(timeout=5)
        elapsed = time.monotonic() - started
        time.sleep(0.7)
        child_escaped = escaped.exists()
    finally:
        if process.poll() is None:
            process.kill()
            process.wait()
        if provider_pid is not None:
            with contextlib.suppress(ProcessLookupError):
                os.killpg(provider_pid, signal.SIGKILL)

    assert code == 130
    assert elapsed < 2
    assert not child_escaped
