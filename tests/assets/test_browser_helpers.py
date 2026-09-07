"""Chromium-family local helpers bound every input and their final output."""

from __future__ import annotations

import json
import os
import runpy
import sqlite3
from pathlib import Path
from typing import Any

import pytest

from .conftest import HelperInstallation

START = "2026-05-04T00:00:00Z"
END = "2026-05-05T00:00:00Z"
WEBKIT_OFFSET_SECONDS = 11_644_473_600


def _main(helpers: HelperInstallation, name: str) -> Any:
    namespace = runpy.run_path(os.fspath(helpers.bin / name))
    return namespace["main"]


def _bookmark_document() -> dict[str, Any]:
    return {
        "roots": {
            "bookmark_bar": {
                "type": "folder",
                "name": "Bookmarks",
                "children": [
                    {
                        "type": "url",
                        "guid": "bookmark-1",
                        "name": "FKF",
                        "url": "https://fmind.github.io/fkf/",
                        "date_added": "13300000000000000",
                    }
                ],
            }
        }
    }


def _write_history(helpers: HelperInstallation) -> Path:
    database = helpers.home / ".config" / "chromium" / "Default" / "History"
    database.parent.mkdir(parents=True, exist_ok=True)
    connection = sqlite3.connect(database)
    try:
        connection.executescript(
            """
            create table urls (
                id integer primary key,
                url text,
                title text,
                visit_count integer,
                typed_count integer
            );
            create table visits (
                id integer primary key,
                url integer,
                visit_time integer,
                originator_cache_guid text,
                visit_duration integer,
                transition integer,
                from_visit integer
            );
            """
        )
        unix_seconds = 1_777_891_200  # 2026-05-04T10:40:00Z
        connection.execute(
            "insert into urls values (?, ?, ?, ?, ?)",
            (1, "https://example.test/path?private=yes#fragment", "Example", 3, 1),
        )
        connection.execute(
            "insert into visits values (?, ?, ?, ?, ?, ?, ?)",
            (
                1,
                1,
                (unix_seconds + WEBKIT_OFFSET_SECONDS) * 1_000_000,
                "",
                2_000_000,
                1,
                0,
            ),
        )
        connection.commit()
    finally:
        connection.close()
    return database


def _write_bookmarks(
    helpers: HelperInstallation,
    *,
    profile: str = "Default",
    document: dict[str, Any] | None = None,
) -> Path:
    path = helpers.home / ".config" / "chromium" / profile / "Bookmarks"
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(
        json.dumps(document or _bookmark_document(), separators=(",", ":")),
        encoding="utf-8",
    )
    return path


def test_chrome_bookmarks_accepts_the_exact_input_limit_and_rejects_one_byte_over(
    helpers: HelperInstallation,
    monkeypatch: pytest.MonkeyPatch,
    capfd: pytest.CaptureFixture[str],
) -> None:
    path = helpers.home / ".config" / "chromium" / "Default" / "Bookmarks"
    path.parent.mkdir(parents=True)
    content = json.dumps(_bookmark_document(), separators=(",", ":")).encode()
    path.write_bytes(content)
    main = _main(helpers, "chrome-bookmarks.py")
    monkeypatch.setitem(main.__globals__, "MAX_BOOKMARKS_BYTES", len(content))
    monkeypatch.setenv("HOME", os.fspath(helpers.home))

    assert main([]) == 0
    exact = capfd.readouterr()
    assert json.loads(exact.out)[0]["uid"] == "chromium/Default~bookmark-1"

    path.write_bytes(content + b" ")
    assert main([]) == 1
    oversized = capfd.readouterr()
    assert oversized.out == ""
    assert "Bookmarks input exceeds" in oversized.err


def test_browser_profile_discovery_accepts_the_limit_and_rejects_the_next_profile(
    helpers: HelperInstallation,
    monkeypatch: pytest.MonkeyPatch,
    capfd: pytest.CaptureFixture[str],
) -> None:
    _write_bookmarks(helpers)
    history = helpers.home / ".config" / "chromium" / "Default" / "History"
    history.touch()
    bookmarks_main = _main(helpers, "chrome-bookmarks.py")
    pages_main = _main(helpers, "chromium-pages.py")
    monkeypatch.setitem(bookmarks_main.__globals__, "MAX_PROFILES", 1)
    monkeypatch.setitem(pages_main.__globals__, "MAX_PROFILES", 1)
    monkeypatch.setenv("HOME", os.fspath(helpers.home))

    assert bookmarks_main([]) == 0
    assert json.loads(capfd.readouterr().out)[0]["uid"] == ("chromium/Default~bookmark-1")
    assert pages_main(["--profiles"]) == 0
    assert capfd.readouterr().out == "chromium/Default\t-\n"

    _write_bookmarks(helpers, profile="Profile 1")
    second_history = history.parent.parent / "Profile 1" / "History"
    second_history.touch()

    assert bookmarks_main([]) == 1
    bookmarks = capfd.readouterr()
    assert bookmarks.out == ""
    assert "profile count exceeds 1" in bookmarks.err
    assert pages_main(["--profiles"]) == 1
    pages = capfd.readouterr()
    assert pages.out == ""
    assert "profile count exceeds 1" in pages.err


def test_chrome_bookmarks_bounds_record_count_and_folder_depth(
    helpers: HelperInstallation,
    monkeypatch: pytest.MonkeyPatch,
    capfd: pytest.CaptureFixture[str],
) -> None:
    path = _write_bookmarks(helpers)
    main = _main(helpers, "chrome-bookmarks.py")
    monkeypatch.setitem(main.__globals__, "MAX_BOOKMARK_RECORDS", 1)
    monkeypatch.setitem(main.__globals__, "MAX_BOOKMARK_DEPTH", 1)
    monkeypatch.setenv("HOME", os.fspath(helpers.home))

    assert main([]) == 0
    assert len(json.loads(capfd.readouterr().out)) == 1

    two_records = _bookmark_document()
    children = two_records["roots"]["bookmark_bar"]["children"]
    children.append(
        {
            "type": "url",
            "guid": "bookmark-2",
            "name": "Second",
            "url": "https://example.test/second",
            "date_added": "13300000000000001",
        }
    )
    path.write_text(json.dumps(two_records), encoding="utf-8")

    assert main([]) == 1
    records = capfd.readouterr()
    assert records.out == ""
    assert "bookmark record count exceeds 1" in records.err

    nested = _bookmark_document()
    original_children = nested["roots"]["bookmark_bar"]["children"]
    nested["roots"]["bookmark_bar"]["children"] = [{"type": "folder", "name": "Nested", "children": original_children}]
    path.write_text(json.dumps(nested), encoding="utf-8")

    assert main([]) == 1
    depth = capfd.readouterr()
    assert depth.out == ""
    assert "bookmark folder depth exceeds 1" in depth.err


def test_chrome_bookmarks_handles_parser_recursion_as_an_input_error(
    helpers: HelperInstallation,
    monkeypatch: pytest.MonkeyPatch,
    capfd: pytest.CaptureFixture[str],
) -> None:
    _write_bookmarks(helpers)
    main = _main(helpers, "chrome-bookmarks.py")
    parser = main.__globals__["json"]

    def fail_recursively(_content: bytes) -> object:
        raise RecursionError

    monkeypatch.setattr(parser, "loads", fail_recursively)
    monkeypatch.setenv("HOME", os.fspath(helpers.home))

    assert main([]) == 1
    captured = capfd.readouterr()

    assert captured.out == ""
    assert "bookmark nesting exceeds" in captured.err


def test_chrome_bookmarks_reports_an_overflowing_timestamp_without_a_traceback(
    helpers: HelperInstallation,
    monkeypatch: pytest.MonkeyPatch,
    capfd: pytest.CaptureFixture[str],
) -> None:
    document = _bookmark_document()
    document["roots"]["bookmark_bar"]["children"][0]["date_added"] = str(10**100)
    _write_bookmarks(helpers, document=document)
    main = _main(helpers, "chrome-bookmarks.py")
    monkeypatch.setenv("HOME", os.fspath(helpers.home))

    assert main([]) == 1
    captured = capfd.readouterr()

    assert captured.out == ""
    assert captured.err.startswith("chrome-bookmarks.py: ")
    assert captured.err.count("\n") == 1
    assert os.fspath(helpers.home) not in captured.err


def test_chromium_profiles_bounds_local_state_without_losing_the_profile(
    helpers: HelperInstallation,
    monkeypatch: pytest.MonkeyPatch,
    capfd: pytest.CaptureFixture[str],
) -> None:
    database = helpers.home / ".config" / "chromium" / "Default" / "History"
    database.parent.mkdir(parents=True)
    database.touch()
    state = database.parent.parent / "Local State"
    content = json.dumps(
        {"profile": {"info_cache": {"Default": {"user_name": "owner@example.test"}}}},
        separators=(",", ":"),
    ).encode()
    state.write_bytes(content)
    main = _main(helpers, "chromium-pages.py")
    monkeypatch.setitem(main.__globals__, "MAX_LOCAL_STATE_BYTES", len(content))
    monkeypatch.setenv("HOME", os.fspath(helpers.home))

    assert main(["--profiles"]) == 0
    exact = capfd.readouterr()
    assert exact.out == "chromium/Default\towner@example.test\n"

    state.write_bytes(content + b" ")
    assert main(["--profiles"]) == 0
    oversized = capfd.readouterr()
    assert oversized.out == "chromium/Default\t-\n"


def test_chromium_profiles_enforces_the_exact_output_budget_before_stdout(
    helpers: HelperInstallation,
    monkeypatch: pytest.MonkeyPatch,
    capfd: pytest.CaptureFixture[str],
) -> None:
    database = helpers.home / ".config" / "chromium" / "Default" / "History"
    database.parent.mkdir(parents=True)
    database.touch()
    main = _main(helpers, "chromium-pages.py")
    monkeypatch.setenv("HOME", os.fspath(helpers.home))

    assert main(["--profiles"]) == 0
    expected = capfd.readouterr().out.encode()

    monkeypatch.setitem(main.__globals__, "MAX_OUTPUT_BYTES", len(expected))
    assert main(["--profiles"]) == 0
    exact = capfd.readouterr()
    assert exact.out.encode() == expected

    monkeypatch.setitem(main.__globals__, "MAX_OUTPUT_BYTES", len(expected) - 1)
    assert main(["--profiles"]) == 1
    oversized = capfd.readouterr()
    assert oversized.out == ""
    assert "aggregate output exceeds" in oversized.err


def test_chromium_pages_accepts_the_exact_snapshot_limit_and_rejects_one_byte_over(
    helpers: HelperInstallation,
    monkeypatch: pytest.MonkeyPatch,
    capfd: pytest.CaptureFixture[str],
) -> None:
    database = _write_history(helpers)
    input_bytes = database.stat().st_size
    main = _main(helpers, "chromium-pages.py")
    monkeypatch.setitem(main.__globals__, "MAX_HISTORY_BYTES", input_bytes)
    monkeypatch.setenv("HOME", os.fspath(helpers.home))
    monkeypatch.setenv("TMPDIR", os.fspath(helpers.temporary))

    assert main([START, END]) == 0
    exact = capfd.readouterr()
    assert json.loads(exact.out)[0]["url"] == "https://example.test/path"

    monkeypatch.setitem(main.__globals__, "MAX_HISTORY_BYTES", input_bytes - 1)
    assert main([START, END]) == 1
    oversized = capfd.readouterr()
    assert oversized.out == ""
    assert "History input exceeds" in oversized.err


def test_chromium_pages_enforces_the_exact_aggregate_output_budget_before_stdout(
    helpers: HelperInstallation,
    monkeypatch: pytest.MonkeyPatch,
    capfd: pytest.CaptureFixture[str],
) -> None:
    _write_history(helpers)
    main = _main(helpers, "chromium-pages.py")
    monkeypatch.setenv("HOME", os.fspath(helpers.home))
    monkeypatch.setenv("TMPDIR", os.fspath(helpers.temporary))

    assert main([START, END]) == 0
    baseline = capfd.readouterr()
    expected = baseline.out.encode()

    monkeypatch.setitem(main.__globals__, "MAX_OUTPUT_BYTES", len(expected))
    assert main([START, END]) == 0
    exact = capfd.readouterr()
    assert exact.out.encode() == expected

    monkeypatch.setitem(main.__globals__, "MAX_OUTPUT_BYTES", len(expected) - 1)
    assert main([START, END]) == 1
    oversized = capfd.readouterr()
    assert oversized.out == ""
    assert "aggregate output exceeds" in oversized.err


def test_chromium_pages_reads_committed_wal_rows_through_the_disk_snapshot(
    helpers: HelperInstallation,
) -> None:
    database = _write_history(helpers)
    writer = sqlite3.connect(database)
    try:
        assert writer.execute("pragma journal_mode = wal").fetchone()[0] == "wal"
        writer.execute("pragma wal_autocheckpoint = 0")
        writer.execute(
            "insert into urls values (?, ?, ?, ?, ?)",
            (2, "https://wal.example.test/committed", "WAL", 1, 0),
        )
        writer.execute(
            "insert into visits values (?, ?, ?, ?, ?, ?, ?)",
            (
                2,
                2,
                (1_777_891_260 + WEBKIT_OFFSET_SECONDS) * 1_000_000,
                "sync",
                0,
                1,
                1,
            ),
        )
        writer.commit()
        assert database.with_name("History-wal").is_file()
        result = helpers.run("chromium-pages.py", START, END)
    finally:
        writer.close()

    assert result.returncode == 0, result.stderr.decode(errors="replace")
    records = json.loads(result.stdout)
    assert [record["uid"] for record in records] == [
        "chromium/Default~1",
        "chromium/Default~2",
    ]


def test_chromium_pages_keeps_stdout_empty_when_a_later_profile_fails(
    helpers: HelperInstallation,
    monkeypatch: pytest.MonkeyPatch,
    capfd: pytest.CaptureFixture[str],
) -> None:
    _write_history(helpers)
    corrupt = helpers.home / ".config" / "chromium" / "Profile 1" / "History"
    corrupt.parent.mkdir()
    corrupt.write_bytes(b"not sqlite")
    main = _main(helpers, "chromium-pages.py")
    monkeypatch.setenv("HOME", os.fspath(helpers.home))
    monkeypatch.setenv("TMPDIR", os.fspath(helpers.temporary))

    assert main([START, END]) == 1
    captured = capfd.readouterr()

    assert captured.out == ""


def test_chromium_history_pipeline_has_no_memory_database_or_fetchall(
    helpers: HelperInstallation,
) -> None:
    source = (helpers.bin / "chromium-pages.py").read_text(encoding="utf-8")

    assert 'sqlite3.connect(":memory:")' not in source
    assert ".fetchall()" not in source


def test_browser_helpers_do_not_follow_symlinked_inputs(
    helpers: HelperInstallation,
    monkeypatch: pytest.MonkeyPatch,
    capfd: pytest.CaptureFixture[str],
) -> None:
    bookmark_target = helpers.root / "outside-bookmarks"
    bookmark_target.write_text(json.dumps(_bookmark_document()), encoding="utf-8")
    bookmark = helpers.home / ".config" / "chromium" / "Default" / "Bookmarks"
    bookmark.parent.mkdir(parents=True)
    bookmark.symlink_to(bookmark_target)
    bookmarks_main = _main(helpers, "chrome-bookmarks.py")
    monkeypatch.setenv("HOME", os.fspath(helpers.home))

    assert bookmarks_main([]) == 1
    bookmarks = capfd.readouterr()
    assert bookmarks.out == ""
    assert "not a regular file" in bookmarks.err

    bookmark.unlink()
    history_target = _write_history(helpers)
    moved = helpers.root / "outside-history"
    history_target.rename(moved)
    history_target.symlink_to(moved)
    pages_main = _main(helpers, "chromium-pages.py")
    monkeypatch.setenv("TMPDIR", os.fspath(helpers.temporary))

    assert pages_main([START, END]) == 1
    pages = capfd.readouterr()
    assert pages.out == ""
    assert "not a regular file" in pages.err
