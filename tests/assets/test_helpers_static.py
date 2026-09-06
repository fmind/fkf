"""Interpreter and probe contracts shared by every bundled helper."""

from __future__ import annotations

import os
import shutil
import subprocess
import sys
from pathlib import Path

import pytest

from fkf.assets import HOOK_SCRIPT, shipped_helpers

from .conftest import HelperInstallation

VERSIONED_HELPERS = frozenset(
    {
        "agent-memory-body.py",
        "agent-prompt-body.py",
        "agent-prompts.py",
        "agent-session-trace.py",
        "chrome-bookmarks.py",
        "gh-runs.py",
        "github-commits-json.py",
        "github-events-json.py",
        "github-generic-list-json.py",
        "github-gists-json.sh",
        "github-search-json.py",
        "github-stars-json.sh",
        "gmail-body.py",
        "gws-calendar-body.py",
        "gws-calendars-json.py",
        "gws-chat-message-body.py",
        "gws-chat-messages.py",
        "gws-doc-text.sh",
        "gws-meeting-notes-json.py",
        "huggingface-repositories-json.py",
        "jira-issues-json.py",
        "kaggle-competitions-json.sh",
        "kaggle-datasets-json.sh",
        "kaggle-json.py",
        "kaggle-kernels-json.py",
        "kaggle-models-json.py",
        "mise-tools-json.sh",
        "repository-facts.py",
        "rss-json.py",
        "writing-index.py",
        "writing-source-json.sh",
    }
)


def test_every_helper_parses_with_its_declared_interpreter(tmp_path: Path) -> None:
    helpers = shipped_helpers()
    for name, content in helpers.items():
        if name.endswith(".sh"):
            completed = subprocess.run(
                ["/bin/sh", "-n"],
                check=False,
                input=content,
                capture_output=True,
            )
            assert completed.returncode == 0, f"{name}: {completed.stderr.decode(errors='replace')}"
            continue
        if name.endswith(".py"):
            compile(content, name, "exec")
            continue
        assert name.endswith(".jq")
        jq = shutil.which("jq")
        assert jq is not None
        combined = tmp_path / "combined.jq"
        combined.write_bytes(content + b'\n"probe" | normalize_agent_prompt\n')
        completed = subprocess.run(  # noqa: S603 - static syntax check of package-owned bytes.
            [jq, "-n", "-f", os.fspath(combined)],
            check=False,
            capture_output=True,
        )
        assert completed.returncode == 0, f"{name}: {completed.stderr.decode(errors='replace')}"


def test_shell_helpers_enable_fail_fast_mode() -> None:
    for name, content in shipped_helpers().items():
        if not name.endswith(".sh"):
            continue
        expected = b"set -u" if name == HOOK_SCRIPT else b"set -eu"
        assert expected in content, f"{name} does not enable {expected.decode()}"


def test_reviewed_probe_vocabulary_is_explicit() -> None:
    helpers = shipped_helpers()
    assert {name for name, content in helpers.items() if b"--version" in content} == VERSIONED_HELPERS


@pytest.mark.parametrize("name", sorted(VERSIONED_HELPERS))
def test_version_probe_never_executes_a_provider(helpers: HelperInstallation, name: str) -> None:
    target = helpers.bin / name
    command = [sys.executable, "-I", os.fspath(target)] if name.endswith(".py") else ["/bin/sh", os.fspath(target)]
    completed = subprocess.run(  # noqa: S603 - package-owned helper under its explicit interpreter.
        [*command, "--version"],
        check=False,
        capture_output=True,
        env={"HOME": os.fspath(helpers.home), "LC_ALL": "C.UTF-8", "PATH": "/nonexistent"},
        timeout=5,
    )
    assert completed.returncode == 0, completed.stderr.decode(errors="replace")
    assert completed.stdout.strip()
    assert name.rsplit(".", maxsplit=1)[0].encode() in completed.stdout
