"""Recursive GitHub collectors share finite work and output budgets."""

from __future__ import annotations

import json
import os
import runpy
from typing import Any

import pytest

from .conftest import HelperInstallation

START = "2026-05-04T00:00:00Z"
END = "2026-05-05T00:00:00Z"
START_EPOCH = 1_777_852_800
END_EPOCH = 1_777_939_200


def _module(helpers: HelperInstallation, name: str) -> dict[str, Any]:
    return runpy.run_path(os.fspath(helpers.bin / name))


def _commit(index: int, timestamp: str = START) -> dict[str, object]:
    return {
        "sha": f"{index:040x}",
        "url": f"https://github.example.test/acme/project/commit/{index}",
        "repository": {"fullName": "acme/project"},
        "commit": {"author": {"date": timestamp, "email": "author@example.test"}, "message": f"Commit {index}"},
    }


def _issue(index: int) -> dict[str, object]:
    return {
        "number": index,
        "title": f"Issue {index}",
        "html_url": f"https://github.example.test/acme/project/issues/{index}",
        "updated_at": START,
        "repository_url": "https://api.github.example.test/repos/acme/project",
        "state": "open",
        "user": {"login": "author"},
        "assignees": [],
    }


def _search(total: int, items: list[dict[str, object]]) -> bytes:
    return json.dumps(
        {"total_count": total, "incomplete_results": False, "items": items},
        separators=(",", ":"),
    ).encode()


@pytest.mark.parametrize("name", ["github-commits-json.py", "github-search-json.py"])
def test_recursive_github_collectors_share_provider_call_budget(
    helpers: HelperInstallation,
    monkeypatch: pytest.MonkeyPatch,
    name: str,
) -> None:
    namespace = _module(helpers, name)
    collect = namespace["collect"]
    response = (
        json.dumps([_commit(index) for index in range(1000)]).encode()
        if name.startswith("github-commits")
        else _search(1000, [])
    )
    monkeypatch.setitem(collect.__globals__, "invoke", lambda _arguments: response)
    monkeypatch.setitem(collect.__globals__, "MAX_PROVIDER_CALLS", 1)
    monkeypatch.setitem(collect.__globals__, "MAX_RECORDS", 10_000)
    arguments: tuple[object, ...] = () if name.startswith("github-commits") else ("is:issue", "assignee:@me", None)

    with pytest.raises(RuntimeError, match="provider-call bound"):
        collect(START_EPOCH, END_EPOCH, *arguments)


@pytest.mark.parametrize("name", ["github-commits-json.py", "github-search-json.py"])
def test_recursive_github_collectors_enforce_record_bound(
    helpers: HelperInstallation,
    monkeypatch: pytest.MonkeyPatch,
    name: str,
) -> None:
    namespace = _module(helpers, name)
    collect = namespace["collect"]
    response = (
        json.dumps([_commit(1), _commit(2)]).encode()
        if name.startswith("github-commits")
        else _search(2, [_issue(1), _issue(2)])
    )
    monkeypatch.setitem(collect.__globals__, "invoke", lambda _arguments: response)
    monkeypatch.setitem(collect.__globals__, "MAX_RECORDS", 1)
    arguments: tuple[object, ...] = () if name.startswith("github-commits") else ("is:issue", "assignee:@me", None)

    with pytest.raises(RuntimeError, match="record bound"):
        collect(START_EPOCH, END_EPOCH, *arguments)


def test_github_commits_rejects_provider_results_outside_the_requested_window(
    helpers: HelperInstallation,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    namespace = _module(helpers, "github-commits-json.py")
    collect = namespace["collect"]
    monkeypatch.setitem(collect.__globals__, "invoke", lambda _arguments: json.dumps([_commit(1, END)]).encode())

    with pytest.raises(RuntimeError, match="outside the requested range"):
        collect(START_EPOCH, END_EPOCH)


@pytest.mark.parametrize("name", ["github-commits-json.py", "github-search-json.py"])
def test_recursive_github_collectors_enforce_exact_newline_inclusive_output_bound(
    helpers: HelperInstallation,
    monkeypatch: pytest.MonkeyPatch,
    capfd: pytest.CaptureFixture[str],
    name: str,
) -> None:
    namespace = _module(helpers, name)
    main = namespace["main"]
    record = (
        namespace["validate"](_commit(1))
        if name.startswith("github-commits")
        else namespace["projected"](_issue(1), "acme/project")
    )
    monkeypatch.setitem(main.__globals__, "collect", lambda *_arguments, **_keywords: [record])
    arguments = [START, END] if name.startswith("github-commits") else ["issues", "assignee", START, END]
    expected = json.dumps([record], ensure_ascii=False, separators=(",", ":")) + "\n"

    monkeypatch.setitem(main.__globals__, "MAX_OUTPUT_BYTES", len(expected.encode()))
    assert main(arguments) == 0
    assert capfd.readouterr().out == expected

    monkeypatch.setitem(main.__globals__, "MAX_OUTPUT_BYTES", len(expected.encode()) - 1)
    assert main(arguments) == 1
    captured = capfd.readouterr()
    assert captured.out == ""
    assert "output bound" in captured.err
