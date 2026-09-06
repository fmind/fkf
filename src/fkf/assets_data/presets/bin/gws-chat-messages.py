#!/usr/bin/env python3
"""Collect bounded metadata for messages in every visible Google Chat space."""

from __future__ import annotations

import json
import subprocess
import sys
from datetime import UTC, datetime, timedelta
from pathlib import Path
from typing import Any

MAX_PROVIDER_BYTES = 64 << 20
MAX_OUTPUT_BYTES = 64 << 20
MAX_SPACES = 10_000
MAX_RECORDS = 100_000
PAGE_ALL = ("--page-all", "--page-limit", "100")


def instant(value: str) -> datetime:
    parsed = datetime.fromisoformat(value)
    if parsed.tzinfo is None:
        raise ValueError
    return parsed.astimezone(UTC)


def bounded(command: list[str]) -> bytes:
    if command[:1] != [sys.executable]:
        raise RuntimeError("unexpected helper executable")
    with subprocess.Popen(
        ["/usr/bin/env", "python3", *command[1:]],
        stdout=subprocess.PIPE,
    ) as process:
        if process.stdout is None:  # pragma: no cover
            raise RuntimeError
        raw = process.stdout.read(MAX_PROVIDER_BYTES + 1)
        if len(raw) > MAX_PROVIDER_BYTES:
            process.kill()
            process.wait()
            raise RuntimeError("provider response exceeds FKF's 64 MiB command bound")
        if process.wait() != 0:
            raise RuntimeError("provider command failed")
    return raw


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


def main(arguments: list[str]) -> int:
    if arguments[:1] in (["--version"], ["-v"]):
        sys.stdout.write("gws-chat-messages.py (fkf base helper)\n")
        return 0
    if len(arguments) != 2:
        sys.stderr.write("usage: gws-chat-messages.py <start> <end>\n")
        return 2
    start, end = arguments
    try:
        start_time, end_time = map(instant, arguments)
        if start_time >= end_time:
            raise ValueError("start must be before end")
        query_start = (start_time - timedelta(seconds=1)).isoformat().replace("+00:00", "Z")
        filter_value = f'createTime > "{query_start}" AND createTime < "{end}"'
        helper = Path(__file__).with_name("gws-page-json.py")
        spaces_pages = documents(
            bounded(
                [
                    sys.executable,
                    "-I",
                    str(helper),
                    "spaces",
                    "gws",
                    "chat",
                    "spaces",
                    "list",
                    "--params",
                    '{"pageSize":1000}',
                    *PAGE_ALL,
                ]
            )
        )
        spaces: set[str] = set()
        for page in spaces_pages:
            values = page.get("spaces", [])
            if not isinstance(values, list) or any(not isinstance(item, dict) for item in values):
                raise ValueError("spaces listing returned invalid JSON pages")
            spaces.update(item["name"] for item in values if isinstance(item.get("name"), str))
            if len(spaces) > MAX_SPACES:
                raise RuntimeError(f"space bound exceeds {MAX_SPACES}")
        records: list[dict[str, Any]] = []
        for space in sorted(spaces):
            params = json.dumps(
                {"parent": space, "pageSize": 1000, "filter": filter_value, "showDeleted": True},
                separators=(",", ":"),
            )
            try:
                pages = documents(
                    bounded(
                        [
                            sys.executable,
                            "-I",
                            str(helper),
                            "messages",
                            "gws",
                            "chat",
                            "spaces",
                            "messages",
                            "list",
                            "--params",
                            params,
                            *PAGE_ALL,
                        ]
                    )
                )
            except RuntimeError as error:
                raise RuntimeError(f"cannot list messages in {space}") from error
            for page in pages:
                messages = page.get("messages", [])
                if not isinstance(messages, list) or any(not isinstance(item, dict) for item in messages):
                    raise ValueError(f"{space} returned invalid message pages")
                for message in messages:
                    created = message.get("createTime")
                    if not isinstance(created, str) or not start <= created < end:
                        continue
                    sender = message.get("sender") or {}
                    message_space = message.get("space") or {}
                    thread = message.get("thread") or {}
                    if not all(isinstance(value, dict) for value in (sender, message_space, thread)):
                        raise ValueError
                    if len(records) >= MAX_RECORDS:
                        raise RuntimeError(f"chat-message record bound exceeds {MAX_RECORDS}")
                    records.append(
                        {
                            "name": message.get("name"),
                            "createTime": created,
                            "lastUpdateTime": message.get("lastUpdateTime"),
                            "deleteTime": message.get("deleteTime"),
                            "sender": {"name": sender.get("name"), "type": sender.get("type")},
                            "space": message_space.get("name", space),
                            "thread": thread.get("name"),
                            "deleted": "deleteTime" in message,
                            "attachmentCount": len(message.get("attachment") or []),
                            "annotationCount": len(message.get("annotations") or []),
                            "cardCount": len(message.get("cardsV2") or []),
                            "title": f"{sender.get('name', 'unknown sender')} in {message_space.get('name', space)}",
                        }
                    )
        unique = {record["name"]: record for record in sorted(records, key=lambda record: str(record["name"]))}
        if len(unique) != len(records):
            raise RuntimeError("message pages returned duplicate identities")
        output = (json.dumps(list(unique.values()), ensure_ascii=False, separators=(",", ":")) + "\n").encode()
        if len(output) > MAX_OUTPUT_BYTES:
            raise RuntimeError(f"output bound exceeds {MAX_OUTPUT_BYTES} bytes")
    except ValueError as error:
        message = str(error)
        if "start must" not in message:
            sys.stderr.write(f"gws-chat-messages.py: {message or 'invalid provider response'}\n")
            return 1
        sys.stderr.write(f"gws-chat-messages.py: {message}\n")
        return 2
    except (OSError, RuntimeError, UnicodeError, TypeError, json.JSONDecodeError) as error:
        sys.stderr.write(f"gws-chat-messages.py: {error}\n")
        return 1
    sys.stdout.buffer.write(output)
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
