"""Observe FKF CLI latency over a deterministic local scale corpus."""

from __future__ import annotations

import argparse
import json
import os
import platform
import sys
import tempfile
import time
from collections.abc import Callable, Sequence
from dataclasses import dataclass
from datetime import UTC, date, datetime, timedelta
from datetime import time as datetime_time
from pathlib import Path
from typing import Any, Final

from fkf import __version__
from fkf.base import open_base
from fkf.documents import Document, Record, event_document_uri, fields_of, schema_of
from fkf.graph import build_graph
from fkf.jsoncodec import JsonValue
from fkf.process import Command, CommandError, SubprocessRunner
from fkf.query import Window
from fkf.store import BASE_DIR_MODE, BASE_FILE_MODE, Layer
from fkf.timeutil import DurationNS

DEFAULT_RECORDS: Final = 100_000
DEFAULT_RELATIONS_PER_RECORD: Final = 5
DEFAULT_TIMEOUT_SECONDS: Final = 300.0
MAX_RECORDS: Final = 1_000_000
MAX_RELATIONS_PER_RECORD: Final = 100
SCALE_NOW: Final = datetime(2026, 1, 31, 12, tzinfo=UTC)

_SCALE_CONFIG: Final = """\
fkf: 1
name: scale
schema:
  id: {description: Stable benchmark record identity., cardinality: one}
  time: {description: Fixed benchmark event time., cardinality: one}
  title: {description: Searchable benchmark title., cardinality: one}
  related: {description: Synthetic benchmark relationships., cardinality: many, relation: true}
layers:
  events: true
  index: false
  tasks: false
  projects: false
  wiki: false
sources:
  scale:
    enabled: true
    layer: events
    run: [printf, "[]"]
    fields:
      id: .id
      time: .time
      title: .title
      related: .related[]
"""


@dataclass(frozen=True, slots=True)
class ScaleCorpus:
    """The exact dimensions and addresses shared by every observation."""

    root: Path
    runtime: Path
    records: int
    edges: int
    window: Window
    first_record_uri: str


@dataclass(frozen=True, slots=True)
class _Operation:
    name: str
    arguments: tuple[str, ...]
    summarize: Callable[[dict[str, Any]], str]


@dataclass(frozen=True, slots=True)
class _Observation:
    operation: str
    elapsed_seconds: float
    result: str


class BenchmarkError(RuntimeError):
    """The benchmark could not complete one bounded observation."""


def _bounded_positive(value: int, *, name: str, maximum: int) -> int:
    if value < 1 or value > maximum:
        raise ValueError(f"{name} must be in 1..{maximum:,}; got {value:,}")
    return value


def _utc_boundary(day: date) -> str:
    return datetime.combine(day, datetime_time.min, UTC).isoformat().replace("+00:00", "Z")


def create_scale_corpus(
    workspace: Path,
    *,
    records: int = DEFAULT_RECORDS,
    relations_per_record: int = DEFAULT_RELATIONS_PER_RECORD,
) -> ScaleCorpus:
    """Create one deterministic base and its validated relation graph."""

    record_count = _bounded_positive(records, name="records", maximum=MAX_RECORDS)
    relation_count = _bounded_positive(
        relations_per_record,
        name="relations per record",
        maximum=MAX_RELATIONS_PER_RECORD,
    )
    root = workspace / "base"
    runtime = workspace / "runtime"
    root.mkdir(parents=True, mode=BASE_DIR_MODE)
    runtime.mkdir(parents=True, mode=BASE_DIR_MODE)
    (runtime / "home").mkdir(mode=BASE_DIR_MODE)
    (runtime / "state").mkdir(mode=BASE_DIR_MODE)
    config_path = root / "fkf.yaml"
    config_path.write_text(_SCALE_CONFIG, encoding="utf-8")
    config_path.chmod(BASE_FILE_MODE)

    base = open_base(str(root))
    base.now = lambda: SCALE_NOW
    source = base.source("scale")
    document_count = min(record_count, 10)
    first_date = SCALE_NOW.date() - timedelta(days=document_count)

    for document_index in range(document_count):
        day = first_date + timedelta(days=document_index)
        start = document_index * record_count // document_count
        end = (document_index + 1) * record_count // document_count
        day_records: list[Record] = []
        for record_index in range(start, end):
            identity = f"record-{record_index:06d}"
            related: list[JsonValue] = [
                f"topic:scale/{identity}/{relation_index}" for relation_index in range(relation_count)
            ]
            day_records.append(
                {
                    "id": identity,
                    "time": f"{day.isoformat()}T12:00:00Z",
                    "title": f"scale benchmark {identity}",
                    "related": related,
                }
            )
        next_day = day + timedelta(days=1)
        base.write_document(
            Document(
                source=source.name,
                layer=Layer.EVENTS,
                date=day.isoformat(),
                window_start=_utc_boundary(day),
                window_end=_utc_boundary(next_day),
                collected_at=SCALE_NOW.isoformat().replace("+00:00", "Z"),
                schema=schema_of(source),
                fields=fields_of(source),
                count=len(day_records),
                records=day_records,
            )
        )

    graph = build_graph(base)
    expected_edges = record_count * relation_count
    if graph.edges != expected_edges:
        raise BenchmarkError(f"scale graph has {graph.edges:,} edges; expected {expected_edges:,}")
    last_date = first_date + timedelta(days=document_count - 1)
    return ScaleCorpus(
        root=root,
        runtime=runtime,
        records=record_count,
        edges=expected_edges,
        window=Window(first_date.isoformat(), last_date.isoformat()),
        first_record_uri=f"{event_document_uri(first_date.isoformat(), source.name)}#record-000000",
    )


def _integer(payload: dict[str, Any], name: str) -> int | None:
    value = payload.get(name)
    return value if isinstance(value, int) and not isinstance(value, bool) else None


def _summarize_find(payload: dict[str, Any]) -> str:
    scanned = _integer(payload, "scanned")
    matched = _integer(payload, "matched")
    return f"{matched:,} matched / {scanned:,} scanned" if matched is not None and scanned is not None else "completed"


def _summarize_context(payload: dict[str, Any]) -> str:
    items = payload.get("items")
    receipt = payload.get("receipt")
    item_count = len(items) if isinstance(items, list) else None
    tokens = _integer(receipt, "encoded_tokens") if isinstance(receipt, dict) else None
    if item_count is not None and tokens is not None:
        return f"{item_count:,} item(s), {tokens:,} encoded tokens"
    return "completed"


def _summarize_graph_build(payload: dict[str, Any]) -> str:
    nested = payload.get("graph")
    graph: dict[str, Any] = nested if isinstance(nested, dict) else payload
    edges = _integer(graph, "edges")
    return f"{edges:,} edges" if edges is not None else "completed"


def _summarize_navigation(payload: dict[str, Any]) -> str:
    edges = payload.get("edges")
    count = len(edges) if isinstance(edges, list) else None
    return f"{count:,} edge(s) returned" if count is not None else "completed"


def _operations(corpus: ScaleCorpus) -> tuple[_Operation, ...]:
    last_record = f"record-{corpus.records - 1:06d}"
    since, until = corpus.window.since, corpus.window.until
    return (
        _Operation(
            "find-count",
            ("find", "benchmark", "--source", "scale", "--since", since, "--until", until, "--count"),
            _summarize_find,
        ),
        _Operation(
            "context",
            ("context", last_record, "--since", since, "--until", until, "--budget", "2048"),
            _summarize_context,
        ),
        _Operation("build-graph", ("build", "graph"), _summarize_graph_build),
        _Operation(
            "navigate",
            ("graph", corpus.first_record_uri, "--out", "--limit", "100"),
            _summarize_navigation,
        ),
    )


def _command_environment(corpus: ScaleCorpus) -> dict[str, str]:
    environment = {key: value for key, value in os.environ.items() if key not in {"FKF_BASE", "HOME", "XDG_STATE_HOME"}}
    environment.update(
        {
            "HOME": str(corpus.runtime / "home"),
            "XDG_STATE_HOME": str(corpus.runtime / "state"),
            "PYTHONHASHSEED": "0",
        }
    )
    return environment


def _run_operation(corpus: ScaleCorpus, operation: _Operation, timeout: float) -> _Observation:
    command = (
        sys.executable,
        "-m",
        "fkf",
        "--base",
        str(corpus.root),
        "--format",
        "json",
        *operation.arguments,
    )
    started = time.perf_counter()
    try:
        result = SubprocessRunner().run(
            Command(
                command,
                DurationNS(round(timeout * 1_000_000_000)),
                environment=_command_environment(corpus),
                base=corpus.root,
            )
        )
    except CommandError as error:
        raise BenchmarkError(f"{operation.name} failed: {error}") from error
    elapsed = time.perf_counter() - started
    try:
        payload = json.loads(result.stdout)
    except (json.JSONDecodeError, UnicodeDecodeError) as error:
        raise BenchmarkError(f"{operation.name} did not return one JSON result: {error}") from error
    if not isinstance(payload, dict):
        raise BenchmarkError(f"{operation.name} returned {type(payload).__name__}; expected a JSON object")
    return _Observation(operation.name, elapsed, operation.summarize(payload))


def _render_table(observations: Sequence[_Observation]) -> str:
    rows = [(item.operation, f"{item.elapsed_seconds:.3f}s", item.result) for item in observations]
    headers = ("operation", "elapsed", "result")
    widths = tuple(max(len(header), *(len(row[index]) for row in rows)) for index, header in enumerate(headers))
    header = "  ".join(value.ljust(widths[index]) for index, value in enumerate(headers))
    rule = "  ".join("-" * width for width in widths)
    body = ["  ".join(value.ljust(widths[index]) for index, value in enumerate(row)) for row in rows]
    return "\n".join((header, rule, *body))


def _positive_integer(value: str) -> int:
    try:
        parsed = int(value)
    except ValueError as error:
        raise argparse.ArgumentTypeError(f"expected an integer, got {value!r}") from error
    if parsed < 1:
        raise argparse.ArgumentTypeError("value must be positive")
    return parsed


def _positive_seconds(value: str) -> float:
    try:
        parsed = float(value)
    except ValueError as error:
        raise argparse.ArgumentTypeError(f"expected seconds, got {value!r}") from error
    if not 0 < parsed <= 3600:
        raise argparse.ArgumentTypeError("timeout must be in 0..3600 seconds")
    return parsed


def _parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        description="Observe four complete FKF CLI operations over one deterministic local corpus."
    )
    parser.add_argument("--records", type=_positive_integer, default=DEFAULT_RECORDS)
    parser.add_argument("--relations", type=_positive_integer, default=DEFAULT_RELATIONS_PER_RECORD)
    parser.add_argument("--timeout", type=_positive_seconds, default=DEFAULT_TIMEOUT_SECONDS)
    return parser


def main(arguments: Sequence[str] | None = None) -> int:
    options = _parser().parse_args(arguments)
    try:
        with tempfile.TemporaryDirectory(prefix="fkf-scale-") as temporary:
            setup_started = time.perf_counter()
            corpus = create_scale_corpus(
                Path(temporary),
                records=options.records,
                relations_per_record=options.relations,
            )
            setup_elapsed = time.perf_counter() - setup_started
            observations = tuple(
                _run_operation(corpus, operation, options.timeout) for operation in _operations(corpus)
            )
            report = "\n".join(
                (
                    "FKF scale observation (one complete run per operation; no pass/fail thresholds)",
                    f"environment: fkf {__version__}, Python {platform.python_version()}, {platform.platform()}",
                    (
                        f"corpus: {corpus.records:,} records, {corpus.edges:,} edges; "
                        f"setup {setup_elapsed:.3f}s; command timeout {options.timeout:g}s"
                    ),
                    "",
                    _render_table(observations),
                    "",
                )
            )
            sys.stdout.write(report)
    except (BenchmarkError, OSError, ValueError) as error:
        sys.stderr.write(f"benchmark failed: {error}\n")
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
