from __future__ import annotations

import threading
from collections import defaultdict
from collections.abc import Callable
from dataclasses import dataclass, field, replace
from datetime import UTC, datetime
from pathlib import Path
from zoneinfo import ZoneInfo

import pytest

import fkf.sync as sync_module
from fkf.base import Base
from fkf.bodies import load_body_manifest, prune_bodies, read_cached_body
from fkf.collection import collect
from fkf.config import RetryPolicy, load_config
from fkf.documents import day_window, event_document_uri, parse_day_in_location
from fkf.errors import CanceledError, UntrustedError
from fkf.process import Cancellation, Command, CommandFailureError, CommandResult
from fkf.source_runtime import Environment
from fkf.sync import (
    DerivedRebuildError,
    RebuildHooks,
    SyncOutcome,
    SyncRequest,
    contiguous_day_spans,
    plan_days,
    preflight_sync,
    previous_completed_days,
    resolve_targets,
    sync,
)
from fkf.trust import write_trust

CONFIG = """\
fkf: 1
name: brain
schema:
  id: {description: Stable identity., cardinality: one}
  time: {description: Event time., cardinality: one}
  title: {description: Meaningful title., cardinality: optional}
  topic: {description: Topic., cardinality: optional}
layers: {events: true, index: true, tasks: false, projects: false, wiki: true}
sources:
  daily:
    enabled: true
    run: [provider, daily, "{{date}}"]
    fields: {id: .id, time: .time, title: .title, topic: .topic}
  range:
    enabled: true
    window: true
    run: [provider, range, "{{start}}", "{{end}}"]
    fields: {id: .id, time: .time, title: .title}
  snapshot:
    enabled: true
    layer: index
    max_age_hours: 12
    auth: [provider, auth]
    run: [provider, snapshot]
    fields: {id: .id, title: .title, topic: .topic}
  reader:
    enabled: true
    run: [provider, reader, "{{base}}", "{{date}}"]
    fields: {id: .id, time: .time, title: .title}
  disabled:
    run: [provider, disabled, "{{date}}"]
    fields: {id: .id, time: .time, title: .title}
sync: {days: 2, index_max_age_hours: 168, timeout: 2s, concurrency: 4}
"""


def event_record(day: str, identity: str = "1") -> bytes:
    return f'{{"id":"{identity}","time":"{day}T12:00:00Z","title":"Title {identity}"}}'.encode()


@dataclass
class FakeRunner:
    callback: Callable[[Command], bytes | BaseException]
    commands: list[Command] = field(default_factory=list)
    active: int = 0
    maximum_active: int = 0
    lock: threading.Lock = field(default_factory=threading.Lock)

    def run(self, command: Command, *, cancel: Cancellation | None = None) -> CommandResult:
        del cancel
        with self.lock:
            self.commands.append(command)
            self.active += 1
            self.maximum_active = max(self.maximum_active, self.active)
        try:
            callback = self.callback
            assert callable(callback)
            result = callback(command)
            if isinstance(result, BaseException):
                raise result
            return CommandResult(result)
        finally:
            with self.lock:
                self.active -= 1


def make_base(
    tmp_path: Path,
    callback: Callable[[Command], bytes | BaseException],
    *,
    now: datetime | None = None,
    config_text: str = CONFIG,
) -> Base:
    root = tmp_path / "brain"
    root.mkdir(parents=True)
    (root / "fkf.yaml").write_text(config_text)
    config = load_config(root)
    return Base(
        config=config,
        store=config.store(),
        environment=Environment.from_config(config, inherited_path="/usr/bin"),
        runner=FakeRunner(callback),
        now=lambda: now or datetime(2026, 9, 6, 12, tzinfo=UTC),
    )


BODY_CONFIG = CONFIG.replace(
    "    fields: {id: .id, time: .time, title: .title, topic: .topic}\n  range:",
    "    fields: {id: .id, time: .time, title: .title, topic: .topic}\n"
    '    body: [provider, body, "{{id}}"]\n'
    "    bodies: sync\n"
    "  range:",
)


def trust_base(base: Base) -> None:
    write_trust(base.config, base.now())


def test_target_and_completed_day_planning_are_strict(tmp_path: Path) -> None:
    base = make_base(tmp_path, lambda _command: b"[]")
    assert [source.name for source in resolve_targets(base, (), allow_disabled=False)] == [
        "daily",
        "range",
        "reader",
        "snapshot",
    ]
    with pytest.raises(ValueError, match="duplicate source"):
        resolve_targets(base, ("daily", "daily"), allow_disabled=False)
    with pytest.raises(ValueError, match="is disabled"):
        resolve_targets(base, ("disabled",), allow_disabled=False)
    assert [source.name for source in resolve_targets(base, ("disabled",), allow_disabled=True)] == ["disabled"]

    days, window = plan_days(base, SyncRequest(days=2))
    assert [day.date().isoformat() for day in days] == ["2026-09-04", "2026-09-05"]
    assert (window.since, window.until) == ("2026-09-04", "2026-09-05")
    with pytest.raises(ValueError, match="today or later"):
        plan_days(base, SyncRequest(date="2026-09-06"))

    apia = ZoneInfo("Pacific/Apia")
    completed = previous_completed_days(datetime(2012, 1, 1, 12, tzinfo=apia), 2)
    assert [day.date().isoformat() for day in completed] == ["2011-12-29", "2011-12-31"]
    assert contiguous_day_spans(("2011-12-29", "2011-12-31"), apia) == (("2011-12-29", "2011-12-31"),)


def test_sync_collects_missing_units_skips_existing_and_rebuilds_in_order(tmp_path: Path) -> None:
    calls: list[str] = []

    def provider(command: Command) -> bytes:
        action = command.argv[1]
        if action == "auth":
            return b""
        if action == "snapshot":
            return b'[{"id":"s1","title":"Snapshot","topic":"sync"}]'
        day = command.argv[-1]
        if action == "reader":
            assert (base.root / event_document_uri(day, "daily")).exists()
        return b"[" + event_record(day, action) + b"]"

    base = make_base(tmp_path, provider)
    trust_base(base)
    report = sync(
        base,
        SyncRequest(targets=("reader", "snapshot", "daily"), date="2026-09-05"),
        rebuild=RebuildHooks(
            wiki=lambda _base: calls.append("wiki") or "wiki",
            graph=lambda _base: calls.append("graph") or "graph",
            lexical=lambda _base: calls.append("lexical") or "lexical",
        ),
    )

    # Index units have no date and therefore sort before dated units, matching the Go report.
    assert [unit.source for unit in report.units] == ["snapshot", "daily", "reader"]
    assert all(unit.outcome is SyncOutcome.WRITTEN for unit in report.units)
    assert (report.written, report.records, report.failed, report.complete) == (3, 3, 0, True)
    assert calls == ["wiki", "graph", "lexical"]
    assert (report.wiki, report.graph, report.index) == ("wiki", "graph", "lexical")

    second = sync(base, SyncRequest(targets=("reader", "snapshot", "daily"), date="2026-09-05"))
    assert [unit.outcome for unit in second.units] == [
        SyncOutcome.SKIPPED_FRESH,
        SyncOutcome.SKIPPED_EXISTING,
        SyncOutcome.SKIPPED_EXISTING,
    ]
    assert isinstance(base.runner, FakeRunner)
    assert len(base.runner.commands) == 4  # three collectors plus the one auth probe


def test_window_source_splits_missing_holes_and_validates_before_writing(tmp_path: Path) -> None:
    ranges: list[tuple[str, str]] = []

    def provider(command: Command) -> bytes:
        ranges.append((command.argv[-2], command.argv[-1]))
        start = command.argv[-2][:10]
        return b"[" + event_record(start, start[-2:]) + b"]"

    base = make_base(tmp_path, provider)
    trust_base(base)
    base.write_document(
        # Seed the middle day through the same validation and storage boundary sync uses.
        collect(
            FakeRunner(lambda _command: b"[" + event_record("2026-09-04", "seed") + b"]"),
            base.config.sources["daily"],
            base.environment,
            day_window(parse_day_in_location("2026-09-04", UTC)),
            base.config.sync.timeout,
            base.now(),
        )
    )
    seeded = base.read_document("events/2026-09-04/daily.json")
    seeded.source = "range"
    base.write_document(seeded)

    report = sync(base, SyncRequest(targets=("range",), days=3))
    assert ranges == [
        ("2026-09-03T00:00:00Z", "2026-09-04T00:00:00Z"),
        ("2026-09-05T00:00:00Z", "2026-09-06T00:00:00Z"),
    ]
    assert [unit.outcome for unit in report.units] == [
        SyncOutcome.WRITTEN,
        SyncOutcome.SKIPPED_EXISTING,
        SyncOutcome.WRITTEN,
    ]

    broken = make_base(tmp_path / "broken", lambda _command: b"[" + event_record("2026-09-03") + b',{"bad":true}]')
    trust_base(broken)
    failed = sync(broken, SyncRequest(targets=("range",), days=2))
    assert failed.failed == 2
    assert not (broken.root / "events").exists()


def test_dry_run_auth_retry_preview_preflight_and_derived_failure_are_safe(tmp_path: Path) -> None:
    attempts: defaultdict[str, int] = defaultdict(int)

    def provider(command: Command) -> bytes | BaseException:
        action = command.argv[1]
        attempts[action] += 1
        if action == "auth":
            return CommandFailureError(1, b"private-account-name")
        if action == "daily" and attempts[action] == 1:
            return CommandFailureError(7, b"private-rate-marker")
        if action == "daily":
            return b"[" + event_record(command.argv[-1]) + b"]"
        raise AssertionError(action)

    base = make_base(tmp_path, provider)
    base.config.sources["daily"] = replace(base.config.sources["daily"], retry=RetryPolicy(attempts=2, on=("exit:7",)))

    dry = sync(base, SyncRequest(targets=("disabled",), date="2026-09-05", dry_run=True))
    assert dry.units[0].outcome is SyncOutcome.PLANNED
    assert attempts == {}

    trust_base(base)

    auth = sync(base, SyncRequest(targets=("snapshot",), date="2026-09-05"))
    assert auth.auth_required == ("snapshot",)
    assert auth.complete is True
    assert "private-account-name" not in auth.failure_summary()

    due = preflight_sync(base, SyncRequest(targets=("daily",), date="2026-09-05"))
    assert due.due
    assert due.due_sources == ("daily",)
    report = sync(base, SyncRequest(targets=("daily",), date="2026-09-05"))
    assert report.units[0].attempts == 2
    assert attempts["daily"] == 2

    preview = sync(base, SyncRequest(targets=("daily",), date="2026-09-05", preview=True))
    assert preview.preview is not None
    assert preview.preview.sample[0].title == "Title 1"
    assert preview.written == 0

    with pytest.raises(DerivedRebuildError, match="documents are complete") as caught:
        sync(
            base,
            SyncRequest(targets=("daily",), date="2026-09-04"),
            rebuild=RebuildHooks(wiki=lambda _base: (_ for _ in ()).throw(RuntimeError("broken cache"))),
        )
    assert caught.value.report.written == 1
    assert (base.root / "events/2026-09-04/daily.json").exists()


def test_sync_refuses_untrusted_execution_before_runner_or_writes(tmp_path: Path) -> None:
    base = make_base(tmp_path, lambda command: b"[" + event_record(command.argv[-1]) + b"]")
    assert isinstance(base.runner, FakeRunner)

    for request in (
        SyncRequest(targets=("daily",), date="2026-09-05"),
        SyncRequest(targets=("daily",), date="2026-09-05", preview=True),
    ):
        with pytest.raises(UntrustedError, match="has never been trusted"):
            sync(base, request)

    assert base.runner.commands == []
    assert not (base.root / "events").exists()

    dry = sync(base, SyncRequest(targets=("disabled",), date="2026-09-05", dry_run=True))
    assert dry.units[0].outcome is SyncOutcome.PLANNED
    assert base.runner.commands == []


def test_sync_preserves_base_cancellation_from_rebuild_hooks(tmp_path: Path) -> None:
    base = make_base(tmp_path, lambda command: b"[" + event_record(command.argv[-1]) + b"]")
    trust_base(base)

    def canceled_rebuild(_base: Base) -> object:
        raise CanceledError("operation canceled")

    with pytest.raises(CanceledError, match="operation canceled"):
        sync(
            base,
            SyncRequest(targets=("daily",), date="2026-09-05"),
            rebuild=RebuildHooks(wiki=canceled_rebuild),
        )
    assert (base.root / "events/2026-09-05/daily.json").exists()


def test_sync_forwards_trust_event_and_stops_between_rebuild_hooks(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    base = make_base(tmp_path, lambda command: b"[" + event_record(command.argv[-1]) + b"]")
    cancel = threading.Event()
    trusted: list[object] = []
    calls: list[str] = []

    def require_trust(_config: object, *, cancel: object) -> None:
        assert cancel is cancel_event
        trusted.append(cancel)

    def wiki(_base: Base) -> object:
        calls.append("wiki")
        cancel_event.set()
        return "wiki"

    def unexpected(_base: Base) -> object:
        pytest.fail("cancellation must stop the remaining rebuild hooks")

    cancel_event = cancel
    monkeypatch.setattr(sync_module, "require_trust", require_trust)

    with pytest.raises(CanceledError) as caught:
        sync(
            base,
            SyncRequest(targets=("daily",), date="2026-09-05"),
            rebuild=RebuildHooks(wiki=wiki, graph=unexpected, lexical=unexpected),
            cancel=cancel,
        )

    assert caught.value.exit_code == 130
    assert trusted == [cancel]
    assert calls == ["wiki"]
    assert (base.root / "events/2026-09-05/daily.json").exists()


def test_sync_body_policy_prefetches_and_repairs_a_pruned_event_cache_once(tmp_path: Path) -> None:
    def provider(command: Command) -> bytes:
        if command.argv[1] == "body":
            return f"body {command.argv[2]}".encode()
        return b"[" + event_record(command.argv[-1]) + b"]"

    base = make_base(tmp_path, provider, config_text=BODY_CONFIG)
    trust_base(base)
    calls: list[str] = []
    hooks = RebuildHooks(
        wiki=lambda _base: calls.append("wiki") or "wiki",
        graph=lambda _base: calls.append("graph") or "graph",
        lexical=lambda _base: calls.append("lexical") or "lexical",
    )
    first = sync(base, SyncRequest(targets=("daily",), date="2026-09-05"), rebuild=hooks)
    assert (first.written, first.bodies_cached, first.body_failed, first.complete) == (1, 1, 0, True)
    assert calls == ["wiki", "graph", "lexical"]
    uri = event_document_uri("2026-09-05", "daily") + "#1"
    assert read_cached_body(base, uri)[0] == "body 1"

    prune_bodies(base)
    assert preflight_sync(base, SyncRequest(targets=("daily",), date="2026-09-05", if_due=True)).due
    body_calls: list[tuple[str, ...]] = []

    def body_only(command: Command) -> bytes:
        body_calls.append(command.argv)
        assert command.argv[1] == "body"
        return b"restored"

    base.runner = FakeRunner(body_only)
    calls.clear()
    restored = sync(
        base,
        SyncRequest(targets=("daily",), date="2026-09-05", if_due=True),
        rebuild=hooks,
    )
    assert (restored.written, restored.skipped, restored.bodies_cached, restored.body_failed) == (0, 1, 1, 0)
    assert restored.graph is None
    assert restored.index == "lexical"
    assert calls == ["lexical"]
    assert body_calls == [("provider", "body", "1")]
    assert not preflight_sync(base, SyncRequest(targets=("daily",), date="2026-09-05", if_due=True)).due


def test_sync_body_policy_bounds_a_failed_event_restore_to_one_retry(tmp_path: Path) -> None:
    body_calls = 0

    def provider(command: Command) -> bytes | BaseException:
        nonlocal body_calls
        if command.argv[1] == "body":
            body_calls += 1
            return CommandFailureError(7, b"private provider detail")
        return b"[" + event_record(command.argv[-1]) + b"]"

    base = make_base(tmp_path, provider, config_text=BODY_CONFIG)
    trust_base(base)
    request = SyncRequest(targets=("daily",), date="2026-09-05", no_graph=True)
    first = sync(base, request)
    assert (first.written, first.body_failed, first.complete, body_calls) == (1, 1, False, 1)
    assert "private provider detail" not in first.failure_summary()
    assert preflight_sync(base, replace(request, if_due=True)).due

    second = sync(base, request)
    assert (second.written, second.skipped, second.body_failed, second.complete, body_calls) == (0, 1, 1, False, 2)
    assert not preflight_sync(base, replace(request, if_due=True)).due


def test_sync_body_policy_restores_only_the_newest_selected_event_document(tmp_path: Path) -> None:
    def provider(command: Command) -> bytes:
        if command.argv[1] == "body":
            return f"body {command.argv[2]}".encode()
        day = command.argv[-1]
        return b"[" + event_record(day, day) + b"]"

    base = make_base(tmp_path, provider, config_text=BODY_CONFIG)
    trust_base(base)
    first = sync(base, SyncRequest(targets=("daily",), days=2, no_graph=True))
    assert (first.written, first.bodies_cached) == (2, 2)
    prune_bodies(base)

    calls: list[tuple[str, ...]] = []

    def body_only(command: Command) -> bytes:
        calls.append(command.argv)
        return b"restored"

    base.runner = FakeRunner(body_only)
    restored = sync(base, SyncRequest(targets=("daily",), days=2, no_graph=True, if_due=True))
    assert (restored.skipped, restored.bodies_cached, restored.body_failed) == (2, 1, 0)
    assert calls == [("provider", "body", "2026-09-05")]
    manifest = load_body_manifest(base)
    assert manifest.event_attempts == {"daily": True}
    assert tuple(manifest.entries) == (event_document_uri("2026-09-05", "daily") + "#2026-09-05",)


def test_sync_body_policy_repairs_each_current_index_record(tmp_path: Path) -> None:
    index_config = BODY_CONFIG.replace(
        "  daily:\n    enabled: true\n",
        "  daily:\n    enabled: true\n    layer: index\n",
        1,
    )

    def provider(command: Command) -> bytes:
        if command.argv[1] == "body":
            return f"body {command.argv[2]}".encode()
        return (
            b'[{"id":"one","time":"2026-09-05T10:00:00Z","title":"One"},'
            b'{"id":"two","time":"2026-09-05T11:00:00Z","title":"Two"}]'
        )

    base = make_base(tmp_path, provider, config_text=index_config)
    trust_base(base)
    request = SyncRequest(targets=("daily",), days=1, no_graph=True)
    first = sync(base, request)
    assert (first.written, first.bodies_cached) == (1, 2)
    prune_bodies(base)
    assert preflight_sync(base, replace(request, if_due=True)).due

    calls: list[tuple[str, ...]] = []

    def body_only(command: Command) -> bytes:
        calls.append(command.argv)
        return b"current"

    base.runner = FakeRunner(body_only)
    restored = sync(base, replace(request, if_due=True))
    assert (restored.written, restored.skipped, restored.bodies_cached, restored.body_failed) == (0, 1, 2, 0)
    assert calls == [("provider", "body", "one"), ("provider", "body", "two")]
