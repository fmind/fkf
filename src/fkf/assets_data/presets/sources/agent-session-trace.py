#!/usr/bin/env python3
"""Project bounded evidence from completed normalized coding-agent sessions."""

from __future__ import annotations

import json
import os
import re
import stat
import subprocess
import sys
import unicodedata
from collections.abc import Iterable, Iterator
from dataclasses import dataclass
from datetime import datetime
from pathlib import Path
from typing import Any
from urllib.parse import urlsplit

MAX_MANIFEST_BYTES = 1 << 16
MAX_TRANSCRIPT_BYTES = 8 << 20
MAX_GIT_BYTES = 1 << 20
MAX_OUTPUT_BYTES = 64 << 20
READ_BYTES = 64 << 10
GITHUB_PART = re.compile(r"^[A-Za-z0-9._-]+$")
COMMAND = re.compile(
    r"^(?:mise run|go test|go vet|go build|golangci-lint|git diff|git status|shellcheck|dprint|npm test|pnpm test|pytest|uv run pytest)(?:\s|$)"
)
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


@dataclass(frozen=True)
class Candidate:
    agent: str
    session_id: str
    ingested_at: str
    high_water_mark: str
    record_count: float
    ingested: datetime
    last: datetime
    path: Path


def fingerprint(value: os.stat_result) -> tuple[int, int, int, int, int, int]:
    return value.st_dev, value.st_ino, value.st_mode, value.st_size, value.st_mtime_ns, value.st_ctime_ns


def read_regular(path: Path, maximum: int, label: str, oversize: str) -> bytes:
    before = path.stat(follow_symlinks=False)
    if not stat.S_ISREG(before.st_mode):
        raise RuntimeError(f"{label} is missing or linked")
    flags = os.O_RDONLY | getattr(os, "O_CLOEXEC", 0) | getattr(os, "O_NOFOLLOW", 0)
    descriptor = os.open(path, flags)
    try:
        opened = os.fstat(descriptor)
        if fingerprint(before) != fingerprint(opened):
            raise RuntimeError(f"{label} changed while it was being opened")
        output = bytearray()
        while chunk := os.read(descriptor, min(READ_BYTES, maximum + 1 - len(output))):
            output.extend(chunk)
            if len(output) > maximum:
                raise RuntimeError(oversize)
        after = os.fstat(descriptor)
    finally:
        os.close(descriptor)
    try:
        current = path.stat(follow_symlinks=False)
    except OSError as error:
        raise RuntimeError(f"{label} changed while it was being read") from error
    if fingerprint(opened) != fingerprint(after) or fingerprint(opened) != fingerprint(current):
        raise RuntimeError(f"{label} changed while it was being read")
    return bytes(output)


def instant(value: object) -> datetime | None:
    if not isinstance(value, str) or not re.fullmatch(
        r"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})", value
    ):
        return None
    try:
        return datetime.fromisoformat(value)
    except ValueError:
        return None


def utf8_prefix(value: str, maximum: int) -> str:
    encoded = value.encode()
    if len(encoded) <= maximum:
        return value
    return encoded[:maximum].decode(errors="ignore")


def safe_layout(value: object) -> str:
    if isinstance(value, str):
        text = value
    elif value is None:
        text = ""
    else:
        text = json.dumps(value, ensure_ascii=False, separators=(",", ":"))
    text = "".join(
        character
        for character in text
        if unicodedata.category(character) != "Cf"
        and not (ord(character) < 32 and character not in "\t\n\r")
        and ord(character) != 127
    )
    return utf8_prefix(text, 6000)


def normalize_prompt(value: object) -> str | None:
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


def candidate(path: Path) -> Candidate | None:
    raw = json.loads(read_regular(path, MAX_MANIFEST_BYTES, "session manifest", "oversized session manifest"))
    if not isinstance(raw, dict):
        return None
    if raw.get("schema_version") != 1 or raw.get("parser_version") != "1" or raw.get("completeness") != "complete":
        return None
    agent, session_id = raw.get("agent"), raw.get("session_id")
    ingested_at, high_water_mark, count = raw.get("ingested_at"), raw.get("high_water_mark"), raw.get("record_count")
    ingested, last = instant(ingested_at), instant(high_water_mark)
    if (
        not isinstance(agent, str)
        or not isinstance(session_id, str)
        or not isinstance(ingested_at, str)
        or not isinstance(high_water_mark, str)
        or not isinstance(count, int | float)
        or isinstance(count, bool)
        or len(agent.encode()) > 64
        or len(session_id.encode()) > 512
        or ingested is None
        or last is None
    ):
        return None
    return Candidate(agent, session_id, ingested_at, high_water_mark, float(count), ingested, last, path.parent)


def manifests(store: Path) -> Iterator[Candidate]:
    """Stream valid candidates without retaining an append-only store's paths."""
    for path in store.rglob("manifest.json"):
        if path.is_symlink() or not path.is_file() or len(path.relative_to(store).parts) != 4:
            continue
        value = candidate(path)
        if value is not None:
            yield value


def relevant_identities(
    values: Iterable[Candidate],
    since: datetime,
    until: datetime,
) -> set[tuple[str, str]]:
    """Retain only identities with one complete generation in the requested window."""
    relevant: set[tuple[str, str]] = set()
    for item in values:
        if since <= item.last < until:
            relevant.add((item.agent, item.session_id))
            if len(relevant) > 8192:
                raise RuntimeError("more than 8192 completed sessions are relevant to the requested window")
    return relevant


def select_candidates(
    values: Iterable[Candidate],
    relevant: set[tuple[str, str]],
    since: datetime,
    until: datetime,
) -> list[Candidate]:
    """Retain the newest complete generation for the bounded relevant identities."""
    latest: dict[tuple[str, str], Candidate] = {}
    for item in values:
        key = item.agent, item.session_id
        if key not in relevant:
            continue
        current = latest.get(key)
        if current is None or (current.ingested, os.fspath(current.path)) < (item.ingested, os.fspath(item.path)):
            latest[key] = item
    selected = sorted(
        (item for item in latest.values() if since <= item.last < until),
        key=lambda item: (item.agent, item.session_id, item.ingested, os.fspath(item.path)),
    )
    if len(selected) > 1024:
        raise RuntimeError("more than 1024 completed sessions fall in the requested window")
    return selected


def session(candidate_value: Candidate) -> dict[str, Any] | None:
    path = candidate_value.path / "transcript.jsonl"
    if path.is_symlink() or not path.is_file():
        raise RuntimeError("session transcript is missing or linked")
    source = read_regular(
        path,
        MAX_TRANSCRIPT_BYTES,
        "session transcript",
        "session transcript exceeds 8 MiB",
    )
    result: dict[str, Any] = {
        "first_at": None,
        "last_at": None,
        "cwd": None,
        "model": None,
        "requests": [],
        "last_assistant": None,
    }
    for line in source.splitlines():
        record = json.loads(line)
        if not isinstance(record, dict):
            raise TypeError("session transcript contains a non-object JSON line")
        if record.get("agent") != candidate_value.agent or record.get("sid") != candidate_value.session_id:
            continue
        if result["first_at"] is None:
            result["first_at"] = record.get("ts")
        result["last_at"] = record.get("ts") or result["last_at"]
        result["cwd"] = record.get("cwd") or result["cwd"]
        result["model"] = record.get("model") or result["model"]
        requests = result["requests"]
        if record.get("role") == "user" and isinstance(requests, list) and len(requests) < 20:
            prompt = normalize_prompt(record.get("content"))
            if prompt is not None and (clean := safe_layout(prompt)):
                requests.append(clean)
        elif record.get("role") == "assistant":
            result["last_assistant"] = safe_layout(record.get("content"))
    if result["first_at"] is None or not result["requests"]:
        return None
    result["last_at"] = candidate_value.high_water_mark
    result["verification"] = verification(result["last_assistant"])
    result.update(
        id=f"{candidate_value.agent}:{candidate_value.session_id}",
        harness=candidate_value.agent,
        sid=candidate_value.session_id,
    )
    return result


def verification(value: object) -> list[str]:
    if not isinstance(value, str):
        return []
    commands = set()
    for line in value.split("\n"):
        line = re.sub(r"^\s*[-*]\s*", "", line)
        line = line.removeprefix("`")
        line = re.sub(r"`\.*$", "", line)
        if len(line) <= 240 and COMMAND.match(line):
            commands.add(line)
    return sorted(commands)[:20]


def repo_name(candidate_value: str) -> str | None:
    if "?" in candidate_value or "#" in candidate_value:
        return None
    path = ""
    if "://" in candidate_value:
        try:
            parsed = urlsplit(candidate_value)
        except ValueError:
            return None
        if parsed.scheme not in {"http", "https", "ssh"} or (parsed.hostname or "").lower() != "github.com":
            return None
        path = parsed.path.removeprefix("/")
    elif candidate_value.startswith("git@github.com:"):
        path = candidate_value.removeprefix("git@github.com:")
    path = path.removesuffix(".git")
    parts = path.split("/")
    if len(parts) != 2 or any(part in {"", ".", ".."} or GITHUB_PART.fullmatch(part) is None for part in parts):
        return None
    return "/".join(parts)


def run_git(
    arguments: list[str], *, environment: dict[str, str] | None = None, allow_failure: bool = False
) -> bytes | None:
    with subprocess.Popen(
        ["git", *arguments],
        stdout=subprocess.PIPE,
        # Git diagnostics are untrusted and were previously captured only to be discarded.
        stderr=subprocess.DEVNULL,
        env=environment,
    ) as process:
        if process.stdout is None:  # pragma: no cover
            raise RuntimeError
        output = process.stdout.read(MAX_GIT_BYTES + 1)
        if len(output) > MAX_GIT_BYTES:
            process.kill()
            process.wait()
            raise RuntimeError("git command output exceeds 1 MiB")
        returncode = process.wait()
    if returncode != 0:
        if allow_failure:
            return None
        raise RuntimeError("git command failed")
    return output


def git_evidence(
    cwd_value: object, cache: dict[str, tuple[list[str], str | None]], first: object, last: object
) -> tuple[list[str], str | None]:
    if (
        not isinstance(cwd_value, str)
        or not cwd_value.startswith("/")
        or "\n" in cwd_value
        or not Path(cwd_value).is_dir()
    ):
        return [], None
    if cwd_value not in cache:
        inside = run_git(
            [
                "-c",
                "core.fsmonitor=false",
                "--no-optional-locks",
                "-C",
                cwd_value,
                "rev-parse",
                "--is-inside-work-tree",
            ],
            allow_failure=True,
        )
        if inside is None:
            cache[cwd_value] = [], None
        else:
            environment = dict(os.environ)
            environment["GIT_OPTIONAL_LOCKS"] = "0"
            status_output = run_git(
                [
                    "-c",
                    "core.fsmonitor=false",
                    "--no-optional-locks",
                    "-C",
                    cwd_value,
                    "-c",
                    "core.quotePath=true",
                    "status",
                    "--short",
                    "--untracked-files=normal",
                    "--ignore-submodules=all",
                ],
                environment=environment,
            )
            if status_output is None:  # pragma: no cover - non-optional commands raise instead.
                raise RuntimeError("git status failed")
            remote_output = run_git(
                [
                    "-c",
                    "core.fsmonitor=false",
                    "--no-optional-locks",
                    "-C",
                    cwd_value,
                    "config",
                    "--get",
                    "remote.origin.url",
                ],
                allow_failure=True,
            )
            cache[cwd_value] = (
                sorted(set(filter(None, status_output.decode(errors="replace").splitlines())))[:200],
                (remote_output.decode(errors="replace").splitlines()[0] if remote_output else None),
            )
    files, remote = cache[cwd_value]
    combined = list(files)
    head = run_git(
        ["-c", "core.fsmonitor=false", "--no-optional-locks", "-C", cwd_value, "rev-parse", "--verify", "HEAD"],
        allow_failure=True,
    )
    if head is not None:
        history = run_git(
            [
                "-c",
                "core.fsmonitor=false",
                "--no-optional-locks",
                "-C",
                cwd_value,
                "-c",
                "core.quotePath=true",
                "log",
                f"--since={first}",
                f"--until={last}",
                "--format=",
                "--name-only",
                "--no-renames",
                "--",
            ],
        )
        if history is not None:
            combined.extend(history.decode(errors="replace").splitlines())
    if len(("\n".join(combined) + ("\n" if combined else "")).encode()) > MAX_GIT_BYTES:
        raise RuntimeError("combined git path evidence exceeds 1 MiB")
    return sorted(set(filter(None, combined)))[:200], repo_name(remote or "")


def trace(candidate_value: Candidate, cache: dict[str, tuple[list[str], str | None]]) -> dict[str, Any] | None:
    result = session(candidate_value)
    if result is None:
        return None
    files, repo = git_evidence(result.get("cwd"), cache, result.get("first_at"), result.get("last_at"))
    if repo is not None:
        result["repo"] = repo
        result["repository_uri"] = f"repo:github.com/{repo}"
    request = re.sub(r"\s+", " ", str(result["requests"][0])).strip()
    result.update(
        time=result["last_at"], title=request[:160] or f"Agent session {result['harness']} {result['sid']}", files=files
    )
    return result


def main(arguments: list[str]) -> int:
    if arguments[:1] in (["--version"], ["-v"]):
        sys.stdout.write("agent-session-trace.py (fkf preset helper)\n")
        return 0
    if len(arguments) not in {2, 3}:
        sys.stderr.write("usage: agent-session-trace.py <start> <end> [not-before]\n")
        return 2
    start, end, *boundary = arguments
    since, until = instant(start), instant(end)
    minimum = instant(boundary[0]) if boundary else None
    if since is None or until is None or since >= until or (boundary and minimum is None):
        sys.stderr.write(
            "agent-session-trace.py: start, end, and not-before must be RFC3339 instants forming a positive range\n"
        )
        return 1
    if minimum is not None and until <= minimum:
        sys.stdout.write("[]\n")
        return 0
    if minimum is not None and since < minimum:
        since = minimum
    home_value = os.environ.get("HOME", "")
    if not home_value.startswith("/"):
        sys.stderr.write("agent-session-trace.py: HOME must be absolute\n")
        return 1
    store = Path(home_value) / ".agents/sessions/v1"
    if not store.is_dir():
        sys.stdout.write("[]\n")
        return 0
    if store.is_symlink():
        sys.stderr.write("agent-session-trace.py: session store must not be a symlink\n")
        return 1
    try:
        for root, directories, files in os.walk(store, followlinks=False):
            for name in [*directories, *files]:
                if stat.S_ISLNK(os.lstat(Path(root) / name).st_mode):
                    raise RuntimeError("session store contains a symlink")
        relevant = relevant_identities(manifests(store), since, until)
        selected = select_candidates(manifests(store), relevant, since, until) if relevant else []
        cache: dict[str, tuple[list[str], str | None]] = {}
        output: list[dict[str, Any]] = []
        retained_bytes = 3
        for value in selected:
            item = trace(value, cache)
            if item is None:
                continue
            retained_bytes += len(json.dumps(item, ensure_ascii=False, separators=(",", ":")).encode())
            retained_bytes += bool(output)
            if retained_bytes > MAX_OUTPUT_BYTES:
                raise RuntimeError("output exceeds 64 MiB")
            output.append(item)
        output.sort(key=lambda item: (str(item["last_at"]), str(item["harness"]), str(item["sid"])))
        encoded = json.dumps(output, ensure_ascii=False, separators=(",", ":")) + "\n"
        if len(encoded.encode()) > MAX_OUTPUT_BYTES:
            raise RuntimeError("output exceeds 64 MiB")
    except (OSError, RuntimeError, TypeError, UnicodeError, ValueError, json.JSONDecodeError) as error:
        sys.stderr.write(f"agent-session-trace.py: {error}\n")
        return 1
    sys.stdout.write(encoded)
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
