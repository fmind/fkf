"""Strict, offline recall-at-k evaluation over the context delivery contract."""

from __future__ import annotations

from dataclasses import dataclass, field
from datetime import datetime
from pathlib import Path
from typing import Final

from pydantic import BaseModel, ConfigDict, ValidationError
from pydantic import Field as PydanticField

from fkf.base import Base
from fkf.config import ConfigError, decode_strict_yaml
from fkf.context import (
    CONTEXT_DELIVERY_JSON,
    CONTEXT_DELIVERY_JSONL,
    CONTEXT_DELIVERY_TEXT,
    DEFAULT_BUDGET,
    ContextPack,
    ContextRequest,
    _ContextBatchError,
    _map_contexts,
    render_context_bytes,
)
from fkf.io import read_file_limited
from fkf.jsoncodec import dumps
from fkf.lexical import RANKING_VERSION, LexicalIndexUse
from fkf.output import register_text
from fkf.process import Cancellation
from fkf.query import Window
from fkf.read import read
from fkf.store import MAX_CONFIG_BYTES, MAX_NARRATIVE_BYTES, validate_within_root
from fkf.uri import URIError, parse_uri

EVAL_SCHEMA_VERSION: Final = 1
EVAL_PATH: Final = "checks/queries.yaml"
MAX_EVAL_K: Final = 100
MAX_EVAL_BUDGET: Final = MAX_NARRATIVE_BYTES // 4
_DELIVERIES: Final = frozenset({CONTEXT_DELIVERY_JSON, CONTEXT_DELIVERY_JSONL, CONTEXT_DELIVERY_TEXT})


class _Boundary(BaseModel):
    model_config = ConfigDict(extra="forbid", strict=True)


class _WindowModel(_Boundary):
    since: str = ""
    until: str = ""


class _QueryModel(_Boundary):
    name: str
    question: str
    window: _WindowModel = _WindowModel()
    k: int | None = None
    budget: int | None = None
    delivery: str = ""
    expect_empty: bool = False
    expected_uris: list[str] = PydanticField(default_factory=list)
    forbidden_uris: list[str] = PydanticField(default_factory=list)
    expected_excerpts: dict[str, list[str]] = PydanticField(default_factory=dict)
    expected_reads: dict[str, list[str]] = PydanticField(default_factory=dict)


class _SuiteModel(_Boundary):
    fkf: int
    k: int
    budget: int | None = None
    delivery: str = ""
    recall_threshold: float
    queries: list[_QueryModel]


@dataclass(frozen=True, slots=True)
class EvalExpectedRank:
    uri: str
    rank: int


@dataclass(frozen=True, slots=True)
class EvalQueryResult:
    name: str
    question: str
    window: Window
    budget: int
    delivery: str
    k: int
    expect_empty: bool
    recall: float
    expected: int
    found_expected: int
    expected_ranks: tuple[EvalExpectedRank, ...]
    missing_expected: tuple[str, ...] = field(default=(), metadata={"json": "missing_expected,omitempty"})
    forbidden_found: tuple[str, ...] = field(default=(), metadata={"json": "forbidden_found,omitempty"})
    delivered_uris: tuple[str, ...] = ()
    delivered_bytes: int = 0
    delivered_tokens: int = 0
    index: LexicalIndexUse = field(default_factory=LexicalIndexUse)
    input_digest: str = ""
    ranking_version: int = RANKING_VERSION
    recall_threshold: float = 0.0
    passed: bool = False
    missing_evidence: tuple[str, ...] = field(default=(), metadata={"json": "missing_evidence,omitempty"})


@dataclass(frozen=True, slots=True)
class EvalReport:
    path: str
    k: int
    budget: int
    delivery: str
    recall_threshold: float
    evaluation_time: str
    queries: tuple[EvalQueryResult, ...]
    passed_queries: int
    failed: int = field(metadata={"json": "failed_queries"})
    passed: bool = False


@dataclass(frozen=True, slots=True)
class _EvalQuery:
    name: str
    question: str
    window: Window
    k: int | None
    budget: int | None
    delivery: str
    expect_empty: bool
    expected_uris: tuple[str, ...]
    forbidden_uris: tuple[str, ...]
    expected_excerpts: dict[str, tuple[str, ...]]
    expected_reads: dict[str, tuple[str, ...]]


@dataclass(frozen=True, slots=True)
class _EvalSuite:
    k: int
    budget: int
    delivery: str
    recall_threshold: float
    queries: tuple[_EvalQuery, ...]


def _invalid(message: str, *, cause: BaseException | None = None) -> ConfigError:
    return ConfigError(f"invalid configuration: {message}", cause=cause)


def _format_evaluation_time(value: datetime, *, seconds: bool = False) -> str:
    if value.tzinfo is None or value.utcoffset() is None:
        raise ValueError("evaluation timestamp must include a timezone")
    rendered = value.isoformat(timespec="seconds" if seconds else "auto")
    return rendered.removesuffix("+00:00") + "Z" if rendered.endswith("+00:00") else rendered


def _delivery(value: str, label: str) -> str:
    selected = value or CONTEXT_DELIVERY_JSON
    if selected not in _DELIVERIES:
        raise _invalid(f"{label}delivery {selected!r} must be json, jsonl, or text")
    return selected


def _canonical_uris(values: tuple[str, ...], label: str) -> tuple[str, ...]:
    result: list[str] = []
    seen: set[str] = set()
    for value in values:
        try:
            rendered = str(parse_uri(value))
        except URIError as error:
            raise _invalid(f"{label}: {error}", cause=error) from error
        if rendered in seen:
            raise _invalid(f"{label}: URI {rendered!r} is duplicated")
        seen.add(rendered)
        result.append(rendered)
    return tuple(result)


def _query(value: _QueryModel, index: int, names: set[str]) -> _EvalQuery:
    name = value.name.strip()
    question = value.question.strip()
    if not name or not question:
        raise _invalid(f"{EVAL_PATH}: queries[{index}] needs non-empty name and question")
    if name in names:
        raise _invalid(f"{EVAL_PATH}: queries[{index}].name {name!r} is duplicated")
    names.add(name)
    if value.k is not None and not 1 <= value.k <= MAX_EVAL_K:
        raise _invalid(f"{EVAL_PATH}: queries[{index}].k is {value.k}; expected 1..{MAX_EVAL_K} when set")
    if value.budget is not None and not 1 <= value.budget <= MAX_EVAL_BUDGET:
        raise _invalid(
            f"{EVAL_PATH}: queries[{index}].budget is {value.budget}; expected 1..{MAX_EVAL_BUDGET} when set"
        )
    delivery = _delivery(value.delivery, f"{EVAL_PATH}: queries[{index}]: ") if value.delivery else ""
    if value.expect_empty and (value.expected_uris or value.forbidden_uris):
        raise _invalid(
            f"{EVAL_PATH}: queries[{index}].expect_empty cannot be combined with expected_uris or forbidden_uris"
        )
    if not value.expect_empty and not value.expected_uris:
        raise _invalid(
            f"{EVAL_PATH}: queries[{index}].expected_uris must contain at least one URI unless expect_empty is true"
        )
    expected = _canonical_uris(tuple(value.expected_uris), f"{EVAL_PATH}: queries[{index}].expected_uris")
    forbidden = _canonical_uris(tuple(value.forbidden_uris), f"{EVAL_PATH}: queries[{index}].forbidden_uris")
    overlap = next((uri for uri in forbidden if uri in expected), "")
    if overlap:
        raise _invalid(f"{EVAL_PATH}: queries[{index}] URI {overlap!r} is both expected and forbidden")
    for kind, evidence in (("expected_excerpts", value.expected_excerpts), ("expected_reads", value.expected_reads)):
        for uri, snippets in evidence.items():
            if uri not in expected or not snippets or any(not snippet.strip() for snippet in snippets):
                raise _invalid(
                    f"{EVAL_PATH}: queries[{index}].{kind} requires expected URI keys and non-empty text lists"
                )
    return _EvalQuery(
        name,
        question,
        Window(value.window.since, value.window.until),
        value.k,
        value.budget,
        delivery,
        value.expect_empty,
        expected,
        forbidden,
        {uri: tuple(snippets) for uri, snippets in value.expected_excerpts.items()},
        {uri: tuple(snippets) for uri, snippets in value.expected_reads.items()},
    )


def _load_suite(base: Base) -> _EvalSuite:
    absolute = base.root / "checks" / "queries.yaml"
    validate_within_root(base.root, absolute)
    try:
        data = read_file_limited(absolute, MAX_CONFIG_BYTES)
    except OSError as error:
        if isinstance(error.__cause__, FileNotFoundError):
            raise _invalid(f"{EVAL_PATH} is missing; add the base's retrieval evaluation set", cause=error) from error
        raise
    try:
        raw = decode_strict_yaml(data, Path(EVAL_PATH))
        model = _SuiteModel.model_validate(raw)
    except ConfigError:
        raise
    except ValidationError as error:
        problem = error.errors(include_url=False)[0]
        location = ".".join(str(part) for part in problem["loc"])
        message = "field not found" if problem["type"] == "extra_forbidden" else str(problem["msg"])
        raise _invalid(f"{EVAL_PATH}: {location}: {message}", cause=error) from error
    if model.fkf != EVAL_SCHEMA_VERSION:
        raise _invalid(f"{EVAL_PATH}: fkf must be {EVAL_SCHEMA_VERSION}; got {model.fkf}")
    if not 1 <= model.k <= MAX_EVAL_K:
        raise _invalid(f"{EVAL_PATH}: k is {model.k}; expected 1..{MAX_EVAL_K}")
    budget = model.budget if model.budget is not None else DEFAULT_BUDGET
    if not 1 <= budget <= MAX_EVAL_BUDGET:
        raise _invalid(f"{EVAL_PATH}: budget is {budget}; expected 1..{MAX_EVAL_BUDGET} when set")
    if not 0 < model.recall_threshold <= 1:
        raise _invalid(
            f"{EVAL_PATH}: recall_threshold is {model.recall_threshold:g}; expected greater than 0 and at most 1"
        )
    if not model.queries:
        raise _invalid(f"{EVAL_PATH}: queries must contain at least one evaluation")
    names: set[str] = set()
    queries = tuple(_query(value, index, names) for index, value in enumerate(model.queries))
    return _EvalSuite(model.k, budget, _delivery(model.delivery, f"{EVAL_PATH}: "), model.recall_threshold, queries)


def _context_request(suite: _EvalSuite, query: _EvalQuery, evaluation_time: datetime) -> ContextRequest:
    budget = query.budget if query.budget is not None else suite.budget
    delivery = query.delivery or suite.delivery
    return ContextRequest(
        query.question,
        query.window,
        budget,
        delivery_format=delivery,
        evaluation_time=evaluation_time,
    )


def _evaluate_query(
    base: Base, suite: _EvalSuite, query: _EvalQuery, pack: ContextPack, cancel: Cancellation | None
) -> EvalQueryResult:
    budget = query.budget if query.budget is not None else suite.budget
    k = query.k if query.k is not None else suite.k
    delivery = query.delivery or suite.delivery
    delivered = tuple(item.uri for item in pack.items)
    ranks = {uri: index for index, uri in enumerate(delivered, start=1)}
    expected_ranks = tuple(EvalExpectedRank(uri, ranks.get(uri, 0)) for uri in query.expected_uris)
    missing = tuple(item.uri for item in expected_ranks if item.rank == 0 or item.rank > k)
    found = len(expected_ranks) - len(missing)
    forbidden = tuple(uri for uri in query.forbidden_uris if uri in ranks)
    encoded = render_context_bytes(pack, delivery)
    missing_evidence: list[str] = []
    for kind, evidence in (("excerpt", query.expected_excerpts), ("read", query.expected_reads)):
        for uri, snippets in evidence.items():
            item = next((item for item in pack.items[:k] if item.uri == uri), None)
            content = item.excerpt if item is not None else ""
            if item is not None and kind == "read":
                result = read(base, uri, cancel=cancel)
                content = result.text or (dumps(result.record).decode() if result.record is not None else "")
            if any(snippet.casefold() not in content.casefold() for snippet in snippets):
                missing_evidence.append(f"{kind}: {uri}")
    if query.expect_empty:
        recall = 0.0 if delivered or pack.matched_but_omitted else 1.0
        passed = not delivered and not pack.matched_but_omitted
    else:
        recall = found / len(query.expected_uris)
        passed = recall >= suite.recall_threshold and not forbidden and not missing_evidence
    return EvalQueryResult(
        query.name,
        query.question,
        pack.receipt.window,
        budget,
        delivery,
        k,
        query.expect_empty,
        recall,
        len(query.expected_uris),
        found,
        expected_ranks,
        missing,
        forbidden,
        delivered,
        len(encoded),
        (len(encoded) + 3) // 4,
        pack.receipt.index,
        pack.receipt.input_digest,
        pack.receipt.ranking_version,
        suite.recall_threshold,
        passed,
        tuple(missing_evidence),
    )


def evaluate(base: Base, *, cancel: Cancellation | None = None) -> EvalReport:
    """Run every declared query against stored evidence without writing or executing."""

    suite = _load_suite(base)
    evaluation_time = base.now()
    requests = tuple(_context_request(suite, query, evaluation_time) for query in suite.queries)
    try:
        results = _map_contexts(
            base,
            requests,
            lambda index, pack: _evaluate_query(base, suite, suite.queries[index], pack, cancel),
            cancel=cancel,
        )
    except _ContextBatchError as failure:
        from fkf.errors import CanceledError, InvalidUsageError, OperationalError

        error = failure.error
        query = suite.queries[failure.index]
        if isinstance(error, CanceledError):
            raise error from failure
        if isinstance(error, InvalidUsageError):
            raise InvalidUsageError(f"evaluate {query.name}: {error}", cause=error) from error
        if isinstance(error, OperationalError):
            raise OperationalError(f"evaluate {query.name}: {error}", cause=error) from error
        raise error from failure
    passed_queries = sum(result.passed for result in results)
    failed = len(results) - passed_queries
    return EvalReport(
        EVAL_PATH,
        suite.k,
        suite.budget,
        suite.delivery,
        suite.recall_threshold,
        _format_evaluation_time(evaluation_time),
        tuple(results),
        passed_queries,
        failed,
        failed == 0,
    )


def _text(report: EvalReport) -> str:
    lines: list[str] = []
    for query in report.queries:
        state = "PASS" if query.passed else "FAIL"
        lines.append(
            f"{state} {query.name:<24} recall@{query.k} {query.recall:.3f} "
            f"({query.found_expected}/{query.expected}) · budget {query.budget} · {query.delivery} delivered "
            f"{len(query.delivered_uris)} items, {query.delivered_tokens} tokens, {query.delivered_bytes} bytes"
        )
        if query.expected_ranks:
            rendered = "[" + " ".join(f"{{{item.uri} {item.rank}}}" for item in query.expected_ranks) + "]"
            lines.append(f"  expected ranks: {rendered}")
        if query.missing_expected:
            lines.append(f"  missing: [{' '.join(query.missing_expected)}]")
        if query.forbidden_found:
            lines.append(f"  forbidden: [{' '.join(query.forbidden_found)}]")
        if query.missing_evidence:
            lines.append(f"  missing evidence: [{', '.join(query.missing_evidence)}]")
    lines.append(
        f"{report.passed_queries} passed, {report.failed} failed · threshold {report.recall_threshold:.3f} · "
        f"default k {report.k} · default budget {report.budget} · "
        f"evaluated {_format_evaluation_time(datetime.fromisoformat(report.evaluation_time), seconds=True)} "
        f"· {report.path}"
    )
    return "\n".join(lines)


register_text(EvalReport, _text)


__all__ = [
    "EVAL_PATH",
    "EVAL_SCHEMA_VERSION",
    "MAX_EVAL_BUDGET",
    "MAX_EVAL_K",
    "EvalExpectedRank",
    "EvalQueryResult",
    "EvalReport",
    "evaluate",
]
