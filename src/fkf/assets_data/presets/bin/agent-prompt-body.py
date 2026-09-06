#!/usr/bin/env python3
"""Resolve one durable coding-agent request body on explicit demand."""

from __future__ import annotations

import json
import os
import re
import stat
import sys
from collections.abc import Iterator
from pathlib import Path

MAX_JSON_BYTES = 8 << 20
MAX_BODY_BYTES = 64 << 20
MAX_FILES = 8192
AGENT = re.compile(r"^[A-Za-z0-9._]+$")
INJECTED_PREFIXES = (
    "# AGENTS.md instructions for",
    "Are you still working on",
    "Your claude.ai usage limit",
    "Caveat: The messages below are auto-generated",
    "[Request interrupted",
    "This session is being continued from a previous",
    "API Error:",
    "<system>",
)
HARNESS_TAGS = (
    "system-reminder",
    "ADDITIONAL_METADATA",
    "USER_SETTINGS_CHANGE",
    "task-notification",
    "local-command-caveat",
    "local-command-stdout",
    "command-name",
    "command-message",
    "command-args",
    "recommended_plugins",
    "environment_context",
    "user-prompt-submit-hook",
    "ide_opened_file",
    "ide_selection",
    "codex_internal_context",
)


def normalize_prompt(value: object) -> str | None:
    if not isinstance(value, str):
        raise TypeError("matching user turn has non-string content")
    text = value
    for tag in HARNESS_TAGS:
        if f"<{tag}" in text:
            text = re.sub(rf"<{re.escape(tag)}(?:\s[^>]*)?>.*?</{re.escape(tag)}>", "", text, flags=re.DOTALL)
    if "USER_REQUEST" in text:
        text = re.sub(r"</?USER_REQUEST>", "", text)
    text = text.lstrip(" \t\n\r")
    if not text or text.startswith(INJECTED_PREFIXES):
        return None
    return text


def timestamp(value: str) -> str | None:
    match = re.fullmatch(r"(\d{4})(\d{2})(\d{2})T(\d{2})(\d{2})(\d{2})(\d{3})?Z", value)
    if match is None:
        return None
    year, month, day, hour, minute, second, milliseconds = match.groups()
    fraction = f".{milliseconds}" if milliseconds is not None else ""
    return f"{year}-{month}-{day}T{hour}:{minute}:{second}{fraction}Z"


def fingerprint(value: os.stat_result) -> tuple[int, int, int, int, int, int]:
    return value.st_dev, value.st_ino, value.st_mode, value.st_size, value.st_mtime_ns, value.st_ctime_ns


def open_directory(path: str | Path, parent: int | None = None) -> int:
    inspected = os.stat(path, dir_fd=parent, follow_symlinks=False)
    if not stat.S_ISDIR(inspected.st_mode):
        raise RuntimeError("transcript root is missing or linked")
    flags = os.O_RDONLY | getattr(os, "O_CLOEXEC", 0) | getattr(os, "O_DIRECTORY", 0) | getattr(os, "O_NOFOLLOW", 0)
    descriptor = os.open(path, flags, dir_fd=parent)
    if fingerprint(inspected) != fingerprint(os.fstat(descriptor)):
        os.close(descriptor)
        raise RuntimeError("transcript root changed while it was being opened")
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


def directory_exists(home: Path, path: Path) -> bool:
    try:
        descriptor = open_chain(home, path.relative_to(home).parts)
    except FileNotFoundError:
        return False
    try:
        return True
    finally:
        os.close(descriptor)


def json_lines(home: Path, path: Path, contains: bytes | None = None) -> Iterator[dict[str, object]]:
    parent = open_chain(home, path.relative_to(home).parts[:-1])
    descriptor = -1
    try:
        name = path.name
        inspected = os.stat(name, dir_fd=parent, follow_symlinks=False)
        if not stat.S_ISREG(inspected.st_mode):
            raise RuntimeError("transcript file is missing or linked")
        flags = os.O_RDONLY | getattr(os, "O_CLOEXEC", 0) | getattr(os, "O_NOFOLLOW", 0)
        descriptor = os.open(name, flags, dir_fd=parent)
        opened = os.fstat(descriptor)
        if fingerprint(inspected) != fingerprint(opened):
            raise RuntimeError("transcript changed while it was being opened")
        with os.fdopen(descriptor, "rb", closefd=False) as stream:
            while line := stream.readline(MAX_JSON_BYTES + 1):
                if len(line) > MAX_JSON_BYTES:
                    raise RuntimeError("JSON line exceeds 8 MiB")
                if contains is not None and contains not in line:
                    continue
                value = json.loads(line)
                if not isinstance(value, dict):
                    raise TypeError("transcript contains a non-object JSON line")
                yield value
        after = os.fstat(descriptor)
        current = os.stat(name, dir_fd=parent, follow_symlinks=False)
        if fingerprint(opened) != fingerprint(after) or fingerprint(opened) != fingerprint(current):
            raise RuntimeError("transcript changed while it was being read")
    finally:
        if descriptor >= 0:
            os.close(descriptor)
        os.close(parent)


def main(arguments: list[str]) -> int:
    if arguments[:1] in (["--version"], ["-v"]):
        sys.stdout.write("agent-prompt-body.py (fkf preset helper)\n")
        return 0
    if len(arguments) != 1:
        sys.stderr.write("usage: agent-prompt-body.py <agent>-<sid>-<compact-ts>\n")
        return 2
    identifier = arguments[0]
    agent, separator, remainder = identifier.partition("-")
    sid, separator_two, stamp = remainder.rpartition("-")
    if not separator or not separator_two or not agent or not sid:
        sys.stderr.write(f"agent-prompt-body.py: '{identifier}' does not contain both a harness and a session id\n")
        return 2
    if agent in {".", ".."} or AGENT.fullmatch(agent) is None:
        sys.stderr.write(f"agent-prompt-body.py: invalid harness '{agent}'\n")
        return 2
    instant = timestamp(stamp)
    if instant is None:
        sys.stderr.write(
            f"agent-prompt-body.py: '{stamp}' is not a compact timestamp; "
            "expected YYYYMMDDTHHMMSSZ or YYYYMMDDTHHMMSSmmmZ\n"
        )
        return 2
    home = Path(os.environ.get("HOME", ""))
    store = home / ".agents" / "sessions" / "v1" / agent
    try:
        if not directory_exists(home, store):
            raise RuntimeError(f"no transcripts for harness '{agent}'")
        body: bytes | None = None
        transcripts: list[Path] = []
        for transcript in store.rglob("transcript.jsonl"):
            if len(transcripts) >= MAX_FILES:
                raise RuntimeError(f"more than {MAX_FILES} transcript files")
            transcripts.append(transcript)
        for transcript in sorted(transcripts):
            for raw in json_lines(home, transcript, instant.encode()):
                if raw.get("role") != "user" or raw.get("ts") != instant or raw.get("sid") != sid:
                    continue
                normalized = normalize_prompt(raw.get("content"))
                if normalized is None:
                    continue
                candidate = normalized.encode()
                if len(candidate) > MAX_BODY_BYTES:
                    raise RuntimeError(f"body is {len(candidate)} bytes; fkf read allows at most {MAX_BODY_BYTES}")
                if body is None:
                    body = candidate
                elif body != candidate:
                    raise RuntimeError(
                        f"2 distinct bodies match harness '{agent}', session '{sid}', "
                        f"timestamp '{instant}'; refusing an ambiguous body"
                    )
        if body is None:
            raise RuntimeError(f"no turn for harness '{agent}', session '{sid}', timestamp '{instant}'")
    except (OSError, UnicodeError, TypeError, ValueError, json.JSONDecodeError, RuntimeError) as error:
        sys.stderr.write(f"agent-prompt-body.py: {error}\n")
        return 1
    sys.stdout.buffer.write(body)
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
