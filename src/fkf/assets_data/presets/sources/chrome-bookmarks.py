#!/usr/bin/env python3
"""Collect stable bookmark metadata from Chromium-family browser profiles."""

from __future__ import annotations

import json
import os
import stat
import sys
from collections.abc import Iterator
from datetime import UTC, datetime, timedelta
from pathlib import Path
from typing import Any

ROOTS = (
    ".config/chromium",
    ".config/google-chrome",
    ".config/google-chrome-beta",
    ".config/microsoft-edge",
    ".config/BraveSoftware/Brave-Browser",
    ".config/vivaldi",
    "Library/Application Support/Chromium",
    "Library/Application Support/Google/Chrome",
    "Library/Application Support/Microsoft Edge",
    "Library/Application Support/BraveSoftware/Brave-Browser",
    "Library/Application Support/Vivaldi",
)
IGNORED_SINGLETON_LINKS = frozenset({"SingletonCookie", "SingletonLock", "SingletonSocket"})
WEBKIT_EPOCH = datetime(1601, 1, 1, tzinfo=UTC)
# Sixty-four profiles is well above ordinary browser use while placing a hard
# ceiling on discovery and per-profile input parsing.
MAX_PROFILES = 64
MAX_BOOKMARKS_BYTES = 16 << 20
MAX_BOOKMARK_DEPTH = 64
MAX_BOOKMARK_RECORDS = 100_000
MAX_OUTPUT_BYTES = 64 << 20


class BookmarkInputError(Exception):
    """A discovered bookmark file cannot be consumed within the helper contract."""


def directory_identity(value: os.stat_result) -> tuple[int, int, int]:
    return value.st_dev, value.st_ino, stat.S_IFMT(value.st_mode)


def open_directory(
    path: str | Path,
    parent: int | None = None,
    *,
    missing_ok: bool = False,
) -> int | None:
    """Open one real directory without following the selected component."""
    try:
        inspected = os.stat(path, dir_fd=parent, follow_symlinks=False)
    except FileNotFoundError:
        if missing_ok:
            return None
        raise BookmarkInputError("Bookmarks profile path is missing or linked") from None
    except OSError as error:
        raise BookmarkInputError("Bookmarks profile path is unreadable or linked") from error
    if not stat.S_ISDIR(inspected.st_mode):
        raise BookmarkInputError("Bookmarks profile path is missing or linked")
    flags = os.O_RDONLY | getattr(os, "O_CLOEXEC", 0) | getattr(os, "O_DIRECTORY", 0) | getattr(os, "O_NOFOLLOW", 0)
    try:
        descriptor = os.open(path, flags, dir_fd=parent)
    except FileNotFoundError:
        if missing_ok:
            return None
        raise BookmarkInputError("Bookmarks profile path is missing or linked") from None
    except OSError as error:
        raise BookmarkInputError("Bookmarks profile path is unreadable or linked") from error
    try:
        opened = os.fstat(descriptor)
    except OSError as error:
        os.close(descriptor)
        raise BookmarkInputError("Bookmarks profile path is unreadable or linked") from error
    if directory_identity(inspected) != directory_identity(opened):
        os.close(descriptor)
        raise BookmarkInputError("Bookmarks profile path changed while it was being opened")
    return descriptor


def open_chain(home: Path, components: tuple[str, ...], *, missing_ok: bool = False) -> int | None:
    """Open a directory chain relative to HOME while retaining only its leaf."""
    directory = open_directory(home)
    if directory is None:  # pragma: no cover - HOME is required.
        raise BookmarkInputError("Bookmarks profile path is missing or linked")
    try:
        for component in components:
            child = open_directory(component, directory, missing_ok=missing_ok)
            if child is None:
                os.close(directory)
                return None
            previous = directory
            directory = child
            os.close(previous)
    except BaseException:
        os.close(directory)
        raise
    return directory


def open_bookmarks_at(
    directory: int,
    *,
    missing_ok: bool = False,
) -> tuple[int, os.stat_result] | None:
    """Bind one Bookmarks leaf below an already validated profile directory."""
    try:
        inspected = os.stat("Bookmarks", dir_fd=directory, follow_symlinks=False)
    except FileNotFoundError:
        if missing_ok:
            return None
        raise BookmarkInputError("Bookmarks input is not a regular file") from None
    except OSError as error:
        raise BookmarkInputError("Bookmarks input is not a regular file") from error
    if not stat.S_ISREG(inspected.st_mode):
        raise BookmarkInputError("Bookmarks input is not a regular file")
    flags = os.O_RDONLY | getattr(os, "O_CLOEXEC", 0) | getattr(os, "O_NOFOLLOW", 0)
    try:
        descriptor = os.open("Bookmarks", flags, dir_fd=directory)
    except OSError as error:
        raise BookmarkInputError("Bookmarks input is not a regular file") from error
    try:
        opened = os.fstat(descriptor)
    except OSError:
        os.close(descriptor)
        raise
    if directory_identity(inspected) != directory_identity(opened):
        os.close(descriptor)
        raise BookmarkInputError("Bookmarks input changed while it was being opened")
    return descriptor, opened


def open_bookmarks(home: Path, path: Path) -> tuple[int, os.stat_result]:
    """Bind a regular Bookmarks file through a no-follow directory chain."""
    try:
        relative = path.relative_to(home)
    except ValueError as error:  # pragma: no cover - discovery always starts below home.
        raise BookmarkInputError("Bookmarks input is outside HOME") from error
    directory = open_chain(home, relative.parts[:-1])
    if directory is None:  # pragma: no cover - missing_ok is false.
        raise BookmarkInputError("Bookmarks profile path is missing or linked")
    try:
        opened = open_bookmarks_at(directory)
        if opened is None:  # pragma: no cover - missing_ok is false.
            raise BookmarkInputError("Bookmarks input is not a regular file")
        return opened
    finally:
        os.close(directory)


def read_bookmarks(home: Path, path: Path) -> bytes:
    """Bind one regular file and read no more than the 16 MiB ceiling plus one byte."""
    descriptor, opened = open_bookmarks(home, path)
    try:
        content = bytearray()
        while len(content) <= MAX_BOOKMARKS_BYTES:
            chunk = os.read(descriptor, MAX_BOOKMARKS_BYTES + 1 - len(content))
            if not chunk:
                break
            content.extend(chunk)
        after = os.fstat(descriptor)
    finally:
        os.close(descriptor)
    if len(content) > MAX_BOOKMARKS_BYTES:
        raise BookmarkInputError(f"Bookmarks input exceeds {MAX_BOOKMARKS_BYTES} bytes")
    if (after.st_size, after.st_mtime_ns) != (opened.st_size, opened.st_mtime_ns):
        raise BookmarkInputError("Bookmarks input changed while it was being read")
    return bytes(content)


def bookmark_files(home: Path) -> list[Path]:
    """Discover at most the declared number of Chromium-family profiles."""
    found: set[Path] = set()
    for root in ROOTS:
        browser = open_chain(home, Path(root).parts, missing_ok=True)
        if browser is None:
            continue
        try:
            try:
                with os.scandir(browser) as entries:
                    for entry in entries:
                        inspected = entry.stat(follow_symlinks=False)
                        if stat.S_ISLNK(inspected.st_mode):
                            # Chromium owns these root-level coordination links. Every other
                            # linked name could stand in for a profile and is rejected unopened.
                            if entry.name not in IGNORED_SINGLETON_LINKS:
                                raise BookmarkInputError("Bookmarks profile path is missing or linked")
                            continue
                        if not stat.S_ISDIR(inspected.st_mode):
                            continue
                        profile = open_directory(entry.name, browser)
                        if profile is None:  # pragma: no cover - missing_ok is false.
                            raise BookmarkInputError("Bookmarks profile path is missing or linked")
                        try:
                            opened = open_bookmarks_at(profile, missing_ok=True)
                            if opened is None:
                                continue
                            descriptor, _status = opened
                            os.close(descriptor)
                        finally:
                            os.close(profile)
                        found.add(home / root / entry.name / "Bookmarks")
                        if len(found) > MAX_PROFILES:
                            raise BookmarkInputError(f"profile count exceeds {MAX_PROFILES}")
            except OSError as error:
                raise BookmarkInputError("Bookmarks profile path is unreadable or linked") from error
        finally:
            os.close(browser)
    return sorted(found)


def nodes(value: Any, folders: tuple[str, ...]) -> Iterator[dict[str, Any]]:
    """Yield bookmark records without materializing a complete subtree."""
    if not isinstance(value, dict):
        return
    kind = value.get("type")
    if kind == "url":
        guid = value.get("guid")
        if not isinstance(guid, str) or not guid:
            raise ValueError("bookmark has no stable guid")
        added = WEBKIT_EPOCH + timedelta(microseconds=int(value["date_added"]))
        yield {
            "guid": guid,
            "title": value.get("name"),
            "url": value.get("url"),
            "folder": "/".join(folders),
            "added": added.replace(microsecond=0).isoformat().replace("+00:00", "Z"),
        }
        return
    if kind != "folder":
        return
    name = value.get("name")
    path = (*folders, str(name))
    if len(path) > MAX_BOOKMARK_DEPTH:
        raise BookmarkInputError(f"bookmark folder depth exceeds {MAX_BOOKMARK_DEPTH}")
    children = value.get("children") or []
    if not isinstance(children, list):
        raise TypeError("bookmark folder children must be an array")
    for child in children:
        yield from nodes(child, path)


def main(arguments: list[str]) -> int:
    if arguments[:1] in (["--version"], ["-v"]):
        sys.stdout.write("chrome-bookmarks.py (fkf preset helper)\n")
        return 0
    home = Path(os.environ.get("HOME", ""))
    try:
        files = bookmark_files(home)
        if not files:
            raise BookmarkInputError("no Chromium-family bookmark profile found")
        records: list[dict[str, Any]] = []
        output_size = len(b"[]\n")
        for path in files:
            profile = f"{path.parent.parent.name}/{path.parent.name}"
            document = json.loads(read_bookmarks(home, path))
            roots = document.get("roots", {})
            if not isinstance(roots, dict):
                raise TypeError("bookmark roots must be an object")
            for root in roots.values():
                for record in nodes(root, ()):
                    record.update({"profile": profile, "uid": f"{profile}~{record['guid']}"})
                    if len(records) >= MAX_BOOKMARK_RECORDS:
                        raise BookmarkInputError(f"bookmark record count exceeds {MAX_BOOKMARK_RECORDS}")
                    encoded = json.dumps(record, ensure_ascii=False, separators=(",", ":")).encode()
                    output_size += len(encoded) + int(bool(records))
                    if output_size > MAX_OUTPUT_BYTES:
                        raise BookmarkInputError(f"aggregate output exceeds {MAX_OUTPUT_BYTES} bytes")
                    records.append(record)
        records.sort(key=lambda record: tuple(str(record[key]) for key in ("profile", "folder", "title", "uid")))
        output = (json.dumps(records, ensure_ascii=False, separators=(",", ":")) + "\n").encode()
        if len(output) != output_size:
            raise BookmarkInputError("aggregate output size changed while it was encoded")
    except RecursionError:
        sys.stderr.write("chrome-bookmarks.py: bookmark nesting exceeds the safe limit\n")
        return 1
    except (
        BookmarkInputError,
        OSError,
        OverflowError,
        UnicodeError,
        TypeError,
        ValueError,
        json.JSONDecodeError,
    ) as error:
        sys.stderr.write(f"chrome-bookmarks.py: {error}\n")
        return 1
    sys.stdout.buffer.write(output)
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
