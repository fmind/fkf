"""Deterministic lexical candidate cache with authenticated sparse lookups."""

from __future__ import annotations

import base64
import hashlib
import json
import os
import re
import stat
import struct
from collections.abc import Iterable, Mapping, Sequence
from contextlib import suppress
from dataclasses import dataclass, field, replace
from datetime import UTC, datetime
from pathlib import Path
from typing import TYPE_CHECKING, Any, Final, cast

from fkf.bodies import (
    BODIES_DIRECTORY,
    BODY_MANIFEST_FILE,
    load_body_manifest,
    read_cached_body_from_manifest,
)
from fkf.documents import Document, Record, event_document_uri, index_document_uri
from fkf.errors import InvalidUsageError, OperationalError
from fkf.fields import (
    DEFAULT_FIELD_WEIGHT,
    FIELD_CATEGORY,
    FIELD_ID,
    FIELD_TIME,
    FIELD_TITLE,
    FIELD_URL,
    FIELD_VISIBILITY,
    FieldSchema,
    is_well_known_field,
    scalar_string,
)
from fkf.graph import IdentityResolver, ResolvedIdentity, graph_input_uris
from fkf.io import FileTooLargeError, atomic_write, open_regular_file, read_file_limited
from fkf.jsoncodec import JsonNumber, dumps, loads
from fkf.learned import list_learned
from fkf.listings import list_tasks
from fkf.markdown import Page, page_commitments
from fkf.pages import load_markdown_layer
from fkf.process import Cancellation, check_cancel
from fkf.query import Window
from fkf.store import (
    BASE_FILE_MODE,
    LAYERS,
    MAX_SOURCE_DOCUMENT_BYTES,
    Layer,
    clean_relative,
    validate_date,
    validate_within_root,
)
from fkf.text import lower as _lower
from fkf.text import terms as _term_tokens
from fkf.timeutil import parse_record_time
from fkf.uri import URIError, parse_uri, resolve_link

if TYPE_CHECKING:
    from fkf.base import Base


LEXICAL_INDEX_PATH: Final = "index/.fkf-index.tsv"
LEXICAL_INDEX_META_PATH: Final = "index/.fkf-index.meta.json"

LEXICAL_INDEX_FALLBACK_MISSING: Final = "missing"
LEXICAL_INDEX_FALLBACK_STALE: Final = "stale"
LEXICAL_INDEX_FALLBACK_CORRUPT: Final = "corrupt"
LEXICAL_INDEX_FALLBACK_QUERY_TOO_SHORT: Final = "query-too-short"

LEXICAL_INDEX_SCHEMA_VERSION: Final = 4
LEXICAL_INDEX_EXTRACTOR_VERSION: Final = 14
RANKING_VERSION: Final = 10
LEXICAL_INDEX_FORMAT: Final = "postings-varint-v3"
LEXICAL_LOOKUP_SHARD_COUNT: Final = 4096

MAX_LEXICAL_INDEX_BYTES: Final = 512 << 20
MAX_LEXICAL_INDEX_ENTRIES: Final = 1_000_000
MAX_LEXICAL_INDEX_LINE_BYTES: Final = 64 << 20
MIN_LEXICAL_LOOKUP_ROW_BYTES: Final = 74

LEXICAL_ENTRY_ROW: Final = "E"
LEXICAL_SCORE_FIELDS_ROW: Final = "D"
LEXICAL_CONTEXT_TOKEN: Final = "T"  # noqa: S105 - row kind, not a credential
LEXICAL_CONTEXT_TRIGRAM: Final = "G"
LEXICAL_CONTEXT_PHRASE: Final = "P"
LEXICAL_FIND_TRIGRAM: Final = "F"
LEXICAL_BODY_TRIGRAM: Final = "B"
LEXICAL_LOOKUP_ROW: Final = "L"
LEXICAL_CANDIDATE_ROW: Final = "C"

POINTS_TERM: Final = 10
RELATED_IDENTIFIER_PRIORITY: Final = 1
DIRECT_IDENTIFIER_PRIORITY: Final = 2
LEXICAL_IDENTIFIER_BLOOM_BYTES: Final = 32

_SHA256_PATTERN = re.compile(r"[0-9a-f]{64}\Z")
_INTEGER_PATTERN = re.compile(r"(?:0|[1-9][0-9]*)\Z")
_SOURCE_NAME_PATTERN = re.compile(r"[a-z0-9][a-z0-9-]*\Z")
_TERM_SCAFFOLDING = frozenset(
    {
        "about",
        "and",
        "can",
        "could",
        "did",
        "do",
        "does",
        "for",
        "from",
        "give",
        "how",
        "i",
        "is",
        "last",
        "me",
        "my",
        "our",
        "please",
        "prepare",
        "show",
        "summarize",
        "take",
        "tell",
        "the",
        "was",
        "were",
        "what",
        "when",
        "where",
        "which",
        "who",
        "why",
        "with",
        "would",
        "your",
    }
)


class LexicalIndexError(OperationalError):
    """A lexical source, projection, or cache generation is invalid."""


class _LexicalIndexStaleError(LexicalIndexError):
    """A structurally valid sidecar belongs to older extraction semantics."""


class _LexicalIndexCorruptError(LexicalIndexError):
    """Derived cache bytes violate their authenticated representation."""


_LexicalIndexStale = _LexicalIndexStaleError
_LexicalIndexCorrupt = _LexicalIndexCorruptError


_EMPTY_WINDOW = Window()


@dataclass(frozen=True, slots=True)
class LexicalIndexUse:
    path: str = LEXICAL_INDEX_PATH
    used: bool = field(default=False, metadata={"json": "used,omitempty"})
    reason: str = field(default="", metadata={"json": "reason,omitempty"})

    def compact(self) -> str:
        """Render the compact receipt spelling used by the Go contract."""

        if self.used:
            return f"{self.path} (used)"
        if self.reason:
            return f"{self.path} ({self.reason})"
        return self.path

    def __json_value__(self) -> str:
        """Return the compact public receipt used by every retrieval response."""

        return self.compact()

    @classmethod
    def parse(cls, value: str) -> LexicalIndexUse:
        if not value:
            return cls(path="")
        if not value.endswith(")") or " (" not in value:
            raise ValueError(f"decode lexical index use {value!r}: expected '<path> (<state>)'")
        path, state = value[:-1].rsplit(" (", maxsplit=1)
        return cls(path=path, used=state == "used", reason="" if state == "used" else state)


@dataclass(frozen=True, slots=True)
class LexicalInputFile:
    path: str
    bytes: int
    modified_unix_nano: int
    sha256: str


@dataclass(frozen=True, slots=True)
class LexicalLookupShard:
    offset: int
    bytes: int
    rows: int
    sha256: str


@dataclass(frozen=True, slots=True)
class LexicalIndexMeta:
    schema_version: int
    extractor_version: int
    format: str
    generated_at: str
    entries: int
    context_entries: int
    postings: int
    posting_rows: int
    bytes: int
    postings_offset: int
    lookup_offset: int
    candidates_offset: int
    entries_sha256: str
    lookup_shards: tuple[LexicalLookupShard, ...]
    inputs_sha256: str
    semantics_sha256: str
    output_sha256: str
    unharvested_bullets: int
    inputs: tuple[LexicalInputFile, ...]


@dataclass(frozen=True, slots=True)
class LexicalIndexBuild:
    uri: str
    meta_uri: str
    entries: int
    context_entries: int
    postings: int
    bytes: int
    mode: str
    elapsed: str
    meta: LexicalIndexMeta
    stale: bool = field(default=False, metadata={"json": "stale,omitempty"})


@dataclass(frozen=True, order=True, slots=True)
class LexicalPostingKey:
    kind: str
    value: str


@dataclass(frozen=True, slots=True)
class LexicalCandidateSegment:
    field: str
    text: str
    weight: int


@dataclass(frozen=True, slots=True)
class LexicalTermSegment:
    field: str
    weight: int
    normalizer: int


@dataclass(frozen=True, slots=True)
class LexicalTermAnalysis:
    matched: bool = False
    identifier_priority: int = 0
    max_weight: int = 0
    segments: tuple[LexicalTermSegment, ...] = ()
    non_body_match: bool = False


@dataclass(frozen=True, slots=True)
class LexicalTermScore:
    analysis: LexicalTermAnalysis
    excerpt_bytes: int = 0


@dataclass(frozen=True, slots=True)
class LexicalIdentifierBound:
    weight: int = field(metadata={"json": "w"})
    points: int = field(metadata={"json": "p"})
    bloom: bytes = field(metadata={"json": "b"})


@dataclass(slots=True)
class LexicalCandidate:
    """The cache's scorer-neutral projection of one durable record or page."""

    uri: str
    kind: str
    source: str = ""
    date: str = ""
    time: str = ""
    title: str = ""
    url: str = ""
    status: str = ""
    excerpt: str = ""
    tags: tuple[str, ...] | None = None
    fields: dict[str, tuple[str, ...]] | None = None
    body: str = ""
    segments: list[LexicalCandidateSegment] = field(default_factory=list)
    identity_terms: set[str] = field(default_factory=set)
    identifier_keys: set[str] = field(default_factory=set)
    direct_identifiers: set[str] = field(default_factory=set)
    relation_fields: set[str] = field(default_factory=set)
    default_excluded: str = ""
    created_evidence: bool = False
    validity_rank: str = ""
    supersedes: tuple[str, ...] | None = None
    semantic_digest: str = ""
    body_available: bool = False
    identifier_bounds: tuple[LexicalIdentifierBound, ...] = ()
    term_analysis: dict[str, LexicalTermAnalysis] = field(default_factory=dict)
    indexed_phrases: set[str] = field(default_factory=set)
    phrase_analysis_complete: bool = False
    count: int = 0
    collapsed_uris: tuple[str, ...] = ()

    def add_segment(self, name: str, text: str, weight: int) -> None:
        value = text.strip()
        if not value:
            return
        self.term_analysis.clear()
        self.segments.append(LexicalCandidateSegment(name, value, max(1, weight)))

    def add_identifier(self, value: str) -> None:
        key = _normalize_identity_key(value)
        if not key:
            return
        self.term_analysis.clear()
        self.identifier_keys.add(key)
        self.direct_identifiers.add(key)

    def add_related_identifier(self, value: str) -> None:
        key = _normalize_identity_key(value)
        if not key:
            return
        self.term_analysis.clear()
        self.identifier_keys.add(key)

    def add_entity_identifier(self, value: str) -> None:
        self.add_related_identifier(value)
        try:
            parsed = parse_uri(value)
        except URIError, ValueError:
            return
        if not parsed.is_entity():
            return
        self.add_related_identifier(parsed.value)
        if "/" in parsed.value:
            self.add_related_identifier(parsed.value.rsplit("/", maxsplit=1)[1])

    @property
    def haystack(self) -> str:
        return _lower(" ".join(segment.text for segment in self.segments))


@dataclass(slots=True)
class LexicalEntry:
    id: int
    uri: str
    kind: str
    source: str = ""
    date: str = ""
    time: str = ""
    valid_from: str = ""
    valid_until: str = ""
    context: bool = False
    body_cached: bool = False
    count: int = 0
    candidate_offset: int = 0
    candidate_bytes: int = 0
    candidate_sha256: str = ""
    rank: str = ""
    collapsed: tuple[str, ...] = ()
    candidate: LexicalCandidate | None = field(default=None, repr=False, compare=False)
    find_texts: tuple[str, ...] = field(default=(), repr=False, compare=False)
    cached_body: str = field(default="", repr=False, compare=False)

    @property
    def is_record(self) -> bool:
        return self.kind in {Layer.EVENTS, Layer.INDEX}

    def active(self, window: Window, as_of: str) -> bool:
        if self.kind in {Layer.EVENTS, Layer.TASKS} and self.date and not window.contains(self.date):
            return False
        if self.kind in {Layer.WIKI, Layer.PROJECTS}:
            if self.valid_from and as_of < self.valid_from:
                return False
            if self.valid_until and as_of > self.valid_until:
                return False
        return True


@dataclass(frozen=True, slots=True)
class LexicalIndexData:
    entries: tuple[LexicalEntry, ...]
    score_fields: tuple[str, ...]
    postings: Mapping[LexicalPostingKey, frozenset[int]]
    term_scores: Mapping[str, Mapping[int, LexicalTermScore]]
    inputs_sha256: str
    inputs: tuple[LexicalInputFile, ...]
    meta: LexicalIndexMeta


@dataclass(frozen=True, slots=True)
class LexicalContextPreparation:
    """One authenticated entry generation reused by a bounded context batch."""

    entries: tuple[LexicalEntry, ...]
    score_fields: tuple[str, ...]
    meta: LexicalIndexMeta | None
    use: LexicalIndexUse


@dataclass(frozen=True, slots=True)
class LexicalSupersession:
    by: str = ""
    rank: str = ""


@dataclass(frozen=True, slots=True)
class LexicalContextPlan:
    entries: tuple[LexicalEntry, ...]
    omitted: tuple[str, ...]
    consulted_bodies: tuple[str, ...]
    pinnable: tuple[str, ...]
    total: int
    inputs_sha256: str
    inputs: tuple[LexicalInputFile, ...]
    unharvested_bullets: int
    meta: LexicalIndexMeta
    summarized: bool
    hydrate_ids: frozenset[int]
    supersessions: Mapping[str, LexicalSupersession]


@dataclass(frozen=True, slots=True)
class LexicalFindPlan:
    candidates: frozenset[str]
    inputs_sha256: str
    inputs: tuple[LexicalInputFile, ...]


@dataclass(slots=True)
class _LexicalCorpus:
    entries: list[LexicalEntry] = field(default_factory=list)
    context_entries: int = 0


@dataclass(frozen=True, slots=True)
class _LexicalIndexEncoding:
    rows: bytes
    postings: int
    posting_rows: int
    postings_offset: int
    lookup_offset: int
    candidates_offset: int
    entries_sha256: str
    lookup_shards: tuple[LexicalLookupShard, ...]


def identifier_shaped(term: str) -> bool:
    return any(separator in term for separator in "-/:@.")


def normalize_query_terms(query: str) -> tuple[str, ...]:
    """Return the stable, deduplicated terms shared by scan and cache paths."""

    terms: list[str] = []
    seen: set[str] = set()
    for token in _term_tokens(query):
        minimum = 2 if identifier_shaped(token) else 3
        if len(token) < minimum or token in _TERM_SCAFFOLDING or token in seen:
            continue
        seen.add(token)
        terms.append(token)
    return tuple(terms)


def lexical_trigrams(value: str) -> tuple[str, ...]:
    runes = tuple(_lower(value))
    if len(runes) < 3:
        return ()
    return tuple(sorted({"".join(runes[index : index + 3]) for index in range(len(runes) - 2)}))


def lexical_phrase_words(value: str) -> tuple[str, ...]:
    words: list[str] = []
    for word in value.split():
        start = 0
        end = len(word)
        while start < end and not (word[start].isalpha() or word[start].isnumeric()):
            start += 1
        while end > start and not (word[end - 1].isalpha() or word[end - 1].isnumeric()):
            end -= 1
        words.append(word[start:end])
    return tuple(words)


def lexical_phrase_supported(phrase: str) -> bool:
    words = tuple(phrase.split())
    return (
        2 <= len(words) <= 4
        and " ".join(words) == phrase
        and words == lexical_phrase_words(phrase)
        and any(identifier_shaped(word) for word in words)
    )


def lexical_identifier_subterms(value: str) -> tuple[str, ...]:
    lowered = _lower(value)
    if not identifier_shaped(lowered):
        return ()
    starts = [0]
    ends = [len(lowered)]
    for index, character in enumerate(lowered):
        if character not in "/:@#%":
            continue
        if index > 0:
            ends.append(index)
        if index + 1 < len(lowered):
            starts.append(index + 1)
    values = {
        lowered[start:end]
        for start in starts
        for end in ends
        if end > start and end - start >= 2 and "/" in lowered[start:end]
    }
    return tuple(sorted(values))


def lexical_posting_shard(key: LexicalPostingKey) -> int:
    digest = hashlib.sha256(f"{key.kind}\0{key.value}".encode()).digest()
    return digest[0] << 4 | digest[1] >> 4


def _encode_lookup_key(key: LexicalPostingKey) -> str:
    digest = hashlib.sha256(f"{key.kind}\0{key.value}".encode()).digest()
    return base64.urlsafe_b64encode(digest[:16]).decode().rstrip("=")


def _normalize_identity_key(value: str) -> str:
    return _lower(value.strip())


def _truncate_runes(value: str, limit: int) -> str:
    trimmed = value.strip()
    if len(trimmed) <= limit:
        return trimmed
    return trimmed[:limit].strip() + "…"


def _canonical_record_time(value: str) -> str:
    instant = parse_record_time(value)
    parsed = instant.to_datetime().astimezone(UTC)
    return parsed.strftime("%Y-%m-%dT%H:%M:%SZ")


def _walk_scalar_leaves(value: object) -> Iterable[str]:
    if isinstance(value, Mapping):
        for key in sorted(value):
            yield from _walk_scalar_leaves(value[key])
    elif isinstance(value, list):
        for item in value:
            yield from _walk_scalar_leaves(item)
    elif (text := scalar_string(value)) is not None:
        yield text


def _projected_fields(document: Document, record: Record) -> dict[str, tuple[str, ...]] | None:
    projected: dict[str, tuple[str, ...]] = {}
    for name in document.fields.names():
        if is_well_known_field(name):
            continue
        values = document.fields.eval_strings(name, record)
        if values:
            projected[name] = tuple(values)
    return projected or None


def _record_candidate(document: Document, record: Record, schema: FieldSchema) -> LexicalCandidate:
    uri = document.record_uri(record)
    if uri is None:
        raise LexicalIndexError(f"stored record in {document.uri()} has no identity URI")
    raw_time = document.fields.eval_string(FIELD_TIME, record)
    record_time = ""
    if raw_time is not None:
        with suppress(ValueError):
            record_time = _canonical_record_time(raw_time)
    title = document.fields.eval_string(FIELD_TITLE, record) or ""
    url = document.fields.eval_string(FIELD_URL, record) or ""
    projected = _projected_fields(document, record)
    candidate = LexicalCandidate(
        uri=uri,
        kind="record",
        source=document.source,
        date=document.date,
        time=record_time,
        title=title,
        url=url,
        fields=projected,
        excerpt=_truncate_runes(" — ".join(value for value in (title, url) if value), 320),
        relation_fields={name for name in document.schema.names() if document.schema[name].relation},
    )
    fragment = ""
    with suppress(URIError, ValueError):
        fragment = parse_uri(uri).fragment
    candidate.add_segment(FIELD_ID, fragment, schema.weight(FIELD_ID))
    candidate.add_identifier(uri)
    candidate.add_identifier(fragment)
    candidate.add_segment(FIELD_TITLE, title, schema.weight(FIELD_TITLE))
    candidate.add_identifier(title)
    candidate.add_segment(FIELD_URL, url, schema.weight(FIELD_URL))
    for name in sorted(projected or {}):
        values = cast(dict[str, tuple[str, ...]], projected)[name]
        candidate.add_segment(name, " ".join(values), schema.weight(name))
        definition = schema.get(name)
        if definition is not None and definition.relation:
            for value in values:
                candidate.add_entity_identifier(value)
    category = next(iter((projected or {}).get(FIELD_CATEGORY, ())), "")
    visibility = next(iter((projected or {}).get(FIELD_VISIBILITY, ())), "")
    candidate.created_evidence = category.lower() == "created"
    if category.lower() == "received":
        candidate.default_excluded = f"{FIELD_CATEGORY}:received"
    if visibility.lower() == "private":
        candidate.default_excluded = f"{FIELD_VISIBILITY}:private"
    return candidate


def _page_candidate(page: Page, kind: Layer, schema: FieldSchema) -> LexicalCandidate:
    raw_relations = {name: tuple(values) for name, values in page.relations.items()}
    fields = raw_relations if "relations" in page.frontmatter else None
    tags = tuple(page.tags) if "tags" in page.frontmatter else None
    candidate = LexicalCandidate(
        uri=page.uri,
        kind=str(kind),
        title=page.title,
        status=page.status,
        tags=tags,
        date=page.date,
        fields=fields,
        body=page.body,
        validity_rank=page.valid_from or page.date,
        supersedes=tuple(raw_relations.get("supersedes", ())) or None,
    )
    candidate.add_segment(FIELD_ID, page.slug, schema.weight(FIELD_ID))
    candidate.add_identifier(page.uri)
    candidate.add_identifier(page.slug)
    candidate.add_segment(FIELD_TITLE, page.title, schema.weight(FIELD_TITLE))
    candidate.add_identifier(page.title)
    candidate.add_segment("description", page.description, DEFAULT_FIELD_WEIGHT)
    candidate.add_segment("type", page.type, DEFAULT_FIELD_WEIGHT)
    candidate.add_segment("tags", " ".join(page.tags), DEFAULT_FIELD_WEIGHT)
    candidate.add_segment("body", page.body, DEFAULT_FIELD_WEIGHT)
    for name, value in page_commitments(page):
        candidate.add_segment(name, value, DEFAULT_FIELD_WEIGHT)
    for name in sorted(raw_relations):
        values = raw_relations[name]
        candidate.add_segment(name, " ".join(values), schema.weight(name))
        definition = schema.get(name)
        if definition is None or not definition.relation:
            continue
        candidate.relation_fields.add(name)
        for value in values:
            candidate.add_related_identifier(value)
            try:
                # Match the durable projection: resolve to classify the target,
                # while retaining ranking v7's authored identifier bytes.
                parsed = resolve_link(page.uri, value)
            except URIError:
                continue
            if parsed.is_entity():
                candidate.add_related_identifier(parsed.value)
                if "/" in parsed.value:
                    candidate.add_related_identifier(parsed.value.rsplit("/", maxsplit=1)[1])
    return candidate


def _apply_identity(candidate: LexicalCandidate, identity: ResolvedIdentity) -> None:
    direct = candidate.uri in identity.pages
    for value in (identity.canonical, *identity.aliases, *identity.names):
        candidate.identity_terms.add(_normalize_identity_key(value))
        if direct:
            candidate.add_identifier(value)
        else:
            candidate.add_entity_identifier(value)
        candidate.add_segment("identity", value, DEFAULT_FIELD_WEIGHT)


def _canonicalize_candidate(candidate: LexicalCandidate, resolver: IdentityResolver) -> None:
    canonical_values: set[str] = set()
    if candidate.fields is not None:
        for name, values in tuple(candidate.fields.items()):
            if name not in candidate.relation_fields:
                continue
            canonical = tuple(resolver.canonical(value) for value in values)
            candidate.fields[name] = canonical
            canonical_values.update(canonical)
    canonical_values.add(resolver.canonical(candidate.uri))
    for value in sorted(canonical_values):
        identity = resolver.exact(value)
        if identity is not None:
            _apply_identity(candidate, identity)


def _collapse_record_candidates(
    candidates: Sequence[LexicalCandidate], cancel: Cancellation | None
) -> list[LexicalCandidate]:
    by_run: dict[str, LexicalCandidate] = {}
    collapsed: list[LexicalCandidate] = []
    for candidate in candidates:
        check_cancel(cancel)
        candidate.collapsed_uris = (candidate.uri,)
        if not candidate.source or not candidate.title.strip():
            collapsed.append(candidate)
            continue
        key = f"{candidate.source}\0{_lower(candidate.title.strip())}"
        kept = by_run.get(key)
        if kept is None:
            by_run[key] = candidate
            collapsed.append(candidate)
            continue
        members = (*kept.collapsed_uris, candidate.uri)
        kept.collapsed_uris = members
        kept.count = len(members)
        candidate_chronology = candidate.time or candidate.date
        kept_chronology = kept.time or kept.date
        if candidate_chronology > kept_chronology:
            index = collapsed.index(kept)
            candidate.collapsed_uris = members
            candidate.count = len(members)
            by_run[key] = candidate
            collapsed[index] = candidate
    for candidate in collapsed:
        check_cancel(cancel)
        candidate.collapsed_uris = tuple(sorted(candidate.collapsed_uris))
    return sorted(collapsed, key=lambda item: item.uri)


def _append_entry(corpus: _LexicalCorpus, by_uri: dict[str, LexicalEntry], entry: LexicalEntry) -> None:
    if not entry.uri or entry.candidate is None:
        raise LexicalIndexError("lexical entry is missing its URI or candidate")
    if entry.uri in by_uri:
        raise LexicalIndexError(f"lexical index entry URI {entry.uri} is duplicated")
    by_uri[entry.uri] = entry
    corpus.entries.append(entry)


def _source_schema(base: Base, source: str) -> FieldSchema:
    declared = base.config.sources.get(source)
    return declared.schema if declared is not None else base.config.schema


def _record_find_texts(record: Record, resolver: IdentityResolver) -> tuple[str, ...]:
    texts: list[str] = []
    for value in _walk_scalar_leaves(record):
        texts.append(value)
        identity = resolver.exact(value)
        if identity is None:
            continue
        texts.extend((identity.canonical, *identity.aliases, *identity.names))
    return tuple(texts)


def _collect_lexical_corpus(base: Base, cancel: Cancellation | None) -> _LexicalCorpus:
    check_cancel(cancel)
    resolver = IdentityResolver.load(base, cancel=cancel)
    manifest = load_body_manifest(base)
    corpus = _LexicalCorpus()
    by_uri: dict[str, LexicalEntry] = {}
    records: list[LexicalCandidate] = []
    document_uris: list[str] = []
    if base.store.enabled(Layer.EVENTS):
        for day in base.event_dates():
            check_cancel(cancel)
            document_uris.extend(event_document_uri(day, name) for name in base.day_documents(day))
    if base.store.enabled(Layer.INDEX):
        document_uris.extend(index_document_uri(name) for name in base.index_documents())
    for uri in document_uris:
        check_cancel(cancel)
        document = base.read_document(uri)
        for record in document.records:
            check_cancel(cancel)
            candidate = _record_candidate(document, record, _source_schema(base, document.source))
            body, _, found = read_cached_body_from_manifest(base, manifest, candidate.uri)
            if found:
                candidate.body = body
                candidate.add_segment("body", body, DEFAULT_FIELD_WEIGHT)
            entry = LexicalEntry(
                id=0,
                uri=candidate.uri,
                kind=str(document.layer),
                source=document.source,
                date=document.date,
                time=candidate.time,
                body_cached=found,
                candidate=candidate,
                find_texts=_record_find_texts(record, resolver),
                cached_body=body,
            )
            _append_entry(corpus, by_uri, entry)
            records.append(candidate)
    for layer in (Layer.PROJECTS, Layer.WIKI):
        if not base.store.enabled(layer):
            continue
        pages, _ = load_markdown_layer(base, layer, cancel=cancel)
        for page in pages:
            check_cancel(cancel)
            candidate = _page_candidate(page, layer, base.config.schema)
            texts = (
                page.title,
                page.slug,
                page.description,
                " ".join(page.aliases),
                " ".join(page.tags),
                page.body,
                *(value for _, value in page_commitments(page)),
            )
            _append_entry(
                corpus,
                by_uri,
                LexicalEntry(
                    id=0,
                    uri=page.uri,
                    kind=str(layer),
                    date=page.date,
                    valid_from=page.valid_from,
                    valid_until=page.valid_until,
                    candidate=candidate,
                    find_texts=texts,
                ),
            )
    if base.store.enabled(Layer.TASKS):
        for trace in list_tasks(base, cancel=cancel).traces:
            check_cancel(cancel)
            page = cast(Page, trace.page)
            page = Page(
                uri=page.uri,
                slug=page.slug,
                type=page.type,
                title=page.title,
                description=page.description,
                status=page.status,
                date=trace.date,
                valid_from=page.valid_from,
                valid_until=page.valid_until,
                tags=page.tags,
                aliases=page.aliases,
                relations=page.relations,
                frontmatter=page.frontmatter,
                body=page.body,
                headings=page.headings,
                links=page.links,
                updated=page.updated,
                bytes=page.bytes,
            )
            candidate = _page_candidate(page, Layer.TASKS, base.config.schema)
            _append_entry(
                corpus,
                by_uri,
                LexicalEntry(
                    id=0,
                    uri=page.uri,
                    kind=str(Layer.TASKS),
                    date=trace.date,
                    valid_from=page.valid_from,
                    valid_until=page.valid_until,
                    candidate=candidate,
                    find_texts=(
                        page.title,
                        page.slug,
                        page.description,
                        " ".join(page.aliases),
                        " ".join(page.tags),
                        page.body,
                        *(value for _, value in page_commitments(page)),
                    ),
                ),
            )
    for entry in corpus.entries:
        check_cancel(cancel)
        if entry.candidate is not None:
            _canonicalize_candidate(entry.candidate, resolver)
    for candidate in _collapse_record_candidates(records, cancel):
        check_cancel(cancel)
        entry = by_uri.get(candidate.uri)
        if entry is None:
            raise LexicalIndexError(f"collapsed lexical candidate {candidate.uri} has no entry")
        entry.context = True
        entry.count = len(candidate.collapsed_uris) if len(candidate.collapsed_uris) > 1 else 0
        entry.collapsed = candidate.collapsed_uris
        entry.candidate = candidate
    for entry in corpus.entries:
        check_cancel(cancel)
        if entry.kind in {Layer.WIKI, Layer.PROJECTS, Layer.TASKS}:
            entry.context = True
        if entry.context:
            corpus.context_entries += 1
    corpus.entries.sort(key=lambda entry: entry.uri)
    if len(corpus.entries) > MAX_LEXICAL_INDEX_ENTRIES:
        raise LexicalIndexError(
            f"lexical index has {len(corpus.entries)} entries; maximum is {MAX_LEXICAL_INDEX_ENTRIES}"
        )
    for identifier, entry in enumerate(corpus.entries):
        check_cancel(cancel)
        entry.id = identifier
    return corpus


def _json_int(value: object, label: str) -> int:
    if not isinstance(value, JsonNumber) or _INTEGER_PATTERN.fullmatch(value.raw) is None:
        raise LexicalIndexError(f"{label} must be an integer")
    return int(value.raw)


def _json_string(value: object, label: str, *, default: str | None = None) -> str:
    if value is None and default is not None:
        return default
    if not isinstance(value, str):
        raise LexicalIndexError(f"{label} must be a string")
    return value


def _go_json(value: object) -> bytes:
    # encoding/json's Marshal path escapes HTML-significant runes; lexical bytes pin that path.
    return dumps(value).replace(b"&", b"\\u0026").replace(b"<", b"\\u003c").replace(b">", b"\\u003e")


@dataclass(frozen=True, slots=True)
class _SemanticSegment:
    name: str = field(metadata={"json": "Field"})
    text: str = field(metadata={"json": "Text"})
    weight: int = field(metadata={"json": "Weight"})


@dataclass(frozen=True, slots=True)
class _CandidateSemantic:
    uri: str = field(metadata={"json": "URI"})
    kind: str = field(metadata={"json": "Kind"})
    source: str = field(metadata={"json": "Source"})
    date: str = field(metadata={"json": "Date"})
    time: str = field(metadata={"json": "Time"})
    title: str = field(metadata={"json": "Title"})
    url: str = field(metadata={"json": "URL"})
    status: str = field(metadata={"json": "Status"})
    haystack: str = field(metadata={"json": "Haystack"})
    default_excluded: str = field(metadata={"json": "DefaultExcluded"})
    validity_rank: str = field(metadata={"json": "ValidityRank"})
    tags: tuple[str, ...] | None = field(metadata={"json": "Tags"})
    identities: tuple[str, ...] = field(metadata={"json": "Identities"})
    identifiers: tuple[str, ...] = field(metadata={"json": "Identifiers"})
    supersedes: tuple[str, ...] | None = field(metadata={"json": "Supersedes"})
    fields: Mapping[str, tuple[str, ...]] | None = field(metadata={"json": "Fields"})
    segments: tuple[_SemanticSegment, ...] = field(metadata={"json": "Segments"})
    created: bool = field(metadata={"json": "Created"})


def candidate_semantic_digest(candidate: LexicalCandidate) -> str:
    semantic = _CandidateSemantic(
        uri=candidate.uri,
        kind=candidate.kind,
        source=candidate.source,
        date=candidate.date,
        time=candidate.time,
        title=candidate.title,
        url=candidate.url,
        status=candidate.status,
        haystack=candidate.haystack,
        default_excluded=candidate.default_excluded,
        validity_rank=candidate.validity_rank,
        tags=candidate.tags,
        identities=tuple(sorted(candidate.identity_terms)),
        identifiers=tuple(sorted(candidate.identifier_keys)),
        supersedes=candidate.supersedes,
        fields=candidate.fields,
        segments=tuple(_SemanticSegment(item.field, item.text, item.weight) for item in candidate.segments),
        created=candidate.created_evidence,
    )
    return hashlib.sha256(_go_json(semantic)).hexdigest()


def _segment_token_count(text: str) -> int:
    return max(1, len(_term_tokens(text)))


def _segment_points(weight: int, normalizer: int) -> int:
    return max(POINTS_TERM, POINTS_TERM * weight // max(1, normalizer))


def _add_identifier_bloom(bloom: bytearray, gram: str) -> None:
    digest = hashlib.sha256(gram.encode()).digest()
    for index in range(0, 8, 2):
        position = int.from_bytes(digest[index : index + 2], "big")
        bit = position % (len(bloom) * 8)
        bloom[bit // 8] |= 1 << (bit % 8)


def _identifier_bloom_contains(bloom: bytes, gram: str) -> bool:
    digest = hashlib.sha256(gram.encode()).digest()
    for index in range(0, 8, 2):
        position = int.from_bytes(digest[index : index + 2], "big")
        bit = position % (len(bloom) * 8)
        if bloom[bit // 8] & (1 << (bit % 8)) == 0:
            return False
    return True


def lexical_candidate_identifier_bounds(
    candidate: LexicalCandidate, *, cancel: Cancellation | None = None
) -> tuple[LexicalIdentifierBound, ...]:
    if candidate.identifier_bounds:
        return candidate.identifier_bounds
    groups: dict[tuple[int, int], bytearray] = {}
    for segment in candidate.segments:
        check_cancel(cancel)
        grams = lexical_trigrams(segment.text)
        if not grams:
            continue
        normalizer = max(1, _segment_token_count(segment.text).bit_length())
        key = (segment.weight, _segment_points(segment.weight, normalizer))
        bloom = groups.setdefault(key, bytearray(LEXICAL_IDENTIFIER_BLOOM_BYTES))
        for gram in grams:
            check_cancel(cancel)
            _add_identifier_bloom(bloom, gram)
    return tuple(
        LexicalIdentifierBound(weight, points, bytes(groups[(weight, points)])) for weight, points in sorted(groups)
    )


def lexical_term_scores(
    candidate: LexicalCandidate, *, cancel: Cancellation | None = None
) -> dict[str, LexicalTermScore]:
    scores: dict[str, LexicalTermScore] = {}
    for segment in candidate.segments:
        check_cancel(cancel)
        tokens = _term_tokens(segment.text)
        normalizer = max(1, max(1, len(tokens)).bit_length())
        terms = set(tokens)
        for token in tokens:
            terms.update(lexical_identifier_subterms(token))
        for term in terms:
            check_cancel(cancel)
            previous = scores.get(term, LexicalTermScore(LexicalTermAnalysis()))
            selected = LexicalTermSegment(segment.field, segment.weight, normalizer)
            segments = previous.analysis.segments
            if not segments or _term_segment_precedes(selected, segments[0]):
                segments = (selected,)
            analysis = LexicalTermAnalysis(
                matched=True,
                identifier_priority=previous.analysis.identifier_priority,
                max_weight=max(previous.analysis.max_weight, segment.weight),
                segments=segments,
                non_body_match=previous.analysis.non_body_match or segment.field != "body",
            )
            scores[term] = LexicalTermScore(analysis, previous.excerpt_bytes)
    for identifier in candidate.identity_terms:
        check_cancel(cancel)
        previous = scores.get(identifier, LexicalTermScore(LexicalTermAnalysis()))
        scores[identifier] = LexicalTermScore(
            LexicalTermAnalysis(
                matched=True,
                identifier_priority=max(previous.analysis.identifier_priority, RELATED_IDENTIFIER_PRIORITY),
                max_weight=previous.analysis.max_weight,
                segments=previous.analysis.segments,
                non_body_match=previous.analysis.non_body_match,
            ),
            previous.excerpt_bytes,
        )
    for identifier in candidate.identifier_keys:
        check_cancel(cancel)
        previous = scores.get(identifier, LexicalTermScore(LexicalTermAnalysis()))
        priority = (
            DIRECT_IDENTIFIER_PRIORITY if identifier in candidate.direct_identifiers else RELATED_IDENTIFIER_PRIORITY
        )
        scores[identifier] = LexicalTermScore(
            LexicalTermAnalysis(
                matched=True,
                identifier_priority=max(previous.analysis.identifier_priority, priority),
                max_weight=previous.analysis.max_weight,
                segments=previous.analysis.segments,
                non_body_match=previous.analysis.non_body_match,
            ),
            previous.excerpt_bytes,
        )
    return scores


def _term_segment_precedes(left: LexicalTermSegment, right: LexicalTermSegment) -> bool:
    left_points = _segment_points(left.weight, left.normalizer)
    right_points = _segment_points(right.weight, right.normalizer)
    return left_points > right_points or (left_points == right_points and left.field < right.field)


def lexical_term_score_is_complete(candidate: LexicalCandidate, term: str, analysis: LexicalTermAnalysis) -> bool:
    best_points = (
        _segment_points(analysis.segments[0].weight, analysis.segments[0].normalizer) if analysis.segments else 0
    )
    grams = lexical_trigrams(term)
    for bound in candidate.identifier_bounds:
        if bound.weight <= analysis.max_weight and bound.points <= best_points:
            continue
        if not grams or all(_identifier_bloom_contains(bound.bloom, gram) for gram in grams):
            return False
    return True


@dataclass(frozen=True, slots=True)
class _CandidateSegmentPayload:
    name: str = field(metadata={"json": "field"})
    text: str = field(default="", metadata={"json": "text,omitempty"})
    weight: int = 0
    body: bool = field(default=False, metadata={"json": "body,omitempty"})


@dataclass(frozen=True, slots=True)
class _ContextCandidatePayload:
    title: str
    url: str
    status: str
    excerpt: str
    tags: tuple[str, ...] | None
    fields: Mapping[str, tuple[str, ...]] | None
    body: str
    segments: tuple[_CandidateSegmentPayload, ...]
    identifiers: tuple[str, ...]
    direct_identifiers: tuple[str, ...]
    identity_terms: tuple[str, ...]
    default_excluded: str
    created_evidence: bool
    validity_rank: str
    supersedes: tuple[str, ...] | None


@dataclass(frozen=True, slots=True)
class _RankCandidatePayload:
    title: str = field(default="", metadata={"json": "title,omitempty"})
    url: str = field(default="", metadata={"json": "url,omitempty"})
    status: str = field(default="", metadata={"json": "status,omitempty"})
    excerpt: str = field(default="", metadata={"json": "excerpt,omitempty"})
    tags: tuple[str, ...] | None = field(default=None, metadata={"json": "tags,omitempty"})
    fields: Mapping[str, tuple[str, ...]] | None = field(default=None, metadata={"json": "fields,omitempty"})
    default_excluded: str = field(default="", metadata={"json": "default_excluded,omitempty"})
    validity_rank: str = field(default="", metadata={"json": "validity_rank,omitempty"})
    supersedes: tuple[str, ...] | None = field(default=None, metadata={"json": "supersedes,omitempty"})
    created_evidence: bool = field(default=False, metadata={"json": "created_evidence,omitempty"})
    body_available: bool = field(default=False, metadata={"json": "body_available,omitempty"})
    identifier_bounds: tuple[LexicalIdentifierBound, ...] = field(
        default=(), metadata={"json": "identifier_bounds,omitempty"}
    )
    semantic_digest: str = ""


def _encode_context_candidate(candidate: LexicalCandidate, cancel: Cancellation | None = None) -> str:
    if candidate.kind == "record":
        raise LexicalIndexError("lexical authored-page candidate is missing or is a record")
    body_marked = candidate.body == ""
    segments: list[_CandidateSegmentPayload] = []
    for segment in candidate.segments:
        check_cancel(cancel)
        text = segment.text
        marked = False
        if not body_marked and segment.field == "body" and segment.text == candidate.body.strip():
            text = ""
            marked = True
            body_marked = True
        segments.append(_CandidateSegmentPayload(segment.field, text, segment.weight, marked))
    if not body_marked:
        raise LexicalIndexError("lexical authored-page body has no matching scoring segment")
    payload = _ContextCandidatePayload(
        title=candidate.title,
        url=candidate.url,
        status=candidate.status,
        excerpt=candidate.excerpt,
        tags=candidate.tags,
        fields=candidate.fields,
        body=candidate.body,
        segments=tuple(segments),
        identifiers=tuple(sorted(candidate.identifier_keys)),
        direct_identifiers=tuple(sorted(candidate.direct_identifiers)),
        identity_terms=tuple(sorted(candidate.identity_terms)),
        default_excluded=candidate.default_excluded,
        created_evidence=candidate.created_evidence,
        validity_rank=candidate.validity_rank,
        supersedes=candidate.supersedes,
    )
    return _go_json(payload).decode()


def _encode_rank_candidate(candidate: LexicalCandidate, cancel: Cancellation | None = None) -> str:
    check_cancel(cancel)
    digest = candidate.semantic_digest or candidate_semantic_digest(candidate)
    payload = _RankCandidatePayload(
        title=candidate.title,
        url=candidate.url,
        status=candidate.status,
        excerpt=candidate.excerpt,
        tags=candidate.tags,
        fields=candidate.fields,
        default_excluded=candidate.default_excluded,
        validity_rank=candidate.validity_rank,
        supersedes=candidate.supersedes,
        created_evidence=candidate.created_evidence,
        body_available=bool(candidate.body) or candidate.body_available,
        identifier_bounds=lexical_candidate_identifier_bounds(candidate, cancel=cancel),
        semantic_digest=digest,
    )
    return _go_json(payload).decode()


def _require_json_object(encoded: str, label: str) -> dict[str, object]:
    try:
        value = json.loads(encoded)
    except json.JSONDecodeError as error:
        raise _LexicalIndexCorrupt(f"decode {label}: {error}") from error
    if not isinstance(value, dict):
        raise _LexicalIndexCorrupt(f"decode {label}: expected an object")
    return cast(dict[str, object], value)


def _payload_string(value: object, label: str, *, empty: bool = True) -> str:
    if not isinstance(value, str) or (not empty and not value):
        raise _LexicalIndexCorrupt(f"{label} must be a string")
    return value


def _payload_bool(value: object, label: str) -> bool:
    if not isinstance(value, bool):
        raise _LexicalIndexCorrupt(f"{label} must be a boolean")
    return value


def _payload_strings(value: object, label: str, *, nullable: bool) -> tuple[str, ...] | None:
    if value is None and nullable:
        return None
    if not isinstance(value, list) or any(not isinstance(item, str) for item in value):
        raise _LexicalIndexCorrupt(f"{label} must be an array of strings")
    return tuple(cast(list[str], value))


def _payload_fields(value: object, label: str, *, nullable: bool) -> dict[str, tuple[str, ...]] | None:
    if value is None and nullable:
        return None
    if not isinstance(value, dict):
        raise _LexicalIndexCorrupt(f"{label} must be an object")
    result: dict[str, tuple[str, ...]] = {}
    for name, raw_values in value.items():
        if not isinstance(name, str):
            raise _LexicalIndexCorrupt(f"{label} has a non-string field name")
        values = _payload_strings(raw_values, f"{label}.{name}", nullable=False)
        result[name] = cast(tuple[str, ...], values)
    return result


def _strictly_sorted_values(values: Sequence[str]) -> bool:
    return all(value and (index == 0 or values[index - 1] < value) for index, value in enumerate(values))


def _decode_context_candidate(entry: LexicalEntry, encoded: str) -> LexicalCandidate:
    if entry.is_record or not encoded:
        raise _LexicalIndexCorrupt("lexical authored-page entry has no candidate projection")
    value = _require_json_object(encoded, "lexical authored-page candidate")
    expected = {
        "title",
        "url",
        "status",
        "excerpt",
        "tags",
        "fields",
        "body",
        "segments",
        "identifiers",
        "direct_identifiers",
        "identity_terms",
        "default_excluded",
        "created_evidence",
        "validity_rank",
        "supersedes",
    }
    if set(value) != expected:
        raise _LexicalIndexCorrupt("lexical authored-page candidate has unknown or missing fields")
    identifiers = cast(tuple[str, ...], _payload_strings(value["identifiers"], "identifiers", nullable=False))
    direct = cast(tuple[str, ...], _payload_strings(value["direct_identifiers"], "direct_identifiers", nullable=False))
    identities = cast(tuple[str, ...], _payload_strings(value["identity_terms"], "identity_terms", nullable=False))
    if not all(_strictly_sorted_values(items) for items in (identifiers, direct, identities)):
        raise _LexicalIndexCorrupt("lexical authored-page candidate identifiers are not canonical")
    raw_segments = value["segments"]
    if not isinstance(raw_segments, list):
        raise _LexicalIndexCorrupt("lexical authored-page candidate segments must be an array")
    body = _payload_string(value["body"], "body")
    candidate = LexicalCandidate(
        uri=entry.uri,
        kind=entry.kind,
        source=entry.source,
        date=entry.date,
        time=entry.time,
        title=_payload_string(value["title"], "title"),
        url=_payload_string(value["url"], "url"),
        status=_payload_string(value["status"], "status"),
        excerpt=_payload_string(value["excerpt"], "excerpt"),
        tags=_payload_strings(value["tags"], "tags", nullable=True),
        fields=_payload_fields(value["fields"], "fields", nullable=True),
        body=body,
        identifier_keys=set(identifiers),
        direct_identifiers=set(direct),
        identity_terms=set(identities),
        default_excluded=_payload_string(value["default_excluded"], "default_excluded"),
        created_evidence=_payload_bool(value["created_evidence"], "created_evidence"),
        validity_rank=_payload_string(value["validity_rank"], "validity_rank"),
        supersedes=_payload_strings(value["supersedes"], "supersedes", nullable=True),
    )
    body_markers = 0
    for raw in raw_segments:
        if not isinstance(raw, dict) or set(raw) - {"field", "text", "weight", "body"}:
            raise _LexicalIndexCorrupt("lexical authored-page candidate has an invalid scoring segment")
        name = raw.get("field")
        text = raw.get("text", "")
        weight = raw.get("weight")
        marker = raw.get("body", False)
        if (
            not isinstance(name, str)
            or not name
            or not isinstance(text, str)
            or not isinstance(weight, int)
            or isinstance(weight, bool)
            or weight < 1
            or not isinstance(marker, bool)
            or (marker and text)
        ):
            raise _LexicalIndexCorrupt("lexical authored-page candidate has an invalid scoring segment")
        if marker:
            body_markers += 1
            text = body.strip()
        if not text or text.strip() != text:
            raise _LexicalIndexCorrupt("lexical authored-page candidate has an invalid scoring segment")
        candidate.segments.append(LexicalCandidateSegment(name, text, weight))
    if body_markers > 1 or (bool(body) and body_markers != 1) or (not body and body_markers != 0):
        raise _LexicalIndexCorrupt("lexical authored-page candidate has invalid body metadata")
    if _encode_context_candidate(candidate) != encoded:
        raise _LexicalIndexCorrupt("lexical authored-page candidate is not canonically encoded")
    return candidate


def _decode_identifier_bounds(value: object) -> tuple[LexicalIdentifierBound, ...]:
    if value is None:
        return ()
    if not isinstance(value, list):
        raise _LexicalIndexCorrupt("lexical rank candidate identifier_bounds must be an array")
    result: list[LexicalIdentifierBound] = []
    previous = (-1, -1)
    for raw in value:
        if not isinstance(raw, dict) or set(raw) != {"w", "p", "b"}:
            raise _LexicalIndexCorrupt("lexical rank candidate has invalid identifier bounds")
        weight, points, encoded = raw["w"], raw["p"], raw["b"]
        if (
            not isinstance(weight, int)
            or isinstance(weight, bool)
            or not isinstance(points, int)
            or isinstance(points, bool)
            or not isinstance(encoded, str)
        ):
            raise _LexicalIndexCorrupt("lexical rank candidate has invalid identifier bounds")
        try:
            bloom = base64.b64decode(encoded, validate=True)
        except ValueError as error:
            raise _LexicalIndexCorrupt("lexical rank candidate has invalid identifier bounds") from error
        current = (weight, points)
        if (
            current <= previous
            or weight <= 0
            or points < POINTS_TERM
            or len(bloom) != LEXICAL_IDENTIFIER_BLOOM_BYTES
            or not any(bloom)
        ):
            raise _LexicalIndexCorrupt("lexical rank candidate has invalid identifier bounds")
        result.append(LexicalIdentifierBound(weight, points, bloom))
        previous = current
    return tuple(result)


def _decode_rank_candidate(entry: LexicalEntry) -> LexicalCandidate:
    if not entry.rank:
        raise _LexicalIndexCorrupt("lexical entry has no rank candidate")
    value = _require_json_object(entry.rank, "lexical rank candidate")
    allowed = {
        "title",
        "url",
        "status",
        "excerpt",
        "tags",
        "fields",
        "default_excluded",
        "validity_rank",
        "supersedes",
        "created_evidence",
        "body_available",
        "identifier_bounds",
        "semantic_digest",
    }
    if set(value) - allowed or "semantic_digest" not in value:
        raise _LexicalIndexCorrupt("lexical rank candidate has unknown or missing fields")
    candidate = LexicalCandidate(
        uri=entry.uri,
        kind="record" if entry.is_record else entry.kind,
        source=entry.source,
        date=entry.date,
        time=entry.time,
        title=_payload_string(value.get("title", ""), "title"),
        url=_payload_string(value.get("url", ""), "url"),
        status=_payload_string(value.get("status", ""), "status"),
        excerpt=_payload_string(value.get("excerpt", ""), "excerpt"),
        tags=_payload_strings(value.get("tags"), "tags", nullable=True),
        fields=_payload_fields(value.get("fields"), "fields", nullable=True),
        default_excluded=_payload_string(value.get("default_excluded", ""), "default_excluded"),
        validity_rank=_payload_string(value.get("validity_rank", ""), "validity_rank"),
        supersedes=_payload_strings(value.get("supersedes"), "supersedes", nullable=True),
        created_evidence=_payload_bool(value.get("created_evidence", False), "created_evidence"),
        body_available=_payload_bool(value.get("body_available", False), "body_available"),
        identifier_bounds=_decode_identifier_bounds(value.get("identifier_bounds")),
        semantic_digest=_payload_string(value["semantic_digest"], "semantic_digest"),
        count=entry.count,
        collapsed_uris=entry.collapsed,
    )
    if _SHA256_PATTERN.fullmatch(candidate.semantic_digest) is None:
        raise _LexicalIndexCorrupt("lexical rank candidate has an invalid semantic digest")
    if _encode_rank_candidate(candidate) != entry.rank:
        raise _LexicalIndexCorrupt("lexical rank candidate is not canonically encoded")
    return candidate


def _write_row(fields: Sequence[str]) -> bytes:
    for index, value in enumerate(fields):
        if any(separator in value for separator in "\t\r\n"):
            raise LexicalIndexError(f"lexical index field {index} contains a TSV separator")
    row = ("\t".join(fields) + "\n").encode()
    if len(row) > MAX_LEXICAL_INDEX_LINE_BYTES:
        raise LexicalIndexError(f"lexical index row is {len(row)} bytes; maximum is {MAX_LEXICAL_INDEX_LINE_BYTES}")
    return row


def _add_posting(postings: dict[LexicalPostingKey, set[int]], kind: str, value: str, identifier: int) -> None:
    if value:
        postings.setdefault(LexicalPostingKey(kind, value), set()).add(identifier)


def _add_context_text_postings(
    postings: dict[LexicalPostingKey, set[int]],
    identifier: int,
    text: str,
    cancel: Cancellation | None,
) -> None:
    for token in _term_tokens(text):
        check_cancel(cancel)
        _add_posting(postings, LEXICAL_CONTEXT_TOKEN, token, identifier)
    for gram in lexical_trigrams(text):
        check_cancel(cancel)
        _add_posting(postings, LEXICAL_CONTEXT_TRIGRAM, gram, identifier)


def _add_context_postings(
    postings: dict[LexicalPostingKey, set[int]],
    identifier: int,
    candidate: LexicalCandidate,
    cancel: Cancellation | None,
) -> None:
    for segment in candidate.segments:
        check_cancel(cancel)
        _add_context_text_postings(postings, identifier, segment.text, cancel)
    for value in candidate.identifier_keys:
        check_cancel(cancel)
        _add_context_text_postings(postings, identifier, value, cancel)


def _add_context_phrase_postings(
    postings: dict[LexicalPostingKey, set[int]],
    identifier: int,
    candidate: LexicalCandidate,
    cancel: Cancellation | None,
) -> None:
    for segment in candidate.segments:
        check_cancel(cancel)
        lower = _lower(segment.text)
        words = lexical_phrase_words(lower)
        for start in range(len(words)):
            check_cancel(cancel)
            for count in range(2, 5):
                if start + count > len(words):
                    break
                phrase_words = words[start : start + count]
                if not any(identifier_shaped(word) for word in phrase_words):
                    continue
                phrase = " ".join(phrase_words)
                if phrase in lower:
                    _add_posting(postings, LEXICAL_CONTEXT_PHRASE, phrase, identifier)


def _add_find_postings(
    postings: dict[LexicalPostingKey, set[int]],
    kind: str,
    identifier: int,
    texts: Iterable[str],
    cancel: Cancellation | None,
) -> None:
    for text in texts:
        check_cancel(cancel)
        for gram in lexical_trigrams(text):
            check_cancel(cancel)
            _add_posting(postings, kind, gram, identifier)


def _build_postings(
    corpus: _LexicalCorpus,
    cancel: Cancellation | None,
) -> tuple[dict[LexicalPostingKey, set[int]], dict[str, dict[int, LexicalTermScore]]]:
    postings: dict[LexicalPostingKey, set[int]] = {}
    scores: dict[str, dict[int, LexicalTermScore]] = {}
    for entry in corpus.entries:
        check_cancel(cancel)
        candidate = entry.candidate
        if candidate is None:
            raise LexicalIndexError(f"lexical entry {entry.uri} has no candidate")
        if entry.is_record:
            _add_find_postings(postings, LEXICAL_FIND_TRIGRAM, entry.id, entry.find_texts, cancel)
            if entry.cached_body:
                _add_find_postings(postings, LEXICAL_BODY_TRIGRAM, entry.id, (entry.cached_body,), cancel)
        _add_context_postings(postings, entry.id, candidate, cancel)
        _add_context_phrase_postings(postings, entry.id, candidate, cancel)
        for term, score in lexical_term_scores(candidate, cancel=cancel).items():
            check_cancel(cancel)
            scores.setdefault(term, {})[entry.id] = score
            _add_posting(postings, LEXICAL_CONTEXT_TOKEN, term, entry.id)
    return postings, scores


def _score_fields(scores: Mapping[str, Mapping[int, LexicalTermScore]], cancel: Cancellation | None) -> tuple[str, ...]:
    fields: set[str] = set()
    for entries in scores.values():
        check_cancel(cancel)
        for score in entries.values():
            check_cancel(cancel)
            if score.analysis.segments:
                fields.add(score.analysis.segments[0].field)
    return tuple(sorted(fields))


def _encode_uvarint(value: int) -> bytes:
    if value < 0 or value >= 1 << 64:
        raise LexicalIndexError("lexical varint is outside uint64")
    encoded = bytearray()
    while value >= 0x80:
        encoded.append((value & 0x7F) | 0x80)
        value >>= 7
    encoded.append(value)
    return bytes(encoded)


def _encode_posting_payload(
    key: LexicalPostingKey,
    identifiers: Sequence[int],
    scores: Mapping[int, LexicalTermScore],
    field_ids: Mapping[str, int],
    cancel: Cancellation | None,
) -> bytes:
    payload = bytearray()
    previous = -1
    for identifier in identifiers:
        check_cancel(cancel)
        if identifier < 0 or identifier <= previous:
            raise LexicalIndexError("lexical posting entry IDs are not strictly increasing")
        delta = identifier + 1 if previous < 0 else identifier - previous
        payload.extend(_encode_uvarint(delta))
        previous = identifier
        if key.kind != LEXICAL_CONTEXT_TOKEN:
            continue
        score = scores.get(identifier)
        if score is None or not score.analysis.matched:
            raise LexicalIndexError("lexical context posting has no matched score")
        segment = score.analysis.segments[0] if score.analysis.segments else None
        field_id = field_ids.get(segment.field, 0) if segment is not None else 0
        if segment is not None and not field_id:
            raise LexicalIndexError("lexical term score field is absent from the dictionary")
        for value in (
            score.analysis.identifier_priority,
            score.analysis.max_weight,
            segment.weight if segment is not None else 0,
            segment.normalizer if segment is not None else 0,
            field_id,
            score.excerpt_bytes,
            int(score.analysis.non_body_match),
        ):
            payload.extend(_encode_uvarint(value))
    return bytes(payload)


def _base36(value: int) -> str:
    if value < 0:
        raise LexicalIndexError("lexical base36 value is negative")
    alphabet = "0123456789abcdefghijklmnopqrstuvwxyz"
    if value == 0:
        return "0"
    digits: list[str] = []
    while value:
        value, remainder = divmod(value, 36)
        digits.append(alphabet[remainder])
    return "".join(reversed(digits))


def _raw_url(data: bytes) -> str:
    return base64.urlsafe_b64encode(data).decode().rstrip("=")


def _build_candidate_rows(corpus: _LexicalCorpus, cancel: Cancellation | None) -> bytes:
    rows = bytearray()
    for entry in corpus.entries:
        check_cancel(cancel)
        if entry.is_record:
            continue
        candidate = entry.candidate
        if candidate is None:
            raise LexicalIndexError(f"lexical entry {entry.uri} has no candidate")
        encoded = _encode_context_candidate(candidate, cancel)
        entry.candidate_offset = len(rows)
        row = _write_row((LEXICAL_CANDIDATE_ROW, str(entry.id), encoded))
        rows.extend(row)
        entry.candidate_bytes = len(row)
        entry.candidate_sha256 = hashlib.sha256(row).hexdigest()
    return bytes(rows)


def _encode_posting_sections(
    prefix: bytes,
    postings: Mapping[LexicalPostingKey, set[int]],
    scores: Mapping[str, Mapping[int, LexicalTermScore]],
    score_fields: Sequence[str],
    candidates: bytes,
    cancel: Cancellation | None,
) -> _LexicalIndexEncoding:
    rows = bytearray(prefix)
    postings_offset = len(rows)
    entries_sha256 = hashlib.sha256(rows).hexdigest()
    lookup: list[list[tuple[str, tuple[str, ...]]]] = [[] for _ in range(LEXICAL_LOOKUP_SHARD_COUNT)]
    field_ids = {name: index + 1 for index, name in enumerate(score_fields)}
    pairs = 0
    for key in sorted(postings):
        check_cancel(cancel)
        identifiers = sorted(postings[key])
        payload = _encode_posting_payload(key, identifiers, scores.get(key.value, {}), field_ids, cancel)
        row = _write_row((key.kind, _raw_url(key.value.encode()), _raw_url(payload)))
        start = len(rows)
        rows.extend(row)
        key_hash = _encode_lookup_key(key)
        descriptor = (
            LEXICAL_LOOKUP_ROW,
            key_hash,
            _base36(start),
            _base36(len(row)),
            _base36(len(identifiers)),
            _raw_url(hashlib.sha256(row).digest()),
        )
        lookup[lexical_posting_shard(key)].append((key_hash, descriptor))
        pairs += len(identifiers)
    lookup_offset = len(rows)
    shards: list[LexicalLookupShard] = []
    for shard_rows in lookup:
        check_cancel(cancel)
        encoded = bytearray()
        previous = ""
        for key_hash, descriptor in sorted(shard_rows):
            check_cancel(cancel)
            if key_hash == previous:
                raise LexicalIndexError("lexical lookup key hash collision")
            encoded.extend(_write_row(descriptor))
            previous = key_hash
        shards.append(
            LexicalLookupShard(
                offset=len(rows),
                bytes=len(encoded),
                rows=len(shard_rows),
                sha256=hashlib.sha256(encoded).hexdigest(),
            )
        )
        rows.extend(encoded)
    candidates_offset = len(rows)
    rows.extend(candidates)
    if len(rows) > MAX_LEXICAL_INDEX_BYTES:
        raise LexicalIndexError(f"lexical index is {len(rows)} bytes; maximum is {MAX_LEXICAL_INDEX_BYTES}")
    return _LexicalIndexEncoding(
        rows=bytes(rows),
        postings=pairs,
        posting_rows=len(postings),
        postings_offset=postings_offset,
        lookup_offset=lookup_offset,
        candidates_offset=candidates_offset,
        entries_sha256=entries_sha256,
        lookup_shards=tuple(shards),
    )


def _encode_corpus(corpus: _LexicalCorpus, cancel: Cancellation | None) -> _LexicalIndexEncoding:
    postings, scores = _build_postings(corpus, cancel)
    score_fields = _score_fields(scores, cancel)
    candidate_rows = _build_candidate_rows(corpus, cancel)
    rows = bytearray(_write_row((LEXICAL_SCORE_FIELDS_ROW, *score_fields)))
    for entry in corpus.entries:
        check_cancel(cancel)
        if entry.candidate is None:
            raise LexicalIndexError(f"lexical entry {entry.uri} has no candidate")
        entry.rank = _encode_rank_candidate(entry.candidate, cancel)
        candidate_offset = str(entry.candidate_offset) if entry.candidate_bytes else ""
        candidate_bytes = str(entry.candidate_bytes) if entry.candidate_bytes else ""
        fields = (
            LEXICAL_ENTRY_ROW,
            str(entry.id),
            entry.uri,
            entry.kind,
            entry.source,
            entry.date,
            entry.time,
            entry.valid_from,
            entry.valid_until,
            str(entry.context).lower(),
            str(entry.body_cached).lower(),
            str(entry.count),
            candidate_offset,
            candidate_bytes,
            entry.candidate_sha256,
            entry.rank,
            *entry.collapsed,
        )
        rows.extend(_write_row(fields))
    return _encode_posting_sections(bytes(rows), postings, scores, score_fields, candidate_rows, cancel)


@dataclass(frozen=True, slots=True)
class _LexicalSemanticField:
    name: str
    weight: int
    relation: bool


@dataclass(frozen=True, slots=True)
class _LexicalSemanticSource:
    name: str
    half_life_days: int
    fields: tuple[_LexicalSemanticField, ...] | None


@dataclass(frozen=True, slots=True)
class _LexicalSemanticIdentity:
    canonical: str
    aliases: tuple[str, ...]
    kind: object = field(default=None, metadata={"json": "kind,omitempty"})
    owner: bool = field(default=False, metadata={"json": "owner,omitempty"})


@dataclass(frozen=True, slots=True)
class _LexicalSemantics:
    ranking_version: int
    layers: tuple[str, ...] | None
    fields: tuple[_LexicalSemanticField, ...] | None
    sources: tuple[_LexicalSemanticSource, ...] | None
    identities: Mapping[str, _LexicalSemanticIdentity]


def lexical_semantics_sha256(base: Base, *, cancel: Cancellation | None = None) -> str:
    """Hash every configured value that changes lexical ranking semantics."""

    check_cancel(cancel)

    def semantic_fields(schema: FieldSchema) -> tuple[_LexicalSemanticField, ...]:
        return tuple(_LexicalSemanticField(name, schema.weight(name), schema[name].relation) for name in schema.names())

    sources: list[_LexicalSemanticSource] = []
    for name in base.config.source_names():
        check_cancel(cancel)
        sources.append(
            _LexicalSemanticSource(
                name,
                base.config.sources[name].recency.half_life_days,
                semantic_fields(base.config.sources[name].schema) or None,
            )
        )
    identities = {
        name: _LexicalSemanticIdentity(identity.canonical, identity.aliases, identity.kind, identity.owner)
        for name, identity in base.config.identities.items()
    }
    semantic = _LexicalSemantics(
        RANKING_VERSION,
        tuple(str(layer) for layer in LAYERS if base.store.enabled(layer)) or None,
        semantic_fields(base.config.schema) or None,
        tuple(sources) or None,
        identities,
    )
    return hashlib.sha256(_go_json(semantic)).hexdigest()


def _write_digest_value(digest: Any, value: str) -> None:
    encoded = value.encode()
    digest.update(struct.pack(">Q", len(encoded)))
    digest.update(encoded)


def _lexical_body_input_paths(base: Base, cancel: Cancellation | None) -> tuple[str, ...]:
    check_cancel(cancel)
    path = base.root / BODIES_DIRECTORY / BODY_MANIFEST_FILE
    try:
        os.lstat(path)
    except FileNotFoundError:
        return ()
    except OSError as error:
        raise LexicalIndexError(f"inspect body manifest: {error}") from error
    manifest = load_body_manifest(base)
    check_cancel(cancel)
    return (f"{BODIES_DIRECTORY}/{BODY_MANIFEST_FILE}", *(entry.path for entry in manifest.entries.values()))


def _lexical_input_paths(base: Base, cancel: Cancellation | None) -> tuple[str, ...]:
    check_cancel(cancel)
    graph_inputs = graph_input_uris(base, cancel=cancel)
    check_cancel(cancel)
    return tuple(sorted({*graph_inputs, *_lexical_body_input_paths(base, cancel)}))


def _lexical_input_absolute(base: Base, relative: str) -> Path:
    if relative.startswith(f"{BODIES_DIRECTORY}/"):
        absolute = base.root.joinpath(*relative.split("/"))
        validate_within_root(base.root, absolute)
        return absolute
    try:
        return base.store.resolve(relative)
    except OperationalError as error:
        raise LexicalIndexError(f"lexical input {relative} is not a regular file or is a symlink: {error}") from error


def _hash_lexical_file(path: Path, cancel: Cancellation | None = None) -> tuple[int, str]:
    digest = hashlib.sha256()
    size = 0
    try:
        handle = open_regular_file(path)
    except OSError as error:
        raise LexicalIndexError(f"open lexical input {path.name}: {error}") from error
    with handle:
        while chunk := handle.read(1 << 20):
            check_cancel(cancel)
            size += len(chunk)
            if size > MAX_LEXICAL_INDEX_BYTES:
                raise FileTooLargeError(f"{path} exceeds {MAX_LEXICAL_INDEX_BYTES} bytes")
            digest.update(chunk)
    return size, digest.hexdigest()


def _inspect_lexical_input(
    base: Base,
    relative: str,
    known: Mapping[str, LexicalInputFile],
    cancel: Cancellation | None,
) -> LexicalInputFile | None:
    check_cancel(cancel)
    absolute = _lexical_input_absolute(base, relative)
    cached = relative.startswith(f"{BODIES_DIRECTORY}/")
    try:
        link_info = os.lstat(absolute)
    except FileNotFoundError:
        if cached:
            return None
        raise LexicalIndexError(f"inspect lexical input {relative}: file does not exist") from None
    except OSError as error:
        raise LexicalIndexError(f"inspect lexical input {relative}: {error}") from error
    if stat.S_ISLNK(link_info.st_mode) or not stat.S_ISREG(link_info.st_mode):
        raise LexicalIndexError(f"lexical input {relative} is not a regular file or is a symlink")
    # Re-stat after the no-symlink audit. Besides binding the fingerprint to the opened target,
    # this gives tests and embedders one explicit generation-drift seam.
    try:
        info = os.stat(absolute)  # noqa: PTH116 - explicit monkeypatchable generation seam
    except FileNotFoundError:
        if cached:
            return None
        raise LexicalIndexError(f"inspect lexical input {relative}: file does not exist") from None
    previous = known.get(relative)
    modified = info.st_mtime_ns
    if (
        previous is not None
        and previous.bytes == info.st_size
        and previous.modified_unix_nano == modified
        and _SHA256_PATTERN.fullmatch(previous.sha256) is not None
    ):
        digest = previous.sha256
    else:
        try:
            hashed_size, digest = _hash_lexical_file(absolute, cancel)
        except FileNotFoundError:
            if cached:
                return None
            raise
        if hashed_size != info.st_size:
            raise LexicalIndexError(f"lexical input {relative} changed while it was being hashed")
    return LexicalInputFile(relative, info.st_size, modified, digest)


def lexical_inputs(
    base: Base,
    prior: Sequence[LexicalInputFile] = (),
    *,
    cancel: Cancellation | None = None,
) -> tuple[tuple[LexicalInputFile, ...], str, str]:
    """Resolve the exact searchable input manifest with stat-first digest reuse."""

    known = {item.path: item for item in prior}
    inputs = tuple(
        item
        for relative in _lexical_input_paths(base, cancel)
        if (item := _inspect_lexical_input(base, relative, known, cancel)) is not None
    )
    semantics = lexical_semantics_sha256(base, cancel=cancel)
    aggregate = hashlib.sha256(b"fkf-lexical-inputs-v1\0")
    _write_digest_value(aggregate, "extractor_version")
    _write_digest_value(aggregate, str(LEXICAL_INDEX_EXTRACTOR_VERSION))
    _write_digest_value(aggregate, "semantics")
    _write_digest_value(aggregate, semantics)
    for item in inputs:
        check_cancel(cancel)
        _write_digest_value(aggregate, item.path)
        _write_digest_value(aggregate, item.sha256)
    return inputs, semantics, aggregate.hexdigest()


def lexical_inputs_match(
    base: Base,
    prior: Sequence[LexicalInputFile],
    expected: str,
    *,
    cancel: Cancellation | None = None,
) -> bool:
    """Revalidate the complete evidence generation after an indexed candidate scan."""

    return lexical_inputs(base, prior, cancel=cancel)[2] == expected


def _unharvested_bullets(base: Base, cancel: Cancellation | None) -> int:
    check_cancel(cancel)
    if not base.store.enabled(Layer.TASKS):
        return 0
    return list_learned(base, cancel=cancel).unharvested


def _canonical_utc_seconds(value: datetime) -> str:
    if value.tzinfo is None:
        value = value.replace(tzinfo=UTC)
    return value.astimezone(UTC).replace(microsecond=0).strftime("%Y-%m-%dT%H:%M:%SZ")


def _elapsed_string(started: datetime, ended: datetime) -> str:
    milliseconds = max(0, round((ended - started).total_seconds() * 1000))
    if milliseconds == 0:
        return "0s"
    if milliseconds % 1000 == 0:
        return f"{milliseconds // 1000}s"
    return f"{milliseconds / 1000:g}s"


def _lexical_index_paths(base: Base) -> tuple[Path, Path]:
    rows = base.root.joinpath(*LEXICAL_INDEX_PATH.split("/"))
    meta = base.root.joinpath(*LEXICAL_INDEX_META_PATH.split("/"))
    validate_within_root(base.root, rows)
    validate_within_root(base.root, meta)
    return rows, meta


def _write_lexical_index(base: Base, encoded: bytes, meta: LexicalIndexMeta, cancel: Cancellation | None) -> None:
    rows, sidecar = _lexical_index_paths(base)
    # The two atomic replacements publish one generation. Refuse cancellation before the
    # first write, then finish both so readers never observe a canceled half-publication.
    check_cancel(cancel)
    atomic_write(rows, encoded, mode=BASE_FILE_MODE)
    try:
        info = rows.stat()
    except OSError as error:
        raise LexicalIndexError(f"inspect written {LEXICAL_INDEX_PATH}: {error}") from error
    if info.st_size != len(encoded):
        raise LexicalIndexError(f"written {LEXICAL_INDEX_PATH} is {info.st_size} bytes; want {len(encoded)}")
    atomic_write(sidecar, dumps(meta, indent=True, newline=True), mode=BASE_FILE_MODE)


def build_lexical_index(base: Base, *, cancel: Cancellation | None = None) -> LexicalIndexBuild:
    """Rebuild and atomically publish one complete deterministic cache generation."""

    started = base.now()
    _, semantics, aggregate = lexical_inputs(base, cancel=cancel)
    corpus = _collect_lexical_corpus(base, cancel)
    unharvested = _unharvested_bullets(base, cancel)
    encoded = _encode_corpus(corpus, cancel)
    confirmed, confirmed_semantics, confirmed_aggregate = lexical_inputs(base, cancel=cancel)
    if aggregate != confirmed_aggregate or semantics != confirmed_semantics:
        raise LexicalIndexError("lexical index inputs changed while the cache was being built; retry")
    rows = encoded.rows
    meta = LexicalIndexMeta(
        schema_version=LEXICAL_INDEX_SCHEMA_VERSION,
        extractor_version=LEXICAL_INDEX_EXTRACTOR_VERSION,
        format=LEXICAL_INDEX_FORMAT,
        generated_at=_canonical_utc_seconds(started),
        entries=len(corpus.entries),
        context_entries=corpus.context_entries,
        postings=encoded.postings,
        posting_rows=encoded.posting_rows,
        bytes=len(rows),
        postings_offset=encoded.postings_offset,
        lookup_offset=encoded.lookup_offset,
        candidates_offset=encoded.candidates_offset,
        entries_sha256=encoded.entries_sha256,
        lookup_shards=encoded.lookup_shards,
        inputs_sha256=aggregate,
        semantics_sha256=semantics,
        output_sha256=hashlib.sha256(rows).hexdigest(),
        unharvested_bullets=unharvested,
        inputs=confirmed,
    )
    _write_lexical_index(base, rows, meta, cancel)
    return LexicalIndexBuild(
        uri=LEXICAL_INDEX_PATH,
        meta_uri=LEXICAL_INDEX_META_PATH,
        entries=len(corpus.entries),
        context_entries=corpus.context_entries,
        postings=encoded.postings,
        bytes=len(rows),
        mode="full",
        elapsed=_elapsed_string(started, base.now()),
        meta=meta,
    )


def _json_array(value: object, label: str) -> list[object]:
    if not isinstance(value, list):
        raise _LexicalIndexCorrupt(f"{label} must be an array")
    return cast(list[object], value)


def _strict_meta_object(value: object, expected: set[str], label: str) -> Mapping[str, object]:
    if not isinstance(value, Mapping) or set(value) != expected:
        raise _LexicalIndexCorrupt(f"{label} has unknown or missing fields")
    return cast(Mapping[str, object], value)


def _decode_lookup_shard(value: object, index: int) -> LexicalLookupShard:
    raw = _strict_meta_object(value, {"offset", "bytes", "rows", "sha256"}, f"lookup shard {index}")
    return LexicalLookupShard(
        offset=_json_int(raw["offset"], f"lookup shard {index} offset"),
        bytes=_json_int(raw["bytes"], f"lookup shard {index} bytes"),
        rows=_json_int(raw["rows"], f"lookup shard {index} rows"),
        sha256=_json_string(raw["sha256"], f"lookup shard {index} sha256"),
    )


def _decode_input_file(value: object, index: int) -> LexicalInputFile:
    raw = _strict_meta_object(value, {"path", "bytes", "modified_unix_nano", "sha256"}, f"input manifest item {index}")
    return LexicalInputFile(
        path=_json_string(raw["path"], f"input manifest item {index} path"),
        bytes=_json_int(raw["bytes"], f"input manifest item {index} bytes"),
        modified_unix_nano=_json_int(raw["modified_unix_nano"], f"input manifest item {index} modified_unix_nano"),
        sha256=_json_string(raw["sha256"], f"input manifest item {index} sha256"),
    )


def _decode_lexical_index_meta(data: bytes, cancel: Cancellation | None = None) -> LexicalIndexMeta:
    check_cancel(cancel)
    try:
        decoded = loads(data)
    except ValueError as error:
        raise _LexicalIndexCorrupt(f"decode {LEXICAL_INDEX_META_PATH}: {error}") from error
    names = {
        "schema_version",
        "extractor_version",
        "format",
        "generated_at",
        "entries",
        "context_entries",
        "postings",
        "posting_rows",
        "bytes",
        "postings_offset",
        "lookup_offset",
        "candidates_offset",
        "entries_sha256",
        "lookup_shards",
        "inputs_sha256",
        "semantics_sha256",
        "output_sha256",
        "unharvested_bullets",
        "inputs",
    }
    raw = _strict_meta_object(decoded, names, "lexical index metadata")
    lookup_shards: list[LexicalLookupShard] = []
    for index, value in enumerate(_json_array(raw["lookup_shards"], "lookup_shards")):
        check_cancel(cancel)
        lookup_shards.append(_decode_lookup_shard(value, index))
    inputs: list[LexicalInputFile] = []
    for index, value in enumerate(_json_array(raw["inputs"], "inputs")):
        check_cancel(cancel)
        inputs.append(_decode_input_file(value, index))
    meta = LexicalIndexMeta(
        schema_version=_json_int(raw["schema_version"], "schema_version"),
        extractor_version=_json_int(raw["extractor_version"], "extractor_version"),
        format=_json_string(raw["format"], "format"),
        generated_at=_json_string(raw["generated_at"], "generated_at"),
        entries=_json_int(raw["entries"], "entries"),
        context_entries=_json_int(raw["context_entries"], "context_entries"),
        postings=_json_int(raw["postings"], "postings"),
        posting_rows=_json_int(raw["posting_rows"], "posting_rows"),
        bytes=_json_int(raw["bytes"], "bytes"),
        postings_offset=_json_int(raw["postings_offset"], "postings_offset"),
        lookup_offset=_json_int(raw["lookup_offset"], "lookup_offset"),
        candidates_offset=_json_int(raw["candidates_offset"], "candidates_offset"),
        entries_sha256=_json_string(raw["entries_sha256"], "entries_sha256"),
        lookup_shards=tuple(lookup_shards),
        inputs_sha256=_json_string(raw["inputs_sha256"], "inputs_sha256"),
        semantics_sha256=_json_string(raw["semantics_sha256"], "semantics_sha256"),
        output_sha256=_json_string(raw["output_sha256"], "output_sha256"),
        unharvested_bullets=_json_int(raw["unharvested_bullets"], "unharvested_bullets"),
        inputs=tuple(inputs),
    )
    _validate_lexical_index_meta(meta, cancel)
    return meta


def _validate_lexical_index_meta(meta: LexicalIndexMeta, cancel: Cancellation | None = None) -> None:
    check_cancel(cancel)
    if meta.schema_version != LEXICAL_INDEX_SCHEMA_VERSION:
        raise _LexicalIndexCorrupt(f"lexical index is corrupt: schema_version {meta.schema_version}")
    if meta.extractor_version != LEXICAL_INDEX_EXTRACTOR_VERSION:
        raise _LexicalIndexStale(f"lexical index is stale: extractor_version {meta.extractor_version}")
    if meta.format != LEXICAL_INDEX_FORMAT:
        raise _LexicalIndexCorrupt(f"lexical index is corrupt: format {meta.format!r}")
    if (
        meta.entries < 0
        or meta.entries > MAX_LEXICAL_INDEX_ENTRIES
        or meta.context_entries < 0
        or meta.context_entries > meta.entries
        or meta.postings < 0
        or meta.posting_rows < 0
        or meta.posting_rows > meta.postings
        or meta.unharvested_bullets < 0
    ):
        raise _LexicalIndexCorrupt("lexical index is corrupt: invalid counts")
    if (
        meta.bytes < 0
        or meta.bytes > MAX_LEXICAL_INDEX_BYTES
        or meta.postings_offset < 0
        or meta.postings_offset > meta.lookup_offset
        or meta.lookup_offset > meta.candidates_offset
        or meta.candidates_offset > meta.bytes
        or meta.postings_offset < len(LEXICAL_SCORE_FIELDS_ROW) + 1
    ):
        raise _LexicalIndexCorrupt("lexical index is corrupt: invalid byte count")
    if any(
        _SHA256_PATTERN.fullmatch(value) is None
        for value in (
            meta.entries_sha256,
            meta.inputs_sha256,
            meta.semantics_sha256,
            meta.output_sha256,
        )
    ):
        raise _LexicalIndexCorrupt("lexical index is corrupt: invalid digest")
    if len(meta.lookup_shards) != LEXICAL_LOOKUP_SHARD_COUNT:
        raise _LexicalIndexCorrupt("lexical index is corrupt: invalid lookup shard count")
    offset = meta.lookup_offset
    rows = 0
    for shard in meta.lookup_shards:
        check_cancel(cancel)
        if (
            shard.offset != offset
            or shard.bytes < 0
            or shard.rows < 0
            or shard.bytes > MAX_LEXICAL_INDEX_BYTES
            or shard.rows > shard.bytes // MIN_LEXICAL_LOOKUP_ROW_BYTES
            or _SHA256_PATTERN.fullmatch(shard.sha256) is None
            or shard.bytes > meta.candidates_offset - offset
        ):
            raise _LexicalIndexCorrupt("lexical index is corrupt: invalid lookup shard metadata")
        offset += shard.bytes
        rows += shard.rows
    if offset != meta.candidates_offset or rows != meta.posting_rows:
        raise _LexicalIndexCorrupt("lexical index is corrupt: lookup shards do not match metadata")
    try:
        parsed = datetime.fromisoformat(meta.generated_at.removesuffix("Z") + "+00:00")
    except ValueError:
        parsed = datetime.min.replace(tzinfo=UTC)
    if _canonical_utc_seconds(parsed) != meta.generated_at:
        raise _LexicalIndexCorrupt("lexical index is corrupt: generated_at is not canonical UTC RFC3339")
    previous = ""
    for item in meta.inputs:
        check_cancel(cancel)
        if item.path <= previous or item.bytes < 0 or _SHA256_PATTERN.fullmatch(item.sha256) is None:
            raise _LexicalIndexCorrupt("lexical index is corrupt: invalid input manifest")
        previous = item.path


def _read_lexical_index_meta(base: Base, cancel: Cancellation | None = None) -> LexicalIndexMeta:
    check_cancel(cancel)
    _, path = _lexical_index_paths(base)
    try:
        path.lstat()
    except FileNotFoundError:
        raise
    try:
        data = read_file_limited(path, MAX_SOURCE_DOCUMENT_BYTES)
    except FileTooLargeError as error:
        raise _LexicalIndexCorrupt(f"decode {LEXICAL_INDEX_META_PATH}: {error}") from error
    except OperationalError as error:
        raise _LexicalIndexCorrupt(f"decode {LEXICAL_INDEX_META_PATH}: {error}") from error
    check_cancel(cancel)
    return _decode_lexical_index_meta(data, cancel)


def _current_lexical_index_meta(
    base: Base, *, inputs: bool, cancel: Cancellation | None = None
) -> tuple[LexicalIndexMeta | None, LexicalIndexUse]:
    check_cancel(cancel)
    use = LexicalIndexUse()
    try:
        meta = _read_lexical_index_meta(base, cancel)
    except FileNotFoundError:
        return None, replace(use, reason=LEXICAL_INDEX_FALLBACK_MISSING)
    except _LexicalIndexStale:
        return None, replace(use, reason=LEXICAL_INDEX_FALLBACK_STALE)
    except LexicalIndexError, OSError, ValueError:
        return None, replace(use, reason=LEXICAL_INDEX_FALLBACK_CORRUPT)
    try:
        semantics = lexical_semantics_sha256(base, cancel=cancel)
        if semantics != meta.semantics_sha256:
            return None, replace(use, reason=LEXICAL_INDEX_FALLBACK_STALE)
        if inputs and lexical_inputs(base, meta.inputs, cancel=cancel)[2] != meta.inputs_sha256:
            return None, replace(use, reason=LEXICAL_INDEX_FALLBACK_STALE)
    except LexicalIndexError, OSError, ValueError:
        raise
    return meta, replace(use, used=True)


def _open_lexical_index_file(base: Base, meta: LexicalIndexMeta, cancel: Cancellation | None = None) -> Any:
    check_cancel(cancel)
    path, _ = _lexical_index_paths(base)
    path.lstat()
    try:
        handle = open_regular_file(path)
    except OperationalError as error:
        raise _LexicalIndexCorrupt(f"open {LEXICAL_INDEX_PATH}: {error}") from error
    try:
        if os.fstat(handle.fileno()).st_size != meta.bytes:
            raise _LexicalIndexCorrupt("lexical index file fingerprint does not match metadata")
        check_cancel(cancel)
    except BaseException:
        handle.close()
        raise
    return handle


def lexical_index_health(base: Base, *, cancel: Cancellation | None = None) -> LexicalIndexUse:
    """Classify sidecar, source inputs, and the postings artifact fingerprint."""

    meta, use = _current_lexical_index_meta(base, inputs=True, cancel=cancel)
    if meta is None:
        return use
    try:
        with _open_lexical_index_file(base, meta, cancel):
            pass
    except FileNotFoundError:
        return replace(use, used=False, reason=LEXICAL_INDEX_FALLBACK_MISSING)
    except OSError, LexicalIndexError:
        return replace(use, used=False, reason=LEXICAL_INDEX_FALLBACK_CORRUPT)
    return use


@dataclass(frozen=True, slots=True)
class _LexicalPostingLookup:
    key_hash: str
    offset: int
    bytes: int
    pairs: int
    sha256: str


def _read_section(handle: Any, offset: int, size: int) -> bytes:
    if offset < 0 or size < 0:
        raise _LexicalIndexCorrupt("lexical section has a negative boundary")
    data = os.pread(handle.fileno(), size, offset)
    if len(data) != size:
        raise _LexicalIndexCorrupt("lexical section is shorter than metadata")
    return data


def _canonical_int(value: str, *, base: int = 10) -> int:
    if not value:
        raise _LexicalIndexCorrupt("lexical integer is empty")
    if base == 10:
        if _INTEGER_PATTERN.fullmatch(value) is None:
            raise _LexicalIndexCorrupt("lexical integer is not canonical")
    elif base == 36 and (
        value != value.lower() or not all(character.isdigit() or "a" <= character <= "z" for character in value)
    ):
        raise _LexicalIndexCorrupt("lexical base36 integer is not canonical")
    try:
        parsed = int(value, base)
    except ValueError as error:
        raise _LexicalIndexCorrupt("lexical integer is invalid") from error
    canonical = str(parsed) if base == 10 else _base36(parsed)
    if parsed < 0 or canonical != value:
        raise _LexicalIndexCorrupt("lexical integer is not canonical")
    return parsed


# Canonical fragments encode exactly the bytes outside this ASCII alphabet.
# Compile once: every lookup validates all entry URIs before trusting offsets.
_FRAGMENT_LITERAL = frozenset("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._:/@+-")
_FRAGMENT_PATTERN = re.compile(
    r"(?:[A-Za-z0-9._:/@+\-]|%(?:"
    + "|".join(f"{value:02X}" for value in range(256) if chr(value) not in _FRAGMENT_LITERAL)
    + r"))*"
)
_LEXICAL_LAYERS = frozenset(str(layer) for layer in LAYERS)


def _valid_lexical_fragment(fragment: str) -> bool:
    return _FRAGMENT_PATTERN.fullmatch(fragment) is not None


def _valid_lexical_uri(uri: str) -> bool:
    if not uri or uri.strip() != uri:
        return False
    path, separator, fragment = uri.rpartition("#")
    if not separator:
        path = uri
    elif not fragment or not _valid_lexical_fragment(fragment):
        return False
    if "?" in path or "://" in path or path.endswith("/"):
        return False
    try:
        return clean_relative(path) == path and path != "."
    except InvalidUsageError, ValueError:
        return False


def _validate_entry(entry: LexicalEntry) -> None:
    if entry.kind not in _LEXICAL_LAYERS:
        raise _LexicalIndexCorrupt(f"lexical entry {entry.id} has invalid layer {entry.kind!r}")
    for value in (entry.date, entry.valid_from, entry.valid_until):
        if value:
            try:
                validate_date(value)
            except ValueError as error:
                raise _LexicalIndexCorrupt(f"lexical entry {entry.id} has invalid date {value!r}") from error
    if entry.time:
        try:
            parsed = datetime.fromisoformat(entry.time.removesuffix("Z") + "+00:00")
        except ValueError as error:
            raise _LexicalIndexCorrupt(f"lexical entry {entry.id} has invalid time {entry.time!r}") from error
        if _canonical_utc_seconds(parsed) != entry.time:
            raise _LexicalIndexCorrupt(f"lexical entry {entry.id} has invalid time {entry.time!r}")
    record = entry.is_record
    invalid = (
        entry.count == 1
        or (entry.count > 0 and entry.count != len(entry.collapsed))
        or (record and entry.context and not entry.collapsed)
        or (record and not entry.context and bool(entry.collapsed))
        or (not record and (entry.count != 0 or bool(entry.collapsed)))
        or (record and (entry.candidate_offset != 0 or entry.candidate_bytes != 0 or bool(entry.candidate_sha256)))
        or (not record and (entry.candidate_bytes < 2 or _SHA256_PATTERN.fullmatch(entry.candidate_sha256) is None))
    )
    if invalid:
        raise _LexicalIndexCorrupt(f"lexical entry {entry.id} has invalid collapse metadata")


def _decode_entry(fields: Sequence[str], expected_id: int) -> LexicalEntry:
    if len(fields) < 16:
        raise _LexicalIndexCorrupt("lexical entry row has too few fields")
    identifier = _canonical_int(fields[1])
    if identifier != expected_id:
        raise _LexicalIndexCorrupt("lexical entry IDs are not contiguous")
    if not _valid_lexical_uri(fields[2]):
        raise _LexicalIndexCorrupt(f"lexical entry {identifier} has invalid URI")
    if fields[9] not in {"true", "false"} or fields[10] not in {"true", "false"}:
        raise _LexicalIndexCorrupt(f"lexical entry {identifier} has invalid boolean flag")
    count = _canonical_int(fields[11])
    candidate_offset = _canonical_int(fields[12]) if fields[12] else 0
    candidate_bytes = _canonical_int(fields[13]) if fields[13] else 0
    entry = LexicalEntry(
        id=identifier,
        uri=fields[2],
        kind=fields[3],
        source=fields[4],
        date=fields[5],
        time=fields[6],
        valid_from=fields[7],
        valid_until=fields[8],
        context=fields[9] == "true",
        body_cached=fields[10] == "true",
        count=count,
        candidate_offset=candidate_offset,
        candidate_bytes=candidate_bytes,
        candidate_sha256=fields[14],
        rank=fields[15],
        collapsed=tuple(fields[16:]),
    )
    _validate_entry(entry)
    return entry


def _validate_collapse_entries(entries: Sequence[LexicalEntry], cancel: Cancellation | None = None) -> None:
    by_uri: dict[str, str] = {}
    sources: dict[str, str] = {}
    for entry in entries:
        check_cancel(cancel)
        if not entry.is_record or not entry.context or len(entry.collapsed) == 1:
            continue
        sources[entry.uri] = entry.source
        for uri in entry.collapsed:
            if uri in by_uri:
                raise _LexicalIndexCorrupt("lexical index repeats collapse membership")
            by_uri[uri] = entry.uri
    members = 0
    for entry in entries:
        check_cancel(cancel)
        if not entry.is_record:
            continue
        if len(entry.collapsed) == 1:
            if not entry.context or entry.collapsed[0] != entry.uri:
                raise _LexicalIndexCorrupt("lexical index has invalid singleton collapse membership")
            continue
        group = by_uri.get(entry.uri, "")
        if not group or sources.get(group) != entry.source:
            raise _LexicalIndexCorrupt("lexical index has invalid collapse membership")
        members += 1
    if members != len(by_uri):
        raise _LexicalIndexCorrupt("lexical index collapse membership names a missing record")


def _decode_entries(
    handle: Any, meta: LexicalIndexMeta, cancel: Cancellation | None = None
) -> tuple[tuple[LexicalEntry, ...], tuple[str, ...]]:
    check_cancel(cancel)
    prefix = _read_section(handle, 0, meta.postings_offset)
    check_cancel(cancel)
    if not prefix.endswith(b"\n"):
        raise _LexicalIndexCorrupt("lexical index entry section is not newline terminated")
    if hashlib.sha256(prefix).hexdigest() != meta.entries_sha256:
        raise _LexicalIndexCorrupt("lexical index entry rows do not match metadata")
    rows = prefix[:-1].split(b"\n")
    if any(len(row) >= MAX_LEXICAL_INDEX_LINE_BYTES for row in rows):
        raise _LexicalIndexCorrupt("lexical index entry row exceeds the line limit")
    lines: list[str] = []
    for row in rows:
        check_cancel(cancel)
        try:
            lines.append(row.decode())
        except UnicodeDecodeError as error:
            raise _LexicalIndexCorrupt("lexical index entry rows are not UTF-8") from error
    if not lines:
        raise _LexicalIndexCorrupt("lexical index entry section has no score-field dictionary")
    dictionary = lines[0].split("\t")
    if not dictionary or dictionary[0] != LEXICAL_SCORE_FIELDS_ROW:
        raise _LexicalIndexCorrupt("lexical index entry section has no score-field dictionary")
    score_fields = tuple(dictionary[1:])
    if score_fields and not _strictly_sorted_values(score_fields):
        raise _LexicalIndexCorrupt("lexical score-field dictionary is not canonical")
    entries: list[LexicalEntry] = []
    for line in lines[1:]:
        check_cancel(cancel)
        if not line.startswith(f"{LEXICAL_ENTRY_ROW}\t"):
            raise _LexicalIndexCorrupt("lexical index entry section contains a non-entry row")
        entries.append(_decode_entry(line.split("\t"), len(entries)))
    if len(entries) != meta.entries:
        raise _LexicalIndexCorrupt("lexical index entry rows do not match metadata")
    _validate_collapse_entries(entries, cancel)
    return tuple(entries), score_fields


def _raw_url_decode(value: str, label: str) -> bytes:
    if not value or "=" in value:
        raise _LexicalIndexCorrupt(f"{label} is not canonical base64url")
    try:
        decoded = base64.urlsafe_b64decode(value + "=" * (-len(value) % 4))
    except (ValueError, UnicodeEncodeError) as error:
        raise _LexicalIndexCorrupt(f"{label} is not canonical base64url") from error
    if _raw_url(decoded) != value:
        raise _LexicalIndexCorrupt(f"{label} is not canonical base64url")
    return decoded


def _decode_lookup_row(row: bytes, meta: LexicalIndexMeta, shard_index: int) -> _LexicalPostingLookup:
    try:
        fields = row.decode().split("\t")
    except UnicodeDecodeError as error:
        raise _LexicalIndexCorrupt("lexical lookup row is not UTF-8") from error
    if len(fields) != 6 or fields[0] != LEXICAL_LOOKUP_ROW:
        raise _LexicalIndexCorrupt("lexical lookup row has invalid fields")
    key = _raw_url_decode(fields[1], "lexical lookup key")
    if len(key) != 16 or (key[0] << 4 | key[1] >> 4) != shard_index:
        raise _LexicalIndexCorrupt("lexical lookup row is in the wrong shard")
    offset = _canonical_int(fields[2], base=36)
    row_bytes = _canonical_int(fields[3], base=36)
    pairs = _canonical_int(fields[4], base=36)
    digest = _raw_url_decode(fields[5], "lexical posting digest")
    if (
        offset < meta.postings_offset
        or offset >= meta.lookup_offset
        or row_bytes < 2
        or row_bytes > MAX_LEXICAL_INDEX_LINE_BYTES
        or row_bytes > meta.lookup_offset - offset
        or pairs < 1
        or len(digest) != hashlib.sha256().digest_size
    ):
        raise _LexicalIndexCorrupt("lexical lookup row has invalid posting metadata")
    return _LexicalPostingLookup(fields[1], offset, row_bytes, pairs, fields[5])


def _read_lookup_shard(
    handle: Any,
    meta: LexicalIndexMeta,
    shard_index: int,
    cancel: Cancellation | None = None,
) -> Mapping[str, _LexicalPostingLookup]:
    check_cancel(cancel)
    if shard_index < 0 or shard_index >= len(meta.lookup_shards):
        raise _LexicalIndexCorrupt("lexical lookup shard is outside the index")
    shard = meta.lookup_shards[shard_index]
    data = _read_section(handle, shard.offset, shard.bytes)
    check_cancel(cancel)
    if hashlib.sha256(data).hexdigest() != shard.sha256:
        raise _LexicalIndexCorrupt("lexical lookup shard does not match metadata")
    if data and not data.endswith(b"\n"):
        raise _LexicalIndexCorrupt("lexical lookup shard is not newline terminated")
    rows = data[:-1].split(b"\n") if data else []
    if any(len(row) >= MAX_LEXICAL_INDEX_LINE_BYTES for row in rows):
        raise _LexicalIndexCorrupt("lexical lookup row exceeds the line limit")
    result: dict[str, _LexicalPostingLookup] = {}
    previous = ""
    for row in rows:
        check_cancel(cancel)
        descriptor = _decode_lookup_row(row, meta, shard_index)
        if previous and previous >= descriptor.key_hash:
            raise _LexicalIndexCorrupt("lexical lookup rows are not in canonical order")
        if descriptor.key_hash in result:
            raise _LexicalIndexCorrupt("lexical lookup repeats a posting key")
        result[descriptor.key_hash] = descriptor
        previous = descriptor.key_hash
    if len(rows) != shard.rows:
        raise _LexicalIndexCorrupt("lexical lookup shard does not match metadata")
    return result


def _posting_key_from_row(row: bytes) -> LexicalPostingKey:
    fields = row.split(b"\t")
    if len(fields) != 3 or len(fields[0]) != 1:
        raise _LexicalIndexCorrupt("lexical posting row has invalid fields")
    try:
        kind = fields[0].decode("ascii")
    except UnicodeDecodeError as error:
        raise _LexicalIndexCorrupt("lexical posting row has an invalid kind") from error
    if kind not in {
        LEXICAL_CONTEXT_TOKEN,
        LEXICAL_CONTEXT_TRIGRAM,
        LEXICAL_CONTEXT_PHRASE,
        LEXICAL_FIND_TRIGRAM,
        LEXICAL_BODY_TRIGRAM,
    }:
        raise _LexicalIndexCorrupt("lexical posting row has an invalid kind")
    try:
        value = _raw_url_decode(fields[1].decode("ascii"), "lexical posting key").decode()
    except (UnicodeDecodeError, _LexicalIndexCorrupt) as error:
        if isinstance(error, _LexicalIndexCorrupt):
            raise
        raise _LexicalIndexCorrupt("lexical posting row has an invalid key") from error
    if not value:
        raise _LexicalIndexCorrupt("lexical posting row has an invalid key")
    return LexicalPostingKey(kind, value)


def _consume_uvarint(data: bytes, offset: int) -> tuple[int, int]:
    value = 0
    shift = 0
    start = offset
    for index in range(10):
        if offset >= len(data):
            raise _LexicalIndexCorrupt("lexical varint is truncated")
        current = data[offset]
        offset += 1
        if current < 0x80:
            if index == 9 and current > 1:
                raise _LexicalIndexCorrupt("lexical varint overflows uint64")
            value |= current << shift
            if _encode_uvarint(value) != data[start:offset]:
                raise _LexicalIndexCorrupt("lexical varint is not canonical")
            return value, offset
        value |= (current & 0x7F) << shift
        shift += 7
    raise _LexicalIndexCorrupt("lexical varint overflows uint64")


def _decode_term_score(payload: bytes, offset: int, score_fields: Sequence[str]) -> tuple[LexicalTermScore, int]:
    values: list[int] = []
    for _ in range(7):
        value, offset = _consume_uvarint(payload, offset)
        values.append(value)
    if values[0] > DIRECT_IDENTIFIER_PRIORITY or values[4] > len(score_fields) or values[6] > 1:
        raise _LexicalIndexCorrupt("lexical posting row has an invalid term score")
    segments: tuple[LexicalTermSegment, ...] = ()
    if any(values[index] for index in (2, 3, 4)):
        if any(values[index] == 0 for index in (2, 3, 4)):
            raise _LexicalIndexCorrupt("lexical posting row has an invalid term score")
        segments = (LexicalTermSegment(score_fields[values[4] - 1], values[2], values[3]),)
    non_body_match = bool(values[6])
    if (not segments and non_body_match) or (segments and segments[0].field != "body" and not non_body_match):
        raise _LexicalIndexCorrupt(
            "lexical posting row has an invalid term score: inconsistent non-body match metadata"
        )
    return (
        LexicalTermScore(
            LexicalTermAnalysis(True, values[0], values[1], segments, non_body_match),
            values[5],
        ),
        offset,
    )


def _decode_posting_payload(
    payload: bytes,
    key: LexicalPostingKey,
    entry_count: int,
    score_fields: Sequence[str],
    cancel: Cancellation | None = None,
) -> tuple[frozenset[int], Mapping[int, LexicalTermScore], int]:
    identifiers: set[int] = set()
    scores: dict[int, LexicalTermScore] = {}
    offset = 0
    previous = -1
    pairs = 0
    while offset < len(payload):
        check_cancel(cancel)
        delta, offset = _consume_uvarint(payload, offset)
        if delta == 0 or delta > entry_count:
            raise _LexicalIndexCorrupt("lexical posting row has invalid entry IDs")
        identifier = delta - 1 if previous < 0 else previous + delta
        if identifier < 0 or identifier >= entry_count:
            raise _LexicalIndexCorrupt("lexical posting row has invalid entry IDs")
        score = None
        if key.kind == LEXICAL_CONTEXT_TOKEN:
            score, offset = _decode_term_score(payload, offset, score_fields)
        identifiers.add(identifier)
        if score is not None:
            scores[identifier] = score
        previous = identifier
        pairs += 1
    if pairs == 0:
        raise _LexicalIndexCorrupt("lexical posting row has no entry IDs")
    return frozenset(identifiers), scores, pairs


def _decode_posting(
    row: bytes,
    key: LexicalPostingKey,
    entry_count: int,
    score_fields: Sequence[str],
    cancel: Cancellation | None = None,
) -> tuple[frozenset[int], Mapping[int, LexicalTermScore], int]:
    check_cancel(cancel)
    parsed = _posting_key_from_row(row)
    if parsed != key:
        raise _LexicalIndexCorrupt("lexical lookup names the wrong posting row")
    fields = row.split(b"\t")
    try:
        payload = _raw_url_decode(fields[2].decode("ascii"), "lexical posting payload")
    except UnicodeDecodeError as error:
        raise _LexicalIndexCorrupt("lexical posting row has an invalid payload") from error
    if not payload:
        raise _LexicalIndexCorrupt("lexical posting row has an invalid payload")
    return _decode_posting_payload(payload, key, entry_count, score_fields, cancel)


def _read_exact_row(handle: Any, offset: int, size: int, digest: str, *, raw: bool) -> bytes:
    encoded = _read_section(handle, offset, size)
    if len(encoded) < 2 or not encoded.endswith(b"\n"):
        raise _LexicalIndexCorrupt("lexical row does not match its authenticated lookup")
    actual = _raw_url(hashlib.sha256(encoded).digest()) if raw else hashlib.sha256(encoded).hexdigest()
    if actual != digest:
        raise _LexicalIndexCorrupt("lexical row does not match its authenticated lookup")
    return encoded[:-1]


def _read_posting(
    handle: Any,
    descriptor: _LexicalPostingLookup,
    key: LexicalPostingKey,
    entry_count: int,
    score_fields: Sequence[str],
    cancel: Cancellation | None = None,
) -> tuple[frozenset[int], Mapping[int, LexicalTermScore]]:
    check_cancel(cancel)
    row = _read_exact_row(handle, descriptor.offset, descriptor.bytes, descriptor.sha256, raw=True)
    if _encode_lookup_key(key) != descriptor.key_hash:
        raise _LexicalIndexCorrupt("lexical lookup names the wrong posting row")
    identifiers, scores, pairs = _decode_posting(row, key, entry_count, score_fields, cancel)
    if pairs != descriptor.pairs:
        raise _LexicalIndexCorrupt("lexical posting count does not match its lookup")
    return identifiers, scores


def _read_context_candidate(
    handle: Any,
    meta: LexicalIndexMeta,
    entry: LexicalEntry,
    cancel: Cancellation | None = None,
) -> LexicalCandidate:
    check_cancel(cancel)
    if (
        entry.is_record
        or entry.candidate_bytes < 2
        or entry.candidate_offset < 0
        or entry.candidate_bytes > meta.bytes - meta.candidates_offset - entry.candidate_offset
    ):
        raise _LexicalIndexCorrupt("lexical candidate location is outside its section")
    row = _read_exact_row(
        handle,
        meta.candidates_offset + entry.candidate_offset,
        entry.candidate_bytes,
        entry.candidate_sha256,
        raw=False,
    )
    check_cancel(cancel)
    fields = row.split(b"\t", 2)
    if len(fields) != 3 or fields[0] != LEXICAL_CANDIDATE_ROW.encode():
        raise _LexicalIndexCorrupt("lexical candidate row has invalid fields")
    try:
        identifier = _canonical_int(fields[1].decode("ascii"))
        encoded = fields[2].decode()
    except (UnicodeDecodeError, _LexicalIndexCorrupt) as error:
        if isinstance(error, _LexicalIndexCorrupt):
            raise
        raise _LexicalIndexCorrupt("lexical candidate row has invalid encoding") from error
    if identifier != entry.id or not encoded:
        raise _LexicalIndexCorrupt("lexical candidate row names the wrong entry")
    return _decode_context_candidate(entry, encoded)


def _load_context_candidates(
    base: Base,
    meta: LexicalIndexMeta,
    entries: Sequence[LexicalEntry],
    cancel: Cancellation | None = None,
) -> None:
    check_cancel(cancel)
    if not any(not entry.is_record for entry in entries):
        return
    with _open_lexical_index_file(base, meta, cancel) as handle:
        for entry in entries:
            check_cancel(cancel)
            if not entry.is_record:
                entry.candidate = _read_context_candidate(handle, meta, entry, cancel)
        if os.fstat(handle.fileno()).st_size != meta.bytes:
            raise _LexicalIndexCorrupt("lexical index file fingerprint does not match metadata")


def _decode_lexical_index(
    base: Base,
    meta: LexicalIndexMeta,
    wanted: Iterable[LexicalPostingKey],
    *,
    full: bool,
    prepared: LexicalContextPreparation | None = None,
    cancel: Cancellation | None = None,
) -> LexicalIndexData:
    check_cancel(cancel)
    wanted_set = frozenset(wanted)
    with _open_lexical_index_file(base, meta, cancel) as handle:
        if full:
            encoded = _read_section(handle, 0, meta.bytes)
            check_cancel(cancel)
            if hashlib.sha256(encoded).hexdigest() != meta.output_sha256:
                raise _LexicalIndexCorrupt("lexical index bytes do not match metadata")
        if prepared is None:
            entries, score_fields = _decode_entries(handle, meta, cancel)
        else:
            if prepared.meta != meta:
                raise _LexicalIndexCorrupt("prepared lexical generation does not match metadata")
            entries, score_fields = prepared.entries, prepared.score_fields
        postings: dict[LexicalPostingKey, frozenset[int]] = {}
        term_scores: dict[str, Mapping[int, LexicalTermScore]] = {}
        if full:
            section = _read_section(handle, meta.postings_offset, meta.lookup_offset - meta.postings_offset)
            check_cancel(cancel)
            if section and not section.endswith(b"\n"):
                raise _LexicalIndexCorrupt("lexical posting section is not newline terminated")
            rows = section[:-1].split(b"\n") if section else []
            if any(len(row) >= MAX_LEXICAL_INDEX_LINE_BYTES for row in rows):
                raise _LexicalIndexCorrupt("lexical posting row exceeds the line limit")
            previous: LexicalPostingKey | None = None
            posting_pairs = 0
            all_descriptors: dict[tuple[int, str], _LexicalPostingLookup] = {}
            for row in rows:
                check_cancel(cancel)
                key = _posting_key_from_row(row)
                if previous is not None and key <= previous:
                    raise _LexicalIndexCorrupt("lexical posting rows are not in canonical order")
                ids, scores, count = _decode_posting(row, key, len(entries), score_fields, cancel)
                posting_pairs += count
                if key in wanted_set:
                    postings[key] = ids
                    if key.kind == LEXICAL_CONTEXT_TOKEN:
                        term_scores[key.value] = scores
                previous = key
            if len(rows) != meta.posting_rows or posting_pairs != meta.postings:
                raise _LexicalIndexCorrupt("lexical posting rows do not match metadata")
            lookup_pairs = 0
            for shard_index in range(LEXICAL_LOOKUP_SHARD_COUNT):
                check_cancel(cancel)
                lookup = _read_lookup_shard(handle, meta, shard_index, cancel)
                for descriptor in lookup.values():
                    check_cancel(cancel)
                    row = _read_exact_row(handle, descriptor.offset, descriptor.bytes, descriptor.sha256, raw=True)
                    key = _posting_key_from_row(row)
                    if _encode_lookup_key(key) != descriptor.key_hash or lexical_posting_shard(key) != shard_index:
                        raise _LexicalIndexCorrupt("lexical lookup names the wrong posting row")
                    _read_posting(handle, descriptor, key, len(entries), score_fields, cancel)
                    all_descriptors[(shard_index, descriptor.key_hash)] = descriptor
                    lookup_pairs += descriptor.pairs
            if len(all_descriptors) != meta.posting_rows or lookup_pairs != meta.postings:
                raise _LexicalIndexCorrupt("lexical lookup rows do not match posting metadata")
            relative = 0
            for entry in entries:
                check_cancel(cancel)
                _decode_rank_candidate(entry)
                if entry.is_record:
                    continue
                if entry.candidate_offset != relative:
                    raise _LexicalIndexCorrupt("lexical candidate rows are not contiguous")
                _read_context_candidate(handle, meta, entry, cancel)
                relative += entry.candidate_bytes
            if relative != meta.bytes - meta.candidates_offset:
                raise _LexicalIndexCorrupt("lexical candidate rows do not match their section")
        else:
            grouped: dict[int, list[LexicalPostingKey]] = {}
            for key in sorted(wanted_set):
                check_cancel(cancel)
                grouped.setdefault(lexical_posting_shard(key), []).append(key)
            for shard_index in sorted(grouped):
                check_cancel(cancel)
                lookup = _read_lookup_shard(handle, meta, shard_index, cancel)
                for key in grouped[shard_index]:
                    check_cancel(cancel)
                    descriptor = lookup.get(_encode_lookup_key(key))
                    if descriptor is None:
                        continue
                    ids, scores = _read_posting(handle, descriptor, key, len(entries), score_fields, cancel)
                    postings[key] = ids
                    if key.kind == LEXICAL_CONTEXT_TOKEN:
                        term_scores[key.value] = scores
        if os.fstat(handle.fileno()).st_size != meta.bytes:
            raise _LexicalIndexCorrupt("lexical index file fingerprint does not match metadata")
    return LexicalIndexData(
        entries,
        score_fields,
        postings,
        term_scores,
        meta.inputs_sha256,
        meta.inputs,
        meta,
    )


def read_lexical_index_for_keys(
    base: Base,
    wanted: Iterable[LexicalPostingKey],
    *,
    cancel: Cancellation | None = None,
) -> tuple[LexicalIndexData | None, LexicalIndexUse]:
    """Read the authenticated prefix and complete lookup shards needed by ``wanted``."""

    meta, use = _current_lexical_index_meta(base, inputs=False, cancel=cancel)
    if meta is None:
        return None, use
    try:
        return _decode_lexical_index(base, meta, wanted, full=False, cancel=cancel), use
    except FileNotFoundError:
        return None, replace(use, used=False, reason=LEXICAL_INDEX_FALLBACK_MISSING)
    except LexicalIndexError, OSError, ValueError:
        return None, replace(use, used=False, reason=LEXICAL_INDEX_FALLBACK_CORRUPT)


def prepare_context_lexical_index(base: Base, *, cancel: Cancellation | None = None) -> LexicalContextPreparation:
    """Authenticate the shared entry prefix once for a context evaluation batch."""

    meta, use = _current_lexical_index_meta(base, inputs=False, cancel=cancel)
    if meta is None:
        return LexicalContextPreparation((), (), None, use)
    try:
        with _open_lexical_index_file(base, meta, cancel) as handle:
            entries, score_fields = _decode_entries(handle, meta, cancel)
            if os.fstat(handle.fileno()).st_size != meta.bytes:
                raise _LexicalIndexCorrupt("lexical index file fingerprint does not match metadata")
    except FileNotFoundError:
        return LexicalContextPreparation((), (), None, replace(use, used=False, reason=LEXICAL_INDEX_FALLBACK_MISSING))
    except LexicalIndexError, OSError, ValueError:
        return LexicalContextPreparation((), (), None, replace(use, used=False, reason=LEXICAL_INDEX_FALLBACK_CORRUPT))
    return LexicalContextPreparation(entries, score_fields, meta, use)


def _read_prepared_context_lexical_index_for_keys(
    base: Base,
    preparation: LexicalContextPreparation,
    wanted: Iterable[LexicalPostingKey],
    *,
    cancel: Cancellation | None = None,
) -> tuple[LexicalIndexData | None, LexicalIndexUse]:
    if preparation.meta is None:
        return None, preparation.use
    try:
        data = _decode_lexical_index(
            base,
            preparation.meta,
            wanted,
            full=False,
            prepared=preparation,
            cancel=cancel,
        )
        return data, preparation.use
    except FileNotFoundError:
        return None, replace(preparation.use, used=False, reason=LEXICAL_INDEX_FALLBACK_MISSING)
    except LexicalIndexError, OSError, ValueError:
        return None, replace(preparation.use, used=False, reason=LEXICAL_INDEX_FALLBACK_CORRUPT)


def read_lexical_index(
    base: Base,
    wanted: Iterable[LexicalPostingKey] = (),
    *,
    cancel: Cancellation | None = None,
) -> tuple[LexicalIndexData | None, LexicalIndexUse]:
    """Validate the full generation and retain only requested posting sets."""

    meta, use = _current_lexical_index_meta(base, inputs=True, cancel=cancel)
    if meta is None:
        return None, use
    try:
        return _decode_lexical_index(base, meta, wanted, full=True, cancel=cancel), use
    except FileNotFoundError:
        return None, replace(use, used=False, reason=LEXICAL_INDEX_FALLBACK_MISSING)
    except LexicalIndexError, OSError, ValueError:
        return None, replace(use, used=False, reason=LEXICAL_INDEX_FALLBACK_CORRUPT)


def lexical_index_status(base: Base, *, cancel: Cancellation | None = None) -> LexicalIndexUse:
    """Fully validate the current cache while classifying derived corruption fail-soft."""

    return read_lexical_index(base, cancel=cancel)[1]


def _context_lexical_keys(terms: Sequence[str]) -> tuple[frozenset[LexicalPostingKey], bool]:
    keys: set[LexicalPostingKey] = set()
    for term in terms:
        keys.add(LexicalPostingKey(LEXICAL_CONTEXT_TOKEN, term))
        if not identifier_shaped(term):
            continue
        grams = lexical_trigrams(term)
        if not grams:
            return frozenset(), False
        keys.update(LexicalPostingKey(LEXICAL_CONTEXT_TRIGRAM, gram) for gram in grams)
    return frozenset(keys), True


def _intersect_postings(
    postings: Mapping[LexicalPostingKey, frozenset[int]], kind: str, values: Iterable[str]
) -> set[int]:
    result: set[int] | None = None
    for value in values:
        ids = postings.get(LexicalPostingKey(kind, value), frozenset())
        result = set(ids) if result is None else result.intersection(ids)
    return result or set()


def _context_term_ids(postings: Mapping[LexicalPostingKey, frozenset[int]], term: str) -> set[int]:
    if not identifier_shaped(term):
        return set(postings.get(LexicalPostingKey(LEXICAL_CONTEXT_TOKEN, term), frozenset()))
    return _intersect_postings(postings, LEXICAL_CONTEXT_TRIGRAM, lexical_trigrams(term))


def _collapse_index(entries: Sequence[LexicalEntry]) -> tuple[Mapping[str, str], Mapping[str, str]]:
    by_uri: dict[str, str] = {}
    sources: dict[str, str] = {}
    for representative in entries:
        if not representative.is_record or not representative.context or len(representative.collapsed) == 1:
            continue
        sources[representative.uri] = representative.source
        for uri in representative.collapsed:
            if uri in by_uri:
                raise _LexicalIndexCorrupt("lexical index repeats collapse membership")
            by_uri[uri] = representative.uri
    return by_uri, sources


def _collapse_group_for(entry: LexicalEntry, by_uri: Mapping[str, str], sources: Mapping[str, str]) -> tuple[str, bool]:
    if not entry.is_record:
        return "", False
    if len(entry.collapsed) == 1:
        if not entry.context or entry.collapsed[0] != entry.uri:
            raise _LexicalIndexCorrupt("lexical index has invalid singleton collapse membership")
        return "", True
    group = by_uri.get(entry.uri, "")
    if not group or sources.get(group) != entry.source:
        raise _LexicalIndexCorrupt("lexical index has invalid collapse membership")
    return group, True


def _entry_chronology(entry: LexicalEntry) -> str:
    return entry.time or entry.date


def _windowed_context_entries(
    entries: Sequence[LexicalEntry], window: Window, as_of: str
) -> tuple[set[int], Mapping[int, LexicalEntry], tuple[str, ...]]:
    by_uri, sources = _collapse_index(entries)
    active: set[int] = set()
    groups: dict[str, list[LexicalEntry]] = {}
    consulted: list[str] = []
    duplicate_members = 0
    for entry in entries:
        group, record = _collapse_group_for(entry, by_uri, sources)
        if group:
            duplicate_members += 1
        if not entry.active(window, as_of):
            continue
        if entry.body_cached:
            consulted.append(entry.uri)
        if not record:
            if entry.context:
                active.add(entry.id)
        elif not group:
            active.add(entry.id)
        else:
            groups.setdefault(group, []).append(entry)
    if duplicate_members != len(by_uri):
        raise _LexicalIndexCorrupt("lexical index collapse membership names a missing record")
    replacements: dict[int, LexicalEntry] = {}
    for members in groups.values():
        # String chronology is canonical and compares directly; URI is the deterministic tie-break.
        representative = min(members, key=lambda item: item.uri)
        for member in members:
            if _entry_chronology(member) > _entry_chronology(representative):
                representative = member
        collapsed = tuple(sorted(member.uri for member in members))
        representative = replace(
            representative,
            context=True,
            collapsed=collapsed,
            count=len(collapsed) if len(collapsed) > 1 else 0,
        )
        active.add(representative.id)
        replacements[representative.id] = representative
    return active, replacements, tuple(sorted(consulted))


def _indexed_supersessions(
    entries: Sequence[LexicalEntry], active: set[int], replacements: Mapping[int, LexicalEntry]
) -> Mapping[str, LexicalSupersession]:
    candidates: dict[str, LexicalCandidate] = {}
    for original in entries:
        if original.id not in active:
            continue
        entry = replacements.get(original.id, original)
        if entry.is_record:
            continue
        candidates[entry.uri] = _decode_rank_candidate(entry)
    superseders: dict[str, list[LexicalCandidate]] = {}
    for candidate in candidates.values():
        for target in candidate.supersedes or ():
            if target in candidates and target != candidate.uri:
                superseders.setdefault(target, []).append(candidate)
    result = {uri: LexicalSupersession() for uri in candidates}

    def apply(uri: str, winner: LexicalCandidate) -> None:
        current = result.get(uri, LexicalSupersession())
        if (
            not current.by
            or winner.validity_rank > current.rank
            or (winner.validity_rank == current.rank and winner.uri < current.by)
        ):
            result[uri] = LexicalSupersession(winner.uri, winner.validity_rank)

    for target, choices in superseders.items():
        choices.sort(key=lambda candidate: candidate.uri)
        choices.sort(key=lambda candidate: candidate.validity_rank, reverse=True)
        winner = choices[0]
        apply(target, winner)
        for loser in choices[1:]:
            apply(loser.uri, winner)
    return result


def _prepare_rank_candidates(
    entries: Sequence[LexicalEntry],
    data: LexicalIndexData,
    terms: Sequence[str],
    query: str,
) -> frozenset[int]:
    hydrate: set[int] = set()
    term_ids = {term: _context_term_ids(data.postings, term) for term in terms}
    exact_ids = {term: data.postings.get(LexicalPostingKey(LEXICAL_CONTEXT_TOKEN, term), frozenset()) for term in terms}
    phrase = _lower(query.strip())
    phrase_ids: set[int] = set()
    exact_phrase_ids: frozenset[int] = frozenset()
    if len(terms) > 1:
        if lexical_phrase_supported(phrase):
            exact_phrase_ids = data.postings.get(LexicalPostingKey(LEXICAL_CONTEXT_PHRASE, phrase), frozenset())
        else:
            phrase_ids = _intersect_postings(data.postings, LEXICAL_CONTEXT_TRIGRAM, lexical_trigrams(phrase))
    for entry in entries:
        candidate = _decode_rank_candidate(entry)
        candidate.term_analysis = {}
        for term in terms:
            if entry.id not in term_ids[term]:
                candidate.term_analysis[term] = LexicalTermAnalysis()
                continue
            if identifier_shaped(term) and entry.id not in exact_ids[term]:
                hydrate.add(entry.id)
                continue
            score = data.term_scores.get(term, {}).get(entry.id)
            if score is None:
                hydrate.add(entry.id)
                continue
            if identifier_shaped(term) and not lexical_term_score_is_complete(candidate, term, score.analysis):
                hydrate.add(entry.id)
                continue
            candidate.term_analysis[term] = score.analysis
        if entry.id in exact_phrase_ids:
            candidate.indexed_phrases = {phrase}
        elif entry.id in phrase_ids:
            hydrate.add(entry.id)
        candidate.phrase_analysis_complete = entry.id not in hydrate
        entry.candidate = candidate
    return frozenset(hydrate)


def query_context_lexical_index(
    base: Base,
    terms: Sequence[str],
    pins: Sequence[str] = (),
    window: Window = _EMPTY_WINDOW,
    as_of: str = "",
    query: str = "",
    summarize: bool = False,
    *,
    preparation: LexicalContextPreparation | None = None,
    cancel: Cancellation | None = None,
) -> tuple[LexicalContextPlan | None, LexicalIndexUse]:
    """Return a conservative authenticated context candidate and statistics plan."""

    check_cancel(cancel)
    keys, supported = _context_lexical_keys(terms)
    if not supported:
        return None, LexicalIndexUse(reason=LEXICAL_INDEX_FALLBACK_QUERY_TOO_SHORT)
    phrase = _lower(query.strip())
    if summarize and len(terms) > 1:
        if lexical_phrase_supported(phrase):
            keys = keys.union({LexicalPostingKey(LEXICAL_CONTEXT_PHRASE, phrase)})
        else:
            keys = keys.union(LexicalPostingKey(LEXICAL_CONTEXT_TRIGRAM, gram) for gram in lexical_trigrams(phrase))
    if preparation is None:
        data, use = read_lexical_index_for_keys(base, keys, cancel=cancel)
    else:
        data, use = _read_prepared_context_lexical_index_for_keys(base, preparation, keys, cancel=cancel)
    if data is None:
        return None, use
    try:
        active, replacements, consulted = _windowed_context_entries(data.entries, window, as_of)
        supersessions = _indexed_supersessions(data.entries, active, replacements)
        pinned = set(pins)
        pinned_ids = {
            entry.id
            for original in data.entries
            if original.id in active
            and not (entry := replacements.get(original.id, original)).is_record
            and entry.uri in pinned
        }
        selected = set(pinned_ids)
        for term in terms:
            check_cancel(cancel)
            selected.update(_context_term_ids(data.postings, term).intersection(active))
        chosen: list[LexicalEntry] = []
        omitted: list[str] = []
        pinnable: list[str] = []
        for original in data.entries:
            check_cancel(cancel)
            if original.id not in active:
                continue
            entry = replacements.get(original.id, original)
            if entry.kind in {str(Layer.WIKI), str(Layer.PROJECTS)}:
                pinnable.append(entry.uri)
            if entry.id in selected:
                # Query preparation annotates candidates, so isolate those mutations
                # from the shared entry generation retained by an eval batch.
                chosen.append(replace(entry, candidate=None))
            else:
                omitted.append(entry.uri)
        if summarize:
            hydrate = _prepare_rank_candidates(chosen, data, terms, query)
        else:
            _load_context_candidates(base, data.meta, chosen, cancel)
            hydrate = frozenset()
    except LexicalIndexError, OSError, ValueError, TypeError:
        return None, LexicalIndexUse(reason=LEXICAL_INDEX_FALLBACK_CORRUPT)
    return (
        LexicalContextPlan(
            entries=tuple(chosen),
            omitted=tuple(omitted),
            consulted_bodies=consulted,
            pinnable=tuple(pinnable),
            total=len(active),
            inputs_sha256=data.inputs_sha256,
            inputs=data.inputs,
            unharvested_bullets=data.meta.unharvested_bullets,
            meta=data.meta,
            summarized=summarize,
            hydrate_ids=hydrate,
            supersessions=supersessions,
        ),
        use,
    )


def _find_lexical_keys(terms: Sequence[str], bodies: bool) -> tuple[frozenset[LexicalPostingKey], bool]:
    keys: set[LexicalPostingKey] = set()
    for term in terms:
        if len(term) < 3:
            return frozenset(), False
        for gram in lexical_trigrams(term):
            keys.add(LexicalPostingKey(LEXICAL_FIND_TRIGRAM, gram))
            if bodies:
                keys.add(LexicalPostingKey(LEXICAL_BODY_TRIGRAM, gram))
    return frozenset(keys), True


def _find_term_ids(postings: Mapping[LexicalPostingKey, frozenset[int]], term: str, bodies: bool) -> set[int]:
    records = _intersect_postings(postings, LEXICAL_FIND_TRIGRAM, lexical_trigrams(term))
    if bodies:
        records.update(_intersect_postings(postings, LEXICAL_BODY_TRIGRAM, lexical_trigrams(term)))
    return records


def query_find_lexical_index(
    base: Base,
    terms: Sequence[str],
    *,
    bodies: bool = False,
    layers: Sequence[Layer] = (),
    sources: Sequence[str] = (),
    window: Window = _EMPTY_WINDOW,
    record_only: bool = False,
    cancel: Cancellation | None = None,
) -> tuple[LexicalFindPlan | None, LexicalIndexUse]:
    """Return conservative URI candidates for find without granting the cache result authority."""

    check_cancel(cancel)
    keys, supported = _find_lexical_keys(terms, bodies)
    if not supported:
        return None, LexicalIndexUse(reason=LEXICAL_INDEX_FALLBACK_QUERY_TOO_SHORT)
    data, use = read_lexical_index_for_keys(base, keys, cancel=cancel)
    if data is None:
        return None, use
    selected: set[int] | None = None
    for term in terms:
        check_cancel(cancel)
        term_ids = _find_term_ids(data.postings, term, bodies)
        selected = term_ids if selected is None else selected.intersection(term_ids)
    selected = selected or set()
    wanted_layers = {str(layer) for layer in layers}
    wanted_sources = set(sources)
    result: set[str] = set()
    for entry in data.entries:
        check_cancel(cancel)
        selected_record = entry.id in selected
        authored_page = not entry.is_record and not record_only
        if not selected_record and not authored_page:
            continue
        if wanted_layers and entry.kind not in wanted_layers:
            continue
        try:
            layer = Layer(entry.kind)
        except ValueError:
            return None, LexicalIndexUse(reason=LEXICAL_INDEX_FALLBACK_CORRUPT)
        if not base.store.enabled(layer):
            continue
        if record_only and layer not in {Layer.EVENTS, Layer.INDEX}:
            continue
        if wanted_sources and entry.source not in wanted_sources:
            continue
        if layer in {Layer.EVENTS, Layer.TASKS} and entry.date and not window.contains(entry.date):
            continue
        result.add(entry.uri)
    return LexicalFindPlan(frozenset(result), data.inputs_sha256, data.inputs), use


__all__ = [
    "LEXICAL_BODY_TRIGRAM",
    "LEXICAL_CONTEXT_PHRASE",
    "LEXICAL_CONTEXT_TOKEN",
    "LEXICAL_CONTEXT_TRIGRAM",
    "LEXICAL_FIND_TRIGRAM",
    "LEXICAL_INDEX_EXTRACTOR_VERSION",
    "LEXICAL_INDEX_FALLBACK_CORRUPT",
    "LEXICAL_INDEX_FALLBACK_MISSING",
    "LEXICAL_INDEX_FALLBACK_QUERY_TOO_SHORT",
    "LEXICAL_INDEX_FALLBACK_STALE",
    "LEXICAL_INDEX_FORMAT",
    "LEXICAL_INDEX_META_PATH",
    "LEXICAL_INDEX_PATH",
    "LEXICAL_INDEX_SCHEMA_VERSION",
    "LEXICAL_LOOKUP_SHARD_COUNT",
    "RANKING_VERSION",
    "LexicalCandidate",
    "LexicalCandidateSegment",
    "LexicalContextPlan",
    "LexicalContextPreparation",
    "LexicalFindPlan",
    "LexicalIdentifierBound",
    "LexicalIndexBuild",
    "LexicalIndexData",
    "LexicalIndexError",
    "LexicalIndexMeta",
    "LexicalIndexUse",
    "LexicalInputFile",
    "LexicalLookupShard",
    "LexicalPostingKey",
    "LexicalSupersession",
    "LexicalTermAnalysis",
    "LexicalTermScore",
    "LexicalTermSegment",
    "build_lexical_index",
    "candidate_semantic_digest",
    "identifier_shaped",
    "lexical_candidate_identifier_bounds",
    "lexical_identifier_subterms",
    "lexical_index_health",
    "lexical_index_status",
    "lexical_inputs",
    "lexical_inputs_match",
    "lexical_phrase_supported",
    "lexical_phrase_words",
    "lexical_posting_shard",
    "lexical_semantics_sha256",
    "lexical_term_score_is_complete",
    "lexical_term_scores",
    "lexical_trigrams",
    "normalize_query_terms",
    "prepare_context_lexical_index",
    "query_context_lexical_index",
    "query_find_lexical_index",
    "read_lexical_index",
    "read_lexical_index_for_keys",
]
