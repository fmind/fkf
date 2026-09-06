"""Inspect and explicitly refresh exact-byte official collection helpers."""

from __future__ import annotations

import hashlib
import os
import stat
from dataclasses import dataclass, field
from enum import StrEnum
from pathlib import Path, PurePosixPath

from fkf.assets import HOOK_SCRIPT, shipped_helpers
from fkf.base import Base
from fkf.config import Config
from fkf.errors import CanceledError
from fkf.io import atomic_write, read_file_limited
from fkf.process import Cancellation
from fkf.store import BASE_BIN_DIR, MAX_CONTROL_FILE_BYTES, UnsafePathError, validate_within_root


class HelperState(StrEnum):
    """Exact-byte agreement between an installed and bundled helper."""

    CURRENT = "current"
    DRIFTED = "drifted"
    MISSING = "missing"


@dataclass(slots=True)
class HelperStatus:
    """State of one official helper required by the enabled execution plan."""

    name: str
    path: str
    state: HelperState
    required: bool
    current_sha256: str = field(default="", metadata={"json": "current_sha256,omitempty"})
    shipped_sha256: str = ""
    refreshed: bool = field(default=False, metadata={"json": "refreshed,omitempty"})


@dataclass(slots=True)
class HelperReport:
    """Exact diff and optional repair result for required official helpers."""

    base: str
    helpers: tuple[HelperStatus, ...]
    current: int = 0
    drifted: int = 0
    missing: int = 0
    refreshed: int = 0


def _check_cancel(cancel: Cancellation | None) -> None:
    if cancel is not None and cancel.is_set():
        raise CanceledError("helper inspection canceled")


def _digest(content: bytes) -> str:
    return hashlib.sha256(content).hexdigest()


def required_helper_names(config: Config | None, helpers: dict[str, bytes] | None = None) -> tuple[str, ...]:
    """Select only official helpers used by enabled sources, plus the session hook."""
    available = shipped_helpers() if helpers is None else helpers
    required = {HOOK_SCRIPT}
    if config is not None:
        for source in config.enabled_sources():
            required.update(requirement for requirement in source.requires if requirement in available)
    return tuple(sorted(required))


def _inspect_one(root: Path, name: str, content: bytes) -> HelperStatus:
    relative = PurePosixPath(BASE_BIN_DIR, name).as_posix()
    target = root / BASE_BIN_DIR / name
    shipped_sha256 = _digest(content)
    status_item = HelperStatus(
        name=name,
        path=relative,
        state=HelperState.MISSING,
        required=True,
        shipped_sha256=shipped_sha256,
    )
    try:
        info = target.lstat()
    except FileNotFoundError:
        return status_item
    except OSError as error:
        raise OSError(f"inspect {relative}: {error}") from error
    if stat.S_ISLNK(info.st_mode):
        raise UnsafePathError(f"refusing helper symlink {relative}")
    if not stat.S_ISREG(info.st_mode):
        raise UnsafePathError(f"helper {relative} is not a regular file")
    current = read_file_limited(target, MAX_CONTROL_FILE_BYTES)
    status_item.current_sha256 = _digest(current)
    status_item.state = HelperState.CURRENT if current == content else HelperState.DRIFTED
    return status_item


def inspect_helpers(
    base: Base,
    *,
    refresh: bool = False,
    cancel: Cancellation | None = None,
) -> HelperReport:
    """Inspect required official helpers and optionally restore exact bundled bytes."""
    _check_cancel(cancel)
    helpers = shipped_helpers()
    bin_directory = base.root / BASE_BIN_DIR
    validate_within_root(base.root, bin_directory)
    statuses: list[HelperStatus] = []
    for name in required_helper_names(base.config, helpers):
        _check_cancel(cancel)
        statuses.append(_inspect_one(base.root, name, helpers[name]))

    # Every target was inspected before the first mutation, so a later conflict writes nothing.
    if refresh:
        for status_item in statuses:
            _check_cancel(cancel)
            if status_item.state is HelperState.CURRENT:
                continue
            target = base.root / Path(status_item.path)
            validate_within_root(base.root, target)
            atomic_write(target, helpers[status_item.name], mode=0o700)
            status_item.state = HelperState.CURRENT
            status_item.current_sha256 = status_item.shipped_sha256
            status_item.refreshed = True

    report = HelperReport(base=os.fspath(base.root), helpers=tuple(statuses))
    report.current = sum(status_item.state is HelperState.CURRENT for status_item in statuses)
    report.drifted = sum(status_item.state is HelperState.DRIFTED for status_item in statuses)
    report.missing = sum(status_item.state is HelperState.MISSING for status_item in statuses)
    report.refreshed = sum(status_item.refreshed for status_item in statuses)
    return report


def install_missing_required_helpers(
    root: Path,
    config: Config | None,
    *,
    cancel: Cancellation | None = None,
) -> tuple[str, ...]:
    """Create missing required helpers without replacing any owner-controlled entry."""
    helpers = shipped_helpers()
    validate_within_root(root, root / BASE_BIN_DIR)
    names = required_helper_names(config, helpers)
    missing: list[tuple[str, Path]] = []
    for name in names:
        _check_cancel(cancel)
        target = root / BASE_BIN_DIR / name
        validate_within_root(root, target)
        try:
            target.lstat()
        except FileNotFoundError:
            missing.append((name, target))
        except OSError as error:
            raise OSError(f"inspect {target}: {error}") from error
    written: list[str] = []
    for name, target in missing:
        _check_cancel(cancel)
        atomic_write(target, helpers[name], mode=0o700)
        written.append(name)
    return tuple(written)


__all__ = [
    "HelperReport",
    "HelperState",
    "HelperStatus",
    "inspect_helpers",
    "install_missing_required_helpers",
    "required_helper_names",
]
