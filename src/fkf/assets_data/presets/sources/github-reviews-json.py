#!/usr/bin/env python3
"""Collect complete GitHub pull-request review contributions through bounded GraphQL pages."""

from __future__ import annotations

import json
import re
import subprocess
import sys
from datetime import UTC, datetime
from typing import Any

MAX_PROVIDER_BYTES = 64 << 20
MAX_OUTPUT_BYTES = 64 << 20
MAX_PAGES = 100
MAX_RECORDS = 10_000
RFC3339 = re.compile(r"^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(?:Z|[+-][0-9]{2}:[0-9]{2})$")
UTC_STAMP = re.compile(r"^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$")
DATABASE_ID = re.compile(r"^[1-9][0-9]*$")
REPOSITORY = re.compile(r"^[^/]+/[^/]+$")
QUERY = """query($from: DateTime!, $to: DateTime!, $cursor: String) {
  viewer {
    contributionsCollection(from: $from, to: $to) {
      pullRequestReviewContributions(first: 100, after: $cursor, orderBy: {direction: ASC}) {
        totalCount
        nodes {
          occurredAt
          user { login }
          repository { nameWithOwner }
          pullRequest { number title url }
          pullRequestReview { id fullDatabaseId submittedAt state url }
        }
        pageInfo { hasNextPage endCursor }
      }
    }
  }
}"""


def instant(value: str) -> datetime:
    if RFC3339.fullmatch(value) is None:
        raise ValueError
    parsed = datetime.fromisoformat(value)
    if parsed.tzinfo is None or parsed.microsecond:
        raise ValueError
    return parsed.astimezone(UTC)


def stamp(value: datetime) -> str:
    return value.isoformat().replace("+00:00", "Z")


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
            raise RuntimeError("GitHub GraphQL page failed")
    return raw


def connection(value: Any) -> dict[str, Any]:
    try:
        if not isinstance(value, dict) or value.get("errors"):
            raise ValueError
        result = value["data"]["viewer"]["contributionsCollection"]["pullRequestReviewContributions"]
    except (KeyError, TypeError) as error:
        raise ValueError from error
    if not isinstance(result, dict):
        raise TypeError
    nodes = result.get("nodes")
    total = result.get("totalCount")
    page_info = result.get("pageInfo")
    if (
        not isinstance(nodes, list)
        or len(nodes) > 100
        or not isinstance(total, int)
        or isinstance(total, bool)
        or total < 0
        or not isinstance(page_info, dict)
        or not isinstance(page_info.get("hasNextPage"), bool)
    ):
        raise ValueError
    for node in nodes:
        if not valid_node(node):
            raise ValueError
    return result


def valid_node(node: Any) -> bool:
    if not isinstance(node, dict):
        return False
    review = node.get("pullRequestReview")
    pull_request = node.get("pullRequest")
    repository = node.get("repository")
    user = node.get("user")
    if not all(isinstance(value, dict) for value in (review, pull_request, repository, user)):
        return False
    number = pull_request.get("number")
    return (
        isinstance(node.get("occurredAt"), str)
        and UTC_STAMP.fullmatch(node["occurredAt"]) is not None
        and isinstance(review.get("id"), str)
        and bool(review["id"])
        and isinstance(review.get("fullDatabaseId"), str)
        and DATABASE_ID.fullmatch(review["fullDatabaseId"]) is not None
        and isinstance(review.get("submittedAt"), str)
        and UTC_STAMP.fullmatch(review["submittedAt"]) is not None
        and isinstance(review.get("state"), str)
        and bool(review["state"])
        and isinstance(review.get("url"), str)
        and review["url"].startswith("https://")
        and isinstance(number, int)
        and not isinstance(number, bool)
        and number > 0
        and isinstance(pull_request.get("title"), str)
        and bool(pull_request["title"])
        and isinstance(pull_request.get("url"), str)
        and pull_request["url"].startswith("https://")
        and isinstance(repository.get("nameWithOwner"), str)
        and REPOSITORY.fullmatch(repository["nameWithOwner"]) is not None
        and isinstance(user.get("login"), str)
        and bool(user["login"])
    )


def project(node: dict[str, Any]) -> dict[str, Any]:
    review = node["pullRequestReview"]
    pull_request = node["pullRequest"]
    repository = node["repository"]["nameWithOwner"]
    reviewer = node["user"]["login"]
    return {
        "id": f"repos/{repository}/pulls/{pull_request['number']}/reviews/{review['fullDatabaseId']}",
        "reviewId": review["id"],
        "occurredAt": node["occurredAt"],
        "submittedAt": review["submittedAt"],
        "state": review["state"],
        "url": review["url"],
        "title": pull_request["title"],
        "pullRequest": {
            "number": pull_request["number"],
            "title": pull_request["title"],
            "url": pull_request["url"],
        },
        "repo": repository,
        "reviewer": reviewer,
        "repository_uri": f"repo:github.com/{repository}",
        "participant_uris": [f"actor:github.com/{reviewer}"],
    }


def main(arguments: list[str]) -> int:
    if len(arguments) != 2:
        sys.stderr.write("usage: github-reviews-json.py <start> <end>\n")
        return 2
    try:
        start = instant(arguments[0])
    except ValueError:
        sys.stderr.write(f"github-reviews-json.py: start is not an RFC3339 timestamp: {arguments[0]}\n")
        return 2
    try:
        end = instant(arguments[1])
    except ValueError:
        sys.stderr.write(f"github-reviews-json.py: end is not an RFC3339 timestamp: {arguments[1]}\n")
        return 2
    if start >= end:
        sys.stderr.write("github-reviews-json.py: start must be before end\n")
        return 2
    try:
        start_utc, end_utc = stamp(start), stamp(end)
        cursor: str | None = None
        expected_total: int | None = None
        seen_ids: set[str] = set()
        seen_count = 0
        records: list[dict[str, Any]] = []
        for _ in range(MAX_PAGES):
            command = ["api", "graphql", "-f", f"query={QUERY}", "-f", f"from={start_utc}", "-f", f"to={end_utc}"]
            if cursor is not None:
                command.extend(("-f", f"cursor={cursor}"))
            try:
                page = connection(json.loads(invoke(command)))
            except ValueError as error:
                raise RuntimeError("GitHub GraphQL returned an invalid review contribution page") from error
            total = page["totalCount"]
            nodes = page["nodes"]
            if expected_total is None:
                expected_total = total
                if total > MAX_RECORDS:
                    raise RuntimeError(
                        f"totalCount {total} exceeds the 100-page safety limit; cannot prove completeness"
                    )
            elif total != expected_total:
                raise RuntimeError("totalCount changed while paginating; cannot prove completeness")
            seen_count += len(nodes)
            if seen_count > expected_total:
                raise RuntimeError("received more nodes than totalCount; cannot prove completeness")
            for node in nodes:
                review_id = node["pullRequestReview"]["id"]
                if review_id in seen_ids:
                    raise RuntimeError("GraphQL pages contain duplicate review nodes; cannot prove completeness")
                seen_ids.add(review_id)
                submitted = node["pullRequestReview"]["submittedAt"]
                if start_utc <= submitted < end_utc:
                    records.append(project(node))
            page_info = page["pageInfo"]
            if not page_info["hasNextPage"]:
                break
            if not nodes:
                raise RuntimeError("GraphQL returned an empty page with hasNextPage=true; cannot prove completeness")
            next_cursor = page_info.get("endCursor")
            if not isinstance(next_cursor, str) or not next_cursor:
                raise RuntimeError("next GraphQL page has no cursor; cannot prove completeness")
            if next_cursor == cursor:
                raise RuntimeError("GraphQL pagination cursor did not advance; cannot prove completeness")
            cursor = next_cursor
        else:
            raise RuntimeError("reached the 100-page safety limit; cannot prove completeness")
        if seen_count != expected_total:
            raise RuntimeError(f"received {seen_count} of totalCount {expected_total} review contributions")
        records.sort(key=lambda record: (record["submittedAt"], record["id"]))
        output = (json.dumps(records, ensure_ascii=False, separators=(",", ":")) + "\n").encode()
        if len(output) > MAX_OUTPUT_BYTES:
            raise RuntimeError(f"output bound exceeds {MAX_OUTPUT_BYTES} bytes")
    except (OSError, RuntimeError, UnicodeError, TypeError, ValueError, json.JSONDecodeError) as error:
        sys.stderr.write(f"github-reviews-json.py: {error}\n")
        return 1
    sys.stdout.buffer.write(output)
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
