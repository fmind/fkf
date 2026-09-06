"""CLI adapters for durable evidence and authored-page listings."""

from __future__ import annotations

import json
from collections.abc import Sequence
from typing import Annotated, Protocol

import typer
from typer import _click as click

from fkf.cli_support import FKFGroup, parent_without_command, state
from fkf.errors import InvalidUsageError, OperationalError
from fkf.learned import LearnedListing, list_learned
from fkf.listings import DayCount, EventListing, IndexListing, TaskListing, list_events, list_index, list_tasks
from fkf.markdown import ValidationReport
from fkf.output import inline, register_jsonl, register_text
from fkf.pages import PageFilter, PageListing, TagVocabulary, build_tag_vocabulary, list_pages
from fkf.query import parse_window
from fkf.store import Layer
from fkf.validation import (
    DEFAULT_PROJECT_STALE_DAYS,
    RecordTitleReport,
    ValidationBundle,
    validate_all,
    validate_markdown_layer,
    validate_record_titles,
)

register_jsonl(EventListing, lambda result: result.days)
register_jsonl(IndexListing, lambda result: result.entries)
register_jsonl(TaskListing, lambda result: result.traces)
register_jsonl(LearnedListing, lambda result: (result,))
register_jsonl(PageListing, lambda result: result.pages)
register_jsonl(ValidationReport, lambda result: result.issues)
register_jsonl(RecordTitleReport, lambda result: result.issues)


class _HasURI(Protocol):
    @property
    def uri(self) -> str: ...


def _uri_width(values: Sequence[_HasURI]) -> int:
    return min(max((len(value.uri) for value in values), default=0), 44)


def _top_sources(sources: tuple[DayCount, ...]) -> str:
    ordered = sorted(sources, key=lambda item: (-item.count, item.source))
    parts = [f"{item.source} {item.count}" for item in ordered[:3]]
    if len(ordered) > 3:
        parts.append(f"+{len(ordered) - 3} more")
    return " · ".join(parts)


def _event_listing_text(result: EventListing) -> str:
    width = _uri_width(result.days)
    lines = [f"{day.uri:<{width}} {day.total:5d}  {_top_sources(day.sources)}" for day in result.days]
    lines.extend(("", f"{len(result.days)} day(s), {result.total} record(s)"))
    return "\n".join(lines)


def _index_listing_text(result: IndexListing) -> str:
    width = _uri_width(result.entries)
    lines = []
    for item in result.entries:
        stale = "  stale" if item.stale else ""
        lines.append(
            f"{item.uri:<{width}} {item.count:6d} record(s)  {item.age_hours:5d}h old  {item.bytes:7d} bytes{stale}"
        )
    lines.extend(("", f"{len(result.entries)} document(s)"))
    return "\n".join(lines)


def _task_listing_text(result: TaskListing) -> str:
    width = _uri_width(result.traces)
    lines = [f"{trace.uri:<{width}}  {inline(trace.title)}" for trace in result.traces]
    lines.extend(("", f"{len(result.traces)} trace(s)"))
    return "\n".join(lines)


def _learned_listing_text(result: LearnedListing) -> str:
    lines: list[str] = []
    current = ""
    for item in result.bullets:
        if item.trace != current:
            lines.append(item.trace)
            current = item.trace
        lines.append(f"  {'harvested  ' if item.harvested else 'unharvested'}  {inline(item.text)}")
    lines.extend(("", f"{result.harvested} harvested, {result.unharvested} unharvested"))
    return "\n".join(lines)


def _page_listing_text(result: PageListing) -> str:
    width = _uri_width(result.pages)
    lines: list[str] = []
    for page in result.pages:
        classifier = page.status if result.layer is Layer.PROJECTS else page.type
        lines.append(f"{page.uri:<{width}}  {(classifier or '-'):<9}  {inline(page.title)}")
        if page.tags:
            lines.append(f"{'':<{width}}  {'':<9}  {' '.join(page.tags)}")
    lines.extend(("", f"{result.total} page(s) in {result.layer}/"))
    return "\n".join(lines)


def _tag_vocabulary_text(result: TagVocabulary) -> str:
    lines = [f"{item.count:4d}  {item.tag:<24} {' '.join(item.pages)}" for item in result.tags]
    if result.untagged:
        lines.extend(("", f"untagged: {' '.join(result.untagged)}"))
    return "\n".join(lines)


register_text(EventListing, _event_listing_text)
register_text(IndexListing, _index_listing_text)
register_text(TaskListing, _task_listing_text)
register_text(LearnedListing, _learned_listing_text)
register_text(PageListing, _page_listing_text)
register_text(TagVocabulary, _tag_vocabulary_text)


def _validation_text(result: ValidationReport) -> str:
    lines = []
    for issue in result.issues:
        location = f"{issue.uri}:{issue.line}" if issue.line else issue.uri
        lines.append(f"{issue.severity:<7} {location}  {issue.message}")
    lines.extend(("", f"{result.pages} page(s): {result.errors} error(s), {result.warnings} warning(s)"))
    return "\n".join(lines)


def _record_title_text(result: RecordTitleReport) -> str:
    lines = [
        f"{issue.severity:<7} source:{issue.source}  {issue.message}: {json.dumps(issue.title, ensure_ascii=True)}"
        for issue in result.issues
    ]
    lines.extend(
        (
            "",
            (
                f"{result.sources} source(s), {result.documents} document(s), {result.records} record(s): "
                f"{result.errors} error(s), {result.warnings} warning(s)"
            ),
        )
    )
    return "\n".join(lines)


def _validation_bundle_text(result: ValidationBundle) -> str:
    sections: list[str] = []
    for name, report in (
        ("wiki", result.wiki),
        ("projects", result.projects),
        ("records", result.records),
        ("lint", result.lint),
    ):
        if report is None:
            continue
        rendered = _record_title_text(report) if isinstance(report, RecordTitleReport) else _validation_text(report)
        sections.append(f"\n{name}\n{rendered}")
    return "\n".join(sections)


register_text(ValidationReport, _validation_text)
register_text(RecordTitleReport, _record_title_text)
register_text(ValidationBundle, _validation_bundle_text)


def _limit(value: int) -> int:
    if value < 0:
        raise InvalidUsageError(f"--limit is {value}; expected zero or a positive integer")
    return value


def register_browse_commands(app: typer.Typer) -> None:
    list_app = typer.Typer(
        cls=FKFGroup,
        invoke_without_command=True,
        no_args_is_help=False,
        help="List stored evidence and authored pages.",
        rich_markup_mode=None,
    )
    app.add_typer(list_app, name="list")

    @list_app.callback()
    def list_parent(ctx: typer.Context) -> None:
        parent_without_command(ctx, "name a subcommand")

    @list_app.command("events", help="List collected event days.")
    def events(
        ctx: typer.Context,
        since: Annotated[str, typer.Option("--since")] = "",
        until: Annotated[str, typer.Option("--until")] = "",
        source: Annotated[str, typer.Option("--source")] = "",
        limit: Annotated[int, typer.Option("--limit")] = 0,
    ) -> None:
        invocation = state(ctx)
        base = invocation.base()
        try:
            window = parse_window(since, until, base.now())
        except ValueError as error:
            raise InvalidUsageError(str(error), cause=error) from error
        invocation.emit(list_events(base, window, source=source, limit=_limit(limit), cancel=invocation.cancel))

    @list_app.command("index", help="List point-in-time index snapshots.")
    def index(ctx: typer.Context) -> None:
        invocation = state(ctx)
        invocation.emit(list_index(invocation.base(), cancel=invocation.cancel))

    tasks_app = typer.Typer(
        cls=FKFGroup,
        invoke_without_command=True,
        no_args_is_help=False,
        help="List task traces or their exact Learned bullets.",
        rich_markup_mode=None,
    )
    list_app.add_typer(tasks_app, name="tasks")

    @tasks_app.callback()
    def tasks(
        ctx: typer.Context,
        since: Annotated[str, typer.Option("--since")] = "",
        until: Annotated[str, typer.Option("--until")] = "",
        limit: Annotated[int, typer.Option("--limit")] = 0,
    ) -> None:
        if ctx.invoked_subcommand is not None:
            return
        invocation = state(ctx)
        base = invocation.base()
        try:
            window = parse_window(since, until, base.now())
        except ValueError as error:
            raise InvalidUsageError(str(error), cause=error) from error
        invocation.emit(list_tasks(base, window, limit=_limit(limit), cancel=invocation.cancel))

    @tasks_app.command("learned", help="List Learned bullets and whether knowledge cites their trace.")
    def learned(
        ctx: typer.Context,
        since: Annotated[str, typer.Option("--since")] = "",
        until: Annotated[str, typer.Option("--until")] = "",
        unharvested: Annotated[bool, typer.Option("--unharvested")] = False,
    ) -> None:
        invocation = state(ctx)
        base = invocation.base()
        try:
            window = parse_window(since, until, base.now())
        except ValueError as error:
            raise InvalidUsageError(str(error), cause=error) from error
        invocation.emit(list_learned(base, window, only_unharvested=unharvested, cancel=invocation.cancel))

    @list_app.command("projects", help="List project pages.")
    def projects(
        ctx: typer.Context,
        status: Annotated[str, typer.Option("--status")] = "",
        tag: Annotated[list[str] | None, typer.Option("--tag")] = None,
        limit: Annotated[int, typer.Option("--limit")] = 0,
    ) -> None:
        invocation = state(ctx)
        invocation.emit(
            list_pages(
                invocation.base(),
                Layer.PROJECTS,
                PageFilter(tags=tuple(tag or ()), status=status, limit=_limit(limit)),
                cancel=invocation.cancel,
            )
        )

    @list_app.command("wiki", help="List wiki pages.")
    def wiki(
        ctx: typer.Context,
        tag: Annotated[list[str] | None, typer.Option("--tag")] = None,
        page_type: Annotated[str, typer.Option("--type")] = "",
        limit: Annotated[int, typer.Option("--limit")] = 0,
    ) -> None:
        invocation = state(ctx)
        invocation.emit(
            list_pages(
                invocation.base(),
                Layer.WIKI,
                PageFilter(tags=tuple(tag or ()), type=page_type, limit=_limit(limit)),
                cancel=invocation.cancel,
            )
        )

    validate_app = typer.Typer(
        cls=FKFGroup,
        invoke_without_command=True,
        no_args_is_help=False,
        help="Check authored pages and collected subject lines.",
        rich_markup_mode=None,
    )
    app.add_typer(validate_app, name="validate")

    @validate_app.callback()
    def validate_parent(
        ctx: typer.Context,
        strict: Annotated[bool, typer.Option("--strict")] = False,
        lint: Annotated[bool, typer.Option("--lint")] = False,
        stale_days: Annotated[int, typer.Option("--stale-days")] = DEFAULT_PROJECT_STALE_DAYS,
    ) -> None:
        if ctx.invoked_subcommand is not None:
            return
        if stale_days < 1:
            raise InvalidUsageError(f"--stale-days is {stale_days}; expected a positive integer")
        if ctx.get_parameter_source("stale_days") is not click.core.ParameterSource.DEFAULT and not lint:
            raise InvalidUsageError("--stale-days requires --lint")
        invocation = state(ctx)
        result = validate_all(
            invocation.base(),
            strict=strict,
            lint=lint,
            stale_days=stale_days,
            cancel=invocation.cancel,
        )
        invocation.emit(result)
        if not result.ok:
            raise OperationalError("validation found errors")

    def validate_layer(ctx: typer.Context, layer: Layer, *, require_status: bool, strict: bool) -> None:
        invocation = state(ctx)
        result = validate_markdown_layer(
            invocation.base(),
            layer,
            require_status=require_status,
            strict=strict,
            cancel=invocation.cancel,
        )
        invocation.emit(result)
        if not result.ok:
            raise OperationalError(f"validation found {result.errors} error(s) in {layer}/")

    @validate_app.command("wiki")
    def validate_wiki(ctx: typer.Context, strict: Annotated[bool, typer.Option("--strict")] = False) -> None:
        validate_layer(ctx, Layer.WIKI, require_status=False, strict=strict)

    @validate_app.command("projects")
    def validate_projects(ctx: typer.Context, strict: Annotated[bool, typer.Option("--strict")] = False) -> None:
        validate_layer(ctx, Layer.PROJECTS, require_status=True, strict=strict)

    @validate_app.command("records")
    def validate_records(ctx: typer.Context, strict: Annotated[bool, typer.Option("--strict")] = False) -> None:
        invocation = state(ctx)
        result = validate_record_titles(invocation.base(), strict=strict, cancel=invocation.cancel)
        invocation.emit(result)
        if not result.ok:
            raise OperationalError(f"record-title validation found {result.errors} error(s)")

    tags_app = typer.Typer(
        cls=FKFGroup,
        invoke_without_command=True,
        no_args_is_help=False,
        help="List the exact authored tag vocabulary.",
        rich_markup_mode=None,
    )
    app.add_typer(tags_app, name="tags")

    @tags_app.callback()
    def tags_parent(ctx: typer.Context) -> None:
        if ctx.invoked_subcommand is None:
            invocation = state(ctx)
            invocation.emit(build_tag_vocabulary(invocation.base(), Layer.WIKI, cancel=invocation.cancel))

    @tags_app.command("wiki")
    def wiki_tags(ctx: typer.Context) -> None:
        invocation = state(ctx)
        invocation.emit(build_tag_vocabulary(invocation.base(), Layer.WIKI, cancel=invocation.cancel))

    @tags_app.command("projects")
    def project_tags(ctx: typer.Context) -> None:
        invocation = state(ctx)
        invocation.emit(build_tag_vocabulary(invocation.base(), Layer.PROJECTS, cancel=invocation.cancel))


__all__ = ["register_browse_commands"]
