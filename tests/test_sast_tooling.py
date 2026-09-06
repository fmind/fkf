from __future__ import annotations

import os
import re
import stat
import subprocess
import tomllib
from pathlib import Path

import yaml

ROOT = Path(__file__).resolve().parents[1]
INSTALLER = ROOT / "scripts/install-sast.sh"
PIN = "f1d2b562b414783763fd02a6ed2736eaed622efa"


def _run(*argv: str, home: Path, check: bool = True) -> subprocess.CompletedProcess[str]:
    environment = {"HOME": str(home), "PATH": os.environ["PATH"]}
    return subprocess.run(  # noqa: S603 - test argv is constructed only from temporary paths.
        argv,
        check=check,
        capture_output=True,
        text=True,
        env=environment,
    )


def test_sast_gate_is_pinned_and_part_of_check() -> None:
    config = tomllib.loads((ROOT / "mise.toml").read_text(encoding="utf-8"))
    tasks = config["tasks"]

    assert config["tools"]["opengrep"] == "1.29.0"
    assert "check:sast" in tasks["check"]["depends"]
    assert tasks["check:sast"]["depends"] == ["install:sast"]
    assert ".opengrep/rules/python/lang/security" in tasks["check:sast"]["run"]
    assert ".opengrep/rules/yaml/github-actions/security" in tasks["check:sast"]["run"]
    assert PIN in tasks["install:sast"]["run"]
    assert re.fullmatch(r"[0-9a-f]{40}", PIN)
    assert "/.opengrep/" in (ROOT / ".gitignore").read_text(encoding="utf-8").splitlines()
    assert INSTALLER.stat().st_mode & stat.S_IXUSR


def test_sast_rule_fixture_cache_is_not_a_repository_scan_target() -> None:
    trivy = yaml.safe_load((ROOT / "trivy.yaml").read_text(encoding="utf-8"))

    assert trivy["scan"]["skip-dirs"] == [".opengrep"]


def test_sast_rule_installer_is_idempotent_and_refuses_local_changes(tmp_path: Path) -> None:
    home = tmp_path / "home"
    upstream = tmp_path / "upstream"
    upstream.mkdir()
    _run("git", "init", "-q", str(upstream), home=home)
    (upstream / "rule.yaml").write_text("rules: []\n", encoding="utf-8")
    _run("git", "-C", str(upstream), "add", "rule.yaml", home=home)
    _run(
        "git",
        "-C",
        str(upstream),
        "-c",
        "user.name=FKF Test",
        "-c",
        "user.email=fkf@example.test",
        "commit",
        "-qm",
        "test rules",
        home=home,
    )
    revision = _run("git", "-C", str(upstream), "rev-parse", "HEAD", home=home).stdout.strip()
    checkout = tmp_path / "rules"

    for _ in range(2):
        _run("bash", str(INSTALLER), revision, str(checkout), str(upstream), home=home)
        assert _run("git", "-C", str(checkout), "rev-parse", "HEAD", home=home).stdout.strip() == revision

    (checkout / "local-change").write_text("preserve\n", encoding="utf-8")
    refused = _run(
        "bash",
        str(INSTALLER),
        revision,
        str(checkout),
        str(upstream),
        home=home,
        check=False,
    )
    assert refused.returncode == 1
    assert "local changes" in refused.stderr
    assert (checkout / "local-change").read_text(encoding="utf-8") == "preserve\n"
