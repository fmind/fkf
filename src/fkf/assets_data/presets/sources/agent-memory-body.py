#!/usr/bin/env python3
"""Print one bounded file from the reviewed coding-agent memory roots."""

from __future__ import annotations

import os
import stat
import sys
from pathlib import Path

LIMIT = 4 * 1024 * 1024
READ_BYTES = 64 << 10
type PathPart = str | bytes | os.PathLike[str] | os.PathLike[bytes]


def reviewed_root(path: Path, home: Path) -> Path | None:
    candidates = (
        (home / ".claude" / "projects", lambda parts: len(parts) == 3 and parts[1] == "memory"),
        (home / ".codex" / "memories", lambda parts: 1 <= len(parts) <= 2),
        (home / ".gemini" / "tmp", lambda parts: len(parts) == 3 and parts[1] == "memory"),
        (home / ".grok" / "memory", lambda parts: 1 <= len(parts) <= 2),
    )
    for root, shape in candidates:
        try:
            relative = path.relative_to(root)
        except ValueError:
            continue
        if path.suffix == ".md" and ".." not in relative.parts and shape(relative.parts):
            return root
    return None


def fingerprint(value: os.stat_result) -> tuple[int, int, int, int, int, int]:
    return value.st_dev, value.st_ino, value.st_mode, value.st_size, value.st_mtime_ns, value.st_ctime_ns


def open_directory(path: PathPart, parent: int | None = None) -> int:
    inspected = os.stat(path, dir_fd=parent, follow_symlinks=False)
    if not stat.S_ISDIR(inspected.st_mode):
        raise RuntimeError("memory root is missing or linked")
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


def read_body(home: Path, path: Path) -> bytes:
    relative = path.relative_to(home)
    parent = open_chain(home, relative.parts[:-1])
    descriptor = -1
    try:
        name = relative.parts[-1]
        inspected = os.stat(name, dir_fd=parent, follow_symlinks=False)
        if not stat.S_ISREG(inspected.st_mode):
            raise RuntimeError("file is absent, linked, or outside the reviewed roots")
        flags = os.O_RDONLY | getattr(os, "O_CLOEXEC", 0) | getattr(os, "O_NOFOLLOW", 0)
        descriptor = os.open(name, flags, dir_fd=parent)
        opened = os.fstat(descriptor)
        if fingerprint(inspected) != fingerprint(opened):
            raise RuntimeError("memory file changed while it was being opened")
        output = bytearray()
        while chunk := os.read(descriptor, min(READ_BYTES, LIMIT + 1 - len(output))):
            output.extend(chunk)
            if len(output) > LIMIT:
                raise RuntimeError(f"file exceeds the {LIMIT}-byte body limit")
        after = os.fstat(descriptor)
        current = os.stat(name, dir_fd=parent, follow_symlinks=False)
        if fingerprint(opened) != fingerprint(after) or fingerprint(opened) != fingerprint(current):
            raise RuntimeError("memory file changed while it was being read")
        return bytes(output)
    finally:
        if descriptor >= 0:
            os.close(descriptor)
        os.close(parent)


def main(arguments: list[str]) -> int:
    if arguments[:1] in (["--version"], ["-v"]):
        sys.stdout.write("agent-memory-body.py (fkf base helper)\n")
        return 0
    if len(arguments) != 1:
        sys.stderr.write("usage: agent-memory-body.py <absolute-memory-file>\n")
        return 2
    path = Path(arguments[0])
    root = reviewed_root(path, Path(os.environ.get("HOME", "")))
    if root is None:
        sys.stderr.write("agent-memory-body.py: path is outside the reviewed harness memory roots\n")
        return 2
    try:
        content = read_body(Path(os.environ.get("HOME", "")), path)
    except RuntimeError as error:
        sys.stderr.write(f"agent-memory-body.py: {error}\n")
        return 1
    except OSError:
        sys.stderr.write("agent-memory-body.py: file is absent, linked, or outside the reviewed roots\n")
        return 1
    sys.stdout.buffer.write(content)
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
