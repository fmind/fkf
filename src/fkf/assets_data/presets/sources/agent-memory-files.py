#!/usr/bin/env python3
"""Project bounded metadata for coding-agent memory files touched in a UTC window."""

from __future__ import annotations

import json
import os
import re
import stat
import sys
from dataclasses import dataclass
from datetime import UTC, datetime
from pathlib import Path
from typing import Any

PREVIEW_BYTES = 65_536
PREVIEW_LINES = 256
READ_BYTES = 64 << 10
MAX_FILES = 8192
MAX_RECORDS = 8192
MAX_OUTPUT_BYTES = 64 << 20
type PathPart = str | bytes | os.PathLike[str] | os.PathLike[bytes]


@dataclass(frozen=True)
class MemoryFile:
    path: Path
    inspected: os.stat_result


def instant(value: str) -> datetime:
    parsed = datetime.fromisoformat(value)
    if parsed.tzinfo is None:
        raise ValueError("timestamp must include an offset")
    return parsed.astimezone(UTC)


def fingerprint(value: os.stat_result) -> tuple[int, int, int, int, int, int]:
    return value.st_dev, value.st_ino, value.st_mode, value.st_size, value.st_mtime_ns, value.st_ctime_ns


def open_directory(path: PathPart, parent: int | None = None, expected: os.stat_result | None = None) -> int:
    inspected = os.stat(path, dir_fd=parent, follow_symlinks=False)
    if not stat.S_ISDIR(inspected.st_mode):
        raise RuntimeError("memory root is missing or linked")
    if expected is not None and fingerprint(expected) != fingerprint(inspected):
        raise RuntimeError("memory root changed while it was being opened")
    flags = os.O_RDONLY | getattr(os, "O_CLOEXEC", 0) | getattr(os, "O_DIRECTORY", 0) | getattr(os, "O_NOFOLLOW", 0)
    descriptor = os.open(path, flags, dir_fd=parent)
    if fingerprint(inspected) != fingerprint(os.fstat(descriptor)):
        os.close(descriptor)
        raise RuntimeError("memory root changed while it was being opened")
    return descriptor


def open_chain(home: Path, components: tuple[str, ...]) -> int:
    descriptor = open_directory(home)
    try:
        for component in components:
            child = open_directory(component, descriptor)
            os.close(descriptor)
            descriptor = child
    except BaseException:
        os.close(descriptor)
        raise
    return descriptor


def read_prefix(home: Path, candidate: MemoryFile) -> bytes:
    relative = candidate.path.relative_to(home)
    parent = open_chain(home, relative.parts[:-1])
    descriptor = -1
    try:
        name = relative.parts[-1]
        inspected = os.stat(name, dir_fd=parent, follow_symlinks=False)
        if not stat.S_ISREG(inspected.st_mode):
            raise RuntimeError("memory file is missing or linked")
        if fingerprint(candidate.inspected) != fingerprint(inspected):
            raise RuntimeError("memory file changed while it was being opened")
        flags = os.O_RDONLY | getattr(os, "O_CLOEXEC", 0) | getattr(os, "O_NOFOLLOW", 0)
        descriptor = os.open(name, flags, dir_fd=parent)
        opened = os.fstat(descriptor)
        if fingerprint(inspected) != fingerprint(opened):
            raise RuntimeError("memory file changed while it was being opened")
        output = bytearray()
        while len(output) < PREVIEW_BYTES and (
            chunk := os.read(descriptor, min(READ_BYTES, PREVIEW_BYTES - len(output)))
        ):
            output.extend(chunk)
        after = os.fstat(descriptor)
        current = os.stat(name, dir_fd=parent, follow_symlinks=False)
        if fingerprint(opened) != fingerprint(after) or fingerprint(opened) != fingerprint(current):
            raise RuntimeError("memory file changed while it was being read")
        return bytes(output)
    finally:
        if descriptor >= 0:
            os.close(descriptor)
        os.close(parent)


def title_of(home: Path, candidate: MemoryFile) -> str:
    preview = read_prefix(home, candidate).decode("utf-8", errors="replace").splitlines()[:PREVIEW_LINES]
    front = bool(preview and preview[0] == "---")
    for index, line in enumerate(preview):
        if index == 0 and front:
            continue
        if front and line == "---":
            front = False
            continue
        if front and line.startswith("name: "):
            return line.removeprefix("name: ")
        if not front and line.startswith("# "):
            return line.removeprefix("# ")
    return candidate.path.stem


def clean_title(value: str) -> str:
    printable = "".join(" " if ord(character) < 32 or ord(character) == 127 else character for character in value)
    return re.sub(r"\s+", " ", printable).strip()[:160]


def allowed_file(parts: tuple[str, ...], memory_component: bool) -> bool:
    if memory_component:
        return len(parts) == 3 and parts[1] == "memory"
    return 1 <= len(parts) <= 2


def allowed_directory(parts: tuple[str, ...], memory_component: bool) -> bool:
    if memory_component:
        return len(parts) == 1 or (len(parts) == 2 and parts[1] == "memory")
    return len(parts) == 1


def scan_files(
    descriptor: int,
    root: Path,
    parts: tuple[str, ...],
    memory_component: bool,
    maximum: int,
    results: list[MemoryFile],
) -> None:
    with os.scandir(descriptor) as entries:
        for entry in entries:
            relative = (*parts, entry.name)
            inspected = entry.stat(follow_symlinks=False)
            if stat.S_ISDIR(inspected.st_mode) and allowed_directory(relative, memory_component):
                child = open_directory(entry.name, descriptor, inspected)
                try:
                    scan_files(child, root, relative, memory_component, maximum, results)
                finally:
                    os.close(child)
            elif (
                stat.S_ISREG(inspected.st_mode)
                and entry.name.endswith(".md")
                and allowed_file(relative, memory_component)
            ):
                if len(results) >= maximum:
                    raise RuntimeError(f"more than {MAX_FILES} memory files")
                results.append(MemoryFile(root.joinpath(*relative), inspected))


def files(home: Path, root: Path, *, memory_component: bool, maximum: int) -> list[MemoryFile]:
    try:
        descriptor = open_chain(home, root.relative_to(home).parts)
    except FileNotFoundError:
        return []
    results: list[MemoryFile] = []
    try:
        scan_files(descriptor, root, (), memory_component, maximum, results)
    finally:
        os.close(descriptor)
    return sorted(results, key=lambda item: os.fspath(item.path))


def main(arguments: list[str]) -> int:
    if len(arguments) != 2:
        sys.stderr.write("usage: agent-memory-files.py <start> <end>\n")
        return 2
    try:
        start, end = map(instant, arguments)
        home = Path(os.environ.get("HOME", ""))
        roots = (
            (home / ".claude" / "projects", "claude", True),
            (home / ".codex" / "memories", "codex", False),
            (home / ".gemini" / "tmp", "gemini", True),
            (home / ".grok" / "memory", "grok", False),
        )
        candidates: list[tuple[MemoryFile, str]] = []
        for root, agent, memory_component in roots:
            candidates.extend(
                (candidate, agent)
                for candidate in files(
                    home,
                    root,
                    memory_component=memory_component,
                    maximum=MAX_FILES - len(candidates),
                )
            )
        records: list[dict[str, Any]] = []
        for candidate, agent in candidates:
            modified = datetime.fromtimestamp(candidate.inspected.st_mtime, UTC)
            if not start <= modified < end:
                continue
            if len(records) >= MAX_RECORDS:
                raise RuntimeError(f"more than {MAX_RECORDS} memory records")
            records.append(
                {
                    "id": os.fspath(candidate.path),
                    "time": modified.replace(microsecond=0).isoformat().replace("+00:00", "Z"),
                    "title": clean_title(title_of(home, candidate)),
                    "agent": agent,
                }
            )
        output = (json.dumps(records, ensure_ascii=False, separators=(",", ":")) + "\n").encode()
        if len(output) > MAX_OUTPUT_BYTES:
            raise RuntimeError("output exceeds 64 MiB")
    except (OSError, RuntimeError, UnicodeError, ValueError) as error:
        sys.stderr.write(f"agent-memory-files.py: {error}\n")
        return 1
    sys.stdout.buffer.write(output)
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
