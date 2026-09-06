"""Durable agent-session traces select one complete, bounded generation."""

from __future__ import annotations

import json
import os
import runpy
import time
from pathlib import Path
from types import FunctionType
from typing import cast

import pytest

from .conftest import HelperInstallation


def _write_generation(
    root: Path,
    name: str,
    *,
    ingested_at: str,
    high_water_mark: str,
    transcript: list[dict[str, object]],
) -> Path:
    generation = root / name
    generation.mkdir(parents=True)
    manifest = {
        "schema_version": 1,
        "parser_version": "1",
        "agent": "codex",
        "session_id": "session-1",
        "completeness": "complete",
        "ingested_at": ingested_at,
        "high_water_mark": high_water_mark,
        "record_count": len(transcript),
    }
    (generation / "manifest.json").write_text(json.dumps(manifest) + "\n", encoding="utf-8")
    (generation / "transcript.jsonl").write_text(
        "".join(json.dumps(record, separators=(",", ":")) + "\n" for record in transcript),
        encoding="utf-8",
    )
    return generation


def test_session_trace_selects_latest_complete_generation_and_path_evidence(
    helpers: HelperInstallation,
) -> None:
    store = helpers.home / ".agents" / "sessions" / "v1" / "codex" / "lineage"
    worktree = helpers.home / "work" / "fkf"
    worktree.mkdir(parents=True)
    _write_generation(
        store,
        "generation-a",
        ingested_at="2026-05-04T09:00:00Z",
        high_water_mark="2026-05-04T08:30:00Z",
        transcript=[
            {
                "ts": "2026-05-04T08:00:00Z",
                "agent": "codex",
                "sid": "session-1",
                "role": "user",
                "content": "Superseded request.",
            }
        ],
    )
    _write_generation(
        store,
        "generation-b",
        ingested_at="2026-05-04T10:00:00Z",
        high_water_mark="2026-05-04T09:30:00Z",
        transcript=[
            {
                "ts": "2026-05-04T08:00:00Z",
                "agent": "codex",
                "sid": "session-1",
                "role": "user",
                "content": "<system-reminder>private harness chrome</system-reminder>Implement session traces.",
                "cwd": os.fspath(worktree),
                "model": "gpt-test",
            },
            {
                "ts": "2026-05-04T08:01:00Z",
                "agent": "codex",
                "sid": "session-1",
                "role": "user",
                "content": "# AGENTS.md instructions for /untrusted",
                "cwd": os.fspath(worktree),
                "model": "gpt-test",
            },
            {
                "ts": "2026-05-04T09:00:00Z",
                "agent": "codex",
                "sid": "session-1",
                "role": "assistant",
                "content": "Implemented it.\ngo test ./services -run SessionTrace\nrm -rf /must-not-be-classified",
                "cwd": os.fspath(worktree),
                "model": "gpt-test",
            },
        ],
    )
    helpers.fake(
        "git",
        """case " $* " in
  *" rev-parse --is-inside-work-tree "*) printf '%s\n' true ;;
  *" rev-parse --verify HEAD "*) printf '%s\n' deadbeef ;;
  *" hash-object --stdin "*) printf '%s\n' 0123456789abcdef0123456789abcdef01234567 ;;
  *" status --short --untracked-files=normal "*) printf '%s\n' ' M services/session_trace.go' '?? services/session_trace_test.go' ;;
  *" config --get remote.origin.url "*) printf '%s\n' 'git@github.com:fmind/fkf.git' ;;
  *" log --since="*) printf '%s\n' 'services/committed.go' 'services/session_trace.go' ;;
  *) exit 2 ;;
esac
""",
    )
    result = helpers.run(
        "agent-session-trace.py",
        "2026-05-04T00:00:00Z",
        "2026-05-05T00:00:00Z",
    )
    assert result.returncode == 0, result.stderr.decode(errors="replace")
    traces = cast(list[dict[str, object]], json.loads(result.stdout))
    assert len(traces) == 1
    trace = traces[0]
    assert trace["id"] == "codex:session-1"
    assert trace["harness"] == "codex"
    assert trace["repo"] == "fmind/fkf"
    assert trace["repository_uri"] == "repo:github.com/fmind/fkf"
    assert trace["time"] == "2026-05-04T09:30:00Z"
    assert trace["title"] == "Implement session traces."
    assert trace["requests"] == ["Implement session traces."]
    assert trace["files"] == [
        " M services/session_trace.go",
        "?? services/session_trace_test.go",
        "services/committed.go",
        "services/session_trace.go",
    ]
    assert trace["verification"] == ["go test ./services -run SessionTrace"]
    assert "rm -rf /must-not-be-classified" in cast(str, trace["last_assistant"])

    excluded = helpers.run(
        "agent-session-trace.py",
        "2026-05-05T00:00:00Z",
        "2026-05-06T00:00:00Z",
    )
    assert excluded.returncode == 0, excluded.stderr.decode(errors="replace")
    assert json.loads(excluded.stdout) == []


def test_session_trace_rejects_oversized_transcripts(helpers: HelperInstallation) -> None:
    store = helpers.home / ".agents" / "sessions" / "v1" / "codex" / "lineage"
    generation = _write_generation(
        store,
        "generation",
        ingested_at="2026-05-04T10:00:00Z",
        high_water_mark="2026-05-04T09:30:00Z",
        transcript=[
            {
                "ts": "2026-05-04T09:00:00Z",
                "agent": "codex",
                "sid": "session-1",
                "role": "user",
                "content": "Bound the input.",
            }
        ],
    )
    with (generation / "transcript.jsonl").open("wb") as stream:
        stream.truncate((8 << 20) + 1)
    result = helpers.run(
        "agent-session-trace.py",
        "2026-05-04T00:00:00Z",
        "2026-05-05T00:00:00Z",
    )
    assert result.returncode != 0
    assert result.stdout == b""
    assert b"session transcript exceeds 8 MiB" in result.stderr


def test_session_trace_refuses_symlinks_anywhere_in_the_store(helpers: HelperInstallation) -> None:
    store = helpers.home / ".agents" / "sessions" / "v1"
    store.mkdir(parents=True)
    outside = helpers.root / "outside"
    outside.mkdir()
    (store / "linked-harness").symlink_to(outside, target_is_directory=True)
    result = helpers.run(
        "agent-session-trace.py",
        "2026-05-04T00:00:00Z",
        "2026-05-05T00:00:00Z",
    )
    assert result.returncode != 0
    assert result.stdout == b""
    assert b"session store contains a symlink" in result.stderr


def test_session_trace_terminates_git_at_the_output_bound_without_partial_output(
    helpers: HelperInstallation,
    monkeypatch: pytest.MonkeyPatch,
    capsys: pytest.CaptureFixture[str],
) -> None:
    store = helpers.home / ".agents" / "sessions" / "v1" / "codex" / "lineage"
    worktree = helpers.home / "work"
    worktree.mkdir()
    _write_generation(
        store,
        "generation",
        ingested_at="2026-05-04T10:00:00Z",
        high_water_mark="2026-05-04T09:30:00Z",
        transcript=[
            {
                "ts": "2026-05-04T09:00:00Z",
                "agent": "codex",
                "sid": "session-1",
                "role": "user",
                "content": "Bound git evidence.",
                "cwd": os.fspath(worktree),
            }
        ],
    )
    escaped = helpers.root / "git-escaped"
    fake_git = helpers.bin / "git"
    fake_git.write_text(
        "#!/usr/bin/env python3\n"
        "import os,pathlib,sys,time\n"
        "arguments=' '.join(sys.argv[1:])\n"
        "if 'rev-parse --is-inside-work-tree' in arguments:\n"
        " print('true')\n"
        "elif 'status --short' in arguments:\n"
        " sys.stdout.write('123456789'); sys.stdout.flush()\n"
        " sys.stderr.write('untrusted provider diagnostic'); sys.stderr.flush()\n"
        " time.sleep(0.6)\n"
        " pathlib.Path(os.environ['GIT_ESCAPE_MARKER']).write_text('escaped')\n"
        "else:\n"
        " raise SystemExit(2)\n",
        encoding="utf-8",
    )
    fake_git.chmod(0o700)
    namespace = runpy.run_path(os.fspath(helpers.bin / "agent-session-trace.py"))
    main = namespace["main"]
    assert isinstance(main, FunctionType)
    monkeypatch.setitem(main.__globals__, "MAX_GIT_BYTES", 8)
    monkeypatch.setenv("HOME", os.fspath(helpers.home))
    monkeypatch.setenv("PATH", helpers.environment()["PATH"])
    monkeypatch.setenv("GIT_ESCAPE_MARKER", os.fspath(escaped))

    assert main(["2026-05-04T00:00:00Z", "2026-05-05T00:00:00Z"]) == 1
    captured = capsys.readouterr()

    assert captured.out == ""
    assert "exceeds" in captured.err
    assert "untrusted provider diagnostic" not in captured.err
    time.sleep(0.7)
    assert not escaped.exists()


@pytest.mark.parametrize("not_before", ["2026-05-01", "not-a-time"])
def test_session_trace_rejects_non_rfc3339_not_before(
    helpers: HelperInstallation,
    not_before: str,
) -> None:
    result = helpers.run(
        "agent-session-trace.py",
        "2026-05-01T00:00:00Z",
        "2026-05-02T00:00:00Z",
        not_before,
    )
    assert result.returncode != 0
    assert result.stdout == b""
    assert b"must be RFC3339 instants" in result.stderr


def test_session_trace_rescans_manifest_stream_for_bounded_latest_selection(
    helpers: HelperInstallation,
    monkeypatch: pytest.MonkeyPatch,
    capsys: pytest.CaptureFixture[str],
) -> None:
    store = helpers.home / ".agents" / "sessions" / "v1"
    store.mkdir(parents=True)
    namespace = runpy.run_path(os.fspath(helpers.bin / "agent-session-trace.py"))
    main = namespace["main"]
    candidate_type = namespace["Candidate"]
    assert isinstance(main, FunctionType)
    candidate = candidate_type(
        "codex",
        "session-1",
        "2026-05-04T10:00:00Z",
        "2026-05-04T09:30:00Z",
        1.0,
        namespace["instant"]("2026-05-04T10:00:00Z"),
        namespace["instant"]("2026-05-04T09:30:00Z"),
        store / "codex" / "lineage" / "generation",
    )
    calls = 0

    def manifest_stream(_store: Path):
        nonlocal calls
        calls += 1
        yield candidate

    monkeypatch.setitem(main.__globals__, "manifests", manifest_stream)
    monkeypatch.setitem(main.__globals__, "trace", lambda _candidate, _cache: None)
    monkeypatch.setenv("HOME", os.fspath(helpers.home))

    assert main(["2026-05-04T00:00:00Z", "2026-05-05T00:00:00Z"]) == 0
    assert capsys.readouterr().out == "[]\n"
    assert calls == 2


def test_session_trace_manifest_scan_is_lazy_and_unsorted(helpers: HelperInstallation) -> None:
    namespace = runpy.run_path(os.fspath(helpers.bin / "agent-session-trace.py"))
    manifests = namespace["manifests"]
    assert isinstance(manifests, FunctionType)
    stream = manifests(helpers.home / ".agents" / "sessions" / "v1")

    assert iter(stream) is stream
    source = (helpers.bin / "agent-session-trace.py").read_text(encoding="utf-8")
    assert "sorted(store.rglob" not in source
