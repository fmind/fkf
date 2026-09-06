#!/usr/bin/env python3
"""Collect privacy-projected Atuin shell activity from a read-only SQLite database."""

from __future__ import annotations

import json
import sqlite3
import sys
from pathlib import Path
from typing import Any

MAX_ROW_TEXT_BYTES = 1 << 20
MAX_RECORDS = 8192
MAX_OUTPUT_BYTES = 64 << 20

TOOLS = frozenset(
    (
        "atuin",
        "bash",
        "bun",
        "cargo",
        "claude",
        "codex",
        "curl",
        "d2",
        "docker",
        "dprint",
        "fd",
        "fkf",
        "gcloud",
        "gh",
        "git",
        "go",
        "gofmt",
        "grok",
        "gws",
        "helm",
        "jq",
        "kaggle",
        "kubectl",
        "mise",
        "npm",
        "npx",
        "opencode",
        "podman",
        "python",
        "python3",
        "rg",
        "ruff",
        "shellcheck",
        "terraform",
        "tofu",
        "uv",
        "xh",
        "yq",
    )
)
ACTIONS = frozenset(
    (
        "add",
        "build",
        "check",
        "clone",
        "commit",
        "context",
        "deploy",
        "diff",
        "doctor",
        "find",
        "fmt",
        "format",
        "get",
        "graph",
        "init",
        "install",
        "list",
        "log",
        "pull",
        "push",
        "read",
        "release",
        "run",
        "search",
        "status",
        "sync",
        "test",
        "trust",
        "update",
        "upgrade",
        "validate",
        "view",
    )
)


def subject(row: dict[str, Any]) -> dict[str, Any]:
    parts = str(row.pop("_command") or "").strip().split()
    candidate = Path(parts[0]).name if parts else ""
    tool = candidate if candidate in TOOLS else None
    action = parts[1] if tool is not None and len(parts) > 1 and parts[1] in ACTIONS else None
    if tool is None:
        cwd = str(row.get("cwd") or "")
        title = f"shell in {Path(cwd).name}" if cwd else "shell activity"
        row["subject"] = f"{title} at {row['time']}"
    else:
        row["subject"] = f"{tool} {action}" if action is not None else tool
        row["tool"] = tool
        if action is not None:
            row["action"] = action
    return row


def bounded_text(value: object, label: str) -> str | None:
    if value is None:
        return None
    if not isinstance(value, bytes):
        raise TypeError(f"{label} is not text")
    if len(value) > MAX_ROW_TEXT_BYTES:
        raise ValueError(f"{label} exceeds {MAX_ROW_TEXT_BYTES} bytes")
    return value.decode()


def main(arguments: list[str]) -> int:
    if len(arguments) != 3:
        sys.stderr.write("usage: atuin-history-json.py <start> <end> <database>\n")
        return 2
    start, end, database = arguments
    connection: sqlite3.Connection | None = None
    try:
        connection = sqlite3.connect(f"file:{Path(database).absolute()}?mode=ro", uri=True)
        connection.row_factory = sqlite3.Row
        rows = connection.execute(
            """select substr(cast(id as blob), 1, ?) as id,
                      substr(cast(cwd as blob), 1, ?) as cwd,
                      cast(exit as integer) as exit,
                      cast(duration as integer) as duration,
                      substr(cast(command as blob), 1, ?) as _command,
                      strftime('%Y-%m-%dT%H:%M:%SZ', timestamp/1000000000, 'unixepoch') as time
                 from history
                where deleted_at is null
                  and timestamp >= strftime('%s', ?)*1000000000
                  and timestamp < strftime('%s', ?)*1000000000
                order by timestamp
                limit ?""",
            (
                MAX_ROW_TEXT_BYTES + 1,
                MAX_ROW_TEXT_BYTES + 1,
                MAX_ROW_TEXT_BYTES + 1,
                start,
                end,
                MAX_RECORDS + 1,
            ),
        )
        output = bytearray(b"[")
        for index, row in enumerate(rows):
            if index >= MAX_RECORDS:
                raise ValueError(f"record count exceeds {MAX_RECORDS}")
            record = {
                "id": bounded_text(row["id"], "id"),
                "cwd": bounded_text(row["cwd"], "cwd"),
                "exit": row["exit"],
                "duration": row["duration"],
                "_command": bounded_text(row["_command"], "command"),
                "time": row["time"],
            }
            encoded = json.dumps(subject(record), ensure_ascii=False, separators=(",", ":")).encode()
            separator = b"," if index else b""
            if len(output) + len(separator) + len(encoded) + len(b"]\n") > MAX_OUTPUT_BYTES:
                raise ValueError(f"output exceeds {MAX_OUTPUT_BYTES} bytes")
            output.extend(separator)
            output.extend(encoded)
        if len(output) + len(b"]\n") > MAX_OUTPUT_BYTES:
            raise ValueError(f"output exceeds {MAX_OUTPUT_BYTES} bytes")
        output.extend(b"]\n")
    except (OSError, sqlite3.Error, TypeError, ValueError) as error:
        sys.stderr.write(f"atuin-history-json.py: {error}\n")
        return 1
    finally:
        if connection is not None:
            connection.close()
    sys.stdout.buffer.write(output)
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
