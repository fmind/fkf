"""Flat authored-page listing, vocabulary, and deterministic lexical search."""

from __future__ import annotations

import os
from dataclasses import dataclass, field, replace
from datetime import UTC, datetime
from typing import TYPE_CHECKING, Final

from fkf.base import Base
from fkf.config import ConfigError
from fkf.io import read_file_limited
from fkf.markdown import PROJECT_STATUSES, Page, parse_page
from fkf.process import Cancellation, check_cancel
from fkf.store import MARKDOWN_EXTENSION, MAX_NARRATIVE_BYTES, Layer

if TYPE_CHECKING:
    from fkf.scan import ScanGuard

MAX_VOCABULARY_LISTED: Final = 20


@dataclass(frozen=True, slots=True)
class PageFilter:
    tags: tuple[str, ...] = ()
    status: str = ""
    type: str = ""
    limit: int = 0


@dataclass(frozen=True, slots=True)
class PageListing:
    layer: Layer
    pages: tuple[Page, ...]
    total: int


@dataclass(frozen=True, slots=True)
class TagCount:
    tag: str
    count: int
    pages: tuple[str, ...]


@dataclass(frozen=True, slots=True)
class TagVocabulary:
    layer: Layer
    tags: tuple[TagCount, ...]
    untagged: tuple[str, ...] = field(metadata={"json": "untagged,omitempty"})
    pages: int


@dataclass(frozen=True, slots=True)
class SearchHit:
    uri: str
    layer: Layer
    slug: str
    title: str = ""
    type: str = ""
    date: str = ""
    tags: tuple[str, ...] = ()
    score: int = 0
    matched: tuple[str, ...] = ()
    excerpt: str = ""


@dataclass(frozen=True, slots=True)
class SearchResult:
    layer: Layer
    terms: tuple[str, ...]
    hits: tuple[SearchHit, ...]


def _join_vocabulary(vocabulary: tuple[str, ...]) -> str:
    if not vocabulary:
        return "none"
    if len(vocabulary) <= MAX_VOCABULARY_LISTED:
        return ", ".join(vocabulary)
    shown = vocabulary[:MAX_VOCABULARY_LISTED]
    return f"{', '.join(shown)}, … and {len(vocabulary) - MAX_VOCABULARY_LISTED} more (see `fkf config`)"


def require_known(field: str, values: tuple[str, ...], vocabulary: tuple[str, ...]) -> None:
    """Reject typos in a closed selector before they become authoritative empty results."""
    known = set(vocabulary)
    for value in values:
        if value not in known:
            raise ConfigError(f"unknown {field} {value!r}; this base declares {_join_vocabulary(vocabulary)}")


def read_page(
    base: Base,
    uri: str,
    *,
    cancel: Cancellation | None = None,
    scan: ScanGuard | None = None,
) -> Page:
    check_cancel(cancel)
    absolute = base.store.resolve(uri)
    data = read_file_limited(absolute, MAX_NARRATIVE_BYTES)
    if scan is not None:
        # Charge the inode bytes actually opened, not racy path metadata.
        scan.consume(len(data))
    check_cancel(cancel)
    try:
        modified = datetime.fromtimestamp(absolute.stat().st_mtime, tz=UTC)
    except OSError:
        modified = None
    page = parse_page(uri, data, modified)
    check_cancel(cancel)
    return page


def load_markdown_layer(
    base: Base,
    layer: Layer,
    *,
    cancel: Cancellation | None = None,
    scan: ScanGuard | None = None,
    metadata_only: bool = False,
) -> tuple[tuple[Page, ...], tuple[str, ...]]:
    check_cancel(cancel)
    directory = base.store.directory(layer)
    try:
        iterator = os.scandir(directory)
    except FileNotFoundError:
        return (), ()
    except OSError as error:
        raise OSError(f"list {directory}: {error}") from error
    pages: list[Page] = []
    nested: list[str] = []
    with iterator:
        entries = []
        for entry in iterator:
            check_cancel(cancel)
            if scan is not None:
                scan.visit()
            entries.append(entry)
        for entry in sorted(entries, key=lambda item: item.name):
            check_cancel(cancel)
            if entry.is_dir(follow_symlinks=False):
                nested.append(f"{layer}/{entry.name}/")
                continue
            if not entry.name.endswith(MARKDOWN_EXTENSION):
                continue
            uri = f"{layer}/{entry.name}"
            page = read_page(base, uri, cancel=cancel, scan=scan)
            if metadata_only:
                page = replace(page, body="", links=(), headings=())
            pages.append(page)
    return tuple(sorted(pages, key=lambda page: page.uri)), tuple(nested)


def _present_tags(pages: tuple[Page, ...], cancel: Cancellation | None) -> tuple[str, ...]:
    seen: dict[str, str] = {}
    for page in pages:
        check_cancel(cancel)
        for tag in page.tags:
            seen.setdefault(tag.strip().lower(), tag)
    return tuple(sorted(seen.values()))


def _has_tag(tags: tuple[str, ...], wanted: str) -> bool:
    normalized = wanted.strip().lower()
    return any(tag.lower() == normalized for tag in tags)


def _matches_filter(page: Page, filters: PageFilter) -> bool:
    if filters.status and page.status != filters.status:
        return False
    if filters.type and page.type != filters.type:
        return False
    return all(_has_tag(page.tags, wanted) for wanted in filters.tags)


def _validate_filter(pages: tuple[Page, ...], filters: PageFilter, cancel: Cancellation | None) -> None:
    if filters.limit < 0:
        raise ValueError("limit must not be negative")
    if filters.status:
        require_known("status", (filters.status,), PROJECT_STATUSES)
    if filters.tags:
        display = _present_tags(pages, cancel)
        known = {tag.strip().lower() for tag in display}
        for wanted in filters.tags:
            if wanted.strip().lower() not in known:
                raise ConfigError(f"unknown tag {wanted!r}; this base declares {_join_vocabulary(display)}")


def list_pages(
    base: Base,
    layer: Layer,
    filters: PageFilter | None = None,
    *,
    cancel: Cancellation | None = None,
    scan: ScanGuard | None = None,
    metadata_only: bool = False,
) -> PageListing:
    filters = filters or PageFilter()
    pages, _nested = load_markdown_layer(
        base,
        layer,
        cancel=cancel,
        scan=scan,
        metadata_only=metadata_only,
    )
    _validate_filter(pages, filters, cancel)
    selected_pages: list[Page] = []
    for page in pages:
        check_cancel(cancel)
        if _matches_filter(page, filters):
            selected = replace(page, body="", links=(), headings=())
            if scan is not None:
                scan.retain(selected)
            selected_pages.append(selected)
    selected = tuple(selected_pages)
    total = len(selected)
    if filters.limit:
        selected = selected[: filters.limit]
    return PageListing(layer, selected, total)


def build_tag_vocabulary(
    base: Base,
    layer: Layer,
    *,
    cancel: Cancellation | None = None,
    scan: ScanGuard | None = None,
    metadata_only: bool = False,
) -> TagVocabulary:
    pages, _nested = load_markdown_layer(
        base,
        layer,
        cancel=cancel,
        scan=scan,
        metadata_only=metadata_only,
    )
    grouped: dict[str, list[str]] = {}
    untagged: list[str] = []
    for page in pages:
        check_cancel(cancel)
        if not page.tags:
            untagged.append(page.slug)
            continue
        for tag in page.tags:
            grouped.setdefault(tag.strip().lower(), []).append(page.slug)
    counts = [TagCount(tag, len(slugs), tuple(sorted(slugs))) for tag, slugs in grouped.items()]
    counts.sort(key=lambda item: (-item.count, item.tag))
    sorted_untagged = tuple(sorted(untagged))
    if scan is not None:
        # The resource returns tags and untagged slugs, not its scanned input pages.
        for item in counts:
            scan.retain(item)
        for slug in sorted_untagged:
            scan.retain(slug)
    return TagVocabulary(layer, tuple(counts), sorted_untagged, len(pages))


def normalize_terms(terms: tuple[str, ...] | list[str]) -> tuple[str, ...]:
    return tuple(term.strip().lower() for term in terms if term.strip())


def _excerpt_around(body: str, term: str) -> str:
    folded = body.lower()
    index = folded.find(term)
    if index < 0:
        return ""
    radius = 90
    start = max(0, index - radius)
    end = min(len(body), index + len(term) + radius)
    excerpt = " ".join(body[start:end].split())
    if start:
        excerpt = f"…{excerpt}"
    if end < len(body):
        excerpt = f"{excerpt}…"
    return excerpt


def _score_page(page: Page, layer: Layer, terms: tuple[str, ...]) -> SearchHit | None:
    title = " ".join((page.title, page.slug, page.description, *page.aliases)).lower()
    tags = " ".join(page.tags).lower()
    body = page.body.lower()
    score = 0
    excerpt = ""
    for term in terms:
        if term in title:
            points = 10
        elif term in tags:
            points = 6
        elif term in body:
            points = 2
        else:
            return None
        score += points
        if not excerpt:
            excerpt = _excerpt_around(page.body, term)
    return SearchHit(
        uri=page.uri,
        layer=layer,
        slug=page.slug,
        title=page.title,
        type=page.type,
        tags=page.tags,
        score=score,
        matched=terms,
        excerpt=excerpt,
    )


def search_pages(
    base: Base,
    layer: Layer,
    terms: tuple[str, ...] | list[str],
    filters: PageFilter | None = None,
    *,
    cancel: Cancellation | None = None,
) -> SearchResult:
    filters = filters or PageFilter()
    pages, _nested = load_markdown_layer(base, layer, cancel=cancel)
    _validate_filter(pages, filters, cancel)
    normalized = normalize_terms(terms)
    if not normalized:
        raise ValueError("search needs at least one term")
    hits = []
    for page in pages:
        check_cancel(cancel)
        if _matches_filter(page, filters) and (hit := _score_page(page, layer, normalized)):
            hits.append(hit)
    hits.sort(key=lambda hit: (-hit.score, hit.uri))
    if filters.limit:
        hits = hits[: filters.limit]
    return SearchResult(layer, normalized, tuple(hits))


__all__ = [
    "MAX_VOCABULARY_LISTED",
    "PageFilter",
    "PageListing",
    "SearchHit",
    "SearchResult",
    "TagCount",
    "TagVocabulary",
    "build_tag_vocabulary",
    "list_pages",
    "load_markdown_layer",
    "normalize_terms",
    "read_page",
    "require_known",
    "search_pages",
]
