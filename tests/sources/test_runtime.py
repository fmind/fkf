"""Direct-argv construction, private retry evidence, and global pacing."""

from __future__ import annotations

import os
import stat
import threading
from collections.abc import Callable
from concurrent.futures import ThreadPoolExecutor
from dataclasses import replace
from pathlib import Path

import pytest

from fkf.config import BodyPolicy, Config, RetryPolicy, Source, SyncConfig
from fkf.errors import OperationalError
from fkf.fields import FieldMap, FieldSchema
from fkf.process import (
    Cancellation,
    Command,
    CommandCanceledError,
    CommandFailureError,
    CommandResult,
    CommandTimeoutError,
    Disclosure,
)
from fkf.source_runtime import (
    Environment,
    Pacer,
    PacingRunner,
    PolicyRunner,
    build_auth_command,
    build_body_command,
    build_run_command,
    build_test_command,
    describe_policy,
    ensure_bin_dir,
    normalize_github_noreply_actor,
)
from fkf.store import Layer, UnsafePathError
from fkf.timeutil import DurationNS, parse_duration


class Window:
    date = "2026-05-04"
    next = "2026-05-05"
    start = "2026-05-04T00:00:00Z"
    end = "2026-05-05T00:00:00Z"


class FakeRunner:
    def __init__(self, outcomes: list[CommandResult | BaseException]) -> None:
        self.outcomes = outcomes
        self.commands: list[Command] = []

    def run(self, command: Command, *, cancel: Cancellation | None = None) -> CommandResult:
        del cancel
        self.commands.append(command)
        outcome = self.outcomes.pop(0)
        if isinstance(outcome, BaseException):
            raise outcome
        return outcome


def config_at(root: Path, *bins: Path) -> Config:
    return Config(
        fkf=1,
        name="brain",
        schema=FieldSchema(),
        layers={Layer.EVENTS: True},
        identities={},
        sources={},
        sync=SyncConfig(),
        bin=tuple(os.fspath(path) for path in bins),
        path=root / "fkf.yaml",
    )


def source(**changes: object) -> Source:
    declared = Source(
        name="github-events",
        run=("provider", "{{date}}", "{{next_date}}", "{{start}}", "{{end}}", "{{base}}", "{{home}}"),
        test=("provider-check", "{{base}}", "{{home}}"),
        auth=("provider", "auth", "status"),
    )
    return replace(declared, **changes)


def test_environment_and_command_builders_share_the_process_path_boundary(tmp_path: Path) -> None:
    root = tmp_path / "brain"
    external = tmp_path / "tools"
    tests = root / "tests"
    for directory in (root / "bin", external, tests):
        directory.mkdir(parents=True, exist_ok=True)
    for path in (root / "bin" / "provider", external / "provider-check", tests / "provider-check"):
        path.write_text("#!/bin/sh\n", encoding="utf-8")
        path.chmod(0o700)

    environment = Environment.from_config(
        config_at(root, external),
        inherited_path=os.pathsep.join((os.fspath(root), os.fspath(external))),
    )
    declared = source(timeout=parse_duration("5s"))

    run = build_run_command(declared, environment, Window(), parse_duration("1m"))
    assert run.argv == (
        "provider",
        "2026-05-04",
        "2026-05-05",
        "2026-05-04T00:00:00Z",
        "2026-05-05T00:00:00Z",
        os.fspath(root),
        os.environ["HOME"],
    )
    assert run.timeout == parse_duration("5s")
    assert run.base == root
    assert run.configured_bin == (os.fspath(external),)
    assert run.disclosure is Disclosure.DECLARED
    assert run.diagnostic is not None
    assert run.diagnostic.source == "github-events"
    assert environment.look_path("provider") == root / "bin" / "provider"

    check = build_test_command(declared, environment, parse_duration("1m"))
    assert check.argv == ("provider-check", os.fspath(root), os.environ["HOME"])
    assert check.source_test is True
    assert check.disclosure is Disclosure.DECLARED
    assert environment.look_path("provider-check") == external / "provider-check"
    assert environment.look_test_path("provider-check") == tests / "provider-check"

    auth = build_auth_command(declared, environment, parse_duration("1m"))
    assert auth.argv == declared.auth
    assert auth.disclosure is Disclosure.QUIET_AUTH
    assert auth.diagnostic is None


@pytest.mark.parametrize("builder", [build_run_command, build_test_command])
def test_home_is_required_only_when_a_declared_argv_uses_it(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
    builder: Callable[..., Command],
) -> None:
    monkeypatch.setenv("HOME", "")
    environment = Environment.from_config(config_at(tmp_path))
    declared = source()
    args = (
        (declared, environment, Window(), parse_duration("1m"))
        if builder is build_run_command
        else (declared, environment, parse_duration("1m"))
    )

    with pytest.raises(OperationalError, match="HOME"):
        builder(*args)

    declared.run = ("provider",)
    assert build_run_command(declared, environment, Window(), parse_duration("1m")).argv == ("provider",)


def test_body_values_stay_one_opaque_argv_and_static_paths_cannot_be_shadowed(tmp_path: Path) -> None:
    root = tmp_path / "brain"
    root.mkdir()
    environment = Environment.from_config(config_at(root))
    fields = FieldMap.from_json_value({"id": ".id", "repo": ".repo", "base": ".provider_base"})
    declared = source(
        fields=fields,
        body=("provider", "view", "{{id}}", "--repo={{repo}}", "--base", "{{base}}"),
    )
    value = "révision {{base}} 42; $(not-a-shell) | 👍"

    command = build_body_command(
        declared,
        fields,
        environment,
        {"id": value, "repo": "fmind/fkf", "provider_base": "--help"},
        parse_duration("1m"),
    )

    assert command.argv == (
        "provider",
        "view",
        value,
        "--repo=fmind/fkf",
        "--base",
        os.fspath(root),
    )
    assert command.disclosure is Disclosure.OPAQUE_BODY
    assert command.diagnostic is None


@pytest.mark.parametrize("value", ["--help", "@response-file", "a\tb", "a\u200bb"])
def test_body_rejects_ambiguous_values_without_disclosing_them(tmp_path: Path, value: str) -> None:
    fields = FieldMap.from_json_value({"id": ".id"})
    declared = source(fields=fields, body=("provider", "{{id}}"))

    with pytest.raises(OperationalError, match="safe opaque argv") as caught:
        build_body_command(
            declared,
            fields,
            Environment.from_config(config_at(tmp_path)),
            {"id": value},
            parse_duration("1m"),
        )

    assert value not in str(caught.value)


def test_commands_revalidate_trust_at_the_last_pre_exec_boundary(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    calls: list[Config] = []

    def fake_trust_check(config: Config) -> Callable[[], None]:
        return lambda: calls.append(config)

    monkeypatch.setattr("fkf.source_runtime.trust_check", fake_trust_check)
    config = config_at(tmp_path)
    command = build_auth_command(source(), Environment.from_config(config), parse_duration("1m"))

    assert command.before_exec is not None
    command.before_exec()
    assert calls == [config]


def test_policy_runner_retries_only_private_declared_evidence_with_linear_backoff() -> None:
    private = b"private-rate-limit-marker"
    runner = FakeRunner(
        [
            CommandFailureError(9, private),
            CommandFailureError(7, private),
            CommandResult(b"[]"),
        ]
    )
    declared = source(
        retry=RetryPolicy(attempts=3, backoff=parse_duration("10ms"), on=("exit:7", "private-rate-limit-marker"))
    )
    waits: list[DurationNS] = []
    policy = PolicyRunner(runner, declared, sleep=lambda duration, _cancel: waits.append(duration))

    assert policy.run(Command(("provider",), parse_duration("1s"))).stdout == b"[]"
    assert policy.attempts == 3
    assert waits == [parse_duration("10ms"), parse_duration("20ms")]
    assert private.decode() not in str(CommandFailureError(7, private))


@pytest.mark.parametrize(
    "failure",
    [
        OperationalError("wrapper private-rate-limit-marker"),
        CommandTimeoutError("command timed out"),
        CommandCanceledError("command canceled"),
    ],
)
def test_policy_runner_never_retries_non_provider_or_terminal_failures(failure: BaseException) -> None:
    runner = FakeRunner([failure, CommandResult(b"should not run")])
    policy = PolicyRunner(
        runner,
        source(retry=RetryPolicy(attempts=2, on=("private-rate-limit-marker",))),
        sleep=lambda _duration, _cancel: None,
    )

    with pytest.raises(type(failure)):
        policy.run(Command(("provider",), parse_duration("1s")))
    assert len(runner.commands) == 1


def test_pacer_reserves_every_concurrent_attempt_on_one_monotonic_timeline() -> None:
    waits: list[DurationNS] = []
    wait_lock = threading.Lock()

    def sleep(duration: DurationNS, _cancel: Cancellation | None) -> None:
        with wait_lock:
            waits.append(duration)

    pacer = Pacer(now_ns=lambda: 1_000_000_000, sleep=sleep)
    declared = source(min_interval=parse_duration("10s"))
    runner = FakeRunner([CommandResult(b"ok") for _ in range(3)])
    paced = PacingRunner(runner, pacer, declared)
    command = Command(("provider",), parse_duration("1s"))

    with ThreadPoolExecutor(max_workers=3) as pool:
        results = list(pool.map(lambda _index: paced.run(command), range(3)))

    assert [result.stdout for result in results] == [b"ok", b"ok", b"ok"]
    assert sorted(waits) == [parse_duration("10s"), parse_duration("20s")]


def test_policy_description_and_github_noreply_normalization_are_deterministic() -> None:
    declared = source(
        max_age_hours=48,
        window=True,
        body=("provider", "{{id}}"),
        bodies=BodyPolicy.SYNC,
        retry=RetryPolicy(attempts=3, backoff=parse_duration("30s"), on=("exit:7",)),
        min_interval=parse_duration("5s"),
        timeout=parse_duration("2m"),
    )
    assert describe_policy(declared) == (
        "index max age 48h; window: one command for the whole requested range; bodies sync: read --body runs on "
        "a cache miss; sync also prefetches missing or provider-modified index entries, new event records, and every "
        "missing body in one newest event document after prune; retry 3 attempts on exit:7, backoff 30s; min interval "
        "5s; timeout 2m0s"
    )
    assert normalize_github_noreply_actor("123456+Fmind@users.noreply.github.com") == "actor:github.com/fmind"
    assert normalize_github_noreply_actor("fmind@users.noreply.github.com") == "actor:github.com/fmind"
    assert normalize_github_noreply_actor("123+bad_login@users.noreply.github.com") is None
    assert normalize_github_noreply_actor("fmind@example.com") is None


def test_ensure_bin_dir_refuses_a_symlink_leaf(tmp_path: Path) -> None:
    root = tmp_path / "brain"
    outside = tmp_path / "outside"
    root.mkdir()
    outside.mkdir()
    (root / "bin").symlink_to(outside, target_is_directory=True)

    with pytest.raises(UnsafePathError):
        ensure_bin_dir(root)


def test_ensure_bin_dir_creates_a_private_helper_directory(tmp_path: Path) -> None:
    root = tmp_path / "brain"
    root.mkdir()

    directory = ensure_bin_dir(root)

    assert directory == root / "bin"
    assert directory.is_dir()
    assert stat.S_IMODE(directory.stat().st_mode) == 0o700
