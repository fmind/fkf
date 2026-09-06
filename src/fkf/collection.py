"""Pure all-or-nothing collection at the declared command boundary."""

from __future__ import annotations

from collections.abc import Sequence
from datetime import datetime, tzinfo

from fkf.config import Source
from fkf.documents import (
    Document,
    IncompleteCollectionError,
    Window,
    build_document,
    build_window_documents,
    decode_records,
)
from fkf.process import Cancellation, CommandCanceledError, Runner
from fkf.source_runtime import Environment, build_run_command
from fkf.store import Layer
from fkf.timeutil import DurationNS


def _incomplete(source: Source, error: BaseException) -> IncompleteCollectionError:
    if isinstance(error, IncompleteCollectionError):
        return error
    return IncompleteCollectionError(f"source {source.name}: {error}", cause=error)


def collect(
    runner: Runner,
    source: Source,
    environment: Environment,
    window: Window,
    timeout: DurationNS,
    collected_at: datetime,
    *,
    cancel: Cancellation | None = None,
) -> Document:
    """Run one source once and return a complete unwritten document."""
    try:
        command = build_run_command(source, environment, window, timeout)
        output = runner.run(command, cancel=cancel).stdout
        records = decode_records(source, output)
        return build_document(
            source,
            records,
            window=window if source.layer is Layer.EVENTS else None,
            collected_at=collected_at,
        )
    except IncompleteCollectionError:
        raise
    except CommandCanceledError:
        raise
    except Exception as error:
        raise _incomplete(source, error) from error


def collect_window(
    runner: Runner,
    source: Source,
    environment: Environment,
    range_window: Window,
    dates: Sequence[str],
    zone: tzinfo,
    timeout: DurationNS,
    collected_at: datetime,
    *,
    cancel: Cancellation | None = None,
) -> dict[str, Document]:
    """Run one windowed source once and partition its complete output by civil day."""
    try:
        command = build_run_command(source, environment, range_window, timeout)
        output = runner.run(command, cancel=cancel).stdout
        records = decode_records(source, output)
        return build_window_documents(source, records, dates, zone, collected_at=collected_at)
    except IncompleteCollectionError:
        raise
    except CommandCanceledError:
        raise
    except Exception as error:
        raise _incomplete(source, error) from error


__all__ = ["collect", "collect_window"]
