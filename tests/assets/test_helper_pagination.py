"""Finite pagination, completeness, and projection gates for provider helpers."""

from __future__ import annotations

import json
import os
import runpy
from datetime import UTC, datetime, time, timedelta
from types import FunctionType
from typing import cast

import pytest

from .conftest import HelperInstallation


def _json_lines(data: bytes) -> list[dict[str, object]]:
    records: list[dict[str, object]] = []
    for line in data.splitlines():
        if not line.strip():
            continue
        value = json.loads(line)
        assert isinstance(value, dict)
        records.append(cast(dict[str, object], value))
    return records


def test_github_list_follows_link_pages_and_projects_repository_metadata(helpers: HelperInstallation) -> None:
    call_log = helpers.root / "gh-calls"
    helpers.fake(
        "gh",
        """printf '%s\n' "$*" >> "$GH_CALL_LOG"
case "$*" in
  *" page=1"*)
    printf '%s\n' 'HTTP/2.0 200 OK' 'Link: <https://api.github.test/user/repos?page=2>; rel="next"' ''
    printf '%s\n' '[{"full_name":"acme/one","html_url":"https://github.test/acme/one","updated_at":"2026-08-01T00:00:00Z","archived":false}]' ;;
  *" page=2"*)
    printf '%s\n' 'HTTP/2.0 200 OK' ''
    printf '%s\n' '[{"full_name":"acme/two","html_url":"https://github.test/acme/two","updated_at":"2026-08-02T00:00:00Z","archived":true}]' ;;
  *) exit 2 ;;
esac
""",
    )
    result = helpers.run(
        "github-list-json.py",
        "user-repositories",
        environment={"GH_CALL_LOG": os.fspath(call_log)},
    )
    assert result.returncode == 0, result.stderr.decode(errors="replace")
    assert [record["nameWithOwner"] for record in _json_lines(result.stdout)] == ["acme/one", "acme/two"]
    calls = call_log.read_text(encoding="utf-8")
    assert "--include" in calls
    assert "page=1" in calls
    assert "page=2" in calls
    assert "per_page=100" in calls


def test_github_list_rejects_advancing_page_limit_without_partial_output(helpers: HelperInstallation) -> None:
    call_log = helpers.root / "gh-calls"
    helpers.fake(
        "gh",
        """printf '%s\n' "$*" >> "$GH_CALL_LOG"
printf '%s\n' 'HTTP/2.0 200 OK' 'Link: <https://api.github.test/notifications?page=next>; rel="next"' '' '[]'
""",
    )
    result = helpers.run(
        "github-list-json.py",
        "notifications",
        "2026-08-01T00:00:00Z",
        "2026-08-02T00:00:00Z",
        environment={"GH_CALL_LOG": os.fspath(call_log)},
    )
    assert result.returncode != 0
    assert result.stdout == b""
    assert b"100-page safety limit" in result.stderr
    assert b"cannot prove completeness" in result.stderr
    assert len(call_log.read_text(encoding="utf-8").splitlines()) == 100


@pytest.mark.parametrize(
    ("collection", "provider_page", "required", "forbidden"),
    [
        (
            "spaces",
            {
                "spaces": [
                    {
                        "name": "spaces/one",
                        "displayName": "Project",
                        "spaceType": "SPACE",
                        "spaceThreadingState": "THREADED_MESSAGES",
                        "lastActiveTime": "2026-08-01T12:00:00Z",
                        "membershipCount": 7,
                        "spaceUri": "https://chat.google.test/room/one",
                        "spaceDetails": {"description": "forbidden-sentinel"},
                    }
                ]
            },
            ("spaces/one", "membershipCount", "SPACE"),
            "forbidden-sentinel",
        ),
        (
            "files",
            {
                "files": [
                    {
                        "id": "file-1",
                        "name": "Plan",
                        "mimeType": "application/vnd.google-apps.document",
                        "webViewLink": "https://drive.google.test/file-1",
                        "modifiedTime": "2026-08-02T12:00:00Z",
                        "createdTime": "2026-08-01T12:00:00Z",
                        "owners": [
                            {
                                "displayName": "Owner",
                                "emailAddress": "owner@example.test",
                                "photoLink": "forbidden-sentinel",
                            }
                        ],
                        "description": "forbidden-sentinel",
                    }
                ]
            },
            ("file-1", "Owner", "owner@example.test"),
            "forbidden-sentinel",
        ),
        (
            "connections",
            {
                "connections": [
                    {
                        "resourceName": "people/one",
                        "etag": "forbidden-sentinel",
                        "names": [{"displayName": "Ada Lovelace", "familyName": "Lovelace"}],
                        "emailAddresses": [{"value": "ada@example.test", "type": "work"}],
                    }
                ]
            },
            ("people/one", "Ada Lovelace", "ada@example.test"),
            "forbidden-sentinel",
        ),
    ],
)
def test_gws_page_projects_only_declared_metadata(
    helpers: HelperInstallation,
    collection: str,
    provider_page: dict[str, object],
    required: tuple[str, ...],
    forbidden: str,
) -> None:
    result = helpers.run("gws-page-json.py", collection, stdin=json.dumps(provider_page) + "\n")
    assert result.returncode == 0, result.stderr.decode(errors="replace")
    output = result.stdout.decode()
    assert forbidden not in output
    assert all(value in output for value in required)


def test_gws_page_preserves_complete_token_chain_and_rejects_open_terminal_cursor(
    helpers: HelperInstallation,
) -> None:
    complete = helpers.run(
        "gws-page-json.py",
        "items",
        stdin='{"items":[],"nextPageToken":"cursor-1"}\n{"items":[{"id":"complete"}]}\n',
    )
    assert complete.returncode == 0, complete.stderr.decode(errors="replace")
    assert len(complete.stdout.splitlines()) == 2
    assert b'"id":"complete"' in complete.stdout

    pages = "".join(
        json.dumps({"items": [], "nextPageToken": f"cursor-{page}"}, separators=(",", ":")) + "\n"
        for page in range(1, 101)
    )
    incomplete = helpers.run("gws-page-json.py", "items", stdin=pages)
    assert incomplete.returncode != 0
    assert incomplete.stdout == b""
    assert b"page limit" in incomplete.stderr
    assert b"cannot prove completeness" in incomplete.stderr


def test_github_notifications_use_exact_window_and_reject_bad_timestamps(helpers: HelperInstallation) -> None:
    helpers.fake(
        "gh",
        """printf '%s\n' 'HTTP/2.0 200 OK' ''
printf '%s\n' "$NOTIFICATIONS"
""",
    )
    window = helpers.run(
        "github-list-json.py",
        "notifications",
        "2026-08-01T00:00:00Z",
        "2026-08-02T00:00:00Z",
        environment={
            "NOTIFICATIONS": json.dumps(
                [
                    {"id": "before", "updated_at": "2026-07-31T23:59:59Z"},
                    {"id": "start", "updated_at": "2026-08-01T00:00:00Z"},
                    {"id": "inside", "updated_at": "2026-08-01T12:00:00Z"},
                    {"id": "end", "updated_at": "2026-08-02T00:00:00Z"},
                ],
                separators=(",", ":"),
            )
        },
    )
    assert window.returncode == 0, window.stderr.decode(errors="replace")
    assert [record["id"] for record in _json_lines(window.stdout)] == ["start", "inside"]

    malformed = helpers.run(
        "github-list-json.py",
        "notifications",
        "2026-08-01T00:00:00Z",
        "2026-08-02T00:00:00Z",
        environment={
            "NOTIFICATIONS": '[{"id":"valid","updated_at":"2026-08-01T12:00:00Z"},{"id":"bad","updated_at":"not-a-time"}]'
        },
    )
    assert malformed.returncode != 0
    assert malformed.stdout == b""
    assert b"valid updated_at" in malformed.stderr


def test_gcloud_audit_rejects_limit_plus_one_without_partial_output(helpers: HelperInstallation) -> None:
    helpers.fake("gcloud", "jq -cn '[range(0; 10001) | {}]'\n")
    result = helpers.run(
        "gcloud-audit-json.py",
        "2026-08-01T00:00:00Z",
        "2026-08-02T00:00:00Z",
    )
    assert result.returncode != 0
    assert result.stdout == b""
    assert b"10000-item safety limit" in result.stderr


def test_github_generic_list_uses_finite_numbered_pages(helpers: HelperInstallation) -> None:
    helpers.fake(
        "gh",
        """case "$*" in
  *" page=1"*) jq -cn '[range(0; 100) | {id:("page-1-" + tostring)}]' ;;
  *" page=2"*) printf '%s\n' '[{"id":"page-2"}]' ;;
  *) exit 2 ;;
esac
""",
    )
    result = helpers.run("github-generic-list-json.py", "/user/starred")
    assert result.returncode == 0, result.stderr.decode(errors="replace")
    records = json.loads(result.stdout)
    assert len(records) == 101
    assert records[-1]["id"] == "page-2"


@pytest.mark.parametrize(("third_page_size", "succeeds"), [(100, False), (1, True)])
def test_github_events_proves_the_documented_three_page_boundary(
    helpers: HelperInstallation,
    third_page_size: int,
    succeeds: bool,
) -> None:
    start_day = datetime.combine((datetime.now(UTC) - timedelta(days=2)).date(), time(), tzinfo=UTC)
    end_day = start_day + timedelta(days=1)
    start = start_day.isoformat().replace("+00:00", "Z")
    end = end_day.isoformat().replace("+00:00", "Z")
    event_time = (start_day + timedelta(hours=12)).isoformat().replace("+00:00", "Z")
    call_log = helpers.root / "gh-calls"
    helpers.fake(
        "gh",
        """printf '%s\n' "$*" >> "$GH_CALL_LOG"
case "$*" in
  *" /user --jq .login"*) printf '%s\n' fmind ;;
  *" page=1"*) jq -cn --arg time "$GH_EVENT_TIME" '[range(0; 100) | {id:("page-1-" + tostring),type:"PushEvent",created_at:$time,public:true,actor:{login:"fmind"},repo:{name:"fmind/fkf"},org:null}]' ;;
  *" page=2"*) jq -cn --arg time "$GH_EVENT_TIME" '[range(0; 100) | {id:("page-2-" + tostring),type:"PushEvent",created_at:$time,public:true,actor:{login:"fmind"},repo:{name:"fmind/fkf"},org:null}]' ;;
  *" page=3"*) jq -cn --arg time "$GH_EVENT_TIME" --argjson size "$THIRD_PAGE_SIZE" '[range(0; $size) | {id:("page-3-" + tostring),type:"IssuesEvent",created_at:$time,public:true,actor:{login:"fmind"},repo:{name:"fmind/fkf"},org:null}]' ;;
  *) exit 42 ;;
esac
""",
    )
    result = helpers.run(
        "github-events-json.py",
        start,
        end,
        environment={
            "GH_CALL_LOG": os.fspath(call_log),
            "GH_EVENT_TIME": event_time,
            "THIRD_PAGE_SIZE": str(third_page_size),
        },
    )
    assert (result.returncode == 0) is succeeds
    assert "page=4" not in call_log.read_text(encoding="utf-8")
    if succeeds:
        assert len(json.loads(result.stdout)) == 201
    else:
        assert result.stdout == b""
        assert b"300-event feed cuts off" in result.stderr
        assert b"completeness cannot be proved" in result.stderr


def test_kaggle_paginator_stops_on_a_short_page_and_normalizes_empty_lists(helpers: HelperInstallation) -> None:
    helpers.fake(
        "kaggle",
        """case "$*" in
  *" -p 1"*) jq -cn '[range(0; 100) | {ref:("owner/page-1-" + tostring)}]' ;;
  *" -p 2"*) printf '%s\n' '[{"ref":"owner/page-2"}]' ;;
  *"competitions list"*) printf '%s\n' 'No competitions found' ;;
  *) exit 2 ;;
esac
""",
    )
    paged = helpers.run("kaggle-json.py", "--all-pages", "datasets", "list", "--format", "json")
    assert paged.returncode == 0, paged.stderr.decode(errors="replace")
    records = json.loads(paged.stdout)
    assert len(records) == 101
    assert records[-1]["ref"] == "owner/page-2"

    empty = helpers.run("kaggle-json.py", "competitions", "list", "--format", "json")
    assert empty.returncode == 0, empty.stderr.decode(errors="replace")
    assert json.loads(empty.stdout) == []


def test_kaggle_helper_bounds_each_provider_page_before_decoding(
    helpers: HelperInstallation,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    helpers.fake("kaggle", "printf 123456789\n")
    namespace = runpy.run_path(os.fspath(helpers.bin / "kaggle-json.py"))
    invoke = namespace["invoke"]
    assert isinstance(invoke, FunctionType)
    monkeypatch.setitem(invoke.__globals__, "MAX_PROVIDER_BYTES", 8)
    monkeypatch.setenv("PATH", helpers.environment()["PATH"])

    with pytest.raises(ValueError, match="provider response exceeds"):
        invoke(["datasets", "list", "--format", "json"])
