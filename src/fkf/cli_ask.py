"""CLI adapters for exact reads and graph exploration."""

from __future__ import annotations

from collections.abc import Mapping
from typing import Annotated

import typer
from typer import _click as click

from fkf.cli_support import FKFGroup, state
from fkf.context import DEFAULT_BUDGET, ContextRequest, build_context
from fkf.errors import InvalidUsageError, OperationalError
from fkf.eval import evaluate
from fkf.find import (
    NO_FIND_LIMIT,
    FindFilter,
    FindResult,
    PageHit,
    RecordHit,
    compact_find_result,
    find,
    parse_where,
)
from fkf.graph import (
    MAX_GRAPH_DEPTH,
    Direction,
    GraphQuery,
    GraphSummary,
    KindCount,
    Neighbourhood,
    NodeListing,
    list_nodes,
    neighbours,
    summarize_graph,
    verify_graph,
)
from fkf.jsoncodec import JsonNumber, dumps, format_float_go
from fkf.locking import WriterLock
from fkf.output import inline, register_jsonl, register_text
from fkf.query import Window, parse_window
from fkf.read import ReadOptions, ReadResult, read
from fkf.store import parse_layer

_GRAPH_VALUE_OPTIONS = frozenset({"--kind", "--depth", "--limit", "--uri"})


class _GraphGroup(FKFGroup):
    """Disambiguate the bare graph URI from its real ``nodes`` subcommand."""

    def parse_args(self, ctx: click.Context, args: list[str]) -> list[str]:
        rewritten = list(args)
        expects_value = False
        for index, argument in enumerate(rewritten):
            if expects_value:
                expects_value = False
                continue
            if argument in _GRAPH_VALUE_OPTIONS:
                expects_value = True
                continue
            if any(argument.startswith(f"{option}=") for option in _GRAPH_VALUE_OPTIONS):
                continue
            if argument.startswith("-"):
                continue
            if self.get_command(ctx, argument) is not None:
                break
            # Click groups otherwise interpret any positional as a subcommand. Rewriting only
            # the first unknown positional retains ``fkf graph <uri>`` without stealing nodes.
            rewritten[index : index + 1] = ["--uri", argument]
            break
        return super().parse_args(ctx, rewritten)


def _positive_or_zero(name: str, value: int) -> None:
    if value < 0:
        raise InvalidUsageError(f"--{name} is {value}; expected zero or a positive integer")


def _graph_text(result: Neighbourhood) -> str:
    lines = [f"{edge.hop}  {edge.kind:<10} {edge.src} -> {edge.dst}  ({edge.via})" for edge in result.edges]
    lines.append(f"\n{len(result.edges)} edge(s), {len(result.nodes)} node(s), {result.stats.lines} row(s) scanned")
    return "\n".join(lines)


def _nodes_text(result: NodeListing) -> str:
    lines = [f"{node.total:5d}  {node.kind:<8} {node.uri}  (in {node.in_}, out {node.out})" for node in result.nodes]
    lines.append(f"\n{result.total} node(s)")
    return "\n".join(lines)


def _kind_counts(label: str, counts: tuple[KindCount, ...]) -> str:
    return f"{label:<6}  {'  '.join(f'{item.kind} {item.count}' for item in counts)}" if counts else ""


def _graph_summary_text(result: GraphSummary) -> str:
    first = f"{result.uri}  {result.edges} edge(s), {result.nodes} node(s)"
    if result.generated_at:
        first += f"  built {result.generated_at}"
    lines = [first]
    lines.extend(filter(None, (_kind_counts("edges", result.edge_kinds), _kind_counts("nodes", result.node_kinds))))
    if result.extractors:
        lines.append(f"{'from':<6}  {' '.join(result.extractors)}")
    return "\n".join(lines)


def _record_scalar(value: object) -> str | None:
    if value is None:
        return "-"
    if isinstance(value, str):
        return value or "-"
    if isinstance(value, bool):
        return str(value).lower()
    if isinstance(value, JsonNumber):
        return value.raw
    if isinstance(value, int):
        return str(value)
    if isinstance(value, float):
        return format_float_go(value, fixed=True)
    return None


def _record_text(record: Mapping[str, object]) -> str:
    keys = sorted(record)
    width = max((len(key) for key in keys if _record_scalar(record[key]) is not None), default=0)
    lines: list[str] = []
    for key in keys:
        scalar = _record_scalar(record[key])
        if scalar is not None:
            lines.append(f"{key:<{width}}  {inline(scalar)}")
            continue
        lines.append(key)
        lines.extend(f"  {line}" for line in dumps(record[key], indent=True).decode().splitlines())
    return "\n".join(lines)


def _document_text(result: ReadResult) -> str:
    document = result.document
    if document is None:
        return ""
    lines = [
        f"{document.uri()}  {document.source}  {document.count} record(s)  collected {document.collected_at}",
        "",
    ]
    for record in document.records:
        uri = document.record_uri(record)
        if uri is None:
            continue
        lines.append(uri)
        if title := document.fields.eval_string("title", record):
            lines.append(f"    {inline(title)}")
    return "\n".join(lines)


def _entity_text(result: ReadResult) -> str:
    entity = result.entity
    if entity is None:
        return ""
    lines = [f"{edge.kind:<10} {edge.src} -> {edge.dst}" for edge in entity.neighbours]
    summary = f"{len(entity.neighbours)} edge(s)"
    if entity.neighbours_truncated:
        summary += " (truncated; raise --limit)"
    lines.extend(("", summary))
    return "\n".join(lines)


def _read_text(result: ReadResult) -> str:
    if result.text is not None:
        # Go's line writer adds a newline even when stored text already has one.
        payload = result.text + "\n"
    elif result.entries is not None:
        payload = "\n".join(result.entries)
    elif result.record is not None:
        payload = _record_text(result.record)
    elif result.page is not None:
        payload = result.page.body
    elif result.document is not None:
        payload = _document_text(result)
    elif result.entity is not None:
        payload = _entity_text(result)
    else:
        payload = dumps(result.selection).decode()
    body = f"\n\n--- body ({result.body_state}) ---\n{result.body}" if result.body is not None else ""
    return f"{result.uri}  [{result.kind}]\n\n{payload}{body}"


def _find_jsonl(result: FindResult) -> tuple[PageHit | RecordHit | object, ...]:
    if result.pages:
        return (*result.pages, *result.records)
    if result.records or not result.volumes:
        return result.records
    return result.volumes


def _find_text(result: FindResult) -> str:
    if result.volumes:
        totals: dict[str, int] = {}
        for volume in result.volumes:
            for source in volume.sources:
                totals[source.source] = totals.get(source.source, 0) + source.count
        lines: list[str] = []
        if result.days:
            lines.extend((f"{result.days[0]} .. {result.days[-1]}  {len(result.days)} day(s)", ""))
        width = min(max(map(len, totals), default=0), 44)
        lines.extend(
            f"{source:<{width}} {count:6d}"
            for source, count in sorted(totals.items(), key=lambda item: (-item[1], item[0]))
        )
        lines.extend(("", f"{result.matched} record(s) across {len(result.volumes)} day(s)"))
    else:
        lines = []
        for hit in result.pages:
            lines.append(f"{hit.layer:<9} {hit.uri}")
            if summary := hit.title or hit.excerpt:
                lines.append(f"          {summary}")
        if result.pages and result.records:
            lines.append("")
        for record in result.records:
            lines.append(f"{record.time or '-'}  {record.uri}")
            if record.title:
                lines.append(f"    {record.title}")
        lines.append("")
        parts: list[str] = []
        if result.pages:
            parts.append(f"{len(result.pages)} page(s)")
        if result.scanned or not result.pages:
            records = f"{result.matched} of {result.scanned} record(s) scanned"
            if result.truncated:
                records += " (truncated; raise --limit)"
            parts.append(records)
        lines.append(", ".join(parts))
    if result.index is not None:
        state = "used" if result.index.used else f"fallback={result.index.reason or '-'}"
        lines.append(f"index {result.index.path} {state}")
    return "\n".join(lines)


register_jsonl(Neighbourhood, lambda result: result.edges)
register_jsonl(NodeListing, lambda result: result.nodes)
register_jsonl(FindResult, _find_jsonl)
register_text(Neighbourhood, _graph_text)
register_text(NodeListing, _nodes_text)
register_text(GraphSummary, _graph_summary_text)
register_text(FindResult, _find_text)
register_text(ReadResult, _read_text)


def register_ask_commands(app: typer.Typer) -> None:
    """Attach the implemented read-only ask surface."""

    @app.command("context", help="Build a token-budgeted evidence pack for an agent.")
    def context_command(
        ctx: typer.Context,
        terms: Annotated[list[str] | None, typer.Argument()] = None,
        since: Annotated[str, typer.Option("--since")] = "",
        until: Annotated[str, typer.Option("--until")] = "",
        budget: Annotated[int, typer.Option("--budget")] = DEFAULT_BUDGET,
        pin: Annotated[list[str] | None, typer.Option("--pin")] = None,
        expand: Annotated[bool, typer.Option("--expand")] = False,
        explain: Annotated[bool, typer.Option("--explain")] = False,
        since_receipt: Annotated[str, typer.Option("--since-receipt")] = "",
    ) -> None:
        if not terms:
            raise InvalidUsageError("fkf context takes the terms to brief an agent on")
        if budget <= 0:
            raise InvalidUsageError(f"--budget is {budget}; expected a positive integer")
        invocation = state(ctx)
        delivery = invocation.output_format.value
        base = invocation.base()
        with WriterLock.acquire(base.root):
            invocation.emit(
                build_context(
                    base,
                    ContextRequest(
                        " ".join(terms),
                        window=Window(since, until),
                        budget=budget,
                        pins=tuple(pin or ()),
                        expand=expand,
                        explain=explain,
                        since_receipt=since_receipt,
                        save_snapshot=True,
                        delivery_format=delivery,
                    ),
                    cancel=invocation.cancel,
                )
            )

    @app.command("find", help="Search every selected layer and return every match.")
    def find_command(
        ctx: typer.Context,
        terms: Annotated[list[str] | None, typer.Argument()] = None,
        layer: Annotated[list[str] | None, typer.Option("--layer")] = None,
        source: Annotated[list[str] | None, typer.Option("--source")] = None,
        since: Annotated[str, typer.Option("--since")] = "",
        until: Annotated[str, typer.Option("--until")] = "",
        grep: Annotated[list[str] | None, typer.Option("--grep")] = None,
        where: Annotated[list[str] | None, typer.Option("--where")] = None,
        limit: Annotated[int, typer.Option("--limit")] = 0,
        count: Annotated[bool, typer.Option("--count")] = False,
        bodies: Annotated[bool, typer.Option("--bodies")] = False,
        raw: Annotated[bool, typer.Option("--raw")] = False,
    ) -> None:
        _positive_or_zero("limit", limit)
        invocation = state(ctx)
        base = invocation.base()
        try:
            window = parse_window(since, until, base.now())
            layers = tuple(parse_layer(value) for value in layer or ())
            clauses = tuple(parse_where(value) for value in where or ())
        except ValueError as error:
            raise InvalidUsageError(str(error), cause=error) from error
        explicit_limit = ctx.get_parameter_source("limit") is not click.core.ParameterSource.DEFAULT
        selected_limit = NO_FIND_LIMIT if explicit_limit and limit == 0 else limit
        result = find(
            base,
            FindFilter(
                sources=tuple(source or ()),
                layers=layers,
                window=window,
                grep=(*(terms or ()), *(grep or ())),
                where=clauses,
                limit=selected_limit,
                bodies=bodies,
            ),
            counting=count,
            cancel=invocation.cancel,
        )
        if invocation.output_format.value != "text" and not raw:
            compact_find_result(result)
        invocation.emit(result)

    @app.command("read", help="Open exactly one URI, selector, section, record, or entity.")
    def read_command(
        ctx: typer.Context,
        uri: Annotated[str, typer.Argument(help="Published FKF URI to open.")],
        body: Annotated[bool, typer.Option("--body", help="Fetch one record body explicitly.")] = False,
        limit: Annotated[int, typer.Option("--limit", help="Bound directory or entity results.")] = 0,
    ) -> None:
        _positive_or_zero("limit", limit)
        invocation = state(ctx)
        base = invocation.base()
        options = ReadOptions(body=body, limit=limit)
        if body:
            with WriterLock.acquire(base.root):
                invocation.emit(read(base, uri, options, cancel=invocation.cancel))
        else:
            invocation.emit(read(base, uri, options, cancel=invocation.cancel))

    @app.command("eval", help="Measure retrieval recall at k against evals/queries.yaml.")
    def eval_command(ctx: typer.Context) -> None:
        invocation = state(ctx)
        report = evaluate(invocation.base(), cancel=invocation.cancel)
        invocation.emit(report)
        if not report.passed:
            raise OperationalError(f"{report.failed} retrieval evaluation(s) failed")

    graph_app = typer.Typer(
        cls=_GraphGroup,
        invoke_without_command=True,
        no_args_is_help=False,
        help="Walk the derived graph or inspect its complete shape.",
        rich_markup_mode=None,
    )
    app.add_typer(graph_app, name="graph")

    @graph_app.callback()
    def graph_command(
        ctx: typer.Context,
        uri: Annotated[str | None, typer.Option("--uri", hidden=True)] = None,
        verify: Annotated[bool, typer.Option("--verify", help="Fully hash inputs and artifacts.")] = False,
        incoming: Annotated[bool, typer.Option("--in", help="Follow incoming edges.")] = False,
        outgoing: Annotated[bool, typer.Option("--out", help="Follow outgoing edges.")] = False,
        both: Annotated[bool, typer.Option("--both", help="Follow both directions.")] = False,
        kind: Annotated[str, typer.Option("--kind", help="Restrict to one edge kind.")] = "",
        depth: Annotated[int, typer.Option("--depth", help="Hops to follow, 1 to 3.")] = 1,
        limit: Annotated[int, typer.Option("--limit", help="Maximum edges to return.")] = 0,
    ) -> None:
        if ctx.invoked_subcommand is not None:
            if verify:
                raise InvalidUsageError("fkf graph --verify accepts no subcommand")
            return
        _positive_or_zero("limit", limit)
        if depth < 1 or depth > MAX_GRAPH_DEPTH:
            raise InvalidUsageError(f"--depth is {depth}; expected 1..{MAX_GRAPH_DEPTH}")
        if sum((incoming, outgoing, both)) > 1:
            raise InvalidUsageError("choose one of --in, --out, or --both")
        invocation = state(ctx)
        base = invocation.base()
        if verify:
            if uri is not None or incoming or outgoing or both or kind or limit or depth != 1:
                raise InvalidUsageError("fkf graph --verify accepts no URI or walk options")
            invocation.emit(verify_graph(base, cancel=invocation.cancel))
            return
        if uri is None:
            invocation.emit(summarize_graph(base, cancel=invocation.cancel))
            return
        direction = Direction.IN if incoming else Direction.OUT if outgoing else Direction.BOTH
        invocation.emit(
            neighbours(
                base,
                GraphQuery(uri, direction, kind, depth, limit=limit),
                cancel=invocation.cancel,
            )
        )

    @graph_app.command("nodes", help="List every graph node, busiest first.")
    def graph_nodes(
        ctx: typer.Context,
        kind: Annotated[str, typer.Option("--kind", help="Restrict to one node kind.")] = "",
        limit: Annotated[int, typer.Option("--limit", help="Maximum nodes to return.")] = 0,
    ) -> None:
        _positive_or_zero("limit", limit)
        invocation = state(ctx)
        invocation.emit(list_nodes(invocation.base(), kind, limit, cancel=invocation.cancel))


__all__ = ["register_ask_commands"]
