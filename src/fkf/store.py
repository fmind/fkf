"""Published base paths, lexical addressing, and symlink confinement."""

from __future__ import annotations

import os
import posixpath
import re
import stat
from dataclasses import dataclass, field
from datetime import date
from enum import StrEnum
from pathlib import Path, PurePosixPath
from typing import Final

from fkf.errors import InvalidUsageError, OperationalError

BASE_DIR_MODE: Final = 0o700
BASE_FILE_MODE: Final = 0o600

GRAPH_FILE: Final = "graph.tsv"
GRAPH_DST_FILE: Final = "graph.dst.tsv"
GRAPH_OFFSETS_FILE: Final = "graph.offsets.tsv"
GRAPH_META_FILE: Final = "graph.meta.json"
GRAPH_GENERATION_FILE: Final = "graph.generation.json"
TASK_TRACE_FILE: Final = "TASKS.md"
BASE_AGENTS_FILE: Final = "AGENTS.md"
BASE_SKILLS_DIR: Final = ".agents/skills"
BASE_SOURCES_DIR: Final = "sources"
BASE_TESTS_DIR: Final = "tests"
BASE_CLIENTS_DIR: Final = "clients"
CONFIG_FILE_NAME: Final = "fkf.yaml"
LOCAL_CONFIG_NAME: Final = "fkf.local.yaml"
MARKDOWN_EXTENSION: Final = ".md"
BASE_ENV_VAR: Final = "FKF_BASE"
MAX_CONFIG_BYTES: Final = 1 << 20
MAX_CONTROL_FILE_BYTES: Final = 1 << 20
MAX_SOURCE_DOCUMENT_BYTES: Final = 64 << 20
MAX_LOCAL_INPUT_BYTES: Final = 64 << 20
MAX_NARRATIVE_BYTES: Final = 4 << 20

_PAGE_SLUG_PATTERN = re.compile(r"^[a-z0-9][a-z0-9._-]*$")
_SOURCE_NAME_PATTERN = re.compile(r"^[a-z0-9][a-z0-9-]*$")


class PathEscapesError(InvalidUsageError):
    """A purported base-relative path escapes its selected root."""


class UnsafePathError(OperationalError):
    """A path contains a symlink or unsafe filesystem entry."""


class NotAddressableError(InvalidUsageError):
    """A path is outside FKF's published resource grammar."""


class LayerDisabledError(InvalidUsageError):
    """A request addresses a layer disabled by this base."""

    def __init__(self, layer: Layer) -> None:
        self.layer = layer
        super().__init__(f"layer {layer} is disabled in {CONFIG_FILE_NAME}; set layers.{layer}: true to enable it")


class NoBaseError(InvalidUsageError):
    """No explicit, environment, or discovered base exists."""


class Layer(StrEnum):
    """One typed storage layer in a base."""

    EVENTS = "events"
    INDEX = "index"
    TASKS = "tasks"
    PROJECTS = "projects"
    WIKI = "wiki"


LAYERS: Final[tuple[Layer, ...]] = tuple(Layer)


def layer_names() -> str:
    """Render the canonical layer list."""
    return ", ".join(LAYERS)


def parse_layer(value: str) -> Layer:
    """Parse one case-insensitive layer name."""
    candidate = value.strip().lower()
    try:
        return Layer(candidate)
    except ValueError as error:
        raise ValueError(f"unknown layer {value!r}; valid layers: {layer_names()}") from error


def expand_home(value: str) -> str:
    """Expand exactly ``~`` and ``~/`` using this process's HOME."""
    cleaned = value.strip()
    if cleaned != "~" and not cleaned.startswith("~/"):
        return cleaned
    home = os.environ.get("HOME", "").strip()
    if not home:
        return cleaned
    return home if cleaned == "~" else str(Path(home) / cleaned[2:])


def resolve_absolute_path(value: str | os.PathLike[str]) -> Path:
    """Anchor a path without resolving its symlinks."""
    expanded = expand_home(os.fspath(value))
    if not expanded:
        raise ValueError("path is empty")
    # ``absolute`` preserves the chosen symlink spelling; trust identity depends on it.
    return Path(os.path.normpath(expanded)).absolute()


def resolve_physical_path(value: str | os.PathLike[str]) -> Path:
    """Resolve existing aliases while preserving a missing suffix."""
    current = resolve_absolute_path(value)
    missing: list[str] = []
    while True:
        try:
            resolved = current.resolve(strict=True)
        except FileNotFoundError:
            parent = current.parent
            if parent == current:
                raise OSError(f"resolve physical path {value}: no existing ancestor") from None
            missing.append(current.name)
            current = parent
            continue
        except OSError as error:
            raise OSError(f"resolve physical path {value}: {error}") from error
        for component in reversed(missing):
            resolved /= component
        return Path(os.path.normpath(resolved))


def clean_relative(relative: str) -> str:
    """Normalize a slash path and reject every spelling that can escape."""
    value = relative.strip()
    if not value:
        raise PathEscapesError("path escapes the base: path is empty")
    if "\0" in value or "\\" in value:
        raise PathEscapesError(f"path escapes the base: {value!r} contains an unsafe character")
    if value.startswith("/"):
        raise PathEscapesError(f"path escapes the base: {value!r} is absolute")
    if value.startswith("~"):
        raise PathEscapesError(f"path escapes the base: {value!r} is home-relative")
    trailing_slash = value.endswith("/")
    cleaned = posixpath.normpath(value)
    if cleaned == ".." or cleaned.startswith("../"):
        raise PathEscapesError(f"path escapes the base: {value!r}")
    # PurePosixPath also rejects nothing itself, so pin FKF's filesystem-neutral grammar.
    if any(part in {"", ".", ".."} for part in PurePosixPath(cleaned).parts):
        raise PathEscapesError(f"path escapes the base: {value!r} is not a valid path")
    return f"{cleaned}/" if trailing_slash and cleaned != "." else cleaned


def validate_date(value: str) -> None:
    """Require the exact YYYY-MM-DD shape, allowing an absent bound."""
    if not value.strip():
        return
    try:
        parsed = date.fromisoformat(value)
    except ValueError as error:
        raise ValueError(f"date must be YYYY-MM-DD: {error}") from error
    if parsed.isoformat() != value:
        raise ValueError("date must be YYYY-MM-DD")


def _root_alias_resolved(path: Path) -> Path:
    """Resolve only an OS-owned first component such as macOS ``/var``."""
    parts = path.parts
    if len(parts) < 2 or not path.is_absolute():
        return path
    root_entry = Path(parts[0], parts[1])
    try:
        entry = root_entry.lstat()
    except FileNotFoundError:
        return path
    except OSError as error:
        raise UnsafePathError(f"unsafe filesystem path: inspect {root_entry}: {error}") from error
    if not stat.S_ISLNK(entry.st_mode):
        return path
    try:
        resolved = root_entry.resolve(strict=True)
    except OSError as error:
        raise UnsafePathError(f"unsafe filesystem path: resolve system root alias {root_entry}: {error}") from error
    return resolved.joinpath(*parts[2:])


def _validate_path_confinement(path: str | os.PathLike[str], *, directory_leaf: bool) -> None:
    # Do not resolve symlinks before the component-by-component audit below.
    candidate = Path(os.path.normpath(expand_home(os.fspath(path)))).absolute()
    if os.fspath(path).strip() in {"", "."}:
        raise UnsafePathError("unsafe filesystem path: path is empty")
    candidate = _root_alias_resolved(candidate)
    current = Path(candidate.anchor)
    for component in candidate.parts[1:]:
        current /= component
        try:
            info = current.lstat()
        except FileNotFoundError:
            return
        except OSError as error:
            raise UnsafePathError(f"unsafe filesystem path: inspect {current}: {error}") from error
        if stat.S_ISLNK(info.st_mode):
            raise UnsafePathError(f"unsafe filesystem path: {current} is a symlink")
        if (current != candidate or directory_leaf) and not stat.S_ISDIR(info.st_mode):
            raise UnsafePathError(f"unsafe filesystem path: intermediate component {current} is not a directory")


def validate_path_confinement(path: str | os.PathLike[str]) -> None:
    """Reject existing symlinks and non-directory intermediate entries."""
    _validate_path_confinement(path, directory_leaf=False)


def validate_directory_confinement(path: str | os.PathLike[str]) -> None:
    """Also require an existing leaf to be a real directory."""
    _validate_path_confinement(path, directory_leaf=True)


def validate_within_root(root: str | os.PathLike[str], absolute: str | os.PathLike[str]) -> None:
    """Reject lexical escapes and every symlink strictly below a selected root."""
    base = Path(os.path.normpath(root))
    target = Path(os.path.normpath(absolute))
    try:
        relative = target.relative_to(base)
    except ValueError as error:
        raise PathEscapesError(f"path escapes the base: {target} is outside {base}") from error
    if relative == Path():
        return
    current = base
    for component in relative.parts:
        current /= component
        try:
            info = current.lstat()
        except FileNotFoundError:
            return
        except OSError as error:
            raise UnsafePathError(f"unsafe filesystem path: inspect {current}: {error}") from error
        if stat.S_ISLNK(info.st_mode):
            raise UnsafePathError(
                f"unsafe filesystem path: {current} is a symlink inside the base; a base addresses only its own files"
            )


def _valid_address_date(value: str) -> bool:
    try:
        validate_date(value)
    except ValueError:
        return False
    return bool(value)


def _addressable_source_document(name: str) -> bool:
    return name.endswith(".json") and bool(_SOURCE_NAME_PATTERN.fullmatch(name[:-5]))


def _addressable_layer_path(layer: Layer, relative: str) -> bool:
    parts = relative.split("/")
    if len(parts) == 1:
        return True
    if layer is Layer.EVENTS:
        if len(parts) == 2:
            return _valid_address_date(parts[1])
        return len(parts) == 3 and _valid_address_date(parts[1]) and _addressable_source_document(parts[2])
    if layer is Layer.INDEX:
        return len(parts) == 2 and _addressable_source_document(parts[1])
    if layer is Layer.TASKS:
        if len(parts) == 2:
            return _valid_address_date(parts[1])
        if len(parts) == 3:
            return _valid_address_date(parts[1]) and bool(_PAGE_SLUG_PATTERN.fullmatch(parts[2]))
        return (
            len(parts) == 4
            and _valid_address_date(parts[1])
            and bool(_PAGE_SLUG_PATTERN.fullmatch(parts[2]))
            and parts[3] == TASK_TRACE_FILE
        )
    if layer in {Layer.PROJECTS, Layer.WIKI}:
        return (
            len(parts) == 2
            and parts[1].endswith(MARKDOWN_EXTENSION)
            and bool(_PAGE_SLUG_PATTERN.fullmatch(parts[1][: -len(MARKDOWN_EXTENSION)]))
        )
    return False


def addressable_base_path(relative: str) -> bool:
    """Return whether a clean relative path belongs to the public grammar."""
    cleaned = relative.removesuffix("/")
    first = cleaned.partition("/")[0]
    try:
        layer = Layer(first)
    except ValueError:
        return cleaned in {
            GRAPH_FILE,
            GRAPH_DST_FILE,
            GRAPH_OFFSETS_FILE,
            GRAPH_META_FILE,
            GRAPH_GENERATION_FILE,
            CONFIG_FILE_NAME,
            BASE_AGENTS_FILE,
        }
    return _addressable_layer_path(layer, cleaned)


@dataclass(frozen=True, slots=True)
class Store:
    """Resolved immutable layout of one base."""

    root: Path
    _enabled: dict[Layer, bool] = field(repr=False)

    def __init__(self, root: str | os.PathLike[str], enabled: dict[Layer, bool] | None = None) -> None:
        object.__setattr__(self, "root", Path(os.path.normpath(expand_home(os.fspath(root)))))
        declared = enabled or {}
        object.__setattr__(self, "_enabled", {layer: bool(declared.get(layer, False)) for layer in LAYERS})

    def enabled(self, layer: Layer) -> bool:
        return self._enabled[layer]

    @property
    def enabled_layers(self) -> tuple[Layer, ...]:
        return tuple(layer for layer in LAYERS if self._enabled[layer])

    def directory(self, layer: Layer) -> Path:
        return self.resolve(layer)

    @staticmethod
    def layer_of(relative: str) -> Layer | None:
        first = relative.removeprefix("/").partition("/")[0]
        try:
            return Layer(first)
        except ValueError:
            return None

    def resolve(self, relative: str | Layer) -> Path:
        cleaned = clean_relative(str(relative)).removesuffix("/")
        if cleaned == ".":
            return self.root
        layer = self.layer_of(cleaned)
        if layer is not None and not self.enabled(layer):
            raise LayerDisabledError(layer)
        if not addressable_base_path(cleaned):
            raise NotAddressableError(
                f"path is not addressable in a base: {cleaned} "
                f"(a base addresses the published shapes under the {layer_names()} layers)"
            )
        absolute = self.root.joinpath(*cleaned.split("/"))
        validate_within_root(self.root, absolute)
        return absolute

    def relative(self, absolute: str | os.PathLike[str]) -> str:
        target = Path(os.path.normpath(absolute))
        try:
            return target.relative_to(self.root).as_posix()
        except ValueError as error:
            raise PathEscapesError(f"path escapes the base: {target} is outside {self.root}") from error

    @property
    def config_path(self) -> Path:
        return self.root / CONFIG_FILE_NAME

    @property
    def local_config_path(self) -> Path:
        return self.root / LOCAL_CONFIG_NAME

    @property
    def sources_dir(self) -> Path:
        return self.root / BASE_SOURCES_DIR

    @property
    def clients_dir(self) -> Path:
        return self.root / BASE_CLIENTS_DIR

    @property
    def tests_dir(self) -> Path:
        return self.root / BASE_TESTS_DIR

    @property
    def versioned(self) -> bool:
        marker = self.root / ".git"
        try:
            info = marker.lstat()
        except OSError:
            return False
        if stat.S_ISLNK(info.st_mode):
            return False
        if stat.S_ISDIR(info.st_mode):
            return _has_git_head(marker)
        if not stat.S_ISREG(info.st_mode):
            return False
        try:
            line = _read_small_regular(marker).decode().strip()
        except OSError, UnicodeError:
            return False
        if not line.startswith("gitdir:") or "\n" in line or "\r" in line:
            return False
        git_dir_value = line.removeprefix("gitdir:").strip()
        if not git_dir_value:
            return False
        git_dir = Path(git_dir_value)
        if not git_dir.is_absolute():
            git_dir = self.root / git_dir
        return _has_git_head(Path(os.path.normpath(git_dir)))

    @property
    def enforce_permissions(self) -> bool:
        return not self.versioned


def _read_small_regular(path: Path) -> bytes:
    before = path.lstat()
    if stat.S_ISLNK(before.st_mode) or not stat.S_ISREG(before.st_mode) or before.st_size > MAX_CONTROL_FILE_BYTES:
        raise UnsafePathError(f"unsafe filesystem path: {path} must be a bounded regular non-symlink file")
    with path.open("rb") as handle:
        after = os.fstat(handle.fileno())
        if not stat.S_ISREG(after.st_mode) or (before.st_dev, before.st_ino) != (after.st_dev, after.st_ino):
            raise UnsafePathError(f"unsafe filesystem path: {path} changed before it was opened")
        return handle.read(MAX_CONTROL_FILE_BYTES + 1)


def _has_git_head(git_dir: Path) -> bool:
    try:
        directory = git_dir.lstat()
        head = (git_dir / "HEAD").lstat()
    except OSError:
        return False
    return (
        stat.S_ISDIR(directory.st_mode)
        and not stat.S_ISLNK(directory.st_mode)
        and stat.S_ISREG(head.st_mode)
        and not stat.S_ISLNK(head.st_mode)
    )


def discover_base(explicit: str) -> tuple[Path, str]:
    """Select a base from flag, environment, then nearest ancestor."""
    if expanded := expand_home(explicit):
        try:
            return resolve_absolute_path(expanded), "flag"
        except ValueError as error:
            raise NoBaseError(f"no fkf base found: resolve --base {explicit!r}: {error}") from error
    environment = os.environ.get(BASE_ENV_VAR, "")
    if expanded := expand_home(environment):
        try:
            return resolve_absolute_path(expanded), "environment"
        except ValueError as error:
            raise NoBaseError(f"no fkf base found: resolve {BASE_ENV_VAR}={environment!r}: {error}") from error
    working = Path.cwd()
    for directory in (working, *working.parents):
        config = directory / CONFIG_FILE_NAME
        try:
            info = config.stat()
        except OSError:
            continue
        if stat.S_ISREG(info.st_mode):
            return directory, "discovery"
    raise NoBaseError(
        f"no fkf base found: pass --base <path>, export {BASE_ENV_VAR}, or run from inside a base "
        f"(a directory holding {CONFIG_FILE_NAME}, found by walking up from {working})"
    )


__all__ = [
    "BASE_AGENTS_FILE",
    "BASE_CLIENTS_DIR",
    "BASE_DIR_MODE",
    "BASE_ENV_VAR",
    "BASE_FILE_MODE",
    "BASE_SKILLS_DIR",
    "BASE_SOURCES_DIR",
    "BASE_TESTS_DIR",
    "CONFIG_FILE_NAME",
    "GRAPH_DST_FILE",
    "GRAPH_FILE",
    "GRAPH_GENERATION_FILE",
    "GRAPH_META_FILE",
    "GRAPH_OFFSETS_FILE",
    "LAYERS",
    "LOCAL_CONFIG_NAME",
    "Layer",
    "LayerDisabledError",
    "NoBaseError",
    "NotAddressableError",
    "PathEscapesError",
    "Store",
    "UnsafePathError",
    "addressable_base_path",
    "clean_relative",
    "discover_base",
    "expand_home",
    "layer_names",
    "parse_layer",
    "resolve_absolute_path",
    "resolve_physical_path",
    "validate_date",
    "validate_directory_confinement",
    "validate_path_confinement",
    "validate_within_root",
]
