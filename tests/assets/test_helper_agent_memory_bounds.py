"""Resource and confinement boundaries for coding-agent memory helpers."""

from __future__ import annotations

import json
import os
import runpy
from pathlib import Path
from types import ModuleType
from typing import Any

import pytest

from .conftest import HelperInstallation

START = "2026-05-04T00:00:00Z"
END = "2026-05-05T00:00:00Z"


def _module(helpers: HelperInstallation, name: str) -> dict[str, Any]:
    return runpy.run_path(os.fspath(helpers.bin / name))


def _memory(helpers: HelperInstallation, name: str = "memory.md", content: bytes = b"# Memory\n") -> Path:
    path = helpers.home / ".codex" / "memories" / name
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_bytes(content)
    os.utime(path, (1_777_885_200, 1_777_885_200))
    return path


def _run_direct(
    helpers: HelperInstallation,
    monkeypatch: pytest.MonkeyPatch,
    helper: str,
    arguments: list[str],
    **limits: int,
) -> int:
    namespace = _module(helpers, helper)
    main = namespace["main"]
    for name, value in limits.items():
        monkeypatch.setitem(main.__globals__, name, value)
    monkeypatch.setenv("HOME", os.fspath(helpers.home))
    return main(arguments)


def test_memory_listing_bounds_files_and_records_before_stdout(
    helpers: HelperInstallation,
    monkeypatch: pytest.MonkeyPatch,
    capfd: pytest.CaptureFixture[str],
) -> None:
    _memory(helpers, "one.md")
    _memory(helpers, "two.md")

    assert _run_direct(helpers, monkeypatch, "agent-memory-files.py", [START, END], MAX_FILES=1) == 1
    files = capfd.readouterr()
    assert files.out == ""
    assert "more than 1 memory files" in files.err

    assert (
        _run_direct(
            helpers,
            monkeypatch,
            "agent-memory-files.py",
            [START, END],
            MAX_FILES=2,
            MAX_RECORDS=1,
        )
        == 1
    )
    records = capfd.readouterr()
    assert records.out == ""
    assert "more than 1 memory records" in records.err


def test_memory_listing_enforces_the_exact_final_output_bound(
    helpers: HelperInstallation,
    monkeypatch: pytest.MonkeyPatch,
    capfd: pytest.CaptureFixture[str],
) -> None:
    _memory(helpers)

    assert _run_direct(helpers, monkeypatch, "agent-memory-files.py", [START, END]) == 0
    baseline = capfd.readouterr().out
    assert len(json.loads(baseline)) == 1

    assert (
        _run_direct(
            helpers,
            monkeypatch,
            "agent-memory-files.py",
            [START, END],
            MAX_OUTPUT_BYTES=len(baseline.encode()),
        )
        == 0
    )
    assert capfd.readouterr().out == baseline

    assert (
        _run_direct(
            helpers,
            monkeypatch,
            "agent-memory-files.py",
            [START, END],
            MAX_OUTPUT_BYTES=len(baseline.encode()) - 1,
        )
        == 1
    )
    oversized = capfd.readouterr()
    assert oversized.out == ""
    assert "output exceeds 64 MiB" in oversized.err


def test_memory_body_uses_an_exact_limit_plus_one_read(
    helpers: HelperInstallation,
    monkeypatch: pytest.MonkeyPatch,
    capfd: pytest.CaptureFixture[str],
) -> None:
    path = _memory(helpers, content=b"12345678")

    assert _run_direct(helpers, monkeypatch, "agent-memory-body.py", [os.fspath(path)], LIMIT=8) == 0
    assert capfd.readouterr().out == "12345678"

    path.write_bytes(b"123456789")
    assert _run_direct(helpers, monkeypatch, "agent-memory-body.py", [os.fspath(path)], LIMIT=8) == 1
    oversized = capfd.readouterr()
    assert oversized.out == ""
    assert "exceeds the 8-byte body limit" in oversized.err


@pytest.mark.parametrize("helper", ["agent-memory-files.py", "agent-memory-body.py"])
def test_memory_helpers_reject_symlinked_root_components(
    helpers: HelperInstallation,
    monkeypatch: pytest.MonkeyPatch,
    capfd: pytest.CaptureFixture[str],
    helper: str,
) -> None:
    target = helpers.home / "codex-target" / "memories"
    target.mkdir(parents=True)
    path = target / "memory.md"
    path.write_text("# Outside\n", encoding="utf-8")
    os.utime(path, (1_777_885_200, 1_777_885_200))
    (helpers.home / ".codex").symlink_to(target.parent, target_is_directory=True)
    arguments = (
        [START, END] if helper == "agent-memory-files.py" else [os.fspath(helpers.home / ".codex/memories/memory.md")]
    )

    assert _run_direct(helpers, monkeypatch, helper, arguments) == 1
    captured = capfd.readouterr()
    assert captured.out == ""
    assert "linked" in captured.err


def test_memory_body_rejects_parent_traversal_inside_a_lexical_root(
    helpers: HelperInstallation,
) -> None:
    root = helpers.home / ".codex" / "memories"
    root.mkdir(parents=True)
    outside = root.parent / "outside.md"
    outside.write_text("outside\n", encoding="utf-8")

    result = helpers.run("agent-memory-body.py", os.fspath(root / ".." / "outside.md"))

    assert result.returncode == 2
    assert result.stdout == b""
    assert b"outside the reviewed harness memory roots" in result.stderr


def test_memory_listing_emits_only_paths_accepted_by_the_body_helper(helpers: HelperInstallation) -> None:
    paths = (
        helpers.home / ".claude/projects/project/memory/claude.md",
        helpers.home / ".codex/memories/project/codex.md",
        helpers.home / ".gemini/tmp/project/memory/gemini.md",
        helpers.home / ".grok/memory/project/grok.md",
    )
    for path in paths:
        path.parent.mkdir(parents=True)
        path.write_text(f"# {path.stem}\n", encoding="utf-8")
        os.utime(path, (1_777_885_200, 1_777_885_200))
    too_deep = helpers.home / ".codex/memories/project/nested/ignored.md"
    too_deep.parent.mkdir(parents=True)
    too_deep.write_text("# Ignored\n", encoding="utf-8")
    os.utime(too_deep, (1_777_885_200, 1_777_885_200))

    listing = helpers.run("agent-memory-files.py", START, END)

    assert listing.returncode == 0, listing.stderr.decode(errors="replace")
    records = json.loads(listing.stdout)
    assert {record["id"] for record in records} == {os.fspath(path) for path in paths}
    for record in records:
        body = helpers.run("agent-memory-body.py", record["id"])
        assert body.returncode == 0, body.stderr.decode(errors="replace")


@pytest.mark.parametrize("helper", ["agent-memory-files.py", "agent-memory-body.py"])
def test_memory_helpers_reject_file_replacement_at_open(
    helpers: HelperInstallation,
    monkeypatch: pytest.MonkeyPatch,
    capfd: pytest.CaptureFixture[str],
    helper: str,
) -> None:
    path = _memory(helpers)
    namespace = _module(helpers, helper)
    main = namespace["main"]
    real_open = os.open
    replaced = False

    def replacing_open(name: str | bytes | os.PathLike[str] | os.PathLike[bytes], flags: int, **kwargs: Any) -> int:
        nonlocal replaced
        if kwargs.get("dir_fd") is not None and os.fsdecode(name) == path.name and not replaced:
            replaced = True
            path.rename(path.with_suffix(".old"))
            path.write_text("replacement\n", encoding="utf-8")
        return real_open(name, flags, **kwargs)

    isolated_os = ModuleType("isolated_os")
    isolated_os.__dict__.update(vars(os))
    isolated_os.__dict__["open"] = replacing_open
    monkeypatch.setitem(main.__globals__, "os", isolated_os)
    monkeypatch.setenv("HOME", os.fspath(helpers.home))
    arguments = [START, END] if helper == "agent-memory-files.py" else [os.fspath(path)]

    assert main(arguments) == 1
    captured = capfd.readouterr()
    assert captured.out == ""
    assert "changed while it was being opened" in captured.err


@pytest.mark.parametrize("helper", ["agent-memory-files.py", "agent-memory-body.py"])
def test_memory_helpers_reject_file_growth_while_reading(
    helpers: HelperInstallation,
    monkeypatch: pytest.MonkeyPatch,
    capfd: pytest.CaptureFixture[str],
    helper: str,
) -> None:
    path = _memory(helpers, content=b"# A\n")
    namespace = _module(helpers, helper)
    main = namespace["main"]
    real_read = os.read
    grew = False

    def growing_read(descriptor: int, count: int) -> bytes:
        nonlocal grew
        chunk = real_read(descriptor, count)
        if chunk and not grew:
            grew = True
            with path.open("ab") as stream:
                stream.write(b"growth\n")
        return chunk

    isolated_os = ModuleType("isolated_os")
    isolated_os.__dict__.update(vars(os))
    isolated_os.__dict__["read"] = growing_read
    monkeypatch.setitem(main.__globals__, "os", isolated_os)
    monkeypatch.setenv("HOME", os.fspath(helpers.home))
    arguments = [START, END] if helper == "agent-memory-files.py" else [os.fspath(path)]

    assert main(arguments) == 1
    captured = capfd.readouterr()
    assert captured.out == ""
    assert "changed while it was being read" in captured.err
