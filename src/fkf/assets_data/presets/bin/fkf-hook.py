#!/usr/bin/env python3
"""Inject bounded offline FKF context at a coding-harness session boundary."""

from __future__ import annotations

import json
import os
import re
import selectors
import signal
import subprocess
import sys
import time
from contextlib import suppress
from pathlib import Path
from typing import Any
from urllib.parse import urlsplit

DAY_BUDGET = 600
REPOSITORY_BUDGET = 850
COMPACT_BUDGET = 600
MAX_INPUT_BYTES = 1 << 16
MAX_INVOKE_BYTES = 1 << 20
INVOKE_TIMEOUT_SECONDS = 5.0
READ_BYTES = 64 << 10
GITHUB_PART = re.compile(r"^[A-Za-z0-9._-]+$")
SYSTEM_PATH = "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin:/run/current-system/sw/bin:/nix/var/nix/profiles/default/bin"


def empty(harness: str) -> int:
    if harness not in {"claude", "kiro"}:
        sys.stdout.write("{}\n")
    return 0


def nested(value: dict[str, Any], *path: str) -> object:
    current: object = value
    for part in path:
        if not isinstance(current, dict):
            return None
        current = current.get(part)
    return current


def protocol_cwd(value: dict[str, Any]) -> str | None:
    candidates = (
        value.get("cwd"),
        value.get("workspace_roots"),
        value.get("workspacePaths"),
        value.get("workspaceRoots"),
        nested(value, "workspaceInfo", "rootPath"),
    )
    for candidate in candidates:
        if isinstance(candidate, str):
            return candidate
        if isinstance(candidate, list) and candidate and isinstance(candidate[0], str):
            return candidate[0]
    return None


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


def stop_process_group(process: subprocess.Popen[bytes]) -> None:
    """Stop the whole provider group so a descendant cannot outlive the hook."""
    with suppress(ProcessLookupError):
        os.killpg(process.pid, signal.SIGKILL)
    with suppress(subprocess.TimeoutExpired):
        process.wait(timeout=1)


def bounded_output(process: subprocess.Popen[bytes]) -> tuple[int, bytes]:
    if process.stdout is None:  # pragma: no cover
        raise RuntimeError("hook child has no stdout pipe")
    output = bytearray()
    deadline = time.monotonic() + INVOKE_TIMEOUT_SECONDS
    with selectors.DefaultSelector() as selector:
        selector.register(process.stdout, selectors.EVENT_READ)
        while True:
            remaining = deadline - time.monotonic()
            if remaining <= 0 or not selector.select(remaining):
                raise TimeoutError("hook child timed out")
            chunk = os.read(process.stdout.fileno(), min(READ_BYTES, MAX_INVOKE_BYTES + 1 - len(output)))
            if not chunk:
                try:
                    return process.wait(timeout=max(0.0, deadline - time.monotonic())), bytes(output)
                except subprocess.TimeoutExpired as error:
                    raise TimeoutError("hook child timed out") from error
            output.extend(chunk)
            if len(output) > MAX_INVOKE_BYTES:
                raise ValueError("hook child output exceeds 1 MiB")


def invoke(arguments: list[str], environment: dict[str, str]) -> str:
    if arguments[:1] == ["git"]:
        command = ["git", *arguments[1:]]
    elif arguments and Path(arguments[0]).is_absolute():
        command = ["/usr/bin/env", *arguments]
    else:
        raise ValueError("unexpected hook executable")
    process = subprocess.Popen(
        command,
        env=environment,
        stdout=subprocess.PIPE,
        stderr=subprocess.DEVNULL,
        start_new_session=True,
    )
    try:
        returncode, output = bounded_output(process)
    except BaseException:
        stop_process_group(process)
        raise
    finally:
        if process.stdout is not None:
            process.stdout.close()
    if returncode != 0:
        return ""
    return output.decode(errors="replace").rstrip("\n")


def main(arguments: list[str]) -> int:
    if len(arguments) < 3:
        sys.stderr.write("usage: fkf-hook.py <claude|codex|gemini|kiro> <fkf-executable> <workspace>\n")
        return 2
    harness, executable_value, workspace_value = arguments[:3]
    base = Path(__file__).resolve().parent.parent
    executable = Path(executable_value)
    workspace = Path(workspace_value)
    try:
        if not executable.is_absolute() or not executable.is_file() or not os.access(executable, os.X_OK):
            return empty(harness)
        if not workspace.is_absolute():
            return empty(harness)
        workspace = workspace.resolve(strict=True)
        home = os.environ.get("HOME", "")
        path = SYSTEM_PATH
        if home.startswith("/") and ":" not in home:
            path = f"{home}/.local/bin:{home}/go/bin:{home}/.local/share/mise/shims:{path}"
        environment = dict(os.environ)
        environment["PATH"] = path
        if sys.stdin.buffer.isatty():
            return empty(harness)
        raw = sys.stdin.buffer.read(MAX_INPUT_BYTES + 1)
        if len(raw) > MAX_INPUT_BYTES:
            return empty(harness)
        payload = json.loads(raw)
        if not isinstance(payload, dict):
            return empty(harness)
        compact = harness == "claude" and payload.get("source") == "compact"
        cwd_value = protocol_cwd(payload)
        if not isinstance(cwd_value, str) or not cwd_value.startswith("/"):
            return empty(harness)
        cwd = Path(cwd_value).resolve(strict=True)
        if not cwd.is_dir() or not cwd.is_relative_to(workspace):
            return empty(harness)
        remote = invoke(["git", "-C", os.fspath(cwd), "remote", "get-url", "origin"], environment)
        repo = repo_name(remote)
        pack = ""
        if not compact:
            day = invoke(
                [
                    os.fspath(executable),
                    "day",
                    "yesterday",
                    "--base",
                    os.fspath(base),
                    "--budget",
                    str(DAY_BUDGET),
                    "--format",
                    "text",
                ],
                environment,
            )
            if day:
                pack = f"Yesterday:\n{day}"
        if repo is not None:
            budget = COMPACT_BUDGET if compact else REPOSITORY_BUDGET
            repository = invoke(
                [
                    os.fspath(executable),
                    "context",
                    "--base",
                    os.fspath(base),
                    "--budget",
                    str(budget),
                    "--format",
                    "text",
                    "--",
                    f"repo:github.com/{repo}",
                ],
                environment,
            )
            if repository:
                pack = f"{pack}\n\nRepository:\n{repository}" if pack else repository
        if not pack:
            return empty(harness)
    except OSError, UnicodeError, ValueError, json.JSONDecodeError:
        return empty(harness)

    if harness in {"claude", "kiro"}:
        sys.stdout.write(pack + "\n")
    elif harness == "codex":
        sys.stdout.write(
            json.dumps(
                {"hookSpecificOutput": {"hookEventName": "SessionStart", "additionalContext": pack}},
                ensure_ascii=False,
                separators=(",", ":"),
            )
            + "\n"
        )
    elif harness == "copilot":
        sys.stdout.write("{}\n")
    elif harness == "gemini":
        event = payload.get("hook_event_name") or "SessionStart"
        sys.stdout.write(
            json.dumps(
                {"hookSpecificOutput": {"hookEventName": event, "additionalContext": pack}},
                ensure_ascii=False,
                separators=(",", ":"),
            )
            + "\n"
        )
    else:
        sys.stderr.write(f"fkf-hook.py: unknown harness {harness}\n")
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
