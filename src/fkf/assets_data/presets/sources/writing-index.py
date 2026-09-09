#!/usr/bin/env python3
"""Index explicitly declared authored documents without reading their bodies."""

from __future__ import annotations

import json
import os
import re
import stat
import subprocess
import sys
import tempfile
from datetime import UTC, datetime
from pathlib import Path
from typing import Any
from urllib.parse import urlsplit

GENERIC_SLUGS = frozenset({"draft", "article", "index", "README", "readme", "announce", "HANDOFF", "main", "content"})
GITHUB_PART = re.compile(r"^[A-Za-z0-9._-]+$")
MAX_FRONT_MATTER_BYTES = 1 << 20
MAX_YQ_BYTES = 1 << 20
MAX_GIT_BYTES = 1 << 16
MAX_OUTPUT_BYTES = 64 << 20


class DocumentIndexError(Exception):
    """A declared document cannot be projected safely."""


def bounded_output(process: subprocess.Popen[bytes], name: str, output_limit: int) -> tuple[int, bytes]:
    """Drain one already-started closed command without retaining diagnostics."""
    with process:
        if process.stdout is None:  # pragma: no cover
            raise RuntimeError
        output = process.stdout.read(output_limit + 1)
        if len(output) > output_limit:
            process.kill()
            process.wait()
            raise DocumentIndexError(f"{name} output exceeds {output_limit} bytes")
        returncode = process.wait()
    return returncode, output


def run_yq(format_name: str, source: bytes) -> tuple[int, bytes]:
    """Run the one fixed front-matter decoder with bounded file-backed stdin."""
    with tempfile.TemporaryFile() as input_stream:
        input_stream.write(source)
        input_stream.seek(0)
        process = subprocess.Popen(
            ["yq", f"-p={format_name}", "-o=json"],
            stdin=input_stream,
            stdout=subprocess.PIPE,
            stderr=subprocess.DEVNULL,
        )
        return bounded_output(process, "yq", MAX_YQ_BYTES)


def run_git(directory: Path) -> tuple[int, bytes]:
    """Read one fixed Git configuration key through bounded stdout."""
    process = subprocess.Popen(
        ["git", "-C", os.fspath(directory), "config", "--get", "remote.origin.url"],
        stdin=subprocess.DEVNULL,
        stdout=subprocess.PIPE,
        stderr=subprocess.DEVNULL,
    )
    return bounded_output(process, "git", MAX_GIT_BYTES)


def decode_front_matter(path: Path, source: bytes, delimiter: bytes) -> dict[str, Any]:
    """Decode one already-bounded front-matter payload."""
    format_name = "toml" if delimiter == b"+++" else "yaml"
    try:
        returncode, output = run_yq(format_name, source)
    except FileNotFoundError as error:
        raise DocumentIndexError("yq is required (mise use -g yq@latest)") from error
    if returncode != 0:
        raise DocumentIndexError(f"malformed {format_name} front matter in {path}")
    try:
        value = json.loads(output or b"{}")
    except json.JSONDecodeError as error:
        raise DocumentIndexError(f"malformed {format_name} front matter in {path}") from error
    if value is None:
        value = {}
    if not isinstance(value, dict):
        raise DocumentIndexError(f"front matter in {path} is not a metadata object")
    return value


def file_fingerprint(value: os.stat_result) -> tuple[int, int, int, int, int, int]:
    return value.st_dev, value.st_ino, value.st_mode, value.st_size, value.st_mtime_ns, value.st_ctime_ns


def read_at(descriptor: int, count: int, offset: int) -> bytes:
    """Read no more than count bytes from a fixed descriptor offset."""
    output = bytearray()
    while len(output) < count:
        chunk = os.pread(descriptor, count - len(output), offset + len(output))
        if not chunk:
            break
        output.extend(chunk)
    return bytes(output)


def front_matter_prefix(descriptor: int, size: int) -> tuple[bytes, bytes, int] | None:
    """Return bounded source, delimiter, and body offset without materializing the body."""
    head = read_at(descriptor, min(size, 5), 0)
    delimiter: bytes | None = None
    opening_end = 0
    for candidate in (b"+++", b"---"):
        if head.startswith(candidate + b"\r\n"):
            delimiter, opening_end = candidate, 5
        elif head.startswith(candidate + b"\n"):
            delimiter, opening_end = candidate, 4
        elif head == candidate or head == candidate + b"\r":
            delimiter, opening_end = candidate, len(head)
    if delimiter is None:
        return None
    prefix_size = min(size, MAX_FRONT_MATTER_BYTES)
    prefix = head + read_at(descriptor, prefix_size - len(head), len(head))
    position = opening_end
    while position < len(prefix):
        line_end = prefix.find(b"\n", position)
        line = prefix[position:] if line_end < 0 else prefix[position:line_end]
        if line.endswith(b"\r"):
            line = line[:-1]
        if line == delimiter and (line_end >= 0 or len(prefix) == size):
            body_offset = len(prefix) if line_end < 0 else line_end + 1
            return prefix[opening_end:position], delimiter, body_offset
        if line_end < 0:
            break
        position = line_end + 1
    raise DocumentIndexError(
        f"opened front matter has no closing {delimiter.decode()} front-matter delimiter within 1 MiB"
    )


def read_document(path: Path) -> tuple[dict[str, Any], int, float]:
    """Bind one declared regular file and derive metadata from its opened descriptor."""
    error_message = f"'{path}' is not a regular file; check for an unmatched declared glob"
    try:
        declared = path.lstat()
    except OSError as error:
        raise DocumentIndexError(error_message) from error
    if not stat.S_ISREG(declared.st_mode):
        raise DocumentIndexError(error_message)
    flags = os.O_RDONLY | getattr(os, "O_CLOEXEC", 0) | getattr(os, "O_NOFOLLOW", 0)
    try:
        descriptor = os.open(path, flags)
    except OSError as error:
        raise DocumentIndexError(error_message) from error
    try:
        opened = os.fstat(descriptor)
        if not stat.S_ISREG(opened.st_mode) or file_fingerprint(declared) != file_fingerprint(opened):
            raise DocumentIndexError(f"'{path}' changed while it was being opened")
        located = front_matter_prefix(descriptor, opened.st_size)
        if located is None:
            front: dict[str, Any] = {}
            chars = opened.st_size
        else:
            source, delimiter, body_offset = located
            front = decode_front_matter(path, source, delimiter)
            body_bytes = opened.st_size - body_offset
            last = read_at(descriptor, 1, opened.st_size - 1) if body_bytes else b""
            if body_bytes and len(last) != 1:
                raise DocumentIndexError(f"'{path}' changed while it was being read")
            # awk preserved input bytes and added one newline to an unterminated final record.
            chars = body_bytes + (1 if body_bytes and last != b"\n" else 0)
        if file_fingerprint(opened) != file_fingerprint(os.fstat(descriptor)):
            raise DocumentIndexError(f"'{path}' changed while it was being read")
    finally:
        os.close(descriptor)
    return front, chars, opened.st_mtime


def github_repository(remote: str) -> str | None:
    """Parse only closed GitHub clone URL forms into an owner/name."""
    if not remote or "?" in remote or "#" in remote or any(ord(char) < 32 or ord(char) == 127 for char in remote):
        return None
    path: str
    if "://" in remote:
        try:
            parsed = urlsplit(remote)
            port = parsed.port
        except ValueError:
            return None
        if (
            parsed.scheme not in {"https", "ssh"}
            or (parsed.hostname or "").lower() != "github.com"
            or port is not None
            or parsed.password is not None
            or (parsed.scheme == "https" and parsed.username is not None)
            or (parsed.scheme == "ssh" and parsed.username != "git")
            or not parsed.path.startswith("/")
        ):
            return None
        path = parsed.path.removeprefix("/")
    else:
        match = re.fullmatch(r"git@(?i:github\.com):(?P<path>.+)", remote)
        if match is None:
            return None
        path = match["path"]
    path = path.removesuffix(".git")
    parts = path.split("/")
    if len(parts) != 2 or any(part in {"", ".", ".."} or GITHUB_PART.fullmatch(part) is None for part in parts):
        return None
    return "/".join(parts)


def remote_repository(path: Path) -> str | None:
    returncode, output = run_git(path.parent)
    if returncode != 0:
        return None
    try:
        remote = output.decode("utf-8")
    except UnicodeDecodeError:
        return None
    # Remove Git's record delimiter only; any additional control byte belongs to the value.
    return github_repository(remote.removesuffix("\n"))


def string_list(value: Any) -> list[str]:
    values = value if isinstance(value, list) else [value]
    return [str(item).lower() if isinstance(item, bool) else str(item) for item in values]


def project(path: Path, home: Path) -> dict[str, Any]:
    front, chars, modified_at = read_document(path)
    slug = path.stem
    if slug in GENERIC_SLUGS:
        slug = path.parent.name
    modified = datetime.fromtimestamp(modified_at, UTC).strftime("%Y-%m-%dT%H:%M:%SZ")
    try:
        relative = "~/" + path.resolve().relative_to(home.resolve()).as_posix()
    except ValueError:
        relative = os.fspath(path)
    record: dict[str, Any] = {
        "id": slug,
        "slug": front.get("slug", slug),
        "title": front.get("title", slug),
        "description": front.get("description"),
        "date": front.get("date"),
        "tags": string_list(front.get("tags", [])),
        "draft": front.get("draft", False),
        "path": relative,
        "modified": modified,
        "chars": chars,
    }
    repository = remote_repository(path)
    if repository is not None:
        record["repo"] = repository
    url = front.get("url", front.get("permalink"))
    if url is not None:
        record["url"] = url
    return record


def main(arguments: list[str]) -> int:
    if arguments[:1] in (["--version"], ["-v"]):
        sys.stdout.write("writing-index.py (fkf preset helper)\n")
        return 0
    if not arguments:
        sys.stderr.write("usage: writing-index.py <file>...\n")
        return 1
    home = Path(os.environ.get("HOME", ""))
    try:
        records = [project(Path(argument), home) for argument in arguments]
        seen: set[str] = set()
        for record, argument in zip(records, arguments, strict=True):
            slug = str(record["id"])
            if slug in seen:
                raise DocumentIndexError(f"two files reduce to the slug '{slug}'; the second is {argument}")
            seen.add(slug)
        records.sort(key=lambda record: str(record.get("date") or record["modified"]), reverse=True)
        output = json.dumps(records, ensure_ascii=False, separators=(",", ":"))
        if len((output + "\n").encode("utf-8")) > MAX_OUTPUT_BYTES:
            raise DocumentIndexError("aggregate output exceeds 64 MiB")
    except (DocumentIndexError, OSError, TypeError, ValueError) as error:
        sys.stderr.write(f"writing-index.py: {error}\n")
        return 1
    sys.stdout.write(output + "\n")
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
