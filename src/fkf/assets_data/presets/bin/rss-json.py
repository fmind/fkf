#!/usr/bin/env python3
"""Collect current RSS, RDF, and Atom feed snapshots with private-feed protection."""

from __future__ import annotations

import base64
import concurrent.futures
import contextlib
import email.utils
import hashlib
import json
import os
import stat
import subprocess
import sys
import tempfile
import unicodedata
from collections.abc import Iterable
from dataclasses import dataclass
from datetime import UTC, datetime
from html.parser import HTMLParser
from pathlib import Path
from typing import Any
from urllib.parse import quote

MAX_OPML_BYTES = 1 << 20
MAX_FEEDS = 256
MAX_FEED_BYTES = 8 << 20
MAX_ERROR_TAIL_BYTES = 8 << 10
MAX_OUTPUT_BYTES = 64 << 20
READ_CHUNK_BYTES = 64 << 10
MAX_FETCH_WORKERS = 8
EMPTY_OUTPUT_BYTES = len(b"[]\n")


@dataclass(frozen=True)
class Feed:
    number: int
    visibility: str
    endpoint: str
    identity: str
    folder: str


FileFingerprint = tuple[int, int, int, int, int, int]


@dataclass(frozen=True)
class FetchResult:
    fingerprint: FileFingerprint | None
    failure: str | None


@dataclass
class Element:
    """Minimal ordered XML node for the feed fields this helper reads."""

    tag: str
    attributes: dict[str, str]
    content: list[str | Element]

    def __iter__(self):
        return (item for item in self.content if isinstance(item, Element))

    def get(self, name: str, default: str | None = None) -> str | None:
        folded = name.casefold()
        return next((value for key, value in self.attributes.items() if key.casefold() == folded), default)

    def iter(self):
        yield self
        for child_element in self:
            yield from child_element.iter()

    def itertext(self):
        for item in self.content:
            if isinstance(item, Element):
                yield from item.itertext()
            else:
                yield item


class XMLParseError(ValueError):
    """The bounded feed bytes are not safe, well-formed XML."""


def file_fingerprint(value: os.stat_result) -> FileFingerprint:
    return value.st_dev, value.st_ino, value.st_mode, value.st_size, value.st_mtime_ns, value.st_ctime_ns


def readable_limit(limit: int) -> str:
    if limit % (1 << 20) == 0:
        return f"{limit >> 20} MiB"
    return f"{limit} bytes"


def read_regular(
    path: Path,
    limit: int,
    label: str,
    *,
    expected: FileFingerprint | None = None,
) -> bytes:
    """Limit-plus-one read bound to one unchanged regular, non-symlink file."""
    try:
        declared = path.lstat()
    except OSError as error:
        raise ValueError(f"{label} is not a regular non-symlink file") from error
    if not stat.S_ISREG(declared.st_mode):
        raise ValueError(f"{label} is not a regular non-symlink file")
    if expected is not None and file_fingerprint(declared) != expected:
        raise ValueError(f"{label} changed after download")

    # Nonblocking open makes an lstat-to-open swap to a FIFO fail closed instead of hanging.
    flags = os.O_RDONLY | getattr(os, "O_CLOEXEC", 0) | getattr(os, "O_NOFOLLOW", 0) | getattr(os, "O_NONBLOCK", 0)
    try:
        descriptor = os.open(path, flags)
    except OSError as error:
        raise ValueError(f"{label} is not a regular non-symlink file") from error
    try:
        opened = os.fstat(descriptor)
        if not stat.S_ISREG(opened.st_mode) or file_fingerprint(declared) != file_fingerprint(opened):
            raise ValueError(f"{label} changed while it was being opened")
        source = bytearray()
        while len(source) <= limit:
            chunk = os.read(descriptor, min(READ_CHUNK_BYTES, limit + 1 - len(source)))
            if not chunk:
                break
            source.extend(chunk)
        if len(source) > limit:
            raise ValueError(f"{label} exceeds {readable_limit(limit)}")
        after = os.fstat(descriptor)
        try:
            located = path.lstat()
        except OSError as error:
            raise ValueError(f"{label} changed while it was being read") from error
        if file_fingerprint(opened) != file_fingerprint(after) or file_fingerprint(after) != file_fingerprint(located):
            raise ValueError(f"{label} changed while it was being read")
    finally:
        os.close(descriptor)
    return bytes(source)


class XMLTreeParser(HTMLParser):
    """Build a strict-enough feed tree without enabling DTD or external-entity machinery."""

    def __init__(self) -> None:
        super().__init__(convert_charrefs=True)
        self.root: Element | None = None
        self.stack: list[Element] = []

    def handle_starttag(self, tag: str, attrs: list[tuple[str, str | None]]) -> None:
        element = Element(tag, {key: value or "" for key, value in attrs}, [])
        if self.stack:
            self.stack[-1].content.append(element)
        elif self.root is None:
            self.root = element
        else:
            raise XMLParseError("multiple XML roots")
        self.stack.append(element)

    def handle_startendtag(self, tag: str, attrs: list[tuple[str, str | None]]) -> None:
        self.handle_starttag(tag, attrs)
        self.handle_endtag(tag)

    def handle_endtag(self, tag: str) -> None:
        if not self.stack or self.stack[-1].tag != tag:
            raise XMLParseError(f"mismatched closing tag {tag}")
        self.stack.pop()

    def handle_data(self, data: str) -> None:
        if self.stack:
            self.stack[-1].content.append(data)
        elif data.strip():
            raise XMLParseError("text outside the XML root")

    def handle_decl(self, decl: str) -> None:
        del decl
        raise XMLParseError("DTD declarations are forbidden")

    def handle_pi(self, data: str) -> None:
        if not data.casefold().startswith("xml "):
            raise XMLParseError("processing instructions are forbidden")


def local_name(tag: str) -> str:
    return tag.rsplit("}", 1)[-1].split(":", 1)[-1]


def clean_title(value: str | None) -> str:
    if value is None:
        return ""
    visible = "".join(
        "" if unicodedata.category(character) == "Cf" else " " if unicodedata.category(character) == "Cc" else character
        for character in value
    )
    return " ".join(visible.split())


def external_url(value: str | None) -> str | None:
    if value is not None and value.startswith("http://"):
        return "https://" + value.removeprefix("http://")
    return value


def element_text(element: Element | None) -> str | None:
    if element is None:
        return None
    value = "".join(element.itertext()).strip()
    return value or element.get("href")


def child(element: Element, *names: str) -> Element | None:
    wanted = {name.casefold() for name in names}
    return next((item for item in element if local_name(item.tag).casefold() in wanted), None)


def children(element: Element, name: str) -> list[Element]:
    return [item for item in element if local_name(item.tag).casefold() == name.casefold()]


def link(element: Element) -> str | None:
    links = children(element, "link")
    if not links:
        return None
    chosen = next((item for item in links if item.get("rel") in {None, "alternate"}), links[0])
    return element_text(chosen)


def parse_time(value: str | None) -> str | None:
    if value is None:
        return None
    parsed: datetime | None = None
    with contextlib.suppress(TypeError, ValueError):
        parsed = email.utils.parsedate_to_datetime(value)
    if parsed is None:
        try:
            parsed = datetime.fromisoformat(value)
        except ValueError:
            try:
                parsed = datetime.fromisoformat(value + "T00:00:00+00:00")
            except ValueError:
                return None
    if parsed.tzinfo is None:
        parsed = parsed.replace(tzinfo=UTC)
    return parsed.astimezone(UTC).replace(microsecond=0).isoformat().replace("+00:00", "Z")


def scrub(value: Any, endpoint: str, identity: str) -> Any:
    if isinstance(value, str):
        return value.replace(endpoint, identity).replace(external_url(endpoint) or endpoint, identity)
    if isinstance(value, list):
        return [scrub(item, endpoint, identity) for item in value]
    if isinstance(value, dict):
        return {key: scrub(item, endpoint, identity) for key, item in value.items()}
    return value


def secure_xml(source: bytes) -> Element:
    """Parse the small feed subset while refusing DTDs and entity declarations."""
    upper = source.upper()
    if b"<!DOCTYPE" in upper or b"<!ENTITY" in upper:
        raise XMLParseError("DTD and entity declarations are forbidden")
    parser = XMLTreeParser()
    try:
        parser.feed(source.decode("utf-8-sig"))
        parser.close()
    except (UnicodeError, XMLParseError) as error:
        raise XMLParseError(str(error)) from error
    if parser.stack:
        raise XMLParseError("unclosed XML element")
    if parser.root is None:
        raise XMLParseError("empty XML document")
    return parser.root


def parse_opml(path: Path, visibility: str) -> list[tuple[str, str, str]]:
    try:
        source = read_regular(path, MAX_OPML_BYTES, f"{visibility} OPML")
        root = secure_xml(source)
    except XMLParseError as error:
        raise ValueError(f"cannot parse {visibility} OPML") from error
    body = next((item for item in root.iter() if local_name(item.tag).casefold() == "body"), None)
    if body is None:
        raise ValueError(f"cannot read {visibility} OPML outlines")
    records: list[tuple[str, str, str]] = []

    def walk(outline: Element, folder: str) -> None:
        endpoint = outline.get("xmlUrl")
        if endpoint is not None:
            if len(records) >= MAX_FEEDS:
                raise ValueError(f"at most {MAX_FEEDS} feeds are allowed")
            records.append((visibility, endpoint, folder.replace("\t", " ").replace("\r", " ").replace("\n", " ")))
        next_folder = outline.get("text") or folder
        for nested in children(outline, "outline"):
            walk(nested, next_folder)

    for outline in children(body, "outline"):
        walk(outline, "")
    return records


def inputs(arguments: list[str]) -> list[Feed]:
    raw: list[tuple[str, str, str]] = []

    def admit(records: list[tuple[str, str, str]]) -> None:
        for record in records:
            if len(raw) >= MAX_FEEDS:
                raise ValueError(f"at most {MAX_FEEDS} feeds are allowed")
            raw.append(record)

    index = 0
    while index < len(arguments):
        value = arguments[index]
        if value == "--optional-opml":
            index += 1
            if index == len(arguments):
                raise ValueError("--optional-opml requires a path")
            path = Path(arguments[index])
            try:
                path.lstat()
            except FileNotFoundError:
                pass
            except OSError as error:
                raise ValueError("cannot inspect optional private OPML") from error
            else:
                admit(parse_opml(path, "private"))
        elif value.startswith("--"):
            raise ValueError(f"unknown option: {value}")
        elif value.startswith(("http://", "https://")):
            admit([("public", value, "")])
        else:
            path = Path(value)
            try:
                path.lstat()
            except OSError as error:
                raise ValueError(f"not an existing OPML file or an http/https feed URL: {value}") from error
            admit(parse_opml(path, "public"))
        index += 1
    if not raw:
        raise ValueError("the OPML names no feed (no outline carries xmlUrl)")
    feeds = []
    for number, (visibility, endpoint, folder) in enumerate(raw, 1):
        if not endpoint.startswith(("http://", "https://")):
            if visibility == "private":
                raise ValueError(f"private feed #{number} must use http or https")
            raise ValueError(f"feed URLs must use http or https: {endpoint}")
        identity = (
            endpoint if visibility == "public" else f"private-feed-{hashlib.sha256(endpoint.encode()).hexdigest()}"
        )
        feeds.append(Feed(number, visibility, endpoint, identity, folder))
    return feeds


def error_tail(path: Path) -> str | None:
    try:
        with path.open("rb") as stream:
            size = os.fstat(stream.fileno()).st_size
            stream.seek(max(0, size - MAX_ERROR_TAIL_BYTES))
            lines = stream.read(MAX_ERROR_TAIL_BYTES).decode("utf-8", errors="replace").splitlines()
    except OSError:
        return None
    return lines[-1] if lines else None


def fetch(feed: Feed, directory: Path) -> FetchResult:
    target = directory / f"{feed.number}.xml"
    error_path = directory / f"{feed.number}.err"
    with error_path.open("wb") as error_stream:
        completed = subprocess.run(
            [
                "curl",
                "--fail",
                "--silent",
                "--show-error",
                "--location",
                "--max-time",
                "60",
                "--max-filesize",
                str(MAX_FEED_BYTES),
                "--proto",
                "=http,https",
                "--proto-redir",
                "=http,https",
                "--retry",
                "2",
                "--retry-delay",
                "2",
                "--retry-all-errors",
                "--user-agent",
                "Mozilla/5.0 (compatible; fkf-rss-json/1.0; +https://fmind.github.io/fkf)",
                "--output",
                os.fspath(target),
                feed.endpoint,
            ],
            check=False,
            stdout=subprocess.DEVNULL,
            stderr=error_stream,
        )
    detail: str | None = None
    if completed.returncode == 0:
        try:
            downloaded = target.lstat()
        except OSError:
            detail = "download did not create a regular file"
        else:
            if not stat.S_ISREG(downloaded.st_mode):
                detail = "download did not create a regular file"
            elif downloaded.st_size > MAX_FEED_BYTES:
                detail = "download exceeds 8 MiB"
            else:
                return FetchResult(file_fingerprint(downloaded), None)
    if feed.visibility == "private":
        return FetchResult(None, f"private feed #{feed.number}: download failed (curl exit {completed.returncode})")
    detail = detail or error_tail(error_path) or f"curl exit {completed.returncode}"
    return FetchResult(None, f"{feed.endpoint}: {detail}")


def feed_shape(root: Element) -> tuple[Element, list[Element]]:
    root_name = local_name(root.tag).casefold()
    if root_name == "rss":
        channel = child(root, "channel")
        if channel is None:
            raise ValueError
        return channel, children(channel, "item")
    if root_name == "feed":
        return root, children(root, "entry")
    if root_name == "rdf":
        channel = child(root, "channel")
        if channel is None:
            raise ValueError
        return channel, children(root, "item")
    raise ValueError


def normalize(feed: Feed, source: bytes) -> tuple[dict[str, Any], list[dict[str, Any]]]:
    root = secure_xml(source)
    channel, item_elements = feed_shape(root)
    items: list[dict[str, Any]] = []
    for element in item_elements:
        identifier = element_text(child(element, "guid", "id")) or link(element)
        if identifier is None:
            continue
        safe_id = (
            scrub(str(identifier), feed.endpoint, feed.identity) if feed.visibility == "private" else str(identifier)
        )
        title = clean_title(element_text(child(element, "title")))
        date_element = child(element, "pubDate", "published", "updated", "date")
        item = {
            "id": f"item:{quote(external_url(feed.identity) or feed.identity, safe='')}:{quote(safe_id, safe='')}",
            "kind": "item",
            "time": parse_time(element_text(date_element)),
            "title": title or f"Feed item {quote(safe_id, safe='')}",
            "url": external_url(link(element)),
            "feed": external_url(feed.identity),
            "folder": feed.folder,
            "visibility": feed.visibility,
        }
        if feed.visibility == "private":
            item = scrub(item, feed.endpoint, feed.identity)
            encoded = base64.b64encode(item["id"].encode()).decode()
            item["id"] = f"item:{feed.identity}:{hashlib.sha256(encoded.encode()).hexdigest()}"
            item["url"] = None
        items.append(item)
    channel_title = clean_title(element_text(child(channel, "title")))
    record = {
        "id": f"feed:{external_url(feed.identity)}",
        "kind": "feed",
        "title": channel_title
        or ("Private feed" if feed.visibility == "private" else f"Feed {quote(feed.identity, safe='')}"),
        "url": None if feed.visibility == "private" else external_url(feed.identity),
        "site_url": None if feed.visibility == "private" else external_url(link(channel)),
        "folder": feed.folder,
        "item_count": len(items),
        "visibility": feed.visibility,
    }
    if feed.visibility == "private":
        record = scrub(record, feed.endpoint, feed.identity)
    return record, items


def encode_output(records: list[dict[str, Any]]) -> bytes:
    """Encode one bounded JSON document before writing any stdout bytes."""
    output = bytearray()
    encoder = json.JSONEncoder(ensure_ascii=False, separators=(",", ":"))
    for chunk in encoder.iterencode(records):
        encoded = chunk.encode("utf-8")
        if len(output) + len(encoded) + 1 > MAX_OUTPUT_BYTES:
            raise ValueError("output exceeds 64 MiB")
        output.extend(encoded)
    output.extend(b"\n")
    return bytes(output)


def encoded_size_within(value: Any, limit: int) -> int | None:
    """Return the exact UTF-8 JSON size, stopping once the remaining budget is exceeded."""
    if limit < 0:
        return None
    size = 0
    encoder = json.JSONEncoder(ensure_ascii=False, separators=(",", ":"))
    for chunk in encoder.iterencode(value):
        size += len(chunk.encode("utf-8"))
        if size > limit:
            return None
    return size


def retain_unique(
    unique: dict[str, dict[str, Any]],
    retained_bytes: int,
    records: Iterable[dict[str, Any]],
) -> tuple[int, bool]:
    """Deduplicate first, then retain only records whose exact final JSON bytes fit."""
    for record in records:
        identifier = record["id"]
        if identifier in unique:
            continue
        separator_bytes = 1 if unique else 0
        record_bytes = encoded_size_within(record, MAX_OUTPUT_BYTES - retained_bytes - separator_bytes)
        if record_bytes is None:
            return retained_bytes, False
        unique[identifier] = record
        retained_bytes += separator_bytes + record_bytes
    return retained_bytes, True


def remove_download(feed: Feed, directory: Path) -> None:
    """Remove one completed worker's bounded temporary files before submitting another."""
    for extension in ("xml", "err"):
        try:
            (directory / f"{feed.number}.{extension}").unlink()
        except FileNotFoundError:
            pass


def main(arguments: list[str]) -> int:
    if arguments[:1] in (["--version"], ["-v"]):
        sys.stdout.write("rss-json.py (fkf preset helper)\n")
        return 0
    if not arguments:
        sys.stderr.write("usage: rss-json.py <feed-url-or-opml>... [--optional-opml <private-opml>]\n")
        return 1
    try:
        feeds = inputs(arguments)
    except ValueError as error:
        sys.stderr.write(f"rss-json.py: {error}\n")
        return 1
    failures: list[str] = []
    unique: dict[str, dict[str, Any]] = {}
    retained_bytes = EMPTY_OUTPUT_BYTES
    output_too_large = retained_bytes > MAX_OUTPUT_BYTES
    with tempfile.TemporaryDirectory(prefix="fkf-rss-") as temporary:
        directory = Path(temporary)
        with concurrent.futures.ThreadPoolExecutor(max_workers=MAX_FETCH_WORKERS) as executor:
            next_index = min(MAX_FETCH_WORKERS, len(feeds))
            pending = [(feed, executor.submit(fetch, feed, directory)) for feed in feeds[:next_index]]
            while pending:
                feed, future = pending.pop(0)
                download = future.result()
                label = f"private feed #{feed.number}" if feed.visibility == "private" else feed.endpoint
                if download.failure is not None:
                    failures.append(download.failure)
                else:
                    try:
                        if download.fingerprint is None:
                            raise ValueError("download metadata is missing")
                        source = read_regular(
                            directory / f"{feed.number}.xml",
                            MAX_FEED_BYTES,
                            label,
                            expected=download.fingerprint,
                        )
                        feed_record, items = normalize(feed, source)
                        invalid_count = 0
                        first_invalid: dict[str, Any] | None = None
                        for item in items:
                            if item["time"] is None:
                                invalid_count += 1
                                if first_invalid is None:
                                    first_invalid = {"feed": item["feed"], "id": item["id"]}
                        if first_invalid is not None:
                            raise ValueError(
                                f"unparseable date for {invalid_count} addressable item(s); first="
                                + json.dumps(first_invalid, separators=(",", ":"))
                            )
                        if not output_too_large:
                            retained_bytes, fits = retain_unique(unique, retained_bytes, (feed_record,))
                            if fits:
                                retained_bytes, fits = retain_unique(unique, retained_bytes, items)
                            output_too_large = not fits
                    except XMLParseError:
                        failures.append(f"{label}: not XML")
                    except (OSError, TypeError, ValueError) as error:
                        failures.append(f"{label}: {error or 'cannot normalize feed JSON'}")
                remove_download(feed, directory)
                if next_index < len(feeds):
                    next_feed = feeds[next_index]
                    next_index += 1
                    pending.append((next_feed, executor.submit(fetch, next_feed, directory)))
        if failures:
            sys.stderr.write(f"rss-json.py: {len(failures)} feed(s) failed; fix or remove them from the list:\n")
            sys.stderr.write("\n".join(failures) + "\n")
            return 1
    if output_too_large:
        sys.stderr.write("rss-json.py: output exceeds 64 MiB\n")
        return 1
    ordered = sorted(
        unique.values(), key=lambda record: (record["kind"], record.get("time") or "", record.get("title") or "")
    )
    try:
        output = encode_output(ordered)
    except ValueError as error:
        sys.stderr.write(f"rss-json.py: {error}\n")
        return 1
    sys.stdout.buffer.write(output)
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
