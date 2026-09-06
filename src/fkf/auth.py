"""Private, trust-gated readiness probes for declared provider commands."""

from __future__ import annotations

from collections.abc import Iterable

from fkf.base import Base
from fkf.config import Source
from fkf.process import Cancellation, CommandFailureError, check_cancel
from fkf.source_runtime import build_auth_command
from fkf.trust import require_trust


def _probe(base: Base, source: Source, cancel: Cancellation | None) -> bool:
    if not source.auth:
        return True
    command = build_auth_command(source, base.environment, base.config.sync.timeout)
    try:
        base.runner.run(command, cancel=cancel)
    except CommandFailureError as error:
        # A normal exit is the provider answering "not ready". Signals, timeouts,
        # unsafe paths, and runner failures remain hard operational errors.
        if error.provider_exit_code is not None:
            return False
        raise
    check_cancel(cancel)
    return True


def probe_source_auth(
    base: Base,
    candidates: Iterable[Source],
    *,
    live: bool,
    cancel: Cancellation | None = None,
) -> tuple[str, ...]:
    """Return enabled source names whose provider is not ready.

    Offline callers pass ``live=False``. Identical literal argv are executed only
    once because multiple sources commonly share one provider login.
    """
    selected = tuple(candidates)
    check_cancel(cancel)
    if not live or not any(source.auth for source in selected):
        return ()
    require_trust(base.config, cancel=cancel)
    observed: dict[tuple[str, ...], bool] = {}
    required: list[str] = []
    for source in selected:
        check_cancel(cancel)
        if not source.auth:
            continue
        ready = observed.get(source.auth)
        if ready is None:
            ready = _probe(base, source, cancel)
            observed[source.auth] = ready
        if not ready:
            required.append(source.name)
    return tuple(sorted(required))


__all__ = ["probe_source_auth"]
