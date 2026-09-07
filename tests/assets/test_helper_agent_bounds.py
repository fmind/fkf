"""Resource bounds for local coding-agent transcript helpers."""

from __future__ import annotations

import json
import os
import runpy
from pathlib import Path
from typing import Any

import pytest

from .conftest import HelperInstallation

START = "2026-05-04T00:00:00Z"
END = "2026-05-05T00:00:00Z"


def _module(helpers: HelperInstallation, name: str) -> dict[str, Any]:
    return runpy.run_path(os.fspath(helpers.bin / name))


def _normalized_transcript(helpers: HelperInstallation) -> Path:
    path = helpers.home / ".agents" / "sessions" / "v1" / "codex" / "lineage" / "session" / "transcript.jsonl"
    path.parent.mkdir(parents=True, exist_ok=True)
    return path


def _prompt_line(*, content: str = "Bound the prompt.", cwd: str | None = None, sid: str = "session-1") -> bytes:
    return (
        json.dumps(
            {"ts": START, "role": "user", "sid": sid, "content": content, "cwd": cwd, "model": "test"},
            separators=(",", ":"),
        ).encode()
        + b"\n"
    )


def _claude_line(*, cwd: str | None = None, session: str = "session-1") -> bytes:
    return (
        json.dumps(
            {"timestamp": START, "type": "user", "sessionId": session, "cwd": cwd, "gitBranch": "main"},
            separators=(",", ":"),
        ).encode()
        + b"\n"
    )


def _run_direct(
    helpers: HelperInstallation,
    monkeypatch: pytest.MonkeyPatch,
    helper: str,
    arguments: list[str],
    **limits: int,
) -> int:
    namespace = _module(helpers, helper)
    main = namespace["main"]
    for name, value in limits.items():
        monkeypatch.setitem(main.__globals__, name, value)
    monkeypatch.setenv("HOME", os.fspath(helpers.home))
    monkeypatch.setenv("PATH", helpers.environment()["PATH"])
    return main(arguments)


@pytest.mark.parametrize("helper", ["agent-prompts.py", "agent-prompt-body.py"])
def test_normalized_prompt_helpers_accept_exact_jsonl_lines_and_reject_one_byte_over(
    helpers: HelperInstallation,
    monkeypatch: pytest.MonkeyPatch,
    capfd: pytest.CaptureFixture[str],
    helper: str,
) -> None:
    transcript = _normalized_transcript(helpers)
    line = _prompt_line()
    transcript.write_bytes(line)
    arguments = [START, END, "0"] if helper == "agent-prompts.py" else ["codex-session-1-20260504T000000Z"]

    assert _run_direct(helpers, monkeypatch, helper, arguments, MAX_JSON_BYTES=len(line)) == 0
    exact = capfd.readouterr()
    assert exact.out

    transcript.write_bytes(line[:-1] + b" \n")
    assert _run_direct(helpers, monkeypatch, helper, arguments, MAX_JSON_BYTES=len(line)) == 1
    oversized = capfd.readouterr()
    assert oversized.out == ""
    assert "JSON line exceeds 8 MiB" in oversized.err


def test_prompt_body_retains_only_one_unique_match(helpers: HelperInstallation) -> None:
    transcript = _normalized_transcript(helpers)
    transcript.write_bytes(_prompt_line(content="First.") + _prompt_line(content="Second."))

    result = helpers.run("agent-prompt-body.py", "codex-session-1-20260504T000000Z")

    assert result.returncode == 1
    assert result.stdout == b""
    assert b"2 distinct bodies match" in result.stderr


def test_agent_sessions_accepts_exact_jsonl_lines_and_rejects_one_byte_over(
    helpers: HelperInstallation,
    monkeypatch: pytest.MonkeyPatch,
    capfd: pytest.CaptureFixture[str],
) -> None:
    transcript = helpers.home / ".claude" / "projects" / "project" / "session.jsonl"
    transcript.parent.mkdir(parents=True)
    line = _claude_line()
    transcript.write_bytes(line)

    assert _run_direct(helpers, monkeypatch, "agent-sessions.py", [START, END], MAX_JSON_BYTES=len(line)) == 0
    assert json.loads(capfd.readouterr().out)[0]["id"] == "claude:session-1"

    transcript.write_bytes(line[:-1] + b" \n")
    assert _run_direct(helpers, monkeypatch, "agent-sessions.py", [START, END], MAX_JSON_BYTES=len(line)) == 1
    oversized = capfd.readouterr()
    assert oversized.out == ""
    assert "JSON line exceeds 8 MiB" in oversized.err


def test_agent_sessions_bounds_whole_json_documents(
    helpers: HelperInstallation,
    monkeypatch: pytest.MonkeyPatch,
    capfd: pytest.CaptureFixture[str],
) -> None:
    session = helpers.home / ".gemini" / "tmp" / "project" / "chats" / "session-1.json"
    session.parent.mkdir(parents=True)
    document = json.dumps(
        {"sessionId": "session-1", "kind": "main", "startTime": START, "messages": []},
        separators=(",", ":"),
    ).encode()
    session.write_bytes(document)

    assert _run_direct(helpers, monkeypatch, "agent-sessions.py", [START, END], MAX_JSON_BYTES=len(document)) == 0
    assert json.loads(capfd.readouterr().out)[0]["id"] == "gemini:session-1"

    session.write_bytes(document + b" ")
    assert _run_direct(helpers, monkeypatch, "agent-sessions.py", [START, END], MAX_JSON_BYTES=len(document)) == 1
    oversized = capfd.readouterr()
    assert oversized.out == ""
    assert "JSON document exceeds 8 MiB" in oversized.err


@pytest.mark.parametrize("helper", ["agent-prompts.py", "agent-sessions.py"])
def test_agent_collectors_bound_nested_git_output_before_stdout(
    helpers: HelperInstallation,
    monkeypatch: pytest.MonkeyPatch,
    capfd: pytest.CaptureFixture[str],
    helper: str,
) -> None:
    worktree = helpers.home / "work"
    worktree.mkdir()
    helpers.fake("git", "printf 123456789\n")
    if helper == "agent-prompts.py":
        _normalized_transcript(helpers).write_bytes(_prompt_line(cwd=os.fspath(worktree)))
        arguments = [START, END, "0"]
    else:
        transcript = helpers.home / ".claude" / "projects" / "project" / "session.jsonl"
        transcript.parent.mkdir(parents=True)
        transcript.write_bytes(_claude_line(cwd=os.fspath(worktree)))
        arguments = [START, END]

    assert _run_direct(helpers, monkeypatch, helper, arguments, MAX_GIT_BYTES=8) == 1
    captured = capfd.readouterr()
    assert captured.out == ""
    assert "git output exceeds 1 MiB" in captured.err


@pytest.mark.parametrize("helper", ["agent-prompts.py", "agent-sessions.py"])
def test_agent_collectors_bound_retained_candidate_paths(
    helpers: HelperInstallation,
    monkeypatch: pytest.MonkeyPatch,
    capfd: pytest.CaptureFixture[str],
    helper: str,
) -> None:
    if helper == "agent-prompts.py":
        root = helpers.home / ".agents" / "sessions" / "v1" / "codex"
        for name in ("one", "two"):
            transcript = root / name / "session" / "transcript.jsonl"
            transcript.parent.mkdir(parents=True)
            transcript.write_bytes(_prompt_line().replace(START.encode(), b"2026-05-03T00:00:00Z"))
        arguments = [START, END, "0"]
        expected = "more than 1 transcript files"
    else:
        root = helpers.home / ".claude" / "projects"
        for name in ("one", "two"):
            transcript = root / name / "session.jsonl"
            transcript.parent.mkdir(parents=True)
            transcript.write_bytes(_claude_line().replace(START.encode(), b"2026-05-03T00:00:00Z"))
        arguments = [START, END]
        expected = "more than 1 session files"

    assert _run_direct(helpers, monkeypatch, helper, arguments, MAX_FILES=1) == 1
    captured = capfd.readouterr()
    assert captured.out == ""
    assert expected in captured.err


def test_agent_prompts_bounds_retained_records_and_final_output(
    helpers: HelperInstallation,
    monkeypatch: pytest.MonkeyPatch,
    capfd: pytest.CaptureFixture[str],
) -> None:
    transcript = _normalized_transcript(helpers)
    first = _prompt_line(sid="one")
    transcript.write_bytes(first)

    assert _run_direct(helpers, monkeypatch, "agent-prompts.py", [START, END, "0"], MAX_RECORDS=1) == 0
    baseline = capfd.readouterr().out
    assert len(json.loads(baseline)) == 1

    assert (
        _run_direct(
            helpers,
            monkeypatch,
            "agent-prompts.py",
            [START, END, "0"],
            MAX_RECORDS=1,
            MAX_OUTPUT_BYTES=len(baseline.encode()),
        )
        == 0
    )
    assert capfd.readouterr().out == baseline

    assert (
        _run_direct(
            helpers,
            monkeypatch,
            "agent-prompts.py",
            [START, END, "0"],
            MAX_RECORDS=1,
            MAX_OUTPUT_BYTES=len(baseline.encode()) - 1,
        )
        == 1
    )
    over_output = capfd.readouterr()
    assert over_output.out == ""
    assert "output exceeds 64 MiB" in over_output.err

    transcript.write_bytes(first + _prompt_line(sid="two"))
    assert _run_direct(helpers, monkeypatch, "agent-prompts.py", [START, END, "0"], MAX_RECORDS=1) == 1
    over_records = capfd.readouterr()
    assert over_records.out == ""
    assert "more than 1 prompt records" in over_records.err


def test_agent_sessions_bounds_retained_records_and_final_output(
    helpers: HelperInstallation,
    monkeypatch: pytest.MonkeyPatch,
    capfd: pytest.CaptureFixture[str],
) -> None:
    root = helpers.home / ".claude" / "projects"
    first = root / "one" / "session.jsonl"
    first.parent.mkdir(parents=True)
    first.write_bytes(_claude_line(session="one"))

    assert _run_direct(helpers, monkeypatch, "agent-sessions.py", [START, END], MAX_RECORDS=1) == 0
    baseline = capfd.readouterr().out
    assert len(json.loads(baseline)) == 1

    assert (
        _run_direct(
            helpers,
            monkeypatch,
            "agent-sessions.py",
            [START, END],
            MAX_RECORDS=1,
            MAX_OUTPUT_BYTES=len(baseline.encode()),
        )
        == 0
    )
    assert capfd.readouterr().out == baseline

    assert (
        _run_direct(
            helpers,
            monkeypatch,
            "agent-sessions.py",
            [START, END],
            MAX_RECORDS=1,
            MAX_OUTPUT_BYTES=len(baseline.encode()) - 1,
        )
        == 1
    )
    over_output = capfd.readouterr()
    assert over_output.out == ""
    assert "output exceeds 64 MiB" in over_output.err

    second = root / "two" / "session.jsonl"
    second.parent.mkdir()
    second.write_bytes(_claude_line(session="two"))
    assert _run_direct(helpers, monkeypatch, "agent-sessions.py", [START, END], MAX_RECORDS=1) == 1
    over_records = capfd.readouterr()
    assert over_records.out == ""
    assert "more than 1 session records" in over_records.err


def test_antigravity_retains_only_earliest_time_and_first_workspace_per_identity(
    helpers: HelperInstallation,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    history = helpers.home / ".gemini" / "antigravity-cli" / "history.jsonl"
    history.parent.mkdir(parents=True)
    history.write_text(
        "\n".join(
            (
                '{"conversationId":"one","timestamp":1777888800000}',
                '{"conversationId":"one","timestamp":1777887000000,"workspace":"/first"}',
                '{"conversationId":"one","timestamp":1777885200000,"workspace":"/later"}',
            )
        )
        + "\n",
        encoding="utf-8",
    )
    namespace = _module(helpers, "agent-sessions.py")
    antigravity = namespace["antigravity"]
    monkeypatch.setitem(antigravity.__globals__, "MAX_RECORDS", 1)

    records = list(antigravity(helpers.home, namespace["instant"](START), namespace["instant"](END)))

    assert records[0]["time"] == "2026-05-04T09:00:00Z"
    assert records[0]["cwd"] == "/first"
    source = (helpers.bin / "agent-sessions.py").read_text(encoding="utf-8")
    assert "list[tuple[datetime, object]]" not in source

    with history.open("a", encoding="utf-8") as stream:
        stream.write('{"conversationId":"two","timestamp":1777885200000}\n')
    with pytest.raises(RuntimeError, match="more than 1 session identities"):
        list(antigravity(helpers.home, namespace["instant"](START), namespace["instant"](END)))


def _trace_generation(helpers: HelperInstallation) -> Path:
    generation = helpers.home / ".agents" / "sessions" / "v1" / "codex" / "lineage" / "generation"
    generation.mkdir(parents=True)
    manifest = {
        "schema_version": 1,
        "parser_version": "1",
        "agent": "codex",
        "session_id": "session-1",
        "completeness": "complete",
        "ingested_at": "2026-05-04T10:00:00Z",
        "high_water_mark": "2026-05-04T09:30:00Z",
        "record_count": 1,
    }
    (generation / "manifest.json").write_text(json.dumps(manifest) + "\n", encoding="utf-8")
    (generation / "transcript.jsonl").write_bytes(
        _prompt_line(content="Bound trace files.").replace(b'"role":"user"', b'"agent":"codex","role":"user"')
    )
    return generation


def test_session_trace_rejects_manifest_replacement_at_open(
    helpers: HelperInstallation,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    generation = _trace_generation(helpers)
    manifest = generation / "manifest.json"
    replacement = helpers.root / "replacement-manifest.json"
    replacement.write_bytes(manifest.read_bytes())
    namespace = _module(helpers, "agent-session-trace.py")
    candidate = namespace["candidate"]
    open_file = namespace["os"].open
    swapped = False

    def swapping_open(path: str | os.PathLike[str], flags: int, *args: int) -> int:
        nonlocal swapped
        if Path(path) == manifest and not swapped:
            swapped = True
            replacement.replace(manifest)
        return open_file(path, flags, *args)

    monkeypatch.setattr(namespace["os"], "open", swapping_open)

    with pytest.raises(RuntimeError, match="changed while it was being opened"):
        candidate(manifest)


def test_session_trace_rejects_transcript_growth_after_open(
    helpers: HelperInstallation,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    generation = _trace_generation(helpers)
    transcript = generation / "transcript.jsonl"
    namespace = _module(helpers, "agent-session-trace.py")
    candidate = namespace["candidate"](generation / "manifest.json")
    session = namespace["session"]
    monkeypatch.setitem(session.__globals__, "MAX_TRANSCRIPT_BYTES", transcript.stat().st_size)
    read_file = namespace["os"].read
    grew = False

    def growing_read(descriptor: int, size: int) -> bytes:
        nonlocal grew
        if not grew:
            grew = True
            with transcript.open("ab") as stream:
                stream.write(b" ")
        return read_file(descriptor, size)

    monkeypatch.setattr(namespace["os"], "read", growing_read)

    with pytest.raises(RuntimeError, match=r"session transcript (?:exceeds 8 MiB|changed while it was being read)"):
        session(candidate)


def test_session_trace_enforces_the_exact_final_output_bound(
    helpers: HelperInstallation,
    monkeypatch: pytest.MonkeyPatch,
    capfd: pytest.CaptureFixture[str],
) -> None:
    _trace_generation(helpers)

    assert _run_direct(helpers, monkeypatch, "agent-session-trace.py", [START, END]) == 0
    baseline = capfd.readouterr().out
    assert len(json.loads(baseline)) == 1

    assert (
        _run_direct(
            helpers,
            monkeypatch,
            "agent-session-trace.py",
            [START, END],
            MAX_OUTPUT_BYTES=len(baseline.encode()),
        )
        == 0
    )
    assert capfd.readouterr().out == baseline

    assert (
        _run_direct(
            helpers,
            monkeypatch,
            "agent-session-trace.py",
            [START, END],
            MAX_OUTPUT_BYTES=len(baseline.encode()) - 1,
        )
        == 1
    )
    oversized = capfd.readouterr()
    assert oversized.out == ""
    assert "output exceeds 64 MiB" in oversized.err
