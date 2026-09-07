"""CLI contracts for base initialization and official-helper maintenance."""

from __future__ import annotations

import io
import json
from pathlib import Path
from threading import Event
from typing import NoReturn, cast

import pytest
import typer
from typer.core import TyperGroup
from typer.main import get_command

from fkf.cli import app
from fkf.cli_setup import _helpers_text, _init_text
from fkf.cli_support import CLIState, run_app
from fkf.errors import CanceledError
from fkf.helpers import HelperReport, HelperState, HelperStatus
from fkf.init import InitReport, InitStep
from fkf.locking import WriterLock


def invoke(*arguments: str) -> tuple[int, str, str]:
    stdout, stderr = io.StringIO(), io.StringIO()
    code = run_app(app, arguments, stdout=stdout, stderr=stderr)
    return code, stdout.getvalue(), stderr.getvalue()


def test_setup_commands_share_the_existing_config_group_and_publish_help() -> None:
    root = cast("TyperGroup", get_command(app))
    assert list(root.commands).count("config") == 1
    assert "init" in root.commands
    config = cast("TyperGroup", root.commands["config"])
    assert set(config.commands) >= {"helpers", "schema"}

    code, stdout, stderr = invoke("init", "--help")
    assert code == 0
    assert stderr == ""
    for contract in (
        "records trust only",
        "no execution input predated init",
        "agent bridges",
        "preserves",
        "--preset",
        "--track-collected",
        "--skip-validate",
    ):
        assert contract in stdout


def test_init_needs_a_target_without_trying_to_open_a_base() -> None:
    code, stdout, stderr = invoke("init", "--skip-git")

    assert code == 2
    assert stdout == ""
    assert "needs a path" in stderr
    assert "no fkf base" not in stderr


def test_init_uses_positional_then_global_base_and_delivers_all_formats(tmp_path: Path) -> None:
    positional = tmp_path / "positional"
    ignored_global = tmp_path / "ignored"
    code, stdout, stderr = invoke(
        "init",
        str(positional),
        "--base",
        str(ignored_global),
        "--preset",
        "minimal",
        "--name",
        "chosen",
        "--track-collected",
        "--skip-git",
        "--skip-validate",
        "--format",
        "json",
    )

    assert code == 0
    assert stderr == ""
    report = json.loads(stdout)
    assert report["base"] == str(positional)
    assert report["name"] == "chosen"
    assert report["track_collected"] is True
    assert positional.joinpath("fkf.yaml").is_file()
    assert not ignored_global.exists()

    code, stdout, stderr = invoke("init", "--base", str(positional), "--skip-git", "--format", "jsonl")
    assert code == 0
    assert stderr == ""
    assert stdout.count("\n") == 1
    assert json.loads(stdout)["refreshed"] is True

    code, stdout, stderr = invoke("init", str(positional), "--skip-git", "--format", "text")
    assert code == 0
    assert stderr == ""
    assert stdout.startswith(f"refreshed {positional}\n")
    assert "never rewrites" in stdout


def test_init_rejects_invalid_demo_usage_before_creating_the_target(tmp_path: Path) -> None:
    for days in (-1, 367):
        target = tmp_path / f"invalid-{days}"
        code, stdout, stderr = invoke("init", str(target), "--demo", str(days), "--skip-git")
        assert code == 2
        assert stdout == ""
        assert "expected 1..366" in stderr
        assert not target.exists()

    target = tmp_path / "mixed"
    code, stdout, stderr = invoke("init", str(target), "--demo", "1", "--preset", "minimal", "--skip-git")
    assert code == 2
    assert stdout == ""
    assert "omit --preset" in stderr
    assert not target.exists()


def test_init_and_helper_refresh_are_writers_but_helper_inspection_is_a_reader(tmp_path: Path) -> None:
    root = tmp_path / "brain"
    code, _, stderr = invoke("init", str(root), "--skip-git", "--format", "json")
    assert code == 0, stderr
    hook = root / "bin" / "fkf-hook.py"
    hook.write_text("#!/bin/sh\necho edited\n", encoding="utf-8")

    with WriterLock.acquire(root):
        code, stdout, stderr = invoke("config", "helpers", "--base", str(root), "--format", "json")
        assert code == 0
        assert json.loads(stdout)["drifted"] == 1
        assert stderr == ""

        code, stdout, stderr = invoke("config", "helpers", "--refresh", "--base", str(root))
        assert code == 1
        assert stdout == ""
        assert "active writer" in stderr

        code, stdout, stderr = invoke("init", str(root), "--skip-git")
        assert code == 1
        assert stdout == ""
        assert "active writer" in stderr


def test_helpers_output_is_exact_and_jsonl_keeps_the_envelope(tmp_path: Path) -> None:
    root = tmp_path / "brain"
    code, _, stderr = invoke("init", str(root), "--skip-git")
    assert code == 0, stderr
    (root / "bin" / "fkf-hook.py").write_text("drifted\n", encoding="utf-8")

    code, stdout, stderr = invoke("config", "helpers", "--base", str(root), "--format", "text")
    assert code == 0
    assert stderr == ""
    assert "bin/fkf-hook.py              drifted, required" in stdout
    assert "  current: " in stdout
    assert "  shipped: " in stdout
    assert stdout.endswith("\n0 current, 1 drifted, 0 missing, 0 refreshed\n")

    code, stdout, stderr = invoke("config", "helpers", "--base", str(root), "--format", "jsonl")
    assert code == 0
    assert stderr == ""
    assert stdout.count("\n") == 1
    report = json.loads(stdout)
    assert report["base"] == str(root)
    assert report["drifted"] == 1
    assert isinstance(report["helpers"], list)


@pytest.mark.parametrize(
    ("service_name", "arguments"),
    [
        ("init_base", ("init",)),
        ("inspect_helpers", ("config", "helpers")),
        ("inspect_helpers", ("config", "helpers", "--refresh")),
    ],
)
def test_setup_services_receive_the_invocation_event_and_cancel_with_exit_130(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
    service_name: str,
    arguments: tuple[str, ...],
) -> None:
    from fkf import cli_setup

    root = tmp_path / "brain"
    code, _stdout, stderr = invoke("init", str(root), "--skip-git")
    assert code == 0, stderr
    invocation_events: list[Event] = []
    original_state = cli_setup.state

    def capture_state(ctx: typer.Context) -> CLIState:
        invocation = original_state(ctx)
        invocation_events.append(invocation.cancel)
        return invocation

    def cancel_service(*_args: object, **kwargs: object) -> NoReturn:
        received = kwargs.get("cancel")
        assert invocation_events
        assert received is invocation_events[-1]
        invocation_events[-1].set()
        raise CanceledError("operation canceled")

    monkeypatch.setattr(cli_setup, "state", capture_state)
    monkeypatch.setattr(cli_setup, service_name, cancel_service)

    command = (*arguments, str(root)) if service_name == "init_base" else (*arguments, "--base", str(root))
    code, stdout, stderr = invoke(*command)

    assert code == 130
    assert stdout == ""
    assert stderr == "fkf: operation canceled\n"


def test_setup_text_renderers_match_the_public_go_layout() -> None:
    init = InitReport(
        base="/base",
        name="brain",
        created=True,
        steps=[InitStep("fkf.yaml", "ready", True), InitStep("AGENTS.md", "preserved", False)],
        next=("fkf status", "fkf sync"),
    )
    assert _init_text(init).splitlines() == [
        "created /base",
        " + fkf.yaml           ready",
        "   AGENTS.md          preserved",
        "",
        "next",
        "  1. fkf status",
        "  2. fkf sync",
    ]

    helpers = HelperReport(
        base="/base",
        helpers=(
            HelperStatus(
                "drifted.sh",
                "bin/drifted.sh",
                HelperState.DRIFTED,
                True,
                "b" * 64,
                "c" * 64,
                True,
            ),
            HelperStatus("missing.sh", "bin/missing.sh", HelperState.MISSING, False, shipped_sha256="d" * 64),
        ),
        current=0,
        drifted=1,
        missing=1,
        refreshed=1,
    )
    assert _helpers_text(helpers).splitlines() == [
        "bin/drifted.sh               drifted, required, refreshed",
        "  current: bbbbbbbbbbbb",
        "  shipped: cccccccccccc",
        "bin/missing.sh               missing",
        "  current: -",
        "  shipped: dddddddddddd",
        "",
        "0 current, 1 drifted, 1 missing, 1 refreshed",
    ]
