#!/usr/bin/env python3
"""Collect GitHub Actions runs from owned repositories through finite range bisection."""

from __future__ import annotations

import json
import re
import subprocess
import sys
from dataclasses import dataclass
from datetime import UTC, datetime
from pathlib import Path
from typing import Any

MAX_PROVIDER_BYTES = 64 << 20
MAX_OUTPUT_BYTES = 64 << 20
MAX_OWNERS = 100
MAX_REPOSITORIES = 10_000
MAX_PROVIDER_CALLS = 10_000
MAX_RECORDS = 100_000
OWNER = re.compile(r"^[A-Za-z0-9][A-Za-z0-9-]*$")


@dataclass
class Budget:
    """Bound aggregate work shared by repository enumeration and range splits."""

    provider_calls: int = 0

    def invoke(self, arguments: list[str]) -> bytes:
        self.provider_calls += 1
        if self.provider_calls > MAX_PROVIDER_CALLS:
            raise RuntimeError(f"provider-call bound exceeds {MAX_PROVIDER_CALLS}")
        return invoke(arguments)


def instant(value: str) -> datetime:
    parsed = datetime.fromisoformat(value)
    if parsed.tzinfo is None or parsed.microsecond:
        raise ValueError
    return parsed.astimezone(UTC)


def stamp(epoch: int) -> str:
    return datetime.fromtimestamp(epoch, UTC).isoformat().replace("+00:00", "Z")


def invoke(arguments: list[str]) -> bytes:
    if arguments[:1] == ["gh"]:
        process = subprocess.Popen(
            ["gh", *arguments[1:]],
            stdout=subprocess.PIPE,
        )
    elif arguments[:1] == [sys.executable]:
        process = subprocess.Popen(
            ["/usr/bin/env", "python3", *arguments[1:]],
            stdout=subprocess.PIPE,
        )
    else:
        raise RuntimeError("unexpected helper executable")
    with process:
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


def collect(repository: str, start: int, end: int, budget: Budget | None = None) -> list[dict[str, Any]]:
    budget = budget or Budget()
    range_start, range_end = stamp(start), stamp(end - 1)
    runs: list[dict[str, Any]] = []
    total: int | None = None
    seen_urls: set[str] = set()
    for page in range(1, 11):
        try:
            value = json.loads(
                budget.invoke(
                    [
                        "gh",
                        "api",
                        "--method",
                        "GET",
                        f"/repos/{repository}/actions/runs",
                        "-f",
                        "per_page=100",
                        "-f",
                        f"page={page}",
                        "-f",
                        f"created={range_start}..{range_end}",
                    ]
                )
            )
        except RuntimeError as error:
            raise RuntimeError(f"cannot collect {repository} for [{range_start}, {range_end}]: {error}") from error
        if (
            not isinstance(value, dict)
            or not isinstance(value.get("workflow_runs"), list)
            or not isinstance(value.get("total_count"), int)
            or isinstance(value.get("total_count"), bool)
            or value["total_count"] < 0
        ):
            raise TypeError(f"GitHub returned an invalid workflow-runs document for {repository}")
        page_total = value["total_count"]
        if total is None:
            total = page_total
            if total > MAX_RECORDS:
                raise RuntimeError(f"workflow-run record bound exceeds {MAX_RECORDS}")
        elif page_total != total:
            raise RuntimeError(f"GitHub changed workflow-run total_count while collecting {repository}")
        page_runs = value["workflow_runs"]
        if len(page_runs) > 100 or any(not isinstance(item, dict) for item in page_runs):
            raise RuntimeError(f"GitHub returned an invalid workflow-runs document for {repository}")
        for item in page_runs:
            url = item.get("html_url")
            if not isinstance(url, str) or not url or url in seen_urls:
                raise RuntimeError(f"GitHub returned duplicate or unidentified workflow runs for {repository}")
            seen_urls.add(url)
        runs.extend(page_runs)
        target = min(total, 1000)
        if len(runs) > target:
            raise RuntimeError(f"GitHub returned more workflow runs than declared for {repository}")
        if len(runs) == target:
            break
        if not page_runs:
            raise RuntimeError(f"GitHub did not return a complete workflow-run result for {repository}")
    if total is None or len(runs) != min(total, 1000):
        raise RuntimeError(f"GitHub did not return a complete workflow-run result for {repository}")
    if total >= 1000:
        if end - start <= 1:
            raise RuntimeError(
                f"one-second slice for {repository} [{range_start}, {range_end}] returned at least 1000 runs; "
                "cannot prove completeness"
            )
        midpoint = start + (end - start) // 2
        records = [
            *collect(repository, start, midpoint, budget),
            *collect(repository, midpoint, end, budget),
        ]
        if len(records) != total or len({record["url"] for record in records}) != len(records):
            raise RuntimeError(f"range split did not produce the declared workflow-run total for {repository}")
        return records
    exclusive_end = stamp(end)
    records = []
    for run in runs:
        created = run.get("created_at")
        if not isinstance(created, str) or not range_start <= created < exclusive_end:
            raise RuntimeError(f"GitHub returned a workflow run outside the requested range for {repository}")
        records.append(
            {
                "databaseId": run.get("id"),
                "workflowName": run.get("name"),
                "displayTitle": run.get("display_title") or run.get("name") or "workflow run",
                "status": run.get("status"),
                "conclusion": run.get("conclusion"),
                "createdAt": created,
                "updatedAt": run.get("updated_at"),
                "headBranch": run.get("head_branch"),
                "headSha": run.get("head_sha"),
                "event": run.get("event"),
                "url": run.get("html_url"),
                "repo": repository,
                "repository_uri": f"repo:github.com/{repository}",
            }
        )
    if len(records) > MAX_RECORDS:
        raise RuntimeError(f"workflow-run record bound exceeds {MAX_RECORDS}")
    return records


def main(arguments: list[str]) -> int:
    if arguments[:1] in (["--version"], ["-v"]):
        sys.stdout.write("gh-runs.py (fkf base helper)\n")
        return 0
    if len(arguments) < 2:
        sys.stderr.write("usage: gh-runs.py <start> <end> [owner...]\n")
        return 2
    try:
        start, end = instant(arguments[0]), instant(arguments[1])
    except ValueError:
        sys.stderr.write("gh-runs.py: start or end is not an RFC3339 timestamp\n")
        return 2
    if start >= end:
        sys.stderr.write("gh-runs.py: start must be before end\n")
        return 2
    try:
        budget = Budget()
        try:
            viewer = budget.invoke(["gh", "api", "/user", "--jq", ".login"]).decode().strip()
        except (OSError, RuntimeError, UnicodeError) as error:
            raise RuntimeError("cannot identify the authenticated GitHub user") from error
        owners = arguments[2:] or [viewer]
        if len(owners) > MAX_OWNERS:
            raise RuntimeError(f"owner bound exceeds {MAX_OWNERS}")
        if any(OWNER.fullmatch(owner) is None for owner in owners):
            invalid = next(owner for owner in owners if OWNER.fullmatch(owner) is None)
            sys.stderr.write(f"gh-runs.py: invalid GitHub owner: {invalid}\n")
            return 2
        helper = Path(__file__).with_name("github-generic-list-json.py")
        repositories: set[str] = set()
        for owner in owners:
            endpoint = "/user/repos" if owner.lower() == viewer.lower() else f"/orgs/{owner}/repos"
            extra = ["-f", "affiliation=owner"] if owner.lower() == viewer.lower() else ["-f", "type=all"]
            try:
                value = json.loads(budget.invoke([sys.executable, "-I", str(helper), endpoint, *extra]))
            except (OSError, RuntimeError, UnicodeError, TypeError, ValueError, json.JSONDecodeError) as error:
                raise RuntimeError(f"cannot enumerate repositories owned by {owner}") from error
            if not isinstance(value, list) or any(
                not isinstance(item, dict) or not isinstance(item.get("full_name"), str) or not item["full_name"]
                for item in value
            ):
                raise RuntimeError("repository listing contained an invalid name")
            repositories.update(item["full_name"] for item in value)
            if len(repositories) > MAX_REPOSITORIES:
                raise RuntimeError(f"repository bound exceeds {MAX_REPOSITORIES}")
        records: list[dict[str, Any]] = []
        for repository in sorted(repositories):
            records.extend(collect(repository, int(start.timestamp()), int(end.timestamp()), budget))
            if len(records) > MAX_RECORDS:
                raise RuntimeError(f"workflow-run record bound exceeds {MAX_RECORDS}")
        by_url = {record["url"]: record for record in records}
        if len(by_url) != len(records):
            raise RuntimeError("GitHub returned duplicate workflow runs across repositories")
        ordered = sorted(
            by_url.values(), key=lambda record: (record["createdAt"], record["repo"], str(record["databaseId"]))
        )
        output = (json.dumps(ordered, ensure_ascii=False, separators=(",", ":")) + "\n").encode()
        if len(output) > MAX_OUTPUT_BYTES:
            raise RuntimeError(f"output bound exceeds {MAX_OUTPUT_BYTES} bytes")
    except (OSError, RuntimeError, UnicodeError, TypeError, ValueError, json.JSONDecodeError) as error:
        sys.stderr.write(f"gh-runs.py: {error}\n")
        return 1
    sys.stdout.buffer.write(output)
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
