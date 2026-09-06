"""Physical-base writer lock contracts."""

from __future__ import annotations

import multiprocessing
import os
from pathlib import Path
from typing import Any

import pytest

import fkf.locking as locking
from fkf.locking import BaseBusyError, StateDirectoryUnavailableError, WriterLock, state_dir


def _hold_lock(root: str, state: str, ready: Any, release: Any) -> None:
    os.environ["XDG_STATE_HOME"] = state
    with WriterLock.acquire(root):
        ready.set()
        release.wait(timeout=10)


def test_state_dir_fails_closed_without_home_or_xdg(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("HOME", "")
    monkeypatch.setenv("XDG_STATE_HOME", "")
    with pytest.raises(StateDirectoryUnavailableError, match="HOME or XDG_STATE_HOME"):
        state_dir()


def test_writer_lock_excludes_process_and_releases(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    state = tmp_path / "state"
    root = tmp_path / "base"
    root.mkdir()
    monkeypatch.setenv("XDG_STATE_HOME", str(state))
    context = multiprocessing.get_context("spawn")
    ready = context.Event()
    release = context.Event()
    process = context.Process(target=_hold_lock, args=(str(root), str(state), ready, release))
    process.start()
    try:
        assert ready.wait(timeout=10)
        with pytest.raises(BaseBusyError, match="active writer"):
            WriterLock.acquire(root)
        release.set()
        process.join(timeout=10)
        assert process.exitcode == 0
        with WriterLock.acquire(root):
            pass
    finally:
        release.set()
        process.kill() if process.is_alive() else None
        process.join(timeout=5)


def test_writer_lock_canonicalizes_symlink_aliases(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("XDG_STATE_HOME", str(tmp_path / "state"))
    real = tmp_path / "real"
    real.mkdir()
    alias = tmp_path / "alias"
    alias.symlink_to(real, target_is_directory=True)
    with WriterLock.acquire(real), pytest.raises(BaseBusyError):
        WriterLock.acquire(alias)


def test_writer_lock_preserves_exclusion_when_a_locked_init_target_appears(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setenv("XDG_STATE_HOME", str(tmp_path / "state"))
    root = tmp_path / "new-base"

    first = WriterLock.acquire(root)
    root.mkdir()
    try:
        with pytest.raises(BaseBusyError, match="active writer"):
            WriterLock.acquire(root)
    finally:
        first.close()


def test_writer_lock_state_is_owner_only_and_outside_base(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    state = tmp_path / "state"
    lock_dir = state / "fkf" / "locks"
    lock_dir.mkdir(parents=True, mode=0o755)
    root = tmp_path / "base"
    root.mkdir()
    monkeypatch.setenv("XDG_STATE_HOME", str(state))
    before = tuple(root.iterdir())
    with WriterLock.acquire(root):
        pass
    assert tuple(root.iterdir()) == before
    assert lock_dir.stat().st_mode & 0o777 == 0o700
    entries = list(lock_dir.iterdir())
    assert len(entries) == 1
    assert entries[0].stat().st_mode & 0o777 == 0o600


def test_writer_lock_rejects_state_physically_inside_base_before_creating_it(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    root = tmp_path / "base"
    root.mkdir()
    alias = tmp_path / "alias"
    alias.symlink_to(root, target_is_directory=True)
    state = alias / "machine-state"
    monkeypatch.setenv("XDG_STATE_HOME", str(state))

    with pytest.raises(StateDirectoryUnavailableError, match="outside the base"):
        WriterLock.acquire(root)

    assert not (root / "machine-state").exists()


def test_writer_lock_rejects_state_logically_inside_base_through_an_escaping_symlink(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    root = tmp_path / "base"
    root.mkdir()
    outside = tmp_path / "outside-state"
    outside.mkdir()
    state_link = root / "state-link"
    state_link.symlink_to(outside, target_is_directory=True)
    monkeypatch.setenv("XDG_STATE_HOME", str(state_link))

    with pytest.raises(StateDirectoryUnavailableError, match="outside the base"):
        WriterLock.acquire(root)

    assert not (outside / "fkf").exists()


@pytest.mark.parametrize("symlinked_component", ["fkf", "locks"])
def test_writer_lock_rejects_symlinked_private_state_directories_without_chmod(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, symlinked_component: str
) -> None:
    root = tmp_path / "base"
    root.mkdir()
    state = tmp_path / "state"
    state.mkdir()
    outside = tmp_path / "outside-directory"
    outside.mkdir(mode=0o755)
    if symlinked_component == "fkf":
        (state / "fkf").symlink_to(outside, target_is_directory=True)
    else:
        machine_state = state / "fkf"
        machine_state.mkdir()
        (machine_state / "locks").symlink_to(outside, target_is_directory=True)
    monkeypatch.setenv("XDG_STATE_HOME", str(state))

    with pytest.raises(StateDirectoryUnavailableError, match="must be a real directory"):
        WriterLock.acquire(root)

    assert outside.stat().st_mode & 0o777 == 0o755
    assert tuple(outside.iterdir()) == ()


def test_writer_lock_rejects_a_symlinked_lock_file_without_chmod(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    root = tmp_path / "base"
    root.mkdir()
    state = tmp_path / "state"
    monkeypatch.setenv("XDG_STATE_HOME", str(state))
    with WriterLock.acquire(root):
        pass
    lock_path = next((state / "fkf" / "locks").iterdir())
    lock_path.unlink()
    outside = tmp_path / "outside-lock"
    outside.write_text("outside", encoding="utf-8")
    outside.chmod(0o644)
    lock_path.symlink_to(outside)

    with pytest.raises(StateDirectoryUnavailableError, match="open writer lock"):
        WriterLock.acquire(root)

    assert outside.stat().st_mode & 0o777 == 0o644
    assert outside.read_text(encoding="utf-8") == "outside"


def test_writer_lock_rejects_a_hardlinked_lock_file_without_chmod(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    root = tmp_path / "base"
    root.mkdir()
    state = tmp_path / "state"
    monkeypatch.setenv("XDG_STATE_HOME", str(state))
    with WriterLock.acquire(root):
        pass
    lock_path = next((state / "fkf" / "locks").iterdir())
    lock_path.unlink()
    outside = tmp_path / "outside-lock"
    outside.write_text("outside", encoding="utf-8")
    outside.chmod(0o644)
    lock_path.hardlink_to(outside)

    with pytest.raises(StateDirectoryUnavailableError, match="single-link regular file"):
        WriterLock.acquire(root)

    assert outside.stat().st_mode & 0o777 == 0o644
    assert outside.read_text(encoding="utf-8") == "outside"


@pytest.mark.parametrize("component", ["fkf", "locks"])
def test_writer_lock_rejects_a_real_directory_replacement_before_open_without_chmod(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, component: str
) -> None:
    root = tmp_path / "base"
    root.mkdir()
    state = tmp_path / "state"
    replacement = tmp_path / f"replacement-{component}"
    replacement.mkdir(mode=0o755)
    sentinel = replacement / "keep.txt"
    sentinel.write_text("outside", encoding="utf-8")
    detached = tmp_path / f"detached-{component}"
    monkeypatch.setenv("XDG_STATE_HOME", str(state))
    real_open = locking.os.open
    swapped = False

    def swap_before_open(
        path: str | bytes | os.PathLike[str],
        flags: int,
        mode: int = 0o777,
        *,
        dir_fd: int | None = None,
    ) -> int:
        nonlocal swapped
        if os.fsdecode(path) == component and not swapped:
            swapped = True
            target = state / "fkf"
            if component == "locks":
                target /= "locks"
            target.rename(detached)
            replacement.rename(target)
        return real_open(path, flags, mode, dir_fd=dir_fd)

    monkeypatch.setattr(locking.os, "open", swap_before_open)

    with pytest.raises(StateDirectoryUnavailableError, match="changed before it was opened"):
        WriterLock.acquire(root)

    current = state / "fkf"
    if component == "locks":
        current /= "locks"
    assert current.stat().st_mode & 0o777 == 0o755
    assert (current / sentinel.name).read_text(encoding="utf-8") == "outside"


def test_writer_lock_does_not_split_when_the_locks_directory_is_replaced(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    root = tmp_path / "base"
    root.mkdir()
    state = tmp_path / "state"
    monkeypatch.setenv("XDG_STATE_HOME", str(state))

    first = WriterLock.acquire(root)
    locks = state / "fkf" / "locks"
    detached = state / "fkf" / "detached-locks"
    locks.rename(detached)
    locks.mkdir()
    try:
        with pytest.raises(BaseBusyError, match="active writer"):
            WriterLock.acquire(root)
    finally:
        first.close()
