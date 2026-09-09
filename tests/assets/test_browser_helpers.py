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


@pytest.mark.parametrize("title", [None, "", " \t\u200b\n"])
def test_browser_visits_project_a_meaningful_title_when_the_page_has_none(
    helpers: HelperInstallation, monkeypatch: pytest.MonkeyPatch, capfd: pytest.CaptureFixture[str], title: str | None
) -> None:
    database = _write_history(helpers)
    connection = sqlite3.connect(database)
    try:
        connection.execute("update urls set title = ?", (title,))
        connection.commit()
    finally:
        connection.close()
    monkeypatch.setenv("HOME", os.fspath(helpers.home))
    assert _main(helpers, "chromium-pages.py")([START, END]) == 0
    output = json.loads(capfd.readouterr().out)
    assert output[0]["title"] == "Visit https://example.test/path"
    assert "private=yes" not in output[0]["title"]


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


@pytest.mark.parametrize("linked_component", ["browser-root", "profile"])
def test_chrome_bookmarks_rejects_symlinked_profile_components(
    helpers: HelperInstallation,
    linked_component: str,
) -> None:
    bookmark = _write_bookmarks(helpers)
    component = bookmark.parent.parent if linked_component == "browser-root" else bookmark.parent
    outside = helpers.root / f"outside-bookmarks-{linked_component}"
    component.rename(outside)
    component.symlink_to(outside, target_is_directory=True)

    result = helpers.run("chrome-bookmarks.py")

    assert result.returncode != 0
    assert result.stdout == b""
    assert b"linked" in result.stderr


@pytest.mark.parametrize("linked_component", ["browser-root", "profile"])
def test_chromium_pages_rejects_symlinked_profile_components(
    helpers: HelperInstallation,
    linked_component: str,
) -> None:
    database = _write_history(helpers)
    component = database.parent.parent if linked_component == "browser-root" else database.parent
    outside = helpers.root / f"outside-history-{linked_component}"
    component.rename(outside)
    component.symlink_to(outside, target_is_directory=True)

    result = helpers.run("chromium-pages.py", START, END)

    assert result.returncode != 0
    assert result.stdout == b""
    assert b"linked" in result.stderr


@pytest.mark.parametrize(
    ("helper_name", "leaf_name", "arguments"),
    [
        ("chrome-bookmarks.py", "Bookmarks", ()),
        ("chromium-pages.py", "History", ("--profiles",)),
    ],
)
@pytest.mark.parametrize("linked_component", ["browser-root", "profile"])
def test_browser_helpers_reject_an_empty_linked_profile_component(
    helpers: HelperInstallation,
    helper_name: str,
    leaf_name: str,
    arguments: tuple[str, ...],
    linked_component: str,
) -> None:
    empty = helpers.root / f"outside-empty-{leaf_name.lower()}"
    empty.mkdir()
    config = helpers.home / ".config"
    config.mkdir()
    browser = config / "chromium"
    if linked_component == "browser-root":
        browser.symlink_to(empty, target_is_directory=True)
    else:
        browser.mkdir()
        (browser / "Default").symlink_to(empty, target_is_directory=True)
    valid = config / "google-chrome" / "Default" / leaf_name
    valid.parent.mkdir(parents=True)
    valid.write_text(json.dumps(_bookmark_document()) if leaf_name == "Bookmarks" else "", encoding="utf-8")

    result = helpers.run(helper_name, *arguments)

    assert result.returncode == 1
    assert result.stdout == b""
    assert b"linked" in result.stderr


@pytest.mark.parametrize(
    ("helper_name", "leaf_name", "arguments"),
    [
        ("chrome-bookmarks.py", "Bookmarks", ()),
        ("chromium-pages.py", "History", ("--profiles",)),
    ],
)
def test_browser_helpers_report_an_unreadable_browser_root(
    helpers: HelperInstallation,
    monkeypatch: pytest.MonkeyPatch,
    capfd: pytest.CaptureFixture[str],
    helper_name: str,
    leaf_name: str,
    arguments: tuple[str, ...],
) -> None:
    blocked = helpers.home / ".config" / "chromium"
    blocked.mkdir(parents=True)
    blocked_status = blocked.stat()
    valid = helpers.home / ".config" / "google-chrome" / "Default" / leaf_name
    valid.parent.mkdir(parents=True)
    valid.write_text(json.dumps(_bookmark_document()) if leaf_name == "Bookmarks" else "", encoding="utf-8")
    real_scandir = os.scandir

    def unavailable(path: int | str | os.PathLike[str]) -> Any:
        status = os.fstat(path) if isinstance(path, int) else Path(path).stat()
        if (status.st_dev, status.st_ino) == (blocked_status.st_dev, blocked_status.st_ino):
            raise PermissionError("simulated browser-root enumeration failure")
        return real_scandir(path)

    monkeypatch.setattr(os, "scandir", unavailable)
    monkeypatch.setenv("HOME", os.fspath(helpers.home))
    monkeypatch.setenv("TMPDIR", os.fspath(helpers.temporary))
    main = _main(helpers, helper_name)

    assert main(list(arguments)) == 1
    captured = capfd.readouterr()

    assert captured.out == ""
    assert "unreadable" in captured.err


@pytest.mark.parametrize(
    ("helper_name", "leaf_name", "arguments"),
    [
        ("chrome-bookmarks.py", "Bookmarks", ()),
        ("chromium-pages.py", "History", ("--profiles",)),
    ],
)
@pytest.mark.parametrize("link_name", ["SingletonCookie", "SingletonLock", "SingletonSocket"])
def test_browser_helpers_ignore_non_profile_links_and_empty_roots(
    helpers: HelperInstallation,
    helper_name: str,
    leaf_name: str,
    arguments: tuple[str, ...],
    link_name: str,
) -> None:
    browser = helpers.home / ".config" / "chromium"
    browser.mkdir(parents=True)
    singleton_target = helpers.root / "singleton-target"
    singleton_target.write_text("browser-instance", encoding="utf-8")
    (browser / link_name).symlink_to(singleton_target)
    valid = helpers.home / ".config" / "google-chrome" / "Default" / leaf_name
    valid.parent.mkdir(parents=True)
    valid.write_text(json.dumps(_bookmark_document()) if leaf_name == "Bookmarks" else "", encoding="utf-8")

    result = helpers.run(helper_name, *arguments)

    assert result.returncode == 0, result.stderr.decode(errors="replace")


@pytest.mark.parametrize(
    ("helper_name", "arguments"),
    [
        ("chrome-bookmarks.py", ()),
        ("chromium-pages.py", ("--profiles",)),
    ],
)
def test_browser_helpers_do_not_query_link_target_metadata(
    helpers: HelperInstallation,
    monkeypatch: pytest.MonkeyPatch,
    capfd: pytest.CaptureFixture[str],
    helper_name: str,
    arguments: tuple[str, ...],
) -> None:
    outside = helpers.root / "outside-linked-profile"
    outside.mkdir()
    browser = helpers.home / ".config" / "chromium"
    browser.mkdir(parents=True)
    (browser / "Default").symlink_to(outside, target_is_directory=True)
    real_scandir = os.scandir

    class GuardedEntry:
        def __init__(self, entry: os.DirEntry[str]) -> None:
            self._entry = entry

        @property
        def name(self) -> str:
            return self._entry.name

        def stat(self, *, follow_symlinks: bool = True) -> os.stat_result:
            if follow_symlinks:
                raise AssertionError("linked target metadata was queried")
            return self._entry.stat(follow_symlinks=False)

        def is_dir(self, *, follow_symlinks: bool = True) -> bool:
            raise AssertionError(f"linked target metadata was queried: follow={follow_symlinks}")

    class GuardedScandir:
        def __init__(self, path: int | str | os.PathLike[str]) -> None:
            self._entries = real_scandir(path)

        def __enter__(self) -> GuardedScandir:
            return self

        def __exit__(self, *_arguments: object) -> None:
            self._entries.close()

        def __iter__(self) -> Any:
            return (GuardedEntry(entry) for entry in self._entries)

    monkeypatch.setattr(os, "scandir", GuardedScandir)
    monkeypatch.setenv("HOME", os.fspath(helpers.home))
    monkeypatch.setenv("TMPDIR", os.fspath(helpers.temporary))
    main = _main(helpers, helper_name)

    assert main(list(arguments)) == 1
    captured = capfd.readouterr()

    assert captured.out == ""
    assert "linked" in captured.err


def test_chromium_snapshot_stays_bound_during_a_profile_path_swap(
    helpers: HelperInstallation,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    original_cwd = Path.cwd()
    database = _write_history(helpers)
    outside_profile = helpers.root / "outside-profile"
    outside_profile.mkdir()
    outside_database = outside_profile / "History"
    outside_database.write_bytes(database.read_bytes())
    outside = sqlite3.connect(outside_database)
    outside.execute("update urls set url = 'https://outside.example.test/'")
    outside.commit()
    outside.close()
    writer = sqlite3.connect(database)
    assert writer.execute("pragma journal_mode = wal").fetchone()[0] == "wal"
    writer.execute("pragma wal_autocheckpoint = 0")
    writer.execute(
        "insert into urls values (?, ?, ?, ?, ?)",
        (2, "https://wal.example.test/committed", "WAL", 1, 0),
    )
    writer.commit()
    parked_profile = helpers.root / "parked-profile"
    main = _main(helpers, "chromium-pages.py")
    sqlite = main.__globals__["sqlite3"]
    real_connect = sqlite.connect
    swapped = False

    def swapping_connect(database_name: Any, *args: Any, **kwargs: Any) -> sqlite3.Connection:
        nonlocal swapped
        if isinstance(database_name, str) and "mode=ro" in database_name and not swapped:
            swapped = True
            database.parent.rename(parked_profile)
            database.parent.symlink_to(outside_profile, target_is_directory=True)
            try:
                return real_connect(database_name, *args, **kwargs)
            finally:
                database.parent.unlink()
                parked_profile.rename(database.parent)
        return real_connect(database_name, *args, **kwargs)

    monkeypatch.setattr(sqlite, "connect", swapping_connect)
    try:
        copied = main.__globals__["snapshot"](helpers.home, database, helpers.temporary / "snapshot.sqlite")
        try:
            assert copied.execute("select url from urls order by id").fetchall() == [
                ("https://example.test/path?private=yes#fragment",),
                ("https://wal.example.test/committed",),
            ]
        finally:
            copied.close()
    finally:
        writer.close()
    assert Path.cwd() == original_cwd
