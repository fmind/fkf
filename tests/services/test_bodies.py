from __future__ import annotations

from datetime import UTC, datetime
from pathlib import Path
from threading import Event

import pytest

from fkf.base import Base
from fkf.bodies import (
    BodyCacheError,
    BodyManifest,
    BodyManifestEntry,
    body_cache_relative,
    cache_body,
    decode_body_manifest,
    load_body_manifest,
    prune_bodies,
    read_cached_body,
    read_or_fetch_body,
    write_body_manifest,
)
from fkf.config import load_config
from fkf.documents import Document, Record, fields_of, schema_of
from fkf.errors import CanceledError
from fkf.process import Cancellation, Command, CommandResult
from fkf.source_runtime import Environment
from fkf.store import Layer

CONFIG = """\
fkf: 1
name: brain
schema:
  id: {description: Stable identity., cardinality: one}
  title: {description: Meaningful title., cardinality: optional}
  modified: {description: Provider modification time., cardinality: optional}
layers: {events: false, index: true, tasks: false, projects: false, wiki: false}
sources:
  snapshot:
    enabled: true
    layer: index
    run: [provider]
    fields: {id: .id, title: .title, modified: .modified}
    body: [provider, body, "{{id}}"]
    bodies: cache
"""


def make_base(tmp_path: Path) -> tuple[Base, Document, Record]:
    root = tmp_path / "brain"
    root.mkdir()
    (root / "fkf.yaml").write_text(CONFIG)
    config = load_config(root)
    source = config.sources["snapshot"]
    record: Record = {"id": "alpha", "title": "Alpha", "modified": "2026-09-05T10:00:00Z"}
    document = Document(
        source="snapshot",
        layer=Layer.INDEX,
        collected_at="2026-09-06T10:00:00Z",
        schema=schema_of(source),
        fields=fields_of(source),
        body=True,
        count=1,
        records=[record],
    )
    base = Base(config=config, store=config.store(), now=lambda: datetime(2026, 9, 6, 12, tzinfo=UTC))
    return base, document, record


def test_cache_body_publishes_verified_manifest_after_text(tmp_path: Path) -> None:
    base, document, record = make_base(tmp_path)
    uri = document.record_uri(record)
    assert uri is not None
    entry = cache_body(base, document, record, uri, "Evidence café")
    assert entry.path == body_cache_relative("snapshot", uri)
    assert entry.provider_modified_at == "2026-09-05T10:00:00Z"
    assert read_cached_body(base, uri) == ("Evidence café", entry, True)
    assert load_body_manifest(base).entries[uri] == entry


def test_cache_body_detects_tampering_and_missing_file_is_a_miss(tmp_path: Path) -> None:
    base, document, record = make_base(tmp_path)
    uri = document.record_uri(record)
    assert uri is not None
    entry = cache_body(base, document, record, uri, "evidence")
    path = base.root / entry.path
    path.write_text("tampered")
    with pytest.raises(BodyCacheError, match="does not match"):
        read_cached_body(base, uri)
    path.unlink()
    assert read_cached_body(base, uri) == ("", None, False)


def test_manifest_is_strict_bounded_and_canonical(tmp_path: Path) -> None:
    base, _document, _record = make_base(tmp_path)
    path = base.root / "bodies" / "manifest.json"
    path.parent.mkdir()
    path.write_text('{"schema_version":1,"entries":{},"surprise":true}')
    with pytest.raises(BodyCacheError, match="unknown field"):
        load_body_manifest(base)

    uri = "index/snapshot.json#alpha"
    bad = BodyManifestEntry(
        uri=uri,
        source="snapshot",
        path="bodies/snapshot/not-canonical.txt",
        sha256="0" * 64,
        bytes=0,
        fetched_at="2026-09-06T12:00:00Z",
    )
    write_body_manifest(base, BodyManifest(entries={uri: bad}))
    with pytest.raises(BodyCacheError, match="canonical cache path"):
        load_body_manifest(base)


@pytest.mark.parametrize("payload", [b'{"schema_version":1}', b'{"schema_version":1,"entries":null}'])
def test_empty_manifest_normalizes_missing_or_null_entries(tmp_path: Path, payload: bytes) -> None:
    base, _document, _record = make_base(tmp_path)
    assert decode_body_manifest(base, payload).entries == {}


def test_prune_removes_manifest_first_and_reports_reclaim(tmp_path: Path) -> None:
    base, document, record = make_base(tmp_path)
    uri = document.record_uri(record)
    assert uri is not None
    cache_body(base, document, record, uri, "evidence")
    report = prune_bodies(base)
    assert report.pruned == 1
    assert report.bytes == len("evidence")
    assert not (base.root / "bodies").exists()
    assert load_body_manifest(base).entries == {}


def test_prune_cancellation_before_mutation_preserves_manifest_and_body(tmp_path: Path) -> None:
    base, document, record = make_base(tmp_path)
    uri = document.record_uri(record)
    assert uri is not None
    entry = cache_body(base, document, record, uri, "evidence")
    cancel = Event()
    cancel.set()

    with pytest.raises(CanceledError):
        prune_bodies(base, cancel=cancel)

    assert load_body_manifest(base).entries[uri] == entry
    assert (base.root / entry.path).read_text(encoding="utf-8") == "evidence"


def test_body_fetch_forwards_cancellation_and_never_caches_a_canceled_result(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    from fkf import bodies

    base, document, record = make_base(tmp_path)
    base.environment = Environment.from_config(base.config, inherited_path="/usr/bin")
    uri = document.record_uri(record)
    assert uri is not None
    cancel = Event()
    trusted: list[object] = []

    def require_trust(_config: object, *, cancel: object) -> None:
        assert cancel is cancel_event
        trusted.append(cancel)

    class Runner:
        def run(self, command: Command, *, cancel: Cancellation | None = None) -> CommandResult:
            del command
            assert cancel is cancel_event
            cancel_event.set()
            return CommandResult(b"provider body")

    cancel_event = cancel
    base.runner = Runner()
    monkeypatch.setattr(bodies, "require_trust", require_trust)

    with pytest.raises(CanceledError) as caught:
        read_or_fetch_body(base, document, record, uri, cancel=cancel)

    assert caught.value.exit_code == 130
    assert trusted == [cancel]
    assert not (base.root / "bodies").exists()
