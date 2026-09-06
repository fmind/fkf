from __future__ import annotations

from datetime import UTC, datetime, timedelta
from pathlib import Path
from threading import Event

import pytest

from fkf.base import Base
from fkf.config import Config, ConfigError, Source, SyncConfig
from fkf.fields import FieldSchema
from fkf.process import Command, CommandFailureError, CommandResult
from fkf.source_runtime import Environment
from fkf.source_tests import SourceTestOutcome, SourceTestRequest, run_source_tests
from fkf.store import Layer, Store


class _Clock:
    def __init__(self) -> None:
        self.value = datetime(2026, 9, 6, tzinfo=UTC)

    def __call__(self) -> datetime:
        result = self.value
        self.value += timedelta(milliseconds=1)
        return result


class _Runner:
    def __init__(self, fail: bool = False) -> None:
        self.fail = fail
        self.calls: list[Command] = []
        self.cancels: list[object] = []

    def run(self, command: Command, *, cancel: object = None) -> CommandResult:
        self.calls.append(command)
        self.cancels.append(cancel)
        if self.fail:
            raise CommandFailureError(7, b"private")
        return CommandResult(b"")


def _base(tmp_path: Path, runner: _Runner) -> Base:
    config_path = tmp_path / "fkf.yaml"
    config_path.write_text("name: test\n")
    sources = {
        "active": Source("active", enabled=True, test=("check.sh", "{{base}}/fixture")),
        "dormant": Source("dormant", enabled=False, test=("other.sh",)),
        "untested": Source("untested", enabled=True),
    }
    config = Config(1, "test", FieldSchema(), dict.fromkeys(Layer, True), {}, sources, SyncConfig(), (), config_path)
    environment = Environment(tmp_path, environment={"PATH": "/usr/bin"}, config=None)
    return Base(config, Store(tmp_path, config.layers), environment, runner, _Clock())


def test_default_selects_only_enabled_hooks(monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> None:
    runner = _Runner()
    base = _base(tmp_path, runner)
    monkeypatch.setattr("fkf.source_tests.require_trust", lambda _config, **_kwargs: None)
    report = run_source_tests(base)
    assert (report.passed, report.failed, report.complete, report.elapsed) == (1, 0, True, "3ms")
    assert report.sources[0].source == "active"
    assert report.sources[0].outcome is SourceTestOutcome.PASSED
    assert runner.calls[0].argv == ("check.sh", f"{tmp_path}/fixture")


def test_all_continues_after_safe_failures(monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> None:
    base = _base(tmp_path, _Runner(fail=True))
    monkeypatch.setattr("fkf.source_tests.require_trust", lambda _config, **_kwargs: None)
    report = run_source_tests(base, SourceTestRequest(all=True))
    assert (report.passed, report.failed, report.complete) == (0, 2, False)
    assert "active: command exited with status 7" in report.failure_summary()
    assert "private" not in report.failure_summary()


def test_named_disabled_hook_runs_and_invalid_selections_fail(monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> None:
    base = _base(tmp_path, _Runner())
    monkeypatch.setattr("fkf.source_tests.require_trust", lambda _config, **_kwargs: None)
    assert run_source_tests(base, SourceTestRequest(("dormant",))).sources[0].source == "dormant"
    with pytest.raises(ConfigError, match="--all cannot"):
        run_source_tests(base, SourceTestRequest(("active",), all=True))
    with pytest.raises(ConfigError, match="declares no test"):
        run_source_tests(base, SourceTestRequest(("untested",)))


def test_cancellation_is_forwarded_to_each_hook(monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> None:
    runner = _Runner()
    base = _base(tmp_path, runner)
    cancel = Event()
    trusted: list[object] = []

    def require_trust(_config: object, *, cancel: object) -> None:
        trusted.append(cancel)

    monkeypatch.setattr("fkf.source_tests.require_trust", require_trust)

    run_source_tests(base, cancel=cancel)

    assert trusted == [cancel]
    assert runner.cancels == [cancel]
