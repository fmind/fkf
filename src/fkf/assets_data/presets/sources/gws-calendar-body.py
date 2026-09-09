#!/usr/bin/env python3
"""Fetch one bounded Google Calendar event body on explicit demand."""

from __future__ import annotations

import json
import subprocess
import sys
from typing import Any

MAX_PROVIDER_BYTES = 64 << 20


def fetch(calendar_id: str, event_id: str) -> dict[str, Any]:
    params = json.dumps({"calendarId": calendar_id, "eventId": event_id}, separators=(",", ":"))
    with subprocess.Popen(
        ["gws", "calendar", "events", "get", "--params", params],
        stdout=subprocess.PIPE,
        stderr=subprocess.DEVNULL,
    ) as process:
        if process.stdout is None:  # pragma: no cover
            raise RuntimeError("cannot capture the calendar event")
        output = process.stdout.read(MAX_PROVIDER_BYTES + 1)
        if len(output) > MAX_PROVIDER_BYTES:
            process.kill()
            process.wait()
            raise RuntimeError("provider response exceeds FKF's 64 MiB command bound")
        if process.wait() != 0:
            raise RuntimeError("cannot fetch the calendar event")
    try:
        value = json.loads(output)
    except (UnicodeDecodeError, json.JSONDecodeError) as error:
        raise RuntimeError("provider returned invalid JSON") from error
    if not isinstance(value, dict) or value.get("id") != event_id:
        raise RuntimeError("provider returned the wrong calendar event")
    return value


def main(arguments: list[str]) -> int:
    if arguments[:1] in (["--version"], ["-v"]):
        sys.stdout.write("gws-calendar-body.py (fkf base helper)\n")
        return 0
    if len(arguments) != 1 or not arguments[0] or arguments[0].startswith("-"):
        sys.stderr.write("usage: gws-calendar-body.py <record-id>\n")
        return 2
    if "~" not in arguments[0]:
        sys.stderr.write("gws-calendar-body.py: record id has no calendar separator\n")
        return 2
    calendar_id, event_id = arguments[0].rsplit("~", 1)
    if not calendar_id or not event_id:
        sys.stderr.write("gws-calendar-body.py: record id has an empty provider identifier\n")
        return 2
    try:
        event = fetch(calendar_id, event_id)
        parts = []
        for key, prefix in (("description", ""), ("location", "Location: "), ("hangoutLink", "Conference: ")):
            value = event.get(key)
            if isinstance(value, str) and value:
                parts.append(prefix + value)
        output = "\n\n".join(parts)
    except (OSError, RuntimeError, TypeError, ValueError) as error:
        sys.stderr.write(f"gws-calendar-body.py: {error}\n")
        return 1
    sys.stdout.write(output + "\n")
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
