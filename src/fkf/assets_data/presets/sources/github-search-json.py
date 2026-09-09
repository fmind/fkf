#!/usr/bin/env python3
"""Collect complete GitHub issue searches through finite paging and range bisection."""

from __future__ import annotations

import json
import re
import subprocess
import sys
from dataclasses import dataclass
from datetime import UTC, datetime
from typing import Any

MAX_PROVIDER_BYTES = 64 << 20
MAX_OUTPUT_BYTES = 64 << 20
MAX_PROVIDER_CALLS = 10_000
MAX_RECORDS = 100_000
MAX_PAGES = 10
RFC3339 = re.compile(r"^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(?:Z|[+-][0-9]{2}:[0-9]{2})$")
REPOSITORY_SCOPE = re.compile(r"^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$")
REPOSITORY_URL = re.compile(r"/repos/(?P<repo>[^/]+/[^/]+)$")
PROJECTION = (
    "{total_count,incomplete_results,items:[.items[] | "
    "{number,title,html_url,updated_at,repository_url,state,"
    "user:(if .user == null then null else {login:.user.login} end),"
    "assignees:[.assignees[]? | {login}]}]}"
)


@dataclass
class Budget:
    """Bound every provider call shared by paging and recursive range splits."""

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
    if parsed.tzinfo is None or parsed.microsecond:
        raise ValueError
    return parsed.astimezone(UTC)


def stamp(epoch: int) -> str:
    return datetime.fromtimestamp(epoch, UTC).isoformat().replace("+00:00", "Z")


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


def envelope(value: Any) -> dict[str, Any]:
    if not isinstance(value, dict):
        raise TypeError
    total = value.get("total_count")
    incomplete = value.get("incomplete_results")
    items = value.get("items")
    if (
        not isinstance(total, int)
        or isinstance(total, bool)
        or total < 0
        or not isinstance(incomplete, bool)
        or not isinstance(items, list)
    ):
        raise ValueError
    return value


def validate_item(value: Any) -> tuple[dict[str, Any], str]:
    if not isinstance(value, dict):
        raise TypeError
    number = value.get("number")
    user = value.get("user")
    assignees = value.get("assignees", [])
    repository_url = value.get("repository_url")
    match = REPOSITORY_URL.search(repository_url) if isinstance(repository_url, str) else None
    if (
        not isinstance(number, int)
        or isinstance(number, bool)
        or not isinstance(value.get("title"), str)
        or not isinstance(value.get("html_url"), str)
        or not value["html_url"]
        or not isinstance(value.get("updated_at"), str)
        or not value["updated_at"]
        or match is None
        or not isinstance(value.get("state"), str)
        or (
            user is not None
            and (not isinstance(user, dict) or not isinstance(user.get("login"), str) or not user["login"])
        )
        or not isinstance(assignees, list)
        or any(
            not isinstance(assignee, dict) or not isinstance(assignee.get("login"), str) or not assignee["login"]
            for assignee in assignees
        )
    ):
        raise ValueError
    return value, match["repo"]


def projected(value: dict[str, Any], repository: str) -> dict[str, Any]:
    user = value.get("user")
    author = user.get("login") if isinstance(user, dict) else None
    assignees = [item["login"] for item in value.get("assignees", [])]
    participants = sorted({login for login in [author, *assignees] if login is not None})
    return {
        "number": value["number"],
        "title": value["title"],
        "url": value["html_url"],
        "updatedAt": value["updated_at"],
        "repository": {"nameWithOwner": repository},
        "repository_uri": f"repo:github.com/{repository}",
        "state": value["state"],
        "author": {"login": author} if author is not None else None,
        "assignee_uris": [f"actor:github.com/{login}" for login in assignees],
        "participant_uris": [f"actor:github.com/{login}" for login in participants],
    }


def collect(
    start: int,
    end: int,
    type_qualifier: str,
    qualifier: str,
    scope: str | None,
    budget: Budget | None = None,
) -> list[dict[str, Any]]:
    budget = budget or Budget()
    range_start, range_end = stamp(start), stamp(end - 1)
    query = f"{type_qualifier} {qualifier} updated:{range_start}..{range_end}"
    pages: list[dict[str, Any]] = []
    retrieved_so_far = 0
    for page_number in range(1, MAX_PAGES + 1):
        try:
            page = envelope(
                json.loads(
                    budget.invoke(
                        [
                            "api",
                            "--method",
                            "GET",
                            "/search/issues",
                            "-H",
                            "Accept: application/vnd.github+json",
                            "-H",
                            "X-GitHub-Api-Version: 2026-03-10",
                            "-f",
                            f"q={query}",
                            "-f",
                            "sort=updated",
                            "-f",
                            "order=asc",
                            "-F",
                            "per_page=100",
                            "-F",
                            f"page={page_number}",
                            "--jq",
                            PROJECTION,
                        ]
                    )
                )
            )
        except RuntimeError as error:
            raise RuntimeError(
                f"GitHub REST search page {page_number} failed for [{range_start}, {range_end}]: {error}"
            ) from error
        except (UnicodeError, TypeError, ValueError, json.JSONDecodeError) as error:
            raise RuntimeError(
                f"GitHub REST search returned an invalid page for [{range_start}, {range_end}]"
            ) from error
        if page["incomplete_results"]:
            raise RuntimeError(
                f"GitHub REST search returned incomplete_results=true for [{range_start}, {range_end}]; "
                "cannot prove completeness"
            )
        if len(page["items"]) > 100:
            raise RuntimeError(
                f"GitHub REST search page {page_number} exceeded 100 items for [{range_start}, {range_end}]"
            )
        pages.append(page)
        total = page["total_count"]
        if total >= 1000:
            break
        retrieved_so_far += len(page["items"])
        if retrieved_so_far >= total or not page["items"]:
            break
    totals = {page["total_count"] for page in pages}
    if len(totals) != 1:
        raise RuntimeError(
            f"GitHub REST search changed total_count between pages for [{range_start}, {range_end}]; cannot prove completeness"
        )
    total = next(iter(totals))
    if total > MAX_RECORDS:
        raise RuntimeError(f"issue-search record bound exceeds {MAX_RECORDS}")
    if total >= 1000:
        if end - start <= 1:
            raise RuntimeError(
                f"one-second slice [{range_start}, {range_end}] reported {total} results; cannot prove completeness"
            )
        midpoint = start + (end - start) // 2
        combined = [
            *collect(start, midpoint, type_qualifier, qualifier, scope, budget),
            *collect(midpoint, end, type_qualifier, qualifier, scope, budget),
        ]
        if len(combined) > MAX_RECORDS:
            raise RuntimeError(f"issue-search record bound exceeds {MAX_RECORDS}")
        return combined
    items = [item for page in pages for item in page["items"]]
    try:
        validated = [validate_item(item) for item in items]
    except ValueError as error:
        raise RuntimeError(
            f"GitHub REST search returned an invalid issue item for [{range_start}, {range_end}]"
        ) from error
    urls = [item[0]["html_url"] for item in validated]
    if len(items) != total:
        raise RuntimeError(
            f"retrieved {len(items)} of {total} results for [{range_start}, {range_end}]; cannot prove completeness"
        )
    if len(set(urls)) != total:
        raise RuntimeError(
            f"retrieved {len(items)} results but only {len(set(urls))} unique URLs for "
            f"[{range_start}, {range_end}]; cannot prove completeness"
        )
    if scope is not None and any(repository != scope for _, repository in validated):
        raise RuntimeError(f"GitHub returned a result outside repository {scope}")
    return [projected(item, repository) for item, repository in validated]


def main(arguments: list[str]) -> int:
    if arguments[:1] in (["--version"], ["-v"]):
        sys.stdout.write("github-search-json.py (fkf preset helper)\n")
        return 0
    if len(arguments) not in {4, 5}:
        sys.stderr.write("usage: github-search-json.py <prs|issues> <assignee|repository scope> <start> <end>\n")
        return 2
    kind, mode = arguments[:2]
    scope: str | None = None
    if mode == "repository":
        if len(arguments) != 5:
            sys.stderr.write("github-search-json.py: repository mode needs one owner/name scope\n")
            return 2
        scope = arguments[2]
        if REPOSITORY_SCOPE.fullmatch(scope) is None:
            sys.stderr.write("github-search-json.py: invalid repository scope\n")
            return 2
        start_value, end_value = arguments[3:]
        qualifier = f"repo:{scope}"
    else:
        if len(arguments) != 4:
            sys.stderr.write("github-search-json.py: assignee mode takes no scope\n")
            return 2
        start_value, end_value = arguments[2:]
        qualifier = "assignee:@me"
    if (kind, mode) not in {
        ("prs", "assignee"),
        ("prs", "repository"),
        ("issues", "assignee"),
        ("issues", "repository"),
    }:
        sys.stderr.write("github-search-json.py: expected prs assignee or issues assignee\n")
        return 2
    type_qualifier = "is:pr" if kind == "prs" else "is:issue"
    try:
        start, end = instant(start_value), instant(end_value)
    except ValueError:
        sys.stderr.write("github-search-json.py: start or end is not an RFC3339 timestamp\n")
        return 2
    if start >= end:
        sys.stderr.write("github-search-json.py: start must be before end\n")
        return 2
    try:
        records = collect(int(start.timestamp()), int(end.timestamp()), type_qualifier, qualifier, scope, Budget())
        unique = {record["url"]: record for record in sorted(records, key=lambda record: record["url"])}
        output = (json.dumps(list(unique.values()), ensure_ascii=False, separators=(",", ":")) + "\n").encode()
        if len(output) > MAX_OUTPUT_BYTES:
            raise RuntimeError(f"output bound exceeds {MAX_OUTPUT_BYTES} bytes")
    except (OSError, RuntimeError, UnicodeError, TypeError, ValueError, json.JSONDecodeError) as error:
        sys.stderr.write(f"github-search-json.py: {error}\n")
        return 1
    sys.stdout.buffer.write(output)
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
