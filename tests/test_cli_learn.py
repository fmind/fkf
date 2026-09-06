"""CLI contract for reviewable learn proposals."""

from __future__ import annotations

import io
import json
from pathlib import Path
from threading import Event
from typing import NoReturn

import pytest
import typer
from typer.core import TyperGroup
from typer.main import get_command

from fkf.cli import app
from fkf.cli_learn import _action_text, _proposal_text, _review_text
from fkf.cli_support import CLIState, run_app
from fkf.errors import CanceledError
from fkf.learn import (
    LearnActionReport,
    LearnAppliedCanceledError,
    LearnCandidate,
    LearnProposal,
    LearnProposalReport,
    LearnReview,
)
from fkf.locking import WriterLock

CONFIG = """\
fkf: 1
name: learn-cli
schema:
  id: {description: Stable record identity., cardinality: one}
  time: {description: Event time., cardinality: one}
  title: {description: Human-readable title., cardinality: optional}
layers: {events: false, index: false, tasks: true, projects: true, wiki: true}
sources: {}
"""


def invoke(*arguments: str) -> tuple[int, str, str]:
    stdout, stderr = io.StringIO(), io.StringIO()
    code = run_app(app, arguments, stdout=stdout, stderr=stderr)
    return code, stdout.getvalue(), stderr.getvalue()


def make_base(tmp_path: Path, *, lesson: str = "Keep learn proposals reviewable.") -> Path:
    root = tmp_path / "brain"
    root.mkdir()
    write(root, "fkf.yaml", CONFIG)
    write(root, "wiki/index.md", "# Wiki\n")
    write(root, "wiki/log.md", "# Log\n\n## 2026-05-09\n\n- Existing entry.\n")
    write(
        root,
        "tasks/2026-05-09/fkf-session/TASKS.md",
        f"# Session\n\n## Learned\n\n- {lesson}\n",
    )
    return root


def write(root: Path, relative: str, contents: str) -> None:
    target = root / relative
    target.parent.mkdir(parents=True, exist_ok=True)
    target.write_text(contents, encoding="utf-8")
    target.chmod(0o600)


def propose(root: Path) -> str:
    code, stdout, stderr = invoke("learn", "propose", "--format", "json", "--base", str(root))
    assert code == 0, stderr
    report = json.loads(stdout)
    assert report["proposal"]["id"]
    return str(report["proposal"]["id"])


def test_learn_help_publishes_the_complete_command_contract_and_aliases(tmp_path: Path) -> None:
    root_command = get_command(app)
    assert isinstance(root_command, TyperGroup)
    assert "learn" in root_command.commands
    learn_command = root_command.commands["learn"]
    assert isinstance(learn_command, TyperGroup)
    assert set(learn_command.commands) == {"propose", "review", "apply", "reject"}

    code, stdout, stderr = invoke("learn", "--help")
    assert code == 0
    assert stderr == ""
    for value in ("propose", "review", "apply", "reject", "reviewable knowledge diffs"):
        assert value in stdout

    for command, flags in (
        ("propose", ("--dry-run",)),
        ("review", ("--diff", "[proposal]")),
        ("apply", ("proposal",)),
        ("reject", ("proposal",)),
    ):
        code, stdout, stderr = invoke("learn", command, "--help")
        assert code == 0
        assert stderr == ""
        for flag in flags:
            assert flag in stdout

    root = make_base(tmp_path)
    code, stdout, stderr = invoke("learn", "p", "--dry-run", "--format", "json", "--base", str(root))
    assert code == 0
    assert stderr == ""
    assert json.loads(stdout)["dry_run"] is True


def test_learn_usage_fails_before_opening_a_base() -> None:
    for arguments, message in (
        (("learn",), "name a subcommand"),
        (("learn", "review", "--diff"), "usage: fkf learn review <proposal> --diff"),
        (("learn", "review", "one", "two"), "unexpected extra argument"),
        (("learn", "apply"), "missing argument"),
        (("learn", "reject", "one", "two"), "unexpected extra argument"),
        (("learn", "propose", "unexpected"), "unexpected extra argument"),
    ):
        code, stdout, stderr = invoke(*arguments)
        assert code == 2
        assert message.casefold() in (stdout + stderr).casefold()
        assert "no fkf base" not in stderr


def test_learn_text_renderers_match_the_go_contract() -> None:
    candidate = LearnCandidate("tasks/2026-05-09/session/TASKS.md", "Keep the review boundary.", "wiki/log.md")
    proposal = LearnProposal("abc123", ".agents/tmp/learn/abc123.diff", 42, ("wiki/log.md",))

    assert _proposal_text(LearnProposalReport(False, (), nothing_to_propose=True)) == "nothing to propose"
    assert _proposal_text(LearnProposalReport(True, (candidate,))) == (
        "- Keep the review boundary. · tasks/2026-05-09/session/TASKS.md#learned -> wiki/log.md\n"
        "\n1 candidate log bullet(s); nothing written"
    )
    assert _proposal_text(LearnProposalReport(False, (candidate,), proposal)) == (
        "staged abc123 · 42 bytes · .agents/tmp/learn/abc123.diff\nreview: fkf learn review abc123 --diff"
    )
    assert _proposal_text(LearnProposalReport(False, (candidate,), proposal, existing=True)).startswith(
        "already staged abc123"
    )

    assert _review_text(LearnReview(())) == "no active learn proposals"
    assert _review_text(LearnReview((proposal,))) == ("abc123 · 42 bytes · .agents/tmp/learn/abc123.diff · wiki/log.md")
    exact_diff = "--- a/wiki/log.md\n+++ b/wiki/log.md\n"
    assert (
        _review_text(LearnReview((LearnProposal("abc123", proposal.path, 42, proposal.files, exact_diff),)))
        == exact_diff
    )

    action = LearnActionReport(
        "abc123",
        "applied",
        ".agents/tmp/learn/applied/abc123.diff",
        ("wiki/log.md",),
        rebuild_error="graph failed",
    )
    assert _action_text(action) == (
        "applied abc123 · .agents/tmp/learn/applied/abc123.diff\n"
        "cache rebuild failed: graph failed\n"
        "repair: fkf build\n"
        "files: wiki/log.md"
    )


def test_learn_dry_run_stages_reviews_and_rejects_in_all_formats(tmp_path: Path) -> None:
    root = make_base(tmp_path, lesson="Preserve an exact review boundary.")

    code, stdout, stderr = invoke("learn", "propose", "--dry-run", "--format", "text", "--base", str(root))
    assert code == 0
    assert stderr == ""
    assert "Preserve an exact review boundary." in stdout
    assert "nothing written" in stdout
    assert not (root / ".agents/tmp/learn").exists()

    proposal_id = propose(root)
    code, stdout, stderr = invoke("learn", "review", "--format", "jsonl", "--base", str(root))
    assert code == 0
    assert stderr == ""
    assert stdout.count("\n") == 1
    assert json.loads(stdout)["proposals"][0]["id"] == proposal_id

    code, stdout, stderr = invoke("learn", "v", proposal_id, "--diff", "--format", "text", "--base", str(root))
    assert code == 0
    assert stderr == ""
    assert "+++ b/wiki/log.md" in stdout
    assert "../tasks/2026-05-09/fkf-session/TASKS.md#learned" in stdout

    code, stdout, stderr = invoke("learn", "r", proposal_id, "--format", "text", "--base", str(root))
    assert code == 0
    assert stderr == ""
    assert f"rejected {proposal_id}" in stdout
    assert "files: wiki/log.md" in stdout

    code, stdout, stderr = invoke("learn", "reject", proposal_id, "--format", "text", "--base", str(root))
    assert code == 0
    assert stderr == ""
    assert f"already-rejected {proposal_id}" in stdout

    code, stdout, stderr = invoke("learn", "review", "--format", "text", "--base", str(root))
    assert code == 0
    assert stdout == "no active learn proposals\n"
    assert stderr == ""


def test_learn_apply_emits_validation_and_rebuild_report(tmp_path: Path) -> None:
    root = make_base(tmp_path)
    proposal_id = propose(root)

    code, stdout, stderr = invoke("learn", "a", proposal_id, "--format", "json", "--base", str(root))

    assert code == 0
    assert stderr == ""
    report = json.loads(stdout)
    assert report["id"] == proposal_id
    assert report["status"] == "applied"
    assert report["files"] == ["wiki/log.md"]
    assert report["validations"]
    assert report["build"]["index"] is not None
    assert "Keep learn proposals reviewable." in (root / "wiki/log.md").read_text(encoding="utf-8")


def test_learn_writers_lock_the_physical_base_but_dry_run_and_review_do_not(tmp_path: Path) -> None:
    root = make_base(tmp_path)
    proposal_id = propose(root)
    alias = tmp_path / "brain-alias"
    alias.symlink_to(root, target_is_directory=True)

    with WriterLock.acquire(root):
        for arguments in (
            ("learn", "propose", "--dry-run"),
            ("learn", "review"),
            ("learn", "review", proposal_id, "--diff"),
        ):
            code, _stdout, stderr = invoke(*arguments, "--base", str(alias))
            assert code == 0, stderr

        for arguments in (
            ("learn", "propose"),
            ("learn", "apply", proposal_id),
            ("learn", "reject", proposal_id),
        ):
            code, stdout, stderr = invoke(*arguments, "--base", str(alias))
            assert code == 1
            assert stdout == ""
            assert "active writer" in stderr


@pytest.mark.parametrize(
    ("service_name", "arguments"),
    [
        ("propose_learn", ("learn", "propose", "--dry-run")),
        ("propose_learn", ("learn", "propose")),
        ("review_learn", ("learn", "review")),
        ("apply_learn", ("learn", "apply", "abc123")),
        ("reject_learn", ("learn", "reject", "abc123")),
    ],
)
def test_learn_services_receive_the_invocation_event_and_cancel_with_exit_130(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
    service_name: str,
    arguments: tuple[str, ...],
) -> None:
    from fkf import cli_learn

    root = make_base(tmp_path)
    invocation_events: list[Event] = []
    original_state = cli_learn.state

    def capture_state(ctx: typer.Context) -> CLIState:
        invocation = original_state(ctx)
        invocation_events.append(invocation.cancel)
        return invocation

    def cancel_service(*_args: object, **kwargs: object) -> NoReturn:
        received = kwargs.get("cancel")
        assert invocation_events
        assert received is invocation_events[-1]
        invocation_events[-1].set()
        raise CanceledError("operation canceled")

    monkeypatch.setattr(cli_learn, "state", capture_state)
    monkeypatch.setattr(cli_learn, service_name, cancel_service)

    code, stdout, stderr = invoke(*arguments, "--base", str(root))

    assert code == 130
    assert stdout == ""
    assert stderr == "fkf: operation canceled\n"


def test_learn_apply_emits_durable_report_before_rebuild_failure(tmp_path: Path) -> None:
    root = make_base(tmp_path, lesson="Keep approved knowledge after cache failure.")
    proposal_id = propose(root)
    (root / "graph.tsv").mkdir()

    code, stdout, stderr = invoke("learn", "apply", proposal_id, "--format", "json", "--base", str(root))

    assert code == 1
    report = json.loads(stdout)
    assert report["status"] == "applied"
    assert report["rebuild_error"]
    assert "run `fkf build` to repair derived caches" in stderr
    assert "Keep approved knowledge after cache failure." in (root / "wiki/log.md").read_text(encoding="utf-8")

    code, stdout, stderr = invoke("learn", "apply", proposal_id, "--format", "text", "--base", str(root))
    assert code == 1
    assert stdout.startswith(f"already-applied {proposal_id}")
    assert "cache rebuild failed:" in stdout
    assert "repair: fkf build" in stdout
    assert "run `fkf build` to repair derived caches" in stderr


def test_learn_apply_emits_durable_report_before_post_approval_cancellation(
    monkeypatch: pytest.MonkeyPatch,
    tmp_path: Path,
) -> None:
    root = make_base(tmp_path)
    report = LearnActionReport(
        "abc123",
        "applied",
        ".agents/tmp/learn/applied/abc123.diff",
        ("wiki/log.md",),
        rebuild_error="command canceled",
    )

    def canceled(_base: object, _proposal_id: str, *, cancel: object | None = None) -> LearnActionReport:
        del cancel
        raise LearnAppliedCanceledError(report)

    monkeypatch.setattr("fkf.cli_learn.apply_learn", canceled)

    code, stdout, stderr = invoke("learn", "apply", "abc123", "--format", "jsonl", "--base", str(root))

    assert code == 130
    assert stdout.count("\n") == 1
    assert json.loads(stdout)["status"] == "applied"
    assert "command canceled" in json.loads(stdout)["rebuild_error"]
    assert "is applied" in stderr
