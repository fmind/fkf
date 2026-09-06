from __future__ import annotations

import json
import logging
import time
from datetime import UTC
from threading import Event
from typing import Any, cast

import anyio
import pytest
from mcp.client import Client
from mcp.shared.exceptions import MCPError
from mcp_types import CallToolResult, JSONRPCResponse, TextContent, TextResourceContents

from fkf import mcp_server
from fkf.base import Base
from fkf.config import load_config
from fkf.documents import build_document, day_window, parse_day_in_location
from fkf.errors import CanceledError
from fkf.graph import build_graph
from fkf.mcp_server import (
    GRAPH_GENERATION_META_KEY,
    MAX_RESPONSE_BYTES,
    PAGE_SIZE,
    RESULT_SIZE_META_KEY,
    UNTRUSTED_EVIDENCE_NOTICE,
    create_server,
)
from fkf.source_runtime import Environment
from fkf.store import LAYERS, Store


def _payload(result: CallToolResult) -> dict[str, Any]:
    assert not result.is_error
    assert len(result.content) == 1
    content = result.content[0]
    assert isinstance(content, TextContent)
    decoded = json.loads(content.text)
    assert decoded == result.structured_content
    assert isinstance(decoded, dict)
    assert result.meta is not None
    size = result.meta[RESULT_SIZE_META_KEY]
    assert isinstance(size, dict)
    assert size["bytes"] <= size["maxBytes"] == MAX_RESPONSE_BYTES
    return decoded


def test_server_publishes_the_exact_read_only_surface(base: Base) -> None:
    async def exercise() -> None:
        server = create_server(base)
        async with Client(server) as client:
            tools = (await client.list_tools()).tools
            resources = (await client.list_resources()).resources
            invalid = await client.call_tool("context", {"query": "x" * 4097})
            minimal_arguments = {
                "find": {},
                "context": {"query": "x"},
                "day": {},
                "timeline": {},
                "list": {"layer": "wiki"},
                "read": {"uri": "wiki/index.md"},
                "graph": {"uri": "wiki/index.md"},
            }
            unknown_results = {
                name: await client.call_tool(name, {**arguments, "undeclared_argument": "private-value"})
                for name, arguments in minimal_arguments.items()
            }

        assert [tool.name for tool in tools] == ["find", "context", "day", "timeline", "list", "read", "graph"]
        schemas = {tool.name: cast(dict[str, dict[str, Any]], tool.input_schema["properties"]) for tool in tools}
        assert schemas["context"]["budget"]["default"] == 4096
        assert schemas["day"]["budget"]["default"] == 600
        assert schemas["timeline"]["budget"]["default"] == 600
        assert schemas["graph"]["depth"]["default"] == 1
        assert {str(resource.uri) for resource in resources} == {
            "fkf://brain/wiki/index",
            "fkf://brain/wiki/tags",
            "fkf://brain/projects",
            "fkf://brain/status",
        }
        for tool in tools:
            assert tool.input_schema["additionalProperties"] is False
            assert tool.description is not None
            assert "Default:" in tool.description
            assert "Example:" in tool.description
            assert tool.annotations is not None
            assert tool.annotations.read_only_hint
            assert tool.annotations.idempotent_hint
            assert tool.annotations.destructive_hint is False
            assert tool.annotations.open_world_hint is False
            assert tool.meta is not None
            assert tool.meta[RESULT_SIZE_META_KEY]["maxBytes"] == MAX_RESPONSE_BYTES
            encoded_schema = json.dumps(tool.input_schema).lower()
            for forbidden in ('"body"', '"sync"', '"write"', '"trust"', '"shell"', '"exec"', '"command"'):
                assert forbidden not in encoded_schema
            properties = cast(dict[str, dict[str, Any]], tool.input_schema["properties"])
            for property_schema in properties.values():
                assert property_schema.get("description")
                if "default" in property_schema and "minimum" in property_schema:
                    assert property_schema["default"] >= property_schema["minimum"]
                if property_schema.get("type") == "string":
                    assert property_schema.get("maxLength", 0) > 0
                if property_schema.get("type") == "array":
                    assert property_schema.get("maxItems") == 64
                    assert property_schema["items"].get("maxLength", 0) > 0
            assert ("cursor" in properties) is (tool.name in {"find", "list", "read", "graph"})
        assert invalid.is_error
        assert isinstance(invalid.structured_content, dict)
        assert "string_too_long" in invalid.structured_content["error"]
        assert str(base.root) not in invalid.structured_content["error"]
        assert invalid.meta is not None
        assert invalid.meta[RESULT_SIZE_META_KEY]["bytes"] <= MAX_RESPONSE_BYTES
        for unknown in unknown_results.values():
            assert unknown.is_error
            assert isinstance(unknown.structured_content, dict)
            assert "unexpected argument" in unknown.structured_content["error"]
            assert "undeclared_argument" not in unknown.structured_content["error"]
            assert "private-value" not in unknown.structured_content["error"]
        assert schemas["find"]["since"]["description"] == schemas["context"]["since"]["description"]
        assert schemas["find"]["until"]["description"] == schemas["context"]["until"]["description"]
        assert PAGE_SIZE == 100
        assert server.instructions is not None
        assert UNTRUSTED_EVIDENCE_NOTICE in server.instructions
        assert str(base.root) not in server.instructions
        assert len(server.instructions.encode()) <= 4096

    anyio.run(exercise)


def test_layer_resources_are_omitted_when_their_layers_are_disabled(base: Base) -> None:
    disabled = Base(
        config=base.config,
        store=Store(base.root, dict.fromkeys(LAYERS, False)),
        environment=base.environment,
        runner=base.runner,
        now=base.now,
        origin=base.origin,
    )

    async def exercise() -> None:
        async with Client(create_server(disabled)) as client:
            resources = await client.list_resources()
        assert [str(resource.uri) for resource in resources.resources] == ["fkf://brain/status"]

    anyio.run(exercise)


def test_all_tools_answer_offline_with_identical_compact_json(populated_base: Base) -> None:
    async def exercise() -> None:
        server = create_server(populated_base)
        async with Client(server) as client:
            calls = {
                "find": {"grep": ["Needle"], "limit": 10},
                "context": {"query": "Needle", "budget": 900, "expand": True},
                "day": {"date": "2026-09-05", "budget": 600},
                "timeline": {"since": "2026-09-05", "until": "2026-09-05", "budget": 600},
                "list": {"layer": "wiki"},
                "read": {"uri": "wiki/needle.md"},
                "graph": {"uri": "repo:github.com/fmind/fkf", "direction": "in"},
            }
            results = {name: await client.call_tool(name, arguments) for name, arguments in calls.items()}
            results["entity"] = await client.call_tool("read", {"uri": "repo:github.com/fmind/fkf"})

        payloads = {name: _payload(result) for name, result in results.items()}
        assert payloads["find"]["records"][0]["uri"].endswith("synthetic.json#a1")
        assert payloads["context"]["receipt"]["input_digest"]
        assert payloads["day"]["groups"][0]["items"][0]["title"] == "Needle record"
        assert payloads["timeline"]["receipt"]["records"] == 1
        assert payloads["list"]["total"] == 2
        assert payloads["read"]["page"]["title"] == "Needle decision"
        assert payloads["graph"]["edges"][0]["dst"] == "repo:github.com/fmind/fkf"
        for name in ("context", "entity", "graph"):
            meta = results[name].meta
            assert meta is not None
            assert len(str(meta.get(GRAPH_GENERATION_META_KEY, ""))) == 64
            assert "ttlMs" not in meta
        assert cast(Any, populated_base.runner).calls == 0

    anyio.run(exercise)


def test_valid_schema_defaults_match_omitted_tool_semantics(populated_base: Base) -> None:
    async def exercise() -> None:
        calls = {
            "context": ({"query": "Needle"}, {"query": "Needle", "budget": 4096}),
            "day": ({"date": "2026-09-05"}, {"date": "2026-09-05", "budget": 600}),
            "timeline": (
                {"since": "2026-09-05", "until": "2026-09-05"},
                {"since": "2026-09-05", "until": "2026-09-05", "budget": 600},
            ),
            "graph": (
                {"uri": "repo:github.com/fmind/fkf", "direction": "in"},
                {"uri": "repo:github.com/fmind/fkf", "direction": "in", "depth": 1},
            ),
        }
        async with Client(create_server(populated_base)) as client:
            for name, (omitted, explicit) in calls.items():
                default_result = await client.call_tool(name, omitted)
                explicit_result = await client.call_tool(name, explicit)
                assert not default_result.is_error
                assert default_result.structured_content == explicit_result.structured_content

    anyio.run(exercise)


def test_direct_tool_call_fails_closed_when_the_contract_registry_drifts(
    base: Base,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    server = create_server(base)
    monkeypatch.delitem(
        mcp_server._TOOL_ARGUMENT_DESCRIPTIONS,  # noqa: SLF001 - exact drift shield for the private registry
        "read",
    )

    async def exercise() -> None:
        with pytest.raises(RuntimeError, match="argument contract is out of sync"):
            await server.call_tool("read", {"uri": "wiki/index.md"})

    anyio.run(exercise)


def test_mcp_logs_accounting_fields_without_copying_evidence(
    populated_base: Base,
    caplog: pytest.LogCaptureFixture,
) -> None:
    async def exercise() -> None:
        async with Client(create_server(populated_base)) as client:
            result = await client.call_tool("find", {"grep": ["Needle"], "limit": 10})
        assert not result.is_error

    with caplog.at_level(logging.INFO, logger="fkf.mcp_server"):
        anyio.run(exercise)

    records = [record for record in caplog.records if record.getMessage() == "fkf mcp call"]
    assert len(records) == 1
    record = records[0]
    assert record.__dict__["tool"] == "find"
    assert record.__dict__["base"] == "brain"
    assert record.__dict__["items"] > 0
    assert record.__dict__["bytes"] > 0
    assert record.__dict__["elapsed_ms"] >= 0
    assert len(record.__dict__["input_digest"]) == 12
    logged = f"{caplog.text} {record.__dict__}"
    for evidence in ("Needle record", "repo:github.com/fmind/fkf", "Durable body"):
        assert evidence not in logged


def test_failed_mcp_call_logs_only_a_bounded_error_class(
    populated_base: Base,
    caplog: pytest.LogCaptureFixture,
) -> None:
    async def exercise() -> None:
        async with Client(create_server(populated_base)) as client:
            result = await client.call_tool("read", {"uri": "../../Needle record"})
        assert result.is_error

    with caplog.at_level(logging.INFO, logger="fkf.mcp_server"):
        anyio.run(exercise)

    records = [record for record in caplog.records if record.getMessage() == "fkf mcp call failed"]
    assert len(records) == 1
    record = records[0]
    assert record.__dict__["tool"] == "read"
    assert record.__dict__["error"] == "path-escapes"
    assert "bytes" not in record.__dict__
    assert "Needle record" not in f"{caplog.text} {record.__dict__}"


def test_in_flight_handler_receives_the_server_event_and_cancels_promptly(
    populated_base: Base,
    monkeypatch: pytest.MonkeyPatch,
    caplog: pytest.LogCaptureFixture,
) -> None:
    cancel = Event()
    entered = Event()

    def blocked_context(_base: Base, _request: object, *, cancel: object = None) -> object:
        assert cancel is not None
        assert cancel is server_cancel
        entered.set()
        assert server_cancel.wait(timeout=1)
        raise CanceledError(f"operation canceled under {populated_base.root}/private-evidence")

    server_cancel = cancel
    monkeypatch.setattr("fkf.context.build_context", blocked_context)

    async def exercise() -> tuple[CallToolResult, float]:
        async def stop() -> None:
            observed = await anyio.to_thread.run_sync(entered.wait, 1)
            assert observed
            cancel.set()

        async with Client(create_server(populated_base, cancel=cancel)) as client:
            started = time.monotonic()
            async with anyio.create_task_group() as tasks:
                tasks.start_soon(stop)
                result = await client.call_tool("context", {"query": "Needle", "budget": 900})
            return result, time.monotonic() - started

    with caplog.at_level(logging.INFO, logger="fkf.mcp_server"):
        result, elapsed = anyio.run(exercise)

    assert elapsed < 1
    assert result.is_error
    assert isinstance(result.structured_content, dict)
    error = str(result.structured_content["error"])
    assert str(populated_base.root) not in error
    assert "./private-evidence" in error
    assert result.meta is not None
    assert "io.github.fmind/private-error-class" not in result.meta
    assert result.meta[RESULT_SIZE_META_KEY]["bytes"] <= MAX_RESPONSE_BYTES
    records = [record for record in caplog.records if record.getMessage() == "fkf mcp call failed"]
    assert len(records) == 1
    assert records[0].__dict__["error"] == "cancelled"
    assert "private-evidence" not in f"{caplog.text} {records[0].__dict__}"


def test_every_tool_and_resource_honors_the_shared_server_cancellation(populated_base: Base) -> None:
    cancel = Event()
    cancel.set()

    async def exercise() -> None:
        calls = {
            "find": {"grep": ["Needle"]},
            "context": {"query": "Needle"},
            "day": {"date": "2026-09-05"},
            "timeline": {"since": "2026-09-05", "until": "2026-09-05"},
            "list": {"layer": "wiki"},
            "read": {"uri": "wiki/needle.md"},
            "graph": {"uri": "repo:github.com/fmind/fkf"},
        }
        async with Client(create_server(populated_base, cancel=cancel)) as client:
            results = [await client.call_tool(name, arguments) for name, arguments in calls.items()]
            for uri in (
                "fkf://brain/wiki/index",
                "fkf://brain/wiki/tags",
                "fkf://brain/projects",
                "fkf://brain/status",
            ):
                with pytest.raises(MCPError, match="canceled"):
                    await client.read_resource(uri, cache_mode="bypass")

        for result in results:
            assert result.is_error
            assert isinstance(result.structured_content, dict)
            assert result.structured_content["error"] in {"command canceled", "operation canceled"}
            assert result.meta is not None
            assert "io.github.fmind/private-error-class" not in result.meta

    anyio.run(exercise)


def test_find_and_list_cursors_are_exhaustive_and_snapshot_bound(populated_base: Base) -> None:
    async def exercise() -> None:
        server = create_server(populated_base)
        async with Client(server) as client:
            found: list[str] = []
            cursor = ""
            first_find_cursor = ""
            while True:
                arguments: dict[str, object] = {"grep": ["Needle"], "limit": 1}
                if cursor:
                    arguments["cursor"] = cursor
                page = _payload(await client.call_tool("find", arguments))
                found.extend(str(item["uri"]) for item in page.get("pages", []))
                found.extend(str(item["uri"]) for item in page.get("records", []))
                cursor = str(page.get("next_cursor", ""))
                first_find_cursor = first_find_cursor or cursor
                if not cursor:
                    break
            assert found == [
                "wiki/needle.md",
                "projects/fkf.md",
                "wiki/index.md",
                "events/2026-09-05/synthetic.json#a1",
            ]

            first = _payload(await client.call_tool("list", {"layer": "wiki", "limit": 1}))
            list_cursor = str(first["next_cursor"])
            second = _payload(await client.call_tool("list", {"layer": "wiki", "limit": 1, "cursor": list_cursor}))
            assert [page["uri"] for page in first["pages"] + second["pages"]] == [
                "wiki/index.md",
                "wiki/needle.md",
            ]

            changed = populated_base.root / "wiki" / "needle.md"
            changed.write_text(changed.read_text() + "\nNeedle changed.\n", encoding="utf-8")
            stale_find = await client.call_tool("find", {"grep": ["Needle"], "limit": 1, "cursor": first_find_cursor})
            stale_list = await client.call_tool("list", {"layer": "wiki", "limit": 1, "cursor": list_cursor})
            changed_query = await client.call_tool("list", {"layer": "wiki", "limit": 2, "cursor": list_cursor})

        for result in (stale_find, stale_list, changed_query):
            assert result.is_error
            error = result.structured_content
            assert isinstance(error, dict)
            assert "cursor" in str(error["error"])

    anyio.run(exercise)


def test_read_never_fetches_body_and_graph_cursor_binds_generation(populated_base: Base) -> None:
    async def exercise() -> None:
        server = create_server(populated_base)
        async with Client(server) as client:
            forbidden = await client.call_tool(
                "read",
                {"uri": "events/2026-09-05/synthetic.json#a1", "body": True},
            )
            first = await client.call_tool(
                "graph",
                {"uri": "wiki/needle.md", "direction": "in", "limit": 1},
            )
            first_payload = _payload(first)
            cursor = str(first_payload.get("next_cursor", ""))
            valid = (
                await client.call_tool(
                    "graph",
                    {"uri": "wiki/needle.md", "direction": "in", "limit": 1, "cursor": cursor},
                )
                if cursor
                else None
            )

            other = populated_base.root / "wiki" / "other.md"
            other.write_text("# Other\n\n[Needle](needle.md)\n", encoding="utf-8")
            build_graph(populated_base)
            stale = (
                await client.call_tool(
                    "graph",
                    {"uri": "wiki/needle.md", "direction": "in", "limit": 1, "cursor": cursor},
                )
                if cursor
                else None
            )

        assert forbidden.is_error
        assert isinstance(forbidden.structured_content, dict)
        assert "unexpected argument" in forbidden.structured_content["error"]
        assert "body" not in forbidden.structured_content["error"]
        assert "synthetic.json" not in forbidden.structured_content["error"]
        assert cast(Any, populated_base.runner).calls == 0
        if valid is not None:
            assert not valid.is_error
        if stale is not None:
            assert stale.is_error
            assert "stale" in str(stale.structured_content)

    anyio.run(exercise)


def test_resources_are_private_conditional_and_keep_the_curated_wiki_body(populated_base: Base) -> None:
    async def exercise() -> None:
        server = create_server(populated_base)
        async with Client(server) as client:
            results = {
                uri: await client.read_resource(uri, cache_mode="bypass")
                for uri in (
                    "fkf://brain/wiki/index",
                    "fkf://brain/wiki/tags",
                    "fkf://brain/projects",
                    "fkf://brain/status",
                )
            }

        for result in results.values():
            assert result.cache_scope == "private"
            assert len(result.contents) == 1
        wiki_content = results["fkf://brain/wiki/index"].contents[0]
        status_content = results["fkf://brain/status"].contents[0]
        assert isinstance(wiki_content, TextResourceContents)
        assert isinstance(status_content, TextResourceContents)
        wiki = json.loads(wiki_content.text)
        status = json.loads(status_content.text)
        assert "Needle decision" in wiki["page"]["body"]
        assert str(populated_base.root) not in json.dumps(status)
        assert "base" not in status
        assert "harnesses" not in status
        assert cast(Any, populated_base.runner).calls == 0

    anyio.run(exercise)


def test_status_resource_anonymizes_absolute_source_test_paths(base: Base) -> None:
    private_test = base.root.parent / "home" / "private" / "provider-check"
    config_path = base.root / "fkf.yaml"
    config_path.write_text(
        config_path.read_text(encoding="utf-8").replace(
            "    run: [provider]\n",
            f'    run: [provider]\n    test: ["{private_test}"]\n',
        ),
        encoding="utf-8",
    )
    config = load_config(base.root)
    configured = Base(
        config=config,
        store=config.store(),
        environment=Environment.from_config(config, inherited_path="/usr/bin:/bin"),
        runner=base.runner,
        now=base.now,
        origin=base.origin,
    )

    async def exercise() -> None:
        async with Client(create_server(configured)) as client:
            result = await client.read_resource("fkf://brain/status", cache_mode="bypass")
        content = result.contents[0]
        assert isinstance(content, TextResourceContents)
        status = json.loads(content.text)
        assert status["sources"][0]["test"] == {"name": "provider-check", "on_path": False}
        assert str(private_test) not in content.text
        assert str(private_test.parent.parent) not in content.text

    anyio.run(exercise)


def test_status_resource_refuses_cumulative_document_bytes(base: Base, monkeypatch: pytest.MonkeyPatch) -> None:
    source = base.source("synthetic")
    paths = []
    for value in ("2026-09-04", "2026-09-05"):
        document = build_document(
            source,
            [{"id": value, "time": f"{value}T12:00:00Z", "title": "bounded status"}],
            window=day_window(parse_day_in_location(value, UTC)),
            collected_at=base.now(),
        )
        base.write_document(document)
        paths.append(base.store.resolve(document.uri()))
    sizes = [path.stat().st_size for path in paths]
    monkeypatch.setattr(mcp_server, "MAX_MCP_SCAN_BYTES", max(sizes) + 1)

    async def exercise() -> None:
        async with Client(create_server(base)) as client:
            with pytest.raises(MCPError, match="MCP status crossed the MCP-only scan ceiling") as caught:
                await client.read_resource("fkf://brain/status", cache_mode="bypass")
        error = str(caught.value)
        assert "source bytes exceed" in error
        assert str(base.root) not in error
        assert str(base.root.parent / "home") not in error

    anyio.run(exercise)


def test_status_resource_refuses_cumulative_page_items(base: Base, monkeypatch: pytest.MonkeyPatch) -> None:
    wiki = base.root / "wiki"
    wiki.mkdir()
    (wiki / "one.md").write_text("# One\n", encoding="utf-8")
    (wiki / "two.md").write_text("# Two\n", encoding="utf-8")
    monkeypatch.setattr(mcp_server, "MAX_MCP_SCAN_ITEMS", 1)

    async def exercise() -> None:
        async with Client(create_server(base)) as client:
            with pytest.raises(MCPError, match="MCP status crossed the MCP-only scan ceiling") as caught:
                await client.read_resource("fkf://brain/status", cache_mode="bypass")
        error = str(caught.value)
        assert "result items exceed 1" in error
        assert str(base.root) not in error
        assert str(base.root.parent / "home") not in error

    anyio.run(exercise)


def test_complete_wire_boundary_replaces_oversize_tools_and_refuses_resources(
    populated_base: Base,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.setattr(mcp_server, "MAX_RESPONSE_BYTES", 64 << 10)
    wiki = populated_base.root / "wiki" / "index.md"
    wiki.write_text("# Wiki\n\n" + 'escape-heavy \\"' * 10_000, encoding="utf-8")
    document = populated_base.read_document("events/2026-09-05/synthetic.json")
    document.records[0]["payload"] = 'escape-heavy \\"' * 10_000
    populated_base.write_document(document)

    async def exercise() -> None:
        server = create_server(populated_base)
        async with Client(server) as client:
            tool = await client.call_tool("read", {"uri": "events/2026-09-05/synthetic.json"})
            with pytest.raises(MCPError):
                await client.read_resource("fkf://brain/wiki/index", cache_mode="bypass")

        assert tool.is_error
        assert "exceeded" in str(tool.structured_content)
        assert tool.meta is not None
        hint = tool.meta[RESULT_SIZE_META_KEY]
        assert isinstance(hint, dict)
        wire = JSONRPCResponse(
            jsonrpc="2.0",
            id=1,
            result=tool.model_dump(mode="json", by_alias=True, exclude_none=True),
        ).model_dump_json(by_alias=True, exclude_unset=True)
        assert hint["bytes"] == len(wire.encode())
        assert len(wire.encode()) <= mcp_server.MAX_RESPONSE_BYTES

    anyio.run(exercise)
