#!/usr/bin/env python3
"""Collect event-start metadata across every visible Google Calendar."""

from __future__ import annotations

import json
import subprocess
import sys
import unicodedata
from datetime import UTC, datetime
from pathlib import Path
from typing import Any
from urllib.parse import quote

MAX_PROVIDER_BYTES = 64 << 20
MAX_OUTPUT_BYTES = 64 << 20
MAX_CALENDARS = 10_000
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


def compact(value: dict[str, Any]) -> dict[str, Any]:
    return {key: item for key, item in value.items() if item is not None}


def instant(value: str) -> datetime:
    parsed = datetime.fromisoformat(value)
    if parsed.tzinfo is None:
        raise TypeError
    return parsed.astimezone(UTC)


def clean_title(value: Any) -> str:
    if not isinstance(value, str):
        return ""
    visible = "".join(
        "" if unicodedata.category(character) == "Cf" else " " if unicodedata.category(character) == "Cc" else character
        for character in value
    )
    return " ".join(visible.split())


def identity(value: str) -> str:
    return quote(value, safe="/:@+").replace("~", "%7E")


def selected(event: dict[str, Any], start: datetime, end: datetime, date: str, next_date: str) -> tuple[bool, str]:
    event_start = event.get("start") or {}
    if not isinstance(event_start, dict):
        raise TypeError
    if event_start.get("date") is not None:
        at = event_start.get("date")
        return isinstance(at, str) and date <= at < next_date, str(at)
    at = event_start.get("dateTime")
    if not isinstance(at, str):
        raise TypeError
    return start <= instant(at) < end, at


def project(event: dict[str, Any], calendar: dict[str, Any], at: str) -> dict[str, Any]:
    event_id = event.get("id")
    calendar_id = calendar["id"]
    if not isinstance(event_id, str) or not event_id:
        raise ValueError
    summary = clean_title(event.get("summary")) or f"Calendar event {quote(event_id, safe='')}"
    creator = event.get("creator") or {}
    organizer = event.get("organizer") or {}
    attendees = event.get("attendees") or []
    attachments = event.get("attachments") or []
    if (
        not isinstance(creator, dict)
        or not isinstance(organizer, dict)
        or not isinstance(attendees, list)
        or not isinstance(attachments, list)
        or any(not isinstance(item, dict) for item in [*attendees, *attachments])
    ):
        raise ValueError
    participant_emails = [organizer.get("email"), *(item.get("email") for item in attendees)]
    return compact(
        {
            **{
                key: event.get(key)
                for key in (
                    "id",
                    "status",
                    "eventType",
                    "created",
                    "updated",
                    "htmlLink",
                    "iCalUID",
                    "recurringEventId",
                    "transparency",
                    "visibility",
                )
            },
            "uid": f"{calendar_id}~{event_id}",
            "summary": summary,
            "at": at,
            "calendar": calendar,
            "start": compact({key: (event.get("start") or {}).get(key) for key in ("date", "dateTime", "timeZone")}),
            "end": compact({key: (event.get("end") or {}).get(key) for key in ("date", "dateTime", "timeZone")}),
            "creator": compact({key: creator.get(key) for key in ("email", "self")}),
            "organizer": compact({key: organizer.get(key) for key in ("email", "self")}),
            "attendees": [
                compact({key: item.get(key) for key in ("email", "responseStatus", "self", "optional", "resource")})
                for item in attendees
            ],
            "attachments": [
                compact({key: item.get(key) for key in ("fileId", "fileUrl", "title")}) for item in attachments
            ],
            "attachment_uris": sorted(
                {
                    f"document:drive.google.com/{identity(item['fileId'])}"
                    for item in attachments
                    if isinstance(item.get("fileId"), str) and item["fileId"]
                }
            ),
            "conference_id": (event.get("conferenceData") or {}).get("conferenceId")
            if isinstance(event.get("conferenceData") or {}, dict)
            else None,
            "participant_uris": sorted(
                {
                    f"person:email/{identity(email.lower())}"
                    for email in participant_emails
                    if isinstance(email, str) and email
                }
            ),
        }
    )


def main(arguments: list[str]) -> int:
    if arguments[:1] in (["--version"], ["-v"]):
        sys.stdout.write("gws-calendars-json.py (fkf base helper)\n")
        return 0
    if len(arguments) != 4:
        sys.stderr.write("usage: gws-calendars-json.py <start> <end> <date> <next-date>\n")
        return 2
    start_value, end_value, date, next_date = arguments
    try:
        start, end = instant(start_value), instant(end_value)
        helper = Path(__file__).with_name("gws-page-json.py")
        calendar_pages = documents(
            bounded(
                [
                    sys.executable,
                    "-I",
                    str(helper),
                    "items",
                    "gws",
                    "calendar",
                    "calendarList",
                    "list",
                    "--params",
                    '{"maxResults":250,"fields":"nextPageToken,items(id,summary,accessRole,primary,selected,hidden,timeZone)"}',
                    *PAGE_ALL,
                ]
            )
        )
        calendars = []
        for page in calendar_pages:
            values = page.get("items", [])
            if not isinstance(values, list) or any(not isinstance(item, dict) for item in values):
                raise ValueError
            projected_calendars = [
                compact(
                    {
                        key: item.get(key)
                        for key in ("id", "summary", "accessRole", "primary", "selected", "hidden", "timeZone")
                    }
                )
                for item in values
            ]
            if len(calendars) + len(projected_calendars) > MAX_CALENDARS:
                raise RuntimeError(f"calendar bound exceeds {MAX_CALENDARS}")
            calendars.extend(projected_calendars)
        if not calendars or any(
            not isinstance(calendar.get("id"), str) or not calendar["id"] for calendar in calendars
        ):
            raise RuntimeError("the account returned no valid calendar")
        records: list[dict[str, Any]] = []
        for calendar in calendars:
            params = json.dumps(
                {"calendarId": calendar["id"], "timeMin": start_value, "timeMax": end_value, "singleEvents": True},
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
                        "calendar",
                        "events",
                        "list",
                        "--params",
                        params,
                        *PAGE_ALL,
                    ]
                )
            )
            for page in pages:
                events = page.get("items", [])
                if not isinstance(events, list) or any(not isinstance(item, dict) for item in events):
                    raise ValueError
                for event in events:
                    keep, at = selected(event, start, end, date, next_date)
                    if keep:
                        if len(records) >= MAX_RECORDS:
                            raise RuntimeError(f"calendar-event record bound exceeds {MAX_RECORDS}")
                        records.append(project(event, calendar, at))
        unique = {record["uid"]: record for record in records}
        if len(unique) != len(records):
            raise RuntimeError("calendar pages returned duplicate event identities")
        ordered = sorted(unique.values(), key=lambda record: (record["at"], record["uid"]))
        output = (json.dumps(ordered, ensure_ascii=False, separators=(",", ":")) + "\n").encode()
        if len(output) > MAX_OUTPUT_BYTES:
            raise RuntimeError(f"output bound exceeds {MAX_OUTPUT_BYTES} bytes")
    except (OSError, RuntimeError, UnicodeError, TypeError, ValueError, json.JSONDecodeError) as error:
        sys.stderr.write(f"gws-calendars-json.py: {error}\n")
        return 1
    sys.stdout.buffer.write(output)
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
