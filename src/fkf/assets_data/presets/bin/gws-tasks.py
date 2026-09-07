#!/usr/bin/env python3
"""Collect every Google task list over one exact half-open window."""

from __future__ import annotations

import json
import subprocess
import sys
from datetime import UTC, datetime
from pathlib import Path
from typing import Any

MAX_PROVIDER_BYTES = 64 << 20
MAX_OUTPUT_BYTES = 64 << 20
MAX_TASKLISTS = 10_000
MAX_RECORDS = 100_000
PAGE_ALL = ("--page-all", "--page-limit", "100")


def bounded(command: list[str]) -> bytes:
    if command[:1] != [sys.executable]:
        raise RuntimeError("unexpected helper executable")
    with subprocess.Popen(
        ["/usr/bin/env", "python3", *command[1:]],
        stdout=subprocess.PIPE,
    ) as process:
        if process.stdout is None:  # pragma: no cover
            raise RuntimeError
        output = process.stdout.read(MAX_PROVIDER_BYTES + 1)
        if len(output) > MAX_PROVIDER_BYTES:
            process.kill()
            process.wait()
            raise RuntimeError("provider response exceeds FKF's 64 MiB command bound")
        if process.wait() != 0:
            raise RuntimeError("provider command failed")
    return output


def documents(raw: bytes) -> list[dict[str, Any]]:
    text = raw.decode()
    decoder = json.JSONDecoder()
    offset = 0
    values: list[dict[str, Any]] = []
    while offset < len(text):
        while offset < len(text) and text[offset].isspace():
            offset += 1
        if offset == len(text):
            break
        value, offset = decoder.raw_decode(text, offset)
        if not isinstance(value, dict):
            raise TypeError
        values.append(value)
    return values


def timestamp(value: Any) -> datetime:
    if not isinstance(value, str):
        raise TypeError
    parsed = datetime.fromisoformat(value)
    if parsed.tzinfo is None:
        raise ValueError
    return parsed.astimezone(UTC)


def compact(value: dict[str, Any]) -> dict[str, Any]:
    return {key: item for key, item in value.items() if item is not None}


def main(arguments: list[str]) -> int:
    if len(arguments) != 2:
        sys.stderr.write("usage: gws-tasks.py <start> <end>\n")
        return 2
    start, end = arguments
    helper = Path(__file__).with_name("gws-page-json.py")
    try:
        tasklist_pages = documents(
            bounded(
                [
                    sys.executable,
                    "-I",
                    str(helper),
                    "items",
                    "gws",
                    "tasks",
                    "tasklists",
                    "list",
                    "--params",
                    '{"maxResults":100}',
                    *PAGE_ALL,
                ]
            )
        )
        tasklists = [item for page in tasklist_pages for item in page.get("items", [])]
        if len(tasklists) > MAX_TASKLISTS:
            raise RuntimeError(f"task-list bound exceeds {MAX_TASKLISTS}")
        if not tasklists or any(
            not isinstance(item, dict) or not isinstance(item.get("id"), str) for item in tasklists
        ):
            raise RuntimeError("the account returned no task list; refusing a complete-looking empty window")
        end_time = timestamp(end)
        records: list[dict[str, Any]] = []
        for tasklist in tasklists:
            identifier = tasklist["id"]
            params = json.dumps(
                {
                    "tasklist": identifier,
                    "showCompleted": True,
                    "showHidden": True,
                    "showDeleted": True,
                    "showAssigned": True,
                    "updatedMin": start,
                },
                separators=(",", ":"),
            )
            pages = documents(
                bounded(
                    [
                        sys.executable,
                        "-I",
                        str(helper),
                        "items",
                        "gws",
                        "tasks",
                        "tasks",
                        "list",
                        "--params",
                        params,
                        *PAGE_ALL,
                    ]
                )
            )
            for page in pages:
                items = page.get("items", [])
                if not isinstance(items, list) or any(not isinstance(item, dict) for item in items):
                    raise ValueError
                for item in items:
                    if timestamp(item.get("updated")) >= end_time:
                        continue
                    if len(records) >= MAX_RECORDS:
                        raise RuntimeError(f"task record bound exceeds {MAX_RECORDS}")
                    records.append(
                        compact(
                            {
                                "id": item.get("id"),
                                "uid": f"{identifier}~{item.get('id')}",
                                "list": identifier,
                                "listTitle": tasklist.get("title", ""),
                                **{
                                    key: item.get(key)
                                    for key in (
                                        "title",
                                        "updated",
                                        "status",
                                        "due",
                                        "completed",
                                        "deleted",
                                        "hidden",
                                        "parent",
                                        "position",
                                        "webViewLink",
                                    )
                                },
                            }
                        )
                    )
        unique = {record["uid"]: record for record in records}
        if len(unique) != len(records):
            raise RuntimeError("task pages returned duplicate identities")
        ordered = sorted(unique.values(), key=lambda record: (record.get("updated", ""), record["uid"]))
        output = (json.dumps(ordered, ensure_ascii=False, separators=(",", ":")) + "\n").encode()
        if len(output) > MAX_OUTPUT_BYTES:
            raise RuntimeError(f"output bound exceeds {MAX_OUTPUT_BYTES} bytes")
    except (OSError, RuntimeError, UnicodeError, TypeError, ValueError, json.JSONDecodeError) as error:
        sys.stderr.write(f"gws-tasks.py: {error}\n")
        return 1
    sys.stdout.buffer.write(output)
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
