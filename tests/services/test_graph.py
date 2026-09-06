from __future__ import annotations

import hashlib
import json
import os
import time
from datetime import UTC, datetime
from io import BytesIO
from pathlib import Path
from threading import Event

import pytest

from fkf.base import Base
from fkf.config import load_config
from fkf.documents import Document
from fkf.errors import CanceledError
from fkf.fields import Cardinality, FieldDefinition, FieldMap, FieldSchema
from fkf.graph import (
    EDGE_FIELD_COUNT,
    EDGE_SCHEMA_VERSION,
    GRAPH_EXTRACTOR_VERSION,
    MAX_EDGE_LINE_BYTES,
    DerivedGraphMissingError,
    Direction,
    Edge,
    EdgeListMeta,
    EdgeQuery,
    EdgeValidationError,
    GraphFileManifest,
    GraphInputSHA256,
    GraphQuery,
    IdentityResolver,
    ValidatedGraphCache,
    build_graph,
    decode_edge,
    encode_edges,
    encode_graph_artifacts,
    extract_edges,
    graph_generation_sha256,
    graph_input_state,
    list_nodes,
    neighbours,
    new_edge_list_meta,
    new_graph_input_sha256,
    read_current_graph_generation,
    scan_edges,
    summarize_graph,
    verify_graph,
    write_edge_list,
)
from fkf.markdown import Page
from fkf.process import Cancellation
from fkf.store import Layer


def _sample_edges() -> list[Edge]:
    return [
        Edge(
            src="events/2026-08-20/gmail.json#a1b2",
            dst="actor:directory/marc",
            kind="participates",
            at="2026-08-20T14:02:00Z",
            via="field:participant",
            indexed="2026-08-21T06:00:00Z",
        ),
        Edge(
            src="events/2026-08-20/gmail.json#a1b2",
            dst="https://example.invalid/browse/FK-412?a=1&b=2",
            kind="mentions",
            at="2026-08-20T14:02:00Z",
            via="field:ticket",
            indexed="2026-08-21T06:00:00Z",
        ),
    ]


def test_encode_edges_is_canonical_deterministic_and_deduplicated() -> None:
    edges = _sample_edges()
    encoded = encode_edges(edges)

    assert encode_edges([edges[1], edges[0], edges[0]]) == encoded
    assert encoded.count(b"\n") == 2
    assert encoded.startswith(b"events/2026-08-20/gmail.json#a1b2\t")
    assert b"?a=1&b=2" in encoded
    assert encoded.splitlines()[0].count(b"\t") == EDGE_FIELD_COUNT - 1


@pytest.mark.parametrize(
    "edge",
    [
        Edge(src="a", dst="b\tc", kind="mentions", via="test"),
        Edge(src="a\nb", dst="c", kind="mentions", via="test"),
        Edge(src="a", dst="b", kind="mentions", via="te\rst"),
        Edge(src="a", dst="b", kind="bad\x1bkind", via="test"),
        Edge(src="a", dst="b", kind="mentions", at="yesterday", via="test"),
        Edge(src="", dst="b", kind="mentions", via="test"),
    ],
)
def test_encode_edges_rejects_unreadable_rows_before_emitting(edge: Edge) -> None:
    with pytest.raises(EdgeValidationError):
        encode_edges([Edge(src="good", dst="row", kind="link", via="test"), edge])


def test_scan_edges_prefilters_and_reports_only_relevant_malformed_rows() -> None:
    encoded = b"a\tb\tmentions\t\ttest\t\na\tnot-enough-columns\na\tc\tmentions\t\ttest\t\n"

    narrow, narrow_stats = scan_edges(BytesIO(encoded), EdgeQuery(kind="mentions"))
    all_rows, audit_stats = scan_edges(BytesIO(encoded), EdgeQuery())

    assert [edge.dst for edge in narrow] == ["b", "c"]
    assert (narrow_stats.matched, narrow_stats.malformed) == (2, 0)
    assert [edge.dst for edge in all_rows] == ["b", "c"]
    assert (audit_stats.lines, audit_stats.matched, audit_stats.malformed) == (3, 2, 1)


def test_decode_edge_requires_exact_valid_columns() -> None:
    assert decode_edge(b"a\tb\tk\t\tv\t") == Edge(src="a", dst="b", kind="k", via="v")
    assert decode_edge(b"a\tb\tk\tv") is None


def test_edge_row_bound_matches_streaming_scanner() -> None:
    edge = Edge(src="a", dst="d" * (MAX_EDGE_LINE_BYTES - 9), kind="k", via="v")
    encoded = encode_edges([edge])

    assert len(encoded) == MAX_EDGE_LINE_BYTES
    assert scan_edges(BytesIO(encoded), EdgeQuery())[1].matched == 1

    with pytest.raises(EdgeValidationError, match="line exceeds"):
        encode_edges([Edge(src="a", dst=edge.dst + "d", kind="k", via="v")])

    final_without_newline = encoded.removesuffix(b"\n")
    assert scan_edges(BytesIO(final_without_newline), EdgeQuery())[1].matched == 1


def _sample_inputs() -> GraphInputSHA256:
    return new_graph_input_sha256(*(character * 64 for character in "abcdef"))


def test_graph_artifacts_are_source_and_destination_sorted_with_exact_offsets() -> None:
    edges = [
        Edge(src="c", dst="b", kind="k", via="v"),
        Edge(src="a", dst="z", kind="k", via="v"),
        Edge(src="a", dst="b", kind="k", via="v"),
    ]

    artifacts = encode_graph_artifacts(edges)

    assert [(edge.src, edge.dst) for edge in scan_edges(BytesIO(artifacts.src), EdgeQuery())[0]] == [
        ("a", "b"),
        ("a", "z"),
        ("c", "b"),
    ]
    assert [(edge.src, edge.dst) for edge in scan_edges(BytesIO(artifacts.dst), EdgeQuery())[0]] == [
        ("a", "b"),
        ("c", "b"),
        ("a", "z"),
    ]
    offset_lines = artifacts.offsets.decode().splitlines()
    assert [line.split("\t")[:2] for line in offset_lines] == [
        ["dst", "b"],
        ["dst", "z"],
        ["src", "a"],
        ["src", "c"],
    ]
    for line in offset_lines:
        direction, node, start, length = line.split("\t")
        data = artifacts.src if direction == "src" else artifacts.dst
        rows = scan_edges(BytesIO(data[int(start) : int(start) + int(length)]), EdgeQuery())[0]
        assert rows
        assert all((edge.src if direction == "src" else edge.dst) == node for edge in rows)


def test_metadata_binds_exact_artifacts_and_closed_input_vocabulary() -> None:
    generated = datetime(2026, 8, 21, 6, 0, 0, 987654, tzinfo=UTC)
    edges = [
        Edge(src="a", dst="b", kind="link", via="markdown-inline"),
        Edge(src="c", dst="b", kind="tag", via="frontmatter:tags"),
    ]

    inputs = _sample_inputs()
    meta = new_edge_list_meta(edges, generated, inputs)
    artifacts = encode_graph_artifacts(edges)

    assert (meta.schema_version, meta.extractor_version) == (EDGE_SCHEMA_VERSION, GRAPH_EXTRACTOR_VERSION)
    assert meta.generated_at == "2026-08-21T06:00:00Z"
    assert meta.edges == 2
    assert meta.bytes == len(artifacts.src)
    assert meta.extractors == ("frontmatter:tags", "markdown-inline")
    assert meta.kinds == ("link", "tag")
    assert tuple(item.uri for item in meta.outputs) == ("graph.dst.tsv", "graph.offsets.tsv", "graph.tsv")
    for item in meta.outputs:
        data = {"graph.tsv": artifacts.src, "graph.dst.tsv": artifacts.dst, "graph.offsets.tsv": artifacts.offsets}[
            item.uri
        ]
        assert item.bytes == len(data)
        assert item.sha256 == hashlib.sha256(data).hexdigest()
    assert inputs.aggregate == new_graph_input_sha256(*(character * 64 for character in "abcdef")).aggregate
    assert len(inputs.aggregate) == 64
    assert len(graph_generation_sha256(meta)) == 64


def test_write_edge_list_publishes_exact_owner_only_generation(tmp_path: Path) -> None:
    generated = datetime(2026, 8, 21, 6, 0, tzinfo=UTC)
    indexed = "2026-08-21T06:00:00Z"
    edges = [Edge(src="a", dst="b", kind="link", via="test", indexed=indexed)]
    rows = tmp_path / "graph.tsv"
    meta_path = tmp_path / "graph.meta.json"
    meta = new_edge_list_meta(edges, generated, _sample_inputs())

    write_edge_list(rows, meta_path, edges, meta)

    assert rows.read_bytes() == encode_edges(edges)
    assert (tmp_path / "graph.dst.tsv").is_file()
    assert (tmp_path / "graph.offsets.tsv").is_file()
    assert rows.stat().st_mode & 0o777 == 0o600
    assert rows.stat().st_mtime_ns == int(generated.timestamp()) * 1_000_000_000
    state = json.loads((tmp_path / "graph.generation.json").read_bytes())
    assert state == {"state": "current", "generation": graph_generation_sha256(meta)}
    assert read_current_graph_generation(tmp_path) == state["generation"]


def test_write_edge_list_rejects_metadata_that_does_not_describe_rows(tmp_path: Path) -> None:
    generated = datetime(2026, 8, 21, 6, 0, tzinfo=UTC)
    edge = Edge(src="a", dst="b", kind="link", via="test", indexed="2026-08-21T06:00:00Z")
    meta = new_edge_list_meta([edge], generated, _sample_inputs())
    lying = EdgeListMeta(
        schema_version=meta.schema_version,
        extractor_version=meta.extractor_version,
        columns=meta.columns,
        separator=meta.separator,
        generated_at=meta.generated_at,
        edges=2,
        extractors=meta.extractors,
        bytes=meta.bytes,
        kinds=meta.kinds,
        inputs=meta.inputs,
        outputs=meta.outputs,
        sha256=meta.sha256,
    )

    with pytest.raises(EdgeValidationError, match="metadata does not describe"):
        write_edge_list(tmp_path / "graph.tsv", tmp_path / "graph.meta.json", [edge], lying)

    assert not (tmp_path / "graph.tsv").exists()


def test_generation_reader_fails_closed_on_unknown_or_building_state(tmp_path: Path) -> None:
    generation = "a" * 64
    path = tmp_path / "graph.generation.json"
    path.write_text(json.dumps({"state": "building", "generation": generation}), encoding="utf-8")
    with pytest.raises(EdgeValidationError, match="not current"):
        read_current_graph_generation(tmp_path)

    path.write_text(json.dumps({"state": "current", "generation": generation, "extra": True}), encoding="utf-8")
    with pytest.raises(EdgeValidationError, match="unknown"):
        read_current_graph_generation(tmp_path)

    path.write_text("{} {}", encoding="utf-8")
    with pytest.raises(EdgeValidationError, match="JSON"):
        read_current_graph_generation(tmp_path)


def test_manifest_requires_sorted_exact_sha256_values() -> None:
    with pytest.raises(EdgeValidationError, match="events input digest"):
        new_graph_input_sha256("A" * 64, *(character * 64 for character in "bcdef"))
    with pytest.raises(EdgeValidationError, match="URI-sorted"):
        new_edge_list_meta(
            [],
            datetime(2026, 8, 21, tzinfo=UTC),
            _sample_inputs(),
            GraphFileManifest(uri="wiki/z.md", bytes=0, modified_unix_nano=0, sha256="a" * 64),
            GraphFileManifest(uri="wiki/a.md", bytes=0, modified_unix_nano=0, sha256="b" * 64),
        )


GRAPH_CONFIG = """\
fkf: 1
name: brain
schema:
  id: {description: Stable identity., cardinality: one}
  time: {description: Fact time., cardinality: optional}
  title: {description: Meaningful title., cardinality: optional}
  participant: {description: Participant., cardinality: many, relation: true}
  related: {description: Related page., cardinality: many, relation: true}
layers: {events: true, index: true, tasks: true, projects: true, wiki: true}
identities:
  owner:
    canonical: person:fmind
    aliases: [actor:github.com/fmind, fmind@example.test]
    kind: person
    owner: true
sources: {}
"""


def _graph_base(tmp_path: Path) -> Base:
    root = tmp_path / "brain"
    root.mkdir()
    (root / "fkf.yaml").write_text(GRAPH_CONFIG, encoding="utf-8")
    config = load_config(root)
    return Base(config=config, store=config.store(), now=lambda: datetime(2026, 8, 21, 6, 0, tzinfo=UTC))


def _write_graph_document(base: Base) -> None:
    fields = FieldMap.from_json_value({"id": ".id", "time": ".time", "participant": ".people[]"})
    schema = FieldSchema(
        {
            "id": FieldDefinition("Stable identity.", Cardinality.ONE),
            "time": FieldDefinition("Fact time.", Cardinality.OPTIONAL),
            "participant": FieldDefinition("Participant.", Cardinality.MANY, relation=True),
        }
    )
    base.write_document(
        Document(
            source="mail",
            layer=Layer.EVENTS,
            date="2026-08-20",
            window_start="2026-08-20T00:00:00Z",
            window_end="2026-08-21T00:00:00Z",
            collected_at="2026-08-21T05:00:00Z",
            schema=schema,
            fields=fields,
            count=1,
            records=[{"id": "m1", "time": "2026-08-20T14:02:00+00:00", "people": ["actor:github.com/fmind"]}],
        )
    )


def test_extract_edges_transcribes_documents_pages_and_exact_identities(tmp_path: Path) -> None:
    base = _graph_base(tmp_path)
    _write_graph_document(base)
    wiki = base.root / "wiki"
    wiki.mkdir()
    (wiki / "owner.md").write_text(
        "---\ntype: person\ntitle: Mederic\naliases: [fmind@example.test]\ntags: [ai]\n"
        "relations:\n  related: [../projects/fkf.md]\n---\n\n# Mederic\n\n[FKF](../projects/fkf.md)\n",
        encoding="utf-8",
    )
    projects = base.root / "projects"
    projects.mkdir()
    (projects / "fkf.md").write_text("# FKF\n", encoding="utf-8")

    edges, counts = extract_edges(base)
    facts = {(edge.src, edge.dst, edge.kind, edge.via) for edge in edges}

    assert counts.documents == 1
    assert counts.pages == 2
    assert (
        "events/2026-08-20/mail.json#m1",
        "person:fmind",
        "participant",
        "field:participant",
    ) in facts
    assert ("person:fmind", "tag:ai", "tag", "frontmatter:tags") in facts
    assert ("person:fmind", "projects/fkf.md", "link", "markdown-inline") in facts
    assert ("person:fmind", "projects/fkf.md", "related", "frontmatter:relations.related") in facts
    assert ("actor:github.com/fmind", "person:fmind", "same-as", "identities.owner.aliases") in facts


def test_extract_edges_forwards_cancellation_through_nested_page_reads(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    from fkf import graph

    base = _graph_base(tmp_path)
    wiki = base.root / "wiki"
    wiki.mkdir()
    (wiki / "a.md").write_text("# A\n\n[Target](b.md#target)\n", encoding="utf-8")
    (wiki / "b.md").write_text("# Target\n", encoding="utf-8")
    task = base.root / "tasks" / "2026-08-20" / "example"
    task.mkdir(parents=True)
    (task / "TASKS.md").write_text("# Task\n", encoding="utf-8")
    cancel = Event()
    loaded: list[object] = []
    read_uris: list[str] = []
    original_load = graph.load_markdown_layer
    original_read = graph.read_page

    def observe_load(
        candidate: Base,
        layer: Layer,
        *,
        cancel: Cancellation | None = None,
    ) -> tuple[tuple[Page, ...], tuple[str, ...]]:
        assert cancel is cancel_event
        loaded.append(cancel)
        return original_load(candidate, layer, cancel=cancel_event)

    def cancel_fragment_read(candidate: Base, uri: str, *, cancel: Cancellation | None = None) -> Page:
        assert cancel is cancel_event
        read_uris.append(uri)
        if uri == "wiki/b.md":
            cancel_event.set()
            raise CanceledError("operation canceled")
        return original_read(candidate, uri, cancel=cancel_event)

    cancel_event = cancel
    monkeypatch.setattr(graph, "load_markdown_layer", observe_load)
    monkeypatch.setattr(graph, "read_page", cancel_fragment_read)

    with pytest.raises(CanceledError, match="operation canceled"):
        extract_edges(base, cancel=cancel)

    assert loaded
    assert all(item is cancel for item in loaded)
    assert "tasks/2026-08-20/example/TASKS.md" in read_uris
    assert read_uris[-1] == "wiki/b.md"


def test_graph_input_state_and_build_bind_every_exact_input(tmp_path: Path) -> None:
    base = _graph_base(tmp_path)
    _write_graph_document(base)
    (base.root / "wiki").mkdir()
    (base.root / "wiki" / "note.md").write_text("# Note\n\n[tag](tag:graph)\n", encoding="utf-8")

    before = graph_input_state(base)
    result = build_graph(base)
    after = graph_input_state(base)

    assert before == after
    assert [item.uri for item in before.files] == [
        "events/2026-08-20/mail.json",
        "wiki/note.md",
    ]
    assert result.uri == "graph.tsv"
    assert result.documents == 1
    assert result.pages == 1
    assert result.edges == 3
    assert result.meta.inputs == before.files
    assert all(
        edge.indexed == "2026-08-21T06:00:00Z"
        for edge in scan_edges(BytesIO((base.root / "graph.tsv").read_bytes()), EdgeQuery())[0]
    )


def test_graph_build_cancellation_before_publish_leaves_no_generation(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    from fkf import graph

    base = _graph_base(tmp_path)
    cancel = Event()

    def cancel_after_extract(_base: Base, *, cancel: object) -> tuple[list[Edge], graph.ExtractCounts]:
        assert cancel is cancel_event
        cancel_event.set()
        return [], graph.ExtractCounts()

    cancel_event = cancel
    monkeypatch.setattr(graph, "extract_edges", cancel_after_extract)

    with pytest.raises(CanceledError):
        build_graph(base, cancel=cancel)

    assert not (base.root / "graph.tsv").exists()
    assert not (base.root / graph.GRAPH_GENERATION_FILE).exists()


def test_identity_resolver_joins_page_aliases_without_using_names_as_graph_edges(tmp_path: Path) -> None:
    base = _graph_base(tmp_path)
    (base.root / "wiki").mkdir()
    (base.root / "wiki" / "owner.md").write_text(
        "---\ntype: person\ntitle: Mederic Hurier\naliases: [fmind@example.test]\n---\n# Mederic\n",
        encoding="utf-8",
    )

    resolver = IdentityResolver.load(base)

    assert resolver.canonical("Mederic Hurier") == "person:fmind"
    assert resolver.canonical("wiki/owner.md") == "person:fmind"
    assert not any(alias.alias == "Mederic Hurier" for alias in resolver.graph_aliases())


def test_neighbours_seek_exact_ranges_and_walk_breadth_first(tmp_path: Path) -> None:
    base = _graph_base(tmp_path)
    (base.root / "wiki").mkdir()
    (base.root / "projects").mkdir()
    (base.root / "wiki" / "a.md").write_text("# A\n\n[B](b.md)\n", encoding="utf-8")
    (base.root / "wiki" / "b.md").write_text("# B\n\n[C](../projects/c.md)\n", encoding="utf-8")
    (base.root / "projects" / "c.md").write_text("# C\n", encoding="utf-8")
    build_graph(base)

    outgoing = neighbours(base, GraphQuery(uri="wiki/a.md", direction=Direction.OUT, depth=2))
    incoming = neighbours(base, GraphQuery(uri="projects/c.md", direction=Direction.IN))

    assert [(edge.src, edge.dst, edge.hop) for edge in outgoing.edges] == [
        ("wiki/a.md", "wiki/b.md", 1),
        ("wiki/b.md", "projects/c.md", 2),
    ]
    assert outgoing.nodes == ("projects/c.md", "wiki/b.md")
    assert outgoing.stats.lines == 2
    assert [(edge.src, edge.dst) for edge in incoming.edges] == [("wiki/b.md", "projects/c.md")]
    assert incoming.stats.lines == 1


def test_graph_query_rejects_unknown_kind_and_reports_truncation(tmp_path: Path) -> None:
    base = _graph_base(tmp_path)
    (base.root / "wiki").mkdir()
    (base.root / "wiki" / "a.md").write_text("# A\n\n[B](b.md) [C](c.md)\n", encoding="utf-8")
    (base.root / "wiki" / "b.md").write_text("# B\n", encoding="utf-8")
    (base.root / "wiki" / "c.md").write_text("# C\n", encoding="utf-8")
    build_graph(base)

    with pytest.raises(ValueError, match=r"unknown edge kind.*typo.*link"):
        neighbours(base, GraphQuery(uri="wiki/a.md", kind="typo"))
    result = neighbours(base, GraphQuery(uri="wiki/a.md", direction=Direction.OUT, limit=1))
    assert result.truncated
    assert len(result.edges) == 1


def test_summary_nodes_and_full_verify_use_one_integrity_bound_generation(tmp_path: Path) -> None:
    base = _graph_base(tmp_path)
    (base.root / "wiki").mkdir()
    (base.root / "wiki" / "a.md").write_text("# A\n\n[B](b.md) [tag](tag:graph)\n", encoding="utf-8")
    (base.root / "wiki" / "b.md").write_text("# B\n", encoding="utf-8")
    build_graph(base)

    summary = summarize_graph(base)
    listing = list_nodes(base)
    verified = verify_graph(base)

    assert summary == verified
    assert summary.edges == 3
    assert summary.nodes == 5
    assert [(item.kind, item.count) for item in summary.edge_kinds] == [("link", 2), ("same-as", 1)]
    assert listing.total == 5
    assert listing.nodes[0].total >= listing.nodes[-1].total


def test_graph_readers_honor_cancellation_before_scanning(tmp_path: Path) -> None:
    base = _graph_base(tmp_path)
    (base.root / "wiki").mkdir()
    (base.root / "wiki" / "a.md").write_text("# A\n\n[B](b.md)\n", encoding="utf-8")
    (base.root / "wiki" / "b.md").write_text("# B\n", encoding="utf-8")
    build_graph(base)
    cancel = Event()
    cancel.set()

    with pytest.raises(CanceledError):
        neighbours(base, GraphQuery(uri="wiki/a.md"), cancel=cancel)
    with pytest.raises(CanceledError):
        list_nodes(base, cancel=cancel)


def test_graph_validation_rejects_changed_artifact_and_unknown_metadata(tmp_path: Path) -> None:
    base = _graph_base(tmp_path)
    (base.root / "wiki").mkdir()
    (base.root / "wiki" / "a.md").write_text("# A\n\n[tag](tag:graph)\n", encoding="utf-8")
    build_graph(base)

    graph = base.root / "graph.tsv"
    graph.write_bytes(graph.read_bytes() + b"bad\trow\n")
    with pytest.raises(EdgeValidationError, match="invalid derived graph cache"):
        summarize_graph(base)

    build_graph(base)
    meta = json.loads((base.root / "graph.meta.json").read_bytes())
    meta["unknown"] = True
    (base.root / "graph.meta.json").write_text(json.dumps(meta), encoding="utf-8")
    with pytest.raises(EdgeValidationError, match="unknown field"):
        neighbours(base, GraphQuery(uri="wiki/a.md"))


def test_missing_primary_graph_has_a_distinct_fresh_clone_error(tmp_path: Path) -> None:
    base = _graph_base(tmp_path)
    with pytest.raises(DerivedGraphMissingError):
        summarize_graph(base)

    (base.root / "graph.tsv").mkdir()
    with pytest.raises(EdgeValidationError, match="regular"):
        summarize_graph(base)


@pytest.mark.skipif(os.environ.get("FKF_RUN_GRAPH_BENCHMARKS") != "1", reason="opt-in graph observation")
@pytest.mark.parametrize("edge_count", [100_000, 500_000])
def test_graph_seek_benchmark_observation(tmp_path: Path, edge_count: int) -> None:
    generated = datetime(2026, 8, 21, 6, 0, tzinfo=UTC)
    edges = [
        Edge(
            src=f"actor:example/{index:06d}",
            dst=f"repo:example/{index % 1000:04d}",
            kind="owns",
            via="benchmark",
            indexed="2026-08-21T06:00:00Z",
        )
        for index in range(edge_count)
    ]
    meta = new_edge_list_meta(edges, generated, _sample_inputs())
    started = time.perf_counter()
    write_edge_list(tmp_path / "graph.tsv", tmp_path / "graph.meta.json", edges, meta)
    built = time.perf_counter() - started
    with (
        (tmp_path / "graph.tsv").open("rb") as src,
        (tmp_path / "graph.dst.tsv").open("rb") as dst,
        (tmp_path / "graph.offsets.tsv").open("rb") as offsets,
    ):
        cache = ValidatedGraphCache(src, dst, offsets, meta)
        started = time.perf_counter()
        rows, stats = cache.scan(EdgeQuery(src=f"actor:example/{edge_count - 1:06d}"))
        queried = time.perf_counter() - started
    assert len(rows) == stats.matched == 1
    print(f"graph edges={edge_count} build={built:.3f}s seek={queried:.6f}s")
