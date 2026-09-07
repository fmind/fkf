"""Multi-page provider helpers bound aggregates before exposing stdout."""

from __future__ import annotations

import json
import os
import runpy
from collections.abc import Callable
from typing import Any

import pytest

from .conftest import HelperInstallation

START = "2026-05-04T00:00:00Z"
END = "2026-05-05T00:00:00Z"


def _module(helpers: HelperInstallation, name: str) -> dict[str, Any]:
    return runpy.run_path(os.fspath(helpers.bin / name))


def _review_page() -> bytes:
    return json.dumps(
        {
            "data": {
                "viewer": {
                    "contributionsCollection": {
                        "pullRequestReviewContributions": {
                            "nodes": [
                                {
                                    "occurredAt": START,
                                    "pullRequestReview": {
                                        "id": "node-1",
                                        "fullDatabaseId": "1",
                                        "submittedAt": START,
                                        "state": "APPROVED",
                                        "url": "https://github.example.test/acme/project/pull/1#review-1",
                                    },
                                    "pullRequest": {
                                        "number": 1,
                                        "title": "Bound provider aggregates",
                                        "url": "https://github.example.test/acme/project/pull/1",
                                    },
                                    "repository": {"nameWithOwner": "acme/project"},
                                    "user": {"login": "reviewer"},
                                }
                            ],
                            "totalCount": 1,
                            "pageInfo": {"hasNextPage": False, "endCursor": None},
                        }
                    }
                }
            }
        },
        separators=(",", ":"),
    ).encode()


@pytest.mark.parametrize(
    ("name", "arguments", "response", "error"),
    [
        ("github-generic-list-json.py", ["/user/repos"], b'[{"id":1}]', "record bound"),
        (
            "github-list-json.py",
            ["user-repositories"],
            b'HTTP/2 200\n\n[{"full_name":"acme/project"}]',
            "record bound",
        ),
        ("github-reviews-json.py", [START, END], _review_page(), "safety limit"),
    ],
)
def test_github_paginators_enforce_aggregate_record_bounds(
    helpers: HelperInstallation,
    monkeypatch: pytest.MonkeyPatch,
    capfd: pytest.CaptureFixture[str],
    name: str,
    arguments: list[str],
    response: bytes,
    error: str,
) -> None:
    namespace = _module(helpers, name)
    main = namespace["main"]
    monkeypatch.setitem(main.__globals__, "invoke", lambda _arguments: response)
    monkeypatch.setitem(main.__globals__, "MAX_RECORDS", 0)

    assert main(arguments) == 1
    captured = capfd.readouterr()
    assert captured.out == ""
    assert error in captured.err


@pytest.mark.parametrize(
    ("name", "arguments", "response"),
    [
        ("github-generic-list-json.py", ["/user/repos"], b'[{"id":1}]'),
        (
            "github-list-json.py",
            ["user-repositories"],
            b'HTTP/2 200\n\n[{"full_name":"acme/project"}]',
        ),
        ("github-reviews-json.py", [START, END], _review_page()),
    ],
)
def test_github_paginators_enforce_exact_newline_inclusive_output_bounds(
    helpers: HelperInstallation,
    monkeypatch: pytest.MonkeyPatch,
    capfd: pytest.CaptureFixture[str],
    name: str,
    arguments: list[str],
    response: bytes,
) -> None:
    namespace = _module(helpers, name)
    main = namespace["main"]
    monkeypatch.setitem(main.__globals__, "invoke", lambda _arguments: response)
    monkeypatch.setitem(main.__globals__, "MAX_OUTPUT_BYTES", 64 << 20)
    assert main(arguments) == 0
    expected = capfd.readouterr().out
    assert expected

    monkeypatch.setitem(main.__globals__, "MAX_OUTPUT_BYTES", len(expected.encode()))
    assert main(arguments) == 0
    assert capfd.readouterr().out == expected

    monkeypatch.setitem(main.__globals__, "MAX_OUTPUT_BYTES", len(expected.encode()) - 1)
    assert main(arguments) == 1
    captured = capfd.readouterr()
    assert captured.out == ""
    assert "output bound" in captured.err


def _gmail_provider(arguments: list[str]) -> bytes:
    if arguments[:1] != ["gws"]:
        return b'{"messages":[{"id":"message-1"}]}\n'
    return json.dumps(
        {
            "id": "message-1",
            "threadId": "thread-1",
            "internalDate": str(1_777_852_800_000),
            "payload": {"headers": []},
        },
        separators=(",", ":"),
    ).encode()


def _calendar_provider(arguments: list[str]) -> bytes:
    if "calendarList" in arguments:
        return b'{"items":[{"id":"calendar-1","summary":"Calendar"}]}\n'
    return b'{"items":[{"id":"event-1","summary":"Event","start":{"dateTime":"2026-05-04T01:00:00Z"}}]}\n'


def _chat_provider(arguments: list[str]) -> bytes:
    if "spaces" in arguments and "messages" not in arguments:
        return b'{"spaces":[{"name":"spaces/one"}]}\n'
    return b'{"messages":[{"name":"spaces/one/messages/one","createTime":"2026-05-04T01:00:00Z"}]}\n'


def _tasks_provider(arguments: list[str]) -> bytes:
    if "tasklists" in arguments:
        return b'{"items":[{"id":"list-1","title":"Tasks"}]}\n'
    return b'{"items":[{"id":"task-1","title":"Task","updated":"2026-05-04T01:00:00Z"}]}\n'


@pytest.mark.parametrize(
    ("name", "arguments", "provider", "parent_limit", "error"),
    [
        ("gmail-json.py", [START, END], _gmail_provider, "MAX_RECORDS", "message record bound"),
        (
            "gws-calendars-json.py",
            [START, END, "2026-05-04", "2026-05-05"],
            _calendar_provider,
            "MAX_CALENDARS",
            "calendar bound",
        ),
        ("gws-chat-messages.py", [START, END], _chat_provider, "MAX_SPACES", "space bound"),
        ("gws-tasks.py", [START, END], _tasks_provider, "MAX_TASKLISTS", "task-list bound"),
    ],
)
def test_fanout_helpers_bound_parent_and_record_work(
    helpers: HelperInstallation,
    monkeypatch: pytest.MonkeyPatch,
    capfd: pytest.CaptureFixture[str],
    name: str,
    arguments: list[str],
    provider: Callable[[list[str]], bytes],
    parent_limit: str,
    error: str,
) -> None:
    namespace = _module(helpers, name)
    main = namespace["main"]
    function_name = "bounded"
    monkeypatch.setitem(main.__globals__, function_name, provider)
    monkeypatch.setitem(main.__globals__, parent_limit, 0)

    assert main(arguments) == 1
    captured = capfd.readouterr()
    assert captured.out == ""
    assert error in captured.err


@pytest.mark.parametrize(
    ("name", "arguments", "provider"),
    [
        ("gmail-json.py", [START, END], _gmail_provider),
        (
            "gws-calendars-json.py",
            [START, END, "2026-05-04", "2026-05-05"],
            _calendar_provider,
        ),
        ("gws-chat-messages.py", [START, END], _chat_provider),
        ("gws-tasks.py", [START, END], _tasks_provider),
    ],
)
def test_fanout_helpers_enforce_exact_output_bounds(
    helpers: HelperInstallation,
    monkeypatch: pytest.MonkeyPatch,
    capfd: pytest.CaptureFixture[str],
    name: str,
    arguments: list[str],
    provider: Callable[[list[str]], bytes],
) -> None:
    namespace = _module(helpers, name)
    main = namespace["main"]
    monkeypatch.setitem(main.__globals__, "bounded", provider)
    monkeypatch.setitem(main.__globals__, "MAX_OUTPUT_BYTES", 64 << 20)
    assert main(arguments) == 0
    expected = capfd.readouterr().out
    assert expected

    monkeypatch.setitem(main.__globals__, "MAX_OUTPUT_BYTES", len(expected.encode()))
    assert main(arguments) == 0
    assert capfd.readouterr().out == expected

    monkeypatch.setitem(main.__globals__, "MAX_OUTPUT_BYTES", len(expected.encode()) - 1)
    assert main(arguments) == 1
    captured = capfd.readouterr()
    assert captured.out == ""
    assert "output bound" in captured.err


@pytest.mark.parametrize("name", ["kaggle-json.py", "kaggle-models-json.py"])
def test_kaggle_collectors_bound_records_and_exact_output(
    helpers: HelperInstallation,
    monkeypatch: pytest.MonkeyPatch,
    capfd: pytest.CaptureFixture[str],
    name: str,
) -> None:
    namespace = _module(helpers, name)
    main = namespace["main"]
    arguments = ["datasets", "list"] if name == "kaggle-json.py" else ["owner"]
    if name == "kaggle-json.py":
        monkeypatch.setitem(main.__globals__, "invoke", lambda _arguments: b'[{"ref":"owner/data"}]')
    else:
        monkeypatch.setitem(
            main.__globals__,
            "page",
            lambda _arguments: (None, [{"id": 1, "ref": "owner/model", "title": "Model"}]),
        )
    monkeypatch.setitem(main.__globals__, "MAX_RECORDS", 0)
    assert main(arguments) == 1
    captured = capfd.readouterr()
    assert captured.out == ""
    assert "record bound" in captured.err

    monkeypatch.setitem(main.__globals__, "MAX_RECORDS", 1)
    monkeypatch.setitem(main.__globals__, "MAX_OUTPUT_BYTES", 64 << 20)
    assert main(arguments) == 0
    expected = capfd.readouterr().out
    monkeypatch.setitem(main.__globals__, "MAX_OUTPUT_BYTES", len(expected.encode()))
    assert main(arguments) == 0
    assert capfd.readouterr().out == expected
    monkeypatch.setitem(main.__globals__, "MAX_OUTPUT_BYTES", len(expected.encode()) - 1)
    assert main(arguments) == 1
    captured = capfd.readouterr()
    assert captured.out == ""
    assert "output bound" in captured.err
