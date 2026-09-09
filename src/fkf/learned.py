"""Deterministic task-lesson backlog over authored Markdown."""

from __future__ import annotations

from dataclasses import dataclass
from typing import cast

from markdown_it import MarkdownIt
from markdown_it.token import Token

from fkf.base import Base
from fkf.errors import InvalidUsageError
from fkf.fields import scalar_string
from fkf.listings import list_tasks
from fkf.markdown import Page
from fkf.pages import load_markdown_layer
from fkf.process import Cancellation, CommandCanceledError
from fkf.query import Window
from fkf.store import TASK_TRACE_FILE, Layer
from fkf.uri import resolve_link

_MARKDOWN = MarkdownIt("commonmark")


@dataclass(frozen=True, slots=True)
class LearnedBullet:
    """One exact list item from a task trace's ``Learned`` section."""

    trace: str
    text: str
    harvested: bool


@dataclass(frozen=True, slots=True)
class LearnedListing:
    """The visible lessons and whole-window harvested backlog counts."""

    window: Window
    bullets: tuple[LearnedBullet, ...]
    harvested: int
    unharvested: int


def _check_cancel(cancel: Cancellation | None) -> None:
    if cancel is not None and cancel.is_set():
        raise CommandCanceledError("command canceled")


def _inline_text(token: Token) -> str:
    rendered: list[str] = []
    for child in token.children or ():
        if child.type in {"text", "code_inline"}:
            rendered.append(child.content)
        elif child.type in {"softbreak", "hardbreak"}:
            rendered.append(" ")
        elif child.type == "image":
            rendered.append(_inline_text(child))
    return "".join(rendered)


def _item_text(tokens: list[Token], start: int) -> str:
    """Render a list item while excluding any nested list's own bullets."""
    item = tokens[start]
    nested_lists = 0
    parts: list[str] = []
    for token in tokens[start + 1 :]:
        if token.type == "list_item_close" and token.level == item.level:
            break
        if token.type in {"bullet_list_open", "ordered_list_open"}:
            nested_lists += 1
            continue
        if token.type in {"bullet_list_close", "ordered_list_close"}:
            nested_lists -= 1
            continue
        if token.type == "inline" and nested_lists == 0:
            parts.append(_inline_text(token))
    return " ".join(" ".join(parts).split())


def learned_bullets(page: Page, *, cancel: Cancellation | None = None) -> tuple[str, ...]:
    """Extract unordered items under headings named exactly ``Learned``."""
    _check_cancel(cancel)
    # Parsed pages already carry rendered CommonMark headings. Most imported traces
    # have no Learned section, so avoid parsing their full transcript a second time.
    if not any(heading.text == "Learned" for heading in page.headings):
        return ()
    tokens = _MARKDOWN.parse(page.body)
    active_level = 0
    list_kinds: list[str] = []
    bullets: list[str] = []
    for index, token in enumerate(tokens):
        _check_cancel(cancel)
        if token.type == "heading_open":
            level = int(token.tag.removeprefix("h"))
            inline = tokens[index + 1] if index + 1 < len(tokens) else None
            heading = _inline_text(inline).strip() if inline is not None and inline.type == "inline" else ""
            if heading == "Learned":
                active_level = level
            elif active_level and level <= active_level:
                active_level = 0
            continue
        if token.type in {"bullet_list_open", "ordered_list_open"}:
            list_kinds.append(token.type)
            continue
        if token.type in {"bullet_list_close", "ordered_list_close"}:
            list_kinds.pop()
            continue
        if (
            active_level
            and token.type == "list_item_open"
            and list_kinds[-1:] == ["bullet_list_open"]
            and (text := _item_text(tokens, index))
        ):
            bullets.append(text)
    return tuple(bullets)


def cited_task_traces(page: Page, *, cancel: Cancellation | None = None) -> frozenset[str]:
    """Resolve authored source citations to exact task files, ignoring fragments."""
    cited: set[str] = set()
    values = page.frontmatter.get("sources")
    if not isinstance(values, list):
        return frozenset()
    for item in values:
        _check_cancel(cancel)
        candidate = scalar_string(item)
        if candidate is None:
            continue
        try:
            resolved = resolve_link(page.uri, candidate)
        except InvalidUsageError:
            continue
        target = resolved.node_uri().partition("#")[0]
        if target.endswith(f"/{TASK_TRACE_FILE}"):
            cited.add(target)
    return frozenset(cited)


def _cited_traces(base: Base, cancel: Cancellation | None) -> frozenset[str]:
    cited: set[str] = set()
    for layer in (Layer.WIKI, Layer.PROJECTS):
        if not base.store.enabled(layer):
            continue
        pages, _nested = load_markdown_layer(base, layer, cancel=cancel)
        for page in pages:
            _check_cancel(cancel)
            cited.update(cited_task_traces(page, cancel=cancel))
    return frozenset(cited)


def list_learned(
    base: Base,
    window: Window | None = None,
    *,
    only_unharvested: bool = False,
    cancel: Cancellation | None = None,
) -> LearnedListing:
    """List exact task lessons and mark whether an authored page cites their trace."""
    _check_cancel(cancel)
    selected_window = window or Window()
    traces = list_tasks(base, selected_window, cancel=cancel)
    cited = _cited_traces(base, cancel)
    bullets: list[LearnedBullet] = []
    harvested = 0
    unharvested = 0
    for trace in traces.traces:
        _check_cancel(cancel)
        is_harvested = trace.uri in cited
        for text in learned_bullets(cast(Page, trace.page), cancel=cancel):
            if is_harvested:
                harvested += 1
            else:
                unharvested += 1
            if only_unharvested and is_harvested:
                continue
            bullets.append(LearnedBullet(trace.uri, text, is_harvested))
    return LearnedListing(selected_window, tuple(bullets), harvested, unharvested)


__all__ = ["LearnedBullet", "LearnedListing", "learned_bullets", "list_learned"]
