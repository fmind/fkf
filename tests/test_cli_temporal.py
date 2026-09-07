from __future__ import annotations

import io
import json
from datetime import UTC
from pathlib import Path
from threading import Event
from typing import NoReturn

import pytest
import typer

from fkf.base import Base
from fkf.cli import app
from fkf.cli_support import CLIState, run_app
from fkf.cli_temporal import _who_text
from fkf.config import load_config
from fkf.day import WhoMatch, WhoNeighbourGroup, WhoReport
from fkf.documents import Document, day_window, fields_of, parse_day_in_location, schema_of
from fkf.errors import CanceledError
from fkf.graph import build_graph
from fkf.store import Layer

CONFIG = """\
fkf: 1
name: temporal-cli
schema:
  id: {description: Stable identity., cardinality: one}
  time: {description: Event time., cardinality: one}
  title: {description: Display title., cardinality: optional}
  participant: {description: People., cardinality: optional, relation: true}
  repository: {description: Repository., cardinality: optional, relation: true}
identities:
  maxime:
    canonical: person:email/maxime@example.test
    aliases: [actor:github.com/maxime]
    kind: person
  fkf:
    canonical: repo:github.com/fmind/fkf
    aliases: [repository:github.com/fmind/fkf]
    kind: repository
layers: {events: true, index: false, tasks: false, projects: false, wiki: true}
sources:
  meetings:
    enabled: true
    run: [provider, meetings]
    fields: {id: .id, time: .time, title: .title, participant: .participant, repository: .repository}
"""


@pytest.fixture
def temporal_base(tmp_path: Path) -> Path:
    root = tmp_path / "base"
    root.mkdir()
    (root / "fkf.yaml").write_text(CONFIG, encoding="utf-8")
    config = load_config(root)
    base = Base(config=config, store=config.store())
    source = config.sources["meetings"]
    bounds = day_window(parse_day_in_location("2026-05-09", UTC))
    base.write_document(
        Document(
            source="meetings",
            layer=Layer.EVENTS,
            date="2026-05-09",
            window_start=bounds.start,
            window_end=bounds.end,
            collected_at=bounds.end,
            schema=schema_of(source),
            fields=fields_of(source),
            count=1,
            records=[
                {
                    "id": "planning",
                    "time": "2026-05-09T09:00:00Z",
                    "title": "Planning",
                    "participant": "actor:github.com/maxime",
                    "repository": "repo:github.com/fmind/fkf",
                }
            ],
        )
    )
    build_graph(base)
    return root


def invoke(*arguments: str) -> tuple[int, str, str]:
    stdout, stderr = io.StringIO(), io.StringIO()
    return run_app(app, arguments, stdout=stdout, stderr=stderr), stdout.getvalue(), stderr.getvalue()


@pytest.mark.parametrize(("command", "delivery"), [("day", "json"), ("d", "jsonl"), ("day", "text")])
def test_day_delivers_the_exact_requested_format(temporal_base: Path, command: str, delivery: str) -> None:
    code, stdout, stderr = invoke(
        command,
        "2026-05-09",
        "--budget",
        "600",
        "--format",
        delivery,
        "--base",
        str(temporal_base),
    )

    assert code == 0
    assert stderr == ""
    if delivery == "text":
        assert "[meetings] 1/1 records" in stdout
        assert "receipt: 2026-05-09..2026-05-09" in stdout
        used = int(stdout.split("receipt: budget 600 · used ", maxsplit=1)[1].split(" ", maxsplit=1)[0])
    else:
        report = json.loads(stdout)
        assert report["receipt"]["format"] == delivery
        assert report["receipt"]["records"] == 1
        used = report["receipt"]["used_tokens"]
        if delivery == "jsonl":
            assert stdout.count("\n") == 1
    assert used == (len(stdout.encode()) + 3) // 4


def test_timeline_accepts_range_filters_and_an_around_duration(temporal_base: Path) -> None:
    code, stdout, stderr = invoke(
        "timeline",
        "--since",
        "2026-05-09",
        "--until",
        "2026-05-09",
        "--source",
        "meetings",
        "--repo",
        "repo:github.com/fmind/fkf",
        "--person",
        "actor:github.com/maxime",
        "--all",
        "--budget",
        "800",
        "--format",
        "json",
        "--base",
        str(temporal_base),
    )
    assert code == 0
    assert stderr == ""
    report = json.loads(stdout)
    uri = report["groups"][0]["items"][0]["uri"]
    assert report["receipt"]["person"] == "person:email/maxime@example.test"

    code, stdout, stderr = invoke(
        "timeline",
        uri,
        "--around",
        "2h",
        "--budget",
        "800",
        "--format",
        "jsonl",
        "--base",
        str(temporal_base),
    )
    assert code == 0
    assert stderr == ""
    assert stdout.count("\n") == 1
    report = json.loads(stdout)
    assert report["receipt"]["around"] == uri
    assert report["receipt"]["around_window"] == "2h0m0s"


@pytest.mark.parametrize(
    ("arguments", "message"),
    [
        (("day", "today", "yesterday"), "unexpected extra argument"),
        (("day", "--budget", "0"), "expected 1.."),
        (("brief", "--budget", "0"), "expected 1.."),
        (("timeline", "--since", "today", "--around", "0"), "positive duration"),
        (("timeline", "--since", "today", "--around", "nope"), "invalid duration"),
        (("timeline", "one", "two"), "unexpected extra argument"),
        (("who",), "missing argument"),
        (("who", "one", "two"), "unexpected extra argument"),
    ],
)
def test_temporal_usage_failures_are_exit_two_before_opening_a_base(
    arguments: tuple[str, ...],
    message: str,
) -> None:
    code, stdout, stderr = invoke(*arguments)

    assert code == 2
    assert stdout == ""
    assert message.casefold() in stderr.casefold()


def test_brief_and_who_use_stable_jsonl_and_text_renderers(temporal_base: Path) -> None:
    code, stdout, stderr = invoke("brief", "--format", "jsonl", "--base", str(temporal_base))
    assert code == 0
    assert stderr == ""
    assert stdout.count("\n") == 1
    assert len(json.loads(stdout)["sections"]) == 5

    code, stdout, stderr = invoke(
        "w",
        "actor:github.com/maxime",
        "--format",
        "text",
        "--base",
        str(temporal_base),
    )
    assert code == 0
    assert stderr == ""
    assert "person:email/maxime@example.test [person]" in stdout
    assert "source: meetings · 1" in stdout
    assert "total: 1 interaction(s)" in stdout


@pytest.mark.parametrize(
    ("service_name", "arguments"),
    [
        ("day", ("day", "2026-05-09")),
        ("timeline", ("timeline", "--since", "2026-05-09")),
        ("brief", ("brief",)),
        ("who", ("who", "maxime")),
    ],
)
def test_temporal_services_receive_the_invocation_event_and_cancel_with_exit_130(
    temporal_base: Path,
    monkeypatch: pytest.MonkeyPatch,
    service_name: str,
    arguments: tuple[str, ...],
) -> None:
    from fkf import cli_temporal

    invocation_events: list[Event] = []
    original_state = cli_temporal.state

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

    monkeypatch.setattr(cli_temporal, "state", capture_state)
    monkeypatch.setattr(cli_temporal, service_name, cancel_service)

    code, stdout, stderr = invoke(*arguments, "--base", str(temporal_base))

    assert code == 130
    assert stdout == ""
    assert stderr == "fkf: operation canceled\n"


def test_who_text_bounds_each_neighbourhood_line() -> None:
    nodes = tuple(f"events/2026-05-09/meetings.json#{index}" for index in range(200))
    report = WhoReport(
        "busy",
        (
            WhoMatch(
                "person:email/busy@example.test",
                kind="person",
                neighbourhood=(WhoNeighbourGroup("event", nodes),),
                neighbourhood_truncated=True,
                total=200,
            ),
        ),
    )

    rendered = _who_text(report)

    assert "+192 more" in rendered
    assert "neighbourhood: truncated at 200 edges" in rendered
    assert all(len(line.encode()) <= 512 for line in rendered.splitlines())


def test_temporal_help_names_the_complete_surface() -> None:
    code, stdout, stderr = invoke("--help")
    assert code == 0
    assert stderr == ""
    for command in ("day", "timeline", "brief", "who"):
        assert command in stdout

    for command, flags in (
        ("day", ("--budget", "--all")),
        ("timeline", ("--since", "--until", "--source", "--repo", "--person", "--around", "--budget", "--all")),
        ("brief", ("--budget",)),
        ("who", ()),
    ):
        code, stdout, stderr = invoke(command, "--help")
        assert code == 0
        assert stderr == ""
        for flag in flags:
            assert flag in stdout
