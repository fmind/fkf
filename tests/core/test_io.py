"""Bounded regular-file and atomic-write contracts."""

from __future__ import annotations

import os
from pathlib import Path

import pytest

from fkf.io import FileTooLargeError, atomic_write, read_file_limited, write_json
from fkf.store import UnsafePathError


def test_read_file_limited_accepts_exact_limit_and_rejects_oversize(tmp_path: Path) -> None:
    path = tmp_path / "cache.json"
    path.write_bytes(b"12345")
    assert read_file_limited(path, 5) == b"12345"
    with pytest.raises(FileTooLargeError, match="limit 4"):
        read_file_limited(path, 4)


def test_read_file_limited_rejects_symlink_and_bad_limit(tmp_path: Path) -> None:
    target = tmp_path / "target"
    target.write_bytes(b"private")
    link = tmp_path / "link"
    link.symlink_to(target)
    with pytest.raises(UnsafePathError):
        read_file_limited(link, 1024)
    with pytest.raises(ValueError, match="positive"):
        read_file_limited(target, 0)


def test_atomic_write_replaces_complete_bytes_and_tightens_mode(tmp_path: Path) -> None:
    path = tmp_path / "nested" / "value"
    atomic_write(path, b"first", mode=0o640)
    assert path.read_bytes() == b"first"
    assert path.stat().st_mode & 0o777 == 0o640
    atomic_write(path, b"second", mode=0o600)
    assert path.read_bytes() == b"second"
    assert path.stat().st_mode & 0o777 == 0o600
    assert not list(path.parent.glob(".*.tmp"))


def test_atomic_write_creates_every_missing_parent_owner_only(tmp_path: Path) -> None:
    tmp_path.chmod(0o750)
    root_mode = tmp_path.stat().st_mode & 0o777
    target = tmp_path / "date" / "task" / "TASKS.md"
    previous = os.umask(0o022)
    try:
        atomic_write(target, b"private trace")
    finally:
        os.umask(previous)
    assert target.read_bytes() == b"private trace"
    assert target.stat().st_mode & 0o777 == 0o600
    assert target.parent.stat().st_mode & 0o777 == 0o700
    assert target.parent.parent.stat().st_mode & 0o777 == 0o700
    assert tmp_path.stat().st_mode & 0o777 == root_mode


def test_write_json_is_deterministic_and_newline_terminated(tmp_path: Path) -> None:
    path = tmp_path / "value.json"
    write_json({"z": 1, "value": "é", "n": 9_007_199_254_740_993}, path)
    assert path.read_bytes() == b'{\n  "n": 9007199254740993,\n  "value": "\xc3\xa9",\n  "z": 1\n}\n'
    assert path.stat().st_mode & 0o777 == 0o600


def test_atomic_write_leaves_old_file_when_replace_fails(monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> None:
    path = tmp_path / "value"
    path.write_bytes(b"old")

    def fail_replace(
        _source: str | bytes | os.PathLike[str] | os.PathLike[bytes],
        _target: str | bytes | os.PathLike[str] | os.PathLike[bytes],
    ) -> None:
        raise OSError("simulated")

    monkeypatch.setattr(os, "replace", fail_replace)
    with pytest.raises(OSError, match="simulated"):
        atomic_write(path, b"new")
    assert path.read_bytes() == b"old"
    assert not list(tmp_path.glob(".*.tmp"))
