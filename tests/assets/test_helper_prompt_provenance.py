"""Durable prompt IDs resolve only through their captured archive provenance."""

from __future__ import annotations

import json
from pathlib import Path

import pytest

from .conftest import HelperInstallation, prompt_body_arguments
from .test_helper_agent_bounds import _normalized_transcript, _prompt_line, _run_direct


@pytest.mark.parametrize(
    ("field", "value"), [("session", "../escape"), ("lineage", "wrong"), ("turn", True), ("turn", 0)]
)
def test_prompt_body_rejects_invalid_stored_provenance(helpers: HelperInstallation, field: str, value: object) -> None:
    arguments = prompt_body_arguments(helpers)
    path = Path(arguments[0]) / "events/2026-05-04/agent-prompts.json"
    document = json.loads(path.read_text())
    document["records"][0][field] = value
    path.write_text(json.dumps(document))
    result = helpers.run("agent-prompt-body.py", *arguments)
    assert result.returncode == 1
    assert result.stdout == b""
    assert b"invalid archive provenance" in result.stderr


def test_prompt_body_rejects_conflicting_cross_date_provenance(helpers: HelperInstallation) -> None:
    arguments = prompt_body_arguments(helpers)
    path = Path(arguments[0]) / "events/2026-05-04/agent-prompts.json"
    document = json.loads(path.read_text())
    document["date"] = "2026-05-03"
    document["records"][0]["turn"] = 2
    other = path.parents[1] / "2026-05-03/agent-prompts.json"
    other.parent.mkdir()
    other.write_text(json.dumps(document))
    result = helpers.run("agent-prompt-body.py", *arguments)
    assert result.returncode == 1
    assert result.stdout == b""
    assert b"conflicting archive provenance" in result.stderr


def test_prompt_body_ignores_other_generations_and_validates_the_recorded_turn(helpers: HelperInstallation) -> None:
    transcript = _normalized_transcript(helpers)
    transcript.write_bytes(_prompt_line())
    other = transcript.parents[1] / ("b" * 64) / "transcript.jsonl"
    other.parent.mkdir()
    other.write_bytes(_prompt_line(content="Different body at the same instant."))
    arguments = prompt_body_arguments(helpers)
    result = helpers.run("agent-prompt-body.py", *arguments)
    assert result.returncode == 0, result.stderr
    assert result.stdout == b"Bound the prompt."
    transcript.write_bytes(_prompt_line().replace(b'"role":"user"', b'"role":"assistant"'))
    result = helpers.run("agent-prompt-body.py", *arguments)
    assert result.returncode == 1
    assert result.stdout == b""
    assert b"no turn" in result.stderr


def test_prompt_body_bounds_evidence_and_transcript_bytes(
    helpers: HelperInstallation, monkeypatch: pytest.MonkeyPatch, capfd: pytest.CaptureFixture[str]
) -> None:
    transcript = _normalized_transcript(helpers)
    transcript.write_bytes(_prompt_line())
    arguments = prompt_body_arguments(helpers)
    assert _run_direct(helpers, monkeypatch, "agent-prompt-body.py", arguments, MAX_DOCUMENT_BYTES=1) == 1
    assert "stored source document exceeds" in capfd.readouterr().err
    assert _run_direct(helpers, monkeypatch, "agent-prompt-body.py", arguments, MAX_TRANSCRIPT_BYTES=1) == 1
    captured = capfd.readouterr()
    assert captured.out == ""
    assert "transcript exceeds" in captured.err


def test_prompt_body_rejects_linked_evidence(helpers: HelperInstallation) -> None:
    arguments = prompt_body_arguments(helpers)
    path = Path(arguments[0]) / "events/2026-05-04/agent-prompts.json"
    external = helpers.root / "external.json"
    path.rename(external)
    path.symlink_to(external)
    result = helpers.run("agent-prompt-body.py", *arguments)
    assert result.returncode == 1
    assert result.stdout == b""
    assert b"linked" in result.stderr
