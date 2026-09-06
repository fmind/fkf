#!/usr/bin/env python3
"""Collect metadata for coding-agent sessions active in one exact time window."""

from __future__ import annotations

import json
import os
import re
import sqlite3
import stat
import subprocess
import sys
from collections.abc import Iterator
from contextlib import contextmanager
from datetime import UTC, datetime
from pathlib import Path
from typing import Any
from urllib.parse import urlsplit

MAX_JSON_BYTES = 8 << 20
MAX_GIT_BYTES = 1 << 20
MAX_FILES = 8192
MAX_RECORDS = 8192
MAX_OUTPUT_BYTES = 64 << 20
GITHUB_PART = re.compile(r"^[A-Za-z0-9._-]+$")


def instant(value: object) -> datetime | None:
    if not isinstance(value, str) or not re.fullmatch(
        r"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})", value
    ):
        return None
    try:
        return datetime.fromisoformat(value)
    except ValueError:
        return None


def fingerprint(value: os.stat_result) -> tuple[int, int, int, int, int, int]:
    return value.st_dev, value.st_ino, value.st_mode, value.st_size, value.st_mtime_ns, value.st_ctime_ns


def identity(value: os.stat_result) -> tuple[int, int, int]:
    return value.st_dev, value.st_ino, value.st_mode


def open_directory(path: str | Path, parent: int | None = None) -> int:
    inspected = os.stat(path, dir_fd=parent, follow_symlinks=False)
    if not stat.S_ISDIR(inspected.st_mode):
        raise RuntimeError("session input root is missing or linked")
    flags = os.O_RDONLY | getattr(os, "O_CLOEXEC", 0) | getattr(os, "O_DIRECTORY", 0) | getattr(os, "O_NOFOLLOW", 0)
    descriptor = os.open(path, flags, dir_fd=parent)
    if fingerprint(inspected) != fingerprint(os.fstat(descriptor)):
        os.close(descriptor)
        raise RuntimeError("session input root changed while it was being opened")
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


def regular_exists(home: Path, path: Path, label: str) -> bool:
    parent = open_chain(home, path.relative_to(home).parts[:-1])
    try:
        try:
            inspected = os.stat(path.name, dir_fd=parent, follow_symlinks=False)
        except FileNotFoundError:
            return False
        if not stat.S_ISREG(inspected.st_mode):
            raise RuntimeError(f"{label} is missing or linked")
        return True
    finally:
        os.close(parent)


def open_regular(home: Path, path: Path, label: str) -> tuple[int, int, os.stat_result, str]:
    parent = open_chain(home, path.relative_to(home).parts[:-1])
    descriptor = -1
    try:
        name = path.name
        inspected = os.stat(name, dir_fd=parent, follow_symlinks=False)
        if not stat.S_ISREG(inspected.st_mode):
            raise RuntimeError(f"{label} is missing or linked")
        flags = os.O_RDONLY | getattr(os, "O_CLOEXEC", 0) | getattr(os, "O_NOFOLLOW", 0)
        descriptor = os.open(name, flags, dir_fd=parent)
        opened = os.fstat(descriptor)
        if fingerprint(inspected) != fingerprint(opened):
            raise RuntimeError(f"{label} changed while it was being opened")
        return parent, descriptor, opened, name
    except BaseException:
        if descriptor >= 0:
            os.close(descriptor)
        os.close(parent)
        raise


def verify_regular(parent: int, descriptor: int, opened: os.stat_result, name: str, label: str) -> None:
    after = os.fstat(descriptor)
    current = os.stat(name, dir_fd=parent, follow_symlinks=False)
    if fingerprint(opened) != fingerprint(after) or fingerprint(opened) != fingerprint(current):
        raise RuntimeError(f"{label} changed while it was being read")


def json_lines(home: Path, path: Path) -> Iterator[dict[str, Any]]:
    parent, descriptor, opened, name = open_regular(home, path, "session transcript")
    try:
        with os.fdopen(descriptor, "rb", closefd=False) as stream:
            while line := stream.readline(MAX_JSON_BYTES + 1):
                if len(line) > MAX_JSON_BYTES:
                    raise RuntimeError("JSON line exceeds 8 MiB")
                value = json.loads(line)
                if not isinstance(value, dict):
                    raise TypeError("session transcript contains a non-object JSON line")
                yield value
        verify_regular(parent, descriptor, opened, name, "session transcript")
    finally:
        os.close(descriptor)
        os.close(parent)


def read_text(home: Path, path: Path) -> str:
    parent, descriptor, opened, name = open_regular(home, path, "session JSON document")
    try:
        value = bytearray()
        while chunk := os.read(descriptor, MAX_JSON_BYTES + 1 - len(value)):
            value.extend(chunk)
            if len(value) > MAX_JSON_BYTES:
                raise RuntimeError("JSON document exceeds 8 MiB")
        verify_regular(parent, descriptor, opened, name, "session JSON document")
        return value.decode()
    finally:
        os.close(descriptor)
        os.close(parent)


def candidates(home: Path, root: Path, pattern: str, maximum_depth: int, since: datetime) -> Iterator[Path]:
    if not directory_exists(home, root):
        return
    threshold = since.timestamp() - 1
    paths: list[Path] = []
    for path in root.rglob(pattern):
        if path.is_symlink() or not path.is_file():
            continue
        if len(path.relative_to(root).parts) <= maximum_depth and path.stat().st_mtime > threshold:
            if len(paths) >= MAX_FILES:
                raise RuntimeError(f"more than {MAX_FILES} session files")
            paths.append(path)
    yield from sorted(paths)


def first_time(values: Iterator[object], since: datetime, until: datetime) -> tuple[str, datetime] | None:
    first: tuple[str, datetime] | None = None
    for value in values:
        parsed = instant(value)
        if (
            isinstance(value, str)
            and parsed is not None
            and since <= parsed < until
            and (first is None or parsed < first[1])
        ):
            first = value, parsed
    return first


def basename(value: object, fallback: str = "?") -> str:
    return Path(value).name if isinstance(value, str) and value else fallback


def claude(home: Path, since: datetime, until: datetime) -> Iterator[dict[str, Any]]:
    for path in candidates(home, home / ".claude/projects", "*.jsonl", 2, since):
        first: dict[str, Any] | None = None
        first_at: datetime | None = None
        title: object = None
        for line in json_lines(home, path):
            at = instant(line.get("timestamp"))
            if line.get("type") in {"user", "assistant"} and at is not None and since <= at < until:
                if first_at is None or at < first_at:
                    first, first_at = line, at
            elif line.get("type") == "ai-title":
                title = line.get("aiTitle")
        if first is not None:
            cwd = first.get("cwd")
            yield {
                "id": first.get("sessionId") or os.fspath(path),
                "time": first.get("timestamp"),
                "agent": "claude",
                "title": title if title is not None else f"claude in {basename(cwd)}",
                "cwd": cwd,
                "branch": first.get("gitBranch"),
            }


def codex(home: Path, since: datetime, until: datetime) -> Iterator[dict[str, Any]]:
    for path in candidates(home, home / ".codex/sessions", "rollout-*.jsonl", 4, since):
        meta: dict[str, Any] | None = None
        first: tuple[str, datetime] | None = None
        for line in json_lines(home, path):
            payload = line.get("payload")
            if meta is None and line.get("type") == "session_meta" and isinstance(payload, dict):
                meta = payload
            value = line.get("timestamp")
            if value is None and line.get("type") == "session_meta" and isinstance(payload, dict):
                value = payload.get("timestamp")
            at = instant(value)
            if isinstance(value, str) and at is not None and since <= at < until and (first is None or at < first[1]):
                first = value, at
        if meta is not None and meta.get("parent_thread_id") is None and first is not None:
            cwd = meta.get("cwd")
            git = meta.get("git") if isinstance(meta.get("git"), dict) else {}
            yield {
                "id": meta.get("id"),
                "time": first[0],
                "agent": "codex",
                "title": f"codex in {basename(cwd)}",
                "cwd": cwd,
                "branch": git.get("branch"),
                "remote": git.get("repository_url"),
            }


def gemini(home: Path, since: datetime, until: datetime) -> Iterator[dict[str, Any]]:
    for path in candidates(home, home / ".gemini/tmp", "session-*.json*", 3, since):
        marker = path.parent.parent / ".project_root"
        try:
            root = read_text(home, marker)
        except OSError:
            root = ""
        if path.suffix == ".jsonl":
            header: dict[str, Any] | None = None
            first: tuple[str, datetime] | None = None
            for line in json_lines(home, path):
                if header is None and line.get("sessionId") is not None:
                    header = line
                value = line.get("timestamp")
                if value is None and line.get("sessionId") is not None:
                    value = line.get("startTime")
                at = instant(value)
                if (
                    isinstance(value, str)
                    and at is not None
                    and since <= at < until
                    and (first is None or at < first[1])
                ):
                    first = value, at
            if header is None or (header.get("kind") or "main") != "main" or first is None:
                continue
            yield {
                "id": header.get("sessionId"),
                "time": first[0],
                "agent": "gemini",
                "title": header.get("summary") or f"gemini in {basename(root)}",
                "cwd": root or None,
                "branch": None,
            }
            continue
        value = json.loads(read_text(home, path))
        if not isinstance(value, dict):
            raise TypeError(f"{path} is not a JSON object")
        messages = value.get("messages") if isinstance(value.get("messages"), list) else []
        first = first_time(
            iter([value.get("startTime"), *(item.get("timestamp") for item in messages if isinstance(item, dict))]),
            since,
            until,
        )
        if value.get("sessionId") is not None and (value.get("kind") or "main") == "main" and first is not None:
            yield {
                "id": value.get("sessionId"),
                "time": first[0],
                "agent": "gemini",
                "title": value.get("summary") or f"gemini in {basename(root)}",
                "cwd": root or None,
                "branch": None,
            }


@contextmanager
def database(home: Path, path: Path) -> Iterator[sqlite3.Connection]:
    parent, descriptor, opened, name = open_regular(home, path, "session database")
    connection: sqlite3.Connection | None = None
    try:
        connection = sqlite3.connect(path.absolute().as_uri() + "?mode=ro", uri=True)
        current = os.stat(name, dir_fd=parent, follow_symlinks=False)
        if identity(opened) != identity(current):
            raise RuntimeError("session database changed while it was being opened")
        connection.execute("pragma query_only = on")
        yield connection
        current = os.stat(name, dir_fd=parent, follow_symlinks=False)
        if identity(opened) != identity(current):
            raise RuntimeError("session database changed while it was being read")
    finally:
        if connection is not None:
            connection.close()
        os.close(descriptor)
        os.close(parent)


def opencode(home: Path, since: datetime, until: datetime) -> Iterator[dict[str, Any]]:
    path = home / ".local/share/opencode/opencode.db"
    if not directory_exists(home, path.parent) or not regular_exists(home, path, "session database"):
        return
    query = """with activity(session_id, activity_time) as (
      select id, time_created from session union all select session_id, time_created from message
    ) select s.id, s.title, s.directory, min(a.activity_time)
      from session s join activity a on a.session_id = s.id
      where s.parent_id is null and a.activity_time >= ? and a.activity_time < ?
      group by s.id, s.title, s.directory"""
    with database(home, path) as connection:
        for identifier, title, directory, activity in connection.execute(
            query, (since.timestamp() * 1000, until.timestamp() * 1000)
        ):
            yield {
                "id": identifier,
                "time": datetime.fromtimestamp(int(activity) // 1000, UTC).isoformat().replace("+00:00", "Z"),
                "agent": "opencode",
                "title": title if title is not None else f"opencode in {basename(directory)}",
                "cwd": directory,
                "branch": None,
            }


def copilot(home: Path, since: datetime, until: datetime) -> Iterator[dict[str, Any]]:
    path = home / ".copilot/session-store.db"
    if not directory_exists(home, path.parent) or not regular_exists(home, path, "session database"):
        return
    query = """with activity(session_id, activity_time) as (
      select id, created_at from sessions union all select session_id, timestamp from turns
    ) select s.id, min(datetime(a.activity_time)), s.summary, s.cwd, s.branch, s.repository
      from sessions s join activity a on a.session_id = s.id
      where datetime(a.activity_time) >= datetime(?) and datetime(a.activity_time) < datetime(?)
      group by s.id, s.summary, s.cwd, s.branch, s.repository"""
    with database(home, path) as connection:
        for identifier, activity, summary, cwd, branch, repo in connection.execute(
            query, (since.isoformat(), until.isoformat())
        ):
            yield {
                "id": identifier,
                "time": str(activity).replace(" ", "T") + "Z",
                "agent": "copilot",
                "title": summary or f"copilot in {basename(cwd)}",
                "cwd": cwd,
                "branch": branch or None,
                "repo": repo or None,
            }


def antigravity(home: Path, since: datetime, until: datetime) -> Iterator[dict[str, Any]]:
    path = home / ".gemini/antigravity-cli/history.jsonl"
    if not directory_exists(home, path.parent) or not regular_exists(home, path, "session transcript"):
        return
    grouped: dict[object, tuple[datetime, object]] = {}
    retained_bytes = 0
    for line in json_lines(home, path):
        identifier, milliseconds = line.get("conversationId"), line.get("timestamp")
        if identifier is None or not isinstance(milliseconds, int | float):
            continue
        at = datetime.fromtimestamp(milliseconds / 1000, UTC)
        if since <= at < until:
            current = grouped.get(identifier)
            workspace = line.get("workspace")
            if current is None:
                if len(grouped) >= MAX_RECORDS:
                    raise RuntimeError(f"more than {MAX_RECORDS} session identities")
                retained_bytes += len(
                    json.dumps([identifier, workspace], ensure_ascii=False, separators=(",", ":")).encode()
                )
                if retained_bytes > MAX_OUTPUT_BYTES:
                    raise RuntimeError("session identity state exceeds 64 MiB")
                grouped[identifier] = at, workspace
            else:
                if current[1] is None and workspace is not None:
                    retained_bytes += len(json.dumps(workspace, ensure_ascii=False, separators=(",", ":")).encode())
                    if retained_bytes > MAX_OUTPUT_BYTES:
                        raise RuntimeError("session identity state exceeds 64 MiB")
                grouped[identifier] = min(at, current[0]), current[1] if current[1] is not None else workspace
    for identifier, (at, cwd) in sorted(grouped.items(), key=lambda item: str(item[0])):
        yield {
            "id": identifier,
            "time": at.isoformat().replace("+00:00", "Z"),
            "agent": "antigravity",
            "cwd": cwd,
            "branch": None,
            "title": f"antigravity in {basename(cwd, 'a workspace')}",
        }


def grok(home: Path, since: datetime, until: datetime) -> Iterator[dict[str, Any]]:
    for path in candidates(home, home / ".grok/sessions", "events.jsonl", 3, since):
        summary_path = path.parent / "summary.json"
        try:
            summary = json.loads(read_text(home, summary_path))
        except FileNotFoundError as error:
            raise RuntimeError(f"Grok session beside {path} has no summary.json") from error
        if not isinstance(summary, dict):
            raise TypeError(f"{summary_path} is not a JSON object")
        first: tuple[str, datetime] | None = None
        session_id: object = None
        non_primary = False
        for line in json_lines(home, path):
            value = line.get("ts")
            at = instant(value)
            if isinstance(value, str) and at is not None and since <= at < until and (first is None or at < first[1]):
                first = value, at
            if line.get("session_id") not in {None, ""}:
                session_id = line["session_id"]
            relationship = line.get("session_relationship")
            if relationship not in {None, "", "primary"}:
                non_primary = True
        if first is None or non_primary:
            continue
        info = summary.get("info") if isinstance(summary.get("info"), dict) else {}
        cwd = info.get("cwd") or summary.get("git_root_dir")
        identifier = info.get("id") or session_id or path.parent.name
        if not isinstance(identifier, str) or not identifier:
            continue
        remotes = summary.get("git_remotes") if isinstance(summary.get("git_remotes"), list) else []
        yield {
            "id": identifier,
            "time": first[0],
            "agent": "grok",
            "title": f"grok in {basename(cwd, 'a workspace')}",
            "cwd": cwd,
            "branch": summary.get("head_branch"),
            "remote": remotes[0] if remotes else None,
        }


def repo_name(candidate: object) -> str | None:
    if not isinstance(candidate, str) or not candidate or "?" in candidate or "#" in candidate:
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


def origin(cwd: object) -> str | None:
    if not isinstance(cwd, str) or not Path(cwd).is_dir():
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
    return output.decode(errors="replace").strip() if returncode == 0 else None


def scalar(value: object) -> str:
    if isinstance(value, str):
        return value
    return json.dumps(value, ensure_ascii=False, separators=(",", ":"))


def encode_output(records: list[dict[str, Any]]) -> str:
    encoded = json.dumps(records, ensure_ascii=False, separators=(",", ":")) + "\n"
    if len(encoded.encode()) > MAX_OUTPUT_BYTES:
        raise RuntimeError("output exceeds 64 MiB")
    return encoded


def main(arguments: list[str]) -> int:
    if len(arguments) != 2:
        sys.stderr.write("usage: agent-sessions.py <start> <end>\n")
        return 2
    since, until = instant(arguments[0]), instant(arguments[1])
    if since is None or until is None or since >= until:
        sys.stderr.write("agent-sessions.py: start and end must be RFC3339 instants forming a positive range\n")
        return 1
    home = Path(os.environ.get("HOME", ""))
    try:
        output: list[dict[str, Any]] = []
        retained_bytes = 3
        providers = (claude, codex, gemini, opencode, copilot, antigravity, grok)
        for provider in providers:
            for record in provider(home, since, until):
                if len(output) >= MAX_RECORDS:
                    raise RuntimeError(f"more than {MAX_RECORDS} session records")
                repo = (
                    repo_name(record.pop("repo", None))
                    or repo_name(record.pop("remote", None))
                    or repo_name(origin(record.get("cwd")))
                )
                record["id"] = f"{record['agent']}:{scalar(record.get('id'))}"
                if repo is not None:
                    record["repo"] = repo
                record["repository_uri"] = f"repo:github.com/{repo}" if repo is not None else None
                retained_bytes += len(json.dumps(record, ensure_ascii=False, separators=(",", ":")).encode())
                retained_bytes += bool(output)
                if retained_bytes > MAX_OUTPUT_BYTES:
                    raise RuntimeError("output exceeds 64 MiB")
                output.append(record)
        encoded = encode_output(output)
    except (OSError, RuntimeError, TypeError, UnicodeError, ValueError, sqlite3.Error, json.JSONDecodeError) as error:
        sys.stderr.write(f"agent-sessions.py: {error}\n")
        return 1
    sys.stdout.write(encoded)
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
