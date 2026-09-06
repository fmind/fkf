"""Whole-base authored knowledge and stored-title validation."""

from __future__ import annotations

from dataclasses import dataclass, field, replace
from datetime import date, datetime
from typing import Any

from fkf.base import Base
from fkf.errors import CanceledError, FKFError
from fkf.fields import FIELD_TITLE, scalar_string, validate_relation_value
from fkf.markdown import Issue, Page, Severity, ValidationReport, parse_page, validate_pages
from fkf.marked_block import BLOCK_END_MARKER, BLOCK_MARKER_PREFIX, MarkedBlockMarkers, parse_marked_block_region
from fkf.pages import load_markdown_layer
from fkf.process import Cancellation, check_cancel
from fkf.read import ReadOptions, read
from fkf.store import Layer
from fkf.uri import URI, Scheme, resolve_link
from fkf.verify import document_uris
from fkf.wiki_index import INDEX_BLOCK_BEGIN, INDEX_PAGE_URI

DEFAULT_PROJECT_STALE_DAYS = 90


@dataclass(frozen=True, slots=True)
class RecordTitleIssue:
    source: str
    title: str
    count: int
    records: int
    severity: Severity
    message: str


@dataclass(frozen=True, slots=True)
class RecordTitleReport:
    sources: int
    documents: int
    records: int
    strict: bool
    errors: int
    warnings: int
    issues: tuple[RecordTitleIssue, ...]
    ok: bool


@dataclass(frozen=True, slots=True)
class ValidationBundle:
    wiki: ValidationReport | None = field(default=None, metadata={"json": "wiki,omitempty"})
    projects: ValidationReport | None = field(default=None, metadata={"json": "projects,omitempty"})
    records: RecordTitleReport | None = field(default=None, metadata={"json": "records,omitempty"})
    lint: ValidationReport | None = field(default=None, metadata={"json": "lint,omitempty"})
    ok: bool = False


def _issue(uri: str, message: str, *, strict: bool, line: int = 0, error: bool = False) -> Issue:
    severity = Severity.ERROR if error or strict else Severity.WARNING
    return Issue(uri, severity, message, line)


def _finish(base: ValidationReport, additions: list[Issue]) -> ValidationReport:
    issues = tuple(sorted((*base.issues, *additions), key=lambda item: (item.uri, item.line)))
    errors = sum(item.severity is Severity.ERROR for item in issues)
    return replace(base, errors=errors, warnings=len(issues) - errors, issues=issues, ok=errors == 0)


def _resolved_page_link(base: Base, page: Page, target: str) -> URI:
    resolved = resolve_link(page.uri, target)
    if resolved.scheme == Scheme.FILE:
        base.store.resolve(resolved.path)
    return resolved


def _relation_issues(base: Base, page: Page, strict: bool, cancel: Cancellation | None) -> list[Issue]:
    del strict  # Relation declarations are structural errors in both modes.
    issues: list[Issue] = []
    for name in sorted(page.relations):
        check_cancel(cancel)
        definition = base.config.schema.get(name)
        if definition is None:
            issues.append(
                _issue(page.uri, f"frontmatter relations.{name} is not declared in fkf.yaml schema", strict=True)
            )
            continue
        if not definition.relation:
            issues.append(_issue(page.uri, f"frontmatter relations.{name} is not declared as a relation", strict=True))
            continue
        values = page.relations[name]
        if not definition.cardinality.allows(len(values)):
            issues.append(
                _issue(
                    page.uri,
                    f"frontmatter relations.{name} has {len(values)} values; cardinality "
                    f"{definition.cardinality} does not allow that count",
                    strict=True,
                )
            )
            continue
        for candidate in values:
            check_cancel(cancel)
            try:
                resolved = _resolved_page_link(base, page, candidate)
                validate_relation_value(resolved.node_uri())
            except (ValueError, FKFError) as error:
                issues.append(
                    _issue(
                        page.uri,
                        f"frontmatter relations.{name} URI {candidate!r}: {error}",
                        strict=True,
                    )
                )
    return issues


def _link_issues(base: Base, page: Page, strict: bool, cancel: Cancellation | None) -> list[Issue]:
    issues: list[Issue] = []
    for link in page.links:
        check_cancel(cancel)
        if not link.target.strip():
            continue
        try:
            resolved = _resolved_page_link(base, page, link.target)
        except (ValueError, FKFError) as error:
            issues.append(_issue(page.uri, f"link {link.target!r}: {error}", strict=True, line=link.line))
            continue
        if resolved.scheme != Scheme.FILE:
            continue
        if not base.exists(resolved.path):
            issues.append(
                _issue(
                    page.uri,
                    f"link {link.target!r} points at {resolved.path}, which does not exist",
                    strict=strict,
                    line=link.line,
                )
            )
            continue
        if resolved.fragment or resolved.jq:
            try:
                read(base, str(resolved), ReadOptions(), cancel=cancel)
            except CanceledError:
                raise
            except Exception as error:
                issues.append(
                    _issue(
                        page.uri,
                        f"link {link.target!r} is not addressable: {error}",
                        strict=strict,
                        line=link.line,
                    )
                )
    return issues


def validate_markdown_layer(
    base: Base,
    layer: Layer,
    *,
    require_status: bool = False,
    strict: bool = False,
    cancel: Cancellation | None = None,
) -> ValidationReport:
    """Validate local Markdown rules, relation declarations, and addressable links."""
    check_cancel(cancel)
    base.require_layer(layer)
    pages, nested = load_markdown_layer(base, layer, cancel=cancel)
    report = validate_pages(
        pages,
        layer=str(layer),
        require_status=require_status,
        strict=strict,
        nested=nested,
    )
    additions: list[Issue] = []
    for page in pages:
        check_cancel(cancel)
        additions.extend(_relation_issues(base, page, strict, cancel))
        additions.extend(_link_issues(base, page, strict, cancel))
    return _finish(report, additions)


@dataclass(slots=True)
class _TitleCounts:
    documents: int = 0
    records: int = 0
    titles: dict[str, int] = field(default_factory=dict)
    display: dict[str, str] = field(default_factory=dict)


def validate_record_titles(
    base: Base,
    *,
    strict: bool = False,
    cancel: Cancellation | None = None,
) -> RecordTitleReport:
    """Warn when one current title projection describes most records in a source."""
    check_cancel(cancel)
    by_source: dict[str, _TitleCounts] = {}
    for uri in document_uris(base, cancel=cancel):
        check_cancel(cancel)
        document = base.read_document(uri)
        counts = by_source.setdefault(document.source, _TitleCounts())
        counts.documents += 1
        current = base.config.sources.get(document.source)
        if current is None or document.fields.paths(FIELD_TITLE) != current.fields.paths(FIELD_TITLE):
            continue
        counts.records += len(document.records)
        for record in document.records:
            check_cancel(cancel)
            title = document.fields.eval_string(FIELD_TITLE, record)
            normalized = " ".join(title.split()) if title else ""
            if not normalized:
                continue
            key = normalized.casefold()
            counts.titles[key] = counts.titles.get(key, 0) + 1
            counts.display.setdefault(key, normalized)
    issues: list[RecordTitleIssue] = []
    for source in sorted(by_source):
        check_cancel(cancel)
        counts = by_source[source]
        if counts.records < 2:
            continue
        for key in sorted(counts.titles):
            check_cancel(cancel)
            count = counts.titles[key]
            if count * 2 <= counts.records:
                continue
            issues.append(
                RecordTitleIssue(
                    source,
                    counts.display[key],
                    count,
                    counts.records,
                    Severity.ERROR if strict else Severity.WARNING,
                    f"title is shared by {count} of {counts.records} records; derive a meaningful subject line",
                )
            )
    errors = len(issues) if strict else 0
    return RecordTitleReport(
        sources=len(by_source),
        documents=sum(value.documents for value in by_source.values()),
        records=sum(value.records for value in by_source.values()),
        strict=strict,
        errors=errors,
        warnings=0 if strict else len(issues),
        issues=tuple(issues),
        ok=errors == 0,
    )


def _frontmatter_scalar(value: Any) -> str | None:
    if isinstance(value, datetime):
        return value.isoformat()
    if isinstance(value, date):
        return value.isoformat()
    if isinstance(value, int) and not isinstance(value, bool):
        return str(value)
    return scalar_string(value)


def _date_like(name: str) -> bool:
    normalized = name.strip().lower()
    return normalized in {"date", "due", "start", "end", "valid_from", "valid_until"} or normalized.endswith(
        ("_date", "_at")
    )


def _relative_date(value: str) -> bool:
    normalized = " ".join(value.casefold().split())
    return (
        normalized in {"today", "yesterday", "tomorrow"}
        or normalized.startswith(("last ", "next ", "this "))
        or normalized.endswith((" ago", " from now"))
    )


def _authored_index_links(page: Page) -> tuple:
    markers = MarkedBlockMarkers(INDEX_BLOCK_BEGIN, BLOCK_MARKER_PREFIX, BLOCK_END_MARKER, BLOCK_END_MARKER)
    region = parse_marked_block_region(page.body, markers)
    if not region.present:
        return page.links
    authored = page.body[: region.begin] + page.body[region.end :]
    return parse_page(page.uri, authored.encode()).links


def _lint_target(
    base: Base,
    page: Page,
    target: str,
    via: str,
    *,
    supersedes: bool,
    known: dict[str, Page],
    inbound: dict[str, int],
    strict: bool,
    cancel: Cancellation | None,
) -> Issue | None:
    check_cancel(cancel)
    try:
        resolved = _resolved_page_link(base, page, target)
    except (ValueError, FKFError) as error:
        return (
            _issue(
                page.uri,
                f"supersedes target {target!r} is not an existing addressable page: {error}",
                strict=strict,
            )
            if supersedes
            else None
        )
    if resolved.scheme != Scheme.FILE:
        if supersedes:
            return _issue(page.uri, f"supersedes target {target!r} must be a wiki or project page URI", strict=strict)
        return None
    if resolved.fragment or resolved.jq:
        if supersedes:
            return _issue(page.uri, f"supersedes target {target!r} must name a whole page", strict=strict)
        if not base.exists(resolved.path):
            return _issue(page.uri, f"{via} target {target!r} does not exist", strict=strict)
        try:
            read(base, str(resolved), ReadOptions(), cancel=cancel)
        except CanceledError:
            raise
        except Exception as error:
            return _issue(page.uri, f"{via} target {target!r} is not addressable: {error}", strict=strict)
        if resolved.path in known and resolved.path != page.uri:
            inbound[resolved.path] = inbound.get(resolved.path, 0) + 1
        return None
    if resolved.path in known:
        if resolved.path != page.uri:
            inbound[resolved.path] = inbound.get(resolved.path, 0) + 1
        return None
    if supersedes:
        return _issue(page.uri, f"supersedes target {target!r} does not exist", strict=strict)
    if not base.exists(resolved.path):
        return _issue(page.uri, f"{via} target {target!r} does not exist", strict=strict)
    return None


def validate_knowledge_lint(
    base: Base,
    *,
    strict: bool = False,
    stale_days: int = DEFAULT_PROJECT_STALE_DAYS,
    cancel: Cancellation | None = None,
) -> ValidationReport:
    """Run advisory cross-page link, orphan, validity, and freshness checks."""
    check_cancel(cancel)
    if stale_days < 1:
        raise ValueError("stale project horizon must be positive")
    pages: list[Page] = []
    for layer in (Layer.WIKI, Layer.PROJECTS):
        check_cancel(cancel)
        if base.store.enabled(layer):
            loaded, _nested = load_markdown_layer(base, layer, cancel=cancel)
            pages.extend(loaded)
    pages.sort(key=lambda page: page.uri)
    known = {page.uri: page for page in pages}
    inbound: dict[str, int] = {}
    issues: list[Issue] = []
    now = base.now()
    for page in pages:
        check_cancel(cancel)
        for name in sorted(page.frontmatter):
            check_cancel(cancel)
            value = _frontmatter_scalar(page.frontmatter[name])
            if value and _date_like(name) and _relative_date(value):
                issues.append(
                    _issue(
                        page.uri,
                        f"frontmatter `{name}` uses relative date {value!r}; write an absolute YYYY-MM-DD date",
                        strict=strict,
                    )
                )
        if page.uri.startswith("projects/") and page.status != "done":
            if page.status == "active" and page.valid_until and page.valid_until < now.date().isoformat():
                issues.append(
                    _issue(
                        page.uri,
                        f"active project valid_until {page.valid_until} is in the past",
                        strict=strict,
                    )
                )
            if page.updated:
                try:
                    updated = datetime.fromisoformat(page.updated)
                    evaluation = now if now.tzinfo is not None else now.astimezone()
                    if (evaluation - updated).days > stale_days:
                        issues.append(
                            _issue(
                                page.uri,
                                f"project page is untouched for {(evaluation - updated).days} days; review or close it",
                                strict=strict,
                            )
                        )
                except ValueError:
                    pass
        if page.uri == "wiki/log.md":
            continue
        links = _authored_index_links(page) if page.uri == INDEX_PAGE_URI else page.links
        issues.extend(
            issue
            for link in links
            if (
                issue := _lint_target(
                    base,
                    page,
                    link.target,
                    "link",
                    supersedes=False,
                    known=known,
                    inbound=inbound,
                    strict=strict,
                    cancel=cancel,
                )
            )
            is not None
        )
        issues.extend(
            issue
            for name in sorted(page.relations)
            for target in page.relations[name]
            if (
                issue := _lint_target(
                    base,
                    page,
                    target,
                    f"relation {name}",
                    supersedes=name == "supersedes",
                    known=known,
                    inbound=inbound,
                    strict=strict,
                    cancel=cancel,
                )
            )
            is not None
        )
    for page in pages:
        check_cancel(cancel)
        if page.uri.startswith("wiki/") and page.slug not in {"index", "log"} and not inbound.get(page.uri):
            issues.append(
                _issue(
                    page.uri,
                    "orphan wiki page has no explicit inbound link or relation outside the generated index",
                    strict=strict,
                )
            )
    issues.sort(key=lambda issue: (issue.uri, issue.line))
    errors = sum(issue.severity is Severity.ERROR for issue in issues)
    return ValidationReport("lint", len(pages), strict, errors, len(issues) - errors, tuple(issues), errors == 0)


def validate_all(
    base: Base,
    *,
    strict: bool = False,
    lint: bool = False,
    stale_days: int = DEFAULT_PROJECT_STALE_DAYS,
    cancel: Cancellation | None = None,
) -> ValidationBundle:
    check_cancel(cancel)
    wiki = (
        validate_markdown_layer(base, Layer.WIKI, strict=strict, cancel=cancel)
        if base.store.enabled(Layer.WIKI)
        else None
    )
    projects = (
        validate_markdown_layer(base, Layer.PROJECTS, require_status=True, strict=strict, cancel=cancel)
        if base.store.enabled(Layer.PROJECTS)
        else None
    )
    records = validate_record_titles(base, strict=strict, cancel=cancel)
    lint_report = validate_knowledge_lint(base, strict=strict, stale_days=stale_days, cancel=cancel) if lint else None
    ok = all(report is None or report.ok for report in (wiki, projects, records, lint_report))
    return ValidationBundle(wiki, projects, records, lint_report, ok)


__all__ = [
    "DEFAULT_PROJECT_STALE_DAYS",
    "RecordTitleIssue",
    "RecordTitleReport",
    "ValidationBundle",
    "validate_all",
    "validate_knowledge_lint",
    "validate_markdown_layer",
    "validate_record_titles",
]
