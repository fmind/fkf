#!/usr/bin/env python3
"""Collect bounded GitHub notification or repository lists using Link pagination."""

from __future__ import annotations

import json
import re
import subprocess
import sys
from datetime import datetime
from typing import Any

PAGE_SIZE = 100
MAX_PAGES = 100
MAX_PROVIDER_BYTES = 64 << 20
MAX_OUTPUT_BYTES = 64 << 20
MAX_RECORDS = PAGE_SIZE * MAX_PAGES
ORG = re.compile(r"^[A-Za-z0-9_.-]+$")


def invoke(arguments: list[str]) -> bytes:
    with subprocess.Popen(
        ["gh", *arguments],
        stdout=subprocess.PIPE,
    ) as process:
        if process.stdout is None:  # pragma: no cover
            raise RuntimeError
        raw = process.stdout.read(MAX_PROVIDER_BYTES + 1)
        if len(raw) > MAX_PROVIDER_BYTES:
            process.kill()
            process.wait()
            raise RuntimeError
        if process.wait() != 0:
            raise RuntimeError
    return raw


def response(raw: bytes) -> tuple[str, list[Any]]:
    text = raw.decode()
    match = re.search(r"\r?\n[ \t]*\r?\n", text)
    if match is None:
        raise TypeError
    headers = text[: match.start()]
    body = json.loads(text[match.end() :])
    if not isinstance(body, list):
        raise TypeError
    return headers, body


def compact(value: dict[str, Any]) -> dict[str, Any]:
    return {key: item for key, item in value.items() if item is not None}


def valid_time(value: Any) -> bool:
    if not isinstance(value, str):
        return False
    try:
        parsed = datetime.fromisoformat(value)
    except ValueError:
        return False
    return parsed.tzinfo is not None


def project_notification(item: Any, start: str, end: str) -> dict[str, Any] | None:
    if not isinstance(item, dict) or not valid_time(item.get("updated_at")):
        raise RuntimeError("every notification must carry a valid updated_at timestamp")
    if not start <= item["updated_at"] < end:
        return None
    subject = item.get("subject") or {}
    repository = item.get("repository") or {}
    if not isinstance(subject, dict) or not isinstance(repository, dict):
        raise TypeError
    full_name = repository.get("full_name")
    return compact(
        {
            **{key: item.get(key) for key in ("id", "unread", "reason", "updated_at", "last_read_at")},
            "subject": compact({key: subject.get(key) for key in ("title", "url", "latest_comment_url", "type")}),
            "repository": compact({key: repository.get(key) for key in ("full_name", "html_url")}),
            "repository_uri": f"repo:github.com/{full_name}" if isinstance(full_name, str) and full_name else None,
        }
    )


def project_repository(item: Any) -> dict[str, Any]:
    if not isinstance(item, dict):
        raise TypeError
    full_name = item.get("full_name")
    if not isinstance(full_name, str) or not full_name:
        raise ValueError
    return {
        "nameWithOwner": full_name,
        "title": full_name,
        "url": item.get("html_url"),
        "repository_uri": f"repo:github.com/{full_name}",
        "updatedAt": item.get("updated_at"),
        "isArchived": item.get("archived"),
    }


def main(arguments: list[str]) -> int:
    mode = arguments[:1]
    if len(arguments) == 3 and mode == ["notifications"]:
        start, end = arguments[1:]
        org = None
    elif len(arguments) == 1 and mode == ["user-repositories"]:
        start = end = ""
        org = None
    elif len(arguments) == 2 and mode == ["org-repositories"] and ORG.fullmatch(arguments[1]) is not None:
        start = end = ""
        org = arguments[1]
    else:
        if mode == ["org-repositories"] and len(arguments) == 2:
            sys.stderr.write("github-list-json.py: invalid organization\n")
        else:
            sys.stderr.write(
                "usage: github-list-json.py <notifications start end|user-repositories|org-repositories org>\n"
            )
        return 2
    records: list[dict[str, Any]] = []
    try:
        for page in range(1, MAX_PAGES + 1):
            if mode == ["notifications"]:
                command = [
                    "api",
                    "--method",
                    "GET",
                    "--include",
                    "/notifications",
                    "-F",
                    "all=true",
                    "-f",
                    f"since={start}",
                    "-f",
                    f"before={end}",
                    "-F",
                    f"per_page={PAGE_SIZE}",
                    "-F",
                    f"page={page}",
                ]
                failure = f"GitHub notifications page {page} failed"
            elif mode == ["user-repositories"]:
                command = [
                    "api",
                    "--method",
                    "GET",
                    "--include",
                    "/user/repos",
                    "-f",
                    "sort=updated",
                    "-f",
                    "direction=desc",
                    "-F",
                    f"per_page={PAGE_SIZE}",
                    "-F",
                    f"page={page}",
                ]
                failure = f"GitHub repository page {page} failed"
            else:
                command = [
                    "api",
                    "--method",
                    "GET",
                    "--include",
                    f"/orgs/{org}/repos",
                    "-f",
                    "type=all",
                    "-f",
                    "sort=updated",
                    "-f",
                    "direction=desc",
                    "-F",
                    f"per_page={PAGE_SIZE}",
                    "-F",
                    f"page={page}",
                ]
                failure = f"GitHub organization repository page {page} failed"
            try:
                headers, items = response(invoke(command))
            except (RuntimeError, OSError) as error:
                raise RuntimeError(failure) from error
            except (UnicodeError, ValueError, json.JSONDecodeError) as error:
                raise RuntimeError(f"GitHub page {page} was not a JSON array") from error
            if len(items) > PAGE_SIZE:
                raise RuntimeError(f"GitHub page {page} exceeded {PAGE_SIZE} items")
            if mode == ["notifications"]:
                projected = [record for item in items if (record := project_notification(item, start, end)) is not None]
            else:
                projected = [project_repository(item) for item in items]
            if len(records) + len(projected) > MAX_RECORDS:
                raise RuntimeError(f"listing record bound exceeds {MAX_RECORDS}")
            records.extend(projected)
            has_next = any(line.lower().startswith("link:") and 'rel="next"' in line for line in headers.splitlines())
            if not has_next:
                output = "".join(
                    json.dumps(record, ensure_ascii=False, separators=(",", ":")) + "\n" for record in records
                ).encode()
                if len(output) > MAX_OUTPUT_BYTES:
                    raise RuntimeError(f"output bound exceeds {MAX_OUTPUT_BYTES} bytes")
                sys.stdout.buffer.write(output)
                return 0
        raise RuntimeError("reached the 100-page safety limit with rel=next; cannot prove completeness")
    except (OSError, RuntimeError, UnicodeError, TypeError, ValueError, json.JSONDecodeError) as error:
        sys.stderr.write(f"github-list-json.py: {error}\n")
        return 1


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
