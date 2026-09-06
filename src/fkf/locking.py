"""Fail-fast advisory writer locks keyed by a base's physical identity."""

from __future__ import annotations

import fcntl
import hashlib
import os
import stat
from dataclasses import dataclass
from pathlib import Path
from types import TracebackType
from typing import Self

from fkf.errors import OperationalError
from fkf.store import BASE_DIR_MODE, BASE_FILE_MODE, expand_home, resolve_absolute_path, resolve_physical_path


class BaseBusyError(OperationalError):
    """Another process is mutating the same physical base."""


class StateDirectoryUnavailableError(OperationalError):
    """Private machine-local state has no safe root."""


_DIRECTORY_FLAGS = (
    os.O_RDONLY | getattr(os, "O_CLOEXEC", 0) | getattr(os, "O_DIRECTORY", 0) | getattr(os, "O_NOFOLLOW", 0)
)


def state_dir() -> Path:
    """Resolve the private XDG-compatible FKF state directory."""
    if configured := os.environ.get("XDG_STATE_HOME", "").strip():
        candidate = Path(expand_home(configured))
        if not candidate.is_absolute():
            raise StateDirectoryUnavailableError("fkf state directory is unavailable: XDG_STATE_HOME must be absolute")
        return candidate / "fkf"
    home_value = os.environ.get("HOME", "").strip()
    if not home_value:
        raise StateDirectoryUnavailableError("fkf state directory is unavailable: set HOME or XDG_STATE_HOME")
    home = Path(home_value)
    if not home.is_absolute():
        raise StateDirectoryUnavailableError("fkf state directory is unavailable: HOME must be absolute")
    return home / ".local" / "state" / "fkf"


def private_state_directory(
    root: str | os.PathLike[str],
    subtree: str,
    *,
    purpose: str,
) -> Path:
    """Resolve one FKF state subtree and keep its physical path outside a base."""
    if not subtree or Path(subtree).name != subtree:
        raise ValueError("FKF state subtree must be one path component")
    directory = state_dir() / subtree
    try:
        absolute_base = resolve_absolute_path(root)
        absolute_directory = resolve_absolute_path(directory)
        physical_base = resolve_physical_path(root)
        physical_directory = resolve_physical_path(directory)
    except OSError as error:
        raise StateDirectoryUnavailableError(f"fkf state directory is unavailable: {error}") from error
    if absolute_directory.is_relative_to(absolute_base) or physical_directory.is_relative_to(physical_base):
        raise StateDirectoryUnavailableError(
            f"fkf state directory is unavailable: {purpose} state must stay outside the base"
        )
    for candidate in (directory.parent, directory):
        try:
            info = candidate.lstat()
        except FileNotFoundError:
            continue
        except OSError as error:
            raise StateDirectoryUnavailableError(
                f"fkf state directory is unavailable: inspect {candidate}: {error}"
            ) from error
        if stat.S_ISLNK(info.st_mode) or not stat.S_ISDIR(info.st_mode):
            raise StateDirectoryUnavailableError(
                f"fkf state directory is unavailable: {candidate} must be a real directory"
            )
    return directory


def _open_or_create_private_child(parent_descriptor: int, name: str, path: Path) -> int:
    try:
        inspected = os.stat(name, dir_fd=parent_descriptor, follow_symlinks=False)
    except FileNotFoundError:
        try:
            os.mkdir(name, BASE_DIR_MODE, dir_fd=parent_descriptor)
        except FileExistsError:
            pass
        except OSError as error:
            raise StateDirectoryUnavailableError(
                f"fkf state directory is unavailable: create {path}: {error}"
            ) from error
        try:
            inspected = os.stat(name, dir_fd=parent_descriptor, follow_symlinks=False)
        except OSError as error:
            raise StateDirectoryUnavailableError(
                f"fkf state directory is unavailable: inspect {path}: {error}"
            ) from error
    except OSError as error:
        raise StateDirectoryUnavailableError(f"fkf state directory is unavailable: inspect {path}: {error}") from error
    if stat.S_ISLNK(inspected.st_mode) or not stat.S_ISDIR(inspected.st_mode):
        raise StateDirectoryUnavailableError(f"fkf state directory is unavailable: {path} must be a real directory")
    try:
        descriptor = os.open(name, _DIRECTORY_FLAGS, dir_fd=parent_descriptor)
    except OSError as error:
        raise StateDirectoryUnavailableError(
            f"fkf state directory is unavailable: {path} must be a real directory: {error}"
        ) from error
    try:
        opened = os.fstat(descriptor)
        if (inspected.st_dev, inspected.st_ino) != (opened.st_dev, opened.st_ino):
            raise StateDirectoryUnavailableError(
                f"fkf state directory is unavailable: {path} changed before it was opened"
            )
        # Only FKF-owned namespace components are tightened; parent XDG/HOME paths are not ours.
        os.fchmod(descriptor, BASE_DIR_MODE)
        return descriptor
    except BaseException:
        os.close(descriptor)
        raise


def _ensure_private_state_directory_open(
    root: str | os.PathLike[str],
    subtree: str,
    *,
    purpose: str,
    child: str = "",
) -> tuple[Path, int]:
    namespace = state_dir()
    private_state_directory(root, subtree, purpose=purpose)
    if child and Path(child).name != child:
        raise ValueError("FKF state child must be one path component")
    anchor = namespace.parent
    try:
        anchor.mkdir(parents=True, mode=BASE_DIR_MODE, exist_ok=True)
        physical_anchor = resolve_physical_path(anchor)
        physical_target = physical_anchor / namespace.name / subtree
        if child:
            physical_target /= child
        if physical_target.is_relative_to(resolve_physical_path(root)):
            raise StateDirectoryUnavailableError(
                f"fkf state directory is unavailable: {purpose} state must stay outside the base"
            )
        inspected_anchor = physical_anchor.stat(follow_symlinks=False)
        descriptor = os.open(physical_anchor, _DIRECTORY_FLAGS)
    except OSError as error:
        raise StateDirectoryUnavailableError(
            f"fkf state directory is unavailable: open state parent {anchor}: {error}"
        ) from error
    try:
        opened_anchor = os.fstat(descriptor)
        if (inspected_anchor.st_dev, inspected_anchor.st_ino) != (opened_anchor.st_dev, opened_anchor.st_ino):
            raise StateDirectoryUnavailableError(
                f"fkf state directory is unavailable: {anchor} changed before it was opened"
            )
        current_path = anchor / namespace.name
        next_descriptor = _open_or_create_private_child(descriptor, namespace.name, current_path)
        os.close(descriptor)
        descriptor = next_descriptor
        current_path /= subtree
        next_descriptor = _open_or_create_private_child(descriptor, subtree, current_path)
        os.close(descriptor)
        descriptor = next_descriptor
        if child:
            current_path /= child
            next_descriptor = _open_or_create_private_child(descriptor, child, current_path)
            os.close(descriptor)
            descriptor = next_descriptor
        try:
            current = current_path.stat(follow_symlinks=False)
        except OSError as error:
            raise StateDirectoryUnavailableError(
                f"fkf state directory is unavailable: inspect {current_path}: {error}"
            ) from error
        opened = os.fstat(descriptor)
        if (current.st_dev, current.st_ino) != (opened.st_dev, opened.st_ino):
            raise StateDirectoryUnavailableError(
                f"fkf state directory is unavailable: {current_path} changed before it was opened"
            )
        return current_path, descriptor
    except BaseException:
        os.close(descriptor)
        raise


def ensure_private_state_directory(
    root: str | os.PathLike[str],
    subtree: str,
    *,
    purpose: str,
    child: str = "",
) -> Path:
    """Create owner-only real state directories after validating their base boundary."""
    directory, descriptor = _ensure_private_state_directory_open(
        root,
        subtree,
        purpose=purpose,
        child=child,
    )
    os.close(descriptor)
    return directory


def _acquire_base_descriptor(root: str | os.PathLike[str], identity: Path) -> int | None:
    try:
        inspected = identity.stat(follow_symlinks=False)
    except FileNotFoundError:
        # `fkf init` locks a not-yet-created target through the persistent
        # state-file key; established bases additionally get an inode lock.
        return None
    try:
        descriptor = os.open(identity, _DIRECTORY_FLAGS)
    except OSError as error:
        raise StateDirectoryUnavailableError(f"base writer lock is unavailable for {root}: {error}") from error
    try:
        opened = os.fstat(descriptor)
        if not stat.S_ISDIR(opened.st_mode) or (inspected.st_dev, inspected.st_ino) != (
            opened.st_dev,
            opened.st_ino,
        ):
            raise StateDirectoryUnavailableError(
                f"base writer lock is unavailable: {root} changed before it was opened"
            )
        try:
            fcntl.flock(descriptor, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError as error:
            raise BaseBusyError(
                f"base has an active writer for {root}; retry after the other fkf command finishes"
            ) from error
        except OSError as error:
            raise StateDirectoryUnavailableError(f"base writer lock is unavailable for {root}: {error}") from error
        return descriptor
    except BaseException:
        os.close(descriptor)
        raise


@dataclass(slots=True)
class WriterLock:
    """One process's exclusive advisory lock for a physical base."""

    _descriptor: int | None
    _base_descriptor: int | None

    @classmethod
    def acquire(cls, root: str | os.PathLike[str]) -> Self:
        """Take the base's one non-blocking persistent-inode lock."""
        identity = resolve_physical_path(root)
        base_descriptor = _acquire_base_descriptor(root, identity)
        try:
            directory, directory_descriptor = _ensure_private_state_directory_open(
                root,
                "locks",
                purpose="writer-lock",
            )
            digest = hashlib.sha256(os.fsencode(identity)).hexdigest()
            lock_path = directory / f"{digest}.lock"
            try:
                descriptor = os.open(
                    lock_path.name,
                    os.O_CREAT | os.O_RDWR | getattr(os, "O_CLOEXEC", 0) | getattr(os, "O_NOFOLLOW", 0),
                    BASE_FILE_MODE,
                    dir_fd=directory_descriptor,
                )
            except OSError as error:
                raise StateDirectoryUnavailableError(
                    f"fkf state directory is unavailable: open writer lock {lock_path}: {error}"
                ) from error
            finally:
                os.close(directory_descriptor)
            try:
                lock_info = os.fstat(descriptor)
                if not stat.S_ISREG(lock_info.st_mode) or lock_info.st_nlink != 1:
                    raise StateDirectoryUnavailableError(
                        f"fkf state directory is unavailable: writer lock {lock_path} must be a single-link regular file"
                    )
                os.fchmod(descriptor, BASE_FILE_MODE)
                try:
                    fcntl.flock(descriptor, fcntl.LOCK_EX | fcntl.LOCK_NB)
                except BlockingIOError as error:
                    raise BaseBusyError(
                        f"base has an active writer for {root}; retry after the other fkf command finishes"
                    ) from error
            except BaseException:
                os.close(descriptor)
                raise
        except BaseException:
            if base_descriptor is not None:
                fcntl.flock(base_descriptor, fcntl.LOCK_UN)
                os.close(base_descriptor)
            raise
        return cls(descriptor, base_descriptor)

    def close(self) -> None:
        """Release this lock; repeated calls are harmless."""
        descriptor = self._descriptor
        if descriptor is None:
            return
        self._descriptor = None
        try:
            fcntl.flock(descriptor, fcntl.LOCK_UN)
        finally:
            os.close(descriptor)
        base_descriptor = self._base_descriptor
        if base_descriptor is not None:
            self._base_descriptor = None
            try:
                fcntl.flock(base_descriptor, fcntl.LOCK_UN)
            finally:
                os.close(base_descriptor)

    def __enter__(self) -> Self:
        return self

    def __exit__(
        self,
        _exception_type: type[BaseException] | None,
        _exception: BaseException | None,
        _traceback: TracebackType | None,
    ) -> None:
        self.close()


__all__ = [
    "BaseBusyError",
    "StateDirectoryUnavailableError",
    "WriterLock",
    "ensure_private_state_directory",
    "private_state_directory",
    "state_dir",
]
