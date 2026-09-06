"""CLI adapters for bounded temporal and identity retrieval."""

from __future__ import annotations

from typing import Annotated

import typer

from fkf.cli_support import state
from fkf.day import (
    DEFAULT_BRIEF_BUDGET,
    DEFAULT_DIGEST_BUDGET,
    MAX_BRIEF_BUDGET,
    MAX_DIGEST_BUDGET,
    BriefReport,
    BriefRequest,
    DayRequest,
    TimelineRequest,
    WhoMatch,
    WhoReport,
    brief,
    day,
    encode_timeline_delivery,
    render_brief_text,
    timeline,
    who,
)
from fkf.errors import InvalidUsageError
from fkf.jsoncodec import dumps
from fkf.output import register_text
from fkf.query import Window
from fkf.timeutil import DurationNS, parse_duration


def _validate_budget(value: int, maximum: int) -> None:
    if value < 1 or value > maximum:
        raise InvalidUsageError(f"--budget is {value}; expected 1..{maximum}")


def _parse_around(value: str | None) -> DurationNS:
    if value is None:
        return DurationNS(0)
    try:
        duration = parse_duration(value)
    except ValueError as error:
        raise InvalidUsageError(str(error), cause=error) from error
    if duration <= 0:
        raise InvalidUsageError("--around must be a positive duration")
    return duration


def _who_nodes_text(nodes: tuple[str, ...]) -> str:
    shown = 8
    if len(nodes) <= shown:
        return ", ".join(nodes)
    return f"{', '.join(nodes[:shown])}, +{len(nodes) - shown} more"


def _who_match_text(match: WhoMatch) -> str:
    lines = [f"{match.canonical} [{match.kind}]"]
    if match.names:
        lines.append(f"names: {', '.join(match.names)}")
    if match.aliases:
        lines.append(f"aliases: {', '.join(match.aliases)}")
    lines.extend(f"page: {page.uri} · {page.title}" for page in match.pages)
    lines.extend(f"source: {count.source} · {count.count}" for count in match.counts)
    lines.extend(f"{group.kind}: {_who_nodes_text(group.nodes)}" for group in match.neighbourhood)
    if match.neighbourhood_truncated:
        lines.append("neighbourhood: truncated at 200 edges")
    lines.extend(f"{record.time} [{record.source}] {record.title} · {record.uri}" for record in match.recent)
    lines.append(f"total: {match.total} interaction(s)")
    return "\n".join(lines)


def _who_text(report: WhoReport) -> str:
    if not report.matches:
        return f"no identity match for {dumps(report.query).decode()}"
    return "\n\n".join(_who_match_text(match) for match in report.matches)


register_text(BriefReport, render_brief_text)
register_text(WhoReport, _who_text)


def register_temporal_commands(app: typer.Typer) -> None:
    """Attach the bounded day, timeline, brief, and who commands."""

    @app.command("day", help="What happened on one day? Render a chronological, budgeted digest.")
    def day_command(
        ctx: typer.Context,
        date: Annotated[str | None, typer.Argument(metavar="[date|today|yesterday]")] = None,
        budget: Annotated[
            int,
            typer.Option("--budget", help="Hard four-bytes-per-token budget for the complete digest."),
        ] = DEFAULT_DIGEST_BUDGET,
        all_records: Annotated[
            bool,
            typer.Option("--all", help="Expand noisy sources instead of representing each by one count."),
        ] = False,
    ) -> None:
        _validate_budget(budget, MAX_DIGEST_BUDGET)
        invocation = state(ctx)
        report = day(
            invocation.base(),
            DayRequest(date or "", budget, all_records, invocation.output_format.value),
            cancel=invocation.cancel,
        )
        # The service accounts for the exact selected encoding, including its final newline.
        invocation.write(encode_timeline_delivery(report))

    @app.command("timeline", help="What happened in a range or around one record? Render a filtered digest.")
    def timeline_command(
        ctx: typer.Context,
        record_uri: Annotated[str | None, typer.Argument(metavar="[record-uri]")] = None,
        since: Annotated[str, typer.Option("--since")] = "",
        until: Annotated[str, typer.Option("--until")] = "",
        source: Annotated[
            list[str] | None,
            typer.Option("--source", help="Restrict to a declared source (repeatable)."),
        ] = None,
        repository: Annotated[
            str,
            typer.Option("--repo", help="Exact repository entity URI, such as repo:github.com/fmind/fkf."),
        ] = "",
        person: Annotated[
            str,
            typer.Option("--person", help="Exact person or actor entity URI."),
        ] = "",
        around: Annotated[
            str | None,
            typer.Option("--around", help="Duration on either side of the record URI (default 2h)."),
        ] = None,
        budget: Annotated[
            int,
            typer.Option("--budget", help="Hard four-bytes-per-token budget for the complete digest."),
        ] = DEFAULT_DIGEST_BUDGET,
        all_records: Annotated[
            bool,
            typer.Option("--all", help="Expand noisy sources instead of representing each by one count."),
        ] = False,
    ) -> None:
        _validate_budget(budget, MAX_DIGEST_BUDGET)
        duration = _parse_around(around)
        invocation = state(ctx)
        report = timeline(
            invocation.base(),
            TimelineRequest(
                window=Window(since, until),
                sources=tuple(source or ()),
                repository=repository,
                person=person,
                around_uri=record_uri or "",
                around=duration,
                budget=budget,
                all=all_records,
                delivery_format=invocation.output_format.value,
            ),
            cancel=invocation.cancel,
        )
        invocation.write(encode_timeline_delivery(report))

    @app.command("brief", help="What needs attention today? Build one budgeted daily control surface.")
    def brief_command(
        ctx: typer.Context,
        budget: Annotated[
            int,
            typer.Option("--budget", help="Hard four-bytes-per-token budget for the complete brief."),
        ] = DEFAULT_BRIEF_BUDGET,
    ) -> None:
        _validate_budget(budget, MAX_BRIEF_BUDGET)
        invocation = state(ctx)
        invocation.emit(brief(invocation.base(), BriefRequest(budget), cancel=invocation.cancel))

    @app.command("who", help="Who is this? Resolve exact identities and show their stored interactions.")
    def who_command(
        ctx: typer.Context,
        query: Annotated[str, typer.Argument(metavar="<name|uri>")],
    ) -> None:
        invocation = state(ctx)
        invocation.emit(who(invocation.base(), query, cancel=invocation.cancel))


__all__ = ["register_temporal_commands"]
