"""Bounded POSIX execution for commands declared by an FKF base."""

from __future__ import annotations

import errno
import logging
import os
import selectors
import shlex
import signal
import stat
import subprocess
import time
from collections.abc import Callable, Mapping, Sequence
from contextlib import suppress
from dataclasses import dataclass, field
from enum import StrEnum
from pathlib import Path
from typing import BinaryIO, Final, Protocol, cast, runtime_checkable

from fkf.errors import CanceledError, OperationalError
from fkf.timeutil import DurationNS

MAX_COMMAND_OUTPUT_BYTES: Final = 64 << 20
DECLARED_COMMAND_DIRECTORY: Final = Path("/")
DECLARED_COMMAND_ENVIRONMENT_POLICY: Final = (
    "provider environment without runtime startup loaders or base-resolving home/config roots"
)

_READ_SIZE: Final = 64 << 10
_POLL_SECONDS: Final = 0.05
_KILL_WAIT_SECONDS: Final = 5.0
_RUNTIME_STARTUP_KEYS: Final = frozenset(
    {
        "BASH_ENV",
        "ENV",
        "ZDOTDIR",
        "fish_function_path",
        "PYTHONHOME",
        "PYTHONPATH",
        "PYTHONSTARTUP",
        "PYTHONINSPECT",
        "PYTHONWARNINGS",
        "PYTHONUSERBASE",
        "PYTHONPLATLIBDIR",
        "NODE_OPTIONS",
        "NODE_PATH",
        "PERL5OPT",
        "PERL5LIB",
        "PERLLIB",
        "RUBYOPT",
        "RUBYLIB",
        "RUBYGEMS_GEMDEPS",
        "GEM_PATH",
        "JAVA_TOOL_OPTIONS",
        "JDK_JAVA_OPTIONS",
        "_JAVA_OPTIONS",
        "LD_PRELOAD",
        "LD_LIBRARY_PATH",
        "LD_AUDIT",
        "GCONV_PATH",
        "R_ENVIRON",
        "R_ENVIRON_USER",
        "R_PROFILE",
        "R_PROFILE_USER",
    }
)
_CONFIG_ROOT_KEYS: Final = (
    "HOME",
    "XDG_CONFIG_HOME",
    "XDG_DATA_HOME",
    "XDG_STATE_HOME",
    "XDG_CACHE_HOME",
)

_LOGGER = logging.getLogger(__name__)


class Disclosure(StrEnum):
    """How much reviewed command context may enter diagnostics."""

    DECLARED = "declared"
    QUIET_AUTH = "quiet_auth"
    OPAQUE_BODY = "opaque_body"


@dataclass(frozen=True, slots=True)
class DeclaredCommandDiagnostic:
    """Base-authored collection context safe to disclose on failure."""

    source: str
    date: str = ""
    window_start: str = ""
    window_end: str = ""


@runtime_checkable
class Cancellation(Protocol):
    """Minimal event-like cancellation contract accepted by the real runner."""

    def is_set(self) -> bool:
        """Return whether the caller has requested cancellation."""
        ...


def check_cancel(cancel: Cancellation | None) -> None:
    """Raise the shared cancellation error at an explicit operation checkpoint."""
    if cancel is not None and cancel.is_set():
        raise CanceledError("operation canceled")


@dataclass(frozen=True, slots=True)
class Command:
    """One direct-argv execution request."""

    argv: tuple[str, ...]
    timeout: DurationNS
    stdin: bytes | None = None
    environment: Mapping[str, str] = field(default_factory=dict)
    base: Path | None = None
    configured_bin: tuple[str, ...] = ()
    source_test: bool = False
    disclosure: Disclosure = Disclosure.OPAQUE_BODY
    diagnostic: DeclaredCommandDiagnostic | None = None
    max_output_bytes: int = MAX_COMMAND_OUTPUT_BYTES
    before_exec: Callable[[], None] | None = field(default=None, repr=False, compare=False)


@dataclass(frozen=True, slots=True)
class CommandResult:
    """Successful bounded command output."""

    stdout: bytes
    returncode: int = 0


class Runner(Protocol):
    """Fake-friendly command execution seam used by source services."""

    def run(self, command: Command, *, cancel: Cancellation | None = None) -> CommandResult:
        """Execute one command or raise a safe typed failure."""
        ...


class CommandError(OperationalError):
    """A declared command could not complete safely."""


class CommandOutputTooLargeError(CommandError):
    """At least one captured stream exceeded its independent byte limit."""


class CommandTimeoutError(CommandError, TimeoutError):
    """A command and its descendants exceeded their deadline."""


class CommandCanceledError(CanceledError):
    """A caller canceled a command and its descendants."""


class CommandFailureError(CommandError):
    """A process exited unsuccessfully while keeping provider stderr private."""

    __slots__ = ("_stderr", "provider_exit_code", "signal_number", "status_class")

    def __init__(self, returncode: int, stderr: bytes) -> None:
        self._stderr = stderr
        if returncode < 0:
            self.status_class = "signal"
            self.provider_exit_code = None
            self.signal_number = -returncode
            diagnostic = f"command terminated by signal {self.signal_number}"
        else:
            self.status_class = "exit"
            self.provider_exit_code = returncode
            self.signal_number = None
            diagnostic = f"command exited with status {returncode}"
        super().__init__(diagnostic)

    def matches_stderr(self, condition: str | bytes) -> bool:
        """Answer one retry condition without exposing captured provider bytes."""
        if not condition:
            return False
        encoded = condition.encode() if isinstance(condition, str) else condition
        return encoded in self._stderr


def _is_within(root: Path, candidate: Path) -> bool:
    try:
        candidate.relative_to(root)
    except ValueError:
        return False
    return True


def _physical(path: Path) -> Path | None:
    try:
        return path.resolve(strict=False)
    except OSError, RuntimeError:
        return None


def _unsafe_config_root(value: str, forbidden_root: Path | None) -> bool:
    if not value:
        return True
    candidate = Path(value)
    if not candidate.is_absolute():
        return True
    if forbidden_root is None:
        return False

    root = Path(os.path.normpath(forbidden_root))
    absolute = Path(os.path.normpath(candidate))
    if _is_within(root, absolute):
        return True
    physical_root = _physical(root)
    physical_candidate = _physical(absolute)
    return physical_root is None or physical_candidate is None or _is_within(physical_root, physical_candidate)


def sanitize_path(path_value: str, forbidden_root: Path | None = None) -> str:
    """Keep unique absolute PATH entries outside a forbidden base."""
    root = Path(os.path.normpath(forbidden_root)) if forbidden_root is not None else None
    physical_root = _physical(root) if root is not None else None
    directories: list[str] = []
    seen: set[str] = set()
    for raw in path_value.split(os.pathsep):
        candidate = Path(raw)
        if not raw or not candidate.is_absolute():
            continue
        cleaned = Path(os.path.normpath(candidate))
        if root is not None:
            if _is_within(root, cleaned):
                continue
            physical = _physical(cleaned)
            if physical is None or physical_root is None or _is_within(physical_root, physical):
                continue
        rendered = os.fspath(cleaned)
        if rendered not in seen:
            directories.append(rendered)
            seen.add(rendered)
    return os.pathsep.join(directories)


def command_path(
    *,
    base: Path | None,
    configured_bin: Sequence[str] = (),
    inherited: str | None = None,
    source_test: bool = False,
) -> str:
    """Build the one PATH policy used by executable checks and execution."""
    if source_test and base is None:
        raise ValueError("source-test execution requires an absolute base")
    if base is not None and not base.is_absolute():
        raise ValueError("command base must be absolute")

    inherited_value = os.environ.get("PATH", "") if inherited is None else inherited
    safe_external = sanitize_path(os.pathsep.join((*configured_bin, inherited_value)), base)
    entries: list[str] = []
    if base is not None:
        if source_test:
            entries.append(os.fspath(base / "tests"))
        entries.append(os.fspath(base / "bin"))
    if safe_external:
        entries.extend(safe_external.split(os.pathsep))
    # Base paths are deliberately admitted above; the generic sanitizer must reject every
    # other path that reaches the same physical repository.
    return os.pathsep.join(dict.fromkeys(entries))


def command_environment(command: Command) -> dict[str, str]:
    """Return a copied, sanitized child environment without logging its values."""
    values = dict(os.environ)
    for key, value in command.environment.items():
        if not key or "=" in key or "\0" in key or "\0" in value:
            raise ValueError("command environment contains an invalid key or value")
        values[key] = value

    for key in tuple(values):
        if key in _RUNTIME_STARTUP_KEYS or key.startswith(("DYLD_", "LUA_INIT")):
            values.pop(key, None)
    for key in _CONFIG_ROOT_KEYS:
        value = values.get(key)
        if value is not None and _unsafe_config_root(value, command.base):
            values.pop(key, None)

    values["PATH"] = command_path(
        base=command.base,
        configured_bin=command.configured_bin,
        inherited=values.get("PATH", ""),
        source_test=command.source_test,
    )
    return values


def resolve_executable(name: str, path_value: str) -> Path:
    """Resolve one absolute path or bare name against the sanitized child PATH."""
    if not name or "\0" in name:
        raise ValueError("executable name is empty or invalid")
    candidate = Path(name)
    if os.sep in name:
        if not candidate.is_absolute():
            raise ValueError(f"relative executable path {name!r} is not allowed; use a bare name or an absolute path")
        if _is_executable_file(candidate):
            return candidate
        raise FileNotFoundError(f"executable {name!r} not found or not executable")

    for directory in path_value.split(os.pathsep):
        if not directory:
            continue
        candidate = Path(directory) / name
        if _is_executable_file(candidate):
            return candidate
    raise FileNotFoundError(f"executable {name!r} not found on PATH")


def _is_executable_file(path: Path) -> bool:
    try:
        info = path.stat()
    except OSError:
        return False
    return stat.S_ISREG(info.st_mode) and info.st_mode & 0o111 != 0


def display_argv(argv: Sequence[str]) -> str:
    """Render direct argv for a declared-command diagnostic only."""
    return " ".join(_display_argument(argument) for argument in argv)


def _display_argument(argument: str) -> str:
    if not any(ord(character) < 0x20 or ord(character) == 0x7F for character in argument):
        return shlex.quote(argument)
    escaped: list[str] = ["$'"]
    for character in argument:
        codepoint = ord(character)
        if character in {"\\", "'"}:
            escaped.extend(("\\", character))
        elif character == "\n":
            escaped.append("\\n")
        elif character == "\r":
            escaped.append("\\r")
        elif character == "\t":
            escaped.append("\\t")
        elif codepoint < 0x20 or codepoint == 0x7F:
            escaped.append(f"\\x{codepoint:02x}")
        else:
            escaped.append(character)
    escaped.append("'")
    return "".join(escaped)


class _Termination(StrEnum):
    TIMEOUT = "timeout"
    CANCELED = "canceled"
    OUTPUT_LIMIT = "output_limit"


@dataclass(slots=True)
class _CapturedStreams:
    stdout: bytearray = field(default_factory=bytearray)
    stderr: bytearray = field(default_factory=bytearray)


class SubprocessRunner:
    """Real direct-argv runner with bounded pipes and process-group cancellation."""

    def run(self, command: Command, *, cancel: Cancellation | None = None) -> CommandResult:
        """Execute one command under the fixed FKF process boundary."""
        _validate_command(command)
        if cancel is not None and cancel.is_set():
            error = CommandCanceledError("command canceled")
            _log_failure(command, error, status=_Termination.CANCELED.value)
            raise error
        environment = command_environment(command)
        executable = resolve_executable(command.argv[0], environment["PATH"])
        if command.before_exec is not None:
            command.before_exec()
        if cancel is not None and cancel.is_set():
            error = CommandCanceledError("command canceled")
            _log_failure(command, error, status=_Termination.CANCELED.value)
            raise error
        quiet = command.disclosure is Disclosure.QUIET_AUTH

        try:
            # The executable comes from a sanitized PATH; the canonical plan is rechecked immediately above.
            process = subprocess.Popen(  # noqa: S603  # nosemgrep: dangerous-subprocess-use-audit
                (os.fspath(executable), *command.argv[1:]),
                cwd=DECLARED_COMMAND_DIRECTORY,
                env=environment,
                stdin=subprocess.PIPE if command.stdin is not None else subprocess.DEVNULL,
                stdout=subprocess.DEVNULL if quiet else subprocess.PIPE,
                stderr=subprocess.DEVNULL if quiet else subprocess.PIPE,
                start_new_session=True,
            )
        except OSError as error:
            raise CommandError("command execution failed", cause=error) from error

        try:
            streams, termination = _communicate(process, command, cancel=cancel, quiet=quiet)
        except BaseException:
            _terminate_process_group(process)
            raise

        if termination is not None:
            _terminate_process_group(process)
            if termination is _Termination.OUTPUT_LIMIT:
                error: CommandError = CommandOutputTooLargeError(
                    f"command output exceeded {command.max_output_bytes} bytes"
                )
            elif termination is _Termination.CANCELED:
                error = CommandCanceledError("command canceled")
            else:
                error = CommandTimeoutError("command timed out")
            _log_failure(command, error, status=termination.value)
            raise error

        returncode = process.wait()
        if returncode != 0:
            failure = CommandFailureError(returncode, bytes(streams.stderr))
            _log_failure(command, failure, status=failure.status_class)
            raise failure
        return CommandResult(stdout=b"" if quiet else bytes(streams.stdout))


def _validate_command(command: Command) -> None:
    if not command.argv:
        raise ValueError("empty command")
    if any(not isinstance(argument, str) or "\0" in argument for argument in command.argv):
        raise ValueError("command argv contains a non-string or NUL byte")
    if int(command.timeout) <= 0:
        raise ValueError("command timeout must be positive")
    if command.max_output_bytes <= 0 or command.max_output_bytes > MAX_COMMAND_OUTPUT_BYTES:
        raise ValueError(f"command output limit must be between 1 and {MAX_COMMAND_OUTPUT_BYTES} bytes")
    if command.base is not None and not command.base.is_absolute():
        raise ValueError("command base must be absolute")
    if command.source_test and command.base is None:
        raise ValueError("source-test execution requires an absolute base")
    if command.disclosure is Disclosure.DECLARED and (command.diagnostic is None or not command.diagnostic.source):
        raise ValueError("declared command disclosure requires a source diagnostic")


def _communicate(
    process: subprocess.Popen[bytes],
    command: Command,
    *,
    cancel: Cancellation | None,
    quiet: bool,
) -> tuple[_CapturedStreams, _Termination | None]:
    streams = _CapturedStreams()
    deadline = time.monotonic() + command.timeout.seconds
    with selectors.DefaultSelector() as selector:
        pending_input: memoryview | None = None
        if process.stdin is not None:
            os.set_blocking(process.stdin.fileno(), False)
            pending_input = memoryview(command.stdin or b"")
            selector.register(process.stdin, selectors.EVENT_WRITE, "stdin")
        if not quiet:
            stdout = process.stdout
            stderr = process.stderr
            if stdout is None or stderr is None:
                raise RuntimeError("captured command pipes were not created")
            for pipe, stream_name in ((stdout, "stdout"), (stderr, "stderr")):
                os.set_blocking(pipe.fileno(), False)
                selector.register(pipe, selectors.EVENT_READ, stream_name)

        while selector.get_map() or process.poll() is None:
            remaining = deadline - time.monotonic()
            termination: _Termination | None = None
            if cancel is not None and cancel.is_set():
                termination = _Termination.CANCELED
            elif remaining <= 0:
                termination = _Termination.TIMEOUT

            if selector.get_map():
                # Drain bytes already acknowledged by the kernel before classifying a
                # simultaneous terminal event, so an exceeded output bound cannot be hidden
                # as cancellation or timeout.
                wait = 0 if termination is not None else min(_POLL_SECONDS, remaining)
                events = selector.select(wait)
            else:
                if termination is not None:
                    return streams, termination
                with suppress(subprocess.TimeoutExpired):
                    process.wait(timeout=min(_POLL_SECONDS, remaining))
                events = ()

            for key, _events in events:
                if key.data == "stdin":
                    if termination is None:
                        pending_input = _write_stdin(selector, cast(BinaryIO, key.fileobj), pending_input)
                    continue
                pipe = cast(BinaryIO, key.fileobj)
                try:
                    chunk = os.read(pipe.fileno(), _READ_SIZE)
                except BlockingIOError:
                    continue
                if not chunk:
                    selector.unregister(pipe)
                    pipe.close()
                    continue
                target = streams.stdout if key.data == "stdout" else streams.stderr
                remaining_bytes = command.max_output_bytes - len(target)
                if len(chunk) > remaining_bytes:
                    target.extend(chunk[:remaining_bytes])
                    return streams, _Termination.OUTPUT_LIMIT
                target.extend(chunk)
            if termination is not None:
                return streams, termination
    return streams, None


def _write_stdin(
    selector: selectors.BaseSelector,
    pipe: BinaryIO,
    pending: memoryview | None,
) -> memoryview | None:
    if pending is None or len(pending) == 0:
        selector.unregister(pipe)
        pipe.close()
        return None
    try:
        written = os.write(pipe.fileno(), pending)
    except BrokenPipeError:
        selector.unregister(pipe)
        pipe.close()
        return None
    except BlockingIOError:
        return pending
    return pending[written:]


def _kill_process_group(process: subprocess.Popen[bytes]) -> None:
    # Poll first to reduce the PID-reuse race before signaling, while still trying
    # the group because an exited leader may have left descendants holding pipes.
    process.poll()
    try:
        os.killpg(process.pid, signal.SIGKILL)
    except PermissionError:
        if process.poll() is not None:
            return
        # Preserve the signaling failure, but stop the direct child so cleanup can
        # reap it instead of leaking pipes or a zombie.
        with suppress(ProcessLookupError):
            process.kill()
        raise
    except ProcessLookupError:
        if process.poll() is None:
            with suppress(ProcessLookupError):
                process.kill()
    except OSError as error:
        if error.errno != errno.ESRCH:
            raise


def _terminate_process_group(process: subprocess.Popen[bytes]) -> None:
    try:
        _kill_process_group(process)
    finally:
        try:
            _close_process_pipes(process)
        finally:
            _wait_after_kill(process)


def _close_process_pipes(process: subprocess.Popen[bytes]) -> None:
    for pipe in (process.stdin, process.stdout, process.stderr):
        if pipe is not None and not pipe.closed:
            pipe.close()


def _wait_after_kill(process: subprocess.Popen[bytes]) -> None:
    try:
        process.wait(timeout=_KILL_WAIT_SECONDS)
    except subprocess.TimeoutExpired:
        process.kill()
        process.wait()


def _log_failure(command: Command, error: CommandError | CommandCanceledError, *, status: str) -> None:
    if command.disclosure is Disclosure.QUIET_AUTH:
        return
    context: dict[str, object] = {"status": status, "diagnostic": str(error)}
    message = "command failed"
    if command.disclosure is Disclosure.DECLARED:
        message = "declared command failed"
        context["command"] = display_argv(command.argv)
        if command.diagnostic is not None:
            context.update(
                source=command.diagnostic.source,
                date=command.diagnostic.date,
                window_start=command.diagnostic.window_start,
                window_end=command.diagnostic.window_end,
            )
    _LOGGER.error(message, extra=context)


__all__ = [
    "DECLARED_COMMAND_DIRECTORY",
    "DECLARED_COMMAND_ENVIRONMENT_POLICY",
    "MAX_COMMAND_OUTPUT_BYTES",
    "Cancellation",
    "Command",
    "CommandCanceledError",
    "CommandError",
    "CommandFailureError",
    "CommandOutputTooLargeError",
    "CommandResult",
    "CommandTimeoutError",
    "DeclaredCommandDiagnostic",
    "Disclosure",
    "Runner",
    "SubprocessRunner",
    "check_cancel",
    "command_environment",
    "command_path",
    "display_argv",
    "resolve_executable",
    "sanitize_path",
]
