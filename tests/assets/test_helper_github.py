"""GitHub search and review collectors keep complete, finite windows."""

from __future__ import annotations

import json
import os
from pathlib import Path
from typing import Any, cast

import pytest

from .conftest import HelperInstallation


def _search_item(url: str, *, repository: str = "acme/project") -> dict[str, object]:
    return {
        "number": 1,
        "title": "Synthetic search result",
        "html_url": url,
        "updated_at": "2026-05-04T00:00:00Z",
        "repository_url": f"https://api.github.example.test/repos/{repository}",
        "state": "open",
        "user": {"login": "octocat"},
        "assignees": [{"login": "owner"}],
    }


def _search_envelope(total: int, items: list[dict[str, object]], *, incomplete: bool = False) -> str:
    return json.dumps(
        {"total_count": total, "incomplete_results": incomplete, "items": items},
        separators=(",", ":"),
    )


@pytest.mark.parametrize(
    "timestamp",
    [
        "2026-05-04T00:00:00",
        "2026-05-04T00:00:00.1Z",
        "2026-05-04 00:00:00Z",
    ],
)
@pytest.mark.parametrize(
    ("helper", "arguments"),
    [
        ("github-commits-json.py", ()),
        ("github-search-json.py", ("prs", "assignee")),
        ("github-reviews-json.py", ()),
    ],
)
def test_github_window_helpers_reject_noncanonical_timestamps_before_provider_execution(
    helpers: HelperInstallation,
    timestamp: str,
    helper: str,
    arguments: tuple[str, ...],
) -> None:
    marker = helpers.root / "provider-called"
    helpers.fake("gh", 'printf called >"$PROVIDER_MARKER"\nexit 99\n')
    result = helpers.run(
        helper,
        *arguments,
        timestamp,
        "2026-05-05T00:00:00Z",
        environment={"PROVIDER_MARKER": os.fspath(marker)},
    )
    assert result.returncode == 2
    assert result.stdout == b""
    assert b"RFC3339 timestamp" in result.stderr
    assert b"Traceback" not in result.stderr
    assert os.fsencode(Path.cwd()) not in result.stderr
    assert not marker.exists()


def test_github_search_splits_saturated_windows_and_deduplicates_urls(helpers: HelperInstallation) -> None:
    root = _search_envelope(1000, [])
    left = _search_envelope(
        2,
        [
            _search_item("https://github.example.test/acme/project/pull/1"),
            _search_item("https://github.example.test/acme/project/pull/2"),
        ],
    )
    right = _search_envelope(
        2,
        [
            _search_item("https://github.example.test/acme/project/pull/2"),
            _search_item("https://github.example.test/acme/project/pull/3"),
        ],
    )
    call_log = helpers.root / "gh-calls"
    helpers.fake(
        "gh",
        """printf '%s\n' "$*" >> "$GH_CALL_LOG"
case "$*" in
  *"updated:2026-05-04T00:00:00Z..2026-05-04T00:00:03Z"*) printf '%s\n' "$ROOT_PAGE" ;;
  *"updated:2026-05-04T00:00:00Z..2026-05-04T00:00:01Z"*) printf '%s\n' "$LEFT_PAGE" ;;
  *"updated:2026-05-04T00:00:02Z..2026-05-04T00:00:03Z"*) printf '%s\n' "$RIGHT_PAGE" ;;
  *) exit 2 ;;
esac
""",
    )
    result = helpers.run(
        "github-search-json.py",
        "prs",
        "assignee",
        "2026-05-04T00:00:00Z",
        "2026-05-04T00:00:04Z",
        environment={
            "GH_CALL_LOG": os.fspath(call_log),
            "ROOT_PAGE": root,
            "LEFT_PAGE": left,
            "RIGHT_PAGE": right,
        },
    )
    assert result.returncode == 0, result.stderr.decode(errors="replace")
    records = json.loads(result.stdout)
    assert [record["url"] for record in records] == [
        "https://github.example.test/acme/project/pull/1",
        "https://github.example.test/acme/project/pull/2",
        "https://github.example.test/acme/project/pull/3",
    ]
    assert all(record["assignee_uris"] == ["actor:github.com/owner"] for record in records)
    calls = call_log.read_text(encoding="utf-8")
    assert len(calls.splitlines()) == 3
    for value in (
        "api --method GET /search/issues",
        "X-GitHub-Api-Version: 2026-03-10",
        "per_page=100",
        "page=1",
        "total_count,incomplete_results",
    ):
        assert value in calls
    assert "--paginate" not in calls


@pytest.mark.parametrize(
    ("mode_arguments", "response", "second_response", "error"),
    [
        (
            ("issues", "assignee", "2026-05-04T00:00:00Z", "2026-05-04T00:00:01Z"),
            _search_envelope(1000, []),
            "",
            "one-second",
        ),
        (
            ("issues", "assignee", "2026-05-04T00:00:00Z", "2026-05-04T00:00:01Z"),
            _search_envelope(
                1,
                [_search_item("https://github.example.test/acme/project/issues/1")],
                incomplete=True,
            ),
            "",
            "incomplete_results=true",
        ),
        (
            ("issues", "assignee", "2026-05-04T00:00:00Z", "2026-05-04T00:00:01Z"),
            _search_envelope(2, [_search_item("https://github.example.test/acme/project/issues/1")]),
            _search_envelope(2, []),
            "retrieved 1 of 2",
        ),
        (
            (
                "issues",
                "repository",
                "acme/project",
                "2026-05-04T00:00:00Z",
                "2026-05-04T00:00:01Z",
            ),
            _search_envelope(
                1,
                [_search_item("https://github.example.test/other/project/issues/1", repository="other/project")],
            ),
            "",
            "outside repository acme/project",
        ),
    ],
)
def test_github_search_completeness_failures_emit_no_partial_records(
    helpers: HelperInstallation,
    mode_arguments: tuple[str, ...],
    response: str,
    second_response: str,
    error: str,
) -> None:
    helpers.fake(
        "gh",
        """case "$*" in
  *"page=2"*) printf '%s\n' "${SECOND_RESPONSE:-$SEARCH_RESPONSE}" ;;
  *) printf '%s\n' "$SEARCH_RESPONSE" ;;
esac
""",
    )
    result = helpers.run(
        "github-search-json.py",
        *mode_arguments,
        environment={"SEARCH_RESPONSE": response, "SECOND_RESPONSE": second_response},
    )
    assert result.returncode != 0
    assert result.stdout == b""
    assert error.encode() in result.stderr


def test_github_search_fetches_explicit_second_page(helpers: HelperInstallation) -> None:
    first = _search_envelope(2, [_search_item("https://github.example.test/acme/project/issues/1")])
    second = _search_envelope(2, [_search_item("https://github.example.test/acme/project/issues/2")])
    call_log = helpers.root / "gh-calls"
    helpers.fake(
        "gh",
        """printf '%s\n' "$*" >> "$GH_CALL_LOG"
case "$*" in *"page=2"*) printf '%s\n' "$SECOND_PAGE" ;; *) printf '%s\n' "$FIRST_PAGE" ;; esac
""",
    )
    result = helpers.run(
        "github-search-json.py",
        "issues",
        "assignee",
        "2026-05-04T00:00:00Z",
        "2026-05-04T00:00:01Z",
        environment={
            "FIRST_PAGE": first,
            "GH_CALL_LOG": os.fspath(call_log),
            "SECOND_PAGE": second,
        },
    )
    assert result.returncode == 0, result.stderr.decode(errors="replace")
    assert len(json.loads(result.stdout)) == 2
    calls = call_log.read_text(encoding="utf-8")
    assert "page=2" in calls
    assert "--paginate" not in calls


def test_obsolete_github_search_mode_stops_before_provider_execution(helpers: HelperInstallation) -> None:
    marker = helpers.root / "provider-called"
    helpers.fake("gh", 'printf called > "$GH_MARKER"\n')
    result = helpers.run(
        "github-search-json.py",
        "prs",
        "reviewed",
        "2026-05-04T00:00:00Z",
        "2026-05-04T00:00:01Z",
        environment={"GH_MARKER": os.fspath(marker)},
    )
    assert result.returncode == 2
    assert result.stdout == b""
    assert not marker.exists()
    assert b"expected prs assignee or issues assignee" in result.stderr


def _review_contribution(
    node_id: str,
    database_id: int,
    submitted_at: str,
    repository: str = "acme/widgets",
    number: int = 42,
) -> dict[str, object]:
    pull_request_url = f"https://github.example.test/{repository}/pull/{number}"
    return {
        "occurredAt": submitted_at,
        "pullRequestReview": {
            "id": node_id,
            "fullDatabaseId": str(database_id),
            "submittedAt": submitted_at,
            "state": "APPROVED",
            "url": f"{pull_request_url}#pullrequestreview-{node_id}",
        },
        "pullRequest": {
            "number": number,
            "title": "Review exact collection windows",
            "url": pull_request_url,
        },
        "repository": {"nameWithOwner": repository},
        "user": {"login": "reviewer"},
    }


def _review_page(nodes: list[dict[str, object]], total: int, has_next: bool, cursor: str | None) -> str:
    return json.dumps(
        {
            "data": {
                "viewer": {
                    "contributionsCollection": {
                        "pullRequestReviewContributions": {
                            "nodes": nodes,
                            "totalCount": total,
                            "pageInfo": {"hasNextPage": has_next, "endCursor": cursor},
                        }
                    }
                }
            }
        },
        separators=(",", ":"),
    )


def _run_reviews(
    helpers: HelperInstallation,
    first: str,
    second: str = "",
    *,
    fail_second: bool = False,
) -> tuple[int, bytes, bytes, str]:
    call_log = helpers.root / "gh-calls"
    helpers.fake(
        "gh",
        """printf '%s\n' "$*" >> "$GH_CALL_LOG"
case "$*" in
  *"cursor=cursor-1"*)
    [ "$FAIL_SECOND" = false ] || exit 42
    printf '%s\n' "$SECOND_PAGE" ;;
  *) printf '%s\n' "$FIRST_PAGE" ;;
esac
""",
    )
    result = helpers.run(
        "github-reviews-json.py",
        "2026-05-04T00:00:00Z",
        "2026-05-05T00:00:00Z",
        environment={
            "FAIL_SECOND": str(fail_second).lower(),
            "FIRST_PAGE": first,
            "GH_CALL_LOG": os.fspath(call_log),
            "SECOND_PAGE": second,
        },
    )
    return result.returncode, result.stdout, result.stderr, call_log.read_text(encoding="utf-8")


def test_github_reviews_paginates_contributions_and_enforces_half_open_window(
    helpers: HelperInstallation,
) -> None:
    first = _review_page(
        [
            _review_contribution("before", 100, "2026-05-03T23:59:59Z"),
            _review_contribution("at-start", 101, "2026-05-04T00:00:00Z"),
            _review_contribution("at-end", 102, "2026-05-05T00:00:00Z"),
        ],
        4,
        True,
        "cursor-1",
    )
    second = _review_page(
        [_review_contribution("inside", 103, "2026-05-04T12:34:56Z", "acme/other", 7)],
        4,
        False,
        None,
    )
    returncode, stdout, stderr, calls = _run_reviews(helpers, first, second)
    assert returncode == 0, stderr.decode(errors="replace")
    records = cast(list[dict[str, Any]], json.loads(stdout))
    assert [record["id"] for record in records] == [
        "repos/acme/widgets/pulls/42/reviews/101",
        "repos/acme/other/pulls/7/reviews/103",
    ]
    required = {
        "reviewId",
        "occurredAt",
        "submittedAt",
        "state",
        "url",
        "title",
        "pullRequest",
        "repo",
        "reviewer",
    }
    assert all(required <= record.keys() and "body" not in record for record in records)
    assert calls.count("api graphql") == 2
    assert "from=2026-05-04T00:00:00Z" in calls
    assert "to=2026-05-05T00:00:00Z" in calls
    assert "cursor=cursor-1" in calls


@pytest.mark.parametrize(
    ("first", "second", "fail_second", "error"),
    [
        (
            _review_page([_review_contribution("first", 201, "2026-05-04T01:00:00Z")], 2, True, "cursor-1"),
            "",
            True,
            "GraphQL page",
        ),
        (
            _review_page([_review_contribution("only", 301, "2026-05-04T01:00:00Z")], 2, False, None),
            "",
            False,
            "totalCount",
        ),
        (
            _review_page([_review_contribution("duplicate", 401, "2026-05-04T01:00:00Z")], 2, True, "cursor-1"),
            _review_page([_review_contribution("duplicate", 401, "2026-05-04T01:00:00Z")], 2, False, None),
            False,
            "duplicate",
        ),
        (_review_page([], 1, True, "cursor-1"), "", False, "empty page"),
        (
            _review_page([_review_contribution("first", 501, "2026-05-04T01:00:00Z")], 10001, True, "cursor-1"),
            "",
            False,
            "100-page safety limit",
        ),
    ],
)
def test_github_review_completeness_failures_emit_no_partial_records(
    helpers: HelperInstallation,
    first: str,
    second: str,
    fail_second: bool,
    error: str,
) -> None:
    returncode, stdout, stderr, _ = _run_reviews(helpers, first, second, fail_second=fail_second)
    assert returncode != 0
    assert stdout == b""
    assert error.encode() in stderr
