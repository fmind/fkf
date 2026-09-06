"""Coding-agent collectors bind local inputs and exact GitHub identities."""

from __future__ import annotations

import json
import os
import runpy
import sqlite3
from pathlib import Path
from types import ModuleType
from typing import Any

import pytest

from .conftest import HelperInstallation

START = "2026-05-04T00:00:00Z"
END = "2026-05-05T00:00:00Z"


def _module(helpers: HelperInstallation, name: str) -> dict[str, Any]:
    return runpy.run_path(os.fspath(helpers.bin / name))


def _prompt_line() -> bytes:
    return (
        json.dumps(
            {"ts": START, "role": "user", "sid": "session-1", "content": "Bound the prompt."},
            separators=(",", ":"),
        ).encode()
        + b"\n"
    )


def _claude_line() -> bytes:
    return (
        json.dumps(
            {"timestamp": START, "type": "user", "sessionId": "session-1", "cwd": "/work"},
            separators=(",", ":"),
        ).encode()
        + b"\n"
    )


def _transcript(helpers: HelperInstallation, helper: str) -> Path:
    if helper == "agent-sessions.py":
        path = helpers.home / ".claude" / "projects" / "project" / "session.jsonl"
        content = _claude_line()
    else:
        path = helpers.home / ".agents" / "sessions" / "v1" / "codex" / "lineage" / "session" / "transcript.jsonl"
        content = _prompt_line()
    path.parent.mkdir(parents=True)
    path.write_bytes(content)
    return path


def _arguments(helper: str) -> list[str]:
    if helper == "agent-prompts.py":
        return [START, END, "0"]
    if helper == "agent-sessions.py":
        return [START, END]
    return ["codex-session-1-20260504T000000Z"]


def test_prompt_repository_accepts_only_explicit_github_remotes(
    helpers: HelperInstallation,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    namespace = _module(helpers, "agent-prompts.py")
    repository = namespace["repository"]
    worktree = helpers.home / "work"
    worktree.mkdir()
    remote = ["https://gitlab.example.test/acme/project.git"]

    class Process:
        stdout = None

        def __enter__(self) -> Process:
            import io

            self.stdout = io.BytesIO((remote[0] + "\n").encode())
            return self

        def __exit__(self, *_arguments: object) -> None:
            return None

        def wait(self) -> int:
            return 0

        def kill(self) -> None:
            return None

    monkeypatch.setattr(repository.__globals__["subprocess"], "Popen", lambda *_arguments, **_keywords: Process())
    assert repository(os.fspath(worktree)) is None

    remote[0] = "git@github.com:acme/project.git"
    assert repository(os.fspath(worktree)) == "acme/project"


@pytest.mark.parametrize("helper", ["agent-prompts.py", "agent-prompt-body.py", "agent-sessions.py"])
def test_agent_helpers_reject_symlinked_store_components(
    helpers: HelperInstallation,
    helper: str,
) -> None:
    outside_home = helpers.root / "outside"
    if helper == "agent-sessions.py":
        transcript = outside_home / ".claude" / "projects" / "project" / "session.jsonl"
        transcript.parent.mkdir(parents=True)
        transcript.write_bytes(_claude_line())
        (helpers.home / ".claude").symlink_to(outside_home / ".claude", target_is_directory=True)
    else:
        transcript = outside_home / ".agents" / "sessions" / "v1" / "codex" / "lineage" / "session" / "transcript.jsonl"
        transcript.parent.mkdir(parents=True)
        transcript.write_bytes(_prompt_line())
        (helpers.home / ".agents").symlink_to(outside_home / ".agents", target_is_directory=True)

    result = helpers.run(helper, *_arguments(helper))
    assert result.returncode == 1
    assert result.stdout == b""
    assert b"linked" in result.stderr


@pytest.mark.parametrize("helper", ["agent-prompts.py", "agent-prompt-body.py", "agent-sessions.py"])
def test_agent_helpers_reject_transcript_replacement_at_open(
    helpers: HelperInstallation,
    monkeypatch: pytest.MonkeyPatch,
    capfd: pytest.CaptureFixture[str],
    helper: str,
) -> None:
    transcript = _transcript(helpers, helper)
    replacement = helpers.root / f"{helper}.replacement"
    replacement.write_bytes(transcript.read_bytes())
    namespace = _module(helpers, helper)
    main = namespace["main"]
    real_open = os.open
    replaced = False

    def replacing_open(
        path: str | bytes | os.PathLike[str] | os.PathLike[bytes],
        flags: int,
        *positional: int,
        **keywords: Any,
    ) -> int:
        nonlocal replaced
        if os.fsdecode(path) == transcript.name and keywords.get("dir_fd") is not None and not replaced:
            replaced = True
            transcript.rename(transcript.with_suffix(".old"))
            replacement.replace(transcript)
        return real_open(path, flags, *positional, **keywords)

    isolated_os = ModuleType("isolated_os")
    isolated_os.__dict__.update(vars(os))
    isolated_os.__dict__["open"] = replacing_open
    monkeypatch.setitem(main.__globals__, "os", isolated_os)
    monkeypatch.setenv("HOME", os.fspath(helpers.home))
    monkeypatch.setenv("PATH", helpers.environment()["PATH"])

    assert main(_arguments(helper)) == 1
    captured = capfd.readouterr()
    assert captured.out == ""
    assert "changed while it was being opened" in captured.err


def _write_opencode(path: Path, identifier: str) -> None:
    connection = sqlite3.connect(path)
    try:
        connection.executescript(
            """
            create table session (
                id text primary key,
                title text,
                directory text,
                parent_id text,
                time_created integer
            );
            create table message (session_id text, time_created integer);
            """
        )
        connection.execute(
            "insert into session values (?, ?, ?, null, ?)",
            (identifier, "Session", "/work", 1_777_852_800_000),
        )
        connection.commit()
    finally:
        connection.close()


def test_agent_sessions_rejects_database_replacement_while_connecting(
    helpers: HelperInstallation,
    monkeypatch: pytest.MonkeyPatch,
    capfd: pytest.CaptureFixture[str],
) -> None:
    database = helpers.home / ".local" / "share" / "opencode" / "opencode.db"
    database.parent.mkdir(parents=True)
    _write_opencode(database, "original")
    replacement = helpers.root / "replacement.db"
    _write_opencode(replacement, "replacement")
    namespace = _module(helpers, "agent-sessions.py")
    main = namespace["main"]
    real_connect = sqlite3.connect
    replaced = False

    def replacing_connect(database_uri: str, *, uri: bool = False) -> sqlite3.Connection:
        nonlocal replaced
        if not replaced and database_uri.startswith("file:"):
            replaced = True
            database.rename(database.with_suffix(".old"))
            replacement.replace(database)
        return real_connect(database_uri, uri=uri)

    monkeypatch.setattr(namespace["sqlite3"], "connect", replacing_connect)
    monkeypatch.setenv("HOME", os.fspath(helpers.home))
    monkeypatch.setenv("PATH", helpers.environment()["PATH"])

    assert main([START, END]) == 1
    captured = capfd.readouterr()
    assert captured.out == ""
    assert "database changed while it was being opened" in captured.err


def test_prompt_body_rejects_a_traversing_harness_identifier(helpers: HelperInstallation) -> None:
    transcript = helpers.home / ".agents" / "sessions" / "lineage" / "session" / "transcript.jsonl"
    transcript.parent.mkdir(parents=True)
    transcript.write_bytes(_prompt_line())

    result = helpers.run("agent-prompt-body.py", "..-session-1-20260504T000000Z")
    assert result.returncode == 2
    assert result.stdout == b""
    assert b"invalid harness" in result.stderr


def test_agent_sessions_treats_absent_stores_in_existing_provider_directories_as_empty(
    helpers: HelperInstallation,
) -> None:
    for directory in (
        helpers.home / ".local/share/opencode",
        helpers.home / ".copilot",
        helpers.home / ".gemini/antigravity-cli",
    ):
        directory.mkdir(parents=True)

    result = helpers.run("agent-sessions.py", START, END)

    assert result.returncode == 0, result.stderr.decode(errors="replace")
    assert json.loads(result.stdout) == []
