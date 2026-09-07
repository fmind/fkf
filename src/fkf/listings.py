"""Read-only listings over durable evidence and authored task traces."""

from __future__ import annotations

import os
from dataclasses import dataclass, field
from datetime import UTC, datetime
from typing import TYPE_CHECKING

from fkf.base import Base
from fkf.documents import event_document_uri, index_document_uri
from fkf.pages import read_page, require_known
from fkf.process import Cancellation, check_cancel
from fkf.query import Window
from fkf.store import TASK_TRACE_FILE, Layer
from fkf.timeutil import Instant, parse_rfc3339

if TYPE_CHECKING:
    from fkf.scan import ScanGuard


@dataclass(frozen=True, slots=True)
class DayCount:
    source: str
    uri: str
    count: int
    body: bool


@dataclass(frozen=True, slots=True)
class EventDay:
    date: str
    uri: str
    total: int
    sources: tuple[DayCount, ...]


@dataclass(frozen=True, slots=True)
class EventListing:
    window: Window
    days: tuple[EventDay, ...]
    total: int


@dataclass(frozen=True, slots=True)
class IndexEntry:
    name: str
    uri: str
    count: int = field(metadata={"json": "count,omitempty"})
    bytes: int
    collected_at: str
    age_hours: int
    stale: bool = field(default=False, metadata={"json": "stale,omitempty"})


@dataclass(frozen=True, slots=True)
class IndexListing:
    entries: tuple[IndexEntry, ...]
    total: int


@dataclass(frozen=True, slots=True)
class TaskTrace:
    date: str
    slug: str
    uri: str
    title: str
    bytes: int
    page: object = field(repr=False, compare=False, metadata={"json": "-"})


@dataclass(frozen=True, slots=True)
class TaskListing:
    window: Window
    traces: tuple[TaskTrace, ...]


def list_events(
    base: Base,
    window: Window | None = None,
    *,
    source: str = "",
    limit: int = 0,
    cancel: Cancellation | None = None,
    scan: ScanGuard | None = None,
) -> EventListing:
    """Walk event dates before documents and report newest days first."""
    check_cancel(cancel)
    window = window or Window()
    if limit < 0:
        raise ValueError("limit must not be negative")
    if source:
        require_known("source", (source,), base.config.source_names())
    days: list[EventDay] = []
    total = 0
    for value in reversed(base.event_dates(scan=scan)):
        check_cancel(cancel)
        if not window.contains(value):
            continue
        sources: list[DayCount] = []
        day_total = 0
        for name in base.day_documents(value, scan=scan):
            check_cancel(cancel)
            if source and name != source:
                continue
            uri = event_document_uri(value, name)
            document = base.read_document(uri, scan=scan)
            sources.append(DayCount(name, document.uri(), document.count, document.body))
            day_total += document.count
        if not sources:
            continue
        day = EventDay(value, f"events/{value}/", day_total, tuple(sources))
        if scan is not None:
            scan.retain(day)
        days.append(day)
        total += day_total
        if limit and len(days) >= limit:
            break
    return EventListing(window, tuple(days), total)


def _clock_instant(now: datetime) -> Instant:
    return Instant.from_datetime(now.replace(tzinfo=UTC) if now.tzinfo is None else now)


def list_index(
    base: Base,
    *,
    limit: int = 0,
    cancel: Cancellation | None = None,
    scan: ScanGuard | None = None,
) -> IndexListing:
    """Describe point-in-time evidence freshness from collected_at, never mtime."""
    check_cancel(cancel)
    if limit < 0:
        raise ValueError("limit must not be negative")
    names = base.index_documents(scan=scan)
    entries: list[IndexEntry] = []
    now = _clock_instant(base.now())
    for name in names[:limit] if limit else names:
        check_cancel(cancel)
        uri = index_document_uri(name)
        try:
            size = base.store.resolve(uri).stat().st_size
        except OSError as error:
            raise OSError(f"inspect {uri}: {error}") from error
        document = base.read_document(uri, scan=scan)
        try:
            collected = parse_rfc3339(document.collected_at)
        except ValueError as error:
            raise ValueError(f"parse {uri} collected_at: {error}") from error
        source = base.config.sources.get(name)
        max_age_hours = base.config.sync.index_max_age_hours
        if source is not None and source.layer is Layer.INDEX:
            max_age_hours = source.effective_max_age_hours(max_age_hours)
        age_ns = now.unix_nanoseconds - collected.unix_nanoseconds
        age_hours = max(0, int(age_ns / 3_600_000_000_000))
        stale = age_ns < 0 or age_ns >= max_age_hours * 3_600_000_000_000
        entry = IndexEntry(
            name=name,
            uri=uri,
            count=document.count,
            bytes=size,
            collected_at=document.collected_at,
            age_hours=age_hours,
            stale=stale,
        )
        if scan is not None:
            scan.retain(entry)
        entries.append(entry)
    return IndexListing(tuple(entries), len(names))


def _subdirectories(
    directory: os.PathLike[str] | str,
    cancel: Cancellation | None = None,
    scan: ScanGuard | None = None,
) -> tuple[str, ...]:
    try:
        iterator = os.scandir(directory)
    except FileNotFoundError:
        return ()
    except OSError as error:
        raise OSError(f"list {directory}: {error}") from error
    names: list[str] = []
    with iterator:
        for entry in iterator:
            check_cancel(cancel)
            if scan is not None:
                scan.visit()
            if entry.is_dir(follow_symlinks=False):
                names.append(entry.name)
    return tuple(sorted(names))


def _date_directories(
    directory: os.PathLike[str] | str,
    cancel: Cancellation | None = None,
    scan: ScanGuard | None = None,
) -> tuple[str, ...]:
    dates: list[str] = []
    for name in _subdirectories(directory, cancel, scan):
        check_cancel(cancel)
        try:
            parsed = datetime.strptime(name, "%Y-%m-%d").date()
        except ValueError:
            continue
        if parsed.isoformat() == name:
            dates.append(name)
    return tuple(dates)


def list_tasks(
    base: Base,
    window: Window | None = None,
    *,
    limit: int = 0,
    cancel: Cancellation | None = None,
    scan: ScanGuard | None = None,
    metadata_only: bool = False,
) -> TaskListing:
    """List task traces newest-day first and slug-stable within each day."""
    check_cancel(cancel)
    window = window or Window()
    if limit < 0:
        raise ValueError("limit must not be negative")
    base.require_layer(Layer.TASKS)
    traces: list[TaskTrace] = []
    for value in reversed(_date_directories(base.store.directory(Layer.TASKS), cancel, scan)):
        check_cancel(cancel)
        if not window.contains(value):
            continue
        for slug in _subdirectories(base.store.resolve(f"tasks/{value}"), cancel, scan):
            check_cancel(cancel)
            uri = f"tasks/{value}/{slug}/{TASK_TRACE_FILE}"
            if not base.exists(uri):
                continue
            page = read_page(base, uri, cancel=cancel, scan=scan)
            trace = TaskTrace(value, slug, uri, page.title, page.bytes, None if metadata_only else page)
            if scan is not None:
                scan.retain(trace)
            traces.append(trace)
            if limit and len(traces) >= limit:
                return TaskListing(window, tuple(traces))
    return TaskListing(window, tuple(traces))


__all__ = [
    "DayCount",
    "EventDay",
    "EventListing",
    "IndexEntry",
    "IndexListing",
    "TaskListing",
    "TaskTrace",
    "list_events",
    "list_index",
    "list_tasks",
]
