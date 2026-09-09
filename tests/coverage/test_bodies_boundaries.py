from __future__ import annotations

import hashlib
import json
import os
from dataclasses import dataclass, replace
from datetime import UTC, datetime, timedelta
from pathlib import Path

import pytest

import fkf.bodies as bodies
from fkf.base import Base
from fkf.bodies import (
    BodyCacheError,
    BodyManifest,
    BodyManifestEntry,
    body_cache_relative,
    cache_body,
    decode_body_manifest,
    encode_body_manifest,
    fetch_body,
    load_body_manifest,
    prune_bodies,
    read_cached_body,
    read_or_fetch_body,
    write_body_manifest,
)
from fkf.config import ConfigError, load_config
from fkf.documents import Document, Record, fields_of, schema_of
from fkf.io import FileTooLargeError
from fkf.process import Command, CommandResult
from fkf.source_runtime import Environment
from fkf.store import MAX_NARRATIVE_BYTES, Layer, UnsafePathError

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


@dataclass
class FakeRunner:
    stdout: bytes
    calls: int = 0

    def run(self, command: Command, *, cancel: object | None = None) -> CommandResult:
        del command, cancel
        self.calls += 1
        return CommandResult(self.stdout)


def make_base(
    tmp_path: Path,
    *,
    config_text: str = CONFIG,
    stdout: bytes = b"provider body",
    now: datetime = datetime(2026, 9, 6, 12, tzinfo=UTC),
) -> tuple[Base, Document, Record, FakeRunner]:
    root = tmp_path / "brain"
    root.mkdir(parents=True)
    (root / "fkf.yaml").write_text(config_text, encoding="utf-8")
    config = load_config(root)
    source = config.sources["snapshot"]
    record: Record = {"id": "alpha", "title": "Alpha", "modified": "2026-09-05T10:00:00Z"}
    document = Document(
        source="snapshot",
        layer=Layer.INDEX,
        collected_at="2026-09-06T10:00:00Z",
        schema=schema_of(source),
        fields=fields_of(source),
        body=source.has_body(),
        count=1,
        records=[record],
    )
    runner = FakeRunner(stdout)
    base = Base(
        config=config,
        store=config.store(),
        environment=Environment.from_config(config, inherited_path="/usr/bin"),
        runner=runner,
        now=lambda: now,
    )
    return base, document, record, runner


def manifest_entry(key: str, **changes: object) -> dict[str, object]:
    source = str(changes.pop("source", "snapshot"))
    entry: dict[str, object] = {
        "uri": key,
        "source": source,
        "path": body_cache_relative(source, key),
        "sha256": "0" * 64,
        "bytes": 0,
        "provider_modified_at": None,
        "fetched_at": "2026-09-06T12:00:00Z",
    }
    entry.update(changes)
    return entry


@pytest.mark.parametrize(
    ("payload", "message"),
    [
        (b"not-json", "decode bodies/manifest.json"),
        (b"[]", "must be a JSON object"),
        (b'{"schema_version":true}', "non-negative integer"),
        (b'{"schema_version":2}', "expected 1"),
        (b'{"schema_version":1,"entries":[]}', "entries must be a JSON object"),
        (b'{"schema_version":1,"event_attempts":[]}', "event_attempts must be a JSON object"),
        (b'{"schema_version":1,"event_attempts":{"Bad Name":true}}', "event_attempts entry"),
        (b'{"schema_version":1,"event_attempts":{"snapshot":"yes"}}', "must be a boolean"),
    ],
)
def test_manifest_envelope_rejects_ambiguous_or_mistyped_state(tmp_path: Path, payload: bytes, message: str) -> None:
    base, _document, _record, _runner = make_base(tmp_path)

    with pytest.raises(BodyCacheError, match=message):
        decode_body_manifest(base, payload)


@pytest.mark.parametrize(
    ("changes", "message"),
    [
        ({"surprise": True}, "unknown field"),
        ({"uri": 7}, "must be a string"),
        ({"source": "Bad Name"}, "source:"),
        ({"sha256": "f" * 63}, "invalid digest or size"),
        ({"bytes": -1}, "non-negative integer"),
        ({"fetched_at": "yesterday"}, "fetched_at"),
    ],
)
def test_manifest_entries_fail_closed_before_cache_reads(
    tmp_path: Path, changes: dict[str, object], message: str
) -> None:
    base, _document, _record, _runner = make_base(tmp_path)
    uri = "index/snapshot.json#alpha"
    entry = manifest_entry(uri, **changes)
    payload = json.dumps({"schema_version": 1, "entries": {uri: entry}}).encode()

    with pytest.raises(BodyCacheError, match=message):
        decode_body_manifest(base, payload)


def test_manifest_capacity_and_encoding_limits_are_enforced(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    base, _document, _record, _runner = make_base(tmp_path)
    uri = "index/snapshot.json#alpha"
    payload = json.dumps({"schema_version": 1, "entries": {uri: manifest_entry(uri)}}).encode()

    monkeypatch.setattr(bodies, "MAX_BODY_CACHE_ENTRIES", 0)
    with pytest.raises(BodyCacheError, match="entries; limit 0"):
        decode_body_manifest(base, payload)

    monkeypatch.setattr(bodies, "MAX_BODY_CACHE_ENTRIES", 2)
    monkeypatch.setattr(bodies, "MAX_BODY_CACHE_BYTES", -1)
    with pytest.raises(BodyCacheError, match="cache exceeds"):
        decode_body_manifest(base, payload)

    monkeypatch.setattr(bodies, "MAX_BODY_CACHE_BYTES", 1)
    monkeypatch.setattr(bodies, "MAX_BODY_MANIFEST_BYTES", 1)
    with pytest.raises(BodyCacheError, match="manifest is"):
        encode_body_manifest(BodyManifest())


def test_cached_body_rejects_non_utf8_and_propagates_non_missing_io_errors(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    base, _document, _record, _runner = make_base(tmp_path)
    uri = "index/snapshot.json#alpha"
    raw = b"\xff"
    entry = BodyManifestEntry(
        uri=uri,
        source="snapshot",
        path=body_cache_relative("snapshot", uri),
        sha256=hashlib.sha256(raw).hexdigest(),
        bytes=len(raw),
        fetched_at="2026-09-06T12:00:00Z",
    )
    write_body_manifest(base, BodyManifest(entries={uri: entry}))
    body_path = base.root / entry.path
    body_path.parent.mkdir(parents=True, exist_ok=True)
    body_path.write_bytes(raw)

    with pytest.raises(BodyCacheError, match="not valid UTF-8"):
        read_cached_body(base, uri)

    def denied(_path: Path, _limit: int) -> bytes:
        raise PermissionError("denied")

    monkeypatch.setattr(bodies, "read_file_limited", denied)
    with pytest.raises(PermissionError, match="denied"):
        load_body_manifest(base)


def test_body_fetch_boundary_handles_disabled_missing_cached_and_invalid_provider_bytes(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setattr(bodies, "require_trust", lambda _config, **_kwargs: None)
    base, document, record, runner = make_base(tmp_path)
    uri = document.record_uri(record)
    assert uri is not None

    cached = cache_body(base, document, record, uri, "cached evidence")
    assert cached.provider_modified_at == "2026-09-05T10:00:00Z"
    assert read_or_fetch_body(base, document, record, uri) == ("cached evidence", "cached")
    assert runner.calls == 0

    (base.root / cached.path).unlink()
    runner.stdout = b"\xff"
    with pytest.raises(BodyCacheError, match="not valid UTF-8"):
        fetch_body(base, document, record)

    disabled_config = CONFIG.replace("enabled: true", "enabled: false")
    disabled, disabled_document, disabled_record, _runner = make_base(
        tmp_path / "disabled", config_text=disabled_config
    )
    with pytest.raises(ConfigError, match="is disabled"):
        fetch_body(disabled, disabled_document, disabled_record)


def test_no_cache_policy_fetches_without_persisting_and_removed_body_command_is_explicit(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setattr(bodies, "require_trust", lambda _config, **_kwargs: None)
    no_cache_config = CONFIG.replace("bodies: cache", "bodies: none")
    base, document, record, runner = make_base(tmp_path, config_text=no_cache_config)
    uri = document.record_uri(record)
    assert uri is not None

    assert read_or_fetch_body(base, document, record, uri) == ("provider body", "fetched")
    assert runner.calls == 1
    assert load_body_manifest(base).entries == {}

    no_body_config = CONFIG.replace('    body: [provider, body, "{{id}}"]\n    bodies: cache\n', "")
    removed, removed_document, removed_record, _runner = make_base(tmp_path / "removed", config_text=no_body_config)
    removed_document.body = True
    removed_uri = removed_document.record_uri(removed_record)
    assert removed_uri is not None
    with pytest.raises(ConfigError, match="no-longer-declared"):
        read_or_fetch_body(removed, removed_document, removed_record, removed_uri)


def test_cache_size_and_selective_prune_boundaries(tmp_path: Path) -> None:
    base, document, record, _runner = make_base(tmp_path)
    uri = document.record_uri(record)
    assert uri is not None

    with pytest.raises(FileTooLargeError, match="body for"):
        cache_body(base, document, record, uri, "x" * (MAX_NARRATIVE_BYTES + 1))
    with pytest.raises(ValueError, match="must not be negative"):
        prune_bodies(base, older_than=-timedelta(seconds=1))
    with pytest.raises(ConfigError, match="unknown source"):
        prune_bodies(base, source="invented")

    cache_body(base, document, record, uri, "recent")
    unchanged = prune_bodies(base, source="snapshot", older_than=timedelta(days=2))
    assert unchanged.pruned == 0
    assert unchanged.message == "body cache unchanged; 1 entry remain"

    pruned = prune_bodies(base, source="snapshot")
    assert pruned.pruned == 1
    assert pruned.message == "pruned 1 body entry; 0 remain"

    manifest = base.root / "bodies" / "manifest.json"
    manifest.parent.mkdir(parents=True)
    manifest.write_text("not-json", encoding="utf-8")
    discarded = prune_bodies(base)
    assert discarded.message == "body cache is empty; invalid manifest discarded"

    symlink_target = tmp_path / "outside"
    symlink_target.mkdir()
    (base.root / "bodies").symlink_to(symlink_target, target_is_directory=True)
    with pytest.raises(UnsafePathError, match="is a symlink"):
        prune_bodies(base)


def test_full_prune_does_not_unlink_through_a_swapped_cache_directory(tmp_path: Path) -> None:
    base, _document, _record, _runner = make_base(tmp_path)
    cache = base.root / "bodies"
    cache.mkdir()
    outside = tmp_path / "outside"
    outside.mkdir()
    outside_manifest = outside / "manifest.json"
    outside_manifest.write_text("outside", encoding="utf-8")

    class SwapBeforeMutation:
        checks = 0

        def is_set(self) -> bool:
            self.checks += 1
            if self.checks == 2:
                cache.rmdir()
                cache.symlink_to(outside, target_is_directory=True)
            return False

    with pytest.raises(BodyCacheError, match="unsafe body cache path"):
        prune_bodies(base, cancel=SwapBeforeMutation())

    assert outside_manifest.read_text(encoding="utf-8") == "outside"


def test_full_prune_does_not_remove_a_real_cache_directory_swapped_after_planning(tmp_path: Path) -> None:
    base, _document, _record, _runner = make_base(tmp_path)
    write_body_manifest(base, BodyManifest())
    cache = base.root / "bodies"
    detached = base.root / "detached-bodies"
    replacement = tmp_path / "replacement"
    replacement.mkdir()
    sentinel = replacement / "keep.txt"
    sentinel.write_text("outside", encoding="utf-8")

    class SwapBeforeMutation:
        checks = 0

        def is_set(self) -> bool:
            self.checks += 1
            if self.checks == 2:
                cache.rename(detached)
                replacement.rename(cache)
            return False

    with pytest.raises(BodyCacheError, match="changed during removal"):
        prune_bodies(base, cancel=SwapBeforeMutation())

    assert (cache / sentinel.name).read_text(encoding="utf-8") == "outside"
    assert (detached / bodies.BODY_MANIFEST_FILE).is_file()


def test_full_prune_does_not_remove_a_real_base_replacement(tmp_path: Path) -> None:
    base, _document, _record, _runner = make_base(tmp_path)
    write_body_manifest(base, BodyManifest())
    root = base.root
    detached = tmp_path / "detached-base"
    replacement = tmp_path / "replacement-base"
    (replacement / "bodies").mkdir(parents=True)
    sentinel = replacement / "bodies" / "keep.txt"
    sentinel.write_text("outside", encoding="utf-8")

    class SwapBeforeMutation:
        checks = 0

        def is_set(self) -> bool:
            self.checks += 1
            if self.checks == 2:
                root.rename(detached)
                replacement.rename(root)
            return False

    with pytest.raises(BodyCacheError, match="changed during removal"):
        prune_bodies(base, cancel=SwapBeforeMutation())

    assert (root / "bodies" / sentinel.name).read_text(encoding="utf-8") == "outside"
    assert (detached / "bodies" / bodies.BODY_MANIFEST_FILE).is_file()


def test_full_prune_does_not_remove_a_directory_replacing_the_open_cache(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    base, _document, _record, _runner = make_base(tmp_path)
    write_body_manifest(base, BodyManifest())
    cache = base.root / "bodies"
    detached = base.root / "detached-bodies"
    replacement = tmp_path / "replacement"
    replacement.mkdir()
    sentinel = replacement / "keep.txt"
    sentinel.write_text("outside", encoding="utf-8")
    real_unlink = bodies.os.unlink
    swapped = False

    def swap_after_manifest_unlink(path: str, *, dir_fd: int | None = None) -> None:
        nonlocal swapped
        real_unlink(path, dir_fd=dir_fd)
        if path == bodies.BODY_MANIFEST_FILE and not swapped:
            swapped = True
            cache.rename(detached)
            replacement.rename(cache)

    monkeypatch.setattr(bodies.os, "unlink", swap_after_manifest_unlink)

    with pytest.raises(BodyCacheError, match="changed during removal"):
        prune_bodies(base)

    assert (cache / sentinel.name).read_text(encoding="utf-8") == "outside"


def test_full_prune_does_not_remove_a_real_nested_directory_replacement(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    base, document, record, _runner = make_base(tmp_path)
    uri = document.record_uri(record)
    assert uri is not None
    cache_body(base, document, record, uri, "selected")
    source_directory = base.root / "bodies" / "snapshot"
    detached = base.root / "detached-snapshot-cache"
    replacement = tmp_path / "replacement-source"
    replacement.mkdir()
    sentinel = replacement / "keep.txt"
    sentinel.write_text("outside", encoding="utf-8")
    real_stat = bodies.os.stat
    swapped = False

    def stat_then_swap(
        path: str | bytes | int,
        *,
        dir_fd: int | None = None,
        follow_symlinks: bool = True,
    ) -> os.stat_result:
        nonlocal swapped
        inspected = real_stat(path, dir_fd=dir_fd, follow_symlinks=follow_symlinks)
        if path == "snapshot" and dir_fd is not None and not swapped:
            swapped = True
            source_directory.rename(detached)
            replacement.rename(source_directory)
        return inspected

    monkeypatch.setattr(bodies.os, "stat", stat_then_swap)

    with pytest.raises(BodyCacheError, match="changed before it was opened"):
        prune_bodies(base)

    assert (source_directory / sentinel.name).read_text(encoding="utf-8") == "outside"
    assert detached.is_dir()


def test_selective_prune_does_not_unlink_through_a_swapped_source_directory(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    base, document, record, _runner = make_base(tmp_path)
    selected_uri = document.record_uri(record)
    assert selected_uri is not None
    selected = cache_body(base, document, record, selected_uri, "selected")
    surviving_record: Record = {"id": "beta", "title": "Beta", "modified": "2026-09-05T11:00:00Z"}
    surviving_uri = document.record_uri(surviving_record)
    assert surviving_uri is not None
    surviving = cache_body(base, document, surviving_record, surviving_uri, "surviving")
    manifest = load_body_manifest(base)
    manifest.entries[selected_uri] = replace(selected, fetched_at="2026-09-01T12:00:00Z")
    write_body_manifest(base, manifest)

    source_directory = base.root / "bodies" / "snapshot"
    detached = base.root / "detached-snapshot-cache"
    outside = tmp_path / "outside-source"
    outside.mkdir()
    outside_sentinel = outside / Path(selected.path).name
    outside_sentinel.write_text("outside", encoding="utf-8")
    real_write_manifest = bodies.write_body_manifest

    def publish_then_swap(target: Base, updated: BodyManifest) -> None:
        real_write_manifest(target, updated)
        source_directory.rename(detached)
        source_directory.symlink_to(outside, target_is_directory=True)

    monkeypatch.setattr(bodies, "write_body_manifest", publish_then_swap)

    with pytest.raises(BodyCacheError, match="unsafe body cache path"):
        prune_bodies(base, older_than=timedelta(days=2))

    assert outside_sentinel.read_text(encoding="utf-8") == "outside"
    source_directory.unlink()
    detached.rename(source_directory)
    assert load_body_manifest(base).entries == {surviving_uri: surviving}


def test_selective_prune_does_not_unlink_from_a_real_source_directory_replacement(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    base, document, record, _runner = make_base(tmp_path)
    selected_uri = document.record_uri(record)
    assert selected_uri is not None
    selected = cache_body(base, document, record, selected_uri, "selected")
    surviving_record: Record = {"id": "beta", "title": "Beta", "modified": "2026-09-05T11:00:00Z"}
    surviving_uri = document.record_uri(surviving_record)
    assert surviving_uri is not None
    surviving = cache_body(base, document, surviving_record, surviving_uri, "surviving")
    manifest = load_body_manifest(base)
    manifest.entries[selected_uri] = replace(selected, fetched_at="2026-09-01T12:00:00Z")
    write_body_manifest(base, manifest)

    source_directory = base.root / "bodies" / "snapshot"
    detached = base.root / "detached-snapshot-cache"
    replacement = tmp_path / "replacement-source"
    replacement.mkdir()
    sentinel = replacement / Path(selected.path).name
    sentinel.write_text("outside", encoding="utf-8")
    real_write_manifest = bodies.write_body_manifest

    def publish_then_swap(target: Base, updated: BodyManifest) -> None:
        real_write_manifest(target, updated)
        source_directory.rename(detached)
        replacement.rename(source_directory)

    monkeypatch.setattr(bodies, "write_body_manifest", publish_then_swap)

    with pytest.raises(BodyCacheError, match="changed during removal"):
        prune_bodies(base, older_than=timedelta(days=2))

    assert (source_directory / sentinel.name).read_text(encoding="utf-8") == "outside"
    source_directory.rename(replacement)
    detached.rename(source_directory)
    assert load_body_manifest(base).entries == {surviving_uri: surviving}


def test_age_filtered_source_prune_preserves_restore_marker_when_no_body_matches(tmp_path: Path) -> None:
    base, document, record, _runner = make_base(tmp_path)
    uri = document.record_uri(record)
    assert uri is not None
    cache_body(base, document, record, uri, "recent")
    manifest = load_body_manifest(base)
    manifest.event_attempts["snapshot"] = True
    write_body_manifest(base, manifest)
    manifest_path = base.root / "bodies" / "manifest.json"
    before = manifest_path.read_bytes()

    report = prune_bodies(base, source="snapshot", older_than=timedelta(days=2))

    assert report.pruned == 0
    assert report.message == "body cache unchanged; 1 entry remain"
    assert manifest_path.read_bytes() == before
    assert load_body_manifest(base).event_attempts == {"snapshot": True}


def test_age_only_prune_rearms_source_when_its_last_body_is_removed(tmp_path: Path) -> None:
    base, document, record, _runner = make_base(tmp_path)
    uri = document.record_uri(record)
    assert uri is not None
    cache_body(base, document, record, uri, "old")
    manifest = load_body_manifest(base)
    manifest.entries[uri] = replace(manifest.entries[uri], fetched_at="2026-09-01T12:00:00Z")
    manifest.event_attempts["snapshot"] = True
    write_body_manifest(base, manifest)

    report = prune_bodies(base, older_than=timedelta(days=2))

    assert report.pruned == 1
    assert load_body_manifest(base).entries == {}
    assert load_body_manifest(base).event_attempts == {}


def test_marker_only_removed_source_remains_a_valid_selective_prune_target(tmp_path: Path) -> None:
    base, _document, _record, _runner = make_base(tmp_path)
    write_body_manifest(base, BodyManifest(event_attempts={"retired": True}))

    report = prune_bodies(base, source="retired")

    assert report.pruned == 0
    assert not (base.root / "bodies").exists()


def test_only_unfiltered_prune_recovers_an_oversized_manifest(tmp_path: Path) -> None:
    base, _document, _record, _runner = make_base(tmp_path)
    manifest = base.root / "bodies" / "manifest.json"
    manifest.parent.mkdir(parents=True)
    manifest.write_bytes(b"x" * (bodies.MAX_BODY_MANIFEST_BYTES + 1))

    with pytest.raises(FileTooLargeError):
        prune_bodies(base, source="snapshot")
    assert manifest.exists()

    report = prune_bodies(base)

    assert report.message == "body cache is empty; invalid manifest discarded"
    assert not manifest.parent.exists()
