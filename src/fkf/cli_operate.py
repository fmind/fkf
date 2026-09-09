"""CLI adapters for trusted hooks and small base mutations."""

from __future__ import annotations

import sys
from datetime import timedelta
from pathlib import Path
from typing import Annotated

import typer

from fkf.build import BuildCheck, BuildOptions, BuildReport, build, build_if_stale, build_stale, parse_build_target
from fkf.cli_support import FKFGroup, parent_without_command, state
from fkf.config import MAX_FRESHNESS_AGE_HOURS
from fkf.config_view import PublicConfig, public_config
from fkf.errors import CanceledError, InvalidUsageError, OperationalError
from fkf.graph import GraphBuild, build_graph
from fkf.lexical import LexicalIndexBuild, build_lexical_index
from fkf.locking import WriterLock
from fkf.new import NewKind, NewRequest, NewResult, create_new
from fkf.output import register_jsonl, register_text
from fkf.schema import encode_config_schema
from fkf.source_tests import SourceTestReport, SourceTestRequest, run_source_tests
from fkf.status import Status, StatusRequest
from fkf.status import report as status_report
from fkf.sync import RebuildHooks, SyncReport, SyncRequest, preflight_sync, sync
from fkf.timeutil import format_duration, parse_duration
from fkf.trust import require_trust
from fkf.trust_service import trust
from fkf.wiki_index import WikiIndexReport, build_wiki_index

register_jsonl(SourceTestReport, lambda report: report.sources)
register_text(
    SourceTestReport,
    lambda report: "\n".join(
        f"{item.outcome}  {item.source}  {item.elapsed}{f'  {item.error}' if item.error else ''}"
        for item in report.sources
    ),
)
register_text(NewResult, lambda result: result.message)
register_jsonl(SyncReport, lambda report: report.units)
register_jsonl(Status, lambda status: status.findings or status.sources)
register_text(
    SyncReport,
    lambda report: "\n".join(
        f"{unit.outcome:<16} {unit.uri}{f'  {unit.error}' if unit.error else ''}" for unit in report.units
    ),
)


def _wiki_index_text(report: WikiIndexReport) -> str:
    state_name = "unchanged"
    if report.stale:
        state_name = "STALE — run `fkf build wiki`"
    elif report.created:
        state_name = "created"
    elif report.changed:
        state_name = "rewritten"
    return f"{report.uri}  {state_name}\n{report.pages} page(s) across {report.types} type(s), {report.tags} tag(s)"


def _graph_build_text(report: GraphBuild | BuildCheck) -> str:
    if report.stale:
        return f"{report.uri}  STALE — run `fkf build graph`"
    if isinstance(report, BuildCheck) or not report.mode:
        return f"{report.uri}  unchanged"
    return (
        f"{report.uri}  {report.edges} edges from {report.documents} document(s) and "
        f"{report.pages} page(s) in {report.elapsed} ({report.mode})"
    )


def _lexical_build_text(report: LexicalIndexBuild | BuildCheck) -> str:
    if report.stale:
        return f"{report.uri}  STALE — run `fkf build index`"
    if isinstance(report, BuildCheck) or not report.mode:
        return f"{report.uri}  unchanged"
    return (
        f"{report.uri}  {report.entries} entries, {report.postings} postings, "
        f"{report.bytes} bytes in {report.elapsed} ({report.mode})"
    )


def _build_text(report: BuildReport) -> str:
    if report.nothing_stale:
        return "nothing stale"
    lines: list[str] = []
    if report.graph is not None:
        lines.append(_graph_build_text(report.graph))
    if report.wiki is not None:
        lines.append(_wiki_index_text(report.wiki))
    if report.bodies is not None:
        lines.append(f"{report.bodies.message} ({report.bodies.bytes} bytes)")
    if report.index is not None:
        lines.append(_lexical_build_text(report.index))
    return "\n".join(lines)


def _config_text(config: PublicConfig) -> str:
    lines = [f"{config.name}  {config.path}"]
    if config.local_path is not None:
        lines.append(f"overlay: {config.local_path}")
    enabled = sorted(name for name, active in config.layers.items() if active)
    lines.extend(
        (
            f"layers:  {' '.join(enabled)}",
            (
                f"sync:    {config.sync.days} day(s), timeout {format_duration(config.sync.timeout)}, "
                f"concurrency {config.sync.concurrency}, index stale after {config.sync.index_max_age_hours}h"
            ),
        )
    )
    if config.bin:
        lines.append(f"bin:     {' '.join(config.bin)}")
    enabled_sources = sum(source.enabled for source in config.sources.values())
    lines.extend(("", f"{len(config.sources)} source(s), {enabled_sources} enabled"))
    for name in sorted(config.sources):
        source = config.sources[name]
        lines.append(
            f"{name:<24} {'on' if source.enabled else 'off':<4} {source.layer:<8} {', '.join(source.requires)}"
        )
    if config.origins:
        lines.append("")
        lines.extend(f"{name:<24} from {config.origins[name]}" for name in sorted(config.origins))
    return "\n".join(lines)


register_text(BuildReport, _build_text)
register_text(PublicConfig, _config_text)


def _status_text(status: Status) -> str:
    trust = "trusted" if status.trust.trusted else "untrusted"
    lines = [f"{status.name}  {status.base}  ({status.base_origin}, {trust})", ""]
    for layer in status.layers:
        if not layer.enabled:
            lines.append(f"{layer.layer:<10} off")
            continue
        detail = f"{layer.since} .. {layer.until}" if layer.since and layer.until else layer.note
        lines.append(f"{layer.layer:<10} {layer.count:6d} {layer.unit:<9} {detail}".rstrip())
    if status.graph is None:
        lines.append(f"{'graph':<10} not built")
    else:
        lines.append(
            f"{'graph':<10} {status.graph.edges:6d} edge(s) over {status.graph.nodes} nodes, "
            f"built {status.graph.generated_at}"
        )
    if status.sources:
        lines.extend(("", "sources"))
        for source in status.sources:
            state_name = "gone" if source.undeclared else "on" if source.enabled else "off"
            suffix = f"  quiet: {source.quiet_reason}" if source.quiet else ""
            if source.missing_dates:
                suffix += "  missing: " + ", ".join(source.missing_dates)
            if source.auth_required:
                suffix += "  auth-required"
            lines.append(
                f"{source.name:<24} {state_name:<4} {source.last_date or '-':<12} {source.last_count:6d}{suffix}"
            )
        lines.extend(
            (
                "",
                (
                    f"{status.enabled} enabled, {status.missing_requirements} missing requirement(s), "
                    f"{status.missing_test_hooks} missing source test hook(s), {status.quiet} quiet"
                ),
            )
        )
    if status.auth_required:
        lines.append(f"auth required: {', '.join(status.auth_required)}")
    if status.findings:
        lines.extend(("", "findings"))
        for finding in status.findings:
            lines.append(f"  [{finding.severity}] {finding.check:<20} {finding.message}")
            lines.extend(f"         {path}" for path in finding.paths)
            if finding.fix:
                lines.append(f"         fix: {finding.fix}")
        lines.extend(("", f"{status.errors} error(s), {status.warnings} warning(s)"))
    if status.next:
        lines.extend(("", "next", *(f"  {item}" for item in status.next)))
    return "\n".join(lines)


register_text(Status, _status_text)


def register_operate_commands(app: typer.Typer) -> typer.Typer:
    @app.command("status", help="Inspect base overview, collector status, and repository health.")
    def status_command(
        ctx: typer.Context,
        max_age_hours: Annotated[int, typer.Option("--max-age-hours")] = 0,
        live: Annotated[bool, typer.Option("--live")] = False,
    ) -> None:
        if max_age_hours < 0 or max_age_hours > MAX_FRESHNESS_AGE_HOURS:
            raise InvalidUsageError(f"--max-age-hours is {max_age_hours}; expected 1..{MAX_FRESHNESS_AGE_HOURS}")
        invocation = state(ctx)
        base = invocation.base()
        result = status_report(
            base,
            StatusRequest(
                max_age_hours=max_age_hours,
                live=live,
                executable=str(Path(sys.argv[0]).resolve()),
            ),
            cancel=invocation.cancel,
        )
        invocation.emit(result)
        if result.stale:
            if max_age_hours:
                raise OperationalError(
                    f"one or more enabled sources are missing or older than --max-age-hours {max_age_hours}"
                )
            raise OperationalError(
                "one or more enabled sources have missing completed days or exceed their configured freshness limit"
            )
        if not result.ok:
            raise OperationalError(f"status found {result.errors} error(s) in {base.root}")

    @app.command("test", help="Run selected trusted source verification hooks.")
    def source_test(
        ctx: typer.Context,
        sources: Annotated[list[str] | None, typer.Argument()] = None,
        all_sources: Annotated[bool, typer.Option("--all")] = False,
    ) -> None:
        invocation = state(ctx)
        report = run_source_tests(
            invocation.base(),
            SourceTestRequest(tuple(sources or ()), all_sources),
            cancel=invocation.cancel,
        )
        invocation.emit(report)
        if not report.complete:
            raise OperationalError(f"{report.failed} source test(s) failed:\n{report.failure_summary()}")

    @app.command("trust", help="Review and record the exact declared execution plan.")
    def trust_command(
        ctx: typer.Context,
        check: Annotated[bool, typer.Option("--check", help="Inspect without recording.")] = False,
        all_items: Annotated[bool, typer.Option("--all", help="Show the complete disclosure in text.")] = False,
    ) -> None:
        invocation = state(ctx)
        base = invocation.base()
        if check:
            report = trust(base, record=False, all_items=all_items, cancel=invocation.cancel)
            invocation.emit(report)
            if not report.state.trusted:
                require_trust(base.config, cancel=invocation.cancel)
            return
        with WriterLock.acquire(base.root):
            invocation.emit(trust(base, record=True, all_items=all_items, cancel=invocation.cancel))

    @app.command("sync", help="Collect completed days that are missing.")
    def sync_command(
        ctx: typer.Context,
        sources: Annotated[list[str] | None, typer.Argument()] = None,
        days: Annotated[int, typer.Option("--days")] = 0,
        date: Annotated[str, typer.Option("--date")] = "",
        force: Annotated[bool, typer.Option("--force")] = False,
        dry_run: Annotated[bool, typer.Option("--dry-run")] = False,
        preview: Annotated[bool, typer.Option("--preview")] = False,
        if_due: Annotated[bool, typer.Option("--if-due")] = False,
        no_graph: Annotated[bool, typer.Option("--no-graph")] = False,
    ) -> None:
        if days and not 1 <= days <= 366:
            raise InvalidUsageError(f"--days is {days}; expected 1..366")
        if if_due and (force or dry_run or preview):
            raise InvalidUsageError("--if-due cannot be combined with --force, --dry-run, or --preview")
        invocation = state(ctx)
        base = invocation.base()
        request = SyncRequest(tuple(sources or ()), days, date, force, dry_run, no_graph, preview, if_due)
        hooks = RebuildHooks(
            wiki=lambda selected: build_wiki_index(selected, write=True, cancel=invocation.cancel),
            graph=lambda selected: build_graph(selected, cancel=invocation.cancel),
            lexical=lambda selected: build_lexical_index(selected, cancel=invocation.cancel),
        )

        def execute() -> None:
            report = sync(base, request, rebuild=hooks, cancel=invocation.cancel)
            invocation.emit(report)
            if not report.complete:
                raise OperationalError(f"{report.failed} evidence unit(s) failed:\n{report.failure_summary()}")

        if dry_run or preview:
            execute()
            return
        if if_due:
            preflight = preflight_sync(base, request)
            if not preflight.due:
                invocation.emit(preflight.report())
                return
        with WriterLock.acquire(base.root):
            execute()

    @app.command("build", help="Rebuild digest-bound derived caches.")
    def build_command(
        ctx: typer.Context,
        target: Annotated[str, typer.Argument()] = "all",
        check: Annotated[bool, typer.Option("--check")] = False,
        if_stale: Annotated[bool, typer.Option("--if-stale")] = False,
        prune: Annotated[bool, typer.Option("--prune")] = False,
        older_than: Annotated[str, typer.Option("--older-than")] = "",
        source: Annotated[str, typer.Option("--source")] = "",
    ) -> None:
        selected = parse_build_target(target)
        if check and if_stale:
            raise InvalidUsageError("--check cannot be combined with --if-stale")
        duration = timedelta(0)
        if older_than:
            try:
                nanoseconds = parse_duration(older_than)
            except ValueError as error:
                raise InvalidUsageError(str(error), cause=error) from error
            if nanoseconds < 0:
                raise InvalidUsageError("--older-than must not be negative")
            duration = timedelta(microseconds=int(nanoseconds) // 1_000)
        options = BuildOptions(selected, check, prune, source, duration)
        invocation = state(ctx)
        base = invocation.base()
        if check:
            report = build(base, options, cancel=invocation.cancel)
            invocation.emit(report)
            if report.stale:
                raise OperationalError("one or more derived artifacts are stale; run `fkf build`")
            return
        if if_stale and not build_stale(base, selected, cancel=invocation.cancel):
            invocation.emit(BuildReport(nothing_stale=True))
            return
        with WriterLock.acquire(base.root):
            report = (
                build_if_stale(base, selected, cancel=invocation.cancel)
                if if_stale
                else build(base, options, cancel=invocation.cancel)
            )
            invocation.emit(report)

    config_app = typer.Typer(
        cls=FKFGroup,
        invoke_without_command=True,
        no_args_is_help=False,
        help="Print the resolved configuration, or the JSON Schema fkf.yaml is validated against.",
        rich_markup_mode=None,
    )
    app.add_typer(config_app, name="config")

    @config_app.callback()
    def config_parent(ctx: typer.Context) -> None:
        if ctx.invoked_subcommand is None:
            invocation = state(ctx)
            invocation.emit(public_config(invocation.base().config))

    @config_app.command("schema", help="Print the JSON Schema for fkf.yaml, for editor completion.")
    def config_schema(ctx: typer.Context) -> None:
        state(ctx).write(encode_config_schema())

    new_app = typer.Typer(
        cls=FKFGroup,
        invoke_without_command=True,
        no_args_is_help=False,
        help="Create one safe authored scaffold.",
        rich_markup_mode=None,
    )
    app.add_typer(new_app, name="new")

    @new_app.callback()
    def new_parent(ctx: typer.Context) -> None:
        parent_without_command(ctx, "name a subcommand")

    def create(ctx: typer.Context, request: NewRequest) -> None:
        invocation = state(ctx)
        base = invocation.base()
        with WriterLock.acquire(base.root):
            result = create_new(base, request, cancel=invocation.cancel)
            if result.kind is not NewKind.HELPER:
                try:
                    build(base, cancel=invocation.cancel)
                except CanceledError as error:
                    raise CanceledError(
                        f"{result.uri} was created but derived rebuild was canceled; run `fkf build` to repair it",
                        cause=error,
                    ) from error
                except Exception as error:
                    raise OperationalError(
                        f"{result.uri} was created but derived rebuild failed; run `fkf build`: {error}",
                        cause=error,
                    ) from error
            invocation.emit(result)

    @new_app.command("task")
    def new_task(
        ctx: typer.Context,
        slug: Annotated[str, typer.Argument()],
        title: Annotated[str, typer.Option("--title")] = "",
    ) -> None:
        create(ctx, NewRequest(NewKind.TASK, slug, title=title))

    @new_app.command("project")
    def new_project(
        ctx: typer.Context,
        slug: Annotated[str, typer.Argument()],
        tag: Annotated[list[str], typer.Option("--tag")],
        title: Annotated[str, typer.Option("--title")] = "",
    ) -> None:
        create(ctx, NewRequest(NewKind.PROJECT, slug, title=title, tags=tuple(tag)))

    @new_app.command("wiki")
    def new_wiki(
        ctx: typer.Context,
        slug: Annotated[str, typer.Argument()],
        tag: Annotated[list[str], typer.Option("--tag")],
        page_type: Annotated[str, typer.Option("--type")] = "decision",
        title: Annotated[str, typer.Option("--title")] = "",
    ) -> None:
        create(ctx, NewRequest(NewKind.WIKI, slug, title=title, type=page_type, tags=tuple(tag)))

    @new_app.command("helper")
    def new_helper(ctx: typer.Context, name: Annotated[str, typer.Argument()]) -> None:
        create(ctx, NewRequest(NewKind.HELPER, name))

    return config_app


__all__ = ["register_operate_commands"]
