#!/usr/bin/env python3
"""Collect one bounded, validated Jira project snapshot through ACLI."""

from __future__ import annotations

import json
import re
import subprocess
import sys
from typing import Any

MAX_PROVIDER_BYTES = 64 << 20
MAX_OUTPUT_BYTES = 64 << 20
SITE = re.compile(r"^(?![.-])(?!.*\.\.)(?!.*(?:-\.|\.-))[a-z0-9.-]+\.atlassian\.net$")
PROJECT = re.compile(r"^[A-Z][A-Z0-9_]*$")


def text_field(value: Any, *keys: str) -> Any:
    if not isinstance(value, dict):
        return None
    for key in keys:
        if key in value:
            return value[key]
    return None


def main(arguments: list[str]) -> int:
    if arguments[:1] in (["--version"], ["-v"]):
        sys.stdout.write("jira-issues-json.py (fkf preset helper)\n")
        return 0
    if len(arguments) != 3:
        sys.stderr.write("usage: jira-issues-json.py <site.atlassian.net> <PROJECT> <filter-jql>\n")
        return 2
    site, project, filter_value = arguments
    if SITE.fullmatch(site) is None:
        sys.stderr.write("jira-issues-json.py: site must be a lowercase *.atlassian.net host\n")
        return 2
    if PROJECT.fullmatch(project) is None:
        sys.stderr.write("jira-issues-json.py: invalid Jira project key\n")
        return 2
    if len(filter_value.encode()) > 512:
        sys.stderr.write("jira-issues-json.py: filter JQL exceeds 512 bytes\n")
        return 2
    if (
        not filter_value
        or "\n" in filter_value
        or "\r" in filter_value
        or re.search(r"order\s+by", filter_value, re.IGNORECASE)
    ):
        sys.stderr.write("jira-issues-json.py: filter JQL must be one non-empty expression without ORDER BY\n")
        return 2
    jql = f'project = "{project}" AND ({filter_value}) ORDER BY key ASC'
    try:
        with subprocess.Popen(
            [
                "acli",
                "jira",
                "workitem",
                "search",
                "--jql",
                jql,
                "--fields",
                "key,summary,status,assignee,url",
                "--limit",
                "10001",
                "--json",
            ],
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
                raise RuntimeError(f"Jira search failed for project {project}")
        value = json.loads(raw)
        if isinstance(value, list):
            issues = value
        elif isinstance(value, dict) and isinstance(value.get("issues"), list) and not value.get("nextPageToken"):
            issues = value["issues"]
        else:
            raise ValueError
        if len(issues) > 10_000:
            raise ValueError
        records: list[dict[str, Any]] = []
        seen: set[str] = set()
        for issue in issues:
            if not isinstance(issue, dict):
                raise TypeError
            fields = issue.get("fields", issue)
            if not isinstance(fields, dict):
                raise TypeError
            key = issue.get("key", fields.get("key"))
            summary = fields.get("summary", issue.get("summary"))
            status = fields.get("status", issue.get("status"))
            assignee = fields.get("assignee", issue.get("assignee"))
            url = fields.get("url", issue.get("url", f"https://{site}/browse/{key or ''}"))
            if not isinstance(key, str) or re.fullmatch(rf"{re.escape(project)}-[1-9][0-9]*", key) is None:
                raise ValueError
            if key in seen or not isinstance(summary, str) or not summary:
                raise ValueError
            if not isinstance(url, str) or not url.startswith(f"https://{site}/"):
                raise ValueError
            seen.add(key)
            if isinstance(status, dict):
                status = status.get("name")
            if isinstance(assignee, dict):
                assignee = assignee.get("displayName", assignee.get("accountId"))
            if status is not None and not isinstance(status, str):
                raise ValueError
            if assignee is not None and not isinstance(assignee, str):
                raise ValueError
            records.append(
                {
                    "id": key,
                    "title": summary,
                    "url": url,
                    "status": status,
                    "assignee": assignee,
                    "project_uri": f"project:jira/{project}",
                    "ticket_uri": f"ticket:jira/{key}",
                }
            )
        records.sort(key=lambda record: record["id"])
        output = (json.dumps(records, ensure_ascii=False, separators=(",", ":")) + "\n").encode()
        if len(output) > MAX_OUTPUT_BYTES:
            raise RuntimeError(f"output bound exceeds {MAX_OUTPUT_BYTES} bytes")
    except RuntimeError as error:
        sys.stderr.write(f"jira-issues-json.py: {error}\n")
        return 1
    except OSError, UnicodeError, TypeError, ValueError, json.JSONDecodeError:
        sys.stderr.write(
            "jira-issues-json.py: Jira returned malformed, duplicate, excessive, or out-of-scope results\n"
        )
        return 1
    sys.stdout.buffer.write(output)
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
