#!/usr/bin/env python3
"""Collect Google meeting-note documents and link them to durable calendar records."""

from __future__ import annotations

import json
import os
import re
import stat
import subprocess
import sys
from datetime import datetime
from pathlib import Path
from typing import Any
from urllib.parse import quote

MAX_PROVIDER_BYTES = 64 << 20
MAX_EVIDENCE_BYTES = 64 << 20
PAGE_ALL = ("--page-all", "--page-limit", "100")
DAY_PATH = re.compile(r"^events/[0-9]{4}-[0-9]{2}-[0-9]{2}/google-calendar-events\.json$")


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


def fragment(value: str) -> str:
    return quote(value, safe="/:@+").replace("~", "%7E")


def instant(value: str) -> datetime:
    parsed = datetime.fromisoformat(value)
    if parsed.tzinfo is None:
        raise TypeError
    return parsed


def event_uri(event: dict[str, Any]) -> str:
    at = event.get("at")
    uid = event.get("uid")
    if not isinstance(at, str) or not isinstance(uid, str):
        raise TypeError
    day = at if re.fullmatch(r"[0-9]{4}-[0-9]{2}-[0-9]{2}", at) else instant(at).astimezone().date().isoformat()
    return f"events/{day}/google-calendar-events.json#{fragment(uid)}"


def file_fingerprint(value: os.stat_result) -> tuple[int, int, int, int, int, int]:
    return value.st_dev, value.st_ino, value.st_mode, value.st_size, value.st_mtime_ns, value.st_ctime_ns


def durable_calendar_document(calendar: Path) -> dict[str, Any]:
    """Read one fixed, bounded calendar evidence file and validate its envelope."""
    unavailable = "enable and sync google-calendar-events before meeting-notes"
    try:
        declared = calendar.lstat()
    except OSError as error:
        raise RuntimeError(unavailable) from error
    if not stat.S_ISREG(declared.st_mode) or declared.st_size > MAX_EVIDENCE_BYTES:
        raise RuntimeError(unavailable)
    flags = os.O_RDONLY | getattr(os, "O_CLOEXEC", 0) | getattr(os, "O_NOFOLLOW", 0)
    try:
        descriptor = os.open(calendar, flags)
    except OSError as error:
        raise RuntimeError(unavailable) from error
    try:
        opened = os.fstat(descriptor)
        if (
            not stat.S_ISREG(opened.st_mode)
            or (declared.st_dev, declared.st_ino) != (opened.st_dev, opened.st_ino)
            or opened.st_size > MAX_EVIDENCE_BYTES
        ):
            raise RuntimeError(unavailable)
        with os.fdopen(descriptor, "rb", closefd=False) as stream:
            raw = stream.read(MAX_EVIDENCE_BYTES + 1)
        if len(raw) > MAX_EVIDENCE_BYTES or file_fingerprint(opened) != file_fingerprint(os.fstat(descriptor)):
            raise RuntimeError(unavailable)
    except OSError as error:
        raise RuntimeError(unavailable) from error
    finally:
        os.close(descriptor)
    try:
        value = json.loads(raw)
    except (UnicodeError, json.JSONDecodeError) as error:
        raise RuntimeError(unavailable) from error
    fields = value.get("fields") if isinstance(value, dict) else None
    if not (
        isinstance(value, dict)
        and value.get("fkf") == 1
        and value.get("source") == "google-calendar-events"
        and isinstance(fields, dict)
        and fields.get("id") == ".uid"
        and isinstance(value.get("records"), list)
    ):
        raise RuntimeError(unavailable)
    return value


def verify_relations(base: Path, records: list[dict[str, Any]]) -> None:
    relations = sorted({uri for record in records for uri in record["meeting_uris"]})
    calendars: dict[str, dict[str, Any]] = {}
    for uri in relations:
        relative, encoded = uri.split("#", 1)
        if DAY_PATH.fullmatch(relative) is None:
            raise RuntimeError("invalid calendar relation path")
        calendar = base / relative
        if (base / "events").is_symlink() or calendar.parent.is_symlink():
            raise RuntimeError("enable and sync google-calendar-events before meeting-notes")
        value = calendars.get(relative)
        if value is None:
            value = durable_calendar_document(calendar)
            calendars[relative] = value
        durable = any(
            isinstance(item, dict) and isinstance(item.get("uid"), str) and fragment(item["uid"]) == encoded
            for item in value["records"]
        )
        if not durable:
            raise RuntimeError("enable and sync google-calendar-events before meeting-notes")


def main(arguments: list[str]) -> int:
    if arguments[:1] in (["--version"], ["-v"]):
        sys.stdout.write("gws-meeting-notes-json.py (fkf base helper)\n")
        return 0
    if len(arguments) != 6:
        sys.stderr.write("usage: gws-meeting-notes-json.py <start> <end> <date> <next-date> <name-prefix> <base>\n")
        return 2
    start, end, date, next_date, prefix, base_value = arguments
    del date, next_date
    base = Path(base_value)
    if not base.is_absolute():
        sys.stderr.write("gws-meeting-notes-json.py: base must be absolute\n")
        return 2
    if "'" in prefix or "\\" in prefix:
        sys.stderr.write("gws-meeting-notes-json.py: name prefix may not contain quote or backslash\n")
        return 2
    try:
        query = (
            "mimeType='application/vnd.google-apps.document' and trashed=false "
            f"and createdTime >= '{start}' and createdTime < '{end}' "
            f"and (name contains ' - Notes by Gemini' or name contains '{prefix}')"
        )
        params = json.dumps(
            {
                "q": query,
                "pageSize": 1000,
                "orderBy": "createdTime asc",
                "fields": "nextPageToken,files(id,name,createdTime,modifiedTime,webViewLink,owners(emailAddress,me))",
            },
            separators=(",", ":"),
        )
        page_helper = Path(__file__).with_name("gws-page-json.py")
        drive_pages = documents(
            bounded(
                [
                    sys.executable,
                    "-I",
                    os.fspath(page_helper),
                    "files",
                    "gws",
                    "drive",
                    "files",
                    "list",
                    "--params",
                    params,
                    *PAGE_ALL,
                ]
            )
        )
        calendar_helper = Path(__file__).with_name("gws-calendars-json.py")
        events_value = json.loads(
            bounded([sys.executable, "-I", os.fspath(calendar_helper), start, end, arguments[2], arguments[3]])
        )
        if not isinstance(events_value, list) or any(not isinstance(item, dict) for item in events_value):
            raise ValueError
        files: dict[str, dict[str, Any]] = {}
        for page in drive_pages:
            values = page.get("files", [])
            if not isinstance(values, list) or any(not isinstance(item, dict) for item in values):
                raise ValueError
            for item in values:
                identifier = item.get("id")
                created = item.get("createdTime")
                if isinstance(identifier, str) and isinstance(created, str) and start <= created < end:
                    files.setdefault(identifier, item)
        records: list[dict[str, Any]] = []
        for file in files.values():
            attached = [
                event
                for event in events_value
                if any(
                    isinstance(attachment, dict) and attachment.get("fileId") == file["id"]
                    for attachment in event.get("attachments", [])
                )
            ]
            meetings = attached
            if not meetings:
                candidates = []
                created = instant(file["createdTime"])
                for event in events_value:
                    if event.get("attachments"):
                        continue
                    summary, at = event.get("summary"), event.get("at")
                    if (
                        isinstance(summary, str)
                        and summary
                        and isinstance(file.get("name"), str)
                        and file["name"].startswith(summary + " - ")
                        and isinstance(at, str)
                        and "T" in at
                    ):
                        distance = abs((instant(at) - created).total_seconds())
                        candidates.append((distance, str(event.get("uid", "")), event))
                candidates.sort(key=lambda item: (item[0], item[1]))
                if candidates and candidates[0][0] <= 21_600:
                    meetings = [candidates[0][2]]
            owners = file.get("owners") or []
            if not isinstance(owners, list) or any(not isinstance(owner, dict) for owner in owners):
                raise ValueError
            record = {
                "id": file["id"],
                "at": file["createdTime"],
                "modified": file.get("modifiedTime"),
                "title": file.get("name"),
                "url": file.get("webViewLink"),
                "owner_uris": sorted(
                    {
                        f"person:email/{fragment(owner['emailAddress'].lower())}"
                        for owner in owners
                        if isinstance(owner.get("emailAddress"), str) and owner["emailAddress"]
                    }
                ),
                "attachment_uris": [f"document:drive.google.com/{fragment(file['id'])}"],
                "meeting_uris": sorted({event_uri(event) for event in meetings}),
            }
            records.append({key: value for key, value in record.items() if value is not None})
        records.sort(key=lambda record: (record["at"], record["id"]))
        verify_relations(base, records)
        output = json.dumps(records, ensure_ascii=False, separators=(",", ":"))
    except RuntimeError as error:
        sys.stderr.write(f"gws-meeting-notes-json.py: {error}\n")
        return 1
    except (OSError, UnicodeError, TypeError, ValueError, json.JSONDecodeError) as error:
        sys.stderr.write(f"gws-meeting-notes-json.py: {error}\n")
        return 1
    sys.stdout.write(output + "\n")
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
