#!/usr/bin/env python3
"""Collect every GitHub commit authored by the active user through finite range bisection."""

from __future__ import annotations

import json
import re
import subprocess
import sys
import unicodedata
from dataclasses import dataclass
from datetime import UTC, datetime
from typing import Any
from urllib.parse import quote

MAX_PROVIDER_BYTES = 64 << 20
MAX_OUTPUT_BYTES = 64 << 20
MAX_PROVIDER_CALLS = 10_000
MAX_RECORDS = 100_000
RFC3339 = re.compile(r"^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(?:Z|[+-][0-9]{2}:[0-9]{2})$")
NOREPLY = re.compile(r"^(?:[0-9]+\+)?(?P<login>[a-z0-9](?:[a-z0-9-]{0,37}[a-z0-9])?)@users\.noreply\.github\.com$")


@dataclass
class Budget:
    """Bound every provider call shared by recursive range splits."""

    provider_calls: int = 0

    def invoke(self, arguments: list[str]) -> bytes:
        self.provider_calls += 1
        if self.provider_calls > MAX_PROVIDER_CALLS:
            raise RuntimeError(f"provider-call bound exceeds {MAX_PROVIDER_CALLS}")
        return invoke(arguments)


def instant(value: str) -> datetime:
    if RFC3339.fullmatch(value) is None:
        raise ValueError
    parsed = datetime.fromisoformat(value)
    if parsed.tzinfo is None:
        raise ValueError
    return parsed.astimezone(UTC).replace(microsecond=0)


def stamp(value: int) -> str:
    return datetime.fromtimestamp(value, UTC).isoformat().replace("+00:00", "Z")


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


def clean_title(value: Any, sha: str) -> str:
    if not isinstance(value, str):
        value = ""
    for line in value.splitlines():
        visible = "".join(
            ""
            if unicodedata.category(character) == "Cf"
            else " "
            if unicodedata.category(character) == "Cc"
            else character
            for character in line
        )
        cleaned = " ".join(visible.split())
        if cleaned:
            return cleaned[:160]
    return f"Commit {sha[:12]}"[:160]


def participant(email: str) -> str:
    lowered = email.lower()
    match = NOREPLY.fullmatch(lowered)
    if match is not None:
        return f"actor:github.com/{match['login']}"
    return "person:email/" + quote(lowered, safe="/:@+").replace("~", "%7E")


def validate(record: Any) -> dict[str, Any]:
    if not isinstance(record, dict):
        raise TypeError
    commit = record.get("commit")
    repository = record.get("repository")
    author = commit.get("author") if isinstance(commit, dict) else None
    if (
        not isinstance(commit, dict)
        or not isinstance(repository, dict)
        or not isinstance(author, dict)
        or not isinstance(record.get("url"), str)
        or not record["url"]
        or not isinstance(record.get("sha"), str)
        or not record["sha"]
        or not isinstance(author.get("date"), str)
        or not author["date"]
        or not isinstance(repository.get("fullName"), str)
        or not repository["fullName"]
    ):
        raise ValueError
    email = author.get("email")
    return {
        **record,
        "title": clean_title(commit.get("message"), record["sha"]),
        "repository_uri": f"repo:github.com/{repository['fullName']}",
        "participant_uris": [participant(email)] if isinstance(email, str) and email else [],
    }


def collect(start: int, end: int, budget: Budget | None = None) -> list[dict[str, Any]]:
    budget = budget or Budget()
    range_start = stamp(start)
    range_end = stamp(end - 1)
    try:
        value = json.loads(
            budget.invoke(
                [
                    "search",
                    "commits",
                    "--author=@me",
                    f"--author-date={range_start}..{range_end}",
                    "--json",
                    "sha,repository,commit,url",
                    "--limit",
                    "1000",
                ]
            )
        )
    except RuntimeError as error:
        raise RuntimeError(f"GitHub search failed for [{range_start}, {range_end}]: {error}") from error
    if not isinstance(value, list):
        raise TypeError("GitHub search did not return one JSON array")
    try:
        records = [validate(record) for record in value]
    except (TypeError, ValueError) as error:
        raise RuntimeError("every result must carry a URL, SHA, and author time") from error
    # GitHub preserves author offsets; lexical RFC3339 order is not instant order.
    if any(not start <= instant(record["commit"]["author"]["date"]).timestamp() < end for record in records):
        raise RuntimeError(f"GitHub returned a commit outside the requested range [{range_start}, {range_end}]")
    if len(records) > MAX_RECORDS:
        raise RuntimeError(f"commit record bound exceeds {MAX_RECORDS}")
    if len(records) < 1000:
        return records
    if end - start <= 1:
        raise RuntimeError(
            f"one-second slice [{range_start}, {range_end}] returned 1000 results; cannot prove completeness"
        )
    midpoint = start + (end - start) // 2
    combined = [*collect(start, midpoint, budget), *collect(midpoint, end, budget)]
    if len(combined) > MAX_RECORDS:
        raise RuntimeError(f"commit record bound exceeds {MAX_RECORDS}")
    return combined


def main(arguments: list[str]) -> int:
    if arguments[:1] in (["--version"], ["-v"]):
        sys.stdout.write("github-commits-json.py (fkf base helper)\n")
        return 0
    if len(arguments) != 2:
        sys.stderr.write("usage: github-commits-json.py <start> <end>\n")
        return 2
    try:
        try:
            start = instant(arguments[0])
        except ValueError:
            sys.stderr.write(f"github-commits-json.py: start is not an RFC3339 timestamp: {arguments[0]}\n")
            return 2
        try:
            end = instant(arguments[1])
        except ValueError:
            sys.stderr.write(f"github-commits-json.py: end is not an RFC3339 timestamp: {arguments[1]}\n")
            return 2
        if start >= end:
            sys.stderr.write("github-commits-json.py: start must be before end\n")
            return 2
        records = collect(int(start.timestamp()), int(end.timestamp()), Budget())
        unique = {record["url"]: record for record in sorted(records, key=lambda record: record["url"])}
        output = (json.dumps(list(unique.values()), ensure_ascii=False, separators=(",", ":")) + "\n").encode()
        if len(output) > MAX_OUTPUT_BYTES:
            raise RuntimeError(f"output bound exceeds {MAX_OUTPUT_BYTES} bytes")
    except (OSError, RuntimeError, UnicodeError, TypeError, ValueError, json.JSONDecodeError) as error:
        sys.stderr.write(f"github-commits-json.py: {error}\n")
        return 1
    sys.stdout.buffer.write(output)
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
