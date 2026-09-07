"""Wheel-safe access to FKF's exact bundled presets, helpers, skills, and evals."""

from __future__ import annotations

import hashlib
import struct
from collections.abc import Mapping
from importlib import resources
from importlib.resources.abc import Traversable
from pathlib import PurePosixPath
from typing import Final

PRESETS: Final = ("minimal", "personal", "team")
BUNDLED_SKILLS: Final = ("fkf-use", "fkf-learn", "daily-brief")
HOOK_SCRIPT: Final = "fkf-hook.py"
DEMO_HELPER: Final = "fkf-demo-json.sh"
_PYTHON_BYTECODE_SUFFIXES: Final = frozenset({".pyc", ".pyo"})


def asset_root() -> Traversable:
    """Return the package resource tree without assuming a filesystem install."""
    return resources.files("fkf").joinpath("assets_data")


def _parts(relative: str) -> tuple[str, ...]:
    path = PurePosixPath(relative)
    if not relative or path.is_absolute() or any(part in {"", ".", ".."} for part in path.parts):
        raise ValueError(f"invalid bundled asset path {relative!r}")
    return path.parts


def asset(relative: str) -> Traversable:
    """Resolve one confined package resource."""
    return asset_root().joinpath(*_parts(relative))


def read_asset(relative: str) -> bytes:
    """Read one exact bundled file from a source tree, wheel, or zip import."""
    target = asset(relative)
    if not target.is_file():
        raise FileNotFoundError(f"bundled asset {relative!r} is missing or not a file")
    return target.read_bytes()


def _walk_files(root: Traversable, prefix: str = "") -> list[tuple[str, bytes]]:
    files: list[tuple[str, bytes]] = []
    for entry in sorted(root.iterdir(), key=lambda item: item.name):
        relative = f"{prefix}/{entry.name}" if prefix else entry.name
        if entry.is_dir():
            # Editable installs expose ignored interpreter caches that wheels and sdists omit.
            if entry.name == "__pycache__":
                continue
            files.extend(_walk_files(entry, relative))
        elif entry.is_file():
            if PurePosixPath(entry.name).suffix in _PYTHON_BYTECODE_SUFFIXES:
                continue
            files.append((relative, entry.read_bytes()))
        else:
            raise ValueError(f"bundled asset {relative!r} is neither a regular file nor a directory")
    return files


def asset_files(relative: str) -> tuple[tuple[str, bytes], ...]:
    """Return all files below one bundled directory in stable relative-path order."""
    root = asset(relative)
    if not root.is_dir():
        raise FileNotFoundError(f"bundled asset directory {relative!r} is missing")
    return tuple(_walk_files(root))


def digest_tree(files: Mapping[str, bytes] | Traversable) -> str:
    """Hash a tree with FKF's injective path/content framing."""
    entries = _walk_files(files) if isinstance(files, Traversable) else sorted(files.items())
    digest = hashlib.sha256()
    for relative, content in entries:
        normalized = PurePosixPath(relative).as_posix()
        encoded = normalized.encode()
        digest.update(b"P")
        digest.update(struct.pack(">Q", len(encoded)))
        digest.update(encoded)
        digest.update(b"C")
        digest.update(struct.pack(">Q", len(content)))
        digest.update(content)
    return digest.hexdigest()


def skill_digest(name: str) -> str:
    """Return the exact digest of one closed-vocabulary bundled skill."""
    if name not in BUNDLED_SKILLS:
        raise ValueError(f"unknown bundled skill {name!r}")
    target = asset(f"skills/{name}")
    if not target.is_dir():
        raise FileNotFoundError(f"bundled skill {name!r} is missing")
    return digest_tree(target)


def shipped_helpers() -> dict[str, bytes]:
    """Load every official helper by basename in deterministic order."""
    helpers = dict(asset_files("presets/bin"))
    if HOOK_SCRIPT not in helpers:
        raise FileNotFoundError(f"bundled helper {HOOK_SCRIPT!r} is missing")
    return helpers


__all__ = [
    "BUNDLED_SKILLS",
    "DEMO_HELPER",
    "HOOK_SCRIPT",
    "PRESETS",
    "asset",
    "asset_files",
    "asset_root",
    "digest_tree",
    "read_asset",
    "shipped_helpers",
    "skill_digest",
]
