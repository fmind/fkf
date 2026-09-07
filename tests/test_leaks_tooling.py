from __future__ import annotations

import tomllib
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]


def test_default_leak_gate_scans_recent_history_and_working_tree() -> None:
    task = tomllib.loads((ROOT / "mise.toml").read_text(encoding="utf-8"))["tasks"]["check:leaks"]
    command = task["run"]

    history = 'gitleaks git --redact=100 --log-opts="--max-count=100" --verbose'
    working_tree = "gitleaks dir . --redact=100 --verbose"

    assert "recent commits and the working tree" in task["description"]
    assert command.count(history) == 1
    assert command.count(working_tree) == 2
    assert command.index(history) < command.index(working_tree)


def test_leak_gate_excludes_only_the_generated_sast_rule_cache() -> None:
    config = tomllib.loads((ROOT / ".gitleaks.toml").read_text(encoding="utf-8"))

    assert config["extend"] == {"useDefault": True}
    assert config["allowlists"] == [
        {
            "description": "Ignore the generated third-party SAST rule cache",
            "paths": [r"^\.opengrep/"],
        }
    ]


def test_explicit_leak_scope_remains_staged_only() -> None:
    task = tomllib.loads((ROOT / "mise.toml").read_text(encoding="utf-8"))["tasks"]["check:leaks"]

    assert 'gitleaks git --redact=100 --verbose "$@"' in task["run"]
    assert "mise run check:leaks --staged" in (ROOT / "lefthook.yml").read_text(encoding="utf-8")
