"""Machine-local execution trust and reviewable plan digests."""

from __future__ import annotations

import multiprocessing
import os
from datetime import UTC, datetime
from pathlib import Path
from threading import Event

import pytest

from fkf.config import load_config
from fkf.errors import CanceledError, UntrustedError
from fkf.io import FileTooLargeError
from fkf.jsoncodec import dumps
from fkf.locking import StateDirectoryUnavailableError
from fkf.process import Command, Disclosure, SubprocessRunner
from fkf.store import MAX_CONTROL_FILE_BYTES, UnsafePathError
from fkf.timeutil import parse_duration
from fkf.trust import (
    ExecutionEntry,
    TrustChange,
    TrustChangeKind,
    TrustItem,
    TrustItemKind,
    TrustRecord,
    TrustRecordError,
    TrustState,
    bin_scripts,
    config_digest,
    diff_trust_items,
    read_trust,
    require_trust,
    trust_check,
    trust_items,
    trust_record_path,
    write_trust,
)
from fkf.trust import test_scripts as inventory_test_scripts

SCHEMA = """\
fkf: 1
name: brain
schema:
  id: {description: Stable provider identity., cardinality: one}
  time: {description: Event time., cardinality: one}
  title: {description: Meaningful subject line., cardinality: optional}
  project: {description: Provider project., cardinality: optional}
  topic: {description: Retrieval-only topic., cardinality: optional}
layers: {events: true, index: true, tasks: true, projects: true, wiki: true}
"""


def _write_base(tmp_path: Path, body: str = "sources: {}\n") -> Path:
    root = tmp_path / "brain"
    root.mkdir()
    (root / "fkf.yaml").write_text(SCHEMA + body, encoding="utf-8")
    return root


def _write_trust_worker(root: str, state_root: str, instant: str) -> None:
    os.environ["XDG_STATE_HOME"] = state_root
    config = load_config(root)
    write_trust(config, datetime.fromisoformat(instant))


def test_typed_json_models_keep_go_field_order_and_omissions() -> None:
    item = TrustItem(kind=TrustItemKind.SCRIPT, name="helper", digest="abc")
    record = TrustRecord(base="/base", digest="aggregate", trusted_at="2026-09-06T08:00:00Z")
    state = TrustState(base="/base", trusted=False, digest="aggregate")
    change = TrustChange(kind=TrustChangeKind.ARMED, item=TrustItemKind.SCRIPT, name="helper")

    assert dumps(item) == b'{"kind":"script","name":"helper","digest":"abc"}'
    assert dumps(record) == b'{"base":"/base","digest":"aggregate","trusted_at":"2026-09-06T08:00:00Z"}'
    assert dumps(state) == b'{"base":"/base","trusted":false,"digest":"aggregate"}'
    assert dumps(change) == b'{"kind":"armed","item":"script","name":"helper"}'


def test_config_digest_matches_the_current_go_oracle(tmp_path: Path) -> None:
    root = _write_base(
        tmp_path,
        """\
bin: [/opt/provider-tools, ~/bin]
sync: {days: 7, index_max_age_hours: 36, timeout: 1500ms, concurrency: 3}
sources:
  github:
    enabled: true
    layer: events
    auth: [gh, auth, status]
    run: [gh, api, --paginate, "updated≥{{start}}"]
    test: [github-check, "{{base}}"]
    fields:
      id: .id
      time: .updated
      title: .title
      project: [.project, .fallback]
      topic: .topic
    body: [gh, view, "{{id}}", --repo, "{{project}}"]
    bodies: cache
    timeout: 5s
    retry: {attempts: 3, backoff: 250ms, on: ["exit:7", transient]}
    min_interval: 1s
    window: true
""",
    )

    config = load_config(root)
    items = trust_items(config)

    # Constants preserve the released trust-digest contract across implementations.
    assert [(item.kind, item.name, item.digest, item.executable) for item in items] == [
        (TrustItemKind.CONFIG, "base", "ebf2c9406be92805f23b9fdaf9b1685e9f85698e47aef9bbd75d47f32df11b25", False),
        (TrustItemKind.SOURCE, "github", "60923b76bc804432267f091113cb638dba30e5e226931e6e3c4b8e66ca616600", False),
    ]
    assert config_digest(config) == "0ccd32750b9c1547a7c7f573d12a1eae0f0488c61d95f94f64e722be08e896bf"


def test_digest_tracks_execution_changes_but_not_retrieval_metadata(tmp_path: Path) -> None:
    initial = """\
sources:
  source:
    enabled: true
    layer: events
    run: [provider, list]
    fields: {id: .id, time: .time, title: .title, topic: .topic}
"""
    root = _write_base(tmp_path, initial)
    before = config_digest(load_config(root))

    semantic_only = initial.replace(".topic", ".category")
    (root / "fkf.yaml").write_text(SCHEMA.replace("Retrieval-only topic.", "Search labels.") + semantic_only)
    assert config_digest(load_config(root)) == before

    execution_change = semantic_only.replace("provider, list", "provider, list, --all")
    (root / "fkf.yaml").write_text(SCHEMA + execution_change)
    assert config_digest(load_config(root)) != before


def test_execution_tree_inventory_is_recursive_deterministic_and_complete(tmp_path: Path) -> None:
    root = _write_base(tmp_path)
    nested = root / "bin" / "lib"
    nested.mkdir(parents=True)
    helper = nested / "helper.sh"
    helper.write_bytes(b"#!/bin/sh\nprintf helper\n")
    helper.chmod(0o700)
    plain = root / "bin" / "README"
    plain.write_text("support\n", encoding="utf-8")
    fifo = root / "bin" / "events.pipe"
    os.mkfifo(fifo)

    inventory = bin_scripts(root)
    assert inventory == (
        ExecutionEntry(
            name="README", kind="script", digest="c05b56ab6d3d07ccca6778ebd799bf98dfa2b7f10dd43601f0938f04e5070878"
        ),
        ExecutionEntry(name="events.pipe", kind="p---------"),
        ExecutionEntry(name="lib", kind="d---------"),
        ExecutionEntry(
            name="lib/helper.sh",
            kind="script",
            digest="365960df36cc0339197a4262a978c32dbaf5226f5892267b8ff08bee4e4a502c",
            executable=True,
        ),
    )
    assert inventory_test_scripts(root) == ()

    # This complete item list and aggregate are from the same Go oracle as the entry kinds.
    config = load_config(root)
    assert [(item.kind, item.name, item.digest, item.executable) for item in trust_items(config)] == [
        (TrustItemKind.CONFIG, "base", "c284f84913af007d617a9fac39111283842b76f9b41a462f5aecdb3cd2546316", False),
        (TrustItemKind.SCRIPT, "README", "ef368410ab8e4e16d994084e0bc431be5ec232b76af69245a38d1f7c2c431d7e", False),
        (
            TrustItemKind.SCRIPT,
            "events.pipe",
            "3fa9a0d8fdbbb08183826329b53e067d64ebe82ee19093eae4dfc72160844519",
            False,
        ),
        (TrustItemKind.SCRIPT, "lib", "f823f1de055e531fd578e3bbf03417d3a89355b245d118c8e7322b16ff517c8c", False),
        (
            TrustItemKind.SCRIPT,
            "lib/helper.sh",
            "e292dfd3ec7630aa10251e6e5f70cbda5e9e526303fbd297165ba339b892733c",
            True,
        ),
    ]
    assert config_digest(config) == "7191b9da98e81c4125aad4330f16f47224ef19f063e51fcfb1fa685e2d06fdf3"


@pytest.mark.parametrize("tree", ["bin", "tests"])
@pytest.mark.parametrize("location", ["root", "nested-file", "nested-directory"])
def test_execution_trees_refuse_every_symlink(tmp_path: Path, tree: str, location: str) -> None:
    root = _write_base(tmp_path)
    outside = tmp_path / "outside"
    outside.mkdir()
    (outside / "helper").write_text("external", encoding="utf-8")
    if location == "root":
        (root / tree).symlink_to(outside, target_is_directory=True)
    else:
        directory = root / tree
        directory.mkdir()
        target = outside / "helper" if location == "nested-file" else outside
        (directory / "link").symlink_to(target, target_is_directory=location == "nested-directory")

    inventory = bin_scripts if tree == "bin" else inventory_test_scripts
    with pytest.raises(UnsafePathError, match="symlink"):
        inventory(root)


@pytest.mark.parametrize("tree", ["bin", "tests"])
def test_execution_tree_refuses_a_non_directory_root(tmp_path: Path, tree: str) -> None:
    root = _write_base(tmp_path)
    (root / tree).write_text("not a directory", encoding="utf-8")

    inventory = bin_scripts if tree == "bin" else inventory_test_scripts
    with pytest.raises(UnsafePathError, match="real directory"):
        inventory(root)


def test_execution_tree_fails_closed_on_oversize_and_mid_walk_disappearance(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    root = _write_base(tmp_path)
    directory = root / "bin"
    directory.mkdir()
    script = directory / "helper"
    script.write_bytes(b"x" * (MAX_CONTROL_FILE_BYTES + 1))
    with pytest.raises(FileTooLargeError):
        bin_scripts(root)

    script.write_text("stable", encoding="utf-8")
    from fkf import trust

    original = trust.read_file_limited

    def vanish(path: str | os.PathLike[str], limit: int) -> bytes:
        Path(path).unlink()
        return original(path, limit)

    monkeypatch.setattr(trust, "read_file_limited", vanish)
    with pytest.raises(OSError, match="read the base script"):
        bin_scripts(root)


def test_execution_tree_inventory_cancels_between_hashed_entries(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    root = _write_base(tmp_path)
    directory = root / "bin"
    directory.mkdir()
    (directory / "a").write_text("first", encoding="utf-8")
    (directory / "b").write_text("second", encoding="utf-8")
    cancel = Event()
    from fkf import trust

    original = trust.read_file_limited

    def cancel_after_first(path: str | os.PathLike[str], limit: int) -> bytes:
        data = original(path, limit)
        if Path(path).name == "a":
            cancel.set()
        return data

    monkeypatch.setattr(trust, "read_file_limited", cancel_after_first)

    with pytest.raises(CanceledError):
        trust_items(load_config(root), cancel=cancel)


def test_trust_cancellation_before_record_publish_writes_no_state(tmp_path: Path) -> None:
    root = _write_base(tmp_path)
    config = load_config(root)
    cancel = Event()
    cancel.set()

    with pytest.raises(CanceledError):
        write_trust(config, datetime(2026, 9, 6, tzinfo=UTC), cancel=cancel)

    assert not Path(trust_record_path(root)).exists()


def test_item_diff_names_content_and_executable_changes_in_stable_order() -> None:
    stored = (
        TrustItem(TrustItemKind.SCRIPT, "helper", "same"),
        TrustItem(TrustItemKind.SOURCE, "removed", "old"),
        TrustItem(TrustItemKind.TEST, "check", "old"),
    )
    current = (
        TrustItem(TrustItemKind.SCRIPT, "helper", "same", executable=True),
        TrustItem(TrustItemKind.SOURCE, "added", "new"),
        TrustItem(TrustItemKind.TEST, "check", "new"),
    )

    assert diff_trust_items(stored, current) == (
        TrustChange(TrustChangeKind.ARMED, TrustItemKind.SCRIPT, "helper"),
        TrustChange(TrustChangeKind.ADDED, TrustItemKind.SOURCE, "added"),
        TrustChange(TrustChangeKind.REMOVED, TrustItemKind.SOURCE, "removed"),
        TrustChange(TrustChangeKind.MODIFIED, TrustItemKind.TEST, "check"),
    )
    assert diff_trust_items(
        (TrustItem(TrustItemKind.SCRIPT, "helper", "same", executable=True),),
        (TrustItem(TrustItemKind.SCRIPT, "helper", "same"),),
    ) == (TrustChange(TrustChangeKind.DISARMED, TrustItemKind.SCRIPT, "helper"),)


def test_write_read_require_and_change_review_are_atomic_owner_only_and_outside_base(tmp_path: Path) -> None:
    root = _write_base(tmp_path)
    config = load_config(root)
    before = tuple(root.iterdir())

    never = read_trust(config)
    assert never.trusted is False
    assert never.stored_digest == ""
    assert never.changes == ()
    with pytest.raises(UntrustedError, match="never been trusted"):
        require_trust(config)

    trusted = write_trust(config, datetime(2026, 9, 6, 10, 11, 12, 999999, tzinfo=UTC))
    assert trusted.trusted is True
    assert trusted.stored_digest == trusted.digest
    assert trusted.trusted_at == "2026-09-06T10:11:12Z"
    assert tuple(root.iterdir()) == before
    record = Path(trusted.record)
    assert not record.is_relative_to(root)
    assert record.parent.stat().st_mode & 0o777 == 0o700
    assert record.stat().st_mode & 0o777 == 0o600
    assert (
        dumps(TrustRecord(trusted.base, trusted.digest, trusted.trusted_at, trusted.items), indent=True, newline=True)
        == record.read_bytes()
    )
    require_trust(config)

    helper = root / "bin" / "helper"
    helper.parent.mkdir()
    helper.write_text("one", encoding="utf-8")
    changed = read_trust(config)
    assert changed.trusted is False
    assert changed.changes == (TrustChange(TrustChangeKind.ADDED, TrustItemKind.SCRIPT, "helper"),)
    with pytest.raises(UntrustedError, match="changed since it was trusted"):
        require_trust(config)


def test_read_trust_binds_the_callers_decoded_snapshot_not_a_disk_reload(tmp_path: Path) -> None:
    root = _write_base(tmp_path)
    config = load_config(root)
    write_trust(config, datetime(2026, 9, 6, tzinfo=UTC))
    (root / "fkf.yaml").write_text(SCHEMA + "sync: {days: 7}\nsources: {}\n", encoding="utf-8")

    require_trust(config)
    with pytest.raises(UntrustedError):
        require_trust(load_config(root))


def test_trust_callback_rechecks_immediately_before_process_start(tmp_path: Path) -> None:
    root = _write_base(tmp_path)
    config = load_config(root)
    helper = root / "bin" / "helper"
    helper.parent.mkdir()
    marker = tmp_path / "started"
    helper.write_text(f"#!/bin/sh\nprintf started > {marker}\n", encoding="utf-8")
    helper.chmod(0o700)
    write_trust(config, datetime(2026, 9, 6, tzinfo=UTC))
    callback = trust_check(config)
    helper.write_text(f"#!/bin/sh\nprintf changed > {marker}\n", encoding="utf-8")

    command = Command(
        argv=("helper",),
        timeout=parse_duration("1s"),
        environment={"PATH": ""},
        base=root,
        disclosure=Disclosure.OPAQUE_BODY,
        before_exec=callback,
    )
    with pytest.raises(UntrustedError):
        SubprocessRunner().run(command)
    assert not marker.exists()


def test_record_path_preserves_chosen_absolute_symlink_spelling(tmp_path: Path) -> None:
    root = _write_base(tmp_path)
    alias = tmp_path / "alias"
    alias.symlink_to(root, target_is_directory=True)

    assert trust_record_path(root) != trust_record_path(alias)
    state = write_trust(load_config(alias), datetime(2026, 9, 6, tzinfo=UTC))
    assert state.base == os.fspath(alias)


def test_trust_state_must_stay_outside_the_base(monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> None:
    root = _write_base(tmp_path)
    monkeypatch.setenv("XDG_STATE_HOME", os.fspath(root / "machine-state"))

    with pytest.raises(StateDirectoryUnavailableError, match="outside the base"):
        read_trust(load_config(root))
    assert not (root / "machine-state").exists()


def test_trust_read_rejects_a_symlinked_private_state_directory(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    root = _write_base(tmp_path)
    state = tmp_path / "state"
    machine_state = state / "fkf"
    machine_state.mkdir(parents=True)
    outside = tmp_path / "outside-trust"
    outside.mkdir()
    (machine_state / "trust").symlink_to(outside, target_is_directory=True)
    monkeypatch.setenv("XDG_STATE_HOME", os.fspath(state))

    with pytest.raises(StateDirectoryUnavailableError, match="must be a real directory"):
        read_trust(load_config(root))


@pytest.mark.parametrize(
    "body",
    [
        b"not-json",
        b"{}",
        b'{"base":"/base","digest":"d","trusted_at":"t","unknown":true}',
        b'{"base":"/base","digest":"d","trusted_at":"t","items":[{"kind":"alien","name":"x","digest":"d"}]}',
        b'{"base":"/base","digest":"d","trusted_at":"t","items":"wrong"}',
    ],
)
def test_trust_record_decode_is_strict(body: bytes, tmp_path: Path) -> None:
    config = load_config(_write_base(tmp_path))
    path = trust_record_path(config.store().root)
    path.parent.mkdir(parents=True)
    path.write_bytes(body)

    with pytest.raises(TrustRecordError, match="decode trust record"):
        read_trust(config)


def test_trust_record_read_is_bounded_and_rejects_symlinks(tmp_path: Path) -> None:
    config = load_config(_write_base(tmp_path))
    path = trust_record_path(config.store().root)
    path.parent.mkdir(parents=True)
    path.write_bytes(b"x" * (MAX_CONTROL_FILE_BYTES + 1))
    with pytest.raises(FileTooLargeError):
        read_trust(config)

    path.unlink()
    outside = tmp_path / "record"
    outside.write_text("{}", encoding="utf-8")
    path.symlink_to(outside)
    with pytest.raises(UnsafePathError):
        read_trust(config)


def test_concurrent_writers_leave_one_complete_trusted_record(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    root = _write_base(tmp_path)
    state_root = tmp_path / "machine-state"
    monkeypatch.setenv("XDG_STATE_HOME", os.fspath(state_root))
    context = multiprocessing.get_context("spawn")
    processes = [
        context.Process(
            target=_write_trust_worker,
            args=(os.fspath(root), os.fspath(state_root), f"2026-09-06T10:11:{second:02d}+00:00"),
        )
        for second in (12, 13)
    ]
    for process in processes:
        process.start()
    for process in processes:
        process.join(timeout=10)
        assert process.exitcode == 0

    state = read_trust(load_config(root))
    assert state.trusted is True
    assert state.trusted_at in {"2026-09-06T10:11:12Z", "2026-09-06T10:11:13Z"}
