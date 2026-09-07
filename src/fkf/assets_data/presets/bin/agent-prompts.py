#!/usr/bin/env python3
"""Collect durable coding-agent request metadata from the normalized lineage store."""

from __future__ import annotations

import json
import os
import re
import stat
import subprocess
import sys
from pathlib import Path
from typing import Any
from urllib.parse import urlsplit

MAX_JSON_BYTES = 8 << 20
MAX_GIT_BYTES = 1 << 20
MAX_FILES = 8192
MAX_RECORDS = 8192
MAX_OUTPUT_BYTES = 64 << 20
GITHUB_PART = re.compile(r"^[A-Za-z0-9._-]+$")
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
    """Mirror the reviewed prompt privacy filter without importing executable base content."""
    if value is None:
        text = ""
    elif isinstance(value, str):
        text = value
    else:
        text = json.dumps(value, ensure_ascii=False, separators=(",", ":"))
    for tag in HARNESS_TAGS:
        if f"<{tag}" in text:
            text = re.sub(rf"<{re.escape(tag)}(?:\s[^>]*)?>.*?</{re.escape(tag)}>", "", text, flags=re.DOTALL)
    if "USER_REQUEST" in text:
        text = re.sub(r"</?USER_REQUEST>", "", text)
    text = text.lstrip(" \t\n\r")
    if not text or text.startswith(INJECTED_PREFIXES):
        return None
    return text


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


def json_lines(home: Path, path: Path):
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


def repo_name(candidate: str) -> str | None:
    if not candidate or "?" in candidate or "#" in candidate:
        return None
    path = candidate
    if "://" in candidate:
        try:
            parsed = urlsplit(candidate)
        except ValueError:
            return None
        if not parsed.scheme or not parsed.netloc or (parsed.hostname or "").lower() != "github.com":
            return None
        path = parsed.path.removeprefix("/")
    elif re.match(r"^[^/:]+:[^/]+/", candidate):
        authority, path = candidate.split(":", 1)
        if authority.rsplit("@", 1)[-1].lower() != "github.com":
            return None
    path = path.removesuffix(".git")
    parts = path.split("/")
    if len(parts) != 2 or any(part in {"", ".", ".."} or GITHUB_PART.fullmatch(part) is None for part in parts):
        return None
    return "/".join(parts)


def repository(cwd: str) -> str | None:
    if not cwd or not Path(cwd).is_dir():
        return None
    with subprocess.Popen(
        ["git", "-C", cwd, "config", "--get", "remote.origin.url"],
        stdout=subprocess.PIPE,
        stderr=subprocess.DEVNULL,
    ) as process:
        if process.stdout is None:  # pragma: no cover
            raise RuntimeError("git command has no stdout pipe")
        output = process.stdout.read(MAX_GIT_BYTES + 1)
        if len(output) > MAX_GIT_BYTES:
            process.kill()
            process.wait()
            raise RuntimeError("git output exceeds 1 MiB")
        returncode = process.wait()
    if returncode != 0:
        return None
    return repo_name(output.decode(errors="replace").strip())


def transcripts(home: Path, store: Path) -> list[Path]:
    if not directory_exists(home, store):
        return []
    paths: list[Path] = []
    for path in store.glob("*/*/*/transcript.jsonl"):
        if len(paths) >= MAX_FILES:
            raise RuntimeError(f"more than {MAX_FILES} transcript files")
        paths.append(path)
    return sorted(paths)


def records(store: Path, start: str, end: str, preview: int, store_text: bool) -> list[dict[str, Any]]:
    home = Path(os.environ.get("HOME", ""))
    projected: list[dict[str, Any]] = []
    retained_bytes = 3
    for transcript in transcripts(home, store):
        for turn, raw in enumerate(json_lines(home, transcript), start=1):
            if raw.get("role") != "user":
                continue
            timestamp = raw.get("ts") or ""
            if not isinstance(timestamp, str) or not start <= timestamp < end:
                continue
            text = normalize_prompt(raw.get("content", ""))
            if text is None:
                continue
            raw_agent, lineage, session = transcript.parts[-4:-1]
            agent = "antigravity" if raw_agent == "agy" else raw_agent
            sid = raw.get("sid")
            cwd = raw.get("cwd") if isinstance(raw.get("cwd"), str) else None
            model = raw.get("model") if isinstance(raw.get("model"), str) else None
            record: dict[str, Any] = {
                "id": f"{raw_agent}-{sid if sid is not None else 'nosid'}-{re.sub(r'[-:.]', '', timestamp)}",
                "time": raw.get("ts"),
                "agent": agent,
                "sid": sid,
                "lineage": lineage,
                "session": session,
                "turn": turn,
                "cwd": cwd,
                "model": model,
                "chars": len(text),
                "lines": len(text.split("\n")),
            }
            if preview > 0:
                record["title"] = re.sub(r"\s+", " ", text[: preview * 4])[:preview]
            else:
                record["title"] = f"{agent} prompt in {Path(cwd).name if cwd else '?'}"
            if store_text:
                record["text"] = text
            if len(projected) >= MAX_RECORDS:
                raise RuntimeError(f"more than {MAX_RECORDS} prompt records")
            retained_bytes += len(json.dumps(record, ensure_ascii=False, separators=(",", ":")).encode())
            retained_bytes += bool(projected)
            if retained_bytes > MAX_OUTPUT_BYTES:
                raise RuntimeError("output exceeds 64 MiB")
            projected.append(record)

    by_cwd: dict[str, str] = {}
    repository_bytes = 0
    for cwd in {item["cwd"] for item in projected if item["cwd"]}:
        repo = repository(cwd)
        if repo is None:
            continue
        repository_bytes += len(repo.encode())
        if repository_bytes > MAX_OUTPUT_BYTES:
            raise RuntimeError("output exceeds 64 MiB")
        by_cwd[cwd] = repo
    selected: dict[tuple[str, object, object], dict[str, Any]] = {}
    for record in sorted(projected, key=lambda item: item["session"]):
        repo = by_cwd.get(record["cwd"])
        if repo is not None:
            record["repo"] = repo
        selected.setdefault((record["agent"], record["sid"], record["time"]), record)
    return sorted(selected.values(), key=lambda item: str(item["time"]))


def encode_output(records: list[dict[str, Any]]) -> str:
    encoded = json.dumps(records, ensure_ascii=False, separators=(",", ":")) + "\n"
    if len(encoded.encode()) > MAX_OUTPUT_BYTES:
        raise RuntimeError("output exceeds 64 MiB")
    return encoded


def main(arguments: list[str]) -> int:
    if arguments[:1] in (["--version"], ["-v"]):
        sys.stdout.write("agent-prompts.py (fkf preset helper)\n")
        return 0
    if len(arguments) not in {2, 3}:
        sys.stderr.write("usage: agent-prompts.py <start> <end> [preview-chars]\n")
        return 2
    start, end, *mode = arguments
    preview_value = mode[0] if mode else "200"
    store_text = preview_value == "full"
    if store_text:
        preview_value = "200"
    if not preview_value.isdigit():
        sys.stderr.write("agent-prompts.py: preview-chars must be a number or 'full'\n")
        return 2
    store = Path(os.environ.get("HOME", "")) / ".agents" / "sessions" / "v1"
    try:
        output = records(store, start, end, int(preview_value), store_text)
        encoded = encode_output(output)
    except (OSError, RuntimeError, UnicodeError, TypeError, ValueError, json.JSONDecodeError) as error:
        sys.stderr.write(f"agent-prompts.py: {error}\n")
        return 1
    sys.stdout.write(encoded)
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
