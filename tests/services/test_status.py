from __future__ import annotations

import shutil
import subprocess
from collections import Counter
from dataclasses import replace
from datetime import UTC, datetime
from pathlib import Path
from threading import Event

import pytest

from fkf.base import Base
from fkf.config import load_config
from fkf.documents import Document, Record, day_window, fields_of, parse_day_in_location, schema_of
from fkf.errors import CanceledError
from fkf.graph import DerivedGraphMissingError, EdgeValidationError
from fkf.lexical import LexicalIndexUse
from fkf.markdown import Severity
from fkf.process import Command, CommandResult
from fkf.source_runtime import Environment
from fkf.status import (
    Finding,
    HarnessRegistration,
    LayerOverview,
    RequirementStatus,
    SourceStatus,
    Status,
    StatusRequest,
    report,
)
from fkf.store import Layer, UnsafePathError
from fkf.trust import TrustState

CONFIG = """\
fkf: 1
name: brain
schema:
  id: {description: Stable identity., cardinality: one}
  time: {description: Event time., cardinality: one}
  title: {description: Meaningful title., cardinality: optional}
layers: {events: true, index: true, tasks: true, projects: true, wiki: true}
sync: {index_max_age_hours: 24}
sources:
  daily:
    enabled: true
    auth: [provider, auth]
    requires: [definitely-missing]
    test: [missing-check]
    run: [provider, daily, "{{date}}"]
    fields: {id: .id, time: .time, title: .title}
  snapshot:
    enabled: true
    layer: index
    max_age_hours: 2
    run: [provider, snapshot]
    fields: {id: .id, title: .title}
"""


def _base(tmp_path: Path, *, now: datetime | None = None) -> Base:
    root = tmp_path / "brain"
    root.mkdir()
    (root / "fkf.yaml").write_text(CONFIG, encoding="utf-8")
    config = load_config(root)
    return Base(
        config=config,
        store=config.store(),
        environment=Environment.from_config(config, inherited_path="/usr/bin:/bin"),
        now=lambda: now or datetime(2026, 9, 6, 12, tzinfo=UTC),
        origin="flag",
    )


def _write_event(base: Base, day: str, count: int, *, source: str = "daily") -> None:
    window = day_window(parse_day_in_location(day, UTC))
    declared = base.config.sources[source]
    records: list[Record] = [
        {"id": f"{day}-{index}", "time": f"{day}T12:00:00Z", "title": f"Title {index}"} for index in range(count)
    ]
    base.write_document(
        Document(
            source=source,
            layer=Layer.EVENTS,
            date=day,
            window_start=window.start,
            window_end=window.end,
            collected_at=window.end,
            schema=schema_of(declared),
            fields=fields_of(declared),
            count=len(records),
            records=records,
        )
    )


def _write_index(base: Base, *, source: str = "snapshot", collected_at: str = "2026-09-06T10:30:00Z") -> None:
    declared = base.config.sources["snapshot"]
    base.write_document(
        Document(
            source=source,
            layer=Layer.INDEX,
            collected_at=collected_at,
            schema=schema_of(declared),
            fields=fields_of(declared),
            count=1,
            records=[{"id": "snapshot-1", "title": "Snapshot"}],
        )
    )


def _finding(status: Status, check: str, contains: str = "") -> Finding | None:
    return next(
        (item for item in status.findings if item.check == check and contains in item.message),
        None,
    )


def test_public_status_types_are_typed_dataclasses() -> None:
    requirement = RequirementStatus("git", True)
    source = SourceStatus("source", True, Layer.EVENTS, requires=(requirement,))
    overview = LayerOverview(Layer.EVENTS, True, "events/", 0, "day")
    finding = Finding("trust", Severity.WARNING, "not trusted")

    assert requirement.on_path
    assert source.requires == (requirement,)
    assert overview.uri == "events/"
    assert finding.severity is Severity.WARNING


def test_report_reads_each_durable_document_once_and_keeps_corruption_as_a_finding(tmp_path: Path) -> None:
    base = _base(tmp_path)
    _write_event(base, "2026-09-05", 1)
    _write_index(base)
    corrupt = base.root / "events/2026-09-04/retired.json"
    corrupt.parent.mkdir(parents=True)
    corrupt.write_text("<<<<<<< HEAD\nnot json\n=======\nother\n>>>>>>> branch\n", encoding="utf-8")
    reads: Counter[str] = Counter()

    def reader(uri: str, limit: int) -> bytes:
        reads[uri] += 1
        return base.read_file(uri, limit)

    status = report(
        base,
        StatusRequest(
            max_age_hours=24,
            skip_git_audit=True,
            document_reader=reader,
            lexical_health=lambda _base: LexicalIndexUse(reason="missing"),
        ),
    )

    assert reads == Counter(
        {
            "events/2026-09-04/retired.json": 1,
            "events/2026-09-05/daily.json": 1,
            "index/snapshot.json": 1,
        }
    )
    assert _finding(status, "documents", "retired.json") is not None
    assert _finding(status, "conflict-markers") is not None
    assert status.sources[0].last_date == "2026-09-05"
    assert status.sources[0].last_count == 1
    assert not status.ok


def test_source_readiness_freshness_quiet_and_undeclared_snapshots(tmp_path: Path) -> None:
    base = _base(tmp_path)
    for index, count in enumerate((100, 120, 90, 110, 100, 130, 100, 110, 3), start=1):
        _write_event(base, f"2026-04-{index:02d}", count)
    _write_index(base, source="retired-index", collected_at="2026-09-06T11:30:00Z")

    status = report(
        base,
        StatusRequest(
            max_age_hours=24,
            skip_git_audit=True,
            lexical_health=lambda _base: LexicalIndexUse(reason="missing"),
        ),
    )
    by_name = {source.name: source for source in status.sources}

    assert [source.name for source in status.sources] == ["daily", "snapshot", "retired-index"]
    assert by_name["daily"].quiet
    assert by_name["daily"].median == 110
    assert by_name["daily"].stale
    assert by_name["snapshot"].stale
    assert by_name["retired-index"].undeclared
    assert (by_name["retired-index"].last_count, by_name["retired-index"].days) == (1, 1)
    assert status.enabled == 2
    assert status.missing_requirements == 1
    assert status.missing_test_hooks == 1
    assert status.quiet == 1
    assert status.stale


def test_legacy_event_freshness_uses_the_transition_safe_local_day_boundary(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setenv("TZ", "Pacific/Apia")
    now = datetime(2011, 12, 30, 11, tzinfo=UTC)
    base = _base(tmp_path, now=now)
    declared = base.config.sources["daily"]
    base.write_document(
        Document(
            source="daily",
            layer=Layer.EVENTS,
            date="2011-12-29",
            collected_at="2011-12-30T10:00:00Z",
            schema=schema_of(declared),
            fields=fields_of(declared),
            count=1,
            records=[{"id": "event", "time": "2011-12-29T12:00:00Z", "title": "Event"}],
        )
    )

    status = report(
        base,
        StatusRequest(
            max_age_hours=24,
            skip_git_audit=True,
            lexical_health=lambda _base: LexicalIndexUse(reason="missing"),
        ),
    )

    assert status.sources[0].last_collected_at == "2011-12-30T10:00:00Z"
    assert status.sources[0].lag_hours == 1
    assert not status.sources[0].stale


def test_weekend_quiet_watchdog_compares_only_the_same_weekday(tmp_path: Path) -> None:
    base = _base(tmp_path, now=datetime(2026, 4, 12, 12, tzinfo=UTC))
    counts = (10, 12, 9, 11, 10, 0, 0, 13, 10, 11, 0)
    for index, count in enumerate(counts, start=1):
        _write_event(base, f"2026-04-{index:02d}", count)

    status = report(
        base,
        StatusRequest(skip_git_audit=True, lexical_health=lambda _base: LexicalIndexUse(reason="missing")),
    )

    assert not status.sources[0].quiet
    assert status.sources[0].median == 0


def test_offline_status_executes_neither_auth_nor_harness_callbacks(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    base = _base(tmp_path)

    def forbidden_auth(*_args: object, **_kwargs: object) -> tuple[str, ...]:
        pytest.fail("offline status called a provider auth probe")

    def forbidden_harness(*_args: object, **_kwargs: object) -> tuple[HarnessRegistration, ...]:
        pytest.fail("offline status inspected user-scope harness files")

    monkeypatch.setattr("fkf.status.probe_source_auth", forbidden_auth)
    monkeypatch.setattr("fkf.status._tracked_paths", lambda _base: pytest.fail("skip_git_audit was ignored"))
    status = report(
        base,
        StatusRequest(
            live=False,
            skip_git_audit=True,
            inspect_harnesses=forbidden_harness,
            lexical_health=lambda _base: LexicalIndexUse(reason="missing"),
        ),
    )

    assert status.auth_required == ()
    assert status.harnesses == ()


def test_live_status_marks_auth_and_uses_the_injected_harness_inspector(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    base = _base(tmp_path)
    trust = TrustState(str(base.root), True, "a" * 64)
    cancel = Event()
    calls: list[tuple[Path, Path | str, object]] = []
    auth_cancellations: list[object] = []
    monkeypatch.setattr("fkf.status.read_trust", lambda _config, **_kwargs: trust)

    def probe(*_args: object, **kwargs: object) -> tuple[str, ...]:
        auth_cancellations.append(kwargs["cancel"])
        return ("daily",)

    monkeypatch.setattr("fkf.status.probe_source_auth", probe)

    def inspect(
        root: Path,
        *,
        executable: Path | str = "",
        cancel: object = None,
    ) -> tuple[HarnessRegistration, ...]:
        calls.append((root, executable, cancel))
        return (HarnessRegistration("codex", registered=False, changes=1),)

    status = report(
        base,
        StatusRequest(
            live=True,
            executable="/usr/local/bin/fkf",
            skip_git_audit=True,
            inspect_harnesses=inspect,
            lexical_health=lambda _base: LexicalIndexUse(reason="missing"),
        ),
        cancel=cancel,
    )

    assert calls == [(base.root, "/usr/local/bin/fkf", cancel)]
    assert auth_cancellations == [cancel]
    assert status.auth_required == ("daily",)
    assert status.sources[0].auth
    assert status.sources[0].auth_required
    assert status.harnesses == (HarnessRegistration("codex", registered=False, changes=1),)


def test_layer_knowledge_summary_and_unharvested_learned_warning(tmp_path: Path) -> None:
    base = _base(tmp_path)
    task = base.root / "tasks/2026-09-05/audit/TASKS.md"
    task.parent.mkdir(parents=True)
    task.write_text("# Audit\n\n## Learned\n\n- Durable lesson\n", encoding="utf-8")
    wiki = base.root / "wiki/retrieval.md"
    wiki.parent.mkdir(parents=True)
    wiki.write_text("---\ntype: decision\ntitle: Retrieval\ntags: [fkf]\n---\n\n# Retrieval\n", encoding="utf-8")
    project = base.root / "projects/fkf.md"
    project.parent.mkdir(parents=True)
    project.write_text(
        "---\ntype: project\ntitle: FKF\nstatus: active\ntags: [fkf]\n---\n\n# FKF\n",
        encoding="utf-8",
    )

    status = report(
        base,
        StatusRequest(skip_git_audit=True, lexical_health=lambda _base: LexicalIndexUse(reason="missing")),
    )
    layers = {overview.layer: overview for overview in status.layers}

    assert (layers[Layer.TASKS].count, layers[Layer.TASKS].unit, layers[Layer.TASKS].until) == (
        1,
        "trace",
        "2026-09-05",
    )
    assert layers[Layer.PROJECTS].note == "1 active"
    assert layers[Layer.WIKI].note == "1 tags"
    assert status.unharvested == 1
    assert _finding(status, "learned", '1 "## Learned" bullet') is not None
    assert any("list tasks learned --unharvested" in item for item in status.next)


def test_graph_and_lexical_damage_are_findings_not_report_failures(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    base = _base(tmp_path)
    monkeypatch.setattr(
        "fkf.status.summarize_graph", lambda _base: (_ for _ in ()).throw(EdgeValidationError("bad rows"))
    )

    status = report(
        base,
        StatusRequest(
            skip_git_audit=True,
            lexical_health=lambda _base: LexicalIndexUse(reason="corrupt"),
        ),
    )

    graph_finding = _finding(status, "derived", "graph cache is invalid")
    lexical_finding = _finding(status, "derived", "lexical index cache is corrupt")
    assert graph_finding is not None
    assert graph_finding.severity is Severity.ERROR
    assert lexical_finding is not None
    assert lexical_finding.severity is Severity.ERROR
    assert status.graph is None
    assert status.errors >= 2
    assert not status.ok


def test_missing_graph_is_a_warning_and_keeps_build_in_next(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    base = _base(tmp_path)
    monkeypatch.setattr(
        "fkf.status.summarize_graph",
        lambda _base, **_kwargs: (_ for _ in ()).throw(DerivedGraphMissingError("not built")),
    )

    status = report(
        base,
        StatusRequest(skip_git_audit=True, lexical_health=lambda _base: LexicalIndexUse(used=True)),
    )

    assert _finding(status, "derived", "graph cache is absent") is not None
    assert any("build graph" in item for item in status.next)


def test_permission_audit_is_read_only_relative_and_skips_symlinks(tmp_path: Path) -> None:
    base = _base(tmp_path)
    helper = base.root / "bin/lib/helper"
    helper.parent.mkdir(parents=True)
    helper.write_text("#!/bin/sh\n", encoding="utf-8")
    helper.chmod(0o755)
    outside = tmp_path / "outside"
    outside.mkdir(mode=0o755)
    link = base.root / "wiki/external"
    link.parent.mkdir(parents=True)
    link.symlink_to(outside, target_is_directory=True)

    status = report(
        base,
        StatusRequest(skip_git_audit=True, lexical_health=lambda _base: LexicalIndexUse(reason="missing")),
    )
    finding = _finding(status, "permissions")

    assert finding is not None
    assert "bin/lib/helper" in finding.paths
    assert all(not Path(path).is_absolute() for path in finding.paths)
    assert "wiki/external" not in finding.paths
    assert helper.stat().st_mode & 0o777 == 0o755
    assert outside.stat().st_mode & 0o777 == 0o755
    assert "chmod 700" in finding.fix
    assert "status --repair" not in finding.fix


def test_status_refuses_a_symlinked_owned_skill_tree(tmp_path: Path) -> None:
    base = _base(tmp_path)
    outside = tmp_path / "outside-skill"
    outside.mkdir()
    agents = base.root / ".agents"
    agents.mkdir()
    try:
        (agents / "skills").symlink_to(outside, target_is_directory=True)
    except OSError as error:
        pytest.skip(f"symlinks are unavailable: {error}")

    with pytest.raises(UnsafePathError, match="symlink inside the base"):
        report(
            base,
            StatusRequest(skip_git_audit=True, lexical_health=lambda _base: LexicalIndexUse(reason="missing")),
        )


def test_git_audit_uses_host_git_and_reports_only_relative_tracked_paths(tmp_path: Path) -> None:
    base = _base(tmp_path)
    git = shutil.which("git")
    assert git is not None
    subprocess.run((git, "-C", str(base.root), "init", "--quiet"), check=True)  # noqa: S603
    secret = base.root / ".env"
    secret.write_text("SECRET=1\n", encoding="utf-8")
    subprocess.run((git, "-C", str(base.root), "add", "-f", ".env"), check=True)  # noqa: S603
    marker = base.root / "base-git-ran"
    fake = base.root / "bin/git"
    fake.parent.mkdir()
    fake.write_text(f"#!/bin/sh\ntouch {marker}\nexit 99\n", encoding="utf-8")
    fake.chmod(0o700)

    status = report(base, StatusRequest(lexical_health=lambda _base: LexicalIndexUse(reason="missing")))
    finding = _finding(status, "tracked-credentials")

    assert finding is not None
    assert finding.paths == (".env",)
    assert finding.severity is Severity.ERROR
    assert not marker.exists()
    assert not status.ok


def test_git_audit_passes_the_status_cancellation_event_to_subprocess_runner(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    from fkf import status as status_module

    base = _base(tmp_path)
    git = shutil.which("git")
    assert git is not None
    subprocess.run((git, "-C", str(base.root), "init", "--quiet"), check=True)  # noqa: S603
    cancel = Event()
    seen: list[object] = []

    class Runner:
        def run(self, command: Command, *, cancel: object = None) -> CommandResult:
            assert command.argv[-1] == "ls-files"
            seen.append(cancel)
            return CommandResult(b"fkf.yaml\n")

    monkeypatch.setattr(status_module, "SubprocessRunner", Runner)

    report(
        base,
        StatusRequest(lexical_health=lambda _base: LexicalIndexUse(reason="missing")),
        cancel=cancel,
    )

    assert seen == [cancel]


def test_finding_order_counts_track_collected_and_commands_are_base_bound(tmp_path: Path) -> None:
    base = _base(tmp_path)
    quoted_root = str(base.root).replace("'", "'\"'\"'")
    begin = "# >>> fkf managed block — do not edit between the markers"
    end = "# <<< fkf managed block"
    (base.root / ".gitignore").write_text(f"{begin}\n# Collected content IS committed.\n{end}\n", encoding="utf-8")

    status = report(
        base,
        StatusRequest(skip_git_audit=True, lexical_health=lambda _base: LexicalIndexUse(reason="missing")),
    )

    assert status.track_collected
    assert [finding.check for finding in status.findings] == sorted(finding.check for finding in status.findings)
    assert status.errors == sum(finding.severity is Severity.ERROR for finding in status.findings)
    assert status.warnings == len(status.findings) - status.errors
    assert status.ok is (status.errors == 0)
    assert all(item.startswith(f"fkf --base '{quoted_root}' ") for item in status.next)


def test_status_request_rejects_invalid_age_and_normalizes_harness_results(tmp_path: Path) -> None:
    base = _base(tmp_path)
    with pytest.raises(ValueError, match="max_age_hours"):
        report(base, StatusRequest(max_age_hours=-1))

    request = StatusRequest(skip_git_audit=True, lexical_health=lambda _base: LexicalIndexUse(reason="missing"))
    assert replace(request, live=True).live


def test_status_cancels_after_a_document_read_without_converting_it_to_a_finding(tmp_path: Path) -> None:
    base = _base(tmp_path)
    _write_event(base, "2026-09-05", 1)
    cancel = Event()

    def reader(uri: str, limit: int) -> bytes:
        data = base.read_file(uri, limit)
        cancel.set()
        return data

    with pytest.raises(CanceledError):
        report(
            base,
            StatusRequest(
                skip_git_audit=True,
                document_reader=reader,
                lexical_health=lambda _base: LexicalIndexUse(reason="missing"),
            ),
            cancel=cancel,
        )
