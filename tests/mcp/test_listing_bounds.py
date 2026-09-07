"""MCP listing pagination remains complete only within explicit scan ceilings."""

from __future__ import annotations

import os
from pathlib import Path

import pytest

from fkf import mcp_server
from fkf.base import Base
from fkf.jsoncodec import dumps
from fkf.pages import PageFilter, list_pages
from fkf.store import Layer


def _list(base: Base, layer: str, *, cursor: str = "") -> tuple[object, int]:
    return mcp_server._list_result(  # noqa: SLF001 - exercise the MCP-only bounded adapter.
        base,
        layer=layer,
        since="",
        until="",
        source="",
        tag=(),
        status="",
        page_type="",
        limit=1,
        cursor=cursor,
        cancel=None,
    )


def _seed_entries(base: Base, layer: str) -> None:
    directory = base.store.directory(Layer(layer))
    directory.mkdir(parents=True, exist_ok=True)
    for index in range(3):
        if layer in {"events", "tasks"}:
            (directory / f"2026-09-0{index + 1}").mkdir()
        elif layer == "index":
            (directory / f"source-{index}.json").write_text("{}\n", encoding="utf-8")
        else:
            (directory / f"page-{index}.md").write_text(f"# Page {index}\n", encoding="utf-8")


def _hide_file_size(monkeypatch: pytest.MonkeyPatch, target: Path) -> None:
    """Model a path-stat result made stale before the bounded file read."""
    original = Path.stat

    def stale_stat(path: Path, *, follow_symlinks: bool = True) -> os.stat_result:
        result = original(path, follow_symlinks=follow_symlinks)
        if path == target:
            fields = list(result)
            fields[6] = 0
            return os.stat_result(fields)
        return result

    monkeypatch.setattr(Path, "stat", stale_stat)


@pytest.mark.parametrize("layer", ["events", "index", "tasks", "wiki"])
def test_mcp_list_refuses_each_layer_after_the_filesystem_scan_ceiling(
    base: Base,
    monkeypatch: pytest.MonkeyPatch,
    layer: str,
) -> None:
    _seed_entries(base, layer)
    monkeypatch.setattr(mcp_server, "MAX_MCP_SCAN_ENTRIES", 2, raising=False)

    with pytest.raises(ValueError, match=r"MCP-only scan ceiling.*CLI"):
        _list(base, layer)


def test_mcp_page_listing_accepts_the_exact_source_byte_ceiling_and_rejects_one_byte_over(
    base: Base,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    directory = base.store.directory(Layer.WIKI)
    directory.mkdir(parents=True)
    content = b"---\ntitle: Bounded page\ntags: [bounded]\n---\n\n# Bounded page\n\nBody.\n"
    (directory / "bounded.md").write_bytes(content)
    monkeypatch.setattr(mcp_server, "MAX_MCP_SCAN_BYTES", len(content), raising=False)

    exact, items = _list(base, "wiki")

    assert items == 1
    assert isinstance(exact, dict)
    assert exact["pages"][0]["uri"] == "wiki/bounded.md"

    monkeypatch.setattr(mcp_server, "MAX_MCP_SCAN_BYTES", len(content) - 1, raising=False)
    with pytest.raises(ValueError, match=r"MCP-only scan ceiling.*source bytes"):
        _list(base, "wiki")


def test_mcp_page_listing_charges_the_bytes_read_not_a_stale_path_stat(
    base: Base,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    directory = base.store.directory(Layer.WIKI)
    directory.mkdir(parents=True)
    page = directory / "bounded.md"
    content = b"# Bounded page\n\nBody.\n"
    page.write_bytes(content)
    _hide_file_size(monkeypatch, page)
    monkeypatch.setattr(mcp_server, "MAX_MCP_SCAN_BYTES", len(content) - 1, raising=False)

    with pytest.raises(ValueError, match=r"MCP-only scan ceiling.*source bytes"):
        _list(base, "wiki")


def test_mcp_event_listing_charges_the_document_bytes_read_not_a_stale_path_stat(
    populated_base: Base,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    document = populated_base.root / "events" / "2026-09-05" / "synthetic.json"
    size = document.stat().st_size
    _hide_file_size(monkeypatch, document)
    monkeypatch.setattr(mcp_server, "MAX_MCP_SCAN_BYTES", size - 1, raising=False)

    with pytest.raises(ValueError, match=r"MCP-only scan ceiling.*source bytes"):
        _list(populated_base, "events")


def test_mcp_directory_read_refuses_an_oversized_complete_item_snapshot(
    base: Base,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    _seed_entries(base, "wiki")
    monkeypatch.setattr(mcp_server, "MAX_MCP_SCAN_ITEMS", 2, raising=False)

    with pytest.raises(ValueError, match=r"MCP-only scan ceiling.*items.*child directory"):
        mcp_server._read_result(  # noqa: SLF001 - exercise the MCP-only bounded adapter.
            base,
            uri="wiki/",
            cursor="",
            cancel=None,
        )


def test_mcp_tag_resource_charges_returned_tags_as_retained_items(
    base: Base,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    directory = base.store.directory(Layer.WIKI)
    directory.mkdir(parents=True)
    (directory / "tagged.md").write_text("---\ntags: [alpha, beta]\n---\n# Tagged\n", encoding="utf-8")
    monkeypatch.setattr(mcp_server, "MAX_MCP_SCAN_ITEMS", 1, raising=False)

    with pytest.raises(ValueError, match=r"MCP-only scan ceiling.*items"):
        mcp_server._tag_resource(base, Layer.WIKI, None)  # noqa: SLF001


def test_mcp_listing_accepts_the_exact_complete_snapshot_ceiling_and_rejects_one_byte_over(
    base: Base,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    _seed_entries(base, "wiki")
    listing = list_pages(base, Layer.WIKI, PageFilter(), metadata_only=True)
    encoded_size = len(dumps(listing))
    monkeypatch.setattr(mcp_server, "MAX_MCP_SNAPSHOT_BYTES", encoded_size)

    _list(base, "wiki")

    monkeypatch.setattr(mcp_server, "MAX_MCP_SNAPSHOT_BYTES", encoded_size - 1)
    with pytest.raises(ValueError, match=r"MCP-only scan ceiling.*complete-result snapshot"):
        _list(base, "wiki")


def test_admitted_directory_read_keeps_opaque_cursor_and_wire_shape(
    base: Base,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    _seed_entries(base, "wiki")
    monkeypatch.setattr(mcp_server, "PAGE_SIZE", 2)

    first, first_items, _generation = mcp_server._read_result(  # noqa: SLF001
        base,
        uri="wiki/",
        cursor="",
        cancel=None,
    )

    assert first_items == 2
    assert isinstance(first, dict)
    assert first["entries"] == ["wiki/page-0.md", "wiki/page-1.md"]
    cursor = first["next_cursor"]
    assert isinstance(cursor, str)

    second, second_items, _generation = mcp_server._read_result(  # noqa: SLF001
        base,
        uri="wiki/",
        cursor=cursor,
        cancel=None,
    )

    assert second_items == 1
    assert isinstance(second, dict)
    assert second == {"uri": "wiki/", "kind": "directory", "entries": ["wiki/page-2.md"]}


def test_mcp_page_listing_strips_each_body_before_retaining_the_collection(
    base: Base,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    _seed_entries(base, "wiki")
    retained_bodies: list[str] = []
    original_guard = mcp_server._MCPScanGuard  # noqa: SLF001

    class ObservedGuard(original_guard):
        def retain(self, value: object) -> None:
            body = getattr(value, "body", "")
            if isinstance(body, str):
                retained_bodies.append(body)
            super().retain(value)

    monkeypatch.setattr(mcp_server, "_MCPScanGuard", ObservedGuard)

    _list(base, "wiki")

    assert retained_bodies == ["", "", ""]
