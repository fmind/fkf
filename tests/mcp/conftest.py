from __future__ import annotations

from datetime import UTC, datetime
from pathlib import Path

import pytest

from fkf.base import Base
from fkf.config import load_config
from fkf.documents import Record, build_document, day_window, parse_day_in_location
from fkf.graph import build_graph
from fkf.process import Command, CommandResult
from fkf.source_runtime import Environment

_CONFIG = """\
fkf: 1
name: brain
schema:
  id: {description: Stable identity., cardinality: one}
  time: {description: Event time., cardinality: one}
  title: {description: Meaningful title., cardinality: optional}
  repo: {description: Repository., cardinality: optional, relation: true}
layers: {events: true, index: true, tasks: true, projects: true, wiki: true}
sources:
  synthetic:
    enabled: true
    run: [provider]
    fields: {id: .id, time: .time, title: .title, repo: .repo}
    body: [provider, body, "{{id}}"]
"""


class _OfflineRunner:
    calls = 0

    def run(self, command: Command, *, cancel: object | None = None) -> CommandResult:
        del command, cancel
        self.calls += 1
        raise AssertionError("an MCP stored read executed a provider command")


@pytest.fixture
def base(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> Base:
    home = tmp_path / "home"
    home.mkdir(exist_ok=True)
    monkeypatch.setenv("HOME", str(home))
    monkeypatch.setenv("XDG_STATE_HOME", str(home / ".local" / "state"))
    monkeypatch.delenv("FKF_BASE", raising=False)
    root = tmp_path / "brain"
    root.mkdir()
    (root / "fkf.yaml").write_text(_CONFIG, encoding="utf-8")
    config = load_config(root)
    return Base(
        config=config,
        store=config.store(),
        environment=Environment.from_config(config, inherited_path="/usr/bin:/bin"),
        runner=_OfflineRunner(),
        now=lambda: datetime(2026, 9, 6, 12, tzinfo=UTC),
        origin="flag",
    )


def _write_event(base: Base, value: str, records: list[Record]) -> None:
    source = base.source("synthetic")
    base.write_document(
        build_document(
            source,
            records,
            window=day_window(parse_day_in_location(value, UTC)),
            collected_at=base.now(),
        )
    )


def _write_page(base: Base, uri: str, body: str) -> None:
    path = base.root / uri
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(body, encoding="utf-8")


@pytest.fixture
def populated_base(base: Base) -> Base:
    _write_event(
        base,
        "2026-09-05",
        [
            {
                "id": "a1",
                "time": "2026-09-05T09:00:00Z",
                "title": "Needle record",
                "repo": "repo:github.com/fmind/fkf",
            }
        ],
    )
    _write_page(base, "wiki/index.md", "# Wiki\n\n- [Needle decision](needle.md)\n")
    _write_page(
        base,
        "wiki/needle.md",
        "---\ntitle: Needle decision\ntype: decision\ntags: [architecture]\n---\n\n# Needle decision\n\nDurable body.\n",
    )
    _write_page(
        base,
        "projects/fkf.md",
        "---\ntitle: FKF\ntype: project\nstatus: active\ntags: [python]\n---\n\n# FKF\n\n[Decision](../wiki/needle.md)\n",
    )
    build_graph(base)
    return base
