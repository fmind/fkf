"""Direct, private, and bounded subprocess execution."""

from __future__ import annotations

import logging
import os
import signal
import threading
import time
from collections.abc import Callable
from pathlib import Path

import pytest

from fkf.process import (
    MAX_COMMAND_OUTPUT_BYTES,
    Command,
    CommandCanceledError,
    CommandFailureError,
    CommandOutputTooLargeError,
    CommandTimeoutError,
    DeclaredCommandDiagnostic,
    Disclosure,
    SubprocessRunner,
    command_environment,
    command_path,
    resolve_executable,
    sanitize_path,
)
from fkf.timeutil import DurationNS

SECOND = DurationNS(1_000_000_000)


def _command(
    *argv: str,
    timeout: DurationNS = SECOND,
    stdin: bytes | None = None,
    environment: dict[str, str] | None = None,
    base: Path | None = None,
    configured_bin: tuple[str, ...] = (),
    source_test: bool = False,
    disclosure: Disclosure = Disclosure.OPAQUE_BODY,
    diagnostic: DeclaredCommandDiagnostic | None = None,
    max_output_bytes: int = MAX_COMMAND_OUTPUT_BYTES,
    before_exec: Callable[[], None] | None = None,
) -> Command:
    return Command(
        argv=tuple(argv),
        timeout=timeout,
        stdin=stdin,
        environment={} if environment is None else environment,
        base=base,
        configured_bin=configured_bin,
        source_test=source_test,
        disclosure=disclosure,
        diagnostic=diagnostic,
        max_output_bytes=max_output_bytes,
        before_exec=before_exec,
    )


def _executable(path: Path, body: str) -> None:
    path.write_text(body)
    path.chmod(0o700)


def test_runner_passes_arguments_directly_and_uses_neutral_cwd(tmp_path: Path) -> None:
    helper = tmp_path / "arguments"
    _executable(helper, '#!/bin/sh\nprintf \'%s|%s\' "$PWD" "$1"\n')
    argument = "space ; $(printf injected) | wildcard*"

    result = SubprocessRunner().run(_command(os.fspath(helper), argument))

    assert result.stdout == f"/|{argument}".encode()
    assert result.returncode == 0


def test_runner_feeds_bytes_to_stdin(tmp_path: Path) -> None:
    helper = tmp_path / "copy"
    _executable(helper, "#!/bin/sh\ncat\n")
    payload = b"abc\x00binary\n"

    result = SubprocessRunner().run(_command(os.fspath(helper), stdin=payload))

    assert result.stdout == payload


def test_environment_is_copied_and_removes_runtime_startup_state(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    base = tmp_path / "base"
    outside = tmp_path / "outside"
    base.mkdir()
    outside.mkdir()
    monkeypatch.setenv("PROVIDER_PROFILE", "kept")
    monkeypatch.setenv("BASH_ENV", "payload")
    monkeypatch.setenv("PYTHONPATH", "wiki")
    monkeypatch.setenv("PYTHONWARNINGS", "ignore::antigravity.X")
    monkeypatch.setenv("DYLD_INSERT_LIBRARIES", "payload")
    monkeypatch.setenv("LUA_INIT_5_4", "payload")
    monkeypatch.setenv("XDG_CONFIG_HOME", os.fspath(base / "wiki"))
    monkeypatch.setenv("XDG_CACHE_HOME", os.fspath(outside))
    overrides = {"EXPLICIT": "value"}

    environment = command_environment(_command("provider", environment=overrides, base=base))

    assert environment["PROVIDER_PROFILE"] == "kept"
    assert environment["EXPLICIT"] == "value"
    assert environment["XDG_CACHE_HOME"] == os.fspath(outside)
    assert "XDG_CONFIG_HOME" not in environment
    assert all(
        key not in environment
        for key in ("BASH_ENV", "PYTHONPATH", "PYTHONWARNINGS", "DYLD_INSERT_LIBRARIES", "LUA_INIT_5_4")
    )
    assert overrides == {"EXPLICIT": "value"}


def test_environment_removes_relative_and_symlinked_base_config_roots(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    base = tmp_path / "base"
    base.mkdir()
    alias = tmp_path / "alias"
    alias.symlink_to(base, target_is_directory=True)

    monkeypatch.setenv("HOME", "relative")
    monkeypatch.setenv("XDG_CONFIG_HOME", os.fspath(alias / "config"))
    environment = command_environment(_command("provider", base=base))

    assert "HOME" not in environment
    assert "XDG_CONFIG_HOME" not in environment


def test_path_policy_admits_only_trusted_base_trees_and_external_absolute_dirs(tmp_path: Path) -> None:
    base = tmp_path / "base"
    external = tmp_path / "external"
    base.mkdir()
    external.mkdir()
    alias = tmp_path / "alias"
    alias.symlink_to(base, target_is_directory=True)
    inherited = os.pathsep.join(("", ".", "relative", os.fspath(base), os.fspath(alias), os.fspath(external)))

    ordinary = command_path(base=base, inherited=inherited)
    source_test = command_path(base=base, inherited=inherited, source_test=True)

    assert ordinary.split(os.pathsep) == [os.fspath(base / "bin"), os.fspath(external)]
    assert source_test.split(os.pathsep) == [os.fspath(base / "tests"), os.fspath(base / "bin"), os.fspath(external)]
    assert sanitize_path(inherited, base) == os.fspath(external)


def test_source_tests_can_shadow_base_bin_but_ordinary_commands_cannot(tmp_path: Path) -> None:
    base = tmp_path / "base"
    bin_directory = base / "bin"
    tests_directory = base / "tests"
    bin_directory.mkdir(parents=True)
    tests_directory.mkdir()
    _executable(bin_directory / "helper", "#!/bin/sh\nprintf bin\n")
    _executable(tests_directory / "helper", "#!/bin/sh\nprintf tests\n")
    runner = SubprocessRunner()

    ordinary = runner.run(_command("helper", base=base, environment={"PATH": ""}))
    source_test = runner.run(_command("helper", base=base, environment={"PATH": ""}, source_test=True))

    assert ordinary.stdout == b"bin"
    assert source_test.stdout == b"tests"


def test_resolver_rejects_relative_paths_missing_commands_and_non_executables(tmp_path: Path) -> None:
    non_executable = tmp_path / "helper"
    non_executable.write_text("text")

    with pytest.raises(ValueError, match="relative executable"):
        resolve_executable("relative/helper", os.fspath(tmp_path))
    with pytest.raises(FileNotFoundError, match="not found on PATH"):
        resolve_executable("missing", os.fspath(tmp_path))
    with pytest.raises(FileNotFoundError, match="not found on PATH"):
        resolve_executable("helper", os.fspath(tmp_path))


@pytest.mark.parametrize("stream", ["stdout", "stderr"])
def test_runner_bounds_each_output_stream_independently(tmp_path: Path, stream: str) -> None:
    helper = tmp_path / "overflow"
    redirect = " >&2" if stream == "stderr" else ""
    _executable(helper, f"#!/bin/sh\nprintf 123456789{redirect}\n")

    with pytest.raises(CommandOutputTooLargeError, match="exceeded 8 bytes"):
        SubprocessRunner().run(_command(os.fspath(helper), max_output_bytes=8))


def test_output_limit_wins_over_a_later_timeout(tmp_path: Path) -> None:
    helper = tmp_path / "overflow"
    _executable(helper, "#!/bin/sh\nwhile :; do printf 123456789; done\n")

    with pytest.raises(CommandOutputTooLargeError):
        SubprocessRunner().run(_command(os.fspath(helper), timeout=DurationNS(5_000_000_000), max_output_bytes=8))


def test_queued_output_limit_wins_over_simultaneous_cancellation(tmp_path: Path) -> None:
    helper = tmp_path / "overflow-then-wait"
    marker = tmp_path / "overflow-written"
    _executable(helper, '#!/bin/sh\nprintf 123456789\nprintf written > "$1"\nsleep 30\n')

    class CancelAfterOverflow:
        def __init__(self) -> None:
            self.checks = 0

        def is_set(self) -> bool:
            self.checks += 1
            if self.checks <= 2:
                return False
            deadline = time.monotonic() + 2
            while time.monotonic() < deadline and not marker.exists():
                time.sleep(0.001)
            assert marker.exists(), "child did not publish its post-output marker"
            return True

    with pytest.raises(CommandOutputTooLargeError):
        SubprocessRunner().run(
            _command(os.fspath(helper), os.fspath(marker), timeout=DurationNS(5_000_000_000), max_output_bytes=8),
            cancel=CancelAfterOverflow(),
        )


def test_failure_exposes_status_and_private_stderr_only_as_a_matching_oracle(
    caplog: pytest.LogCaptureFixture, tmp_path: Path
) -> None:
    helper = tmp_path / "fail"
    _executable(helper, "#!/bin/sh\nprintf private-provider-value >&2\nprintf sensitive-payload\nexit 7\n")
    command = _command(
        os.fspath(helper),
        disclosure=Disclosure.DECLARED,
        diagnostic=DeclaredCommandDiagnostic(source="github", date="2026-09-06"),
    )

    with caplog.at_level(logging.ERROR, logger="fkf.process"), pytest.raises(CommandFailureError) as caught:
        SubprocessRunner().run(command)

    failure = caught.value
    assert failure.status_class == "exit"
    assert failure.provider_exit_code == 7
    assert failure.exit_code == 1
    assert failure.signal_number is None
    assert failure.matches_stderr("provider-value")
    assert not failure.matches_stderr("")
    diagnostics = f"{failure!s} {failure!r} {caplog.text}"
    assert "private-provider-value" not in diagnostics
    assert "sensitive-payload" not in diagnostics
    assert str(failure) == "command exited with status 7"
    assert caplog.records[0].__dict__["command"] == os.fspath(helper)
    assert caplog.records[0].__dict__["source"] == "github"


def test_declared_display_escapes_terminal_control_characters(caplog: pytest.LogCaptureFixture, tmp_path: Path) -> None:
    helper = tmp_path / "fail"
    _executable(helper, "#!/bin/sh\nexit 2\n")
    command = _command(
        os.fspath(helper),
        "line\nnext\x1b",
        disclosure=Disclosure.DECLARED,
        diagnostic=DeclaredCommandDiagnostic(source="source"),
    )

    with caplog.at_level(logging.ERROR, logger="fkf.process"), pytest.raises(CommandFailureError):
        SubprocessRunner().run(command)

    display = caplog.records[0].__dict__["command"]
    assert "\n" not in display
    assert "\x1b" not in display
    assert r"\n" in display
    assert r"\x1b" in display


def test_signal_failure_has_a_safe_diagnostic(tmp_path: Path) -> None:
    helper = tmp_path / "signal"
    _executable(helper, "#!/bin/sh\nkill -TERM $$\n")

    with pytest.raises(CommandFailureError, match=f"signal {signal.SIGTERM}") as caught:
        SubprocessRunner().run(_command(os.fspath(helper)))

    assert caught.value.status_class == "signal"
    assert caught.value.signal_number == signal.SIGTERM
    assert caught.value.provider_exit_code is None
    assert caught.value.exit_code == 1


def test_auth_discards_output_and_logs_nothing(caplog: pytest.LogCaptureFixture, tmp_path: Path) -> None:
    helper = tmp_path / "auth"
    _executable(helper, '#!/bin/sh\nprintf public\nprintf private >&2\nexit "${1:-0}"\n')
    runner = SubprocessRunner()

    success = runner.run(_command(os.fspath(helper), disclosure=Disclosure.QUIET_AUTH))
    with caplog.at_level(logging.ERROR, logger="fkf.process"), pytest.raises(CommandFailureError) as caught:
        runner.run(_command(os.fspath(helper), "9", disclosure=Disclosure.QUIET_AUTH))

    assert success.stdout == b""
    assert str(caught.value) == "command exited with status 9"
    assert caplog.records == []


def test_body_failure_never_logs_opaque_argument(caplog: pytest.LogCaptureFixture, tmp_path: Path) -> None:
    helper = tmp_path / "body"
    _executable(helper, "#!/bin/sh\nexit 4\n")
    private_field = "stored-private-field-value"

    with caplog.at_level(logging.ERROR, logger="fkf.process"), pytest.raises(CommandFailureError):
        SubprocessRunner().run(_command(os.fspath(helper), private_field, disclosure=Disclosure.OPAQUE_BODY))

    assert private_field not in caplog.text
    assert not hasattr(caplog.records[0], "command")


def test_timeout_kills_descendants_and_bounds_wall_clock(tmp_path: Path) -> None:
    helper = tmp_path / "descendants"
    marker = tmp_path / "survived"
    _executable(helper, '#!/bin/sh\n(sleep 1; printf survived > "$1") &\nsleep 30 | cat\n')
    started = time.monotonic()

    with pytest.raises(CommandTimeoutError):
        SubprocessRunner().run(_command(os.fspath(helper), os.fspath(marker), timeout=DurationNS(150_000_000)))

    assert time.monotonic() - started < 5
    time.sleep(1.1)
    assert not marker.exists()


def test_explicit_cancellation_kills_the_process_group(tmp_path: Path) -> None:
    helper = tmp_path / "wait"
    started = tmp_path / "started"
    _executable(helper, '#!/bin/sh\nprintf started > "$1"\nsleep 30\n')
    cancel = threading.Event()

    def request_cancel() -> None:
        for _ in range(100):
            if started.exists():
                cancel.set()
                return
            time.sleep(0.01)

    thread = threading.Thread(target=request_cancel)
    thread.start()
    with pytest.raises(CommandCanceledError):
        SubprocessRunner().run(
            _command(os.fspath(helper), os.fspath(started), timeout=DurationNS(10_000_000_000)), cancel=cancel
        )
    thread.join()

    assert started.exists()


def test_already_canceled_command_never_starts(tmp_path: Path) -> None:
    helper = tmp_path / "start"
    marker = tmp_path / "started"
    _executable(helper, '#!/bin/sh\nprintf started > "$1"\n')
    cancel = threading.Event()
    cancel.set()

    with pytest.raises(CommandCanceledError):
        SubprocessRunner().run(_command(os.fspath(helper), os.fspath(marker)), cancel=cancel)

    assert not marker.exists()


def test_failed_pre_exec_revalidation_prevents_process_start(tmp_path: Path) -> None:
    helper = tmp_path / "start"
    marker = tmp_path / "started"
    _executable(helper, '#!/bin/sh\nprintf started > "$1"\n')

    def reject_changed_trust() -> None:
        raise RuntimeError("trust changed")

    with pytest.raises(RuntimeError, match="trust changed"):
        SubprocessRunner().run(_command(os.fspath(helper), os.fspath(marker), before_exec=reject_changed_trust))

    assert not marker.exists()


@pytest.mark.parametrize("limit", [0, MAX_COMMAND_OUTPUT_BYTES + 1])
def test_invalid_output_limits_are_rejected_before_execution(limit: int) -> None:
    with pytest.raises(ValueError, match="output limit"):
        SubprocessRunner().run(_command("missing", max_output_bytes=limit))
