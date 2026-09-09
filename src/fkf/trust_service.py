"""Complete human-reviewable disclosure for FKF's execution trust boundary."""

from __future__ import annotations

import json
import os
from dataclasses import dataclass, field

from fkf.base import Base
from fkf.config import Client
from fkf.fields import FieldMap
from fkf.output import register_text
from fkf.process import DECLARED_COMMAND_DIRECTORY, DECLARED_COMMAND_ENVIRONMENT_POLICY, Cancellation, check_cancel
from fkf.source_runtime import describe_policy
from fkf.store import BASE_CLIENTS_DIR, BASE_SOURCES_DIR, BASE_TESTS_DIR, LAYERS
from fkf.timeutil import format_duration
from fkf.trust import (
    ExecutionEntry,
    TrustChange,
    TrustChangeKind,
    TrustItemKind,
    TrustState,
    capture_trust,
    read_trust,
    write_trust,
)


@dataclass(frozen=True, slots=True)
class TrustedBasePolicy:
    layers: dict[str, bool]
    days: int
    index_max_age_hours: int
    timeout: str
    concurrency: int
    working_directory: str
    environment: str


@dataclass(frozen=True, slots=True)
class TrustedSource:
    name: str
    enabled: bool
    layer: str
    auth: tuple[str, ...] = field(default=(), metadata={"json": "auth,omitempty"})
    run: tuple[str, ...] = field(default=(), metadata={"json": "run,omitempty"})
    test: tuple[str, ...] = field(default=(), metadata={"json": "test,omitempty"})
    body: tuple[str, ...] = field(default=(), metadata={"json": "body,omitempty"})
    body_fields: dict[str, str | list[str]] = field(default_factory=dict, metadata={"json": "body_fields,omitempty"})
    policy: str = field(default="", metadata={"json": "policy,omitempty"})


@dataclass(frozen=True, slots=True, kw_only=True)
class TrustReport:
    base: str
    policy: TrustedBasePolicy
    bin: tuple[str, ...] = field(default=(), metadata={"json": "bin,omitempty"})
    commands: tuple[TrustedSource, ...]
    scripts: tuple[ExecutionEntry, ...] = field(default=(), metadata={"json": "scripts,omitempty"})
    tests: tuple[ExecutionEntry, ...] = field(default=(), metadata={"json": "tests,omitempty"})
    clients: tuple[ExecutionEntry, ...] = field(default=(), metadata={"json": "clients,omitempty"})
    apps: dict[str, Client] = field(default_factory=dict, metadata={"json": "apps,omitempty"})
    state: TrustState
    all: bool = field(default=False, metadata={"json": "-"})
    recorded: bool


def _body_fields(source_fields: FieldMap, names: tuple[str, ...]) -> dict[str, str | list[str]]:
    encoded = source_fields.to_json_value()
    return {name: encoded[name] for name in names}


def trust(base: Base, *, record: bool, all_items: bool = False, cancel: Cancellation | None = None) -> TrustReport:
    """Disclose the exact execution plan, optionally approving its digest locally."""
    check_cancel(cancel)
    policy = TrustedBasePolicy(
        layers={str(layer): enabled for layer, enabled in base.config.layers.items()},
        days=base.config.sync.days,
        index_max_age_hours=base.config.sync.index_max_age_hours,
        timeout=format_duration(base.config.sync.timeout),
        concurrency=base.config.sync.concurrency,
        working_directory=os.fspath(DECLARED_COMMAND_DIRECTORY),
        environment=DECLARED_COMMAND_ENVIRONMENT_POLICY,
    )
    commands: list[TrustedSource] = []
    for name in base.config.source_names():
        check_cancel(cancel)
        source = base.config.sources[name]
        commands.append(
            TrustedSource(
                source.name,
                source.enabled,
                str(source.layer),
                auth=source.auth,
                run=source.run,
                test=source.test,
                body=source.body,
                body_fields=_body_fields(source.fields, source.body_field_names()),
                policy=describe_policy(source),
            )
        )
    snapshot = capture_trust(base.config, cancel=cancel)
    current = (
        write_trust(base.config, base.now(), cancel=cancel, snapshot=snapshot)
        if record
        else read_trust(base.config, cancel=cancel, snapshot=snapshot)
    )
    return TrustReport(
        base=os.fspath(base.root),
        policy=policy,
        bin=base.config.bin,
        commands=tuple(commands),
        scripts=snapshot.scripts,
        tests=snapshot.tests,
        clients=snapshot.clients,
        apps=base.config.clients,
        state=current,
        all=all_items,
        recorded=record,
    )


def _quoted_argv(arguments: tuple[str, ...]) -> str:
    return ", ".join(json.dumps(argument, ensure_ascii=True) for argument in arguments)


def _trusted_source_text(source: TrustedSource) -> list[str]:
    lines = [f"  enabled: {str(source.enabled).lower()}", f"  layer:   {source.layer}"]
    for label, arguments in (("auth", source.auth), ("run", source.run), ("test", source.test), ("body", source.body)):
        if arguments:
            lines.append(f"  {label + ':':<5} [{_quoted_argv(arguments)}]")
    for name in sorted(source.body_fields):
        value = source.body_fields[name]
        rendered = value if isinstance(value, str) else f"[{', '.join(value)}]"
        lines.append(f"  body field {name}: {rendered}")
    if source.policy:
        lines.append(f"  how:  {source.policy}")
    return lines


def _script_tree_text(heading: str, scripts: tuple[ExecutionEntry, ...]) -> list[str]:
    if not scripts:
        return []
    lines = ["", heading]
    for script in scripts:
        if script.kind == "symlink":
            detail = f"symlink -> {script.target}"
        elif not script.executable:
            detail = f"{script.digest[:12]} (not executable)"
        else:
            detail = script.digest[:12]
        lines.append(f"  {script.name:<24} {detail}")
    return lines


def _summarizable_changes(changes: tuple[TrustChange, ...]) -> bool:
    return bool(changes) and all(
        change.item in {TrustItemKind.SOURCE, TrustItemKind.SCRIPT, TrustItemKind.TEST, TrustItemKind.CLIENT}
        for change in changes
    )


def _change_note(change: TrustChange) -> str:
    if change.kind is TrustChangeKind.ARMED:
        return "  (unchanged contents, now executable — this is what PATH picks up)"
    if change.kind is TrustChangeKind.DISARMED:
        return "  (unchanged contents, no longer executable)"
    return ""


def _changes_text(report: TrustReport) -> str:
    lines = [f"{report.base} changed since it was trusted on {report.state.trusted_at}", ""]
    if report.bin:
        lines.append("applies to every command below")
        lines.extend(f"  bin:  {directory} (on PATH)" for directory in report.bin)
        lines.append("")
    sources = {source.name: source for source in report.commands}
    scripts = {
        TrustItemKind.SCRIPT: {script.name: script for script in report.scripts},
        TrustItemKind.TEST: {script.name: script for script in report.tests},
        TrustItemKind.CLIENT: {script.name: script for script in report.clients},
    }
    for change in report.state.changes:
        if change.item is TrustItemKind.CONFIG:
            continue
        lines.append(f"{change.kind} {change.item} {change.name}{_change_note(change)}")
        if change.item is TrustItemKind.SOURCE and change.name in sources:
            lines.extend(_trusted_source_text(sources[change.name]))
        elif change.item in scripts and change.name in scripts[change.item]:
            directory = {TrustItemKind.TEST: BASE_TESTS_DIR, TrustItemKind.CLIENT: BASE_CLIENTS_DIR}.get(
                change.item, BASE_SOURCES_DIR
            )
            lines.append(f"  {directory}/{change.name}  {scripts[change.item][change.name].digest[:12]}")
    if report.recorded:
        lines.extend(("", f"trusted {report.base} (digest {report.state.digest[:12]})"))
    else:
        lines.extend(
            (
                "",
                f"{len(report.state.changes)} change(s). `fkf trust --all` prints everything; `fkf trust` records.",
            )
        )
    return "\n".join(lines)


def render_trust_text(report: TrustReport) -> str:
    """Render the complete human-reviewable trust disclosure."""
    if not report.all and _summarizable_changes(report.state.changes):
        return _changes_text(report)
    lines = ["collection policy"]
    lines.extend(f"  layer: {layer}={str(report.policy.layers.get(str(layer), False)).lower()}" for layer in LAYERS)
    lines.extend(
        (
            (
                f"  sync:  {report.policy.days} day(s), index stale after {report.policy.index_max_age_hours}h, "
                f"timeout {report.policy.timeout}, concurrency {report.policy.concurrency}"
            ),
            "",
            "execution",
            f"  direct argv, cwd {report.policy.working_directory}, {report.policy.environment}",
            "",
        )
    )
    if report.bin:
        lines.append("applies to every command below")
        lines.extend(f"  bin:  {directory} (on PATH)" for directory in report.bin)
        lines.append("")
    if not report.commands:
        lines.append(f"{report.base} enables no source, so nothing would run.")
    for source in report.commands:
        lines.append(source.name)
        lines.extend(_trusted_source_text(source))
    for name, client in sorted(report.apps.items()):
        lines.extend(["", f"client {name}: {client.url}", f"  script: clients/{client.script} (uv run --script)"])
    lines.extend(_script_tree_text("clients/ (explicit uv scripts; outside PATH)", report.clients))
    lines.extend(_script_tree_text("sources/ (on PATH for every command; first for run: and body:)", report.scripts))
    lines.extend(_script_tree_text("tests/ (first on PATH for test: hooks only)", report.tests))
    if report.recorded:
        lines.extend(("", f"trusted {report.base} (digest {report.state.digest[:12]})"))
    else:
        state_name = f"trusted since {report.state.trusted_at}" if report.state.trusted else "NOT trusted"
        lines.extend(("", f"{report.base}: {state_name}"))
    return "\n".join(lines)


register_text(TrustReport, render_trust_text)


__all__ = ["TrustReport", "TrustedBasePolicy", "TrustedSource", "render_trust_text", "trust"]
