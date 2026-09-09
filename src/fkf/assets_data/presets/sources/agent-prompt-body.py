#!/usr/bin/env python3
"""Resolve one durable coding-agent request body on explicit demand."""

from __future__ import annotations

import hashlib
import json
import os
import re
import stat
import sys
from collections.abc import Iterator
from datetime import date, timedelta
from pathlib import Path

MAX_JSON_BYTES = 8 << 20
MAX_BODY_BYTES = 64 << 20
MAX_DOCUMENT_BYTES = 64 << 20
MAX_TRANSCRIPT_BYTES = 64 << 20
AGENT = re.compile(r"^[A-Za-z0-9._]+$")
SOURCE = re.compile(r"^[a-z0-9][a-z0-9-]{0,127}$")
GENERATION = re.compile(r"^[a-f0-9]{64}$")
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


def json_lines(home: Path, path: Path, *, document: bool = False) -> Iterator[dict[str, object]]:
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
        if not document and opened.st_size > MAX_TRANSCRIPT_BYTES:
            raise RuntimeError("transcript exceeds 64 MiB")
        with os.fdopen(descriptor, "rb", closefd=False) as stream:
            if document:
                encoded = stream.read(MAX_DOCUMENT_BYTES + 1)
                if len(encoded) > MAX_DOCUMENT_BYTES:
                    raise RuntimeError("stored source document exceeds 64 MiB")
                lines = iter((encoded,))
            else:
                lines = iter(lambda: stream.readline(MAX_JSON_BYTES + 1), b"")
            consumed = 0
            for line in lines:
                consumed += len(line)
                if not document and consumed > MAX_TRANSCRIPT_BYTES:
                    raise RuntimeError("transcript exceeds 64 MiB")
                if not document and len(line) > MAX_JSON_BYTES:
                    raise RuntimeError("JSON line exceeds 8 MiB")
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


def recorded_turn(base: Path, source: str, identifier: str, instant: str, lineage: str) -> tuple[str, int]:
    """Use collection-time provenance; a timestamp can name distinct archived bodies."""
    selected: tuple[str, int] | None = None
    # Every civil collection date is within one day of the record's UTC date,
    # including evidence collected before a timezone configuration changed.
    utc_day = date.fromisoformat(instant[:10])
    for offset in (-1, 0, 1):
        day = (utc_day + timedelta(days=offset)).isoformat()
        path = base / "events" / day / f"{source}.json"
        try:
            documents = tuple(json_lines(base, path, document=True))
        except FileNotFoundError:
            continue
        envelope = documents[0]
        if (
            envelope.get("fkf") != 1
            or envelope.get("source") != source
            or envelope.get("date") != day
            or envelope.get("layer") != "events"
        ):
            raise ValueError("stored source document does not match its address")
        records = envelope.get("records")
        if not isinstance(records, list):
            raise TypeError("stored source document has no record list")
        for record in records:
            if not isinstance(record, dict) or record.get("id") != identifier:
                continue
            generation, turn = record.get("session"), record.get("turn")
            if (
                record.get("lineage") != lineage
                or not isinstance(generation, str)
                or GENERATION.fullmatch(generation) is None
                or not isinstance(turn, int)
                or isinstance(turn, bool)
                or turn < 1
            ):
                raise ValueError("stored prompt has invalid archive provenance")
            candidate = (generation, turn)
            if selected is not None and selected != candidate:
                raise ValueError("stored prompt has conflicting archive provenance")
            selected = candidate
    if selected is None:
        raise ValueError("prompt has no stored archive provenance")
    return selected


def main(arguments: list[str]) -> int:
    if arguments[:1] in (["--version"], ["-v"]):
        sys.stdout.write("agent-prompt-body.py (fkf preset helper)\n")
        return 0
    if len(arguments) != 3:
        sys.stderr.write("usage: agent-prompt-body.py <base> <source> <agent>-<sid>-<compact-ts>\n")
        return 2
    base_value, source, identifier = arguments
    if not Path(base_value).is_absolute() or SOURCE.fullmatch(source) is None:
        sys.stderr.write("agent-prompt-body.py: expected an absolute base and a source name\n")
        return 2
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
    # The normalized store addresses a lineage by these exact NUL-framed bytes.
    # The stored generation and turn resolve the exact body without scanning history.
    lineage = hashlib.sha256(f"{agent}\0{sid}\0".encode()).hexdigest()
    store = home / ".agents" / "sessions" / "v1" / agent / lineage
    try:
        generation, selected_turn = recorded_turn(
            Path(base_value).resolve(strict=True), source, identifier, instant, lineage
        )
        if not directory_exists(home, store):
            raise RuntimeError(f"no transcripts for harness '{agent}'")
        body: bytes | None = None
        for turn, raw in enumerate(json_lines(home, store / generation / "transcript.jsonl"), start=1):
            if turn != selected_turn:
                continue
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
        if body is None:
            raise RuntimeError(f"no turn for harness '{agent}', session '{sid}', timestamp '{instant}'")
    except (OSError, UnicodeError, TypeError, ValueError, json.JSONDecodeError, RuntimeError) as error:
        sys.stderr.write(f"agent-prompt-body.py: {error}\n")
        return 1
    sys.stdout.buffer.write(body)
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
