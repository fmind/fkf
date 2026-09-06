"""Bounded regular-file reads and durable atomic writes."""

from __future__ import annotations

import os
import stat
import tempfile
from contextlib import suppress
from pathlib import Path
from typing import Any, BinaryIO

from fkf.errors import OperationalError
from fkf.jsoncodec import dumps
from fkf.store import BASE_DIR_MODE, BASE_FILE_MODE, UnsafePathError


class FileTooLargeError(OperationalError):
    """A file exceeds its boundary-specific byte limit."""


def open_regular_file(path: str | os.PathLike[str]) -> BinaryIO:
    """Open and bind a regular non-symlink leaf to its inspected inode."""
    candidate = Path(path)
    try:
        before = candidate.lstat()
    except OSError as error:
        raise OSError(f"inspect {candidate}: {error}") from error
    if stat.S_ISLNK(before.st_mode) or not stat.S_ISREG(before.st_mode):
        raise UnsafePathError(f"unsafe filesystem path: {candidate} must be a regular non-symlink file")
    try:
        handle = candidate.open("rb")
    except OSError as error:
        raise OSError(f"open {candidate}: {error}") from error
    try:
        after = os.fstat(handle.fileno())
        if not stat.S_ISREG(after.st_mode) or (before.st_dev, before.st_ino) != (after.st_dev, after.st_ino):
            raise UnsafePathError(f"unsafe filesystem path: {candidate} changed before it was opened as a regular file")
    except BaseException:
        handle.close()
        raise
    return handle


def read_file_limited(path: str | os.PathLike[str], limit: int) -> bytes:
    """Read one regular file with metadata and streaming byte limits."""
    candidate = Path(path)
    if limit <= 0:
        raise ValueError(f"read {candidate}: size limit must be positive")
    try:
        with open_regular_file(candidate) as handle:
            size = os.fstat(handle.fileno()).st_size
            if size > limit:
                raise FileTooLargeError(f"file exceeds size limit: {candidate} is {size} bytes (limit {limit})")
            data = handle.read(limit + 1)
    except FileTooLargeError, UnsafePathError:
        raise
    except OSError as error:
        raise OSError(f"read {candidate}: {error}") from error
    if len(data) > limit:
        raise FileTooLargeError(f"file exceeds size limit: {candidate} grew beyond {limit} bytes")
    return data


def sync_directory(directory: str | os.PathLike[str]) -> None:
    """Persist a directory-entry mutation."""
    descriptor = os.open(directory, os.O_RDONLY | getattr(os, "O_DIRECTORY", 0))
    try:
        os.fsync(descriptor)
    finally:
        os.close(descriptor)


def atomic_write(path: str | os.PathLike[str], data: bytes, *, mode: int = BASE_FILE_MODE) -> None:
    """Durably replace a file with complete owner-controlled bytes."""
    target = Path(path)
    directory = target.parent
    directory.mkdir(mode=BASE_DIR_MODE, parents=True, exist_ok=True)
    descriptor, temporary_name = tempfile.mkstemp(prefix=f".{target.name}.", suffix=".tmp", dir=directory)
    temporary = Path(temporary_name)
    try:
        with os.fdopen(descriptor, "wb") as handle:
            handle.write(data)
            handle.flush()
            os.fchmod(handle.fileno(), mode)
            os.fsync(handle.fileno())
        temporary.replace(target)
        sync_directory(directory)
    finally:
        with suppress(FileNotFoundError):
            temporary.unlink()


def write_json(data: Any, path: str | os.PathLike[str]) -> None:
    """Encode deterministic indented UTF-8 JSON and replace one file."""
    try:
        encoded = dumps(data, indent=True, newline=True)
    except (TypeError, ValueError) as error:
        raise ValueError(f"failed to marshal JSON: {error}") from error
    atomic_write(path, encoded)


__all__ = [
    "FileTooLargeError",
    "atomic_write",
    "open_regular_file",
    "read_file_limited",
    "sync_directory",
    "write_json",
]
