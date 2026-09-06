#!/usr/bin/env python3
"""Project bounded, declared repository metadata without executing repository code."""

from __future__ import annotations

import configparser
import hashlib
import json
import os
import re
import sys
import urllib.parse
from pathlib import Path
from typing import Any

import tomllib

MAX_ROOTS = 16
MAX_REPOSITORIES = 256
MAX_FILE_BYTES = 256 << 10
MAX_INPUT_BYTES = 4 << 20
MAX_OUTPUT_BYTES = 4 << 20
TASK_NAMES = ("build", "test")
INSTRUCTION_PATHS = (
    "AGENTS.md",
    "CLAUDE.md",
    "GEMINI.md",
    ".github/copilot-instructions.md",
)
MANIFESTS = ("go.mod", "pyproject.toml", "package.json", "mise.toml")
SAFE_HOST = re.compile(r"^[a-z0-9](?:[a-z0-9.-]*[a-z0-9])?$")
SAFE_PATH = re.compile(r"^[A-Za-z0-9._~+/-]+$")
DYNAMIC = re.compile(r"\$\{|\{\{|`|\$\(")


class FactsError(RuntimeError):
    """One unsafe or excessive input invalidates the complete snapshot."""


def fail(message: str, code: int = 1) -> int:
    print(f"repository-facts.py: {message}", file=sys.stderr)
    return code


def read_small(path: Path, budget: list[int]) -> bytes:
    if path.is_symlink() or not path.is_file():
        raise FactsError(f"metadata path is not a regular file: {path.name}")
    size = path.stat(follow_symlinks=False).st_size
    if size > MAX_FILE_BYTES:
        raise FactsError(f"metadata file exceeds {MAX_FILE_BYTES} bytes: {path.name}")
    budget[0] += size
    if budget[0] > MAX_INPUT_BYTES:
        raise FactsError(f"metadata input exceeds {MAX_INPUT_BYTES} bytes")
    return path.read_bytes()


def repository_candidates(root: Path) -> list[tuple[Path, str]]:
    if root.is_symlink() or not root.is_dir():
        raise FactsError(f"root must be a real directory: {root}")
    if os.path.realpath(root) != os.path.abspath(root):
        raise FactsError(f"root path traverses a symlink: {root}")
    if (root / ".git").exists() and not (root / ".git").is_dir():
        raise FactsError(f"repository control path is not a directory: {root}/.git")
    if (root / ".git").is_dir() and not (root / ".git").is_symlink():
        return [(root, ".")]
    candidates: list[tuple[Path, str]] = []
    try:
        entries = sorted(os.scandir(root), key=lambda entry: os.fsencode(entry.name))
    except OSError as error:
        raise FactsError(f"cannot enumerate root: {root}") from error
    for entry in entries:
        if entry.is_symlink() or not entry.is_dir(follow_symlinks=False):
            continue
        path = Path(entry.path)
        git_dir = path / ".git"
        if git_dir.is_symlink():
            raise FactsError(f"repository control path is a symlink: {entry.name}/.git")
        if git_dir.exists() and not git_dir.is_dir():
            raise FactsError(f"repository control path is not a directory: {entry.name}/.git")
        if git_dir.is_dir():
            candidates.append((path, entry.name))
    return candidates


def sanitize_remote(value: str) -> tuple[str, str] | None:
    raw = value.strip()
    if not raw or any(ord(char) < 32 or ord(char) == 127 for char in raw):
        return None
    if "://" not in raw and re.fullmatch(r"[^@/:\s]+@[^/:\s]+:[^\s]+", raw):
        _, location = raw.split("@", 1)
        host, path = location.split(":", 1)
        raw = f"ssh://{host}/{path}"
    parsed = urllib.parse.urlsplit(raw)
    host = (parsed.hostname or "").lower()
    path = parsed.path.removesuffix(".git").strip("/")
    if parsed.scheme not in {"https", "ssh"} or not SAFE_HOST.fullmatch(host):
        return None
    if not path or not SAFE_PATH.fullmatch(path) or ".." in path.split("/"):
        return None
    sanitized = urllib.parse.urlunsplit((parsed.scheme, host, "/" + path, "", ""))
    return sanitized, f"repo:{host}/{path}"


def remotes(git_dir: Path, budget: list[int]) -> list[tuple[str, str]]:
    config = git_dir / "config"
    if not config.exists():
        return []
    parser = configparser.RawConfigParser(interpolation=None, strict=False)
    try:
        parser.read_string(read_small(config, budget).decode("utf-8"))
    except (UnicodeDecodeError, configparser.Error) as error:
        raise FactsError(".git/config is not bounded valid UTF-8 Git config") from error
    values: list[tuple[str, str]] = []
    origin: tuple[str, str] | None = None
    for section in parser.sections():
        if not section.startswith('remote "') or not parser.has_option(section, "url"):
            continue
        sanitized = sanitize_remote(parser.get(section, "url"))
        if sanitized is not None:
            values.append(sanitized)
            if section == 'remote "origin"':
                origin = sanitized
    # Forks commonly declare an upstream remote. Alphabetical URL order must not turn
    # local setup facts into facts about that other repository.
    return sorted(set(values), key=lambda value: (value != origin, value))


def literal_task(value: Any) -> str | None:
    if isinstance(value, str):
        command = value
    elif isinstance(value, list) and all(isinstance(item, str) for item in value):
        command = " && ".join(value)
    elif isinstance(value, dict):
        return literal_task(value.get("run"))
    else:
        return None
    if not command or DYNAMIC.search(command):
        return None
    return command


def metadata(path: Path, budget: list[int]) -> tuple[list[str], list[dict[str, Any]], list[str]]:
    languages: set[str] = set()
    tasks: list[dict[str, Any]] = []
    instructions: list[str] = []
    for relative in INSTRUCTION_PATHS:
        target = path / relative
        if target.exists() or target.is_symlink():
            # Instruction bridges such as CLAUDE.md -> AGENTS.md are ordinary repository
            # metadata. Keep the pointer only when its entire target stays inside this repo.
            try:
                target = target.resolve(strict=True)
                target.relative_to(path)
            except ValueError as error:
                raise FactsError(f"instruction path escapes the repository: {relative}") from error
            if not target.is_file():
                raise FactsError(f"instruction path is not a regular file: {relative}")
            if target.stat(follow_symlinks=False).st_size > MAX_FILE_BYTES:
                raise FactsError(f"instruction file exceeds {MAX_FILE_BYTES} bytes: {relative}")
            instructions.append(relative)
    documents: dict[str, Any] = {}
    for name in MANIFESTS:
        target = path / name
        if not target.exists():
            continue
        raw = read_small(target, budget)
        try:
            if name == "go.mod":
                raw.decode("utf-8")
                languages.add("Go")
                continue
            documents[name] = json.loads(raw) if name.endswith(".json") else tomllib.loads(raw.decode("utf-8"))
        except (UnicodeDecodeError, json.JSONDecodeError, tomllib.TOMLDecodeError) as error:
            raise FactsError(f"cannot parse {name}") from error
        if name == "pyproject.toml":
            languages.add("Python")
        elif name == "package.json":
            languages.add("TypeScript/JavaScript")
    package_scripts = documents.get("package.json", {}).get("scripts", {})
    mise_tasks = documents.get("mise.toml", {}).get("tasks", {})
    for name in TASK_NAMES:
        sources = (("package.json", package_scripts), ("mise.toml", mise_tasks))
        for source, declarations in sources:
            if not isinstance(declarations, dict) or name not in declarations:
                continue
            command = literal_task(declarations[name])
            item: dict[str, Any] = {"name": name, "source": source, "available": command is not None}
            if command is not None:
                item["command"] = command
            tasks.append(item)
    return sorted(languages), tasks, instructions


def collect(roots: list[str]) -> list[dict[str, Any]]:
    if not roots or len(roots) > MAX_ROOTS:
        raise FactsError(f"expected 1..{MAX_ROOTS} explicit roots")
    records: list[dict[str, Any]] = []
    budget = [0]
    for root_index, raw_root in enumerate(roots, 1):
        root = Path(raw_root).expanduser()
        if not root.is_absolute():
            raise FactsError("roots must be absolute or ~-relative after expansion")
        for repository, relative in repository_candidates(root):
            if len(records) >= MAX_REPOSITORIES:
                raise FactsError(f"repository count exceeds {MAX_REPOSITORIES}")
            remote_values = remotes(repository / ".git", budget)
            languages, tasks, instructions = metadata(repository, budget)
            encoded_relative = urllib.parse.quote_from_bytes(os.fsencode(relative), safe="")
            root_ref = f"root-{root_index}" + ("" if relative == "." else f"/{encoded_relative}")
            local_id = hashlib.sha256(root_ref.encode()).hexdigest()[:16]
            identity = remote_values[0][1] if remote_values else f"repo:local/{local_id}"
            records.append({
                "id": identity,
                "title": identity.removeprefix("repo:"),
                "repository_uri": identity,
                "root_ref": root_ref,
                "remotes": [value[0] for value in remote_values],
                "languages": languages,
                "declared_tasks": tasks,
                "instructions": instructions,
                "proof": "declarations-only",
            })
    records.sort(key=lambda record: record["id"])
    if len({record["id"] for record in records}) != len(records):
        raise FactsError("repository identities are not unique across roots")
    return records


def main() -> int:
    if len(sys.argv) == 2 and sys.argv[1] in {"--version", "-v"}:
        print("repository-facts.py (fkf preset helper)")
        return 0
    try:
        encoded = (json.dumps(collect(sys.argv[1:]), ensure_ascii=False, separators=(",", ":")) + "\n").encode()
        if len(encoded) > MAX_OUTPUT_BYTES:
            raise FactsError(f"output exceeds {MAX_OUTPUT_BYTES} bytes")
    except (FactsError, OSError) as error:
        return fail(str(error), 2 if not sys.argv[1:] else 1)
    sys.stdout.buffer.write(encoded)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
