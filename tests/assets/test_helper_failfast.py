"""Provider failures and completeness defects never become partial durable evidence."""

from __future__ import annotations

import json
import os
from typing import cast

import pytest

from .conftest import HelperInstallation


def _jira_issue(key: str) -> dict[str, object]:
    return {
        "key": key,
        "fields": {
            "summary": "Shared issue",
            "status": {"name": "In Progress"},
            "assignee": {"displayName": "Example Owner"},
            "url": f"https://team.atlassian.net/browse/{key}",
        },
    }


def _run_jira(helpers: HelperInstallation, payload: str, *, exit_code: int = 0):
    call_log = helpers.root / "acli-call"
    payload_file = helpers.root / "acli-payload.json"
    payload_file.write_text(payload, encoding="utf-8")
    helpers.fake(
        "acli",
        """printf '%s\n' "$*" > "$ACLI_CALL_LOG"
cat "$ACLI_PAYLOAD"
exit "$ACLI_EXIT_CODE"
""",
    )
    return (
        helpers.run(
            "jira-issues-json.py",
            "team.atlassian.net",
            "TEAM",
            "statusCategory != Done",
            environment={
                "ACLI_CALL_LOG": os.fspath(call_log),
                "ACLI_EXIT_CODE": str(exit_code),
                "ACLI_PAYLOAD": os.fspath(payload_file),
            },
        ),
        call_log,
    )


@pytest.mark.parametrize("issues", [[_jira_issue("TEAM-1"), _jira_issue("TEAM-2")], []])
def test_jira_accepts_one_bounded_aggregate_and_projects_it(
    helpers: HelperInstallation,
    issues: list[dict[str, object]],
) -> None:
    result, call_log = _run_jira(helpers, json.dumps({"issues": issues}, separators=(",", ":")))
    assert result.returncode == 0, result.stderr.decode(errors="replace")
    assert len(json.loads(result.stdout)) == len(issues)
    call = call_log.read_text(encoding="utf-8")
    assert 'project = "TEAM" AND (statusCategory != Done) ORDER BY key ASC' in call
    assert "--fields key,summary,status,assignee,url" in call
    assert "--limit 10001" in call
    assert "--json" in call


@pytest.mark.parametrize(
    ("payload", "exit_code"),
    [
        ('{"error":"denied"}', 1),
        ('{"issues":"wrong"}', 0),
        ('[{"key":"OTHER-1","summary":"wrong","url":"https://team.atlassian.net/browse/OTHER-1"}]', 0),
        ('{"issues":[],"nextPageToken":"same"}', 0),
        (
            (
                '[{"key":"TEAM-1","summary":"one","url":"https://team.atlassian.net/browse/TEAM-1"},'
                '{"key":"TEAM-1","summary":"two","url":"https://team.atlassian.net/browse/TEAM-1"}]'
            ),
            0,
        ),
    ],
)
def test_jira_rejects_provider_and_boundary_errors_without_output(
    helpers: HelperInstallation,
    payload: str,
    exit_code: int,
) -> None:
    result, _ = _run_jira(helpers, payload, exit_code=exit_code)
    assert result.returncode != 0
    assert result.stdout == b""


def test_jira_rejects_the_limit_plus_one_completeness_sentinel(helpers: HelperInstallation) -> None:
    issues = [_jira_issue(f"TEAM-{index}") for index in range(1, 10002)]
    result, _ = _run_jira(helpers, json.dumps(issues, separators=(",", ":")))
    assert result.returncode != 0
    assert result.stdout == b""
    assert b"excessive" in result.stderr


def test_huggingface_projects_native_cli_metadata_without_storage_claims(helpers: HelperInstallation) -> None:
    helpers.fake(
        "hf",
        """[ "$*" = "repos ls --limit 10001 --format json" ] || exit 64
printf '%s\n' '[{"id":"owner/zeta","type":"dataset","updated":"2026-09-03","visibility":"public","storage":"30 bytes"},{"id":"owner/alpha","type":"model","updated":"2026-09-02","visibility":"private","storage":"10 bytes"},{"id":"owner/beta","type":"bucket","updated":"2026-09-01","visibility":"private","storage":"20 bytes"}]'
""",
    )
    result = helpers.run("huggingface-repositories-json.py")
    assert result.returncode == 0, result.stderr.decode(errors="replace")
    records = cast(list[dict[str, object]], json.loads(result.stdout))
    assert [record["uid"] for record in records] == [
        "model:owner/alpha",
        "bucket:owner/beta",
        "dataset:owner/zeta",
    ]
    assert records[1]["url"] == "https://huggingface.co/buckets/owner/beta"
    assert b"storage" not in result.stdout


@pytest.mark.parametrize("case", ["malformed", "duplicate", "unknown", "over", "partial"])
def test_huggingface_incomplete_inventory_never_emits_a_prefix(
    helpers: HelperInstallation,
    case: str,
) -> None:
    helpers.fake(
        "hf",
        """case "$HF_CASE" in
  malformed) printf '%s\n' '{' ;;
  duplicate) printf '%s\n' '[{"id":"owner/a","type":"model"},{"id":"owner/a","type":"model"}]' ;;
  unknown) printf '%s\n' '[{"id":"owner/a","type":"collection"}]' ;;
  over) jq -nc '[range(0; 10001) | {id:("owner/repo-" + tostring),type:"model",updated:"2026-09-03",visibility:"private"}]' ;;
  partial) printf '%s\n' '[{"id":"owner/partial","type":"model"}]'; exit 9 ;;
  *) exit 64 ;;
esac
""",
    )
    result = helpers.run("huggingface-repositories-json.py", environment={"HF_CASE": case})
    assert result.returncode != 0
    assert result.stdout == b""
    assert b"cannot prove a complete repository inventory" in result.stderr


def test_agent_session_provider_failures_emit_no_partial_records(helpers: HelperInstallation) -> None:
    database = helpers.home / ".local" / "share" / "opencode" / "opencode.db"
    database.parent.mkdir(parents=True)
    database.touch()
    database.write_text("not a sqlite database", encoding="utf-8")
    sqlite_failure = helpers.run(
        "agent-sessions.py",
        "2026-05-04T00:00:00Z",
        "2026-05-05T00:00:00Z",
    )
    assert sqlite_failure.returncode != 0
    assert sqlite_failure.stdout == b""

    claude = helpers.home / ".claude" / "projects"
    claude.mkdir(parents=True)
    (claude / "broken.jsonl").write_text("{\n", encoding="utf-8")
    find_failure = helpers.run(
        "agent-sessions.py",
        "2026-05-04T00:00:00Z",
        "2026-05-05T00:00:00Z",
    )
    assert find_failure.returncode != 0
    assert find_failure.stdout == b""


@pytest.mark.parametrize(
    ("helper", "arguments", "home_directory", "provider", "provider_body", "error"),
    [
        (
            "agent-memory-files.py",
            ("not-a-time", "2026-05-05T00:00:00Z"),
            ".codex/memories",
            "unused-find",
            "exit 39\n",
            b"",
        ),
        (
            "git-log-json.py",
            ("not-a-time", "2026-05-05", "{root}", "author@example.test"),
            "repositories",
            "unused-git",
            "exit 40\n",
            b"",
        ),
        (
            "github-commits-json.py",
            ("2026-05-04T00:00:00Z", "2026-05-05T00:00:00Z"),
            "repositories",
            "gh",
            "printf '%s\\n' 'API rate limit exceeded for this account' >&2\nexit 1\n",
            b"API rate limit exceeded",
        ),
    ],
)
def test_representative_helper_failures_do_not_emit_partial_stdout(
    helpers: HelperInstallation,
    helper: str,
    arguments: tuple[str, ...],
    home_directory: str,
    provider: str,
    provider_body: str,
    error: bytes,
) -> None:
    directory = helpers.home / home_directory
    directory.mkdir(parents=True)
    helpers.fake(provider, provider_body)
    rendered = tuple(os.fspath(directory) if argument == "{root}" else argument for argument in arguments)
    result = helpers.run(helper, *rendered)
    assert result.returncode != 0
    assert result.stdout == b""
    assert error in result.stderr
