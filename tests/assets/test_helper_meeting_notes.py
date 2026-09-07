"""Durable-calendar boundaries for the bundled meeting-notes helper."""

from __future__ import annotations

import json
import os
import runpy
from pathlib import Path
from types import FunctionType
from typing import Any

import pytest

from .conftest import HelperInstallation


def _calendar_path(base: Path) -> Path:
    return base / "events" / "2026-05-04" / "google-calendar-events.json"


def _write_calendar(base: Path) -> Path:
    calendar = _calendar_path(base)
    calendar.parent.mkdir(parents=True)
    calendar.write_text(
        json.dumps(
            {
                "fkf": 1,
                "source": "google-calendar-events",
                "fields": {"id": ".uid"},
                "records": [{"uid": "event-1"}, {"uid": "event-2"}],
            },
            separators=(",", ":"),
        ),
        encoding="utf-8",
    )
    return calendar


def _relations() -> list[dict[str, Any]]:
    prefix = "events/2026-05-04/google-calendar-events.json#"
    return [
        {"meeting_uris": [prefix + "event-1"]},
        {"meeting_uris": [prefix + "event-2"]},
    ]


def _verify(helpers: HelperInstallation) -> tuple[FunctionType, dict[str, Any]]:
    namespace = runpy.run_path(os.fspath(helpers.bin / "gws-meeting-notes-json.py"))
    verify = namespace["verify_relations"]
    assert isinstance(verify, FunctionType)
    return verify, namespace


def _main_with_fake_providers(
    helpers: HelperInstallation,
    monkeypatch: pytest.MonkeyPatch,
) -> tuple[FunctionType, dict[str, Any], list[list[str]]]:
    namespace = runpy.run_path(os.fspath(helpers.bin / "gws-meeting-notes-json.py"))
    main = namespace["main"]
    assert isinstance(main, FunctionType)
    calls: list[list[str]] = []

    def bounded(command: list[str]) -> bytes:
        calls.append(command)
        if any("gws-page-json.py" in argument for argument in command):
            return (
                b'{"files":[{"id":"doc-1","name":"Review - Notes by Gemini",'
                b'"createdTime":"2026-05-04T09:00:00Z","owners":[]}]}'
            )
        return b'[{"uid":"event-1","at":"2026-05-04T09:00:00Z","attachments":[{"fileId":"doc-1"}]}]'

    monkeypatch.setitem(main.__globals__, "bounded", bounded)
    return main, namespace, calls


def _arguments(base: Path) -> list[str]:
    return [
        "2026-05-04T00:00:00Z",
        "2026-05-05T00:00:00Z",
        "2026-05-04",
        "2026-05-05",
        "Fmind",
        os.fspath(base),
    ]


def test_meeting_relations_decode_each_durable_day_document_once(
    helpers: HelperInstallation,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    base = helpers.root / "base"
    _write_calendar(base)
    verify, namespace = _verify(helpers)
    loads = namespace["json"].loads
    calls = 0

    def counted_loads(value: bytes | bytearray | str) -> Any:
        nonlocal calls
        calls += 1
        return loads(value)

    monkeypatch.setattr(namespace["json"], "loads", counted_loads)

    verify(base, _relations())

    assert calls == 1


def test_meeting_relations_reject_oversize_calendar_without_whole_file_read(
    helpers: HelperInstallation,
    monkeypatch: pytest.MonkeyPatch,
    capsys: pytest.CaptureFixture[str],
) -> None:
    base = helpers.root / "base"
    calendar = _write_calendar(base)
    with calendar.open("r+b") as stream:
        stream.truncate((64 << 20) + 1)
    main, _namespace, calls = _main_with_fake_providers(helpers, monkeypatch)

    def reject_unbounded_read(_path: Path) -> bytes:
        raise AssertionError("meeting-notes must not materialize an unbounded calendar document")

    monkeypatch.setattr(Path, "read_bytes", reject_unbounded_read)

    assert main(_arguments(base)) == 1

    captured = capsys.readouterr()
    assert captured.out == ""
    assert "enable and sync google-calendar-events" in captured.err
    assert len(calls) == 2


def test_meeting_relations_reject_calendar_replaced_while_opening(
    helpers: HelperInstallation,
    monkeypatch: pytest.MonkeyPatch,
    capsys: pytest.CaptureFixture[str],
) -> None:
    base = helpers.root / "base"
    calendar = _write_calendar(base)
    replacement = helpers.root / "replacement.json"
    replacement.write_bytes(calendar.read_bytes())
    main, namespace, calls = _main_with_fake_providers(helpers, monkeypatch)
    open_file = namespace["os"].open
    swapped = False

    def swapping_open(path: str | os.PathLike[str], flags: int, *args: Any, **kwargs: Any) -> int:
        nonlocal swapped
        if Path(path) == calendar and not swapped:
            swapped = True
            replacement.replace(calendar)
        return open_file(path, flags, *args, **kwargs)

    monkeypatch.setattr(namespace["os"], "open", swapping_open)

    assert main(_arguments(base)) == 1

    assert swapped
    captured = capsys.readouterr()
    assert captured.out == ""
    assert "enable and sync google-calendar-events" in captured.err
    assert len(calls) == 2


def test_meeting_relations_reject_symlinked_calendar_without_output(
    helpers: HelperInstallation,
    monkeypatch: pytest.MonkeyPatch,
    capsys: pytest.CaptureFixture[str],
) -> None:
    base = helpers.root / "base"
    calendar = _write_calendar(base)
    outside = helpers.root / "outside.json"
    calendar.replace(outside)
    calendar.symlink_to(outside)
    main, _namespace, calls = _main_with_fake_providers(helpers, monkeypatch)

    assert main(_arguments(base)) == 1

    captured = capsys.readouterr()
    assert captured.out == ""
    assert "enable and sync google-calendar-events" in captured.err
    assert len(calls) == 2
