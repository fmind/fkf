"""Residual provider helpers bound page materialization and final output."""

from __future__ import annotations

import io
import json
import os
import runpy
import sys
from datetime import UTC, datetime, time, timedelta
from pathlib import Path
from types import SimpleNamespace
from typing import Any

import pytest

from .conftest import HelperInstallation


def _module(helpers: HelperInstallation, name: str) -> dict[str, Any]:
    return runpy.run_path(os.fspath(helpers.bin / name))


class _KaggleApi:
    def __init__(self, kernels: object) -> None:
        self._kernels = kernels

    def authenticate(self) -> None:
        return None

    def kernels_list_with_response(self, **_arguments: object) -> object:
        return SimpleNamespace(kernels=self._kernels)


def _kernel(reference: str) -> object:
    return SimpleNamespace(
        ref=reference,
        title="Kernel",
        author="owner",
        last_run_time=None,
        total_votes=0,
    )


def _install_kaggle_api(
    namespace: dict[str, Any],
    monkeypatch: pytest.MonkeyPatch,
    kernels: object,
) -> None:
    module = SimpleNamespace(KaggleApi=lambda: _KaggleApi(kernels))
    monkeypatch.setattr(namespace["importlib"], "import_module", lambda _name: module)


def test_kaggle_kernels_reads_only_the_page_size_sentinel(
    helpers: HelperInstallation,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    namespace = _module(helpers, "kaggle-kernels-json.py")

    def kernels() -> object:
        yield _kernel("owner/one")
        yield _kernel("owner/two")
        raise AssertionError("the helper read beyond the page-size sentinel")

    _install_kaggle_api(namespace, monkeypatch, kernels())
    provider_records = namespace["provider_records"]
    monkeypatch.setitem(provider_records.__globals__, "PAGE_SIZE", 1)

    with pytest.raises(namespace["InvariantError"], match="page-size-exceeded"):
        provider_records()


def test_kaggle_kernels_bounds_records_and_exact_output(
    helpers: HelperInstallation,
    monkeypatch: pytest.MonkeyPatch,
    capfd: pytest.CaptureFixture[str],
) -> None:
    namespace = _module(helpers, "kaggle-kernels-json.py")
    main = namespace["main"]
    _install_kaggle_api(namespace, monkeypatch, [_kernel("owner/one")])
    monkeypatch.setitem(main.__globals__, "kaggle_interpreter", lambda: Path(sys.executable))
    monkeypatch.setitem(main.__globals__, "MAX_RECORDS", 0)

    assert main([]) == 1
    captured = capfd.readouterr()
    assert captured.out == ""
    assert "record-bound-exceeded" in captured.err

    monkeypatch.setitem(main.__globals__, "MAX_RECORDS", 1)
    monkeypatch.setitem(main.__globals__, "MAX_OUTPUT_BYTES", 64 << 20)
    assert main([]) == 0
    expected = capfd.readouterr().out
    assert expected

    monkeypatch.setitem(main.__globals__, "MAX_OUTPUT_BYTES", len(expected.encode()))
    assert main([]) == 0
    assert capfd.readouterr().out == expected

    monkeypatch.setitem(main.__globals__, "MAX_OUTPUT_BYTES", len(expected.encode()) - 1)
    assert main([]) == 1
    captured = capfd.readouterr()
    assert captured.out == ""
    assert "output-bound-exceeded" in captured.err


def test_gws_page_bounds_exact_newline_inclusive_projected_output(
    helpers: HelperInstallation,
    monkeypatch: pytest.MonkeyPatch,
    capfd: pytest.CaptureFixture[str],
) -> None:
    namespace = _module(helpers, "gws-page-json.py")
    main = namespace["main"]
    payload = b'{"items":[{"id":"one"}]}\n'

    def invoke(limit: int) -> tuple[int, str, str]:
        stdin = io.TextIOWrapper(io.BytesIO(payload), encoding="utf-8")
        monkeypatch.setattr(sys, "stdin", stdin)
        monkeypatch.setitem(main.__globals__, "MAX_OUTPUT_BYTES", limit)
        status = main(["items"])
        captured = capfd.readouterr()
        return status, captured.out, captured.err

    status, expected, _ = invoke(64 << 20)
    assert status == 0
    assert expected

    status, output, _ = invoke(len(expected.encode()))
    assert status == 0
    assert output == expected

    status, output, error = invoke(len(expected.encode()) - 1)
    assert status == 1
    assert output == ""
    assert "output bound" in error


class _Process:
    def __init__(self, payload: bytes) -> None:
        self.stdout = io.BytesIO(payload)

    def __enter__(self) -> _Process:
        return self

    def __exit__(self, *_arguments: object) -> None:
        return None

    def kill(self) -> None:
        return None

    def wait(self) -> int:
        return 0


def _fixed_provider_main(
    namespace: dict[str, Any],
    name: str,
    monkeypatch: pytest.MonkeyPatch,
) -> tuple[Any, list[str]]:
    main = namespace["main"]
    if name == "github-events-json.py":
        start_at = datetime.combine((datetime.now(UTC) - timedelta(days=2)).date(), time(), tzinfo=UTC)
        event_at = (start_at + timedelta(hours=1)).isoformat().replace("+00:00", "Z")
        event = {
            "id": "event-1",
            "type": "PushEvent",
            "created_at": event_at,
            "public": True,
            "actor": {"login": "fmind"},
            "repo": {"name": "fmind/fkf"},
            "org": None,
        }

        def invoke(arguments: list[str]) -> bytes:
            return b"fmind\n" if "/user" in arguments else json.dumps([event], separators=(",", ":")).encode()

        monkeypatch.setitem(main.__globals__, "invoke", invoke)
        return main, [
            start_at.isoformat().replace("+00:00", "Z"),
            (start_at + timedelta(days=1)).isoformat().replace("+00:00", "Z"),
        ]
    if name == "gcloud-audit-json.py":
        payload = [
            {
                "insertId": "entry-1",
                "timestamp": "2026-05-04T01:00:00Z",
                "resource": {"labels": {"project_id": "project"}},
                "protoPayload": {},
            }
        ]
        monkeypatch.setitem(main.__globals__, "run", lambda _arguments: json.dumps(payload).encode())
        return main, ["2026-05-04T00:00:00Z", "2026-05-05T00:00:00Z"]
    if name == "huggingface-repositories-json.py":
        payload = b'[{"id":"owner/model","type":"model","updated":"2026-05-04","visibility":"private"}]'
        monkeypatch.setattr(namespace["subprocess"], "Popen", lambda *_args, **_kwargs: _Process(payload))
        return main, []
    payload = b'[{"key":"TEAM-1","summary":"Issue","url":"https://team.atlassian.net/browse/TEAM-1"}]'
    monkeypatch.setattr(namespace["subprocess"], "Popen", lambda *_args, **_kwargs: _Process(payload))
    return main, ["team.atlassian.net", "TEAM", "statusCategory != Done"]


@pytest.mark.parametrize(
    "name",
    [
        "github-events-json.py",
        "gcloud-audit-json.py",
        "huggingface-repositories-json.py",
        "jira-issues-json.py",
    ],
)
def test_fixed_provider_helpers_bound_exact_newline_inclusive_output(
    helpers: HelperInstallation,
    monkeypatch: pytest.MonkeyPatch,
    capfd: pytest.CaptureFixture[str],
    name: str,
) -> None:
    namespace = _module(helpers, name)
    main, arguments = _fixed_provider_main(namespace, name, monkeypatch)
    monkeypatch.setitem(main.__globals__, "MAX_OUTPUT_BYTES", 64 << 20)
    assert main(arguments) == 0
    expected = capfd.readouterr().out
    assert expected

    monkeypatch.setitem(main.__globals__, "MAX_OUTPUT_BYTES", len(expected.encode()))
    assert main(arguments) == 0
    assert capfd.readouterr().out == expected

    monkeypatch.setitem(main.__globals__, "MAX_OUTPUT_BYTES", len(expected.encode()) - 1)
    assert main(arguments) == 1
    captured = capfd.readouterr()
    assert captured.out == ""
    assert captured.err
