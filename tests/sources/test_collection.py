from __future__ import annotations

from dataclasses import dataclass, field
from datetime import UTC, datetime
from pathlib import Path

import pytest

from fkf.collection import collect, collect_window
from fkf.config import load_config
from fkf.documents import IncompleteCollectionError, day_window, parse_day_in_location
from fkf.process import Command, CommandResult
from fkf.source_runtime import Environment
from fkf.timeutil import parse_duration

CONFIG = """\
fkf: 1
name: brain
schema:
  id: {description: Stable identity., cardinality: one}
  time: {description: Event time., cardinality: one}
  title: {description: Meaningful title., cardinality: optional}
layers: {events: true, index: true, tasks: false, projects: false, wiki: false}
sources:
  events:
    enabled: true
    run: [provider, --start, "{{start}}", --end, "{{end}}"]
    fields: {id: .id, time: .time, title: .title}
  windowed:
    enabled: true
    window: true
    run: [provider, --start, "{{start}}", --end, "{{end}}"]
    fields: {id: .id, time: .time, title: .title}
  snapshot:
    enabled: true
    layer: index
    run: [provider]
    fields: {id: .id, title: .title}
"""


@dataclass
class FakeRunner:
    stdout: bytes
    calls: list[Command] = field(default_factory=list)

    def run(self, command: Command, *, cancel: object | None = None) -> CommandResult:
        del cancel
        self.calls.append(command)
        return CommandResult(self.stdout)


def setup(tmp_path: Path):
    root = tmp_path / "brain"
    root.mkdir()
    (root / "fkf.yaml").write_text(CONFIG)
    config = load_config(root)
    return config, Environment.from_config(config, inherited_path="/usr/bin")


def test_collect_returns_complete_event_and_index_documents_without_writing(tmp_path: Path) -> None:
    config, environment = setup(tmp_path)
    now = datetime(2026, 9, 6, 12, tzinfo=UTC)
    window = day_window(parse_day_in_location("2026-09-05", UTC))
    event_runner = FakeRunner(b'[{"id":"1","time":"2026-09-05T10:00:00Z","title":"One"}]')
    event = collect(event_runner, config.sources["events"], environment, window, parse_duration("1m"), now)
    assert event.uri() == "events/2026-09-05/events.json"
    assert event.count == 1
    assert not (tmp_path / event.uri()).exists()
    assert event_runner.calls[0].argv[-1] == "2026-09-06T00:00:00Z"

    index = collect(
        FakeRunner(b'[{"id":"a","title":"Snapshot"}]'),
        config.sources["snapshot"],
        environment,
        window,
        parse_duration("1m"),
        now,
    )
    assert index.uri() == "index/snapshot.json"
    assert not index.date
    assert not index.window_start


def test_collect_window_runs_once_and_emits_empty_requested_days(tmp_path: Path) -> None:
    config, environment = setup(tmp_path)
    range_window = day_window(parse_day_in_location("2026-09-04", UTC))
    # The command range covers both days even though the first helper supplies one-day bounds;
    # argv substitution is asserted separately, while bucketing owns the requested date list.
    runner = FakeRunner(b'[{"id":"2","time":"2026-09-05T12:00:00Z","title":"Two"}]')
    documents = collect_window(
        runner,
        config.sources["windowed"],
        environment,
        range_window,
        ["2026-09-04", "2026-09-05"],
        UTC,
        parse_duration("1m"),
        datetime(2026, 9, 6, tzinfo=UTC),
    )
    assert len(runner.calls) == 1
    assert documents["2026-09-04"].records == []
    assert documents["2026-09-05"].count == 1


@pytest.mark.parametrize(
    "output",
    [b"", b"{}", b'[{"id":"1","time":"outside","title":"One"}]', b'[{"id":"1","time":"2026-09-05T10:00:00Z"}]'],
)
def test_collect_wraps_every_incomplete_output_without_partial_state(tmp_path: Path, output: bytes) -> None:
    config, environment = setup(tmp_path)
    window = day_window(parse_day_in_location("2026-09-05", UTC))
    with pytest.raises(IncompleteCollectionError, match="source events"):
        collect(
            FakeRunner(output),
            config.sources["events"],
            environment,
            window,
            parse_duration("1m"),
            datetime(2026, 9, 6, tzinfo=UTC),
        )
