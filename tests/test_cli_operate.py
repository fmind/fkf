"""CLI cancellation contracts for status, trust, sync, build, and new."""

from __future__ import annotations

import io
from dataclasses import replace
from datetime import UTC, datetime
from pathlib import Path
from threading import Event
from typing import NoReturn

import pytest
import typer

from fkf.base import Base
from fkf.cli import app
from fkf.cli_support import CLIState, run_app
from fkf.errors import CanceledError
from fkf.new import NewRequest, NewResult
from fkf.process import Cancellation

CONFIG = """\
fkf: 1
name: operate-cli
schema:
  id: {description: Stable identity., cardinality: one}
  time: {description: Event time., cardinality: one}
  title: {description: Meaningful title., cardinality: optional}
layers: {events: true, index: true, tasks: true, projects: true, wiki: true}
sources: {}
"""


def invoke(*arguments: str) -> tuple[int, str, str]:
    stdout, stderr = io.StringIO(), io.StringIO()
    code = run_app(app, arguments, stdout=stdout, stderr=stderr)
    return code, stdout.getvalue(), stderr.getvalue()


@pytest.fixture
def base_root(tmp_path: Path) -> Path:
    root = tmp_path / "base"
    root.mkdir()
    (root / "fkf.yaml").write_text(CONFIG, encoding="utf-8")
    return root


@pytest.mark.parametrize(
    ("service_name", "arguments"),
    [
        ("status_report", ("status",)),
        ("trust", ("trust", "--check")),
        ("build", ("build", "--check")),
        ("create_new", ("new", "task", "cancel-me")),
    ],
)
def test_operate_services_receive_the_invocation_event_and_cancel_with_exit_130(
    base_root: Path,
    monkeypatch: pytest.MonkeyPatch,
    service_name: str,
    arguments: tuple[str, ...],
) -> None:
    from fkf import cli_operate

    invocation_events: list[Event] = []
    original_state = cli_operate.state

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

    monkeypatch.setattr(cli_operate, "state", capture_state)
    monkeypatch.setattr(cli_operate, service_name, cancel_service)

    code, stdout, stderr = invoke(*arguments, "--base", str(base_root))

    assert code == 130
    assert stdout == ""
    assert stderr == "fkf: operation canceled\n"


def test_sync_rebuild_hooks_share_the_invocation_event(base_root: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    from fkf import cli_operate

    invocation_events: list[Event] = []
    rebuilt: list[str] = []
    original_state = cli_operate.state

    def capture_state(ctx: typer.Context) -> CLIState:
        invocation = original_state(ctx)
        invocation_events.append(invocation.cancel)
        return invocation

    def rebuild(name: str):
        def execute(_base: Base, **kwargs: object) -> object:
            assert kwargs["cancel"] is invocation_events[-1]
            rebuilt.append(name)
            return object()

        return execute

    def cancel_sync(base: Base, _request: object, *, rebuild: object, cancel: Event) -> NoReturn:
        from fkf.sync import RebuildHooks

        assert cancel is invocation_events[-1]
        assert isinstance(rebuild, RebuildHooks)
        assert rebuild.wiki is not None
        assert rebuild.graph is not None
        assert rebuild.lexical is not None
        rebuild.wiki(base)
        rebuild.graph(base)
        rebuild.lexical(base)
        cancel.set()
        raise CanceledError("operation canceled")

    monkeypatch.setattr(cli_operate, "state", capture_state)
    monkeypatch.setattr(cli_operate, "build_wiki_index", rebuild("wiki"))
    monkeypatch.setattr(cli_operate, "build_graph", rebuild("graph"))
    monkeypatch.setattr(cli_operate, "build_lexical_index", rebuild("index"))
    monkeypatch.setattr(cli_operate, "sync", cancel_sync)

    code, stdout, stderr = invoke("sync", "--dry-run", "--base", str(base_root))

    assert code == 130
    assert stdout == ""
    assert stderr == "fkf: operation canceled\n"
    assert rebuilt == ["wiki", "graph", "index"]


def test_new_reports_durable_uri_when_post_create_rebuild_is_canceled(
    base_root: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    from fkf import cli_operate

    original_create = cli_operate.create_new

    def fixed_create(
        base: Base,
        request: NewRequest,
        *,
        cancel: Cancellation | None = None,
    ) -> NewResult:
        return original_create(
            base,
            replace(request, now=datetime(2026, 9, 6, tzinfo=UTC)),
            cancel=cancel,
        )

    def cancel_rebuild(_base: Base, **kwargs: object) -> NoReturn:
        cancel = kwargs["cancel"]
        assert isinstance(cancel, Event)
        cancel.set()
        raise CanceledError("operation canceled")

    monkeypatch.setattr(cli_operate, "create_new", fixed_create)
    monkeypatch.setattr(cli_operate, "build", cancel_rebuild)

    code, stdout, stderr = invoke("new", "task", "durable", "--base", str(base_root))

    uri = "tasks/2026-09-06/durable/TASKS.md"
    assert code == 130
    assert stdout == ""
    assert stderr == f"fkf: {uri} was created but derived rebuild was canceled; run `fkf build` to repair it\n"
    assert (base_root / uri).is_file()
