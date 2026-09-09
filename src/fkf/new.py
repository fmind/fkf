"""Strict scaffolds for task traces, authored pages, and source helpers."""

from __future__ import annotations

import re
import unicodedata
from dataclasses import dataclass
from datetime import datetime
from enum import StrEnum
from pathlib import Path
from typing import Final

from fkf.base import Base
from fkf.io import atomic_write
from fkf.markdown import Severity, markdown_literal_text, parse_page, validate_pages
from fkf.process import Cancellation, check_cancel
from fkf.source_runtime import ensure_sources_dir
from fkf.store import BASE_FILE_MODE, Layer, validate_within_root

_SLUG_PATTERN: Final = re.compile(r"^[a-z0-9][a-z0-9._-]*$")
_TAG_PATTERN: Final = re.compile(r"^[a-z0-9][a-z0-9-]*$")


class NewKind(StrEnum):
    TASK = "task"
    PROJECT = "project"
    WIKI = "wiki"
    HELPER = "helper"


@dataclass(frozen=True, slots=True)
class NewRequest:
    kind: NewKind
    slug: str
    title: str = ""
    type: str = ""
    tags: tuple[str, ...] = ()
    now: datetime | None = None


@dataclass(frozen=True, slots=True)
class NewResult:
    kind: NewKind
    path: Path
    created: bool
    message: str
    uri: str = ""
    run: tuple[str, ...] = ()
    requires: tuple[str, ...] = ()


def parse_new_kind(value: str) -> NewKind:
    aliases = {"t": NewKind.TASK, "p": NewKind.PROJECT, "w": NewKind.WIKI, "h": NewKind.HELPER}
    normalized = value.strip().lower()
    if normalized in aliases:
        return aliases[normalized]
    try:
        return NewKind(normalized)
    except ValueError as error:
        raise ValueError(f"unknown kind {value!r}; expected task, project, wiki, or helper") from error


def _title_from_slug(slug: str) -> str:
    return " ".join(part[:1].upper() + part[1:] for part in re.split(r"[-_/]+", slug) if part)


def _normalize_tags(tags: tuple[str, ...]) -> tuple[str, ...]:
    selected: list[str] = []
    seen: set[str] = set()
    for declared in tags:
        for part in declared.split(","):
            tag = part.strip()
            if not tag:
                continue
            if _TAG_PATTERN.fullmatch(tag) is None:
                raise ValueError(f"tag {tag!r} must be lowercase kebab-case")
            if tag not in seen:
                selected.append(tag)
                seen.add(tag)
    if not selected:
        raise ValueError("at least one tag is required when writing a project or wiki page")
    return tuple(selected)


def _validate_text(name: str, value: str) -> None:
    if not value:
        raise ValueError(f"{name} is required")
    for character in value:
        category = unicodedata.category(character)
        if category in {"Cc", "Cf"}:
            raise ValueError(f"{name} contains control or invisible character U+{ord(character):04X}")


def _task_template(title: str) -> bytes:
    safe = markdown_literal_text(title)
    return f"""# {safe}

## 1. {safe}

- **Request**: {safe}
- **Trace**:
  1. <!-- Record each completed step and its outcome. -->
- **Files**:
  - <!-- List each changed file, or write none. -->
- **Verification**:
  - <!-- Record each exact command and its result. -->

## Learned

<!-- Add durable lessons as bullets, or leave this section empty. -->
""".encode()


def _yaml_quoted(value: str) -> str:
    # Double-quoted JSON strings are a valid deterministic YAML scalar.
    import json

    return json.dumps(value, ensure_ascii=False)


def _page_template(*, page_type: str, title: str, tags: tuple[str, ...], status: str = "", body: str = "") -> bytes:
    lines = ["---", f"type: {_yaml_quoted(page_type)}", f"title: {_yaml_quoted(title)}"]
    if status:
        lines.append(f"status: {_yaml_quoted(status)}")
    lines.append("tags: [" + ", ".join(_yaml_quoted(tag) for tag in tags) + "]")
    lines.extend(("---", "", f"# {markdown_literal_text(title)}"))
    return ("\n".join(lines) + body).encode()


def _validate_generated_page(layer: Layer, uri: str, content: bytes, *, require_status: bool) -> None:
    page = parse_page(uri, content)
    report = validate_pages((page,), layer=layer, require_status=require_status, strict=True)
    if report.ok:
        return
    messages = "; ".join(issue.message for issue in report.issues if issue.severity is Severity.ERROR)
    raise ValueError(f"generated page violates the {layer} write contract: {messages}")


def _ensure_new(path: Path, label: str) -> None:
    try:
        path.lstat()
    except FileNotFoundError:
        return
    except OSError as error:
        raise OSError(f"inspect {label}: {error}") from error
    raise ValueError(f"{label} already exists")


def _helper_template(name: str) -> tuple[bytes, tuple[str, ...]]:
    usage = f"{name} <start> <end>"
    suffix = Path(name).suffix
    if suffix == ".sh":
        content = (
            f'#!/bin/sh\nset -eu\n\n[ "$#" -eq 2 ] || {{ echo "usage: {usage}" >&2; exit 2; }}\n'
            f'echo "{name}: not implemented" >&2\nexit 1\n'
        )
        return content.encode(), (name,)
    if suffix == ".py":
        content = (
            "#!/usr/bin/env python3\nimport sys\n\n"
            f'if len(sys.argv) != 3:\n    print("usage: {usage}", file=sys.stderr)\n    raise SystemExit(2)\n'
            f'print("{name}: not implemented", file=sys.stderr)\nraise SystemExit(1)\n'
        )
        return content.encode(), (name, "python3")
    raise ValueError(f"helper name {name!r} must end in .sh or .py")


def create_new(base: Base, request: NewRequest, *, cancel: Cancellation | None = None) -> NewResult:
    """Create exactly one safe scaffold and never replace owner-authored bytes."""
    check_cancel(cancel)
    slug = request.slug.strip()
    if slug and _SLUG_PATTERN.fullmatch(slug) is None:
        raise ValueError(
            f"{request.kind} slug {slug!r} must be one flat lowercase name using only letters, digits, dot, "
            "underscore, and hyphen"
        )
    title = request.title.strip() or _title_from_slug(slug)
    _validate_text("title", title)
    now = request.now or base.now()

    if request.kind is NewKind.HELPER:
        if not slug:
            raise ValueError("helper name is required (e.g. `fkf new helper collect-prs.sh`)")
        content, requirements = _helper_template(slug)
        check_cancel(cancel)
        directory = ensure_sources_dir(base.root)
        path = directory / slug
        validate_within_root(base.root, path)
        _ensure_new(path, f"helper sources/{slug}")
        check_cancel(cancel)
        atomic_write(path, content, mode=0o700)
        return NewResult(
            request.kind,
            path,
            True,
            f"created helper at sources/{slug}",
            run=(slug, "{{start}}", "{{end}}"),
            requires=requirements,
        )

    if request.kind is NewKind.TASK:
        base.require_layer(Layer.TASKS)
        if not slug:
            raise ValueError("task slug is required (e.g. `fkf new task my-feature`)")
        day = now.date().isoformat()
        uri = f"tasks/{day}/{slug}/TASKS.md"
        path = base.store.resolve(uri)
        _ensure_new(path, f"task trace {uri}")
        check_cancel(cancel)
        atomic_write(path, _task_template(title), mode=BASE_FILE_MODE)
        return NewResult(request.kind, path, True, f"created task trace at {uri}", uri=uri)

    if request.kind is NewKind.PROJECT:
        base.require_layer(Layer.PROJECTS)
        if not slug:
            raise ValueError("project slug is required (e.g. `fkf new project my-project`)")
        tags = _normalize_tags(request.tags)
        uri = f"projects/{slug}.md"
        path = base.store.resolve(uri)
        _ensure_new(path, f"project page {uri}")
        content = _page_template(
            page_type="project",
            title=title,
            tags=tags,
            status="active",
            body="\n\n## Intent\n\n## Open questions\n\n## Decisions\n",
        )
        _validate_generated_page(Layer.PROJECTS, uri, content, require_status=True)
        check_cancel(cancel)
        atomic_write(path, content, mode=BASE_FILE_MODE)
        return NewResult(request.kind, path, True, f"created project page at {uri}", uri=uri)

    if request.kind is NewKind.WIKI:
        base.require_layer(Layer.WIKI)
        if not slug:
            raise ValueError("wiki slug is required (e.g. `fkf new wiki my-concept`)")
        page_type = request.type.strip() or "decision"
        if _TAG_PATTERN.fullmatch(page_type) is None:
            raise ValueError(f"wiki type {page_type!r} must be lowercase kebab-case")
        tags = _normalize_tags(request.tags)
        uri = f"wiki/{slug}.md"
        path = base.store.resolve(uri)
        _ensure_new(path, f"wiki page {uri}")
        content = _page_template(page_type=page_type, title=title, tags=tags)
        _validate_generated_page(Layer.WIKI, uri, content, require_status=False)
        check_cancel(cancel)
        atomic_write(path, content, mode=BASE_FILE_MODE)
        return NewResult(request.kind, path, True, f"created wiki page at {uri}", uri=uri)

    raise ValueError(f"unknown new kind {request.kind!r}; want task, project, wiki, or helper")


__all__ = ["NewKind", "NewRequest", "NewResult", "create_new", "parse_new_kind"]
