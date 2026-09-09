"""Manifest-verified, rebuildable local cache for explicitly fetched bodies."""

from __future__ import annotations

import hashlib
import os
import secrets
import stat
from collections.abc import Iterator, Mapping
from contextlib import contextmanager, suppress
from dataclasses import dataclass, field, replace
from datetime import UTC, datetime, timedelta
from pathlib import Path, PurePosixPath
from typing import Final, cast

from fkf.base import Base
from fkf.config import BodyPolicy, ConfigError, validate_source_name
from fkf.documents import Document, Record
from fkf.errors import OperationalError
from fkf.io import FileTooLargeError, atomic_write, read_file_limited
from fkf.jsoncodec import JsonNumber, JsonValue, dumps, loads
from fkf.process import Cancellation, check_cancel
from fkf.source_runtime import build_body_command
from fkf.store import BASE_FILE_MODE, MAX_NARRATIVE_BYTES, validate_within_root
from fkf.timeutil import Instant, format_rfc3339, parse_rfc3339
from fkf.trust import require_trust

BODIES_DIRECTORY: Final = "bodies"
BODY_MANIFEST_FILE: Final = "manifest.json"
BODY_MANIFEST_SCHEMA: Final = 1
MAX_BODY_CACHE_ENTRIES: Final = 4096
MAX_BODY_CACHE_BYTES: Final = 512 << 20
# Cache metadata repeats record URIs and must accommodate the declared entry capacity.
MAX_BODY_MANIFEST_BYTES: Final = 8 << 20

_ENTRY_KEYS: Final = frozenset({"uri", "source", "path", "sha256", "bytes", "provider_modified_at", "fetched_at"})
_MANIFEST_KEYS: Final = frozenset({"schema_version", "entries", "event_attempts"})
_DIRECTORY_FLAGS: Final = (
    os.O_RDONLY | getattr(os, "O_CLOEXEC", 0) | getattr(os, "O_DIRECTORY", 0) | getattr(os, "O_NOFOLLOW", 0)
)
_READ_FLAGS: Final = os.O_RDONLY | getattr(os, "O_CLOEXEC", 0) | getattr(os, "O_NOFOLLOW", 0)


class BodyCacheError(OperationalError):
    """A body cache cannot be trusted as the bytes its manifest names."""


@dataclass(frozen=True, slots=True)
class BodyManifestEntry:
    uri: str
    source: str
    path: str
    sha256: str
    bytes: int
    provider_modified_at: str = field(default="", metadata={"json": "provider_modified_at,omitempty"})
    fetched_at: str = ""


@dataclass(slots=True)
class BodyManifest:
    schema_version: int = BODY_MANIFEST_SCHEMA
    entries: dict[str, BodyManifestEntry] = field(default_factory=dict)
    event_attempts: dict[str, bool] = field(default_factory=dict, metadata={"json": "event_attempts,omitempty"})


@dataclass(frozen=True, slots=True)
class BodiesBuildReport:
    pruned: int
    bytes: int
    message: str


@dataclass(slots=True)
class _BodyCacheHandle:
    base: Base
    root: Path
    root_descriptor: int
    root_identity: tuple[int, int]
    descriptor: int | None
    identity: tuple[int, int] | None


@dataclass(slots=True)
class _BodySourceHandle:
    name: str
    path: Path
    descriptor: int
    identity: tuple[int, int]


def _identity(info: os.stat_result) -> tuple[int, int]:
    return info.st_dev, info.st_ino


def _body_cache_error(action: str, path: Path, error: OSError) -> BodyCacheError:
    return BodyCacheError(f"unsafe body cache path: {action} {path}: {error}")


@contextmanager
def _open_body_cache(base: Base) -> Iterator[_BodyCacheHandle]:
    """Bind a cache mutation to one physical base and cache directory generation."""

    try:
        root = base.root.resolve(strict=True)
        inspected_root = root.stat(follow_symlinks=False)
        root_descriptor = os.open(root, _DIRECTORY_FLAGS)
    except OSError as error:
        raise _body_cache_error("open base", base.root, error) from error
    try:
        opened_root = os.fstat(root_descriptor)
        if not stat.S_ISDIR(opened_root.st_mode) or _identity(inspected_root) != _identity(opened_root):
            raise BodyCacheError(f"unsafe body cache path: {base.root} changed before it was opened")
        directory = base.root / BODIES_DIRECTORY
        try:
            inspected = os.stat(BODIES_DIRECTORY, dir_fd=root_descriptor, follow_symlinks=False)
        except FileNotFoundError:
            handle = _BodyCacheHandle(base, root, root_descriptor, _identity(opened_root), None, None)
        except OSError as error:
            raise _body_cache_error("inspect", directory, error) from error
        else:
            if stat.S_ISLNK(inspected.st_mode) or not stat.S_ISDIR(inspected.st_mode):
                raise BodyCacheError(f"unsafe body cache path: {directory} is not a real directory")
            try:
                descriptor = os.open(BODIES_DIRECTORY, _DIRECTORY_FLAGS, dir_fd=root_descriptor)
            except OSError as error:
                raise _body_cache_error("open", directory, error) from error
            opened = os.fstat(descriptor)
            if _identity(inspected) != _identity(opened):
                os.close(descriptor)
                raise BodyCacheError(f"unsafe body cache path: {directory} changed before it was opened")
            handle = _BodyCacheHandle(
                base,
                root,
                root_descriptor,
                _identity(opened_root),
                descriptor,
                _identity(opened),
            )
        try:
            yield handle
        finally:
            if handle.descriptor is not None:
                os.close(handle.descriptor)
    finally:
        os.close(root_descriptor)


def _verify_body_cache(handle: _BodyCacheHandle, action: str) -> None:
    directory = handle.base.root / BODIES_DIRECTORY
    try:
        current_root = handle.root.stat(follow_symlinks=False)
    except OSError as error:
        raise _body_cache_error("inspect base", handle.root, error) from error
    if _identity(current_root) != handle.root_identity:
        raise BodyCacheError(f"unsafe body cache path: {handle.base.root} changed during {action}")
    try:
        current = os.stat(BODIES_DIRECTORY, dir_fd=handle.root_descriptor, follow_symlinks=False)
    except FileNotFoundError:
        if handle.identity is None:
            return
        raise BodyCacheError(f"unsafe body cache path: {directory} changed during {action}") from None
    except OSError as error:
        raise _body_cache_error("inspect", directory, error) from error
    if (
        handle.identity is None
        or stat.S_ISLNK(current.st_mode)
        or not stat.S_ISDIR(current.st_mode)
        or _identity(current) != handle.identity
    ):
        raise BodyCacheError(f"unsafe body cache path: {directory} changed during {action}")


def _open_body_source(handle: _BodyCacheHandle, name: str) -> _BodySourceHandle | None:
    descriptor = handle.descriptor
    if descriptor is None:
        return None
    path = handle.base.root / BODIES_DIRECTORY / name
    try:
        inspected = os.stat(name, dir_fd=descriptor, follow_symlinks=False)
    except FileNotFoundError:
        return None
    except OSError as error:
        raise _body_cache_error("inspect", path, error) from error
    if stat.S_ISLNK(inspected.st_mode) or not stat.S_ISDIR(inspected.st_mode):
        raise BodyCacheError(f"unsafe body cache path: {path} is not a real directory")
    try:
        source_descriptor = os.open(name, _DIRECTORY_FLAGS, dir_fd=descriptor)
    except OSError as error:
        raise _body_cache_error("open", path, error) from error
    opened = os.fstat(source_descriptor)
    if _identity(inspected) != _identity(opened):
        os.close(source_descriptor)
        raise BodyCacheError(f"unsafe body cache path: {path} changed before it was opened")
    return _BodySourceHandle(name, path, source_descriptor, _identity(opened))


def _verify_body_source(cache: _BodyCacheHandle, source: _BodySourceHandle, action: str) -> None:
    descriptor = cache.descriptor
    if descriptor is None:
        raise BodyCacheError(f"unsafe body cache path: {source.path} changed during {action}")
    try:
        current = os.stat(source.name, dir_fd=descriptor, follow_symlinks=False)
    except OSError as error:
        if isinstance(error, FileNotFoundError):
            raise BodyCacheError(f"unsafe body cache path: {source.path} changed during {action}") from None
        raise _body_cache_error("inspect", source.path, error) from error
    if stat.S_ISLNK(current.st_mode) or not stat.S_ISDIR(current.st_mode):
        raise BodyCacheError(f"unsafe body cache path: {source.path} is not a real directory")
    if _identity(current) != source.identity:
        raise BodyCacheError(f"unsafe body cache path: {source.path} changed during {action}")


def _read_at_limited(descriptor: int, name: str, path: Path, limit: int) -> bytes:
    try:
        file_descriptor = os.open(name, _READ_FLAGS, dir_fd=descriptor)
    except OSError as error:
        raise OSError(f"read {path}: {error}") from error
    try:
        opened = os.fstat(file_descriptor)
        if not stat.S_ISREG(opened.st_mode):
            raise OSError(f"read {path}: not a regular file")
        chunks: list[bytes] = []
        remaining = limit + 1
        while remaining:
            chunk = os.read(file_descriptor, remaining)
            if not chunk:
                break
            chunks.append(chunk)
            remaining -= len(chunk)
        data = b"".join(chunks)
        if len(data) > limit:
            raise FileTooLargeError(f"read {path}: file exceeds {limit} bytes")
        current = os.stat(name, dir_fd=descriptor, follow_symlinks=False)
        if _identity(current) != _identity(opened):
            raise OSError(f"read {path}: file changed while it was being read")
        return data
    finally:
        os.close(file_descriptor)


def _atomic_write_at(descriptor: int, name: str, data: bytes) -> None:
    temporary = f".{name}.{os.getpid()}.{secrets.token_hex(8)}.tmp"
    file_descriptor: int | None = None
    try:
        file_descriptor = os.open(
            temporary,
            os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_CLOEXEC", 0) | getattr(os, "O_NOFOLLOW", 0),
            BASE_FILE_MODE,
            dir_fd=descriptor,
        )
        view = memoryview(data)
        while view:
            written = os.write(file_descriptor, view)
            if written <= 0:
                raise OSError("short write")
            view = view[written:]
        os.fchmod(file_descriptor, BASE_FILE_MODE)
        os.fsync(file_descriptor)
        os.close(file_descriptor)
        file_descriptor = None
        os.replace(temporary, name, src_dir_fd=descriptor, dst_dir_fd=descriptor)
        os.fsync(descriptor)
        temporary = ""
    finally:
        if file_descriptor is not None:
            os.close(file_descriptor)
        if temporary:
            with suppress(FileNotFoundError):
                os.unlink(temporary, dir_fd=descriptor)


def body_cache_relative(source: str, uri: str) -> str:
    digest = hashlib.sha256(uri.encode()).hexdigest()
    return f"{BODIES_DIRECTORY}/{source}/{digest}.txt"


def body_manifest_path(base: Base) -> Path:
    return base.root / BODIES_DIRECTORY / BODY_MANIFEST_FILE


def _missing(error: OSError) -> bool:
    current: BaseException | None = error
    while current is not None:
        if isinstance(current, FileNotFoundError):
            return True
        current = current.__cause__ or current.__context__
    return False


def _object(value: object, label: str) -> Mapping[str, JsonValue]:
    if not isinstance(value, Mapping) or any(not isinstance(key, str) for key in value):
        raise BodyCacheError(f"{label} must be a JSON object")
    return cast(Mapping[str, JsonValue], value)


def _string(value: object, label: str, *, optional: bool = False) -> str:
    if value is None and optional:
        return ""
    if not isinstance(value, str):
        raise BodyCacheError(f"{label} must be a string")
    return value


def _integer(value: object, label: str) -> int:
    if not isinstance(value, JsonNumber) or not value.raw.isascii() or not value.raw.isdigit():
        raise BodyCacheError(f"{label} must be a non-negative integer")
    return int(value.raw)


def _boolean(value: object, label: str) -> bool:
    if not isinstance(value, bool):
        raise BodyCacheError(f"{label} must be a boolean")
    return value


def _validate_entry(base: Base, key: str, entry: BodyManifestEntry) -> None:
    if entry.uri != key or not entry.source or entry.path != body_cache_relative(entry.source, key):
        raise BodyCacheError(f"body manifest entry {key!r} does not match its URI, source, and canonical cache path")
    try:
        validate_source_name(entry.source)
    except ValueError as error:
        raise BodyCacheError(f"body manifest entry {key!r} source: {error}") from error
    if len(entry.sha256) != 64 or any(char not in "0123456789abcdef" for char in entry.sha256):
        raise BodyCacheError(f"body manifest entry {key!r} has invalid digest or size")
    if entry.bytes < 0 or entry.bytes > MAX_NARRATIVE_BYTES:
        raise BodyCacheError(f"body manifest entry {key!r} has invalid digest or size")
    try:
        parse_rfc3339(entry.fetched_at)
    except ValueError as error:
        raise BodyCacheError(f"body manifest entry {key!r} fetched_at: {error}") from error
    validate_within_root(base.root, base.root.joinpath(*PurePosixPath(entry.path).parts))


def _validate_capacity(manifest: BodyManifest) -> None:
    if len(manifest.entries) > MAX_BODY_CACHE_ENTRIES:
        raise BodyCacheError(
            f"body cache has {len(manifest.entries)} entries; limit {MAX_BODY_CACHE_ENTRIES}; "
            "run `fkf build bodies --prune`"
        )
    total = sum(entry.bytes for entry in manifest.entries.values())
    if total > MAX_BODY_CACHE_BYTES:
        raise BodyCacheError(f"body cache exceeds {MAX_BODY_CACHE_BYTES} bytes; run `fkf build bodies --prune`")


def decode_body_manifest(base: Base, data: bytes) -> BodyManifest:
    try:
        raw = _object(loads(data), "body manifest")
    except ValueError as error:
        raise BodyCacheError(f"decode {BODIES_DIRECTORY}/{BODY_MANIFEST_FILE}: {error}") from error
    unknown = set(raw) - _MANIFEST_KEYS
    if unknown:
        raise BodyCacheError(f"body manifest has unknown field {min(unknown)!r}")
    schema_version = _integer(raw.get("schema_version"), "body manifest schema_version")
    if schema_version != BODY_MANIFEST_SCHEMA:
        raise BodyCacheError(f"body manifest schema_version is {schema_version}; expected {BODY_MANIFEST_SCHEMA}")
    # Older empty manifests may omit or null the map; both mean the same rebuildable cache state.
    raw_entries_value = raw.get("entries")
    raw_entries = {} if raw_entries_value is None else _object(raw_entries_value, "body manifest entries")
    entries: dict[str, BodyManifestEntry] = {}
    for key, value in raw_entries.items():
        item = _object(value, f"body manifest entry {key!r}")
        unknown_entry = set(item) - _ENTRY_KEYS
        if unknown_entry:
            raise BodyCacheError(f"body manifest entry {key!r} has unknown field {min(unknown_entry)!r}")
        entry = BodyManifestEntry(
            uri=_string(item.get("uri"), f"entry {key!r}.uri"),
            source=_string(item.get("source"), f"entry {key!r}.source"),
            path=_string(item.get("path"), f"entry {key!r}.path"),
            sha256=_string(item.get("sha256"), f"entry {key!r}.sha256"),
            bytes=_integer(item.get("bytes"), f"entry {key!r}.bytes"),
            provider_modified_at=_string(
                item.get("provider_modified_at"), f"entry {key!r}.provider_modified_at", optional=True
            ),
            fetched_at=_string(item.get("fetched_at"), f"entry {key!r}.fetched_at"),
        )
        _validate_entry(base, key, entry)
        entries[key] = entry
    attempts: dict[str, bool] = {}
    raw_attempts = raw.get("event_attempts")
    if raw_attempts is not None:
        for source, value in _object(raw_attempts, "body manifest event_attempts").items():
            try:
                validate_source_name(source)
            except ValueError as error:
                raise BodyCacheError(f"body manifest event_attempts entry {source!r}: {error}") from error
            attempts[source] = _boolean(value, f"event_attempts.{source}")
    manifest = BodyManifest(schema_version, entries, attempts)
    _validate_capacity(manifest)
    return manifest


def encode_body_manifest(manifest: BodyManifest) -> bytes:
    _validate_capacity(manifest)
    encoded = dumps(manifest, indent=True, newline=True)
    if len(encoded) > MAX_BODY_MANIFEST_BYTES:
        raise BodyCacheError(
            f"body cache manifest is {len(encoded)} bytes; limit {MAX_BODY_MANIFEST_BYTES}; run `fkf build bodies --prune`"
        )
    return encoded


def load_body_manifest(base: Base | _BodyCacheHandle) -> BodyManifest:
    target = base.base if isinstance(base, _BodyCacheHandle) else base
    path = body_manifest_path(target)
    validate_within_root(target.root, path)
    try:
        if isinstance(base, _BodyCacheHandle):
            if base.descriptor is None:
                return BodyManifest()
            data = _read_at_limited(base.descriptor, BODY_MANIFEST_FILE, path, MAX_BODY_MANIFEST_BYTES)
        else:
            data = read_file_limited(path, MAX_BODY_MANIFEST_BYTES)
    except OSError as error:
        if _missing(error):
            return BodyManifest()
        raise
    return decode_body_manifest(target, data)


def write_body_manifest(base: Base | _BodyCacheHandle, manifest: BodyManifest) -> None:
    target = base.base if isinstance(base, _BodyCacheHandle) else base
    path = body_manifest_path(target)
    validate_within_root(target.root, path)
    encoded = encode_body_manifest(manifest)
    if isinstance(base, _BodyCacheHandle):
        if base.descriptor is None:
            raise BodyCacheError(f"unsafe body cache path: {path.parent} changed during manifest publication")
        _verify_body_cache(base, "manifest publication")
        _atomic_write_at(base.descriptor, BODY_MANIFEST_FILE, encoded)
        _verify_body_cache(base, "manifest publication")
        return
    atomic_write(path, encoded, mode=BASE_FILE_MODE)


def read_cached_body_from_manifest(
    base: Base, manifest: BodyManifest, uri: str
) -> tuple[str, BodyManifestEntry | None, bool]:
    entry = manifest.entries.get(uri)
    if entry is None:
        return "", None, False
    absolute = base.root.joinpath(*PurePosixPath(entry.path).parts)
    validate_within_root(base.root, absolute)
    try:
        data = read_file_limited(absolute, MAX_NARRATIVE_BYTES)
    except OSError as error:
        if _missing(error):
            return "", None, False
        raise BodyCacheError(f"read cached body for {uri}: {error}") from error
    digest = hashlib.sha256(data).hexdigest()
    if len(data) != entry.bytes or digest != entry.sha256:
        raise BodyCacheError(f"cached body for {uri} does not match its manifest; run `fkf build bodies --prune`")
    try:
        return data.decode(), entry, True
    except UnicodeDecodeError as error:
        raise BodyCacheError(f"cached body for {uri} is not valid UTF-8; run `fkf build bodies --prune`") from error


def read_cached_body(base: Base, uri: str) -> tuple[str, BodyManifestEntry | None, bool]:
    return read_cached_body_from_manifest(base, load_body_manifest(base), uri)


def body_provider_modified_at(document: Document, record: Record) -> str:
    for name in ("modified", "time"):
        if value := document.fields.eval_string(name, record):
            return value
    return ""


def _fetched_at(now: datetime) -> str:
    if now.tzinfo is None or now.utcoffset() is None:
        now = now.replace(tzinfo=UTC)
    return format_rfc3339(Instant.from_datetime(now))


def cache_body(base: Base, document: Document, record: Record, uri: str, body: str) -> BodyManifestEntry:
    data = body.encode()
    if len(data) > MAX_NARRATIVE_BYTES:
        raise FileTooLargeError(f"body for {uri} is {len(data)} bytes; limit {MAX_NARRATIVE_BYTES}")
    manifest = load_body_manifest(base)
    relative = body_cache_relative(document.source, uri)
    absolute = base.root.joinpath(*PurePosixPath(relative).parts)
    validate_within_root(base.root, absolute)
    entry = BodyManifestEntry(
        uri=uri,
        source=document.source,
        path=relative,
        sha256=hashlib.sha256(data).hexdigest(),
        bytes=len(data),
        provider_modified_at=body_provider_modified_at(document, record),
        fetched_at=_fetched_at(base.now()),
    )
    manifest.entries[uri] = entry
    encode_body_manifest(manifest)
    atomic_write(absolute, data, mode=BASE_FILE_MODE)
    write_body_manifest(base, manifest)
    return entry


def fetch_body(
    base: Base,
    document: Document,
    record: Record,
    *,
    cancel: Cancellation | None = None,
) -> str:
    """Execute the currently trusted opaque body argv for one stored record."""
    source = base.source(document.source)
    if not source.enabled:
        raise ConfigError(f"source {source.name} is disabled; enable and re-trust it before fetching a body")
    require_trust(base.config, cancel=cancel)
    timeout = source.timeout if source.timeout > 0 else base.config.sync.timeout
    command = build_body_command(source, document.fields, base.environment, record, timeout)
    command = replace(command, max_output_bytes=MAX_NARRATIVE_BYTES)
    output = base.runner.run(command, cancel=cancel).stdout
    check_cancel(cancel)
    try:
        return output.decode()
    except UnicodeDecodeError as error:
        raise BodyCacheError(f"body for source {source.name} is not valid UTF-8") from error


def read_or_fetch_body(
    base: Base,
    document: Document,
    record: Record,
    uri: str,
    *,
    cancel: Cancellation | None = None,
) -> tuple[str, str]:
    """Return body text and one of cached/fetched after explicit online authorization."""
    source = base.source(document.source)
    if not source.has_body():
        state = "no-longer-declared" if document.body else "never"
        raise ConfigError(f"source {source.name} declares no body: command ({state})")
    if source.caches_bodies():
        body, _entry, found = read_cached_body(base, uri)
        if found:
            return body, "cached"
    body = fetch_body(base, document, record, cancel=cancel)
    if source.bodies in {BodyPolicy.CACHE, BodyPolicy.SYNC}:
        cache_body(base, document, record, uri, body)
    return body, "fetched"


def prune_bodies(
    base: Base,
    *,
    source: str = "",
    older_than: timedelta = timedelta(0),
    cancel: Cancellation | None = None,
) -> BodiesBuildReport:
    """Prune the entire cache or a manifest-selected subset without following links."""
    if older_than < timedelta(0):
        raise ValueError("older-than duration must not be negative")
    check_cancel(cancel)
    directory = base.root / BODIES_DIRECTORY
    validate_within_root(base.root, directory)
    with _open_body_cache(base) as cache:
        return _prune_open_body_cache(cache, source=source, older_than=older_than, cancel=cancel)


def _prune_open_body_cache(
    cache: _BodyCacheHandle,
    *,
    source: str,
    older_than: timedelta,
    cancel: Cancellation | None,
) -> BodiesBuildReport:
    base = cache.base
    try:
        manifest = load_body_manifest(cache)
    except Exception:
        if source or older_than > timedelta(0):
            raise
        # A full prune is the recovery path named by every rebuildable-cache failure.
        # Selective deletion still needs a readable manifest to identify safe targets.
        check_cancel(cancel)
        _remove_body_cache(cache)
        return BodiesBuildReport(0, 0, "body cache is empty; invalid manifest discarded")
    if source:
        check_cancel(cancel)
        known = tuple(
            sorted(
                set(base.config.source_names())
                | {entry.source for entry in manifest.entries.values()}
                | set(manifest.event_attempts)
            )
        )
        if source not in known:
            raise ConfigError(f"unknown source {source!r}; this base declares {', '.join(known) if known else 'none'}")
    if not source and older_than == timedelta(0):
        report = BodiesBuildReport(
            len(manifest.entries),
            sum(entry.bytes for entry in manifest.entries.values()),
            _prune_message(len(manifest.entries), 0),
        )
        check_cancel(cancel)
        _remove_body_cache(cache)
        return report

    cutoff = base.now() - older_than
    selected: list[tuple[str, BodyManifestEntry]] = []
    for uri, entry in manifest.entries.items():
        check_cancel(cancel)
        if source and entry.source != source:
            continue
        try:
            fetched = parse_rfc3339(entry.fetched_at).to_datetime()
        except ValueError as error:
            raise BodyCacheError(f"body manifest entry {uri!r} fetched_at: {error}") from error
        if older_than > timedelta(0) and fetched > cutoff.astimezone(UTC):
            continue
        selected.append((uri, entry))

    check_cancel(cancel)
    affected_sources = {entry.source for _uri, entry in selected}
    for uri, _entry in selected:
        del manifest.entries[uri]

    attempts_changed = False
    # A source-only prune deliberately rearms that source. With an age filter, a no-op must
    # leave the marker untouched; otherwise an "unchanged" prune schedules a provider fetch.
    if source and (older_than == timedelta(0) or source in affected_sources):
        attempts_changed = source in manifest.event_attempts
        manifest.event_attempts.pop(source, None)

    remaining_sources = {entry.source for entry in manifest.entries.values()}
    for affected in affected_sources:
        if affected not in remaining_sources and affected in manifest.event_attempts:
            del manifest.event_attempts[affected]
            attempts_changed = True

    if not selected and not attempts_changed:
        return BodiesBuildReport(0, 0, _prune_message(0, len(manifest.entries)))
    # Publish the new manifest as one logical mutation; orphan cleanup after this point is safe and rebuildable.
    check_cancel(cancel)
    if not (manifest.entries or manifest.event_attempts):
        _remove_body_cache(cache)
    else:
        _remove_selected_bodies(cache, manifest, selected)
    pruned = len(selected)
    reclaimed = sum(entry.bytes for _, entry in selected)
    return BodiesBuildReport(pruned, reclaimed, _prune_message(pruned, len(manifest.entries)))


def _remove_body_cache(cache: _BodyCacheHandle) -> None:
    directory = cache.base.root / BODIES_DIRECTORY
    _verify_body_cache(cache, "removal")
    descriptor = cache.descriptor
    if descriptor is None:
        return
    try:
        _clear_body_cache_directory(cache, descriptor, directory)
        _verify_body_cache(cache, "removal")
        os.rmdir(BODIES_DIRECTORY, dir_fd=cache.root_descriptor)
    except BodyCacheError:
        raise
    except OSError as error:
        raise BodyCacheError(f"remove body cache directory {directory}: {error}") from error


def _clear_body_cache_directory(cache: _BodyCacheHandle, descriptor: int, directory: Path) -> None:
    """Remove entries only through one already-open cache directory."""

    try:
        iterator = os.scandir(descriptor)
    except OSError as error:
        raise BodyCacheError(f"remove body cache directory {directory}: {error}") from error
    with iterator:
        for entry in iterator:
            try:
                inspected = os.stat(entry.name, dir_fd=descriptor, follow_symlinks=False)
            except FileNotFoundError:
                continue
            except OSError as error:
                raise BodyCacheError(f"remove body cache entry {directory / entry.name}: {error}") from error
            if stat.S_ISDIR(inspected.st_mode):
                child = _open_child_cache_directory(descriptor, entry.name, directory / entry.name, inspected)
                try:
                    _clear_body_cache_directory(cache, child, directory / entry.name)
                    current = os.stat(entry.name, dir_fd=descriptor, follow_symlinks=False)
                    if _identity(current) != _identity(inspected):
                        raise BodyCacheError(f"unsafe body cache path: {directory / entry.name} changed during removal")
                    os.rmdir(entry.name, dir_fd=descriptor)
                except FileNotFoundError as error:
                    raise BodyCacheError(
                        f"unsafe body cache path: {directory / entry.name} changed during removal"
                    ) from error
                except BodyCacheError:
                    raise
                except OSError as error:
                    raise BodyCacheError(f"remove body cache entry {directory / entry.name}: {error}") from error
                finally:
                    os.close(child)
            else:
                try:
                    os.unlink(entry.name, dir_fd=descriptor)
                except FileNotFoundError:
                    continue
                except OSError as error:
                    raise BodyCacheError(f"remove body cache entry {directory / entry.name}: {error}") from error
            if descriptor == cache.descriptor:
                _verify_body_cache(cache, "removal")


def _open_child_cache_directory(
    descriptor: int,
    name: str,
    path: Path,
    inspected: os.stat_result,
) -> int:
    try:
        child = os.open(name, _DIRECTORY_FLAGS, dir_fd=descriptor)
    except OSError as error:
        raise _body_cache_error("open", path, error) from error
    opened = os.fstat(child)
    if _identity(inspected) != _identity(opened):
        os.close(child)
        raise BodyCacheError(f"unsafe body cache path: {path} changed before it was opened")
    return child


def _remove_selected_bodies(
    cache: _BodyCacheHandle,
    manifest: BodyManifest,
    selected: list[tuple[str, BodyManifestEntry]],
) -> None:
    sources: dict[str, _BodySourceHandle | None] = {}
    try:
        for name in sorted({entry.source for _uri, entry in selected}):
            sources[name] = _open_body_source(cache, name)
        _verify_body_cache(cache, "manifest publication")
        for source in sources.values():
            if source is not None:
                _verify_body_source(cache, source, "manifest publication")
        write_body_manifest(cache, manifest)
        for source in sources.values():
            if source is not None:
                _verify_body_source(cache, source, "removal")
        for _uri, entry in selected:
            bound = sources[entry.source]
            if bound is not None:
                _remove_body_cache_entry(cache, bound, entry.path)
        # Empty source directories are harmless. Keeping them avoids a final
        # pathname-based rmdir that cannot atomically compare the target inode.
        _verify_body_cache(cache, "removal")
    finally:
        for source in sources.values():
            if source is not None:
                os.close(source.descriptor)


def _remove_body_cache_entry(cache: _BodyCacheHandle, source: _BodySourceHandle, relative: str) -> None:
    """Unlink one manifest-owned body through its pre-publication source directory."""

    parts = PurePosixPath(relative).parts
    if len(parts) != 3 or parts[:2] != (BODIES_DIRECTORY, source.name):
        raise BodyCacheError(f"unsafe body cache path: {relative} is not a canonical body path")
    _verify_body_source(cache, source, "removal")
    try:
        os.unlink(parts[2], dir_fd=source.descriptor)
    except FileNotFoundError:
        return
    except OSError as error:
        raise BodyCacheError(f"remove cached body {relative}: {error}") from error
    _verify_body_source(cache, source, "removal")


def _prune_message(pruned: int, remaining: int) -> str:
    if pruned == 0:
        return f"body cache unchanged; {remaining} entr{'y' if remaining == 1 else 'ies'} remain"
    return f"pruned {pruned} body entr{'y' if pruned == 1 else 'ies'}; {remaining} remain"


__all__ = [
    "BODIES_DIRECTORY",
    "BODY_MANIFEST_FILE",
    "BODY_MANIFEST_SCHEMA",
    "BodiesBuildReport",
    "BodyCacheError",
    "BodyManifest",
    "BodyManifestEntry",
    "body_cache_relative",
    "body_manifest_path",
    "body_provider_modified_at",
    "cache_body",
    "decode_body_manifest",
    "encode_body_manifest",
    "fetch_body",
    "load_body_manifest",
    "prune_bodies",
    "read_cached_body",
    "read_cached_body_from_manifest",
    "read_or_fetch_body",
    "write_body_manifest",
]
