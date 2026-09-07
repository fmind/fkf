"""GitHub Actions collection is complete and globally bounded."""

from __future__ import annotations

import json
import os
import runpy
from typing import Any

import pytest

from .conftest import HelperInstallation


def _module(helpers: HelperInstallation) -> dict[str, Any]:
    return runpy.run_path(os.fspath(helpers.bin / "gh-runs.py"))


def _run(run_id: int) -> dict[str, object]:
    return {
        "id": run_id,
        "name": "CI",
        "display_title": f"run {run_id}",
        "status": "completed",
        "conclusion": "success",
        "created_at": "2026-05-04T00:00:00Z",
        "updated_at": "2026-05-04T00:00:01Z",
        "head_branch": "main",
        "head_sha": f"{run_id:040x}",
        "event": "push",
        "html_url": f"https://github.example.test/acme/project/actions/runs/{run_id}",
    }


def _envelope(total: int, runs: list[dict[str, object]]) -> bytes:
    return json.dumps({"total_count": total, "workflow_runs": runs}, separators=(",", ":")).encode()


def test_actions_total_count_mismatch_fails_instead_of_accepting_a_short_page(
    helpers: HelperInstallation,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    namespace = _module(helpers)
    collect = namespace["collect"]
    responses = iter((_envelope(2, [_run(1)]), _envelope(2, [])))
    monkeypatch.setitem(collect.__globals__, "invoke", lambda _arguments: next(responses))

    with pytest.raises(RuntimeError, match="complete workflow-run result"):
        collect("acme/project", 1_777_852_800, 1_777_939_200)


def test_actions_collection_shares_call_and_record_budgets_across_pages(
    helpers: HelperInstallation,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    namespace = _module(helpers)
    collect = namespace["collect"]
    first_page = [_run(index) for index in range(1, 101)]
    responses = iter((_envelope(101, first_page), _envelope(101, [_run(101)])))
    monkeypatch.setitem(collect.__globals__, "invoke", lambda _arguments: next(responses))
    monkeypatch.setitem(collect.__globals__, "MAX_PROVIDER_CALLS", 1)

    with pytest.raises(RuntimeError, match="provider-call bound"):
        collect("acme/project", 1_777_852_800, 1_777_939_200)

    responses = iter((_envelope(2, [_run(1), _run(2)]),))
    monkeypatch.setitem(collect.__globals__, "invoke", lambda _arguments: next(responses))
    monkeypatch.setitem(collect.__globals__, "MAX_PROVIDER_CALLS", 10)
    monkeypatch.setitem(collect.__globals__, "MAX_RECORDS", 1)
    with pytest.raises(RuntimeError, match="record bound"):
        collect("acme/project", 1_777_852_800, 1_777_939_200)


def test_actions_main_bounds_repositories_and_exact_final_output(
    helpers: HelperInstallation,
    monkeypatch: pytest.MonkeyPatch,
    capfd: pytest.CaptureFixture[str],
) -> None:
    namespace = _module(helpers)
    main = namespace["main"]
    repository_payload = [b'[{"full_name":"acme/one"},{"full_name":"acme/two"}]']
    expected_record = {
        "databaseId": 1,
        "workflowName": "CI",
        "displayTitle": "run 1",
        "status": "completed",
        "conclusion": "success",
        "createdAt": "2026-05-04T00:00:00Z",
        "updatedAt": "2026-05-04T00:00:01Z",
        "headBranch": "main",
        "headSha": f"{1:040x}",
        "event": "push",
        "url": "https://github.example.test/acme/project/actions/runs/1",
        "repo": "acme/project",
        "repository_uri": "repo:github.com/acme/project",
    }

    def fake_invoke(arguments: list[str]) -> bytes:
        if arguments[:3] == ["gh", "api", "/user"]:
            return b"acme\n"
        if arguments[:1] == [main.__globals__["sys"].executable]:
            return repository_payload[0]
        raise AssertionError(arguments)

    monkeypatch.setitem(main.__globals__, "invoke", fake_invoke)
    monkeypatch.setitem(main.__globals__, "collect", lambda *_arguments, **_keywords: [expected_record])
    monkeypatch.setitem(main.__globals__, "MAX_REPOSITORIES", 1)
    assert main(["2026-05-04T00:00:00+00:00", "2026-05-05T00:00:00+00:00"]) == 1
    captured = capfd.readouterr()
    assert captured.out == ""
    assert "repository bound" in captured.err

    monkeypatch.setitem(main.__globals__, "MAX_REPOSITORIES", 2)
    repository_payload[0] = b'[{"full_name":"acme/one"}]'
    expected = json.dumps([expected_record], ensure_ascii=False, separators=(",", ":")) + "\n"
    monkeypatch.setitem(main.__globals__, "MAX_OUTPUT_BYTES", len(expected.encode()))
    assert main(["2026-05-04T00:00:00+00:00", "2026-05-05T00:00:00+00:00"]) == 0
    assert capfd.readouterr().out == expected

    monkeypatch.setitem(main.__globals__, "MAX_OUTPUT_BYTES", len(expected.encode()) - 1)
    assert main(["2026-05-04T00:00:00+00:00", "2026-05-05T00:00:00+00:00"]) == 1
    captured = capfd.readouterr()
    assert captured.out == ""
    assert "output bound" in captured.err
