"""CommonMark parsing and local authored-page validation for FKF layers."""

from __future__ import annotations

import html
import math
import re
import unicodedata
from collections.abc import Iterable, Mapping, Sequence
from dataclasses import dataclass, field
from datetime import UTC, date, datetime
from enum import StrEnum
from pathlib import PurePosixPath
from typing import Any

import yaml
from markdown_it.token import Token
from yaml.nodes import MappingNode, Node, ScalarNode, SequenceNode

from fkf.errors import InvalidUsageError
from fkf.fields import scalar_string
from fkf.uri import _INLINE_MARKDOWN, _rendered_inline_tokens, anchor_slug


class MarkdownError(InvalidUsageError):
    """A Markdown page cannot be parsed without guessing."""


_TAG_PATTERN = re.compile(r"^[a-z0-9][a-z0-9-]*$")
_SLUG_PATTERN = re.compile(r"^[a-z0-9][a-z0-9._-]*$")
_ISO_DATE_PATTERN = re.compile(r"^\d{4}-\d{2}-\d{2}$")
_YAML_BOOL_TAG = "tag:yaml.org,2002:bool"
_MAX_FRONTMATTER_DEPTH = 128
_MAX_FRONTMATTER_EXPANDED_NODES = 100_000
_MAX_FRONTMATTER_ALIAS_VISITS = 10_000
_MAX_FRONTMATTER_EXPANDED_SCALAR_BYTES = 4 << 20


class _YAML12SafeLoader(yaml.SafeLoader):
    """PyYAML's safe loader with YAML 1.2 true/false boolean resolution."""


_YAML12SafeLoader.yaml_implicit_resolvers = {
    key: [(tag, pattern) for tag, pattern in resolvers if tag != _YAML_BOOL_TAG]
    for key, resolvers in yaml.SafeLoader.yaml_implicit_resolvers.items()
}
_YAML12SafeLoader.add_implicit_resolver(
    _YAML_BOOL_TAG,
    re.compile(r"^(?:true|True|TRUE|false|False|FALSE)$"),
    list("tTfF"),
)


_INVISIBLE_RUNES = {
    "\u00ad": "soft hyphen",
    "\u200b": "zero-width space",
    "\u200c": "zero-width non-joiner",
    "\u200d": "zero-width joiner",
    "\u200e": "left-to-right mark",
    "\u200f": "right-to-left mark",
    "\u202a": "left-to-right embedding",
    "\u202b": "right-to-left embedding",
    "\u202c": "pop directional formatting",
    "\u202d": "left-to-right override",
    "\u202e": "right-to-left override",
    "\u2060": "word joiner",
    "\u2066": "left-to-right isolate",
    "\u2067": "right-to-left isolate",
    "\u2068": "first strong isolate",
    "\u2069": "pop directional isolate",
    "\ufeff": "zero-width no-break space",
}

PROJECT_STATUSES = ("active", "paused", "done")


@dataclass(frozen=True, slots=True)
class Link:
    """One rendered Markdown link and the extractor that found it."""

    target: str
    line: int
    via: str
    title: str = field(default="", metadata={"json": "title,omitempty"})


@dataclass(frozen=True, slots=True)
class Heading:
    """One rendered heading and its addressable anchor."""

    level: int
    text: str
    anchor: str
    line: int


@dataclass(frozen=True, slots=True)
class Page:
    """One parsed authored Markdown file."""

    uri: str
    slug: str
    type: str = field(default="", metadata={"json": "type,omitempty"})
    title: str = field(default="", metadata={"json": "title,omitempty"})
    description: str = field(default="", metadata={"json": "description,omitempty"})
    status: str = field(default="", metadata={"json": "status,omitempty"})
    date: str = field(default="", metadata={"json": "date,omitempty"})
    valid_from: str = field(default="", metadata={"json": "valid_from,omitempty"})
    valid_until: str = field(default="", metadata={"json": "valid_until,omitempty"})
    tags: tuple[str, ...] = field(default=(), metadata={"json": "tags,omitempty"})
    aliases: tuple[str, ...] = field(default=(), metadata={"json": "aliases,omitempty"})
    relations: Mapping[str, tuple[str, ...]] = field(default_factory=dict, metadata={"json": "relations,omitempty"})
    frontmatter: Mapping[str, Any] = field(default_factory=dict, metadata={"json": "frontmatter,omitempty"})
    body: str = field(default="", metadata={"json": "-"})
    headings: tuple[Heading, ...] = field(default=(), metadata={"json": "headings,omitempty"})
    links: tuple[Link, ...] = field(default=(), metadata={"json": "links,omitempty"})
    updated: str = field(default="", metadata={"json": "updated,omitempty"})
    bytes: int = 0

    def valid_at(self, as_of: str) -> bool:
        """Return whether the inclusive authored validity window admits an ISO date."""

        if not _valid_iso_date(as_of):
            return False
        if self.valid_from and not _valid_iso_date(self.valid_from):
            return False
        if self.valid_until and not _valid_iso_date(self.valid_until):
            return False
        return (not self.valid_from or self.valid_from <= as_of) and (not self.valid_until or as_of <= self.valid_until)


class Severity(StrEnum):
    ERROR = "error"
    WARNING = "warning"


@dataclass(frozen=True, slots=True)
class Issue:
    uri: str
    severity: Severity
    message: str
    line: int = field(default=0, metadata={"json": "line,omitempty"})


@dataclass(frozen=True, slots=True)
class ValidationReport:
    layer: str
    pages: int
    strict: bool
    errors: int
    warnings: int
    issues: tuple[Issue, ...]
    ok: bool


def parse_page(uri: str, data: bytes, modified: datetime | None = None) -> Page:
    """Parse one page while retaining every unknown frontmatter key."""

    try:
        text = data.decode()
    except UnicodeDecodeError as error:
        raise MarkdownError(f"{uri}: Markdown is not valid UTF-8") from error
    frontmatter_text, body, body_line = _split_frontmatter(text)
    authored_frontmatter = _load_frontmatter(uri, frontmatter_text)
    # Known textual roles use the authored YAML type, while the retained envelope
    # mirrors Go's JSON view (notably date-only time.Time values).
    frontmatter = _normalize_frontmatter(uri, authored_frontmatter)
    relations = _frontmatter_relations(authored_frontmatter)
    headings, links = _extract_markdown(body, body_line)
    title = _frontmatter_string(authored_frontmatter, "title")
    if not title and headings:
        title = headings[0].text
    updated = ""
    if modified is not None:
        normalized_modified = modified.replace(tzinfo=UTC) if modified.tzinfo is None else modified.astimezone(UTC)
        updated = _format_rfc3339(normalized_modified)
    return Page(
        uri=uri,
        slug=PurePosixPath(uri).name.removesuffix(".md"),
        type=_frontmatter_string(authored_frontmatter, "type"),
        title=title,
        description=_frontmatter_string(authored_frontmatter, "description"),
        status=_frontmatter_string(authored_frontmatter, "status"),
        date=_frontmatter_string(authored_frontmatter, "date"),
        valid_from=_frontmatter_string(authored_frontmatter, "valid_from"),
        valid_until=_frontmatter_string(authored_frontmatter, "valid_until"),
        tags=_frontmatter_strings(authored_frontmatter, "tags"),
        aliases=_frontmatter_strings(authored_frontmatter, "aliases"),
        relations=relations,
        frontmatter=frontmatter,
        body=body,
        headings=headings,
        links=links,
        updated=updated,
        bytes=len(data),
    )


def _split_frontmatter(text: str) -> tuple[str, str, int]:
    if not text.startswith(("---\n", "---\r\n")):
        return "", text, 1
    lines = text.split("\n")
    for index in range(1, len(lines)):
        if lines[index].rstrip("\r") != "---":
            continue
        return "\n".join(lines[1:index]), "\n".join(lines[index + 1 :]), index + 2
    raise MarkdownError("frontmatter opening delimiter has no closing delimiter")


def _load_frontmatter(uri: str, source: str) -> dict[str, Any]:
    if not source:
        return {}
    loader = _YAML12SafeLoader(source)
    try:
        node = loader.get_single_node()
        if node is None:
            return {}
        _validate_frontmatter_graph(uri, node)
        value = loader.construct_document(node)
    except RecursionError as error:
        raise MarkdownError(f"{uri}: parse YAML frontmatter: nesting exceeds the parser limit") from error
    except (ValueError, OverflowError, yaml.YAMLError) as error:
        raise MarkdownError(f"{uri}: parse YAML frontmatter: {error}") from error
    finally:
        loader.dispose()
    if value is None:
        return {}
    if not isinstance(value, dict) or any(not isinstance(key, str) for key in value):
        raise MarkdownError(f"{uri}: frontmatter must be a mapping with string keys")
    return value


def _validate_frontmatter_graph(uri: str, root: Node) -> None:
    """Bound the expanded YAML graph before aliases become recursive Python objects."""

    stack: list[tuple[Node, int, bool]] = [(root, 1, False)]
    active: set[int] = set()
    seen: set[int] = set()
    expanded = 0
    expanded_scalar_bytes = 0
    alias_visits = 0
    while stack:
        node, depth, leaving = stack.pop()
        identity = id(node)
        if leaving:
            active.remove(identity)
            continue
        if identity in active:
            raise MarkdownError(f"{uri}: recursive YAML alias in frontmatter")
        if depth > _MAX_FRONTMATTER_DEPTH:
            raise MarkdownError(f"{uri}: YAML frontmatter exceeds the {_MAX_FRONTMATTER_DEPTH}-level depth limit")
        expanded += 1
        if expanded > _MAX_FRONTMATTER_EXPANDED_NODES:
            raise MarkdownError(f"{uri}: YAML alias expansion exceeds the {_MAX_FRONTMATTER_EXPANDED_NODES}-node limit")
        if identity in seen:
            alias_visits += 1
            if alias_visits > _MAX_FRONTMATTER_ALIAS_VISITS:
                raise MarkdownError(
                    f"{uri}: YAML alias expansion exceeds the {_MAX_FRONTMATTER_ALIAS_VISITS}-visit limit"
                )
        else:
            seen.add(identity)

        if isinstance(node, MappingNode):
            _validate_unique_mapping_keys(uri, node)
            children = tuple(item for pair in node.value for item in pair)
        elif isinstance(node, SequenceNode):
            children = tuple(node.value)
        elif isinstance(node, ScalarNode):
            children = ()
            expanded_scalar_bytes += len(node.value.encode())
            if expanded_scalar_bytes > _MAX_FRONTMATTER_EXPANDED_SCALAR_BYTES:
                raise MarkdownError(
                    f"{uri}: YAML alias expansion exceeds the "
                    f"{_MAX_FRONTMATTER_EXPANDED_SCALAR_BYTES}-byte scalar limit"
                )
        else:
            raise MarkdownError(f"{uri}: unsupported YAML node in frontmatter")
        if not children:
            continue
        active.add(identity)
        stack.append((node, depth, True))
        stack.extend((child, depth + 1, False) for child in reversed(children))


def _validate_unique_mapping_keys(uri: str, node: MappingNode) -> None:
    """Reject PyYAML's last-value-wins behavior, matching yaml.v3."""

    seen: dict[tuple[type[Node], str], int] = {}
    for key, _value in node.value:
        if not isinstance(key.value, str):
            continue
        identity = (type(key), key.value)
        line = key.start_mark.line + 1
        if first_line := seen.get(identity):
            raise MarkdownError(
                f"{uri}: duplicate YAML mapping key {key.value!r} at line {line} (first defined at line {first_line})"
            )
        seen[identity] = line


def _normalize_frontmatter(uri: str, value: dict[str, Any]) -> dict[str, Any]:
    """Project safe YAML values onto the deterministic Go-compatible JSON shape."""

    normalized = _normalize_frontmatter_value(uri, "frontmatter", value)
    if not isinstance(normalized, dict):  # The loader owns this root invariant.
        raise AssertionError("frontmatter normalization changed the root shape")
    return normalized


def _normalize_frontmatter_value(uri: str, path: str, value: Any) -> Any:
    if value is None or isinstance(value, str | bool | int):
        return value
    if isinstance(value, float):
        if not math.isfinite(value):
            raise MarkdownError(f"{uri}: {path} contains a non-finite float outside the JSON boundary")
        return value
    if isinstance(value, datetime):
        return _format_frontmatter_datetime(value)
    if isinstance(value, date):
        return f"{value.isoformat()}T00:00:00Z"
    if isinstance(value, Mapping):
        output: dict[str, Any] = {}
        for key, item in value.items():
            if not isinstance(key, str):
                raise MarkdownError(f"{uri}: {path} has a non-string YAML mapping key {key!r}")
            output[key] = _normalize_frontmatter_value(uri, f"{path}.{key}", item)
        return output
    if isinstance(value, list):
        return [_normalize_frontmatter_value(uri, f"{path}[{index}]", item) for index, item in enumerate(value)]
    raise MarkdownError(f"{uri}: {path} contains unsupported YAML value of type {type(value).__name__}")


def _frontmatter_string(frontmatter: Mapping[str, Any], key: str) -> str:
    return _frontmatter_scalar_string(frontmatter[key]) if key in frontmatter else ""


def _frontmatter_strings(frontmatter: Mapping[str, Any], key: str) -> tuple[str, ...]:
    value = frontmatter.get(key)
    if isinstance(value, list):
        return tuple(rendered for item in value if (rendered := _frontmatter_scalar_string(item)))
    if isinstance(value, str):
        return tuple(part for part in re.split(r"[ ,]+", value) if part)
    return ()


def _frontmatter_scalar_string(value: Any) -> str:
    if isinstance(value, bool):
        return scalar_string(value) or ""
    if isinstance(value, int):
        return str(value)
    if isinstance(value, datetime):
        if value.hour == value.minute == value.second == value.microsecond == 0:
            return value.date().isoformat()
        return _format_rfc3339(value)
    if isinstance(value, date):
        return value.isoformat()
    return _scalar_string_allow_nonfinite(value)


def _frontmatter_relations(frontmatter: Mapping[str, Any]) -> dict[str, tuple[str, ...]]:
    if "relations" not in frontmatter:
        return {}
    raw = frontmatter["relations"]
    if not isinstance(raw, dict) or any(not isinstance(key, str) for key in raw):
        raise MarkdownError("frontmatter relations must be a mapping of field names to URI lists")
    relations: dict[str, tuple[str, ...]] = {}
    for name in sorted(raw):
        items = raw[name]
        if not isinstance(items, list):
            raise MarkdownError(f"frontmatter relations.{name} must be a URI list")
        values: list[str] = []
        for item in items:
            value = _relation_scalar_string(item)
            if not value:
                raise MarkdownError(f"frontmatter relations.{name} must contain only non-empty URI strings")
            values.append(value)
        relations[name] = tuple(values)
    return relations


def _relation_scalar_string(value: Any) -> str:
    # YAML integers remain ints, unlike decoded JSON numbers, so Go's relation boundary
    # refuses them while accepting strings, booleans, and float64 values.
    return "" if isinstance(value, int) and not isinstance(value, bool) else _scalar_string_allow_nonfinite(value)


def _scalar_string_allow_nonfinite(value: Any) -> str:
    if isinstance(value, float) and not math.isfinite(value):
        if math.isnan(value):
            return "NaN"
        return "+Inf" if value > 0 else "-Inf"
    return scalar_string(value) or ""


def _format_rfc3339(value: datetime) -> str:
    if value.tzinfo is None:
        value = value.replace(tzinfo=UTC)
    rendered = value.isoformat(timespec="seconds")
    return rendered.removesuffix("+00:00") + "Z" if rendered.endswith("+00:00") else rendered


def _format_frontmatter_datetime(value: datetime) -> str:
    """Match time.Time's RFC3339Nano JSON form, including trimmed fractions."""

    normalized = value.replace(tzinfo=UTC) if value.tzinfo is None else value
    rendered = normalized.isoformat(timespec="microseconds" if normalized.microsecond else "seconds")
    timestamp, offset = rendered[:-6], rendered[-6:]
    if normalized.microsecond:
        timestamp = timestamp.rstrip("0")
    return timestamp + ("Z" if offset == "+00:00" else offset)


def _extract_markdown(body: str, first_line: int) -> tuple[tuple[Heading, ...], tuple[Link, ...]]:
    environment: dict[str, Any] = {}
    tokens = _INLINE_MARKDOWN.parse(body, environment)
    headings: list[Heading] = []
    links: list[Link] = []
    used_anchors: set[str] = set()
    for index, token in enumerate(tokens):
        if token.type == "heading_open" and token.map is not None and index + 1 < len(tokens):
            inline = tokens[index + 1]
            text = _rendered_inline_tokens(inline.children or []).strip()
            base = anchor_slug(text)
            anchor = base
            suffix = 1
            while anchor in used_anchors:
                anchor = f"{base}-{suffix}"
                suffix += 1
            used_anchors.add(anchor)
            headings.append(
                Heading(
                    level=int(token.tag.removeprefix("h")), text=text, anchor=anchor, line=first_line + token.map[0]
                )
            )
        if token.type == "inline" and token.children is not None and token.map is not None:
            links.extend(_extract_inline_links(token, first_line + token.map[0]))
        if token.type == "definition" and token.map is not None:
            target = _rendered_markdown_value(str(token.meta.get("url", "")))
            if target:
                links.append(Link(target=target, line=first_line + token.map[0], via="markdown-reference"))
    return tuple(headings), tuple(links)


def _extract_inline_links(inline: Token, block_line: int) -> list[Link]:
    children = inline.children or []
    links: list[Link] = []
    for index, token in enumerate(children):
        if token.type == "image":
            if "label" in token.meta:
                continue
            target = _rendered_markdown_value(str(token.attrs.get("src", "")))
            if target:
                links.append(
                    Link(
                        target=target,
                        title=_rendered_markdown_value(str(token.attrs.get("title", ""))),
                        line=_inline_source_line(inline.content, token, block_line),
                        via="markdown-inline",
                    )
                )
            continue
        if token.type != "link_open" or "label" in token.meta:
            continue
        href = str(token.attrs.get("href", ""))
        if token.info == "auto":
            if href.lower().startswith("mailto:"):
                continue
            visible = children[index + 1].content if index + 1 < len(children) else ""
            target = _rendered_markdown_value(visible)
            via = "markdown-autolink"
        else:
            target = _rendered_markdown_value(href)
            via = "markdown-inline"
        if target:
            links.append(
                Link(
                    target=target,
                    title=_rendered_markdown_value(str(token.attrs.get("title", ""))),
                    line=_inline_source_line(inline.content, token, block_line),
                    via=via,
                )
            )
    return links


def _inline_source_line(source: str, token: Token, block_line: int) -> int:
    offset = token.meta.get("fkf_source_offset", 0)
    return block_line + source[:offset].count("\n") if isinstance(offset, int) else block_line


def _rendered_markdown_value(value: str) -> str:
    return html.unescape(_markdown_unescape(value.strip()))


def _markdown_unescape(value: str) -> str:
    output: list[str] = []
    cursor = 0
    while cursor < len(value):
        if value[cursor] == "\\" and cursor + 1 < len(value) and value[cursor + 1] in _ASCII_PUNCTUATION:
            cursor += 1
        output.append(value[cursor])
        cursor += 1
    return "".join(output)


_ASCII_PUNCTUATION = "!\"#$%&'()*+,-./:;<=>?@[\\]^_`{|}~"


def find_invisible(value: str) -> tuple[str, str] | None:
    """Return the first invisible character and its name, when present."""

    for character in value:
        if name := _INVISIBLE_RUNES.get(character):
            return character, name
    return None


def _authored_rune_problem(character: str, *, allow_layout: bool) -> str | None:
    if name := _INVISIBLE_RUNES.get(character):
        return f"contains invisible character U+{ord(character):04X} ({name})"
    layout = character in "\n\r\t"
    if unicodedata.category(character) == "Cc" and (not allow_layout or not layout):
        return f"contains control character U+{ord(character):04X}"
    return None


def _authored_text_problem(value: str, *, allow_layout: bool) -> str | None:
    return next(
        (problem for character in value if (problem := _authored_rune_problem(character, allow_layout=allow_layout))),
        None,
    )


def markdown_literal_text(value: str) -> str:
    """Serialize an authored scalar as one line of literal, structure-free Markdown."""

    output: list[str] = []
    for character in " ".join(value.split()):
        if _authored_rune_problem(character, allow_layout=False):
            output.append(f"U&#43;{ord(character):04X}")
        elif character in "\\`*_{}[]<>()#+-.!|~&:@":
            output.append(f"&#{ord(character)};")
        else:
            output.append(character)
    return "".join(output)


def markdown_code_text(value: str) -> str:
    """Serialize a tag inside one line of Markdown code without adding structure."""

    output: list[str] = []
    for character in " ".join(value.split()):
        if _authored_rune_problem(character, allow_layout=False):
            output.append(f"U+{ord(character):04X}")
        elif character == "`":
            output.append("&#96;")
        elif character == "<":
            output.append("&lt;")
        else:
            output.append(character)
    return "".join(output)


@dataclass(slots=True)
class _ReportBuilder:
    layer: str
    strict: bool
    issues: list[Issue] = field(default_factory=list)

    def warn(self, uri: str, line: int, message: str) -> None:
        severity = Severity.ERROR if self.strict else Severity.WARNING
        self.issues.append(Issue(uri=uri, severity=severity, line=line, message=message))

    def fail(self, uri: str, line: int, message: str) -> None:
        self.issues.append(Issue(uri=uri, severity=Severity.ERROR, line=line, message=message))


def validate_pages(
    pages: Sequence[Page],
    *,
    layer: str,
    require_status: bool,
    strict: bool,
    nested: Iterable[str] = (),
) -> ValidationReport:
    """Apply page-local Markdown rules to one flat layer."""

    report = _ReportBuilder(layer=layer, strict=strict)
    for uri in nested:
        report.fail(uri, 0, f"the {layer} layer is flat: move this page to {layer}/<slug>.md and classify it with tags")
    seen: dict[str, str] = {}
    for page in pages:
        _validate_page(report, page, layer, require_status, seen)
    issues = tuple(sorted(report.issues, key=lambda issue: (issue.uri, issue.line)))
    errors = sum(issue.severity is Severity.ERROR for issue in issues)
    warnings = len(issues) - errors
    return ValidationReport(
        layer=layer,
        pages=len(pages),
        strict=strict,
        errors=errors,
        warnings=warnings,
        issues=issues,
        ok=errors == 0,
    )


def _validate_page(
    report: _ReportBuilder,
    page: Page,
    layer: str,
    require_status: bool,
    seen: dict[str, str],
) -> None:
    normalized_slug = page.slug.lower()
    if previous := seen.get(normalized_slug):
        report.fail(page.uri, 0, f"slug collides with {previous}; slugs are unique per layer")
    seen[normalized_slug] = page.uri

    structural = layer == "wiki" and page.slug in {"index", "log"}
    if not page.type and not structural:
        report.warn(page.uri, 0, "frontmatter `type` is required on write (OKF v0.2); reading stays permissive")
    if not page.title:
        report.warn(page.uri, 0, "no title: add frontmatter `title` or a level-one heading")
    if require_status and page.status not in PROJECT_STATUSES:
        report.fail(
            page.uri,
            0,
            f"frontmatter `status` is required and must be active, paused, or done (got {page.status!r})",
        )
    _validate_page_validity(report, page)
    if not page.tags and not structural:
        report.warn(page.uri, 0, "no tags: the page is absent from tag-filtered navigation and harder to discover")
    for tag in page.tags:
        if _TAG_PATTERN.fullmatch(tag) is None:
            report.warn(page.uri, 0, f"tag {tag!r} must be lowercase kebab-case")
    if _SLUG_PATTERN.fullmatch(page.slug) is None:
        report.fail(page.uri, 0, f"slug {page.slug!r} must be lowercase letters, digits, dot, underscore, and hyphen")
    if problem := _authored_text_problem(page.body, allow_layout=True):
        report.fail(page.uri, 0, f"body {problem}; remove it before writing")
    for heading in page.headings:
        if not anchor_slug(heading.text):
            report.fail(page.uri, heading.line, "heading has no addressable anchor; add a letter or number")
    _validate_frontmatter_text(report, page)
    if layer == "wiki":
        _validate_wiki_special_pages(report, page)


def _validate_page_validity(report: _ReportBuilder, page: Page) -> None:
    for name, value in (("valid_from", page.valid_from), ("valid_until", page.valid_until)):
        if value and not _valid_iso_date(value):
            report.fail(page.uri, 0, f"frontmatter `{name}` must be an absolute YYYY-MM-DD date (got {value!r})")
    if page.valid_from and page.valid_until and page.valid_from > page.valid_until:
        report.fail(
            page.uri,
            0,
            f"frontmatter `valid_from` {page.valid_from} is after `valid_until` {page.valid_until}",
        )


def _valid_iso_date(value: str) -> bool:
    if _ISO_DATE_PATTERN.fullmatch(value) is None:
        return False
    try:
        return date.fromisoformat(value).isoformat() == value
    except ValueError:
        return False


def _validate_frontmatter_text(report: _ReportBuilder, page: Page) -> None:
    def inspect(name: str, value: Any) -> None:
        if isinstance(value, str):
            if problem := _authored_text_problem(value, allow_layout=False):
                report.fail(page.uri, 0, f"frontmatter {name} {problem}; remove it before writing")
        elif isinstance(value, list):
            for index, item in enumerate(value):
                inspect(f"{name}[{index}]", item)
        elif isinstance(value, dict):
            for key in sorted(value, key=str):
                rendered_key = str(key)
                if problem := _authored_text_problem(rendered_key, allow_layout=False):
                    report.fail(page.uri, 0, f"frontmatter key {rendered_key!r} {problem}; remove it before writing")
                inspect(f"{name}.{rendered_key}", value[key])

    for field_name in sorted(page.frontmatter):
        if problem := _authored_text_problem(field_name, allow_layout=False):
            report.fail(page.uri, 0, f"frontmatter key {field_name!r} {problem}; remove it before writing")
        inspect(field_name, page.frontmatter[field_name])


def _validate_wiki_special_pages(report: _ReportBuilder, page: Page) -> None:
    if page.slug == "index":
        if not page.headings or page.headings[0].level != 1:
            report.fail(page.uri, 0, "wiki/index.md must start with a level-one heading")
        return
    if page.slug != "log":
        return
    previous = ""
    seen: dict[str, int] = {}
    for heading in page.headings:
        if heading.level != 2:
            continue
        if _ISO_DATE_PATTERN.fullmatch(heading.text) is None:
            report.fail(page.uri, heading.line, f"wiki/log.md level-two headings are ISO dates; got {heading.text!r}")
            continue
        if first := seen.get(heading.text):
            report.fail(
                page.uri, heading.line, f"wiki/log.md repeats the date {heading.text}, first seen at line {first}"
            )
        seen[heading.text] = heading.line
        if previous and heading.text > previous:
            report.fail(page.uri, heading.line, f"wiki/log.md is newest first; {heading.text} follows {previous}")
        previous = heading.text


__all__ = [
    "PROJECT_STATUSES",
    "Heading",
    "Issue",
    "Link",
    "MarkdownError",
    "Page",
    "Severity",
    "ValidationReport",
    "find_invisible",
    "markdown_code_text",
    "markdown_literal_text",
    "parse_page",
    "validate_pages",
]
