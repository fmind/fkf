"""Machine-local approval of a base's exact decoded execution plan."""

from __future__ import annotations

import hashlib
import os
import stat
import struct
from collections.abc import Callable, Mapping, Sequence
from dataclasses import dataclass, field, replace
from datetime import UTC, datetime
from enum import StrEnum
from pathlib import Path
from typing import TYPE_CHECKING, Final

from fkf.errors import OperationalError, UntrustedError
from fkf.io import atomic_write, read_file_limited
from fkf.jsoncodec import dumps, loads
from fkf.locking import ensure_private_state_directory, private_state_directory
from fkf.process import DECLARED_COMMAND_DIRECTORY, DECLARED_COMMAND_ENVIRONMENT_POLICY, Cancellation, check_cancel
from fkf.store import (
    BASE_CLIENTS_DIR,
    BASE_FILE_MODE,
    BASE_SOURCES_DIR,
    BASE_TESTS_DIR,
    LAYERS,
    MAX_CONTROL_FILE_BYTES,
    UnsafePathError,
    expand_home,
    resolve_absolute_path,
)

if TYPE_CHECKING:
    from fkf.config import Config, Source

_FRAMED_TRUST_VERSION: Final = "fkf-framed-trust-v1"
_AGGREGATE_DOMAIN: Final = "execution-plan-v2"
_BASE_DOMAIN: Final = "base-execution-v2"
_SOURCE_DOMAIN: Final = "source-execution-v3"
_SCRIPT_DOMAIN: Final = "script-v2"


class TrustItemKind(StrEnum):
    """Reviewable kinds in one execution plan."""

    CONFIG = "config"
    SOURCE = "source"
    SCRIPT = "script"
    TEST = "test"
    CLIENT = "client"


class TrustChangeKind(StrEnum):
    """How one reviewable item changed since approval."""

    ADDED = "added"
    REMOVED = "removed"
    MODIFIED = "modified"
    ARMED = "armed"
    DISARMED = "disarmed"


@dataclass(frozen=True, slots=True)
class ExecutionEntry:
    """One entry below a base-controlled executable tree."""

    name: str = field(metadata={"json": "name"})
    kind: str = field(metadata={"json": "kind"})
    digest: str = field(default="", metadata={"json": "digest"})
    target: str = field(default="", metadata={"json": "target,omitempty"})
    executable: bool = field(default=False, metadata={"json": "executable"})


@dataclass(frozen=True, slots=True)
class TrustItem:
    """One stable review unit reduced to its execution digest."""

    kind: TrustItemKind = field(metadata={"json": "kind"})
    name: str = field(metadata={"json": "name"})
    digest: str = field(metadata={"json": "digest"})
    executable: bool = field(default=False, metadata={"json": "executable,omitempty"})


@dataclass(frozen=True, slots=True)
class TrustChange:
    """One reviewable difference from the approved item set."""

    kind: TrustChangeKind = field(metadata={"json": "kind"})
    item: TrustItemKind = field(metadata={"json": "item"})
    name: str = field(metadata={"json": "name"})


@dataclass(frozen=True, slots=True)
class TrustRecord:
    """The owner-only machine-local record for one chosen base spelling."""

    base: str = field(metadata={"json": "base"})
    digest: str = field(metadata={"json": "digest"})
    trusted_at: str = field(metadata={"json": "trusted_at"})
    items: tuple[TrustItem, ...] = field(default=(), metadata={"json": "items,omitempty"})


@dataclass(frozen=True, slots=True)
class TrustState:
    """Current execution trust plus an honest per-item change review."""

    base: str = field(metadata={"json": "base"})
    trusted: bool = field(metadata={"json": "trusted"})
    digest: str = field(metadata={"json": "digest"})
    stored_digest: str = field(default="", metadata={"json": "stored_digest,omitempty"})
    trusted_at: str = field(default="", metadata={"json": "trusted_at,omitempty"})
    record: str = field(default="", metadata={"json": "record,omitempty"})
    items: tuple[TrustItem, ...] = field(default=(), metadata={"json": "items,omitempty"})
    changes: tuple[TrustChange, ...] = field(default=(), metadata={"json": "changes,omitempty"})


@dataclass(frozen=True, slots=True)
class TrustSnapshot:
    """One immutable execution-tree review shared by disclosure and recording."""

    base: str
    scripts: tuple[ExecutionEntry, ...]
    clients: tuple[ExecutionEntry, ...]
    tests: tuple[ExecutionEntry, ...]
    items: tuple[TrustItem, ...]
    digest: str


class TrustRecordError(OperationalError):
    """A machine-local trust record is malformed or has an unknown shape."""


class _FramedDigest:
    """Unambiguous SHA-256 fields compatible with the Go trust oracle."""

    __slots__ = ("_hash",)

    def __init__(self, domain: str) -> None:
        self._hash = hashlib.sha256()
        self.value(_FRAMED_TRUST_VERSION)
        self.value(domain)

    def value(self, value: str) -> None:
        encoded = value.encode()
        self._hash.update(struct.pack(">Q", len(encoded)))
        self._hash.update(encoded)

    def field(self, name: str, value: str) -> None:
        self.value(name)
        self.value(value)

    def boolean(self, name: str, value: bool) -> None:
        self.field(name, "true" if value else "false")

    def integer(self, name: str, value: int) -> None:
        self.field(name, str(value))

    def hexdigest(self) -> str:
        return self._hash.hexdigest()


def diff_trust_items(stored: Sequence[TrustItem], current: Sequence[TrustItem]) -> tuple[TrustChange, ...]:
    """Return deterministic added, removed, modified, armed, and disarmed changes."""
    previous = {(item.kind, item.name): item for item in stored}
    seen: set[tuple[TrustItemKind, str]] = set()
    changes: list[TrustChange] = []
    for item in current:
        key = (item.kind, item.name)
        seen.add(key)
        old = previous.get(key)
        if old is None:
            changes.append(TrustChange(TrustChangeKind.ADDED, item.kind, item.name))
        elif old.digest != item.digest:
            changes.append(TrustChange(TrustChangeKind.MODIFIED, item.kind, item.name))
        elif old.executable != item.executable:
            kind = TrustChangeKind.ARMED if item.executable else TrustChangeKind.DISARMED
            changes.append(TrustChange(kind, item.kind, item.name))
    changes.extend(
        TrustChange(TrustChangeKind.REMOVED, item.kind, item.name)
        for item in stored
        if (item.kind, item.name) not in seen
    )
    changes.sort(key=lambda change: (change.item, change.name))
    return tuple(changes)


def trust_record_path(root: str | os.PathLike[str]) -> Path:
    """Name state by the chosen absolute spelling, without resolving aliases."""
    absolute = resolve_absolute_path(root)
    name = hashlib.sha256(os.fsencode(absolute)).hexdigest() + ".json"
    return private_state_directory(absolute, "trust", purpose="trust") / name


def source_scripts(root: str | os.PathLike[str], *, cancel: Cancellation | None = None) -> tuple[ExecutionEntry, ...]:
    """Inventory every entry under the trusted collection helper tree."""
    return _execution_tree(root, BASE_SOURCES_DIR, cancel)


def client_scripts(root: str | os.PathLike[str], *, cancel: Cancellation | None = None) -> tuple[ExecutionEntry, ...]:
    """Inventory every entry under the trusted app-client tree."""
    return _execution_tree(root, BASE_CLIENTS_DIR, cancel)


def test_scripts(
    root: str | os.PathLike[str],
    *,
    cancel: Cancellation | None = None,  # noqa: PT028
) -> tuple[ExecutionEntry, ...]:
    """Inventory every entry under the trusted source-test helper tree."""
    return _execution_tree(root, BASE_TESTS_DIR, cancel)


def _execution_tree(root: str | os.PathLike[str], tree: str, cancel: Cancellation | None) -> tuple[ExecutionEntry, ...]:
    check_cancel(cancel)
    directory = Path(os.path.normpath(expand_home(os.fspath(root)))) / tree
    try:
        root_info = directory.lstat()
    except FileNotFoundError:
        return ()
    except OSError as error:
        raise OSError(f"inspect execution tree {directory}: {error}") from error
    if stat.S_ISLNK(root_info.st_mode):
        raise UnsafePathError(f"unsafe filesystem path: managed tree {directory} is a symlink")
    if not stat.S_ISDIR(root_info.st_mode):
        raise UnsafePathError(f"unsafe filesystem path: managed tree {directory} must be a real directory")

    entries: list[ExecutionEntry] = []
    _walk_execution_tree(directory, directory, entries, cancel)
    entries.sort(key=lambda entry: entry.name)
    return tuple(entries)


def _walk_execution_tree(
    root: Path, directory: Path, entries: list[ExecutionEntry], cancel: Cancellation | None
) -> None:
    # Re-inspection keeps a directory replacement between its parent listing and recursion from
    # silently redirecting the audit through a link or a different entry kind.
    try:
        directory_info = directory.lstat()
    except OSError as error:
        raise OSError(f"inspect execution tree entry {directory}: {error}") from error
    if stat.S_ISLNK(directory_info.st_mode) or not stat.S_ISDIR(directory_info.st_mode):
        raise UnsafePathError(f"unsafe filesystem path: managed tree entry {directory} must be a real directory")
    try:
        with os.scandir(directory) as iterator:
            children = sorted(iterator, key=lambda entry: entry.name)
    except OSError as error:
        raise OSError(f"inspect execution tree entry {directory}: {error}") from error

    for child in children:
        check_cancel(cancel)
        path = directory / child.name
        try:
            info = path.lstat()
        except OSError as error:
            raise OSError(f"inspect execution tree entry {path}: {error}") from error
        if stat.S_ISLNK(info.st_mode):
            raise UnsafePathError(f"unsafe filesystem path: managed tree entry {path} is a symlink")
        name = path.relative_to(root).as_posix()
        if stat.S_ISREG(info.st_mode):
            try:
                content = read_file_limited(path, MAX_CONTROL_FILE_BYTES)
            except OSError as error:
                raise OSError(f"read the base script {path}: {error}") from error
            entries.append(
                ExecutionEntry(
                    name=name,
                    kind="script",
                    digest=hashlib.sha256(content).hexdigest(),
                    executable=bool(info.st_mode & 0o111),
                )
            )
            continue
        entries.append(ExecutionEntry(name=name, kind=_go_file_mode_kind(info.st_mode)))
        if stat.S_ISDIR(info.st_mode):
            _walk_execution_tree(root, path, entries, cancel)


def _go_file_mode_kind(mode: int) -> str:
    """Render Python stat kinds like Go's ``FileMode.Type().String``."""
    if stat.S_ISDIR(mode):
        prefix = "d"
    elif stat.S_ISFIFO(mode):
        prefix = "p"
    elif stat.S_ISSOCK(mode):
        prefix = "S"
    elif stat.S_ISBLK(mode):
        prefix = "D"
    elif stat.S_ISCHR(mode):
        prefix = "Dc"
    else:
        prefix = "?"
    return prefix + "---------"


def _trust_items(
    config: Config,
    scripts: tuple[ExecutionEntry, ...],
    tests: tuple[ExecutionEntry, ...],
    clients: tuple[ExecutionEntry, ...],
    cancel: Cancellation | None,
) -> tuple[TrustItem, ...]:
    check_cancel(cancel)
    items = [TrustItem(TrustItemKind.CONFIG, "base", _base_execution_digest(config))]
    for name in config.source_names():
        check_cancel(cancel)
        source = config.sources[name]
        items.append(TrustItem(TrustItemKind.SOURCE, source.name, _source_execution_digest(source)))
    for kind, entries in (
        (TrustItemKind.SCRIPT, scripts),
        (TrustItemKind.TEST, tests),
        (TrustItemKind.CLIENT, clients),
    ):
        check_cancel(cancel)
        items.extend(
            TrustItem(kind, script.name, _script_trust_digest(script), executable=script.executable)
            for script in entries
        )
    items.sort(key=lambda item: (item.kind, item.name))
    return tuple(items)


def capture_trust(config: Config, *, cancel: Cancellation | None = None) -> TrustSnapshot:
    """Capture the exact configuration and execution trees for one trust decision."""
    root = config.store().root
    scripts = source_scripts(root, cancel=cancel)
    tests = test_scripts(root, cancel=cancel)
    clients = client_scripts(root, cancel=cancel)
    client_files = {entry.name for entry in clients if entry.kind == "script"}
    for name, client in config.clients.items():
        if client.script not in client_files:
            raise UnsafePathError(f"client {name}: clients/{client.script} must be a regular non-symlink file")
    items = _trust_items(config, scripts, tests, clients, cancel)
    return TrustSnapshot(
        base=os.fspath(root),
        scripts=scripts,
        clients=clients,
        tests=tests,
        items=items,
        digest=_digest_trust_items(items, cancel),
    )


def trust_items(config: Config, *, cancel: Cancellation | None = None) -> tuple[TrustItem, ...]:
    """Reduce one decoded configuration and both execution trees to review units."""
    return capture_trust(config, cancel=cancel).items


def config_digest(config: Config, *, cancel: Cancellation | None = None) -> str:
    """Hash the review items for the caller's exact decoded configuration snapshot."""
    return capture_trust(config, cancel=cancel).digest


def _digest_trust_items(items: Sequence[TrustItem], cancel: Cancellation | None = None) -> str:
    digest = _FramedDigest(_AGGREGATE_DOMAIN)
    for item in items:
        check_cancel(cancel)
        digest.field("item-kind", item.kind)
        digest.field("item-name", item.name)
        digest.field("item-digest", item.digest)
        digest.boolean("item-executable", item.executable)
    return digest.hexdigest()


def _script_trust_digest(script: ExecutionEntry) -> str:
    digest = _FramedDigest(_SCRIPT_DOMAIN)
    digest.field("kind", script.kind)
    digest.field("content-digest", script.digest)
    return digest.hexdigest()


def _base_execution_digest(config: Config) -> str:
    digest = _FramedDigest(_BASE_DOMAIN)
    digest.field("source-directory", BASE_SOURCES_DIR)
    digest.field("client-directory", BASE_CLIENTS_DIR)
    digest.field("command-directory", os.fspath(DECLARED_COMMAND_DIRECTORY))
    digest.field("command-environment", DECLARED_COMMAND_ENVIRONMENT_POLICY)
    for layer in LAYERS:
        digest.field("layer-name", layer)
        digest.boolean("layer-enabled", config.layers.get(layer, False))
    digest.integer("sync-days", config.sync.days)
    digest.integer("index-max-age-hours", config.sync.index_max_age_hours)
    digest.integer("timeout", int(config.sync.timeout))
    digest.integer("concurrency", config.sync.concurrency)
    for name, client in sorted(config.clients.items()):
        digest.field("client-name", name)
        digest.field("client-url", client.url)
        digest.field("client-script", client.script)
    for directory in config.bin:
        digest.field("bin", directory)
    return digest.hexdigest()


def _source_execution_digest(source: Source) -> str:
    digest = _FramedDigest(_SOURCE_DOMAIN)
    digest.boolean("enabled", source.enabled)
    digest.field("layer", source.layer)
    digest.boolean("max-age-hours-set", source.max_age_hours is not None)
    if source.max_age_hours is not None:
        digest.integer("max-age-hours", source.max_age_hours)
    for name, arguments in (
        ("auth", source.auth),
        ("run", source.run),
        ("test", source.test),
        ("body", source.body),
    ):
        for argument in arguments:
            digest.field(name, argument)
    digest.field("bodies-policy", source.bodies)
    for name in source.body_field_names():
        for field_path in source.fields.paths(name):
            digest.field("body-field-name", name)
            digest.field("body-field-path", str(field_path))
    digest.integer("timeout", int(source.timeout))
    digest.integer("retry-attempts", source.retry.attempts)
    digest.integer("retry-backoff", int(source.retry.backoff))
    digest.integer("min-interval", int(source.min_interval))
    digest.boolean("window", source.window)
    for condition in source.retry.on:
        digest.field("retry-on", condition)
    return digest.hexdigest()


def read_trust(
    config: Config,
    *,
    cancel: Cancellation | None = None,
    snapshot: TrustSnapshot | None = None,
) -> TrustState:
    """Compare current execution items with the machine-local approval record."""
    root = config.store().root
    current = snapshot or capture_trust(config, cancel=cancel)
    if current.base != os.fspath(root):
        raise ValueError("trust snapshot belongs to another base")
    items = current.items
    digest = current.digest
    path = trust_record_path(root)
    state = TrustState(base=os.fspath(root), trusted=False, digest=digest, record=os.fspath(path), items=items)
    try:
        check_cancel(cancel)
        data = read_file_limited(path, MAX_CONTROL_FILE_BYTES)
    except OSError as error:
        if _caused_by_missing_file(error):
            return state
        raise
    record = _decode_trust_record(data, path)
    trusted = record.digest == digest
    changes = diff_trust_items(record.items, items) if not trusted and record.items else ()
    return replace(
        state,
        trusted=trusted,
        stored_digest=record.digest,
        trusted_at=record.trusted_at,
        changes=changes,
    )


def write_trust(
    config: Config,
    now: datetime,
    *,
    cancel: Cancellation | None = None,
    snapshot: TrustSnapshot | None = None,
) -> TrustState:
    """Atomically approve the caller's exact decoded configuration snapshot."""
    if now.tzinfo is None or now.utcoffset() is None:
        raise ValueError("trust timestamp must include a timezone")
    state = read_trust(config, cancel=cancel, snapshot=snapshot)
    trusted_at = now.astimezone(UTC).replace(microsecond=0).isoformat().replace("+00:00", "Z")
    record = TrustRecord(state.base, state.digest, trusted_at, state.items)
    path = Path(state.record)
    # Cancellation is fail-closed only before publication; once the state directories exist,
    # the atomic record replacement is one indivisible approval operation.
    check_cancel(cancel)
    ensure_private_state_directory(config.store().root, "trust", purpose="trust")
    atomic_write(path, dumps(record, indent=True, newline=True), mode=BASE_FILE_MODE)
    return replace(state, trusted=True, stored_digest=state.digest, trusted_at=trusted_at, changes=())


def require_trust(config: Config, *, cancel: Cancellation | None = None) -> None:
    """Refuse execution until the current decoded plan has been approved locally."""
    state = read_trust(config, cancel=cancel)
    if state.trusted:
        return
    if not state.stored_digest:
        raise UntrustedError(
            f"base is not trusted on this machine: {state.base} has never been trusted here; "
            f"run `fkf trust --base {state.base}` to read its commands and record them"
        )
    raise UntrustedError(
        f"base is not trusted on this machine: the configuration of {state.base} changed since it was trusted "
        f"on {state.trusted_at}; review the change and run `fkf trust --base {state.base}`"
    )


def trust_check(config: Config, *, cancel: Cancellation | None = None) -> Callable[[], None]:
    """Bind the decoded configuration snapshot for ``Command.before_exec``."""

    def check() -> None:
        require_trust(config, cancel=cancel)

    return check


def _caused_by_missing_file(error: BaseException) -> bool:
    current: BaseException | None = error
    while current is not None:
        if isinstance(current, FileNotFoundError):
            return True
        current = current.__cause__
    return False


def _decode_trust_record(data: bytes, path: Path) -> TrustRecord:
    try:
        value = loads(data)
        record = _required_object(value, "record", {"base", "digest", "trusted_at"}, {"items"})
        items_value = record.get("items", [])
        if not isinstance(items_value, list):
            raise ValueError("record.items must be an array")
        items = tuple(_decode_trust_item(item, index) for index, item in enumerate(items_value))
        return TrustRecord(
            base=_required_string(record, "base", "record"),
            digest=_required_string(record, "digest", "record"),
            trusted_at=_required_string(record, "trusted_at", "record"),
            items=items,
        )
    except (TypeError, ValueError) as error:
        raise TrustRecordError(f"decode trust record {path}: {error}", cause=error) from error


def _decode_trust_item(value: object, index: int) -> TrustItem:
    label = f"record.items[{index}]"
    item = _required_object(value, label, {"kind", "name", "digest"}, {"executable"})
    try:
        kind = TrustItemKind(_required_string(item, "kind", label))
    except ValueError as error:
        raise ValueError(f"{label}.kind is unknown") from error
    executable = item.get("executable", False)
    if not isinstance(executable, bool):
        raise ValueError(f"{label}.executable must be a boolean")
    return TrustItem(
        kind=kind,
        name=_required_string(item, "name", label),
        digest=_required_string(item, "digest", label),
        executable=executable,
    )


def _required_object(
    value: object,
    label: str,
    required: set[str],
    optional: set[str],
) -> Mapping[str, object]:
    if not isinstance(value, Mapping) or any(not isinstance(key, str) for key in value):
        raise ValueError(f"{label} must be an object")
    keys = set(value)
    missing = required - keys
    unknown = keys - required - optional
    if missing:
        raise ValueError(f"{label} is missing {', '.join(sorted(missing))}")
    if unknown:
        raise ValueError(f"{label} has unknown field {min(unknown)}")
    return value


def _required_string(value: Mapping[str, object], name: str, label: str) -> str:
    item = value[name]
    if not isinstance(item, str):
        raise ValueError(f"{label}.{name} must be a string")
    return item


__all__ = [
    "ExecutionEntry",
    "TrustChange",
    "TrustChangeKind",
    "TrustItem",
    "TrustItemKind",
    "TrustRecord",
    "TrustRecordError",
    "TrustSnapshot",
    "TrustState",
    "capture_trust",
    "client_scripts",
    "config_digest",
    "diff_trust_items",
    "read_trust",
    "require_trust",
    "source_scripts",
    "test_scripts",
    "trust_check",
    "trust_items",
    "trust_record_path",
    "write_trust",
]
