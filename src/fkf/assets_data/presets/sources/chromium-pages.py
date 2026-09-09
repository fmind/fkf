#!/usr/bin/env python3
"""Collect privacy-projected Chromium history from consistent SQLite snapshots."""

from __future__ import annotations

import json
import os
import re
import sqlite3
import stat
import sys
import tempfile
import unicodedata
from collections.abc import Iterator
from datetime import UTC, datetime
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
HTTPS_URL = re.compile(r"^https://(?P<authority>[^/?#]+)(?P<path>/[^?#]*)?(?:[?#].*)?$")
AUTHORITY = re.compile(r"^(?:[A-Za-z0-9.-]+|\[[0-9A-Fa-f:.]+\])(?::[0-9]+)?$")
# Bound both filesystem discovery and repeated per-profile snapshot work.
MAX_PROFILES = 64
MAX_LOCAL_STATE_BYTES = 4 << 20
# This bounds each live main/WAL snapshot and its on-disk backup, while the independent
# 64 MiB final-output ceiling bounds the disk-backed row spool across all profiles.
MAX_HISTORY_BYTES = 512 << 20
MAX_OUTPUT_BYTES = 64 << 20


class ChromiumInputError(Exception):
    """A local browser input or aggregate result crossed a declared safety boundary."""


def file_identity(value: os.stat_result) -> tuple[int, int, int]:
    return value.st_dev, value.st_ino, stat.S_IFMT(value.st_mode)


def open_directory(
    path: str | Path,
    parent: int | None,
    label_name: str,
    *,
    missing_ok: bool = False,
) -> int | None:
    """Open one real directory without following the selected component."""
    try:
        inspected = os.stat(path, dir_fd=parent, follow_symlinks=False)
    except FileNotFoundError:
        if missing_ok:
            return None
        raise ChromiumInputError(f"{label_name} profile path is missing or linked") from None
    except OSError as error:
        raise ChromiumInputError(f"{label_name} profile path is unreadable or linked") from error
    if not stat.S_ISDIR(inspected.st_mode):
        raise ChromiumInputError(f"{label_name} profile path is missing or linked")
    flags = os.O_RDONLY | getattr(os, "O_CLOEXEC", 0) | getattr(os, "O_DIRECTORY", 0) | getattr(os, "O_NOFOLLOW", 0)
    try:
        descriptor = os.open(path, flags, dir_fd=parent)
    except FileNotFoundError:
        if missing_ok:
            return None
        raise ChromiumInputError(f"{label_name} profile path is missing or linked") from None
    except OSError as error:
        raise ChromiumInputError(f"{label_name} profile path is unreadable or linked") from error
    try:
        opened = os.fstat(descriptor)
    except OSError as error:
        os.close(descriptor)
        raise ChromiumInputError(f"{label_name} profile path is unreadable or linked") from error
    if file_identity(inspected) != file_identity(opened):
        os.close(descriptor)
        raise ChromiumInputError(f"{label_name} profile path changed while it was being opened")
    return descriptor


def open_chain(
    home: Path,
    path: Path,
    label_name: str,
    *,
    missing_ok: bool = False,
) -> int | None:
    """Open a directory below HOME while retaining only its leaf descriptor."""
    try:
        relative = path.relative_to(home)
    except ValueError as error:  # pragma: no cover - callers construct paths below home.
        raise ChromiumInputError(f"{label_name} input is outside HOME") from error
    directory = open_directory(home, None, label_name)
    if directory is None:  # pragma: no cover - HOME is required.
        raise ChromiumInputError(f"{label_name} profile path is missing or linked")
    try:
        for component in relative.parts:
            child = open_directory(component, directory, label_name, missing_ok=missing_ok)
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


def open_regular_at(
    directory: int,
    name: str,
    label_name: str,
    *,
    missing_ok: bool = False,
) -> tuple[int, os.stat_result] | None:
    """Bind one regular leaf below an already validated directory."""
    try:
        inspected = os.stat(name, dir_fd=directory, follow_symlinks=False)
    except FileNotFoundError:
        if missing_ok:
            return None
        raise ChromiumInputError(f"{label_name} input is not a regular file") from None
    except OSError as error:
        raise ChromiumInputError(f"{label_name} input is not a regular file") from error
    if not stat.S_ISREG(inspected.st_mode):
        raise ChromiumInputError(f"{label_name} input is not a regular file")
    flags = os.O_RDONLY | getattr(os, "O_CLOEXEC", 0) | getattr(os, "O_NOFOLLOW", 0)
    try:
        descriptor = os.open(name, flags, dir_fd=directory)
    except OSError as error:
        raise ChromiumInputError(f"{label_name} input is not a regular file") from error
    try:
        opened = os.fstat(descriptor)
    except OSError:
        os.close(descriptor)
        raise
    if file_identity(inspected) != file_identity(opened):
        os.close(descriptor)
        raise ChromiumInputError(f"{label_name} input changed while it was being opened")
    return descriptor, opened


def open_regular(
    home: Path,
    path: Path,
    label_name: str,
    *,
    missing_ok: bool = False,
) -> tuple[int, os.stat_result] | None:
    """Bind one regular file through a no-follow directory chain."""
    directory = open_chain(home, path.parent, label_name, missing_ok=missing_ok)
    if directory is None:
        return None
    try:
        return open_regular_at(directory, path.name, label_name, missing_ok=missing_ok)
    finally:
        os.close(directory)


def regular_file(
    home: Path,
    path: Path,
    label_name: str,
    *,
    missing_ok: bool = False,
) -> os.stat_result | None:
    """Validate one regular file without retaining its descriptor."""
    opened = open_regular(home, path, label_name, missing_ok=missing_ok)
    if opened is None:
        return None
    descriptor, metadata = opened
    os.close(descriptor)
    return metadata


def read_regular(home: Path, path: Path, limit: int, label_name: str) -> bytes:
    """Bind one regular file and read at most its declared ceiling plus one byte."""
    opened = open_regular(home, path, label_name)
    if opened is None:  # pragma: no cover - missing_ok is false.
        raise ChromiumInputError(f"{label_name} input is not a regular file")
    descriptor, opened_status = opened
    try:
        content = bytearray()
        while len(content) <= limit:
            chunk = os.read(descriptor, limit + 1 - len(content))
            if not chunk:
                break
            content.extend(chunk)
        after = os.fstat(descriptor)
    finally:
        os.close(descriptor)
    if len(content) > limit:
        raise ChromiumInputError(f"{label_name} input exceeds {limit} bytes")
    if (after.st_size, after.st_mtime_ns) != (opened_status.st_size, opened_status.st_mtime_ns):
        raise ChromiumInputError(f"{label_name} input changed while it was being read")
    return bytes(content)


def profiles(home: Path) -> list[Path]:
    found: set[Path] = set()
    for root in ROOTS:
        browser = open_chain(home, home / root, "History", missing_ok=True)
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
                                raise ChromiumInputError("History profile path is missing or linked")
                            continue
                        if not stat.S_ISDIR(inspected.st_mode):
                            continue
                        profile = open_directory(entry.name, browser, "History")
                        if profile is None:  # pragma: no cover - missing_ok is false.
                            raise ChromiumInputError("History profile path is missing or linked")
                        try:
                            opened = open_regular_at(profile, "History", "History", missing_ok=True)
                            if opened is None:
                                continue
                            descriptor, _status = opened
                            os.close(descriptor)
                        finally:
                            os.close(profile)
                        found.add(home / root / entry.name / "History")
                        if len(found) > MAX_PROFILES:
                            raise ChromiumInputError(f"profile count exceeds {MAX_PROFILES}")
            except OSError as error:
                raise ChromiumInputError("History profile path is unreadable or linked") from error
        finally:
            os.close(browser)
    return sorted(found)


def label(database: Path) -> str:
    return f"{database.parent.parent.name}/{database.parent.name}"


def account(home: Path, database: Path) -> str:
    state = database.parent.parent / "Local State"
    try:
        value = json.loads(read_regular(home, state, MAX_LOCAL_STATE_BYTES, "Local State"))
        name = value["profile"]["info_cache"][database.parent.name].get("user_name", "-")
        return name if isinstance(name, str) else "-"
    except (
        ChromiumInputError,
        OSError,
        RecursionError,
        KeyError,
        TypeError,
        UnicodeError,
        json.JSONDecodeError,
    ):
        return "-"


def profile_output(home: Path, databases: list[Path]) -> bytes:
    """Encode the complete bounded profile inventory before exposing stdout."""
    output = bytearray()
    for database in databases:
        line = f"{label(database)}\t{account(home, database)}\n".encode()
        if len(output) + len(line) > MAX_OUTPUT_BYTES:
            raise ChromiumInputError(f"aggregate output exceeds {MAX_OUTPUT_BYTES} bytes")
        output.extend(line)
    return bytes(output)


def instant(value: str) -> int:
    parsed = datetime.fromisoformat(value)
    if parsed.tzinfo is None:
        raise ValueError
    return int(parsed.astimezone(UTC).timestamp())


def safe_url(value: Any) -> str | None:
    if not isinstance(value, str):
        return None
    match = HTTPS_URL.fullmatch(value)
    if match is None:
        return None
    authority = match["authority"].rsplit("@", 1)[-1]
    if AUTHORITY.fullmatch(authority) is None:
        return None
    return f"https://{authority}{match['path'] or ''}"


def visit_title(value: Any, url: str | None, visit: Any) -> str:
    """An untitled visit is still evidence; never fall back to the raw private URL."""
    title = value if isinstance(value, str) else ""
    title = "".join(
        "" if unicodedata.category(character) == "Cf" else " " if unicodedata.category(character) == "Cc" else character
        for character in title
    )
    return " ".join(title.split()) or (f"Visit {url}" if url else f"Browser visit {visit}")


def history_input_size(directory: int, database_name: str) -> int:
    """Measure the main database and every SQLite sidecar that a read may consult."""
    total = 0
    for suffix in ("", "-wal", "-shm", "-journal"):
        opened = open_regular_at(directory, database_name + suffix, "History", missing_ok=bool(suffix))
        if opened is None:
            continue
        descriptor, metadata = opened
        try:
            total += metadata.st_size
        finally:
            os.close(descriptor)
    if total > MAX_HISTORY_BYTES:
        raise ChromiumInputError(f"History input exceeds {MAX_HISTORY_BYTES} bytes")
    return total


def snapshot_source(directory: int, database_name: str) -> sqlite3.Connection:
    """Open SQLite relative to one retained profile directory and pin its read generation."""
    flags = os.O_RDONLY | getattr(os, "O_CLOEXEC", 0) | getattr(os, "O_DIRECTORY", 0)
    previous = os.open(".", flags)
    try:
        # The helper is single-threaded, so process-wide fchdir is the portable way to
        # make SQLite resolve both the database and its sidecars from the retained fd.
        os.fchdir(directory)
        for attempt in range(2):
            source: sqlite3.Connection | None = None
            try:
                source = sqlite3.connect(f"file:{database_name}?mode=ro", uri=True)
                source.execute("pragma query_only = on")
                source.execute("begin")
                # Opening the schema also binds any live WAL before the process cwd is restored.
                source.execute("select count(*) from sqlite_schema").fetchone()
            except sqlite3.Error:
                if source is not None:
                    source.close()
                if attempt:
                    raise
                # A profile rename during connect can stale SQLite's canonical path before
                # it opens the WAL. One retry remains anchored to the retained directory.
                continue
            return source
        raise AssertionError("unreachable")  # pragma: no cover
    finally:
        try:
            os.fchdir(previous)
        finally:
            os.close(previous)


def snapshot(home: Path, database: Path, target: Path) -> sqlite3.Connection:
    """Pin the live WAL generation and copy one bounded, consistent snapshot to disk."""
    directory = open_chain(home, database.parent, "History")
    if directory is None:  # pragma: no cover - missing_ok is false.
        raise ChromiumInputError("History profile path is missing or linked")
    opened = open_regular_at(directory, database.name, "History")
    if opened is None:  # pragma: no cover - missing_ok is false.
        os.close(directory)
        raise ChromiumInputError("History input is not a regular file")
    descriptor, before = opened
    try:
        history_input_size(directory, database.name)
        source = snapshot_source(directory, database.name)
        try:
            current_opened = open_regular_at(directory, database.name, "History")
            if current_opened is None:  # pragma: no cover - missing_ok is false.
                raise ChromiumInputError("History input is not a regular file")
            current_descriptor, current = current_opened
            os.close(current_descriptor)
            if (current.st_dev, current.st_ino) != (before.st_dev, before.st_ino):
                raise ChromiumInputError("History input changed while it was being opened")
            history_input_size(directory, database.name)
            copied = sqlite3.connect(target)
            try:
                page_size = int(source.execute("pragma page_size").fetchone()[0])
                page_count = int(source.execute("pragma page_count").fetchone()[0])
                if page_size * page_count > MAX_HISTORY_BYTES:
                    raise ChromiumInputError(f"History snapshot exceeds {MAX_HISTORY_BYTES} bytes")
                source.backup(copied)
                copied_size = target.stat().st_size
                if copied_size > MAX_HISTORY_BYTES:
                    raise ChromiumInputError(f"History snapshot exceeds {MAX_HISTORY_BYTES} bytes")
            except BaseException:
                copied.close()
                raise
            return copied
        finally:
            source.close()
    finally:
        os.close(descriptor)
        os.close(directory)


def rows(connection: sqlite3.Connection, start: int, end: int) -> Iterator[dict[str, Any]]:
    """Yield one row at a time from a previously bounded on-disk snapshot."""
    connection.row_factory = sqlite3.Row
    values = connection.execute(
        """select v.id as visit,
                      u.url as url,
                      u.title as title,
                      case when v.originator_cache_guid = '' then 'local' else 'synced' end as origin,
                      v.visit_duration / 1000000 as seconds,
                      v.transition & 255 as transition,
                      u.visit_count as visit_count,
                      u.typed_count as typed_count,
                      v.from_visit as from_visit,
                      strftime('%Y-%m-%dT%H:%M:%SZ', v.visit_time/1000000 - 11644473600, 'unixepoch') as time
                 from visits v join urls u on u.id = v.url
                where v.visit_time/1000000 - 11644473600 >= ?
                  and v.visit_time/1000000 - 11644473600 < ?
                order by v.visit_time""",
        (start, end),
    )
    for value in values:
        yield dict(value)


def write_output(records: sqlite3.Connection, target: Path) -> None:
    """Materialize the complete ordered array before exposing any byte on stdout."""
    with target.open("wb") as output:
        output.write(b"[")
        first = True
        for (payload,) in records.execute("select payload from records order by time, uid, rowid"):
            if not first:
                output.write(b",")
            output.write(payload)
            first = False
        output.write(b"]\n")


def main(arguments: list[str]) -> int:
    home = Path(os.environ.get("HOME", ""))
    try:
        databases = profiles(home)
    except ChromiumInputError as error:
        sys.stderr.write(f"chromium-pages.py: {error}\n")
        return 1
    if not databases:
        sys.stderr.write("chromium-pages.py: no Chromium-family profile found\n")
        return 1
    if arguments[:1] == ["--profiles"]:
        try:
            output = profile_output(home, databases)
        except (ChromiumInputError, UnicodeError) as error:
            sys.stderr.write(f"chromium-pages.py: {error}\n")
            return 1
        sys.stdout.buffer.write(output)
        return 0
    if len(arguments) < 2:
        sys.stderr.write("usage: chromium-pages.py <start> <end> [profile...]\n")
        return 2
    try:
        start, end = instant(arguments[0]), instant(arguments[1])
    except ValueError:
        sys.stderr.write("chromium-pages.py: start and end must be RFC3339 timestamps\n")
        return 2
    if arguments[2:]:
        if len(arguments[2:]) > MAX_PROFILES:
            sys.stderr.write(f"chromium-pages.py: selected profile count exceeds {MAX_PROFILES}\n")
            return 1
        selected: list[Path] = []
        by_label = {label(database): database for database in databases}
        for wanted in arguments[2:]:
            if wanted not in by_label:
                sys.stderr.write(
                    f"chromium-pages.py: no profile labelled '{wanted}'; run 'chromium-pages.py --profiles'\n"
                )
                return 1
            selected.append(by_label[wanted])
        databases = selected
    try:
        temporary_parent = os.environ.get("TMPDIR") or None
        with tempfile.TemporaryDirectory(prefix="fkf-chromium-pages-", dir=temporary_parent) as directory:
            root = Path(directory)
            staged = sqlite3.connect(root / "records.sqlite")
            try:
                staged.execute("create table records (time text, uid text, payload blob)")
                output_size = len(b"[]\n")
                record_count = 0
                for index, database in enumerate(databases):
                    profile = label(database)
                    snapshot_path = root / f"history-{index}.sqlite"
                    copied = snapshot(home, database, snapshot_path)
                    try:
                        for record in rows(copied, start, end):
                            url = safe_url(record.get("url"))
                            record.update(
                                {
                                    "url": url,
                                    "title": visit_title(record.get("title"), url, record["visit"]),
                                    "profile": profile,
                                    "uid": f"{profile}~{record['visit']}",
                                }
                            )
                            payload = json.dumps(record, ensure_ascii=False, separators=(",", ":")).encode()
                            output_size += len(payload) + int(record_count > 0)
                            if output_size > MAX_OUTPUT_BYTES:
                                raise ChromiumInputError(f"aggregate output exceeds {MAX_OUTPUT_BYTES} bytes")
                            staged.execute(
                                "insert into records values (?, ?, ?)",
                                (record["time"], record["uid"], payload),
                            )
                            record_count += 1
                    finally:
                        try:
                            copied.close()
                        finally:
                            snapshot_path.unlink(missing_ok=True)
                staged.commit()
                output = root / "output.json"
                write_output(staged, output)
                if output.stat().st_size != output_size:
                    raise ChromiumInputError("aggregate output size changed while it was encoded")
            finally:
                staged.close()
            with output.open("rb") as stream:
                while chunk := stream.read(1 << 20):
                    sys.stdout.buffer.write(chunk)
    except (ChromiumInputError, OSError, sqlite3.Error, TypeError, ValueError) as error:
        sys.stderr.write(f"chromium-pages.py: {error}\n")
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
