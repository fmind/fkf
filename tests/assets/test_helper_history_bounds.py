"""Local Git history discovery and output fail closed at finite bounds."""

from __future__ import annotations

import json
import os
import runpy
from typing import Any

import pytest

from .conftest import HelperInstallation


def _module(helpers: HelperInstallation) -> dict[str, Any]:
    return runpy.run_path(os.fspath(helpers.bin / "git-log-json.py"))


def _record() -> dict[str, object]:
    return {
        "hash": "a" * 40,
        "author_email": "author@example.test",
        "message": "bounded history",
        "repo_full": "acme/project",
        "time": "2026-05-04T00:00:00Z",
        "uid": f"acme/project@{'a' * 40}",
        "repository_uri": "repo:github.com/acme/project",
        "participant_uris": ["person:email/author@example.test"],
    }


def test_git_marker_discovery_propagates_scandir_errors(
    helpers: HelperInstallation,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    namespace = _module(helpers)
    markers = namespace["markers"]

    def denied(_path: object) -> object:
        raise PermissionError("denied")

    monkeypatch.setattr(markers.__globals__["os"], "scandir", denied)
    with pytest.raises(PermissionError, match="denied"):
        markers(helpers.home)


def test_git_marker_discovery_bounds_entries_and_repositories(
    helpers: HelperInstallation,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    namespace = _module(helpers)
    markers = namespace["markers"]
    (helpers.home / "one").mkdir()
    (helpers.home / "two").mkdir()
    monkeypatch.setitem(markers.__globals__, "MAX_WALK_ENTRIES", 1)
    with pytest.raises(RuntimeError, match="filesystem-entry bound"):
        markers(helpers.home)

    (helpers.home / "one" / ".git").mkdir()
    (helpers.home / "two" / ".git").mkdir()
    monkeypatch.setitem(markers.__globals__, "MAX_WALK_ENTRIES", 10)
    monkeypatch.setitem(markers.__globals__, "MAX_REPOSITORIES", 1)
    with pytest.raises(RuntimeError, match="repository bound"):
        markers(helpers.home)


def test_git_main_enforces_record_and_exact_output_bounds(
    helpers: HelperInstallation,
    monkeypatch: pytest.MonkeyPatch,
    capfd: pytest.CaptureFixture[str],
) -> None:
    namespace = _module(helpers)
    main = namespace["main"]
    record = _record()
    marker = helpers.home / "repo" / ".git"
    marker.mkdir(parents=True)
    monkeypatch.setitem(main.__globals__, "git_epoch", lambda _base, option, _value: 1 if option == "since" else 2)
    monkeypatch.setitem(main.__globals__, "markers", lambda _root: [marker])
    monkeypatch.setitem(main.__globals__, "invoke", lambda _arguments, **_keywords: os.fsencode(marker) + b"\n")
    monkeypatch.setitem(
        main.__globals__,
        "repository_identity",
        lambda _gitdir, _script_base: ("acme/project", "acme/project"),
    )
    monkeypatch.setitem(main.__globals__, "log_records", lambda *_arguments: iter((record,)))
    arguments = ["2026-05-04", "2026-05-05", os.fspath(helpers.home), "author@example.test"]

    monkeypatch.setitem(main.__globals__, "MAX_RECORDS", 0)
    assert main(arguments) == 1
    captured = capfd.readouterr()
    assert captured.out == ""
    assert "record bound" in captured.err

    expected = json.dumps([record], ensure_ascii=False, separators=(",", ":")) + "\n"
    monkeypatch.setitem(main.__globals__, "MAX_RECORDS", 1)
    monkeypatch.setitem(main.__globals__, "MAX_OUTPUT_BYTES", len(expected.encode()))
    assert main(arguments) == 0
    assert capfd.readouterr().out == expected

    monkeypatch.setitem(main.__globals__, "MAX_OUTPUT_BYTES", len(expected.encode()) - 1)
    assert main(arguments) == 1
    captured = capfd.readouterr()
    assert captured.out == ""
    assert "output bound" in captured.err
