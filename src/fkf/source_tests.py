"""Sequential execution of optional, trust-gated source verification hooks."""

from __future__ import annotations

from dataclasses import dataclass
from datetime import datetime
from enum import StrEnum

from fkf.base import Base
from fkf.config import ConfigError, Source
from fkf.process import Cancellation, CommandCanceledError, display_argv
from fkf.source_runtime import build_test_command
from fkf.timeutil import DurationNS, format_duration
from fkf.trust import require_trust


class SourceTestOutcome(StrEnum):
    PASSED = "passed"
    FAILED = "failed"


@dataclass(frozen=True, slots=True)
class SourceTestRequest:
    targets: tuple[str, ...] = ()
    all: bool = False


@dataclass(frozen=True, slots=True)
class SourceTestResult:
    source: str
    enabled: bool
    command: str
    outcome: SourceTestOutcome
    elapsed: str
    error: str = ""


@dataclass(frozen=True, slots=True)
class SourceTestReport:
    base: str
    sources: tuple[SourceTestResult, ...]
    passed: int
    failed: int
    complete: bool
    elapsed: str

    def failure_summary(self) -> str:
        """Render only reviewed source names, argv, and safe runner diagnostics."""
        return "\n".join(
            f"{result.source}: {result.error} (command: {result.command})"
            for result in self.sources
            if result.outcome is SourceTestOutcome.FAILED
        )


def _elapsed(start: datetime, end: datetime) -> str:
    nanoseconds = round((end - start).total_seconds() * 1_000) * 1_000_000
    return format_duration(DurationNS(nanoseconds))


def _targets(base: Base, request: SourceTestRequest) -> tuple[Source, ...]:
    if request.all and request.targets:
        raise ConfigError("--all cannot be combined with source names")
    if not request.targets:
        return tuple(
            source
            for source in (base.config.sources[name] for name in base.config.source_names())
            if source.test and (request.all or source.enabled)
        )
    seen: set[str] = set()
    targets: list[Source] = []
    for name in request.targets:
        if name in seen:
            raise ConfigError(f"duplicate source {name!r}; name each test target once")
        seen.add(name)
        source = base.source(name)
        if not source.test:
            raise ConfigError(f"source {name} declares no test hook in {base.config.path}")
        targets.append(source)
    return tuple(targets)


def run_source_tests(
    base: Base,
    request: SourceTestRequest | None = None,
    *,
    cancel: Cancellation | None = None,
) -> SourceTestReport:
    """Run selected hooks in stable order and continue after safe hook failures."""
    if cancel is not None and cancel.is_set():
        raise CommandCanceledError("command canceled")
    started = base.now()
    request = SourceTestRequest() if request is None else request
    targets = _targets(base, request)
    if not targets:
        return SourceTestReport(str(base.root), (), 0, 0, True, _elapsed(started, base.now()))
    require_trust(base.config, cancel=cancel)
    results: list[SourceTestResult] = []
    passed = 0
    failed = 0
    for source in targets:
        if cancel is not None and cancel.is_set():
            raise CommandCanceledError("command canceled")
        result_started = base.now()
        command = build_test_command(source, base.environment, base.config.sync.timeout)
        try:
            base.runner.run(command, cancel=cancel)
        except CommandCanceledError:
            raise
        except Exception as error:
            failed += 1
            results.append(
                SourceTestResult(
                    source.name,
                    source.enabled,
                    display_argv(command.argv),
                    SourceTestOutcome.FAILED,
                    _elapsed(result_started, base.now()),
                    str(error),
                )
            )
        else:
            passed += 1
            results.append(
                SourceTestResult(
                    source.name,
                    source.enabled,
                    display_argv(command.argv),
                    SourceTestOutcome.PASSED,
                    _elapsed(result_started, base.now()),
                )
            )
    return SourceTestReport(str(base.root), tuple(results), passed, failed, failed == 0, _elapsed(started, base.now()))


__all__ = [
    "SourceTestOutcome",
    "SourceTestReport",
    "SourceTestRequest",
    "SourceTestResult",
    "run_source_tests",
]
