from __future__ import annotations

import hashlib
import os
import re
from datetime import UTC, datetime
from pathlib import Path
from threading import Event

import pytest

from fkf.base import Base
from fkf.build import BuildReport, BuildTarget
from fkf.config import load_config
from fkf.errors import CanceledError, InvalidUsageError, OperationalError
from fkf.graph import GRAPH_FILE
from fkf.jsoncodec import dumps
from fkf.learn import (
    LearnAppliedCanceledError,
    LearnRebuildError,
    apply_learn,
    propose_learn,
    reject_learn,
    review_learn,
)
from fkf.lexical import LEXICAL_INDEX_PATH
from fkf.process import Cancellation, CommandCanceledError
from fkf.store import Layer
from fkf.validation import ValidationReport

CONFIG = """\
fkf: 1
name: learn-test
schema:
  id: {description: Stable record identity., cardinality: one}
  time: {description: Event time., cardinality: one}
  title: {description: Human-readable title., cardinality: optional}
  related: {description: Related resource., cardinality: many, relation: true}
layers: {events: false, index: false, tasks: true, projects: true, wiki: true}
sources: {}
"""


def make_base(tmp_path: Path) -> Base:
    root = tmp_path / "brain"
    root.mkdir()
    (root / "fkf.yaml").write_text(CONFIG)
    config = load_config(root)
    base = Base(config=config, store=config.store(), now=lambda: datetime(2026, 5, 10, 12, tzinfo=UTC))
    write(base, "wiki/index.md", "# Wiki\n")
    write(base, "wiki/log.md", "# Log\n\n## 2026-05-09\n\n- Existing entry.\n")
    write(
        base,
        "tasks/2026-05-09/kagglathon-abc/TASKS.md",
        "# Session\n\n## Learned\n\n- Preserve the bounded trust digest.\n",
    )
    return base


def write(base: Base, uri: str, text: str) -> Path:
    target = base.root / uri
    target.parent.mkdir(parents=True, exist_ok=True)
    target.write_text(text)
    target.chmod(0o600)
    return target


def queue_bytes(base: Base, encoded: bytes) -> str:
    proposal_id = hashlib.sha256(encoded).hexdigest()
    queue = base.root / ".agents/tmp/learn"
    queue.mkdir(mode=0o700, parents=True, exist_ok=True)
    target = queue / f"{proposal_id}.diff"
    target.write_bytes(encoded)
    target.chmod(0o600)
    return proposal_id


def queue_proposal(base: Base, diff: str) -> str:
    return queue_bytes(base, diff.encode())


def test_propose_is_deterministic_and_cites_trace_provenance(tmp_path: Path) -> None:
    base = make_base(tmp_path)

    dry = propose_learn(base, dry_run=True)
    assert dry.dry_run
    assert not dry.nothing_to_propose
    assert dry.proposal is None
    assert [(item.trace, item.text, item.target) for item in dry.candidates] == [
        (
            "tasks/2026-05-09/kagglathon-abc/TASKS.md",
            "Preserve the bounded trust digest.",
            "wiki/log.md",
        )
    ]
    assert not (base.root / ".agents/tmp/learn").exists()
    assert dumps(dry) == (
        b'{"dry_run":true,"candidates":[{"trace":"tasks/2026-05-09/kagglathon-abc/TASKS.md",'
        b'"text":"Preserve the bounded trust digest.","target":"wiki/log.md"}]}'
    )

    first = propose_learn(base)
    assert first.proposal is not None
    assert not first.existing
    assert first.proposal.files == ("wiki/log.md",)
    assert first.proposal.path == f".agents/tmp/learn/{first.proposal.id}.diff"
    assert len(first.proposal.id) == 64
    queued = base.root / first.proposal.path
    assert queued.stat().st_mode & 0o777 == 0o600

    second = propose_learn(base)
    assert second.proposal == first.proposal
    assert second.existing

    reviewed = review_learn(base, first.proposal.id, include_diff=True)
    assert len(reviewed.proposals) == 1
    proposal = reviewed.proposals[0]
    assert proposal.diff.startswith("--- a/wiki/log.md\n+++ b/wiki/log.md\n")
    assert "../tasks/2026-05-09/kagglathon-abc/TASKS.md#learned" in proposal.diff
    assert "+- Preserve the bounded trust digest." in proposal.diff

    queued.write_text("different bytes\n")
    with pytest.raises(OperationalError, match="proposal id collision"):
        propose_learn(base)


def test_propose_reports_nothing_without_creating_storage(tmp_path: Path) -> None:
    base = make_base(tmp_path)
    write(base, "tasks/2026-05-09/kagglathon-abc/TASKS.md", "# Session\n\nNo learned section.\n")

    report = propose_learn(base)

    assert report.nothing_to_propose
    assert report.candidates == ()
    assert report.proposal is None
    assert not (base.root / ".agents/tmp/learn").exists()


def test_propose_treats_an_absent_log_as_an_empty_page(tmp_path: Path) -> None:
    base = make_base(tmp_path)
    (base.root / "wiki/log.md").unlink()

    report = propose_learn(base)

    assert report.proposal is not None
    assert (
        review_learn(base, report.proposal.id, include_diff=True)
        .proposals[0]
        .diff.startswith("--- a/wiki/log.md\n+++ b/wiki/log.md\n")
    )


def test_review_is_read_only_strict_and_stably_sorted(tmp_path: Path) -> None:
    base = make_base(tmp_path)
    assert review_learn(base).proposals == ()
    assert not (base.root / ".agents/tmp/learn").exists()
    with pytest.raises(InvalidUsageError, match="requires one proposal id"):
        review_learn(base, include_diff=True)

    first = queue_proposal(
        base,
        "--- a/wiki/log.md\n+++ b/wiki/log.md\n@@ -1 +1 @@\n-# Log\n+# Updated log\n",
    )
    second = queue_proposal(
        base,
        "--- a/wiki/log.md\n+++ b/wiki/log.md\n@@ -1 +1 @@\n-# Log\n+# Another log\n",
    )
    queue = base.root / ".agents/tmp/learn"
    (queue / "README.txt").write_text("ignored")
    (queue / "directory.diff").mkdir()

    review = review_learn(base)
    assert [proposal.id for proposal in review.proposals] == sorted((first, second))
    assert all(not proposal.diff for proposal in review.proposals)

    (queue / "INVALID.diff").write_text("invalid")
    with pytest.raises(InvalidUsageError, match="active queue contains invalid filename"):
        review_learn(base)


def test_review_refuses_invalid_ids_and_symlinked_storage(tmp_path: Path) -> None:
    base = make_base(tmp_path)
    for proposal_id in ("../escape", "UPPERCASE", "a" * 65):
        with pytest.raises(InvalidUsageError, match="lowercase letters, digits, and hyphens"):
            review_learn(base, proposal_id)

    parent = base.root / ".agents/tmp"
    parent.mkdir(mode=0o700, parents=True)
    (parent / "learn").symlink_to(tmp_path / "outside", target_is_directory=True)
    with pytest.raises(InvalidUsageError, match="real directory below the base"):
        review_learn(base)


def test_reject_archives_without_changing_pages_and_is_idempotent(tmp_path: Path) -> None:
    base = make_base(tmp_path)
    before = (base.root / "wiki/log.md").read_bytes()
    proposed = propose_learn(base)
    assert proposed.proposal is not None

    rejected = reject_learn(base, proposed.proposal.id)

    assert rejected.status == "rejected"
    assert rejected.path == f".agents/tmp/learn/rejected/{proposed.proposal.id}.diff"
    assert rejected.files == ("wiki/log.md",)
    assert (base.root / rejected.path).stat().st_mode & 0o777 == 0o600
    assert (base.root / "wiki/log.md").read_bytes() == before
    assert reject_learn(base, proposed.proposal.id).status == "already-rejected"
    with pytest.raises(InvalidUsageError, match="was rejected and cannot be applied"):
        apply_learn(base, proposed.proposal.id)


def test_apply_validates_archives_rebuilds_and_repairs_idempotently(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    base = make_base(tmp_path)
    proposed = propose_learn(base)
    assert proposed.proposal is not None
    proposal_id = proposed.proposal.id
    calls: list[str] = []

    def rebuild(selected: Base, *, cancel: object = None) -> BuildReport:
        del cancel
        assert (selected.root / f".agents/tmp/learn/applied/{proposal_id}.diff").is_file()
        assert not (selected.root / f".agents/tmp/learn/{proposal_id}.diff").exists()
        calls.append("build")
        return BuildReport(nothing_stale=True)

    monkeypatch.setattr("fkf.learn.build_if_stale", rebuild)

    applied = apply_learn(base, proposed.proposal.id)

    assert applied.status == "applied"
    assert applied.files == ("wiki/log.md",)
    assert len(applied.validations) == 1
    assert applied.validations[0].ok
    assert applied.build == BuildReport(nothing_stale=True)
    assert calls == ["build"]
    log = (base.root / "wiki/log.md").read_text()
    assert "## 2026-05-10\n\n- Preserve the bounded trust digest." in log
    assert "../tasks/2026-05-09/kagglathon-abc/TASKS.md#learned" in log

    repeated = apply_learn(base, proposed.proposal.id)
    assert repeated.status == "already-applied"
    assert calls == ["build", "build"]
    assert (base.root / "wiki/log.md").read_text() == log
    with pytest.raises(InvalidUsageError, match="already applied and cannot be rejected"):
        reject_learn(base, proposed.proposal.id)


def test_apply_rebuilds_real_wiki_graph_and_lexical_caches(tmp_path: Path) -> None:
    base = make_base(tmp_path)
    proposed = propose_learn(base)
    assert proposed.proposal is not None

    report = apply_learn(base, proposed.proposal.id)

    assert report.build is not None
    assert report.build.wiki is not None
    assert report.build.graph is not None
    assert report.build.index is not None
    assert (base.root / GRAPH_FILE).is_file()
    assert (base.root / LEXICAL_INDEX_PATH).is_file()


@pytest.mark.parametrize(
    ("diff", "message"),
    [
        (b"", "diff is empty"),
        (b"\xff\n", "not valid UTF-8"),
        (b"--- a/wiki/a.md\x00\n", "without NUL"),
        (b"--- a/wiki/a.md\r\n", "LF line endings"),
        (b"--- a/wiki/a.md\n+++ b/wiki/a.md\n@@ -1 +1 @@\n-old\n+new", "must end with a newline"),
        (b"+++ b/wiki/a.md\n", "expected an old-file header"),
        (b"--- a/wiki/a.md\n+++ /dev/null\n@@ -1,1 +0,0 @@\n-old\n", "deletion is not supported"),
        (b"--- a/wiki/a.md\n+++ b/wiki/b.md\n@@ -1 +1 @@\n-old\n+new\n", "renames are not supported"),
        (b"--- /dev/null\n+++ b/wiki/nested/a.md\n@@ -0,0 +1 @@\n+x\n", "one flat"),
        (b"--- a/wiki/a.md\n+++ b/wiki/a.md\n", "has no hunks"),
        (b"--- a/wiki/a.md\n+++ b/wiki/a.md\n@@ malformed\n", "malformed hunk header"),
        (b"--- a/wiki/a.md\n+++ b/wiki/a.md\n@@ -1 +1 @@\n?bad\n", "must begin space, +, or -"),
        (b"--- a/wiki/a.md\n+++ b/wiki/a.md\n@@ -1,2 +1,2 @@\n one\n", "hunk declares"),
        (
            b"--- a/wiki/a.md\n+++ b/wiki/a.md\n@@ -1 +1 @@\n-old\n+new\n" * 2,
            "repeats target",
        ),
    ],
    ids=(
        "empty",
        "invalid-utf8",
        "nul",
        "crlf",
        "unterminated",
        "old-header",
        "deletion",
        "rename",
        "nested",
        "no-hunks",
        "malformed-hunk",
        "invalid-hunk-line",
        "short-hunk",
        "repeated-target",
    ),
)
def test_review_rejects_ambiguous_or_unsafe_diff_bytes(tmp_path: Path, diff: bytes, message: str) -> None:
    base = make_base(tmp_path)
    proposal_id = queue_bytes(base, diff)
    with pytest.raises(InvalidUsageError, match=re.escape(message)):
        review_learn(base, proposal_id, include_diff=True)


def test_apply_rolls_back_invalid_authored_and_graph_changes(tmp_path: Path) -> None:
    base = make_base(tmp_path)
    invalid_project = queue_proposal(
        base,
        "--- /dev/null\n"
        "+++ b/projects/invalid.md\n"
        "@@ -0,0 +1,6 @@\n"
        "+---\n"
        "+type: project\n"
        "+tags: [invalid]\n"
        "+---\n"
        "+\n"
        "+# Missing status\n",
    )
    with pytest.raises(InvalidUsageError, match="strict projects validation failed"):
        apply_learn(base, invalid_project)
    assert not (base.root / "projects/invalid.md").exists()
    assert review_learn(base, invalid_project, include_diff=True).proposals

    invalid_relation = queue_proposal(
        base,
        "--- /dev/null\n"
        "+++ b/wiki/broken-relation.md\n"
        "@@ -0,0 +1,11 @@\n"
        "+---\n"
        "+type: insight\n"
        "+title: Broken relation\n"
        "+tags: [test]\n"
        "+relations:\n"
        "+  related: [log.md#missing-heading]\n"
        "+---\n"
        "+\n"
        "+# Broken relation\n"
        "+\n"
        "+This relation must fail before publication.\n",
    )
    with pytest.raises(ValueError, match="fragment does not name an addressable child"):
        apply_learn(base, invalid_relation)
    assert not (base.root / "wiki/broken-relation.md").exists()
    assert review_learn(base, invalid_relation, include_diff=True).proposals


def test_apply_rejects_tampering_targets_and_symlinks(tmp_path: Path) -> None:
    base = make_base(tmp_path)
    proposed = propose_learn(base)
    assert proposed.proposal is not None
    active = base.root / proposed.proposal.path
    active.write_text("--- a/wiki/log.md\n+++ b/wiki/log.md\n@@ -1 +1 @@\n-# Log\n+# Tampered\n")
    with pytest.raises(InvalidUsageError, match="does not match its SHA-256 digest"):
        apply_learn(base, proposed.proposal.id)
    assert "Tampered" not in (base.root / "wiki/log.md").read_text()

    escape = queue_proposal(
        base,
        "--- /dev/null\n+++ b/events/2026-05-10/escape.md\n@@ -0,0 +1,1 @@\n+outside\n",
    )
    with pytest.raises(InvalidUsageError, match=r"flat wiki/\*\.md or projects/\*\.md"):
        apply_learn(base, escape)

    outside = tmp_path / "outside.md"
    outside.write_text("outside\n")
    (base.root / "wiki/linked.md").symlink_to(outside)
    linked = queue_proposal(
        base,
        "--- a/wiki/linked.md\n+++ b/wiki/linked.md\n@@ -1 +1 @@\n-old\n+new\n",
    )
    with pytest.raises(Exception, match="symlink"):
        apply_learn(base, linked)
    assert outside.read_text() == "outside\n"


def test_cache_failure_keeps_approved_edit_and_exposes_repairable_report(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    base = make_base(tmp_path)
    proposed = propose_learn(base)
    assert proposed.proposal is not None
    fail = True

    def rebuild(_base: Base, *, cancel: object = None) -> BuildReport:
        del cancel
        nonlocal fail
        if fail:
            raise OSError("graph publication failed")
        return BuildReport(nothing_stale=True)

    monkeypatch.setattr("fkf.learn.build_if_stale", rebuild)
    with pytest.raises(LearnRebuildError, match=r"run `fkf build` to repair derived caches") as caught:
        apply_learn(base, proposed.proposal.id)

    assert caught.value.report.status == "applied"
    assert caught.value.report.rebuild_error == "graph publication failed"
    approved = (base.root / "wiki/log.md").read_bytes()
    assert proposed.candidates[0].text.encode() in approved
    assert (base.root / caught.value.report.path).is_file()

    fail = False
    repaired = apply_learn(base, proposed.proposal.id)
    assert repaired.status == "already-applied"
    assert repaired.build == BuildReport(nothing_stale=True)
    assert (base.root / "wiki/log.md").read_bytes() == approved


def test_archive_failure_rolls_back_authored_bytes_and_leaves_proposal_active(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    base = make_base(tmp_path)
    proposed = propose_learn(base)
    assert proposed.proposal is not None
    before = (base.root / "wiki/log.md").read_bytes()

    def fail_sync(_source: Path, _destination: Path) -> None:
        raise OSError("forced archive sync failure")

    monkeypatch.setattr("fkf.learn._sync_move", fail_sync)
    with pytest.raises(OSError, match="sync applied proposal archive"):
        apply_learn(base, proposed.proposal.id)

    assert (base.root / "wiki/log.md").read_bytes() == before
    assert (base.root / proposed.proposal.path).is_file()
    assert not (base.root / f".agents/tmp/learn/applied/{proposed.proposal.id}.diff").exists()


def test_concurrent_editor_bytes_are_never_overwritten_or_rolled_back(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    base = make_base(tmp_path)
    first = write(base, "wiki/a.md", "# A\n")
    second = write(base, "wiki/b.md", "# B\n")
    proposal_id = queue_proposal(
        base,
        "--- a/wiki/a.md\n+++ b/wiki/a.md\n@@ -1 +1 @@\n-# A\n+# Updated A\n"
        "--- a/wiki/b.md\n+++ b/wiki/b.md\n@@ -1 +1 @@\n-# B\n+# Updated B\n",
    )
    from fkf import learn

    original_atomic_write = learn.atomic_write
    edited = False

    def interleaved_atomic_write(path: str | os.PathLike[str], data: bytes, *, mode: int = 0o600) -> None:
        nonlocal edited
        original_atomic_write(path, data, mode=mode)
        if Path(path) == first and not edited:
            edited = True
            second.write_text("# Owner edit\n")
            second.chmod(0o600)

    monkeypatch.setattr("fkf.learn.atomic_write", interleaved_atomic_write)
    with pytest.raises(InvalidUsageError, match=r"target wiki/b\.md changed"):
        apply_learn(base, proposal_id)

    assert first.read_text() == "# A\n"
    assert second.read_text() == "# Owner edit\n"
    assert review_learn(base, proposal_id, include_diff=True).proposals


def test_cancellation_is_fail_closed_before_mutation(tmp_path: Path) -> None:
    base = make_base(tmp_path)
    canceled = Event()
    canceled.set()

    with pytest.raises(CommandCanceledError):
        propose_learn(base, cancel=canceled)
    assert not (base.root / ".agents/tmp/learn").exists()

    proposed = propose_learn(base)
    assert proposed.proposal is not None
    before = (base.root / "wiki/log.md").read_bytes()
    with pytest.raises(CommandCanceledError):
        review_learn(base, proposed.proposal.id, cancel=canceled)
    with pytest.raises(CommandCanceledError):
        reject_learn(base, proposed.proposal.id, cancel=canceled)
    with pytest.raises(CommandCanceledError):
        apply_learn(base, proposed.proposal.id, cancel=canceled)
    assert (base.root / "wiki/log.md").read_bytes() == before
    assert (base.root / proposed.proposal.path).is_file()


def test_cancellation_after_archival_reports_applied_state(monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> None:
    base = make_base(tmp_path)
    proposed = propose_learn(base)
    assert proposed.proposal is not None
    canceled = Event()

    def cancel_before_rebuild(_base: Base, _target: object = None, *, cancel: object) -> BuildReport:
        assert cancel is cancel_event
        raise CanceledError("operation canceled")

    cancel_event = canceled
    monkeypatch.setattr("fkf.learn.build_if_stale", cancel_before_rebuild)
    with pytest.raises(LearnAppliedCanceledError) as caught:
        apply_learn(base, proposed.proposal.id, cancel=canceled)
    assert caught.value.exit_code == 130
    assert caught.value.report.status == "applied"
    assert caught.value.report.rebuild_error == "operation canceled"
    assert (base.root / caught.value.report.path).is_file()
    assert proposed.candidates[0].text in (base.root / "wiki/log.md").read_text()


def test_apply_forwards_one_event_through_validation_graph_and_rebuild(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    from fkf import learn

    base = make_base(tmp_path)
    proposed = propose_learn(base)
    assert proposed.proposal is not None
    cancel = Event()
    forwarded: set[str] = set()
    original_validate = learn.validate_markdown_layer
    original_extract = learn.extract_edges
    original_build = learn.build_if_stale

    def validate(
        selected: Base,
        layer: Layer,
        *,
        require_status: bool = False,
        strict: bool = False,
        cancel: Cancellation | None = None,
    ) -> ValidationReport:
        assert cancel is cancel_event
        forwarded.add("validation")
        return original_validate(
            selected,
            layer,
            require_status=require_status,
            strict=strict,
            cancel=cancel_event,
        )

    def extract(selected: Base, *, cancel: object) -> object:
        assert cancel is cancel_event
        forwarded.add("graph")
        return original_extract(selected, cancel=cancel_event)

    def build(
        selected: Base,
        target: BuildTarget | str = BuildTarget.ALL,
        *,
        cancel: Cancellation | None = None,
    ) -> BuildReport:
        assert cancel is cancel_event
        forwarded.add("build")
        return original_build(selected, target, cancel=cancel_event)

    cancel_event = cancel
    monkeypatch.setattr(learn, "validate_markdown_layer", validate)
    monkeypatch.setattr(learn, "extract_edges", extract)
    monkeypatch.setattr(learn, "build_if_stale", build)

    report = apply_learn(base, proposed.proposal.id, cancel=cancel)

    assert report.status == "applied"
    assert forwarded == {"validation", "graph", "build"}


def test_missing_and_terminal_transitions_keep_failure_categories(tmp_path: Path) -> None:
    base = make_base(tmp_path)
    for proposal_id in ("../escape", "UPPERCASE", "a" * 65):
        with pytest.raises(InvalidUsageError):
            apply_learn(base, proposal_id)
        with pytest.raises(InvalidUsageError):
            reject_learn(base, proposal_id)
    with pytest.raises(FileNotFoundError, match="learn proposal missing does not exist"):
        apply_learn(base, "missing")
    with pytest.raises(FileNotFoundError, match="learn proposal missing does not exist"):
        reject_learn(base, "missing")
