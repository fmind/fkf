from __future__ import annotations

import hashlib
from datetime import UTC, datetime
from pathlib import Path
from threading import Event

import pytest

from fkf import trust_service
from fkf.base import Base
from fkf.config import Config, Source, SyncConfig
from fkf.errors import CanceledError
from fkf.fields import Cardinality, FieldDefinition, FieldMap, FieldPath, FieldPaths, FieldSchema
from fkf.jsoncodec import dumps
from fkf.output import text_bytes
from fkf.store import Layer, Store
from fkf.trust import TrustState, read_trust
from fkf.trust_service import trust


def _base(tmp_path: Path) -> Base:
    config_path = tmp_path / "fkf.yaml"
    config_path.write_text("name: trust\n")
    fields = FieldMap({"id": FieldPaths((FieldPath.parse(".id"),))})
    schema = FieldSchema({"id": FieldDefinition("Identity", Cardinality.ONE)})
    source = Source(
        "provider",
        enabled=True,
        run=("provider", "list"),
        body=("provider", "get", "{{id}}"),
        fields=fields,
        schema=schema,
    )
    layers = dict.fromkeys(Layer, True)
    config = Config(1, "trust", schema, layers, {}, {source.name: source}, SyncConfig(), (), config_path)
    return Base(config, Store(tmp_path, layers), now=lambda: datetime(2026, 9, 6, tzinfo=UTC))


def test_trust_check_discloses_commands_and_body_fields(monkeypatch, tmp_path: Path) -> None:
    base = _base(tmp_path)
    state = TrustState(str(tmp_path), False, "digest")
    monkeypatch.setattr("fkf.trust_service.read_trust", lambda _config, **_kwargs: state)
    report = trust(base, record=False)
    assert not report.recorded
    assert report.state is state
    assert report.commands[0].run == ("provider", "list")
    assert report.commands[0].body_fields == {"id": ".id"}
    assert report.policy.working_directory == "/"
    encoded = dumps(report)
    assert encoded.index(b'"commands"') < encoded.index(b'"state"')
    assert encoded.index(b'"state"') < encoded.index(b'"recorded"')
    rendered, native = text_bytes(report)
    assert native
    assert rendered.startswith(
        b"collection policy\n"
        b"  layer: events=true\n"
        b"  layer: index=true\n"
        b"  layer: tasks=true\n"
        b"  layer: projects=true\n"
        b"  layer: wiki=true\n"
    )
    assert b'  run:  ["provider", "list"]\n' in rendered
    assert b"  body field id: .id\n" in rendered


def test_trust_record_uses_atomic_trust_writer(monkeypatch, tmp_path: Path) -> None:
    base = _base(tmp_path)
    state = TrustState(str(tmp_path), True, "digest", stored_digest="digest")
    observed: list[datetime] = []

    def write(_config: Config, now: datetime, *, cancel=None, snapshot=None) -> TrustState:
        del cancel, snapshot
        observed.append(now)
        return state

    monkeypatch.setattr("fkf.trust_service.write_trust", write)
    report = trust(base, record=True, all_items=True)
    assert report.recorded
    assert report.all
    assert observed == [datetime(2026, 9, 6, tzinfo=UTC)]


def test_trust_records_the_exact_execution_tree_it_disclosed(monkeypatch, tmp_path: Path) -> None:
    root = tmp_path / "base"
    root.mkdir()
    base = _base(root)
    helper = root / "bin" / "provider"
    helper.parent.mkdir()
    reviewed = b"#!/bin/sh\nprintf reviewed\n"
    changed = b"#!/bin/sh\nprintf changed-after-review\n"
    helper.write_bytes(reviewed)
    helper.chmod(0o700)
    capture = trust_service.capture_trust

    def change_after_review(config: Config, *, cancel=None):
        snapshot = capture(config, cancel=cancel)
        helper.write_bytes(changed)
        return snapshot

    monkeypatch.setattr(trust_service, "capture_trust", change_after_review)

    report = trust(base, record=True, all_items=True)

    assert report.scripts[0].digest == hashlib.sha256(reviewed).hexdigest()
    assert not read_trust(base.config).trusted
    helper.write_bytes(reviewed)
    assert read_trust(base.config).trusted


def test_trust_honors_pre_cancellation_before_inventory_or_write(tmp_path: Path) -> None:
    base = _base(tmp_path)
    canceled = Event()
    canceled.set()

    with pytest.raises(CanceledError, match="operation canceled"):
        trust(base, record=True, cancel=canceled)
