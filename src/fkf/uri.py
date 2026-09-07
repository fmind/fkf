"""Canonical FKF URI parsing and Markdown-link resolution."""

from __future__ import annotations

import posixpath
import re
import string
import unicodedata
from collections.abc import Callable
from dataclasses import dataclass
from enum import StrEnum
from urllib.parse import quote_plus, urlsplit

from markdown_it import MarkdownIt
from markdown_it.rules_inline import autolink, image, link
from markdown_it.rules_inline.state_inline import StateInline
from markdown_it.token import Token

from fkf.errors import InvalidUsageError
from fkf.fields import validate_entity_scheme
from fkf.store import clean_relative


class URIError(InvalidUsageError):
    """A malformed or out-of-bounds FKF URI."""


class Scheme(StrEnum):
    """Built-in URI kinds; entity schemes remain base-defined strings."""

    FILE = "file"
    TAG = "tag"
    EXTERNAL = "external"


_ENTITY_SCHEME = re.compile(r"^([a-z][a-z0-9+.-]*):(.*)$", re.DOTALL)
_FRAGMENT_SAFE = frozenset((string.ascii_letters + string.digits + "._:/@+-").encode())
_HEX = frozenset(string.hexdigits.encode())


@dataclass(frozen=True, slots=True)
class URI:
    """One parsed address in the FKF URI grammar."""

    raw: str
    scheme: Scheme | str
    path: str = ""
    directory: bool = False
    fragment: str = ""
    jq: str = ""
    value: str = ""

    def __str__(self) -> str:
        if self.scheme == Scheme.EXTERNAL:
            return self.value
        if self.scheme != Scheme.FILE:
            return f"{self.scheme}:{encode_fragment(self.value)}"
        rendered = self.path
        if self.directory:
            rendered += "/"
        if self.jq:
            rendered += f"?jq={quote_plus(self.jq, safe='')}"
        if self.fragment:
            rendered += f"#{encode_fragment(self.fragment)}"
        return rendered

    def is_entity(self) -> bool:
        """Return whether the URI names a graph entity rather than a file or URL."""

        return self.scheme not in {Scheme.FILE, Scheme.EXTERNAL}

    def node_uri(self) -> str:
        """Return the graph identity, omitting a file URI's jq read expression."""

        if self.scheme != Scheme.FILE:
            return str(self)
        rendered = self.path + ("/" if self.directory else "")
        if self.fragment:
            rendered += f"#{encode_fragment(self.fragment)}"
        return rendered

    def file_uri(self) -> str:
        """Return the underlying file URI without fragment or jq expression."""

        if self.scheme != Scheme.FILE:
            return str(self)
        return self.path + ("/" if self.directory else "")


def parse_uri(raw: str) -> URI:
    """Parse any published FKF URI form and return its canonical representation."""

    trimmed = raw.strip()
    if not trimmed:
        raise URIError("invalid URI: empty")
    if trimmed.lower().startswith("https://"):
        return _parse_https(raw, trimmed)

    head, _ = _split_uri_extras(trimmed)
    if "://" in head:
        raise URIError(f"invalid URI: {raw!r}: external URIs must use https")

    match = _ENTITY_SCHEME.fullmatch(trimmed)
    if match is not None:
        scheme, encoded = match.groups()
        _validate_entity_scheme(scheme)
        value = encoded.strip()
        if not value:
            raise URIError(f"invalid URI: {raw!r} names no {scheme}")
        try:
            decoded = decode_fragment(value)
        except URIError as error:
            raise URIError(f"invalid URI: {raw!r}: {error}") from error
        return _new_entity_uri(scheme, decoded)

    return _parse_file_uri(trimmed)


def _parse_https(raw: str, trimmed: str) -> URI:
    if any(ord(character) < 0x20 for character in trimmed):
        raise URIError(f"invalid URI: {raw!r}: invalid control character in URL")
    try:
        parsed = urlsplit(trimmed)
    except ValueError as error:
        raise URIError(f"invalid URI: {raw!r}: {error}") from error
    if parsed.scheme.lower() != "https" or not parsed.netloc:
        raise URIError(f"invalid URI: {raw!r} is not an absolute HTTPS URI")
    return URI(raw=trimmed, scheme=Scheme.EXTERNAL, value=trimmed)


def _validate_entity_scheme(scheme: str) -> None:
    try:
        validate_entity_scheme(scheme)
    except ValueError as error:
        raise URIError(f"invalid URI: {scheme!r}: {error}", cause=error) from error


def _new_entity_uri(scheme: str, value: str) -> URI:
    _validate_entity_scheme(scheme)
    normalized = value.strip()
    if not normalized:
        raise URIError(f"invalid URI: names no {scheme}")
    canonical = f"{scheme}:{encode_fragment(normalized)}"
    return URI(raw=canonical, scheme=scheme, value=normalized)


def encode_fragment(value: str) -> str:
    """Percent-encode UTF-8 bytes while retaining readable identity punctuation."""

    return "".join(chr(byte) if byte in _FRAGMENT_SAFE else f"%{byte:02X}" for byte in value.encode())


def decode_fragment(fragment: str) -> str:
    """Decode the exact percent syntax emitted by :func:`encode_fragment`."""

    encoded = fragment.encode()
    decoded = bytearray()
    index = 0
    while index < len(encoded):
        byte = encoded[index]
        if byte != ord("%"):
            decoded.append(byte)
            index += 1
            continue
        if index + 2 >= len(encoded):
            raise URIError(f"fragment {fragment!r} ends in a truncated percent escape")
        digits = encoded[index + 1 : index + 3]
        if any(digit not in _HEX for digit in digits):
            raise URIError(f"fragment {fragment!r} holds an invalid percent escape")
        decoded.append(int(digits.decode(), 16))
        index += 3
    try:
        return decoded.decode()
    except UnicodeDecodeError as error:
        raise URIError(f"fragment {fragment!r} is not valid UTF-8") from error


def _parse_file_uri(trimmed: str) -> URI:
    rest = trimmed
    fragment = ""
    hash_index = rest.rfind("#")
    if hash_index >= 0:
        if hash_index == len(rest) - 1:
            raise URIError(f"invalid URI: {trimmed!r} names no fragment")
        try:
            fragment = decode_fragment(rest[hash_index + 1 :])
        except URIError as error:
            raise URIError(f"invalid URI: {trimmed!r}: {error}") from error
        rest = rest[:hash_index]

    jq = ""
    query_index = rest.find("?")
    if query_index >= 0:
        expression = rest[query_index + 1 :]
        rest = rest[:query_index]
        if not expression.startswith("jq="):
            raise URIError(f"invalid URI: {trimmed!r}: the only supported query is ?jq=<expr>")
        jq = _query_unescape(expression.removeprefix("jq="), trimmed)
        if not jq.strip():
            raise URIError(f"invalid URI: {trimmed!r}: ?jq= names no expression")

    try:
        cleaned = clean_relative(rest)
    except InvalidUsageError as error:
        raise URIError(f"invalid URI: {trimmed!r}: {error}", cause=error) from error
    if cleaned == ".":
        raise URIError(f"invalid URI: {trimmed!r}: the base root is not addressable")
    directory = cleaned.endswith("/")
    path = cleaned.removesuffix("/")
    if directory and (fragment or jq):
        raise URIError(f"invalid URI: {trimmed!r}: a directory has no fragment or jq expression")
    provisional = URI(raw="", scheme=Scheme.FILE, path=path, directory=directory, fragment=fragment, jq=jq)
    return URI(
        raw=str(provisional),
        scheme=Scheme.FILE,
        path=path,
        directory=directory,
        fragment=fragment,
        jq=jq,
    )


def _query_unescape(value: str, raw: str) -> str:
    encoded = value.replace("+", " ").encode()
    decoded = bytearray()
    index = 0
    while index < len(encoded):
        if encoded[index] != ord("%"):
            decoded.append(encoded[index])
            index += 1
            continue
        if index + 2 >= len(encoded) or any(digit not in _HEX for digit in encoded[index + 1 : index + 3]):
            raise URIError(f"invalid URI: {raw!r}: invalid URL escape")
        decoded.append(int(encoded[index + 1 : index + 3].decode(), 16))
        index += 3
    try:
        return decoded.decode()
    except UnicodeDecodeError as error:
        raise URIError(f"invalid URI: {raw!r}: jq expression is not valid UTF-8") from error


def resolve_link(from_relative: str, target: str) -> URI:
    """Resolve a link relative to its authored Markdown page without base escape."""

    trimmed = target.strip()
    if not trimmed or trimmed.startswith("#"):
        raise URIError(f"invalid URI: {target!r} is a same-page anchor")
    head, tail = _split_uri_extras(trimmed)
    if "://" in head or _ENTITY_SCHEME.fullmatch(head) is not None or head.startswith("mailto:"):
        return parse_uri(trimmed)
    if trimmed.startswith("/"):
        return parse_uri(trimmed.removeprefix("/"))
    joined = posixpath.join(posixpath.dirname(from_relative), head)
    return parse_uri(joined + tail)


def _split_uri_extras(value: str) -> tuple[str, str]:
    cuts = [position for delimiter in ("?", "#") if (position := value.find(delimiter)) >= 0]
    cut = min(cuts, default=len(value))
    return value[:cut], value[cut:]


def relative_link(from_relative: str, target: str) -> str:
    """Render a base-relative target as a portable link from one Markdown page."""

    head, tail = _split_uri_extras(target)
    source_directory = posixpath.dirname(from_relative)
    if source_directory == "." or not source_directory:
        return head + tail
    return _relative_path(source_directory, head) + tail


def _relative_path(source: str, target: str) -> str:
    source_parts = source.split("/")
    target_parts = target.split("/")
    common = 0
    while common < len(source_parts) and common < len(target_parts) and source_parts[common] == target_parts[common]:
        common += 1
    parts = [".."] * (len(source_parts) - common) + target_parts[common:]
    return "/".join(parts) if parts else "."


def anchor_slug(heading: str) -> str:
    """Render a heading fragment using FKF's GitHub-compatible rules."""

    visible = _visible_heading_text(heading).strip().lower()
    output: list[str] = []
    for character in visible:
        if character.isalpha() or character.isnumeric() or _is_mark(character) or character in "-_":
            output.append(character)
        elif character == " ":
            output.append("-")
    return "".join(output)


def _is_mark(character: str) -> bool:
    return unicodedata.category(character).startswith("M")


type InlineRule = Callable[[StateInline, bool], bool]


def _tracked(rule: InlineRule) -> InlineRule:
    def wrapped(state: StateInline, silent: bool) -> bool:
        start = state.pos
        before = len(state.tokens)
        matched = rule(state, silent)
        if matched and not silent:
            for token in state.tokens[before:]:
                if token.type in {"link_open", "image"}:
                    token.meta["fkf_source_offset"] = start
        return matched

    return wrapped


_INLINE_MARKDOWN = MarkdownIt("commonmark", {"inline_definitions": True, "store_labels": True})
_INLINE_MARKDOWN.inline.ruler.at("link", _tracked(link))
_INLINE_MARKDOWN.inline.ruler.at("image", _tracked(image))
_INLINE_MARKDOWN.inline.ruler.at("autolink", _tracked(autolink))


def _visible_heading_text(value: str) -> str:
    tokens = _INLINE_MARKDOWN.parse(f"# {value}\n")
    inline = next((token for token in tokens if token.type == "inline"), None)
    if inline is None or inline.children is None:
        return value
    return _rendered_inline_tokens(inline.children)


def _rendered_inline_tokens(tokens: list[Token]) -> str:
    rendered: list[str] = []
    for token in tokens:
        if token.type == "html_inline":
            continue
        if token.type in {"softbreak", "hardbreak"}:
            rendered.append(" ")
        elif token.type in {"text", "code_inline"}:
            rendered.append(token.content)
        elif token.type == "image":
            rendered.append(_rendered_inline_tokens(token.children or []))
    return "".join(rendered)


__all__ = [
    "URI",
    "Scheme",
    "URIError",
    "anchor_slug",
    "decode_fragment",
    "encode_fragment",
    "parse_uri",
    "relative_link",
    "resolve_link",
]
