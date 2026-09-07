from __future__ import annotations

import base64
import json
from typing import Any

import pytest

import fkf.find as find_service
from fkf import mcp_paging
from fkf.base import Base
from fkf.find import FindFilter
from fkf.mcp_paging import PageCursor


def _token(value: object) -> str:
    encoded = json.dumps(value, separators=(",", ":")).encode()
    return base64.urlsafe_b64encode(encoded).rstrip(b"=").decode()


def test_page_cursor_is_restart_stable_and_binds_tool_query_and_snapshot() -> None:
    query = {"layer": "wiki", "limit": 10}
    first = PageCursor.open("", tool="list", query=query)
    snapshot = "a" * 64
    raw = first.next(snapshot, 10)

    continued = PageCursor.open(raw, tool="list", query=query)
    assert continued.continued
    assert continued.offset == 10
    continued.bind_snapshot(snapshot)
    with pytest.raises(ValueError, match="stale"):
        continued.bind_snapshot("b" * 64)
    with pytest.raises(ValueError, match="belongs to list"):
        PageCursor.open(raw, tool="read", query=query)
    with pytest.raises(ValueError, match="effective query"):
        PageCursor.open(raw, tool="list", query={"layer": "wiki", "limit": 5})


@pytest.mark.parametrize(
    "raw",
    [
        "%%%",
        "a" * 513,
        _token({"v": 2, "tool": "list", "query_sha256": "a" * 64, "snapshot_sha256": "b" * 64, "offset": 1}),
        _token({"v": 1, "tool": "list", "query_sha256": "no", "snapshot_sha256": "b" * 64, "offset": 1}),
        _token({"v": 1, "tool": "list", "query_sha256": "a" * 64, "snapshot_sha256": "b" * 64, "offset": 0}),
        _token(
            {
                "v": 1,
                "tool": "list",
                "query_sha256": "a" * 64,
                "snapshot_sha256": "b" * 64,
                "offset": 1,
                "extra": True,
            }
        ),
    ],
)
def test_page_cursor_rejects_malformed_or_unpublished_shapes(raw: str) -> None:
    with pytest.raises(ValueError, match="invalid cursor"):
        PageCursor.open(raw, tool="list", query={"limit": 1})


def test_find_page_uses_the_bounded_scan_without_materializing_exhaustive_find(
    populated_base: Base,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    retained: list[tuple[int, int]] = []
    original = find_service._retain_bounded  # noqa: SLF001

    def observe(*args: Any, **kwargs: Any) -> list[Any]:
        values = original(*args, **kwargs)
        retained.append((args[2], len(values)))
        return values

    def unexpected(*_args: object, **_kwargs: object) -> None:
        raise AssertionError("MCP find called the exhaustive materializing find() path")

    monkeypatch.setattr(find_service, "_retain_bounded", observe)
    monkeypatch.setattr(mcp_paging, "find", unexpected, raising=False)

    page, items = mcp_paging.find_page(
        populated_base,
        FindFilter(grep=("Needle",)),
        counting=False,
        limit=1,
        cursor="",
        query={"grep": ["Needle"], "limit": 1},
    )

    assert items == 1
    assert isinstance(page, dict)
    assert page["next_cursor"]
    assert retained
    assert max(length for _capacity, length in retained) == 2
    assert all(capacity == 2 and length <= capacity for capacity, length in retained)
