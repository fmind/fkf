#!/usr/bin/env python3
"""Collect guarded public activity for the authenticated GitHub user."""

from __future__ import annotations

import json
import re
import subprocess
import sys
from datetime import UTC, datetime, timedelta
from typing import Any

MAX_PROVIDER_BYTES = 64 << 20
MAX_OUTPUT_BYTES = 64 << 20
USER = re.compile(r"^[A-Za-z0-9][A-Za-z0-9-]*$")


def instant(value: Any) -> datetime:
    if not isinstance(value, str):
        raise TypeError
    parsed = datetime.fromisoformat(value)
    if parsed.tzinfo is None:
        raise TypeError
    return parsed.astimezone(UTC)


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


def validated(event: Any) -> dict[str, Any]:
    if not isinstance(event, dict):
        raise TypeError
    actor = event.get("actor")
    repository = event.get("repo")
    organization = event.get("org")
    if (
        not isinstance(event.get("id"), str)
        or not event["id"]
        or not isinstance(event.get("type"), str)
        or not event["type"]
        or not isinstance(event.get("created_at"), str)
        or not isinstance(event.get("public"), bool)
        or not isinstance(actor, dict)
        or not isinstance(actor.get("login"), str)
        or not actor["login"]
        or not isinstance(repository, dict)
        or not isinstance(repository.get("name"), str)
        or not repository["name"]
        or (
            organization is not None
            and (
                not isinstance(organization, dict)
                or not isinstance(organization.get("login"), str)
                or not organization["login"]
            )
        )
    ):
        raise ValueError
    instant(event["created_at"])
    return event


def main(arguments: list[str]) -> int:
    if arguments[:1] in (["--version"], ["-v"]):
        sys.stdout.write("github-events-json.py (fkf base helper)\n")
        return 0
    if len(arguments) != 2:
        sys.stderr.write("usage: github-events-json.py <start> <end>\n")
        return 2
    try:
        start, end = map(instant, arguments)
    except TypeError, ValueError:
        bad = "start" if _valid(arguments[0]) is False else "end"
        sys.stderr.write(
            f"github-events-json.py: {bad} is not an RFC3339 timestamp: {arguments[0 if bad == 'start' else 1]}\n"
        )
        return 2
    if start >= end:
        sys.stderr.write("github-events-json.py: start must be before end\n")
        return 2
    now = datetime.now(UTC)
    if start < now - timedelta(days=30):
        sys.stderr.write("github-events-json.py: requested start predates GitHub's 30-day event retention\n")
        return 1
    if end > now - timedelta(hours=6):
        sys.stderr.write("github-events-json.py: requested end is inside GitHub's documented six-hour latency window\n")
        return 1
    try:
        try:
            user = invoke(["api", "/user", "--jq", ".login"]).decode().strip()
        except (OSError, RuntimeError, UnicodeError) as error:
            raise RuntimeError("cannot identify the authenticated GitHub user") from error
        if USER.fullmatch(user) is None:
            sys.stderr.write(f"github-events-json.py: invalid GitHub user: {user}\n")
            return 2
        events: list[dict[str, Any]] = []
        for page in range(1, 4):
            try:
                value = json.loads(
                    invoke(
                        [
                            "api",
                            "--method",
                            "GET",
                            f"/users/{user}/events",
                            "-f",
                            "per_page=100",
                            "-f",
                            f"page={page}",
                        ]
                    )
                )
            except RuntimeError as error:
                raise RuntimeError(f"cannot list events for {user} on page {page}") from error
            if not isinstance(value, list) or len(value) > 100:
                raise RuntimeError("GitHub returned an invalid event page")
            events.extend(validated(event) for event in value)
            if len(value) < 100:
                break
        identifiers = [event["id"] for event in events]
        if len(identifiers) != len(set(identifiers)):
            raise RuntimeError("every event must be unique and carry the complete metadata projection")
        if len(events) >= 300:
            cutoff = events[-1]["created_at"]
            if instant(cutoff) >= start:
                raise RuntimeError(
                    f"the 300-event feed cuts off at {cutoff}, at or after requested start {arguments[0]}; "
                    "completeness cannot be proved"
                )
        records = []
        for event in events:
            if not start <= instant(event["created_at"]) < end:
                continue
            actor = event["actor"]["login"]
            repository = event["repo"]["name"]
            organization = event.get("org")
            records.append(
                {
                    "id": event["id"],
                    "type": event["type"],
                    "title": f"{event['type']} in {repository}",
                    "created_at": event["created_at"],
                    "public": event["public"],
                    "actor": actor,
                    "repo": repository,
                    "repository_uri": f"repo:github.com/{repository}",
                    "participant_uris": [f"actor:github.com/{actor}"],
                    "org": organization.get("login") if isinstance(organization, dict) else None,
                }
            )
        records.sort(key=lambda record: (record["created_at"], record["id"]))
        output = (json.dumps(records, ensure_ascii=False, separators=(",", ":")) + "\n").encode()
        if len(output) > MAX_OUTPUT_BYTES:
            raise RuntimeError(f"output bound exceeds {MAX_OUTPUT_BYTES} bytes")
    except TypeError, ValueError:
        sys.stderr.write(
            "github-events-json.py: every event must be unique and carry the complete metadata projection\n"
        )
        return 1
    except (OSError, RuntimeError, UnicodeError, json.JSONDecodeError) as error:
        sys.stderr.write(f"github-events-json.py: {error}\n")
        return 1
    sys.stdout.buffer.write(output)
    return 0


def _valid(value: str) -> bool:
    try:
        instant(value)
    except ValueError:
        return False
    return True


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
