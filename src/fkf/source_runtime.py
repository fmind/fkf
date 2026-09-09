"""Trusted direct-argv planning, retry, and pacing for declared sources."""

from __future__ import annotations

import os
import re
import threading
import time
from collections.abc import Callable, Mapping
from dataclasses import dataclass, field
from pathlib import Path
from typing import Protocol

from fkf.config import BodyPolicy, Config, Source, valid_body_value
from fkf.errors import OperationalError
from fkf.fields import FieldMap
from fkf.process import (
    Cancellation,
    Command,
    CommandCanceledError,
    CommandFailureError,
    CommandResult,
    DeclaredCommandDiagnostic,
    Disclosure,
    Runner,
    command_path,
    resolve_executable,
    sanitize_path,
)
from fkf.store import BASE_DIR_MODE, BASE_SOURCES_DIR, expand_home, validate_directory_confinement
from fkf.timeutil import DurationNS, format_duration
from fkf.trust import trust_check

_PLACEHOLDER_PATTERN = re.compile(r"\{\{([a-z][a-z0-9_-]*)\}\}")
_GITHUB_LOGIN_PATTERN = re.compile(r"^[A-Za-z0-9](?:[A-Za-z0-9-]{0,37}[A-Za-z0-9])?$")


class WindowLike(Protocol):
    """The civil-window fields needed to render collection argv."""

    @property
    def date(self) -> str: ...

    @property
    def next(self) -> str: ...

    @property
    def start(self) -> str: ...

    @property
    def end(self) -> str: ...


class _Sleep(Protocol):
    def __call__(self, duration: DurationNS, cancel: Cancellation | None, /) -> None: ...


@dataclass(frozen=True, slots=True)
class Environment:
    """The machine-local execution context resolved for one base."""

    root: Path
    bin: tuple[str, ...] = ()
    environment: Mapping[str, str] = field(default_factory=dict)
    config: Config | None = field(default=None, repr=False, compare=False)

    @classmethod
    def from_config(cls, config: Config, *, inherited_path: str | None = None) -> Environment:
        """Resolve configured directories while excluding inherited paths into the base."""
        root = config.store().root
        inherited = os.environ.get("PATH", "") if inherited_path is None else inherited_path
        safe_inherited = sanitize_path(inherited, root)
        configured = tuple(expand_home(entry) for entry in config.bin)
        return cls(root=root, bin=configured, environment={"PATH": safe_inherited}, config=config)

    def path(self, *, source_test: bool = False) -> str:
        """Return the same PATH the process runner will use."""
        return command_path(
            base=self.root,
            configured_bin=self.bin,
            inherited=self.environment.get("PATH", ""),
            source_test=source_test,
        )

    def look_path(self, name: str) -> Path | None:
        """Resolve a collection/body requirement against the actual child PATH."""
        return _look_path(name, self.path())

    def look_test_path(self, name: str) -> Path | None:
        """Resolve a source-hook requirement with ``tests/`` taking precedence."""
        return _look_path(name, self.path(source_test=True))


def _look_path(name: str, path_value: str) -> Path | None:
    try:
        return resolve_executable(name, path_value)
    except FileNotFoundError, ValueError:
        return None


def _effective_timeout(source: Source, fallback: DurationNS) -> DurationNS:
    return source.timeout if source.timeout > 0 else fallback


def _uses_placeholder(argv: tuple[str, ...], name: str) -> bool:
    placeholder = f"{{{{{name}}}}}"
    return any(placeholder in argument for argument in argv)


def _add_home(argv: tuple[str, ...], values: dict[str, str], *, source: Source, command: str) -> None:
    if not _uses_placeholder(argv, "home"):
        return
    home = os.environ.get("HOME")
    if home is None or not home.strip():
        raise OperationalError(
            f"plan {command} command for source {source.name}: cannot expand {{{{home}}}}: HOME is unset or empty"
        )
    values["home"] = home


def _substitute(template: str, values: Mapping[str, str]) -> str:
    # Substitution is one pass over authored text, so placeholder-looking collected data is
    # never interpreted as another instruction.
    return _PLACEHOLDER_PATTERN.sub(lambda match: values.get(match.group(1), match.group(0)), template)


def _command(
    argv: tuple[str, ...],
    source: Source,
    environment: Environment,
    timeout: DurationNS,
    *,
    disclosure: Disclosure,
    diagnostic: DeclaredCommandDiagnostic | None = None,
    source_test: bool = False,
) -> Command:
    return Command(
        argv=argv,
        timeout=_effective_timeout(source, timeout),
        environment=dict(environment.environment),
        base=environment.root,
        configured_bin=tuple(environment.bin),
        source_test=source_test,
        disclosure=disclosure,
        diagnostic=diagnostic,
        before_exec=trust_check(environment.config) if environment.config is not None else None,
    )


def build_run_command(
    source: Source,
    environment: Environment,
    window: WindowLike,
    timeout: DurationNS,
) -> Command:
    """Substitute only FKF-owned collection values into direct argv."""
    values = {
        "date": window.date,
        "next_date": window.next,
        "start": window.start,
        "end": window.end,
        "base": os.fspath(environment.root),
    }
    _add_home(source.run, values, source=source, command="run")
    argv = tuple(_substitute(argument, values) for argument in source.run)
    diagnostic = DeclaredCommandDiagnostic(
        source=source.name,
        date=window.date,
        window_start=window.start,
        window_end=window.end,
    )
    return _command(argv, source, environment, timeout, disclosure=Disclosure.DECLARED, diagnostic=diagnostic)


def build_test_command(source: Source, environment: Environment, timeout: DurationNS) -> Command:
    """Build a source hook with only stable paths and the test-only PATH."""
    values = {"base": os.fspath(environment.root)}
    _add_home(source.test, values, source=source, command="test")
    argv = tuple(_substitute(argument, values) for argument in source.test)
    diagnostic = DeclaredCommandDiagnostic(source=source.name)
    return _command(
        argv,
        source,
        environment,
        timeout,
        disclosure=Disclosure.DECLARED,
        diagnostic=diagnostic,
        source_test=True,
    )


def build_auth_command(source: Source, environment: Environment, timeout: DurationNS) -> Command:
    """Build one literal, silent authentication-readiness probe."""
    return _command(tuple(source.auth), source, environment, timeout, disclosure=Disclosure.QUIET_AUTH)


def build_body_command(
    source: Source,
    fields: FieldMap,
    environment: Environment,
    record: Mapping[str, object],
    timeout: DurationNS,
) -> Command:
    """Build a body fetch whose collected substitutions stay opaque argv values."""
    if not source.has_body():
        raise OperationalError(
            f"source {source.name} declares no body: command, so its record bodies are not fetchable"
        )
    values = {"base": os.fspath(environment.root)}
    _add_home(source.body, values, source=source, command="body")
    for name in source.fields.names():
        if name in {"base", "home"} or not _uses_placeholder(source.body, name):
            continue
        if name not in fields:
            raise OperationalError(
                f"collected document declares no fields.{name} mapping required by the current body: command; "
                "recollect the record before fetching its body"
            )
        try:
            value = fields.eval_string(name, record)
        except ValueError as error:
            raise OperationalError(
                f"record has no scalar value at the declared fields.{name} path", cause=error
            ) from error
        if value is None:
            raise OperationalError(f"record has no scalar value at the declared fields.{name} path")
        if not valid_body_value(value):
            raise OperationalError(
                f"refusing to run body: for source {source.name}: the {name} value is not a safe opaque argv value "
                "(valid UTF-8, 1..256 bytes, no leading '-' or '@', controls, or invisible format characters)"
            )
        values[name] = value
    argv = tuple(_substitute(argument, values) for argument in source.body)
    return _command(argv, source, environment, timeout, disclosure=Disclosure.OPAQUE_BODY)


def _sleep(duration: DurationNS, cancel: Cancellation | None) -> None:
    deadline = time.monotonic_ns() + int(duration)
    while True:
        if cancel is not None and cancel.is_set():
            raise CommandCanceledError("command canceled")
        remaining = deadline - time.monotonic_ns()
        if remaining <= 0:
            return
        time.sleep(min(remaining / 1_000_000_000, 0.05))


class PolicyRunner:
    """Apply one source's declared retry conditions around another runner."""

    def __init__(self, inner: Runner, source: Source | None, *, sleep: _Sleep = _sleep) -> None:
        self._inner = inner
        self._source = source
        self._sleep = sleep
        self._lock = threading.Lock()
        self._attempts = 0

    @property
    def attempts(self) -> int:
        """Return how many executions this unit actually consumed."""
        with self._lock:
            return self._attempts

    def run(self, command: Command, *, cancel: Cancellation | None = None) -> CommandResult:
        """Retry only matching opaque command failures, never FKF or terminal failures."""
        allowed = 1 if self._source is None else self._source.retry_attempts()
        for attempt in range(1, allowed + 1):
            with self._lock:
                self._attempts = attempt
            try:
                return self._inner.run(command, cancel=cancel)
            except CommandFailureError as error:
                if cancel is not None and cancel.is_set():
                    raise CommandCanceledError("command canceled", cause=error) from error
                if attempt == allowed or not self._retryable(error):
                    raise
                wait = self._backoff(attempt)
                if wait > 0:
                    self._sleep(wait, cancel)
        raise RuntimeError("retry loop exhausted without returning or raising")

    def _backoff(self, attempt: int) -> DurationNS:
        if self._source is None or self._source.retry.backoff <= 0:
            return DurationNS(0)
        return DurationNS(attempt * int(self._source.retry.backoff))

    def _retryable(self, error: CommandFailureError) -> bool:
        if self._source is None:
            return False
        for condition in self._source.retry.on:
            if condition.startswith("exit:"):
                try:
                    wanted = int(condition.removeprefix("exit:").strip())
                except ValueError:
                    continue
                if error.provider_exit_code == wanted:
                    return True
                continue
            if error.matches_stderr(condition):
                return True
        return False


class Pacer:
    """Reserve one monotonic invocation timeline per source across concurrency."""

    def __init__(
        self,
        now_ns: Callable[[], int] | None = None,
        *,
        sleep: _Sleep = _sleep,
    ) -> None:
        self._now_ns = time.monotonic_ns if now_ns is None else now_ns
        self._sleep = sleep
        self._lock = threading.Lock()
        self._next: dict[str, int] = {}

    def wait(self, source: Source | None, *, cancel: Cancellation | None = None) -> None:
        """Wait for this invocation's slot after reserving the following one."""
        if source is None or source.min_interval <= 0:
            return
        with self._lock:
            now = self._now_ns()
            wait_ns = max(0, self._next.get(source.name, now) - now)
            self._next[source.name] = now + wait_ns + int(source.min_interval)
        if wait_ns > 0:
            self._sleep(DurationNS(wait_ns), cancel)


@dataclass(slots=True)
class PacingRunner:
    """Reserve a provider-rate-limit slot for every attempt at the runner boundary."""

    inner: Runner
    pacer: Pacer | None
    source: Source | None

    def run(self, command: Command, *, cancel: Cancellation | None = None) -> CommandResult:
        if self.pacer is not None:
            self.pacer.wait(self.source, cancel=cancel)
        return self.inner.run(command, cancel=cancel)


def describe_policy(source: Source | None) -> str:
    """Render deterministic invocation policy text for a trust review."""
    if source is None:
        return ""
    parts: list[str] = []
    if source.max_age_hours is not None:
        parts.append(f"index max age {source.max_age_hours}h")
    if source.window:
        parts.append("window: one command for the whole requested range")
    if source.has_body():
        detail = "bodies none: every read --body runs the body command and stores nothing"
        if source.bodies is BodyPolicy.CACHE:
            detail = "bodies cache: read --body runs the body command on a cache miss, then caches it"
        elif source.bodies is BodyPolicy.SYNC:
            detail = (
                "bodies sync: read --body runs on a cache miss; sync also prefetches missing or provider-modified "
                "index entries, new event records, and every missing body in one newest event document after prune"
            )
        parts.append(detail)
    if source.retry.attempts > 1:
        detail = f"retry {source.retry.attempts} attempts on {', '.join(source.retry.on)}"
        if source.retry.backoff > 0:
            detail += f", backoff {format_duration(source.retry.backoff)}"
        parts.append(detail)
    if source.min_interval > 0:
        parts.append(f"min interval {format_duration(source.min_interval)}")
    if source.timeout > 0:
        parts.append(f"timeout {format_duration(source.timeout)}")
    return "; ".join(parts)


def normalize_github_noreply_actor(value: str) -> str | None:
    """Derive a stable actor URI from GitHub's documented noreply email forms."""
    local, separator, domain = value.strip().partition("@")
    if not separator or domain.lower() != "users.noreply.github.com":
        return None
    prefix, has_id, login = local.partition("+")
    if has_id:
        if not prefix or not prefix.isascii() or not prefix.isdigit():
            return None
        local = login
    if _GITHUB_LOGIN_PATTERN.fullmatch(local) is None:
        return None
    return f"actor:github.com/{local.lower()}"


def ensure_sources_dir(root: str | os.PathLike[str]) -> Path:
    """Create the trusted helper directory after checking every existing component."""
    directory = Path(os.path.normpath(root)) / BASE_SOURCES_DIR
    validate_directory_confinement(directory)
    directory.mkdir(mode=BASE_DIR_MODE, parents=True, exist_ok=True)
    return directory


__all__ = [
    "Environment",
    "Pacer",
    "PacingRunner",
    "PolicyRunner",
    "WindowLike",
    "build_auth_command",
    "build_body_command",
    "build_run_command",
    "build_test_command",
    "describe_policy",
    "ensure_sources_dir",
    "normalize_github_noreply_actor",
]
