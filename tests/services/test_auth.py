from __future__ import annotations

from dataclasses import replace
from pathlib import Path
from threading import Event

import pytest

from fkf.auth import probe_source_auth
from fkf.base import Base
from fkf.config import Config, Source, SyncConfig
from fkf.fields import FieldSchema
from fkf.process import Command, CommandFailureError, CommandResult
from fkf.source_runtime import Environment
from fkf.store import Layer, Store


class _Runner:
    def __init__(self, failures: set[tuple[str, ...]] | None = None) -> None:
        self.failures = set() if failures is None else failures
        self.calls: list[Command] = []
        self.cancellations: list[object] = []

    def run(self, command: Command, *, cancel: object = None) -> CommandResult:
        self.calls.append(command)
        self.cancellations.append(cancel)
        if command.argv in self.failures:
            raise CommandFailureError(1, b"private account detail")
        return CommandResult(b"")


def _base(tmp_path: Path, runner: _Runner) -> Base:
    config_path = tmp_path / "fkf.yaml"
    config_path.write_text("name: test\n")
    sources = {
        "a": Source("a", enabled=True, auth=("provider", "ready")),
        "b": Source("b", enabled=True, auth=("provider", "ready")),
        "c": Source("c", enabled=True, auth=("other", "ready")),
    }
    config = Config(1, "test", FieldSchema(), dict.fromkeys(Layer, True), {}, sources, SyncConfig(), (), config_path)
    environment = Environment(tmp_path, environment={"PATH": "/usr/bin"}, config=None)
    return Base(config, Store(tmp_path, config.layers), environment, runner)


def test_offline_auth_probe_executes_nothing(tmp_path: Path) -> None:
    runner = _Runner()
    base = _base(tmp_path, runner)
    assert probe_source_auth(base, base.config.enabled_sources(), live=False) == ()
    assert runner.calls == []


def test_live_auth_probe_deduplicates_identical_argv(monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> None:
    runner = _Runner({("provider", "ready")})
    base = _base(tmp_path, runner)
    monkeypatch.setattr("fkf.auth.require_trust", lambda _config, **_kwargs: None)
    assert probe_source_auth(base, base.config.enabled_sources(), live=True) == ("a", "b")
    assert [call.argv for call in runner.calls] == [("provider", "ready"), ("other", "ready")]


def test_live_auth_probe_passes_cancellation_to_trust_and_provider_runner(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    runner = _Runner()
    base = _base(tmp_path, runner)
    cancel = Event()
    observed: list[object] = []

    def trusted(_config: object, *, cancel: object) -> None:
        observed.append(cancel)

    monkeypatch.setattr("fkf.auth.require_trust", trusted)

    assert probe_source_auth(base, base.config.enabled_sources(), live=True, cancel=cancel) == ()
    assert observed == [cancel]
    assert runner.cancellations == [cancel, cancel]


def test_probe_without_auth_is_ignored(monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> None:
    runner = _Runner()
    base = _base(tmp_path, runner)
    base.config.sources["a"] = replace(base.config.sources["a"], auth=())
    base.config.sources["b"] = replace(base.config.sources["b"], auth=())
    base.config.sources["c"] = replace(base.config.sources["c"], auth=())
    monkeypatch.setattr("fkf.auth.require_trust", lambda _config: pytest.fail("trust should not run"))
    assert probe_source_auth(base, base.config.enabled_sources(), live=True) == ()
