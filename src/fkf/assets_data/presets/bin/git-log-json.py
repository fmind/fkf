#!/usr/bin/env python3
"""Collect authored Git commits across clones and linked worktrees under declared roots."""

from __future__ import annotations

import json
import os
import re
import subprocess
import sys
from collections.abc import Iterator
from datetime import UTC, datetime
from pathlib import Path
from typing import Any
from urllib.parse import quote, urlsplit

MAX_PROVIDER_BYTES = 64 << 20
MAX_OUTPUT_BYTES = 64 << 20
MAX_ROOTS = 100
MAX_AUTHORS = 100
MAX_WALK_ENTRIES = 1_000_000
MAX_REPOSITORIES = 10_000
MAX_RECORDS = 100_000
HOST = re.compile(r"^[a-z0-9.-]+$")
SEGMENT = re.compile(r"^[A-Za-z0-9._~+%@-]+$")
GITHUB_SEGMENT = re.compile(r"^[A-Za-z0-9._-]+$")
NOREPLY = re.compile(r"^(?:[0-9]+\+)?(?P<login>[a-z0-9](?:[a-z0-9-]{0,37}[a-z0-9])?)@users\.noreply\.github\.com$")


def invoke(arguments: list[str], *, input_bytes: bytes | None = None, allow_absent: bool = False) -> bytes | None:
    if arguments[:1] != ["git"]:
        raise RuntimeError("unexpected provider executable")
    with subprocess.Popen(
        ["git", *arguments[1:]],
        stdin=subprocess.PIPE if input_bytes is not None else None,
        stdout=subprocess.PIPE,
    ) as process:
        if process.stdout is None:  # pragma: no cover
            raise RuntimeError
        if input_bytes is not None:
            if process.stdin is None:  # pragma: no cover
                raise RuntimeError
            process.stdin.write(input_bytes)
            process.stdin.close()
        raw = process.stdout.read(MAX_PROVIDER_BYTES + 1)
        if len(raw) > MAX_PROVIDER_BYTES:
            process.kill()
            process.wait()
            raise RuntimeError("Git output exceeds FKF's 64 MiB command bound")
        status = process.wait()
        if allow_absent and status == 1:
            return None
        if status != 0:
            raise RuntimeError
    return raw


def git_epoch(script_base: Path, option: str, value: str) -> int:
    raw = invoke(["git", "-C", os.fspath(script_base), "rev-parse", f"--{option}={value}"])
    if raw is None:
        raise RuntimeError
    text = raw.decode().strip()
    prefix = "--max-age=" if option == "since" else "--min-age="
    if not text.startswith(prefix) or not text.removeprefix(prefix).isdigit():
        raise RuntimeError("git could not resolve the requested time bounds")
    return int(text.removeprefix(prefix))


def markers(root: Path) -> list[Path]:
    results: list[Path] = []
    directories = [root]
    visited = 0
    while directories:
        directory = directories.pop()
        # scandir streams wide directories and propagates unreadable-subtree failures.
        with os.scandir(directory) as entries:
            for entry in entries:
                visited += 1
                if visited > MAX_WALK_ENTRIES:
                    raise RuntimeError(f"filesystem-entry bound exceeds {MAX_WALK_ENTRIES}")
                if entry.is_symlink():
                    continue
                path = Path(entry.path)
                if entry.name == ".git":
                    if entry.is_dir(follow_symlinks=False) or entry.is_file(follow_symlinks=False):
                        results.append(path)
                        if len(results) > MAX_REPOSITORIES:
                            raise RuntimeError(f"repository bound exceeds {MAX_REPOSITORIES}")
                    continue
                if entry.is_dir(follow_symlinks=False):
                    directories.append(path)
    return results


def valid_host(value: str) -> bool:
    return (
        bool(value)
        and not value.startswith(".")
        and not value.endswith(".")
        and ".." not in value
        and HOST.fullmatch(value) is not None
        and all(label and not label.startswith("-") and not label.endswith("-") for label in value.split("."))
    )


def remote_key(remote: str) -> str | None:
    candidate = remote.split("#", 1)[0].split("?", 1)[0]
    host: str
    path: str
    port: int | None = None
    if "://" in candidate:
        try:
            parsed = urlsplit(candidate)
            host = (parsed.hostname or "").lower()
            port = parsed.port
        except ValueError:
            return None
        path = parsed.path.removeprefix("/")
        if not parsed.scheme or not parsed.netloc or not path:
            return None
    elif re.match(r"^[^/:]+:[^/]+/", candidate):
        authority, path = candidate.split(":", 1)
        host = authority.rsplit("@", 1)[-1].lower()
    else:
        return None
    if not valid_host(host) or (port is not None and not 1 <= port <= 65535):
        return None
    path = path.removesuffix(".git")
    segments = path.split("/")
    if len(segments) < 2 or any(
        segment in {"", ".", ".."} or SEGMENT.fullmatch(segment) is None for segment in segments
    ):
        return None
    suffix = f":{port}" if port is not None else ""
    return f"{host}{suffix}/{path}"


def repository_identity(gitdir: Path, script_base: Path) -> tuple[str | None, str]:
    raw = invoke(["git", f"--git-dir={gitdir}", "config", "--get", "remote.origin.url"], allow_absent=True)
    remote = raw.decode().strip() if raw is not None else ""
    key = remote_key(remote)
    full: str | None = None
    if key is not None and key.startswith("github.com/"):
        parts = key.removeprefix("github.com/").split("/")
        if len(parts) == 2 and all(part not in {"", ".", ".."} and GITHUB_SEGMENT.fullmatch(part) for part in parts):
            full = "/".join(parts)
    if full is not None:
        return full, full
    identity_key = f"remote:{key}" if key is not None else f"gitdir:{gitdir}"
    # Git's hash-object honors repositories that have selected a non-SHA1 object format.
    digest_raw = invoke(
        ["git", "-C", os.fspath(script_base), "hash-object", "--stdin"], input_bytes=(identity_key + "\n").encode()
    )
    digest = digest_raw.decode().strip() if digest_raw is not None else ""
    if not digest or any(character not in "0123456789abcdef" for character in digest):
        raise RuntimeError("git returned an invalid repository identity digest")
    return None, f"opaque:{digest}"


def participant(email: str) -> str:
    lowered = email.lower()
    match = NOREPLY.fullmatch(lowered)
    if match is not None:
        return f"actor:github.com/{match['login']}"
    return "person:email/" + quote(lowered, safe="/:@+").replace("~", "%7E")


def log_records(
    gitdir: Path,
    authors: list[str],
    since: str,
    until: str,
    repo_full: str | None,
    repo_identity: str,
    since_epoch: int,
    until_epoch: int,
) -> Iterator[dict[str, Any]]:
    raw = invoke(
        [
            "git",
            f"--git-dir={gitdir}",
            "log",
            "--all",
            "--fixed-strings",
            *(f"--author={author}" for author in authors),
            f"--since={since}",
            f"--until={until}",
            "--pretty=format:%H%x00%ct%x00%aE%x00%s%x00",
        ]
    )
    if raw is None or not raw:
        return
    if not raw.endswith(b"\0"):
        raise RuntimeError("git log output has no terminal NUL")
    offset = 0
    while offset < len(raw):
        fields: list[bytes] = []
        for _ in range(4):
            boundary = raw.find(b"\0", offset)
            if boundary < 0:
                raise RuntimeError("git log output has an incomplete field group")
            fields.append(raw[offset:boundary])
            offset = boundary + 1
        commit_hash = fields[0].decode(errors="replace").removeprefix("\n")
        epoch = int(fields[1])
        if not since_epoch <= epoch < until_epoch:
            continue
        email = fields[2].decode(errors="replace")
        record = {
            "hash": commit_hash,
            "author_email": email,
            "message": fields[3].decode(errors="replace"),
            "repo_full": repo_full,
            "time": datetime.fromtimestamp(epoch, UTC).isoformat().replace("+00:00", "Z"),
            "uid": f"{repo_identity}@{commit_hash}",
            "repository_uri": f"repo:github.com/{repo_full}" if repo_full is not None else None,
            "participant_uris": [participant(email)] if email else [],
        }
        yield record


def main(arguments: list[str]) -> int:
    if len(arguments) < 3:
        sys.stderr.write("usage: git-log-json.py <since> <until> <root>[:<root>...] [author...]\n")
        return 2
    since, until, roots_value, *authors = arguments
    home = os.environ.get("HOME", "")
    roots = [Path(home + value[1:]) if value.startswith("~") else Path(value) for value in roots_value.split(":")]
    if len(roots) > MAX_ROOTS:
        sys.stderr.write(f"git-log-json.py: root bound exceeds {MAX_ROOTS}\n")
        return 1
    if len(authors) > MAX_AUTHORS:
        sys.stderr.write(f"git-log-json.py: author bound exceeds {MAX_AUTHORS}\n")
        return 1
    for root in roots:
        if not root.is_dir():
            sys.stderr.write(
                f"git-log-json.py: {root} is not a directory; point the third argument of run: at your clones\n"
            )
            return 1
    since = since if "T" in since else since + "T00:00:00"
    until = until if "T" in until else until + "T00:00:00"
    script_base = Path(__file__).resolve().parent.parent
    try:
        since_epoch = git_epoch(script_base, "since", since)
        until_epoch = git_epoch(script_base, "until", until)
        if since_epoch >= until_epoch:
            raise RuntimeError("since must be before until")
        if not authors:
            raw = invoke(["git", "-C", os.fspath(script_base), "config", "--get", "user.email"], allow_absent=True)
            author = raw.decode().strip() if raw is not None else ""
            if not author:
                raise RuntimeError(
                    "git config user.email is unset and no author was given; append your identities to run:"
                )
            authors = [author]
        marker_set: set[Path] = set()
        for root in roots:
            marker_set.update(markers(root))
            if len(marker_set) > MAX_REPOSITORIES:
                raise RuntimeError(f"repository bound exceeds {MAX_REPOSITORIES}")
        marker_paths = sorted(marker_set)
        gitdirs: set[Path] = set()
        for marker in marker_paths:
            raw = invoke(
                ["git", "-C", os.fspath(marker.parent), "rev-parse", "--path-format=absolute", "--git-common-dir"]
            )
            if raw is None:
                raise RuntimeError
            gitdirs.add(Path(raw.decode().strip()))
            if len(gitdirs) > MAX_REPOSITORIES:
                raise RuntimeError(f"repository bound exceeds {MAX_REPOSITORIES}")
        unique: dict[str, dict[str, Any]] = {}
        output_size = len(b"[]\n")
        for gitdir in sorted(gitdirs):
            repo_full, identity = repository_identity(gitdir, script_base)
            for record in log_records(gitdir, authors, since, until, repo_full, identity, since_epoch, until_epoch):
                uid = record["uid"]
                previous = unique.get(uid)
                if previous is not None:
                    if previous != record:
                        raise RuntimeError(f"conflicting duplicate commit identity: {uid}")
                    continue
                if len(unique) >= MAX_RECORDS:
                    raise RuntimeError(f"commit record bound exceeds {MAX_RECORDS}")
                record_size = len(json.dumps(record, ensure_ascii=False, separators=(",", ":")).encode())
                output_size += record_size + (1 if unique else 0)
                if output_size > MAX_OUTPUT_BYTES:
                    raise RuntimeError(f"output bound exceeds {MAX_OUTPUT_BYTES} bytes")
                unique[uid] = record
        ordered = [unique[uid] for uid in sorted(unique)]
        output = (json.dumps(ordered, ensure_ascii=False, separators=(",", ":")) + "\n").encode()
        if len(output) > MAX_OUTPUT_BYTES:  # Keep accounting tied to the exact emitted bytes.
            raise RuntimeError(f"output bound exceeds {MAX_OUTPUT_BYTES} bytes")
    except (OSError, RuntimeError, UnicodeError, TypeError, ValueError) as error:
        sys.stderr.write(f"git-log-json.py: {error}\n")
        return 1
    sys.stdout.buffer.write(output)
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
