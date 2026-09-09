"""Scaffold, refresh, assets, and deterministic demo contracts."""

from __future__ import annotations

import hashlib
import os
from datetime import UTC, datetime
from pathlib import Path
from threading import Event
from zoneinfo import ZoneInfo

import pytest

import fkf.init as init_module
from fkf.assets import BUNDLED_SKILLS, PRESETS, digest_tree, read_asset, shipped_helpers
from fkf.build import BuildOptions, BuildReport, BuildTarget
from fkf.config import load_config
from fkf.errors import CanceledError, InvalidUsageError
from fkf.init import InitRequest, init_base, install_skills, tracks_collected
from fkf.process import Cancellation
from fkf.trust import TrustState, read_trust

NOW = datetime(2026, 9, 6, 12, 34, tzinfo=UTC)


def test_assets_preserve_repository_bytes_and_tree_framing() -> None:
    repository = Path(__file__).parents[2]
    assert PRESETS == ("minimal", "personal", "team")
    assert BUNDLED_SKILLS == ("fkf-use", "fkf-learn", "daily-brief")
    assets_root = repository / "src/fkf/assets_data"
    for asset_prefix in ("presets", "skills"):
        source_root = assets_root / asset_prefix
        for source in sorted(path for path in source_root.rglob("*") if path.is_file()):
            relative = source.relative_to(source_root).as_posix()
            assert read_asset(f"{asset_prefix}/{relative}") == source.read_bytes()
    assert "fkf-hook.py" in shipped_helpers()
    left = digest_tree({"SKILL.md": b"instructions"})
    right = digest_tree({"SKILL.m": b"dinstructions"})
    assert left != right
    assert left == hashlib.sha256(b"P\0\0\0\0\0\0\0\x08SKILL.mdC\0\0\0\0\0\0\0\x0cinstructions").hexdigest()


def test_create_refresh_preserves_owned_and_owner_files(tmp_path: Path) -> None:
    root = tmp_path / "My Brain"
    created = init_base(InitRequest(path=root, skip_git=True), now=lambda: NOW)

    assert created.created is True
    assert created.refreshed is False
    assert created.name == "my-brain"
    assert created.preset == "minimal"
    assert created.trusted is True
    assert load_config(root).name == "my-brain"
    assert (root / "sources" / "fkf-hook.py").stat().st_mode & 0o777 == 0o700
    assert (root / "CLAUDE.md").read_text(encoding="utf-8") == "@AGENTS.md\n"
    assert (root / ".claude" / "skills").readlink() == Path("../.agents/skills")
    assert {path.name for path in (root / ".agents" / "skills").iterdir()} == set(BUNDLED_SKILLS)
    assert read_trust(load_config(root)).trusted is True
    assert tracks_collected(root) is False

    config = (root / "fkf.yaml").read_bytes()
    agents = (root / "AGENTS.md").read_bytes()
    helper = root / "sources" / "fkf-hook.py"
    helper.write_text("owner helper\n", encoding="utf-8")
    (root / "CLAUDE.md").write_text("owner instructions\n", encoding="utf-8")
    skill = root / ".agents" / "skills" / "fkf-use" / "SKILL.md"
    skill.write_text("drifted\n", encoding="utf-8")
    refreshed = init_base(InitRequest(path=root, track_collected=True), now=lambda: NOW)

    assert refreshed.created is False
    assert refreshed.refreshed is True
    assert refreshed.track_collected is True
    assert (root / "fkf.yaml").read_bytes() == config
    assert (root / "AGENTS.md").read_bytes() == agents
    assert helper.read_text(encoding="utf-8") == "owner helper\n"
    assert (root / "CLAUDE.md").read_text(encoding="utf-8") == "owner instructions\n"
    assert skill.read_bytes() == read_asset("skills/fkf-use/SKILL.md")
    assert tracks_collected(root) is True


@pytest.mark.parametrize("preset", PRESETS)
def test_every_preset_loads_and_only_materializes_enabled_helpers(tmp_path: Path, preset: str) -> None:
    root = tmp_path / preset
    report = init_base(InitRequest(path=root, preset=preset, skip_git=True), now=lambda: NOW)
    config = load_config(root)

    assert report.declared == len(config.sources)
    assert report.enabled == len(config.enabled_sources())
    if preset == "team":
        assert config.enabled_sources() == ()
    official = shipped_helpers()
    expected = {"fkf-hook.py"}
    for source in config.enabled_sources():
        expected.update(name for name in source.requires if name in official)
    assert {path.name for path in (root / "sources").iterdir()} == expected


def test_preexisting_execution_inputs_prevent_automatic_trust(tmp_path: Path) -> None:
    root = tmp_path / "brain"
    (root / "sources").mkdir(parents=True)
    (root / "sources" / "owner").write_text("owner\n", encoding="utf-8")

    report = init_base(InitRequest(path=root, skip_git=True), now=lambda: NOW)

    assert report.trusted is False
    assert read_trust(load_config(root)).trusted is False


def test_malformed_second_managed_file_preflights_before_first_write(tmp_path: Path) -> None:
    root = tmp_path / "brain"
    root.mkdir()
    ignore = root / ".gitignore"
    ignore.write_text("owner\n", encoding="utf-8")
    attributes = root / ".gitattributes"
    attributes.write_text("# >>> fkf managed block broken\n", encoding="utf-8")

    with pytest.raises(Exception, match="non-canonical begin"):
        init_base(InitRequest(path=root, skip_git=True), now=lambda: NOW)

    assert ignore.read_text(encoding="utf-8") == "owner\n"
    assert not (root / "fkf.yaml").exists()


def test_demo_is_bounded_and_deterministic(tmp_path: Path) -> None:
    with pytest.raises(InvalidUsageError, match=r"1\.\.366"):
        init_base(InitRequest(path=tmp_path / "zero", demo=-1, name="demo"), now=lambda: NOW)
    with pytest.raises(InvalidUsageError, match="omit --preset"):
        init_base(InitRequest(path=tmp_path / "mixed", demo=1, preset="personal"), now=lambda: NOW)

    first = tmp_path / "first"
    second = tmp_path / "second"
    one = init_base(InitRequest(path=first, demo=2, skip_git=True), now=lambda: NOW)
    two = init_base(InitRequest(path=second, demo=2, skip_git=True), now=lambda: NOW)
    assert one.demo is not None
    assert two.demo is not None
    assert (one.demo.records, one.demo.pages, one.demo.since, one.demo.until) == (72, 6, "2026-09-04", "2026-09-05")
    first_files = {
        path.relative_to(first).as_posix(): path.read_bytes()
        for path in first.rglob("*")
        if path.is_file() and path.relative_to(first).parts[0] in {"events", "projects", "wiki"}
    }
    second_files = {
        path.relative_to(second).as_posix(): path.read_bytes()
        for path in second.rglob("*")
        if path.is_file() and path.relative_to(second).parts[0] in {"events", "projects", "wiki"}
    }
    assert first_files == second_files


def test_demo_is_byte_identical_across_timezones_on_one_local_day(tmp_path: Path) -> None:
    def render(parent: Path, now: datetime) -> dict[str, bytes]:
        root = parent / "demo"
        init_base(InitRequest(path=root, demo=2, skip_git=True), now=lambda: now)
        return {
            path.relative_to(root).as_posix(): path.read_bytes()
            for path in root.rglob("*")
            if path.is_file() and not path.is_symlink()
        }

    east = render(tmp_path / "east", datetime(2026, 5, 10, 1, 2, tzinfo=ZoneInfo("Pacific/Kiritimati")))
    west = render(tmp_path / "west", datetime(2026, 5, 10, 23, 59, tzinfo=ZoneInfo("Etc/GMT+12")))
    assert east == west


def test_failed_demo_init_is_retryable_and_removes_created_executables(tmp_path: Path) -> None:
    root = tmp_path / "brain"
    root.mkdir()
    owner = root / "owner.txt"
    owner.write_text("keep\n", encoding="utf-8")
    (root / "graph.tsv").mkdir()

    with pytest.raises(Exception, match=r"already holds graph\.tsv"):
        init_base(InitRequest(path=root, demo=1, skip_git=True), now=lambda: NOW)

    assert owner.read_text(encoding="utf-8") == "keep\n"
    assert not (root / "fkf.yaml").exists()
    assert not (root / "sources" / "fkf-hook.py").exists()
    assert not (root / "sources" / "fkf-demo-json.sh").exists()
    assert tuple((root / "events").iterdir()) == ()


def test_init_git_uses_host_path_not_preexisting_base_bin(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    root = tmp_path / "brain"
    (root / "sources").mkdir(parents=True)
    marker = tmp_path / "base-git-ran"
    malicious = root / "sources" / "git"
    malicious.write_text(f"#!/bin/sh\ntouch '{marker}'\n", encoding="utf-8")
    malicious.chmod(0o700)
    monkeypatch.setenv("PATH", os.pathsep.join((os.fspath(root / "sources"), "/usr/bin", "/bin")))

    report = init_base(InitRequest(path=root), now=lambda: NOW)

    assert (root / ".git" / "HEAD").is_file()
    assert not marker.exists()
    assert report.trusted is False


def test_init_rejects_unsafe_targets_and_cancellation(tmp_path: Path) -> None:
    outside = tmp_path / "outside"
    outside.mkdir()
    root = tmp_path / "brain"
    root.mkdir()
    (root / "sources").symlink_to(outside, target_is_directory=True)
    with pytest.raises(Exception, match="symlink"):
        init_base(InitRequest(path=root, skip_git=True), now=lambda: NOW)
    assert not (root / "fkf.yaml").exists()

    canceled = Event()
    canceled.set()
    with pytest.raises(CanceledError):
        install_skills(tmp_path / "base", cancel=canceled)


def test_init_forwards_one_event_through_scaffold_build_and_initial_trust(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    cancel = Event()
    seen: list[str] = []

    def execution_tree(_root: object, *, cancel: object) -> tuple[()]:
        assert cancel is cancel_event
        seen.append("execution-tree")
        return ()

    def build(_base: object, _options: object = None, *, cancel: object) -> None:
        assert cancel is cancel_event
        seen.append("build")

    def write_trust(_config: object, _now: datetime, *, cancel: object) -> None:
        assert cancel is cancel_event
        seen.append("trust")

    def read_trust(_config: object, *, cancel: object) -> TrustState:
        assert cancel is cancel_event
        seen.append("trust-read")
        return TrustState(str(root), True, "digest")

    cancel_event = cancel
    root = tmp_path / "brain"
    monkeypatch.setattr(init_module, "source_scripts", execution_tree)
    monkeypatch.setattr(init_module, "test_scripts", execution_tree)
    monkeypatch.setattr(init_module, "build", build)
    monkeypatch.setattr(init_module, "write_trust", write_trust)
    monkeypatch.setattr(init_module, "read_trust", read_trust)

    report = init_base(InitRequest(path=root, skip_git=True), now=lambda: NOW, cancel=cancel)
    refreshed = init_base(InitRequest(path=root, skip_git=True), now=lambda: NOW, cancel=cancel)

    assert report.trusted
    assert refreshed.trusted
    assert seen == [
        "execution-tree",
        "execution-tree",
        "execution-tree",
        "execution-tree",
        "build",
        "trust",
        "execution-tree",
        "execution-tree",
        "trust-read",
        "build",
        "trust-read",
    ]


def test_demo_forwards_one_event_to_each_derived_builder_and_graph_input_scan(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    cancel = Event()
    targets: list[BuildTarget] = []
    graph_scans = 0

    def build(
        _base: object,
        options: BuildOptions | None = None,
        *,
        cancel: Cancellation | None = None,
    ) -> BuildReport:
        assert cancel is cancel_event
        targets.append((options or BuildOptions()).target)
        return BuildReport()

    def graph_inputs(_base: object, *, cancel: Cancellation | None = None) -> tuple[str, ...]:
        nonlocal graph_scans
        assert cancel is cancel_event
        graph_scans += 1
        return ()

    cancel_event = cancel
    monkeypatch.setattr(init_module, "build", build)
    monkeypatch.setattr(init_module, "graph_input_uris", graph_inputs)

    report = init_base(InitRequest(path=tmp_path / "demo", demo=1, skip_git=True), now=lambda: NOW, cancel=cancel)

    assert report.demo is not None
    assert targets == [BuildTarget.WIKI, BuildTarget.GRAPH, BuildTarget.INDEX]
    assert graph_scans == 1


def test_refresh_does_not_swallow_cancellation_from_best_effort_trust_read(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    root = tmp_path / "brain"
    init_base(InitRequest(path=root, skip_git=True), now=lambda: NOW)
    before = {path.relative_to(root): path.read_bytes() for path in root.rglob("*") if path.is_file()}
    cancel = Event()

    def canceled_read(_config: object, *, cancel: object) -> None:
        assert cancel is cancel_event
        cancel_event.set()
        raise CanceledError("operation canceled")

    cancel_event = cancel
    monkeypatch.setattr(init_module, "read_trust", canceled_read)

    with pytest.raises(CanceledError):
        init_base(InitRequest(path=root, track_collected=True), now=lambda: NOW, cancel=cancel)

    assert {path.relative_to(root): path.read_bytes() for path in root.rglob("*") if path.is_file()} == before
