"""Bounded offline day, timeline, brief, and identity retrieval."""

from __future__ import annotations

import copy
import hashlib
from collections.abc import Mapping, Sequence
from dataclasses import dataclass, field, replace
from datetime import UTC, date, datetime, timedelta
from functools import cmp_to_key
from typing import Final, cast

from fkf import DISPLAY_VERSION
from fkf.base import Base
from fkf.config import ConfigError, IdentityKind
from fkf.errors import InvalidUsageError
from fkf.fields import scalar_string
from fkf.find import NO_FIND_LIMIT, FindFilter, RecordHit, SourceCount, _record_hit, find
from fkf.graph import (
    Direction,
    GraphQuery,
    IdentityResolver,
    NeighbourEdge,
    ResolvedIdentity,
    neighbours_from_cache,
    node_kind,
    open_validated_graph_cache,
)
from fkf.jsoncodec import dumps
from fkf.listings import list_tasks
from fkf.markdown import Page
from fkf.pages import load_markdown_layer, read_page, require_known
from fkf.process import Cancellation, CommandCanceledError
from fkf.query import TemporalQuery, Window, parse_temporal_query, parse_window
from fkf.status import Status, StatusRequest
from fkf.status import report as status_report
from fkf.store import MAX_NARRATIVE_BYTES, Layer
from fkf.timeutil import DurationNS, format_duration, parse_duration, parse_record_time, parse_rfc3339
from fkf.uri import Scheme, parse_uri

DEFAULT_DIGEST_BUDGET: Final = 600
MAX_DIGEST_BUDGET: Final = MAX_NARRATIVE_BYTES // 4
DEFAULT_BRIEF_BUDGET: Final = 1_200
MAX_BRIEF_BUDGET: Final = MAX_NARRATIVE_BYTES // 4

DIGEST_DELIVERY_JSON: Final = "json"
DIGEST_DELIVERY_JSONL: Final = "jsonl"
DIGEST_DELIVERY_TEXT: Final = "text"
DIGEST_DELIVERY_COMPACT_JSON: Final = "json-compact"

_DIGEST_SUMMARY_THRESHOLD: Final = 6
_DIGEST_CORE_PRIORITY: Final = 300
_DEFAULT_AROUND: Final = parse_duration("2h")
_BRIEF_VERSION: Final = 2
_WHO_EXPANSION_LIMIT: Final = 200
_MAX_WHO_RECENT: Final = 10


class DigestBudgetError(InvalidUsageError):
    """The requested budget cannot carry even the complete minimal receipt."""

    def __init__(self, requested: int, minimum: int) -> None:
        self.requested = requested
        self.minimum = minimum
        super().__init__(f"digest budget {requested} is too small; minimum for this receipt is {minimum}")


class BriefBudgetError(InvalidUsageError):
    """The requested brief budget cannot carry its complete minimal receipt."""

    def __init__(self, requested: int, minimum: int) -> None:
        self.requested = requested
        self.minimum = minimum
        super().__init__(f"brief budget {requested} is too small; minimum for this receipt is {minimum}")


@dataclass(slots=True)
class DigestItem:
    """One chronologically placed timeline line."""

    time: str = field(default="", metadata={"json": "time,omitempty"})
    uri: str = ""
    title: str = ""
    count: int = field(default=0, metadata={"json": "count,omitempty"})
    _priority: int = field(default=0, repr=False, compare=False, metadata={"json": "-"})


@dataclass(slots=True)
class DigestGroup:
    """One source's complete contribution to a timeline."""

    source: str
    count: int = 0
    summarized: bool = field(default=False, metadata={"json": "summarized,omitempty"})
    items: list[DigestItem] = field(default_factory=list, metadata={"json": "items,omitempty"})
    _priority: int = field(default=0, repr=False, compare=False, metadata={"json": "-"})


@dataclass(slots=True)
class DigestReceipt:
    """Reproducibility and complete omission accounting for one timeline."""

    base: str
    window: Window
    budget: int
    format: str
    used_tokens: int = 0
    json_tokens: int = 0
    text_tokens: int = 0
    records: int = 0
    selected: int = 0
    dropped: int = field(default=0, metadata={"json": "dropped,omitempty"})
    people: int = field(default=0, metadata={"json": "people,omitempty"})
    dropped_people: int = field(default=0, metadata={"json": "dropped_people,omitempty"})
    repositories: int = field(default=0, metadata={"json": "repositories,omitempty"})
    dropped_repositories: int = field(default=0, metadata={"json": "dropped_repositories,omitempty"})
    input_digest: str = ""
    as_of: str = ""
    sources: tuple[str, ...] = field(default=(), metadata={"json": "sources,omitempty"})
    repository: str = field(default="", metadata={"json": "repository,omitempty"})
    person: str = field(default="", metadata={"json": "person,omitempty"})
    around: str = field(default="", metadata={"json": "around,omitempty"})
    around_window: str = field(default="", metadata={"json": "around_window,omitempty"})


@dataclass(slots=True)
class TimelineReport:
    """The common bounded response for one-day and range digests."""

    groups: list[DigestGroup]
    people: list[str] = field(default_factory=list, metadata={"json": "people,omitempty"})
    repositories: list[str] = field(default_factory=list, metadata={"json": "repositories,omitempty"})
    receipt: DigestReceipt = field(kw_only=True)
    _people_priority: dict[str, int] = field(default_factory=dict, repr=False, compare=False, metadata={"json": "-"})
    _repository_priority: dict[str, int] = field(
        default_factory=dict,
        repr=False,
        compare=False,
        metadata={"json": "-"},
    )


@dataclass(frozen=True, slots=True)
class DayRequest:
    """Select one local calendar day."""

    date: str = ""
    budget: int = 0
    all: bool = False
    delivery_format: str = ""


@dataclass(frozen=True, slots=True)
class TimelineRequest:
    """Select a dated range or records around one stored record URI."""

    window: Window = field(default_factory=Window)
    sources: tuple[str, ...] = ()
    repository: str = ""
    person: str = ""
    around_uri: str = ""
    around: DurationNS = field(default_factory=lambda: DurationNS(0))
    budget: int = 0
    all: bool = False
    delivery_format: str = ""
    _base_name: str = field(default="", repr=False, compare=False, metadata={"json": "-"})

    def __post_init__(self) -> None:
        object.__setattr__(self, "sources", tuple(self.sources))
        object.__setattr__(self, "around", DurationNS(self.around))


def _check_canceled(cancel: Cancellation | None) -> None:
    if cancel is not None and cancel.is_set():
        raise CommandCanceledError("command canceled")


def _aware_now(value: datetime) -> datetime:
    return value.replace(tzinfo=UTC) if value.tzinfo is None else value


def _resolve_digest_day(value: str, now: datetime) -> str:
    normalized = value.strip().lower() or "today"
    try:
        window = parse_window(normalized, normalized, now)
    except ValueError as error:
        raise ConfigError(f"day {value!r}: {error}", cause=error) from error
    if window.since != window.until:
        raise ConfigError(f"day {value!r} does not resolve to one date")
    return window.since


def _digest_day_origin(value: str) -> str:
    return value.strip().lower() or "today"


def day(base: Base, request: DayRequest | None = None, *, cancel: Cancellation | None = None) -> TimelineReport:
    """Render one local calendar day using a single base-clock read."""

    selected = request or DayRequest()
    now = _aware_now(base.now())
    value = _resolve_digest_day(selected.date, now)
    return _timeline_at(
        base,
        TimelineRequest(
            window=Window(value, value, _digest_day_origin(selected.date)),
            budget=selected.budget,
            all=selected.all,
            delivery_format=selected.delivery_format,
        ),
        now,
        cancel,
    )


def timeline(
    base: Base,
    request: TimelineRequest,
    *,
    cancel: Cancellation | None = None,
) -> TimelineReport:
    """Render a chronological, relation-filtered range without executing a source."""

    return _timeline_at(base, request, _aware_now(base.now()), cancel)


def _timeline_at(
    base: Base,
    request: TimelineRequest,
    now: datetime,
    cancel: Cancellation | None,
) -> TimelineReport:
    _check_canceled(cancel)
    base.require_layer(Layer.EVENTS)
    resolver = IdentityResolver.load(base, cancel=cancel)
    selected = replace(
        request,
        repository=resolver.canonical(request.repository) if request.repository else "",
        person=resolver.canonical(request.person) if request.person else "",
        _base_name=base.config.name,
    )
    selected = _validate_timeline_request(base, resolver, selected)
    try:
        parsed = parse_window(selected.window.since, selected.window.until, now)
    except ValueError as error:
        raise ConfigError(str(error), cause=error) from error
    window = Window(parsed.since, parsed.until, selected.window.derived_from)

    lower: int | None = None
    upper: int | None = None
    if selected.around_uri:
        around = selected.around or _DEFAULT_AROUND
        center = _record_time_for_uri(base, selected.around_uri)
        lower = center - int(around)
        upper = center + int(around)
        location = now.tzinfo or UTC
        lower_day = parse_record_time(str(lower)).to_datetime().astimezone(location).date().isoformat()
        upper_day = parse_record_time(str(upper)).to_datetime().astimezone(location).date().isoformat()
        window = Window(lower_day, upper_day, f"--around {format_duration(around)}")
        selected = replace(selected, around=around)

    found = find(
        base,
        FindFilter(sources=selected.sources, layers=(Layer.EVENTS,), window=window, limit=NO_FIND_LIMIT),
        cancel=cancel,
    )
    records: list[RecordHit] = []
    for record in found.records:
        _check_canceled(cancel)
        if not _record_matches_relations(record, selected.repository, selected.person):
            continue
        if lower is not None and upper is not None and not _record_within(record, lower, upper):
            continue
        records.append(record)
    return _build_timeline_report(records, selected, window, now, resolver)


def _validate_timeline_request(base: Base, resolver: IdentityResolver, request: TimelineRequest) -> TimelineRequest:
    if request.budget < 0 or request.budget > MAX_DIGEST_BUDGET:
        raise ConfigError(f"--budget must be between 1 and {MAX_DIGEST_BUDGET}")
    if request.around < 0:
        raise ConfigError("--around must be a positive duration")
    if request.around_uri and (request.window.since or request.window.until):
        raise ConfigError("a record URI with --around cannot be combined with --since or --until")
    if not request.around_uri and request.around:
        raise ConfigError("--around needs one record URI")
    if not request.around_uri and not request.window.since:
        raise ConfigError("timeline needs --since, or one record URI with --around")
    delivery = request.delivery_format or DIGEST_DELIVERY_JSON
    if delivery not in {
        DIGEST_DELIVERY_JSON,
        DIGEST_DELIVERY_JSONL,
        DIGEST_DELIVERY_TEXT,
        DIGEST_DELIVERY_COMPACT_JSON,
    }:
        raise ConfigError(f"digest delivery format {delivery!r} is not json, jsonl, text, or json-compact")
    _validate_digest_entity(resolver, "--repo", request.repository, IdentityKind.REPOSITORY)
    _validate_digest_entity(resolver, "--person", request.person, IdentityKind.PERSON)
    sources = tuple(sorted(set(request.sources)))
    require_known("source", sources, base.config.source_names())
    return replace(request, sources=sources, delivery_format=delivery)


def _digest_identity_kind(resolver: IdentityResolver, value: str) -> IdentityKind | None:
    if kind := resolver.kind(value):
        return kind
    scheme = value.partition(":")[0]
    if scheme in {"person", "actor"}:
        return IdentityKind.PERSON
    if scheme in {"organization", "org"}:
        return IdentityKind.ORGANIZATION
    if scheme in {"repository", "repo"}:
        return IdentityKind.REPOSITORY
    return None


def _validate_digest_entity(
    resolver: IdentityResolver,
    flag: str,
    value: str,
    expected: IdentityKind,
) -> None:
    if not value:
        return
    try:
        parsed = parse_uri(value)
    except ValueError as error:
        raise ConfigError(
            f"{flag} needs an entity URI such as repo:github.com/fmind/fkf or person:email/name@example.test",
            cause=error,
        ) from error
    if not parsed.is_entity():
        raise ConfigError(
            f"{flag} needs an entity URI such as repo:github.com/fmind/fkf or person:email/name@example.test"
        )
    kind = _digest_identity_kind(resolver, value)
    if kind is not expected:
        raise ConfigError(f"{flag} needs a {expected} identity; {value!r} is {kind or 'unclassified'}")


def _record_time_for_uri(base: Base, raw: str) -> int:
    try:
        uri = parse_uri(raw)
    except ValueError as error:
        raise ConfigError(str(error), cause=error) from error
    if uri.scheme != Scheme.FILE or not uri.fragment or uri.jq or not uri.path.endswith(".json"):
        raise ConfigError("timeline --around needs one stored record URI")
    document = base.read_document(uri.path)
    record = document.find_record(uri.fragment)
    if record is None:
        raise ConfigError(f"{uri.path} holds no record with id {uri.fragment!r}")
    value = document.fields.eval_string("time", record)
    if value is None:
        raise ConfigError(f"{raw} has no event time")
    try:
        return parse_record_time(value).unix_nanoseconds
    except ValueError as error:
        raise ConfigError(f"{raw} event time: {error}", cause=error) from error


def _record_within(record: RecordHit, lower: int, upper: int) -> bool:
    try:
        value = parse_record_time(record.time).unix_nanoseconds
    except ValueError:
        return False
    return lower <= value <= upper


def _record_relation_values(record: RecordHit) -> list[str]:
    if not record.fields:
        return []
    return [
        value
        for name in sorted(record.fields)
        if name in record._relation_fields  # noqa: SLF001 - the projection carries its schema contract privately
        for value in record.fields[name]
    ]


def _record_matches_relations(record: RecordHit, repository: str, person: str) -> bool:
    values = _record_relation_values(record)
    return (not repository or repository in values) and (not person or person in values)


def _compare_timeline_records(left: RecordHit, right: RecordHit) -> int:
    try:
        left_time = parse_record_time(left.time).unix_nanoseconds
        right_time = parse_record_time(right.time).unix_nanoseconds
    except ValueError:
        left_time = right_time = 0
    if left_time != right_time:
        return -1 if left_time < right_time else 1
    if left.time != right.time:
        return -1 if left.time < right.time else 1
    return (left.uri > right.uri) - (left.uri < right.uri)


def _one_line(value: str) -> str:
    return " ".join(value.split())


def _record_title(record: RecordHit) -> str:
    return _one_line(record.title) or record.uri


def _record_priority(record: RecordHit) -> int:
    return (
        _DIGEST_CORE_PRIORITY
        + (50 if record.title else 0)
        + (25 if record.time else 0)
        + min(25, len(record._relation_fields) * 5)  # noqa: SLF001 - projected schema is part of the digest
    )


def _group_records(records: Sequence[RecordHit], *, all_records: bool) -> list[DigestGroup]:
    counts: dict[str, int] = {}
    for record in records:
        counts[record.source] = counts.get(record.source, 0) + 1
    groups: list[DigestGroup] = []
    by_source: dict[str, tuple[DigestGroup, dict[str, int]]] = {}
    for record in records:
        current = by_source.get(record.source)
        if current is None:
            group = DigestGroup(
                record.source,
                summarized=not all_records and counts[record.source] >= _DIGEST_SUMMARY_THRESHOLD,
            )
            current = (group, {})
            by_source[record.source] = current
            groups.append(group)
        group, titles = current
        group.count += 1
        if group.summarized:
            continue
        title = _record_title(record)
        priority = _record_priority(record)
        index = titles.get(title)
        if index is not None:
            item = group.items[index]
            item.count = 2 if item.count == 0 else item.count + 1
            item._priority = max(item._priority, priority)  # noqa: SLF001
            continue
        titles[title] = len(group.items)
        group.items.append(DigestItem(record.time, record.uri, title, _priority=priority))
        group._priority = max(group._priority, priority)  # noqa: SLF001
    return groups


def _digest_entities(
    records: Sequence[RecordHit],
    resolver: IdentityResolver,
    *,
    include_owner: bool,
) -> tuple[list[str], list[str], dict[str, int], dict[str, int]]:
    people: dict[str, int] = {}
    repositories: dict[str, int] = {}
    for record in records:
        priority = _record_priority(record)
        for raw in _record_relation_values(record):
            value = resolver.canonical(raw)
            kind = _digest_identity_kind(resolver, value)
            if kind is IdentityKind.PERSON and (include_owner or not resolver.is_owner(value)):
                people[value] = max(people.get(value, 0), priority)
            elif kind is IdentityKind.REPOSITORY:
                repositories[value] = max(repositories.get(value, 0), priority)

    def ordered(values: Mapping[str, int]) -> list[str]:
        return sorted(values, key=lambda value: (-values[value], value))

    return ordered(people), ordered(repositories), people, repositories


@dataclass(frozen=True, slots=True)
class _DigestInputRecord:
    uri: str = field(metadata={"json": "URI"})
    source: str = field(metadata={"json": "Source"})
    time: str = field(metadata={"json": "Time"})
    title: str = field(metadata={"json": "Title"})
    group_title: str = field(metadata={"json": "GroupTitle"})
    group_key: str = field(metadata={"json": "GroupKey"})
    priority: int = field(metadata={"json": "Priority"})
    fields: Mapping[str, tuple[str, ...]] | None = field(metadata={"json": "Fields"})
    relations: tuple[str, ...] = field(metadata={"json": "Relations"})


@dataclass(frozen=True, slots=True)
class _DigestInput:
    base: str = field(metadata={"json": "Base"})
    window: Window = field(metadata={"json": "Window"})
    as_of: str = field(metadata={"json": "AsOf"})
    sources: tuple[str, ...] | None = field(metadata={"json": "Sources"})
    repository: str = field(metadata={"json": "Repository"})
    person: str = field(metadata={"json": "Person"})
    around_uri: str = field(metadata={"json": "AroundURI"})
    around: str = field(metadata={"json": "Around"})
    budget: int = field(metadata={"json": "Budget"})
    all: bool = field(metadata={"json": "All"})
    delivery_format: str = field(metadata={"json": "DeliveryFormat"})
    people: tuple[str, ...] = field(metadata={"json": "People"})
    repositories: tuple[str, ...] = field(metadata={"json": "Repositories"})
    records: tuple[_DigestInputRecord, ...] = field(metadata={"json": "Records"})


def _input_digest(
    records: Sequence[RecordHit],
    request: TimelineRequest,
    window: Window,
    as_of: str,
    people: Sequence[str],
    repositories: Sequence[str],
) -> str:
    projected = tuple(
        _DigestInputRecord(
            record.uri,
            record.source,
            record.time,
            record.title,
            _record_title(record),
            _record_title(record),
            _record_priority(record),
            record.fields,
            tuple(sorted(record._relation_fields)),  # noqa: SLF001 - digest binds relation schema
        )
        for record in records
    )
    payload = _DigestInput(
        request._base_name,  # noqa: SLF001 - normalized internal request field
        window,
        as_of,
        request.sources or None,
        request.repository,
        request.person,
        request.around_uri,
        format_duration(request.around),
        request.budget,
        request.all,
        request.delivery_format,
        tuple(people),
        tuple(repositories),
        projected,
    )
    # Go's encoding/json escapes HTML-significant bytes before hashing; the receipt
    # digest is a permanent cross-runtime contract even though delivery JSON does not.
    encoded = dumps(payload).replace(b"&", b"\\u0026").replace(b"<", b"\\u003c").replace(b">", b"\\u003e")
    return hashlib.sha256(encoded).hexdigest()


def _build_timeline_report(
    records: list[RecordHit],
    request: TimelineRequest,
    window: Window,
    now: datetime,
    resolver: IdentityResolver,
) -> TimelineReport:
    records.sort(key=cmp_to_key(_compare_timeline_records))
    budget = request.budget or DEFAULT_DIGEST_BUDGET
    request = replace(request, budget=budget)
    people, repositories, people_priority, repository_priority = _digest_entities(
        records,
        resolver,
        include_owner=bool(request.person and resolver.is_owner(request.person)),
    )
    receipt = DigestReceipt(
        request._base_name,  # noqa: SLF001 - normalized internal request field
        window,
        budget,
        request.delivery_format,
        records=len(records),
        as_of=now.date().isoformat(),
        people=len(people),
        repositories=len(repositories),
        sources=request.sources,
        repository=request.repository,
        person=request.person,
        around=request.around_uri,
        around_window=format_duration(request.around) if request.around_uri else "",
    )
    report = TimelineReport(
        _group_records(records, all_records=request.all),
        people,
        repositories,
        people_priority,
        repository_priority,
        receipt=receipt,
    )
    receipt.input_digest = _input_digest(records, request, window, receipt.as_of, people, repositories)
    minimum = _minimum_timeline_budget(report)
    if budget < minimum:
        raise DigestBudgetError(budget, minimum)
    while True:
        _account_timeline(report)
        if len(encode_timeline_delivery(report)) <= budget * 4:
            return report
        if not _trim_timeline(report):
            raise DigestBudgetError(budget, minimum)


def _item_count(item: DigestItem) -> int:
    return max(1, item.count)


def _selected_records(groups: Sequence[DigestGroup]) -> int:
    return sum(group.count if group.summarized else sum(_item_count(item) for item in group.items) for group in groups)


def _receipt(report: TimelineReport) -> DigestReceipt:
    return report.receipt


def _account_timeline(report: TimelineReport) -> None:
    receipt = _receipt(report)
    receipt.selected = _selected_records(report.groups)
    receipt.dropped = receipt.records - receipt.selected
    receipt.dropped_people = receipt.people - len(report.people)
    receipt.dropped_repositories = receipt.repositories - len(report.repositories)
    for _ in range(12):
        json_tokens = _bytes_to_tokens(len(_encode_timeline_json(report)))
        text_tokens = _bytes_to_tokens(len(render_timeline_text(report).encode()))
        used = _bytes_to_tokens(len(encode_timeline_delivery(report)))
        if (receipt.json_tokens, receipt.text_tokens, receipt.used_tokens) == (json_tokens, text_tokens, used):
            return
        receipt.json_tokens, receipt.text_tokens, receipt.used_tokens = json_tokens, text_tokens, used


def _minimum_timeline_budget(report: TimelineReport) -> int:
    minimum = copy.deepcopy(report)
    receipt = _receipt(minimum)
    minimum.groups = []
    minimum.people = []
    minimum.repositories = []
    receipt.selected = 0
    receipt.dropped = receipt.records
    receipt.dropped_people = receipt.people
    receipt.dropped_repositories = receipt.repositories
    receipt.budget = 1
    for _ in range(12):
        _account_timeline(minimum)
        needed = receipt.used_tokens
        if needed <= receipt.budget:
            return receipt.budget
        receipt.budget = needed
    return receipt.used_tokens


def _trim_low_priority_group(report: TimelineReport) -> bool:
    selected: tuple[int, int, int] | None = None
    for group_index, group in enumerate(report.groups):
        if group.summarized:
            continue
        for item_index, item in enumerate(group.items):
            candidate = (item._priority, -group_index, -item_index)  # noqa: SLF001
            if item._priority <= _DIGEST_CORE_PRIORITY and (selected is None or candidate < selected):  # noqa: SLF001
                selected = candidate
    if selected is None:
        return False
    _priority, negative_group, negative_item = selected
    group_index = -negative_group
    group = report.groups[group_index]
    item_index = -negative_item
    if len(group.items) > 1:
        del group.items[item_index]
    else:
        del report.groups[group_index]
    return True


def _trim_low_priority_entity(values: list[str], priorities: Mapping[str, int]) -> bool:
    for index in range(len(values) - 1, -1, -1):
        if priorities.get(values[index], 0) < _DIGEST_CORE_PRIORITY:
            del values[index]
            return True
    return False


def _lowest_priority_item(group: DigestGroup) -> int:
    selected = 0
    for candidate in range(1, len(group.items)):
        if group.items[candidate]._priority <= group.items[selected]._priority:  # noqa: SLF001
            selected = candidate
    return selected


def _trim_timeline(report: TimelineReport) -> bool:
    if _trim_low_priority_group(report):
        return True
    if _trim_low_priority_entity(report.repositories, report._repository_priority):  # noqa: SLF001
        return True
    if _trim_low_priority_entity(report.people, report._people_priority):  # noqa: SLF001
        return True
    selected: tuple[int, int, int, int] | None = None
    for group_index, group in enumerate(report.groups):
        if len(group.items) <= 1:
            continue
        item_index = _lowest_priority_item(group)
        candidate = (group.items[item_index]._priority, -len(group.items), -group_index, item_index)  # noqa: SLF001
        if selected is None or candidate < selected:
            selected = candidate
    if selected is not None:
        _priority, _negative_count, negative_group, item_index = selected
        del report.groups[-negative_group].items[item_index]
        return True
    if len(report.repositories) > 1:
        report.repositories.pop()
        return True
    if len(report.people) > 1:
        report.people.pop()
        return True
    if report.groups:
        report.groups.pop()
        return True
    if report.repositories:
        report.repositories.pop()
        return True
    if report.people:
        report.people.pop()
        return True
    return False


def _encode_timeline_json(report: TimelineReport) -> bytes:
    return dumps(report, indent=True, newline=True)


def encode_timeline_delivery(report: TimelineReport) -> bytes:
    """Encode the exact public format named by the receipt."""

    delivery = _receipt(report).format
    if delivery == DIGEST_DELIVERY_TEXT:
        return render_timeline_text(report).encode()
    if delivery == DIGEST_DELIVERY_JSONL:
        return dumps(report, newline=True)
    if delivery == DIGEST_DELIVERY_COMPACT_JSON:
        return dumps(report)
    return _encode_timeline_json(report)


def _qualified_citation(base_name: str, uri: str) -> str:
    if not base_name or not uri or uri.startswith("fkf://"):
        return uri
    return f"fkf://{base_name}/{uri}"


def render_timeline_text(report: TimelineReport) -> str:
    """Render the canonical compact timeline representation."""

    receipt = _receipt(report)
    output: list[str] = []
    for group in report.groups:
        if group.summarized:
            output.append(f"[{group.source}] {group.count} records summarized")
            continue
        shown = sum(_item_count(item) for item in group.items)
        output.append(f"[{group.source}] {shown}/{group.count} records")
        for item in group.items:
            count = f" x{item.count}" if item.count > 1 else ""
            output.append(f"{item.time} {item.title}{count} · {_qualified_citation(receipt.base, item.uri)}")
    if report.people:
        output.append(f"people: {', '.join(report.people)}")
    if report.repositories:
        output.append(f"repositories: {', '.join(report.repositories)}")
    output.append(
        f"receipt: {receipt.window.since}..{receipt.window.until} · records {receipt.records} · "
        f"selected {receipt.selected} · dropped {receipt.dropped} · base {receipt.base}"
    )
    output.append(
        f"receipt: budget {receipt.budget} · used {receipt.used_tokens} · "
        f"json {receipt.json_tokens} · text {receipt.text_tokens}"
    )
    output.append(f"receipt: as_of {receipt.as_of} · input_sha256 {receipt.input_digest}")
    return "\n".join(output) + "\n"


def _bytes_to_tokens(size: int) -> int:
    return (size + 3) // 4


@dataclass(frozen=True, slots=True)
class WhoNeighbourGroup:
    """Adjacent canonical nodes grouped by their stable classification."""

    kind: str
    nodes: tuple[str, ...]


@dataclass(frozen=True, slots=True)
class WhoMatch:
    """One resolved identity joined to pages, graph neighbours, and records."""

    canonical: str
    kind: str = field(default="", metadata={"json": "kind,omitempty"})
    owner: bool = field(default=False, metadata={"json": "owner,omitempty"})
    aliases: tuple[str, ...] = ()
    names: tuple[str, ...] = field(default=(), metadata={"json": "names,omitempty"})
    pages: tuple[Page, ...] = field(default=(), metadata={"json": "pages,omitempty"})
    neighbourhood: tuple[WhoNeighbourGroup, ...] = field(
        default=(),
        metadata={"json": "neighbourhood,omitempty"},
    )
    neighbourhood_truncated: bool = field(
        default=False,
        metadata={"json": "neighbourhood_truncated,omitempty"},
    )
    counts: tuple[SourceCount, ...] = ()
    recent: tuple[RecordHit, ...] = ()
    total: int = 0


@dataclass(frozen=True, slots=True)
class WhoReport:
    """Every declared identity component matching one human spelling or URI."""

    query: str
    matches: tuple[WhoMatch, ...]


def who(base: Base, query: str, *, cancel: Cancellation | None = None) -> WhoReport:
    """Answer an identity question solely from stored evidence and the validated graph."""

    query = query.strip()
    if not query:
        raise ConfigError("who needs a name or URI")
    _check_canceled(cancel)
    resolver = IdentityResolver.load(base, cancel=cancel)
    matches = tuple(
        _build_who_match(base, resolver, identity, cancel) for identity in _match_identities(resolver, query)
    )
    return WhoReport(query, matches)


def _match_identities(resolver: IdentityResolver, query: str) -> tuple[ResolvedIdentity, ...]:
    exact = resolver.exact(query)
    if exact is not None:
        return (exact,)
    needle = query.strip().lower()
    return tuple(
        identity
        for identity in resolver.identities()
        if any(
            needle in candidate.strip().lower()
            for candidate in (identity.canonical, *identity.aliases, *identity.names)
        )
    )


def _build_who_match(
    base: Base,
    resolver: IdentityResolver,
    identity: ResolvedIdentity,
    cancel: Cancellation | None,
) -> WhoMatch:
    pages: list[Page] = []
    for uri in identity.pages:
        _check_canceled(cancel)
        pages.append(read_page(base, uri, cancel=cancel))

    found = find(base, FindFilter(grep=(identity.canonical,), limit=NO_FIND_LIMIT), cancel=cancel)
    direct: dict[str, None] = {}
    records: list[RecordHit] = []
    direct_newest_first: list[str] = []
    for record in found.records:
        _check_canceled(cancel)
        if not _record_names_identity(record, identity.canonical, resolver):
            continue
        records.append(replace(record, raw=None))
        direct[record.uri] = None
        direct_newest_first.append(record.uri)

    neighbourhood, linked, truncated = _who_linked_record_uris(
        base,
        identity.canonical,
        direct,
        direct_newest_first,
        cancel,
    )
    for uri in linked:
        _check_canceled(cancel)
        if uri in direct:
            continue
        records.append(_read_who_record(base, uri, resolver))
        direct[uri] = None
    records.sort(key=lambda record: record.uri)
    records.sort(key=lambda record: record.time, reverse=True)
    counts: dict[str, int] = {}
    for record in records:
        counts[record.source] = counts.get(record.source, 0) + 1
    return WhoMatch(
        canonical=identity.canonical,
        kind=str(identity.kind) if identity.kind is not None else "",
        owner=identity.owner,
        aliases=identity.aliases,
        names=identity.names,
        pages=tuple(pages),
        neighbourhood=_group_who_neighbours(identity.canonical, neighbourhood, resolver),
        neighbourhood_truncated=truncated,
        counts=tuple(SourceCount(source, counts[source]) for source in sorted(counts)),
        recent=tuple(records[:_MAX_WHO_RECENT]),
        total=len(records),
    )


def _record_names_identity(record: RecordHit, canonical: str, resolver: IdentityResolver) -> bool:
    return any(resolver.canonical(value) == canonical for value in _record_relation_values(record))


def _who_linked_record_uris(
    base: Base,
    canonical: str,
    direct: Mapping[str, None],
    direct_newest_first: Sequence[str],
    cancel: Cancellation | None,
) -> tuple[tuple[NeighbourEdge, ...], tuple[str, ...], bool]:
    with open_validated_graph_cache(base, cancel=cancel) as cache:
        neighbourhood = neighbours_from_cache(
            cache,
            GraphQuery(canonical, direction=Direction.BOTH, depth=1, limit=_WHO_EXPANSION_LIMIT),
            cancel=cancel,
        )
        linked: set[str] = set()
        seen_edges: set[tuple[str, str, str, str, str]] = set()
        truncated = neighbourhood.truncated
        for index, seed in enumerate(direct_newest_first):
            _check_canceled(cancel)
            if index >= _WHO_EXPANSION_LIMIT or len(seen_edges) >= _WHO_EXPANSION_LIMIT:
                truncated = True
                break
            adjacent = neighbours_from_cache(
                cache,
                GraphQuery(seed, direction=Direction.BOTH, depth=1, limit=_WHO_EXPANSION_LIMIT),
                cancel=cancel,
            )
            truncated = truncated or adjacent.truncated
            for neighbour in adjacent.edges:
                key = neighbour.edge.sort_key()
                if key in seen_edges:
                    continue
                if len(seen_edges) >= _WHO_EXPANSION_LIMIT:
                    truncated = True
                    break
                seen_edges.add(key)
                for endpoint in (neighbour.src, neighbour.dst):
                    if endpoint != seed and endpoint not in direct and _is_who_record_uri(base, endpoint):
                        linked.add(endpoint)
        cache.revalidate_bytes()
    return neighbourhood.edges, tuple(sorted(linked)), truncated


def _is_who_record_uri(base: Base, raw: str) -> bool:
    try:
        uri = parse_uri(raw)
    except ValueError:
        return False
    if uri.scheme != Scheme.FILE or not uri.fragment or uri.jq or not uri.path.endswith(".json"):
        return False
    return base.store.layer_of(uri.path) in {Layer.EVENTS, Layer.INDEX}


def _read_who_record(base: Base, raw: str, resolver: IdentityResolver) -> RecordHit:
    uri = parse_uri(raw)
    document = base.read_document(uri.path)
    record = document.find_record(uri.fragment)
    if record is None:
        raise ConfigError(f"identity-linked document {uri.path} holds no record with id {uri.fragment!r}")
    return replace(_record_hit(document, record).canonicalized(resolver), raw=None)


def _group_who_neighbours(
    canonical: str,
    edges: Sequence[NeighbourEdge],
    resolver: IdentityResolver,
) -> tuple[WhoNeighbourGroup, ...]:
    by_kind: dict[str, set[str]] = {}
    for neighbour in edges:
        other = neighbour.src if resolver.canonical(neighbour.dst) == canonical else neighbour.dst
        other = resolver.canonical(other)
        if other == canonical:
            continue
        identity_kind = resolver.kind(other)
        kind = str(identity_kind) if identity_kind is not None else node_kind(other)
        by_kind.setdefault(kind, set()).add(other)
    return tuple(WhoNeighbourGroup(kind, tuple(sorted(by_kind[kind]))) for kind in sorted(by_kind))


@dataclass(slots=True)
class BriefItem:
    """One actionable or citable brief line."""

    uri: str = field(default="", metadata={"json": "uri,omitempty"})
    time: str = field(default="", metadata={"json": "time,omitempty"})
    title: str = ""
    detail: str = field(default="", metadata={"json": "detail,omitempty"})
    count: int = field(default=0, metadata={"json": "count,omitempty"})


@dataclass(slots=True)
class BriefSection:
    """One stable section with its complete pre-budget total."""

    name: str
    title: str
    total: int = 0
    items: list[BriefItem] = field(default_factory=list)


@dataclass(slots=True)
class BriefReceipt:
    """Complete offline JSON/text budget and provenance accounting."""

    base: str
    budget: int
    used_tokens: int = 0
    json_tokens: int = 0
    text_tokens: int = 0
    candidates: int = 0
    selected: int = 0
    dropped: int = field(default=0, metadata={"json": "dropped,omitempty"})
    as_of: str = ""
    input_digest: str = ""
    owner: str = field(default="", metadata={"json": "owner,omitempty"})
    stale_sources: tuple[str, ...] = field(default=(), metadata={"json": "stale_sources,omitempty"})
    unharvested: int = field(default=0, metadata={"json": "unharvested,omitempty"})
    brief_version: int = 0
    tool_version: str = ""


@dataclass(slots=True)
class BriefReport:
    """The bounded daily control surface."""

    sections: list[BriefSection]
    receipt: BriefReceipt


@dataclass(frozen=True, slots=True)
class BriefRequest:
    """Select the complete daily brief."""

    budget: int = 0


def brief(base: Base, request: BriefRequest | None = None, *, cancel: Cancellation | None = None) -> BriefReport:
    """Compose stored evidence, source health, and authored work without provider execution."""

    _check_canceled(cancel)
    selected = request or BriefRequest()
    budget = selected.budget or DEFAULT_BRIEF_BUDGET
    if budget < 1 or budget > MAX_BRIEF_BUDGET:
        raise ConfigError(f"--budget must be between 1 and {MAX_BRIEF_BUDGET}")
    now = _aware_now(base.now())
    resolver = IdentityResolver.load(base, cancel=cancel)
    owner = next((identity.canonical for identity in resolver.identities() if identity.owner), "")
    status = status_report(base, StatusRequest(skip_git_audit=True, evaluation_time=now), cancel=cancel)
    _check_canceled(cancel)

    sections = [_brief_attention(status)]
    sections.append(_brief_recent_evidence(base, now, 0, "today", "Today", cancel))
    sections.append(_brief_tasks_due(base, now, cancel))
    sections.append(_brief_recent_evidence(base, now, -1, "yesterday", "Yesterday", cancel))
    sections.append(_brief_active_projects(base, now, cancel))
    stale = tuple(sorted(source.name for source in status.sources if source.enabled and source.stale))
    receipt = BriefReceipt(
        base.config.name,
        budget,
        as_of=now.date().isoformat(),
        owner=owner,
        stale_sources=stale,
        unharvested=status.unharvested,
        brief_version=_BRIEF_VERSION,
        tool_version=DISPLAY_VERSION,
    )
    report = BriefReport(sections, receipt)
    receipt.candidates = _brief_item_count(sections)
    receipt.input_digest = _brief_input_digest(report)
    minimum = _minimum_brief_budget(report)
    if budget < minimum:
        raise BriefBudgetError(budget, minimum)
    while True:
        _account_brief(report)
        if _brief_fits(report):
            return report
        if not _trim_brief(report):
            raise BriefBudgetError(budget, minimum)


def _brief_attention(status: Status) -> BriefSection:
    section = BriefSection("attention", "Attention")
    for source in status.sources:
        if not source.enabled or not source.stale:
            continue
        detail = "missing or beyond its configured freshness limit"
        if source.last_collected_at:
            detail = f"{source.lag_hours}h since last collection"
        section.items.append(BriefItem("fkf.yaml", title=f"Refresh stale source {source.name}", detail=detail))
    if status.unharvested > 0:
        section.items.append(
            BriefItem(
                "tasks/",
                title="Review unharvested learnings",
                detail="fkf list tasks learned --unharvested",
                count=status.unharvested,
            )
        )
    if not status.trust.trusted:
        section.items.append(BriefItem("fkf.yaml", title="Review and trust this base", detail="fkf trust"))
    section.total = len(section.items)
    return section


def _brief_recent_evidence(
    base: Base,
    now: datetime,
    offset: int,
    name: str,
    title: str,
    cancel: Cancellation | None,
) -> BriefSection:
    section = BriefSection(name, title)
    if not base.store.enabled(Layer.EVENTS):
        return section
    value = (now.date() + timedelta(days=offset)).isoformat()
    report = _timeline_at(
        base,
        TimelineRequest(
            window=Window(value, value, name),
            budget=MAX_DIGEST_BUDGET,
            delivery_format=DIGEST_DELIVERY_JSON,
        ),
        now,
        cancel,
    )
    for group in report.groups:
        if group.summarized:
            section.items.append(BriefItem(title=group.source, detail="records summarized", count=group.count))
            continue
        section.items.extend(
            BriefItem(item.uri, item.time, item.title, group.source, item.count) for item in group.items
        )
    section.total = len(section.items)
    return section


_CLOSED_STATUSES: Final = frozenset({"done", "closed", "cancelled", "canceled", "complete", "completed", "archived"})


def _frontmatter_string(page: Page, name: str) -> str:
    value = page.frontmatter.get(name)
    return scalar_string(value) or ""


def _brief_date(value: str) -> str:
    candidate = value[:10]
    try:
        parsed = date.fromisoformat(candidate)
    except ValueError:
        return ""
    return candidate if parsed.isoformat() == candidate else ""


def _brief_tasks_due(base: Base, now: datetime, cancel: Cancellation | None) -> BriefSection:
    section = BriefSection("tasks_due", "Authored tasks due")
    if not base.store.enabled(Layer.TASKS):
        return section
    today = now.date().isoformat()
    for trace in list_tasks(base, cancel=cancel).traces:
        _check_canceled(cancel)
        page = cast(Page, trace.page)
        due = _brief_date(_frontmatter_string(page, "due"))
        if not due or due > today or page.status.strip().lower() in _CLOSED_STATUSES:
            continue
        section.items.append(BriefItem(page.uri, title=_brief_page_title(page), detail=f"due {due}"))
    _sort_brief_items(section.items)
    section.total = len(section.items)
    return section


def _brief_active_projects(base: Base, now: datetime, cancel: Cancellation | None) -> BriefSection:
    section = BriefSection("active_projects", "Active projects touched this week")
    if not base.store.enabled(Layer.PROJECTS):
        return section
    pages, _ = load_markdown_layer(base, Layer.PROJECTS, cancel=cancel)
    since = (now.date() - timedelta(days=now.weekday())).isoformat()
    today = now.date().isoformat()
    location = now.tzinfo or UTC
    for page in pages:
        _check_canceled(cancel)
        if page.status and page.status.casefold() != "active":
            continue
        try:
            touched = parse_rfc3339(page.updated).to_datetime().astimezone(location).date().isoformat()
        except ValueError:
            continue
        if since <= touched <= today:
            section.items.append(BriefItem(page.uri, title=_brief_page_title(page), detail=f"touched {touched}"))
    section.items.sort(key=lambda item: item.uri)
    section.items.sort(key=lambda item: item.detail, reverse=True)
    section.total = len(section.items)
    return section


def _brief_page_title(page: Page) -> str:
    return _one_line(page.title) or page.uri


def _sort_brief_items(items: list[BriefItem]) -> None:
    items.sort(key=lambda item: item.uri)
    items.sort(key=lambda item: item.time, reverse=True)


def _brief_item_count(sections: Sequence[BriefSection]) -> int:
    return sum(len(section.items) for section in sections)


@dataclass(frozen=True, slots=True)
class _BriefInput:
    version: int
    base: str
    as_of: str
    owner: str = field(default="", metadata={"json": "owner,omitempty"})
    stale_sources: tuple[str, ...] = field(default=(), metadata={"json": "stale_sources,omitempty"})
    unharvested: int = field(default=0, metadata={"json": "unharvested,omitempty"})
    sections: tuple[BriefSection, ...] = ()


def _brief_input_digest(report: BriefReport) -> str:
    receipt = report.receipt
    payload = _BriefInput(
        _BRIEF_VERSION,
        receipt.base,
        receipt.as_of,
        receipt.owner,
        receipt.stale_sources,
        receipt.unharvested,
        tuple(report.sections),
    )
    return hashlib.sha256(dumps(payload)).hexdigest()


def _account_brief(report: BriefReport) -> None:
    receipt = report.receipt
    receipt.selected = _brief_item_count(report.sections)
    receipt.dropped = receipt.candidates - receipt.selected
    for _ in range(12):
        json_tokens = _bytes_to_tokens(len(encode_brief_json(report)))
        text_tokens = _bytes_to_tokens(len(render_brief_text(report).encode()))
        used = max(json_tokens, text_tokens)
        if (receipt.json_tokens, receipt.text_tokens, receipt.used_tokens) == (json_tokens, text_tokens, used):
            return
        receipt.json_tokens, receipt.text_tokens, receipt.used_tokens = json_tokens, text_tokens, used


def _brief_fits(report: BriefReport) -> bool:
    limit = report.receipt.budget * 4
    return len(encode_brief_json(report)) <= limit and len(render_brief_text(report).encode()) <= limit


def _minimum_brief_budget(report: BriefReport) -> int:
    minimum = copy.deepcopy(report)
    for section in minimum.sections:
        section.items = []
    receipt = minimum.receipt
    receipt.budget = 1
    receipt.selected = 0
    receipt.dropped = receipt.candidates
    for _ in range(12):
        _account_brief(minimum)
        if receipt.used_tokens <= receipt.budget:
            return receipt.budget
        receipt.budget = receipt.used_tokens
    return receipt.used_tokens


def _trim_brief(report: BriefReport) -> bool:
    for section in reversed(report.sections):
        if len(section.items) > 1:
            section.items.pop()
            return True
    for section in reversed(report.sections):
        if section.items:
            section.items.clear()
            return True
    return False


def encode_brief_json(report: BriefReport) -> bytes:
    """Encode the exact indented JSON envelope counted by the brief budget."""

    return dumps(report, indent=True, newline=True)


def _dash(value: str) -> str:
    return value or "-"


def render_brief_text(report: BriefReport) -> str:
    """Render the exact compact brief representation counted by its budget."""

    receipt = report.receipt
    output = [f"brief {receipt.as_of}"]
    for section in report.sections:
        output.append(f"[{section.title}] {len(section.items)}/{section.total}")
        for item in section.items:
            prefix = item.time or "-"
            count = f" x{item.count}" if item.count > 1 else ""
            detail = f" · {item.detail}" if item.detail else ""
            uri = f" · {_qualified_citation(receipt.base, item.uri)}" if item.uri else ""
            output.append(f"{prefix} {item.title}{count}{detail}{uri}")
    output.append(
        f"receipt: selected {receipt.selected}/{receipt.candidates} · dropped {receipt.dropped} · "
        f"budget {receipt.budget} · used {receipt.used_tokens} · base {receipt.base}"
    )
    output.append(
        f"receipt: json {receipt.json_tokens} · text {receipt.text_tokens} · "
        f"owner {_dash(receipt.owner)} · offline=true"
    )
    output.append(
        f"receipt: input_sha256 {receipt.input_digest} · brief v{receipt.brief_version} · fkf {receipt.tool_version}"
    )
    return "\n".join(output) + "\n"


__all__ = [
    "DEFAULT_BRIEF_BUDGET",
    "DEFAULT_DIGEST_BUDGET",
    "DIGEST_DELIVERY_COMPACT_JSON",
    "DIGEST_DELIVERY_JSON",
    "DIGEST_DELIVERY_JSONL",
    "DIGEST_DELIVERY_TEXT",
    "MAX_BRIEF_BUDGET",
    "MAX_DIGEST_BUDGET",
    "BriefBudgetError",
    "BriefItem",
    "BriefReceipt",
    "BriefReport",
    "BriefRequest",
    "BriefSection",
    "DayRequest",
    "DigestBudgetError",
    "DigestGroup",
    "DigestItem",
    "DigestReceipt",
    "TemporalQuery",
    "TimelineReport",
    "TimelineRequest",
    "WhoMatch",
    "WhoNeighbourGroup",
    "WhoReport",
    "Window",
    "brief",
    "day",
    "encode_brief_json",
    "encode_timeline_delivery",
    "parse_temporal_query",
    "parse_window",
    "render_brief_text",
    "render_timeline_text",
    "timeline",
    "who",
]
