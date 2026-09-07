#!/usr/bin/env python3
"""Collect projected Gmail metadata in an exact half-open UTC window."""

from __future__ import annotations

import email.utils
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
MAX_RECORDS = 10_000
PAGE_ALL = ("--page-all", "--page-limit", "100")


def instant(value: str) -> datetime:
    parsed = datetime.fromisoformat(value)
    if parsed.tzinfo is None:
        raise ValueError("timestamp must include an offset")
    return parsed.astimezone(UTC)


def bounded(command: list[str]) -> bytes:
    if command[:1] == ["gws"]:
        process = subprocess.Popen(["gws", *command[1:]], stdout=subprocess.PIPE)
    elif command[:1] == [sys.executable]:
        process = subprocess.Popen(["/usr/bin/env", "python3", *command[1:]], stdout=subprocess.PIPE)
    else:
        raise RuntimeError("unexpected helper executable")
    with process:
        if process.stdout is None:  # pragma: no cover
            raise RuntimeError("cannot capture provider output")
        output = process.stdout.read(MAX_PROVIDER_BYTES + 1)
        if len(output) > MAX_PROVIDER_BYTES:
            process.kill()
            process.wait()
            raise RuntimeError("provider response exceeds FKF's 64 MiB command bound")
        if process.wait() != 0:
            raise RuntimeError("provider command failed")
    return output


def documents(raw: bytes) -> list[Any]:
    text = raw.decode()
    decoder = json.JSONDecoder()
    offset = 0
    values: list[Any] = []
    while offset < len(text):
        while offset < len(text) and text[offset].isspace():
            offset += 1
        if offset == len(text):
            break
        value, offset = decoder.raw_decode(text, offset)
        values.append(value)
    return values


def clean_title(value: Any) -> str:
    if not isinstance(value, str):
        return ""
    visible = "".join(
        "" if unicodedata.category(character) == "Cf" else " " if unicodedata.category(character) == "Cc" else character
        for character in value
    )
    return " ".join(visible.split())


def address_uris(values: list[Any]) -> list[str]:
    addresses: set[str] = set()
    for value in values:
        if not isinstance(value, str):
            continue
        for _, address in email.utils.getaddresses([value]):
            lowered = address.lower()
            if lowered.count("@") == 1 and not any(character.isspace() for character in lowered):
                addresses.add("person:email/" + quote(lowered, safe="/:@+").replace("~", "%7E"))
    return sorted(addresses)


def project(message: Any, start_ms: int, end_ms: int) -> dict[str, Any] | None:
    if not isinstance(message, dict):
        raise TypeError("message must be an object")
    internal = int(message.get("internalDate"))
    if not start_ms <= internal < end_ms:
        return None
    payload = message.get("payload") or {}
    headers = payload.get("headers") if isinstance(payload, dict) else None
    if not isinstance(headers, list) or any(not isinstance(item, dict) for item in headers):
        raise ValueError("message headers must be an array")
    mapped = {str(item.get("name", "")).lower(): item.get("value") for item in headers}
    subject = clean_title(mapped.get("subject")) or "Email without subject"
    recipients = [value for key in ("to", "cc") if (value := mapped.get(key)) is not None]
    return {
        "id": message.get("id"),
        "threadId": message.get("threadId"),
        "internalDate": message.get("internalDate"),
        "labelIds": message.get("labelIds"),
        "sizeEstimate": message.get("sizeEstimate"),
        "subject": subject,
        "from": mapped.get("from"),
        "to": [address for value in recipients for _, address in email.utils.getaddresses([str(value)])],
        "list_id": mapped.get("list-id"),
        "participant_uris": address_uris([mapped.get("from"), *recipients]),
    }


def main(arguments: list[str]) -> int:
    if len(arguments) != 2:
        sys.stderr.write("usage: gmail-json.py <start> <end>\n")
        return 2
    try:
        start, end = map(instant, arguments)
        start_ms = int(start.timestamp()) * 1000
        end_ms = int(end.timestamp()) * 1000
        search_after = int(start.timestamp()) - 1
        search_before = int(end.timestamp())
        params = json.dumps(
            {"userId": "me", "q": f"after:{search_after} before:{search_before}"}, separators=(",", ":")
        )
        page_helper = Path(__file__).with_name("gws-page-json.py")
        pages = bounded(
            [
                sys.executable,
                "-I",
                str(page_helper),
                "messages",
                "gws",
                "gmail",
                "users",
                "messages",
                "list",
                "--params",
                params,
                *PAGE_ALL,
            ]
        )
        identifiers: list[str] = []
        seen_identifiers: set[str] = set()
        for page in documents(pages):
            if not isinstance(page, dict) or not isinstance(page.get("messages", []), list):
                raise TypeError("invalid message listing")
            for item in page.get("messages", []):
                if not isinstance(item, dict) or not isinstance(item.get("id"), str):
                    raise TypeError("invalid message identifier")
                identifier = item["id"]
                if identifier in seen_identifiers:
                    raise RuntimeError("message listing returned duplicate identifiers")
                if len(identifiers) >= MAX_RECORDS:
                    raise RuntimeError(f"message record bound exceeds {MAX_RECORDS}")
                seen_identifiers.add(identifier)
                identifiers.append(identifier)
        records: list[dict[str, Any]] = []
        for identifier in identifiers:
            get_params = json.dumps({"userId": "me", "id": identifier, "format": "metadata"}, separators=(",", ":"))
            message = json.loads(bounded(["gws", "gmail", "users", "messages", "get", "--params", get_params]))
            record = project(message, start_ms, end_ms)
            if record is not None:
                records.append(record)
        output = "".join(
            json.dumps(record, ensure_ascii=False, separators=(",", ":")) + "\n" for record in records
        ).encode()
        if len(output) > MAX_OUTPUT_BYTES:
            raise RuntimeError(f"output bound exceeds {MAX_OUTPUT_BYTES} bytes")
    except (OSError, RuntimeError, UnicodeError, TypeError, ValueError, json.JSONDecodeError) as error:
        sys.stderr.write(f"gmail-json.py: {error}\n")
        return 1
    sys.stdout.buffer.write(output)
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
