"""Reviewable, trace-backed learning proposals over authored knowledge pages."""

from __future__ import annotations

import hashlib
import os
import re
import stat
from dataclasses import dataclass, field, replace
from pathlib import Path, PurePosixPath
from typing import Final

import yaml
from yaml.nodes import MappingNode, Node, ScalarNode, SequenceNode

from fkf.base import Base
from fkf.build import BuildReport, build_if_stale
from fkf.errors import CanceledError, FKFError, InvalidUsageError, OperationalError
from fkf.graph import extract_edges
from fkf.io import atomic_write, read_file_limited, sync_directory
from fkf.learned import list_learned
from fkf.markdown import ValidationReport, parse_page
from fkf.process import Cancellation, CommandCanceledError
from fkf.query import Window
from fkf.store import BASE_DIR_MODE, BASE_FILE_MODE, MARKDOWN_EXTENSION, MAX_NARRATIVE_BYTES, Layer, clean_relative
from fkf.validation import validate_markdown_layer

LEARN_PROPOSAL_RELATIVE: Final = ".agents/tmp/learn"
MAX_LEARN_PROPOSAL_BYTES: Final = 1 << 20
MAX_LEARN_PATCH_FILES: Final = 32
MAX_LEARN_PATCH_HUNKS: Final = 256

_PROPOSAL_ID_PATTERN = re.compile(r"^[a-z0-9][a-z0-9-]{0,63}$")
_HUNK_PATTERN = re.compile(r"^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@(?: .*)?$")


@dataclass(frozen=True, slots=True)
class LearnCandidate:
    """One unharvested task lesson a deterministic log proposal can cite."""

    trace: str
    text: str
    target: str


@dataclass(frozen=True, slots=True)
class LearnProposal:
    """One reviewable unified diff; ``diff`` is present only on explicit review."""

    id: str
    path: str
    bytes: int
    files: tuple[str, ...]
    diff: str = field(default="", metadata={"json": "diff,omitempty"})


@dataclass(frozen=True, slots=True)
class LearnProposalReport:
    """A proposal attempt, including the non-writing dry-run candidate list."""

    dry_run: bool
    candidates: tuple[LearnCandidate, ...]
    proposal: LearnProposal | None = field(default=None, metadata={"json": "proposal,omitempty"})
    existing: bool = field(default=False, metadata={"json": "existing,omitempty"})
    nothing_to_propose: bool = field(default=False, metadata={"json": "nothing_to_propose,omitempty"})


@dataclass(frozen=True, slots=True)
class LearnReview:
    """The active proposal queue or one exact proposal and its requested diff."""

    proposals: tuple[LearnProposal, ...]


@dataclass(frozen=True, slots=True)
class LearnActionReport:
    """One apply or reject transition into the private ignored archive."""

    id: str
    status: str
    path: str
    files: tuple[str, ...] = field(default=(), metadata={"json": "files,omitempty"})
    validations: tuple[ValidationReport, ...] = field(default=(), metadata={"json": "validations,omitempty"})
    build: BuildReport | None = field(default=None, metadata={"json": "build,omitempty"})
    rebuild_error: str = field(default="", metadata={"json": "rebuild_error,omitempty"})


class LearnProposalError(InvalidUsageError):
    """A proposal violates the closed diff, target, queue, or transition contract."""


class LearnRebuildError(OperationalError):
    """The authored proposal is applied but one derived cache still needs repair."""

    def __init__(self, report: LearnActionReport, error: BaseException) -> None:
        self.report = report
        super().__init__(
            f"proposal {report.id} is applied; run `fkf build` to repair derived caches: {error}",
            cause=error,
        )


class LearnAppliedCanceledError(CommandCanceledError):
    """Cancellation observed after approval, with the durable applied state attached."""

    def __init__(self, report: LearnActionReport, error: BaseException | None = None) -> None:
        self.report = report
        detail = str(error) if error is not None else "command canceled"
        message = f"proposal {report.id} is applied; run `fkf build` to repair derived caches: {detail}"
        super().__init__(message, cause=error)


@dataclass(frozen=True, slots=True)
class _PatchLine:
    kind: str
    text: str


@dataclass(frozen=True, slots=True)
class _PatchHunk:
    old_start: int
    old_count: int
    new_start: int
    new_count: int
    lines: tuple[_PatchLine, ...]


@dataclass(frozen=True, slots=True)
class _FilePatch:
    uri: str
    new: bool
    hunks: tuple[_PatchHunk, ...]


@dataclass(frozen=True, slots=True)
class _LearnUpdate:
    uri: str
    absolute: Path
    data: bytes
    mode: int


@dataclass(frozen=True, slots=True)
class _LearnSnapshot:
    absolute: Path
    exists: bool = False
    data: bytes = b""
    mode: int = BASE_FILE_MODE
    limit: int = MAX_NARRATIVE_BYTES


class _IndentDumper(yaml.SafeDumper):
    """Match yaml.v3's readable indentation for block sequences."""

    def increase_indent(self, flow: bool = False, indentless: bool = False) -> int:
        del indentless
        return super().increase_indent(flow, False)


def _proposal_error(message: str) -> LearnProposalError:
    return LearnProposalError(f"learn proposal: {message}")


def _check_cancel(cancel: Cancellation | None) -> None:
    if cancel is not None and cancel.is_set():
        raise CommandCanceledError("command canceled")


def _normalize_proposal_id(value: str) -> str:
    normalized = value.removesuffix(".diff")
    if _PROPOSAL_ID_PATTERN.fullmatch(normalized) is None:
        raise _proposal_error(f"proposal id {normalized!r} must be lowercase letters, digits, and hyphens")
    return normalized


def _proposal_digest(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def _validate_proposal_digest(proposal_id: str, data: bytes) -> None:
    wanted = _proposal_digest(data)
    if proposal_id != wanted:
        raise _proposal_error(f"proposal id {proposal_id} does not match its SHA-256 digest {wanted}")


def _proposal_path(archive: str, proposal_id: str) -> str:
    parts = [LEARN_PROPOSAL_RELATIVE]
    if archive:
        parts.append(archive)
    parts.append(f"{proposal_id}.diff")
    return PurePosixPath(*parts).as_posix()


def _relative_below(root: Path, target: Path) -> str:
    try:
        return target.relative_to(root).as_posix()
    except ValueError as error:
        raise _proposal_error(f"{target} must remain below the base") from error


def _validate_learn_directory(root: Path, target: Path) -> None:
    relative = _relative_below(root, target)
    cursor = root
    for component in PurePosixPath(relative).parts:
        cursor /= component
        try:
            info = cursor.lstat()
        except FileNotFoundError:
            raise
        except OSError as error:
            raise OSError(f"inspect {cursor}: {error}") from error
        if stat.S_ISLNK(info.st_mode) or not stat.S_ISDIR(info.st_mode):
            raise _proposal_error(f"{relative} must be a real directory below the base")


def _inspect_learn_directory(base: Base, archive: str = "") -> Path:
    directory = base.root.joinpath(*PurePosixPath(LEARN_PROPOSAL_RELATIVE).parts)
    _validate_learn_directory(base.root, directory)
    if archive:
        directory /= archive
        _validate_learn_directory(base.root, directory)
    return directory


def _ensure_learn_component(root: Path, target: Path) -> None:
    try:
        info = target.lstat()
    except FileNotFoundError:
        try:
            target.mkdir(mode=BASE_DIR_MODE)
        except OSError as error:
            raise OSError(f"create {target.name}: {error}") from error
        return
    except OSError as error:
        raise OSError(f"inspect {target}: {error}") from error
    if stat.S_ISLNK(info.st_mode) or not stat.S_ISDIR(info.st_mode):
        relative = _relative_below(root, target)
        raise _proposal_error(f"{relative} must be a real directory below the base")


def _ensure_learn_directory(base: Base, archive: str = "") -> Path:
    root = base.root
    directory = root
    for component in PurePosixPath(LEARN_PROPOSAL_RELATIVE).parts:
        directory /= component
        _ensure_learn_component(root, directory)
    if archive:
        if archive not in {"applied", "rejected"}:
            raise _proposal_error(f"unknown learn archive {archive!r}")
        directory /= archive
        _ensure_learn_component(root, directory)
    return directory


def _learn_text_lines(data: bytes) -> list[str]:
    if not data:
        return []
    try:
        text = data.decode()
    except UnicodeDecodeError as error:
        raise ValueError("page is not valid UTF-8") from error
    if not text.endswith("\n"):
        raise ValueError("page must end with a newline before a unified diff can update it")
    return text.removesuffix("\n").split("\n")


def _header_path(line: str, prefix: str) -> str:
    value = line.removeprefix(prefix)
    boundary = min((index for index in (value.find("\t"), value.find(" ")) if index >= 0), default=-1)
    if boundary >= 0:
        value = value[:boundary]
    if not value:
        raise ValueError("file header has no path")
    return value


def _normalize_patch_path(value: str, prefix: str) -> str:
    if not value.startswith(prefix):
        raise _proposal_error(f"path {value!r} must begin {prefix}")
    candidate = value.removeprefix(prefix)
    try:
        cleaned = clean_relative(candidate)
    except (InvalidUsageError, ValueError) as error:
        raise _proposal_error(f"path {candidate!r}: {error}") from error
    if cleaned != candidate or "\\" in candidate:
        raise _proposal_error(f"path {candidate!r} is not canonical")
    return candidate


def _patch_target(old_path: str, new_path: str) -> tuple[str, bool]:
    if new_path == "/dev/null":
        raise _proposal_error("page deletion is not supported")
    new_uri = _normalize_patch_path(new_path, "b/")
    create = old_path == "/dev/null"
    if not create:
        old_uri = _normalize_patch_path(old_path, "a/")
        if old_uri != new_uri:
            raise _proposal_error(f"renames are not supported: {old_uri} becomes {new_uri}")
    parts = new_uri.split("/")
    if (
        len(parts) != 2
        or parts[0] not in {str(Layer.WIKI), str(Layer.PROJECTS)}
        or not parts[1].endswith(MARKDOWN_EXTENSION)
    ):
        raise _proposal_error(f"target {new_uri!r} must be one flat wiki/*.md or projects/*.md page")
    return new_uri, create


def _hunk_count(value: str) -> int:
    if not value:
        return 0
    try:
        count = int(value)
    except ValueError as error:
        raise ValueError("not a non-negative integer") from error
    if count < 0:
        raise ValueError("not a non-negative integer")
    return count


def _parse_hunk(lines: list[str], cursor: int) -> tuple[_PatchHunk, int]:
    match = _HUNK_PATTERN.fullmatch(lines[cursor])
    if match is None:
        raise _proposal_error(f"line {cursor + 1}: malformed hunk header")
    old_start = int(match.group(1))
    new_start = int(match.group(3))
    try:
        old_count = _hunk_count(match.group(2) or "")
    except ValueError as error:
        raise _proposal_error(f"line {cursor + 1}: old hunk count: {error}") from error
    try:
        new_count = _hunk_count(match.group(4) or "")
    except ValueError as error:
        raise _proposal_error(f"line {cursor + 1}: new hunk count: {error}") from error
    if match.group(2) is None:
        old_count = 1
    if match.group(4) is None:
        new_count = 1
    patch_lines: list[_PatchLine] = []
    old_seen = 0
    new_seen = 0
    cursor += 1
    while cursor < len(lines) and (old_seen < old_count or new_seen < new_count):
        line = lines[cursor]
        if not line:
            raise _proposal_error(f"line {cursor + 1}: hunk line has no prefix")
        kind = line[0]
        if kind == " ":
            old_seen += 1
            new_seen += 1
        elif kind == "-":
            old_seen += 1
        elif kind == "+":
            new_seen += 1
        else:
            raise _proposal_error(f"line {cursor + 1}: hunk line must begin space, +, or -")
        if old_seen > old_count or new_seen > new_count:
            raise _proposal_error(f"line {cursor + 1}: hunk exceeds its declared counts")
        patch_lines.append(_PatchLine(kind, line[1:]))
        cursor += 1
    if old_seen != old_count or new_seen != new_count:
        raise _proposal_error(f"hunk declares -{old_count},+{new_count} lines but contains -{old_seen},+{new_seen}")
    return _PatchHunk(old_start, old_count, new_start, new_count, tuple(patch_lines)), cursor


def _parse_file_patch(lines: list[str], cursor: int) -> tuple[_FilePatch, int]:
    if not lines[cursor].startswith("--- "):
        raise _proposal_error(f"line {cursor + 1}: expected an old-file header beginning `--- `")
    try:
        old_path = _header_path(lines[cursor], "--- ")
    except ValueError as error:
        raise _proposal_error(f"line {cursor + 1}: {error}") from error
    cursor += 1
    if cursor >= len(lines) or not lines[cursor].startswith("+++ "):
        raise _proposal_error(f"line {cursor + 1}: expected a new-file header beginning `+++ `")
    try:
        new_path = _header_path(lines[cursor], "+++ ")
    except ValueError as error:
        raise _proposal_error(f"line {cursor + 1}: {error}") from error
    uri, create = _patch_target(old_path, new_path)
    hunks: list[_PatchHunk] = []
    cursor += 1
    while cursor < len(lines) and lines[cursor].startswith("@@ "):
        hunk, cursor = _parse_hunk(lines, cursor)
        hunks.append(hunk)
        if len(hunks) > MAX_LEARN_PATCH_HUNKS:
            raise _proposal_error(f"{uri} has more than {MAX_LEARN_PATCH_HUNKS} hunks")
    if not hunks:
        raise _proposal_error(f"{uri} has no hunks")
    return _FilePatch(uri, create, tuple(hunks)), cursor


def _parse_diff(data: bytes) -> tuple[_FilePatch, ...]:
    if not data:
        raise _proposal_error("diff is empty")
    if len(data) > MAX_LEARN_PROPOSAL_BYTES:
        raise _proposal_error(f"diff is {len(data)} bytes; limit is {MAX_LEARN_PROPOSAL_BYTES}")
    try:
        text = data.decode()
    except UnicodeDecodeError as error:
        raise _proposal_error("diff is not valid UTF-8") from error
    if "\x00" in text or "\r" in text:
        raise _proposal_error("diff must use UTF-8 and LF line endings without NUL bytes")
    if not data.endswith(b"\n"):
        raise _proposal_error("diff must end with a newline")
    lines = text.removesuffix("\n").split("\n")
    patches: list[_FilePatch] = []
    seen: set[str] = set()
    cursor = 0
    while cursor < len(lines):
        if not lines[cursor]:
            cursor += 1
            continue
        patch, cursor = _parse_file_patch(lines, cursor)
        if patch.uri in seen:
            raise _proposal_error(f"diff repeats target {patch.uri}")
        seen.add(patch.uri)
        patches.append(patch)
        if len(patches) > MAX_LEARN_PATCH_FILES:
            raise _proposal_error(f"diff changes more than {MAX_LEARN_PATCH_FILES} files")
    if not patches:
        raise _proposal_error("diff changes no files")
    return tuple(patches)


def _apply_file_patch(original: bytes, patch: _FilePatch) -> bytes:
    try:
        old = _learn_text_lines(original)
    except ValueError as error:
        raise _proposal_error(f"{patch.uri}: {error}") from error
    result: list[str] = []
    cursor = 0
    for index, hunk in enumerate(patch.hunks, start=1):
        old_position = hunk.old_start if hunk.old_count == 0 else hunk.old_start - 1
        new_position = hunk.new_start if hunk.new_count == 0 else hunk.new_start - 1
        if old_position < cursor or old_position > len(old):
            raise _proposal_error(
                f"{patch.uri} hunk {index} old position {hunk.old_start} is outside or overlaps the file"
            )
        result.extend(old[cursor:old_position])
        if len(result) != new_position:
            raise _proposal_error(
                f"{patch.uri} hunk {index} new position {hunk.new_start} does not follow the preceding hunks"
            )
        position = old_position
        for line in hunk.lines:
            if line.kind == " ":
                if position >= len(old) or old[position] != line.text:
                    raise _proposal_error(f"{patch.uri} hunk {index} context does not match line {position + 1}")
                result.append(line.text)
                position += 1
            elif line.kind == "-":
                if position >= len(old) or old[position] != line.text:
                    raise _proposal_error(f"{patch.uri} hunk {index} removal does not match line {position + 1}")
                position += 1
            else:
                result.append(line.text)
        cursor = position
    result.extend(old[cursor:])
    return ("\n".join(result) + "\n").encode() if result else b""


def _render_diff(uri: str, old_data: bytes, new_data: bytes) -> bytes:
    old_lines = _learn_text_lines(old_data)
    new_lines = _learn_text_lines(new_data)
    prefix = 0
    while prefix < len(old_lines) and prefix < len(new_lines) and old_lines[prefix] == new_lines[prefix]:
        prefix += 1
    if prefix == len(old_lines) and prefix == len(new_lines):
        return b""
    suffix = 0
    while (
        suffix < len(old_lines) - prefix
        and suffix < len(new_lines) - prefix
        and old_lines[len(old_lines) - 1 - suffix] == new_lines[len(new_lines) - 1 - suffix]
    ):
        suffix += 1
    start = max(0, prefix - 3)
    old_end = min(len(old_lines), len(old_lines) - suffix + 3)
    new_end = min(len(new_lines), len(new_lines) - suffix + 3)
    old_count = old_end - start
    new_count = new_end - start
    old_start = start if old_count == 0 else start + 1
    new_start = start if new_count == 0 else start + 1
    lines = [f"--- a/{uri}", f"+++ b/{uri}", f"@@ -{old_start},{old_count} +{new_start},{new_count} @@"]
    lines.extend(f" {line}" for line in old_lines[start:prefix])
    lines.extend(f"-{line}" for line in old_lines[prefix : len(old_lines) - suffix])
    lines.extend(f"+{line}" for line in new_lines[prefix : len(new_lines) - suffix])
    lines.extend(f" {line}" for line in old_lines[len(old_lines) - suffix : old_end])
    data = ("\n".join(lines) + "\n").encode()
    if len(data) > MAX_LEARN_PROPOSAL_BYTES:
        raise _proposal_error(f"generated diff is {len(data)} bytes; limit is {MAX_LEARN_PROPOSAL_BYTES}")
    return data


def _split_frontmatter(data: bytes) -> tuple[bytes, bytes]:
    try:
        text = data.decode()
    except UnicodeDecodeError as error:
        raise _proposal_error("wiki/log.md: Markdown is not valid UTF-8") from error
    if not text.startswith(("---\n", "---\r\n")):
        return b"", data
    lines = text.split("\n")
    for index in range(1, len(lines)):
        if lines[index].rstrip("\r") == "---":
            return "\n".join(lines[1:index]).encode(), "\n".join(lines[index + 1 :]).encode()
    raise _proposal_error("wiki/log.md: frontmatter opening delimiter has no closing delimiter")


def _frontmatter_mapping(frontmatter: bytes) -> MappingNode:
    if not frontmatter:
        return MappingNode("tag:yaml.org,2002:map", [])
    try:
        document = yaml.compose(frontmatter.decode(), Loader=yaml.SafeLoader)
    except (UnicodeDecodeError, yaml.YAMLError) as error:
        raise _proposal_error(f"wiki/log.md frontmatter: {error}") from error
    if not isinstance(document, MappingNode):
        raise _proposal_error("wiki/log.md frontmatter must be a mapping")
    return document


def _add_learn_sources(frontmatter: bytes, traces: list[str]) -> bytes:
    mapping = _frontmatter_mapping(frontmatter)
    sources: Node | None = None
    for key, value in mapping.value:
        if key.value == "sources":
            sources = value
            break
    if sources is None:
        sources = SequenceNode("tag:yaml.org,2002:seq", [])
        mapping.value.append((ScalarNode("tag:yaml.org,2002:str", "sources"), sources))
    if not isinstance(sources, SequenceNode):
        raise _proposal_error("wiki/log.md frontmatter sources must be a list")
    existing: set[str] = set()
    for item in sources.value:
        if not isinstance(item, ScalarNode) or not item.value:
            raise _proposal_error("wiki/log.md frontmatter sources must contain only non-empty strings")
        existing.add(item.value)
    for trace in traces:
        citation = f"../{trace}#learned"
        if citation not in existing:
            sources.value.append(ScalarNode("tag:yaml.org,2002:str", citation))
            existing.add(citation)
    try:
        encoded = yaml.serialize(mapping, Dumper=_IndentDumper, indent=4, allow_unicode=True)
    except yaml.YAMLError as error:
        raise _proposal_error(f"encode wiki/log.md frontmatter: {error}") from error
    return encoded.encode()


def _ensure_trailing_newline(data: bytes) -> bytes:
    return data if data.endswith(b"\n") else data + b"\n"


def _insert_log_bullets(body: bytes, candidates: tuple[LearnCandidate, ...], date: str) -> bytes:
    lines = _learn_text_lines(_ensure_trailing_newline(body))
    bullets = [f"- {candidate.text}" for candidate in candidates]
    heading = f"## {date}"
    for index, line in enumerate(lines):
        if line != heading:
            continue
        at = index + 1
        if at < len(lines) and not lines[at]:
            at += 1
        lines[at:at] = [*bullets, ""]
        return ("\n".join(lines) + "\n").encode()
    at = next((index for index, line in enumerate(lines) if line.startswith("## ")), len(lines))
    block = [heading, "", *bullets, ""]
    if at > 0 and lines[at - 1]:
        block.insert(0, "")
    lines[at:at] = block
    return ("\n".join(lines) + "\n").encode()


def _propose_log(existing: bytes, candidates: tuple[LearnCandidate, ...], date: str) -> bytes:
    frontmatter, body = _split_frontmatter(existing)
    if not existing:
        body = b"# Log\n"
    traces = sorted({candidate.trace for candidate in candidates})
    frontmatter = _add_learn_sources(frontmatter, traces)
    body = _insert_log_bullets(body, candidates, date)
    return b"---\n" + frontmatter + b"---\n\n" + body


def _proposal_from_patches(
    proposal_id: str, data: bytes, patches: tuple[_FilePatch, ...], *, include_diff: bool
) -> LearnProposal:
    return LearnProposal(
        proposal_id,
        _proposal_path("", proposal_id),
        len(data),
        tuple(patch.uri for patch in patches),
        data.decode() if include_diff else "",
    )


def _read_proposal(
    base: Base, archive: str, proposal_id: str, *, include_diff: bool
) -> tuple[LearnProposal, Path, bytes]:
    directory = _inspect_learn_directory(base, archive)
    absolute = directory / f"{proposal_id}.diff"
    try:
        info = absolute.lstat()
    except OSError:
        raise
    if stat.S_ISLNK(info.st_mode) or not stat.S_ISREG(info.st_mode):
        raise _proposal_error(f"{_proposal_path(archive, proposal_id)} is not a regular non-symlink file")
    data = read_file_limited(absolute, MAX_LEARN_PROPOSAL_BYTES)
    _validate_proposal_digest(proposal_id, data)
    try:
        patches = _parse_diff(data)
    except LearnProposalError as error:
        raise _proposal_error(f"{_proposal_path(archive, proposal_id)}: {error}") from error
    proposal = replace(
        _proposal_from_patches(proposal_id, data, patches, include_diff=include_diff),
        path=_proposal_path(archive, proposal_id),
    )
    return proposal, absolute, data


def propose_learn(
    base: Base,
    *,
    dry_run: bool = False,
    cancel: Cancellation | None = None,
) -> LearnProposalReport:
    """Create one deterministic ``wiki/log.md`` diff; mutating callers own ``WriterLock``."""

    _check_cancel(cancel)
    base.require_layer(Layer.TASKS)
    base.require_layer(Layer.WIKI)
    listing = list_learned(base, Window(), only_unharvested=True, cancel=cancel)
    candidates = tuple(
        sorted(
            (LearnCandidate(item.trace, item.text, "wiki/log.md") for item in listing.bullets),
            key=lambda item: (item.trace, item.text),
        )
    )
    report = LearnProposalReport(dry_run, candidates)
    if not candidates:
        return replace(report, nothing_to_propose=True)
    if dry_run:
        return report
    _check_cancel(cancel)
    uri = "wiki/log.md"
    log_path = base.store.resolve(uri)
    try:
        log_path.lstat()
    except FileNotFoundError:
        old_data = b""
    else:
        try:
            old_data = read_file_limited(log_path, MAX_NARRATIVE_BYTES)
        except OSError as error:
            raise OSError(f"read {uri}: {error}") from error
    new_data = _propose_log(old_data, candidates, base.now().date().isoformat())
    diff = _render_diff(uri, old_data, new_data)
    if not diff:
        return replace(report, nothing_to_propose=True)
    patches = _parse_diff(diff)
    proposal_id = _proposal_digest(diff)
    proposal = _proposal_from_patches(proposal_id, diff, patches, include_diff=False)
    directory = _ensure_learn_directory(base)
    absolute = directory / f"{proposal_id}.diff"
    try:
        absolute.lstat()
    except FileNotFoundError:
        existing = None
    else:
        existing = read_file_limited(absolute, MAX_LEARN_PROPOSAL_BYTES)
    if existing is not None:
        if existing != diff:
            raise OperationalError(f"learn proposal id collision at {proposal.path}")
        return replace(report, proposal=proposal, existing=True)
    _check_cancel(cancel)
    atomic_write(absolute, diff, mode=BASE_FILE_MODE)
    return replace(report, proposal=proposal)


def review_learn(
    base: Base,
    proposal_id: str = "",
    *,
    include_diff: bool = False,
    cancel: Cancellation | None = None,
) -> LearnReview:
    """List active proposals or read one exact proposal without creating storage."""

    _check_cancel(cancel)
    if proposal_id:
        normalized = _normalize_proposal_id(proposal_id)
        proposal, _absolute, _data = _read_proposal(base, "", normalized, include_diff=include_diff)
        return LearnReview((proposal,))
    if include_diff:
        raise _proposal_error("review --diff requires one proposal id")
    try:
        directory = _inspect_learn_directory(base)
    except FileNotFoundError:
        return LearnReview(())
    try:
        entries = sorted(os.scandir(directory), key=lambda item: item.name)
    except OSError as error:
        raise OSError(f"list learn proposals: {error}") from error
    proposals: list[LearnProposal] = []
    for entry in entries:
        if entry.is_dir(follow_symlinks=False) or not entry.name.endswith(".diff"):
            continue
        normalized = entry.name.removesuffix(".diff")
        if _PROPOSAL_ID_PATTERN.fullmatch(normalized) is None:
            raise _proposal_error(f"active queue contains invalid filename {entry.name!r}")
        proposal, _absolute, _data = _read_proposal(base, "", normalized, include_diff=False)
        proposals.append(proposal)
    proposals.sort(key=lambda item: item.id)
    return LearnReview(tuple(proposals))


def _prepare_archive(base: Base, archive: str, proposal_id: str) -> tuple[Path, Path]:
    directory = _ensure_learn_directory(base, archive)
    destination = directory / f"{proposal_id}.diff"
    try:
        destination.lstat()
    except FileNotFoundError:
        return directory, destination
    except OSError:
        raise
    raise _proposal_error(f"{archive} archive already contains {proposal_id}")


def _restore_move(destination: Path, source: Path) -> None:
    try:
        source.lstat()
    except FileNotFoundError:
        pass
    except OSError:
        raise
    else:
        raise OSError(f"cannot restore proposal because {source} now exists")
    destination.rename(source)


def _move_validated_proposal(source: Path, destination: Path, proposal_id: str, expected: bytes) -> None:
    current = read_file_limited(source, MAX_LEARN_PROPOSAL_BYTES)
    _validate_proposal_digest(proposal_id, current)
    if current != expected:
        raise _proposal_error(f"proposal {proposal_id} changed before it could be archived")
    source.rename(destination)
    try:
        archived = read_file_limited(destination, MAX_LEARN_PROPOSAL_BYTES)
        _validate_proposal_digest(proposal_id, archived)
        if archived != expected:
            raise _proposal_error(f"proposal {proposal_id} changed while it was being archived")
    except BaseException as error:
        try:
            _restore_move(destination, source)
        except BaseException as restore_error:
            raise OSError(f"{error}; restore proposal move: {restore_error}") from error
        raise


def _sync_move(source_directory: Path, destination_directory: Path) -> None:
    sync_directory(destination_directory)
    if source_directory != destination_directory:
        sync_directory(source_directory)


def _terminal_proposal(base: Base, archive: str, proposal_id: str) -> LearnProposal | None:
    try:
        proposal, _absolute, _data = _read_proposal(base, archive, proposal_id, include_diff=False)
    except FileNotFoundError:
        return None
    return proposal


def reject_learn(
    base: Base,
    proposal_id: str,
    *,
    cancel: Cancellation | None = None,
) -> LearnActionReport:
    """Move one active proposal to the rejected archive; the caller owns ``WriterLock``."""

    _check_cancel(cancel)
    normalized = _normalize_proposal_id(proposal_id)
    try:
        proposal, source, proposal_data = _read_proposal(base, "", normalized, include_diff=False)
    except FileNotFoundError:
        if rejected := _terminal_proposal(base, "rejected", normalized):
            return LearnActionReport(normalized, "already-rejected", rejected.path)
        if _terminal_proposal(base, "applied", normalized) is not None:
            raise _proposal_error(f"{normalized} was already applied and cannot be rejected") from None
        raise FileNotFoundError(f"learn proposal {normalized} does not exist") from None
    destination_directory, destination = _prepare_archive(base, "rejected", normalized)
    _check_cancel(cancel)
    current = read_file_limited(source, MAX_LEARN_PROPOSAL_BYTES)
    _validate_proposal_digest(normalized, current)
    _move_validated_proposal(source, destination, normalized, proposal_data)
    try:
        _sync_move(source.parent, destination_directory)
    except BaseException as error:
        try:
            _restore_move(destination, source)
        except BaseException as restore_error:
            raise OSError(f"sync rejected proposal archive: {error}; restore proposal move: {restore_error}") from error
        raise OSError(f"sync rejected proposal archive: {error}") from error
    return LearnActionReport(normalized, "rejected", _proposal_path("rejected", normalized), proposal.files)


def _snapshot_absolute(uri: str, absolute: Path, limit: int) -> _LearnSnapshot:
    try:
        info = absolute.lstat()
    except FileNotFoundError:
        return _LearnSnapshot(absolute, limit=limit)
    except OSError as error:
        raise OSError(f"inspect {uri}: {error}") from error
    if stat.S_ISLNK(info.st_mode) or not stat.S_ISREG(info.st_mode):
        raise _proposal_error(f"target {uri} is not a regular non-symlink file")
    try:
        data = read_file_limited(absolute, limit)
    except OSError as error:
        raise OSError(f"read {uri}: {error}") from error
    return _LearnSnapshot(absolute, True, data, stat.S_IMODE(info.st_mode), limit)


def _snapshot_file(base: Base, uri: str, limit: int) -> _LearnSnapshot:
    return _snapshot_absolute(uri, base.store.resolve(uri), limit)


def _prepare_updates(
    base: Base, patches: tuple[_FilePatch, ...], cancel: Cancellation | None
) -> tuple[tuple[_LearnUpdate, ...], dict[str, _LearnSnapshot], tuple[Layer, ...]]:
    updates: list[_LearnUpdate] = []
    snapshots: dict[str, _LearnSnapshot] = {}
    layers: set[Layer] = set()
    for patch in patches:
        _check_cancel(cancel)
        layer = base.store.layer_of(patch.uri)
        if layer is None:
            raise _proposal_error(f"target {patch.uri!r} has no authored layer")
        base.require_layer(layer)
        snapshot = _snapshot_file(base, patch.uri, MAX_NARRATIVE_BYTES)
        if patch.new == snapshot.exists:
            if patch.new:
                raise _proposal_error(f"new page {patch.uri} already exists")
            raise _proposal_error(f"page {patch.uri} does not exist")
        updated = _apply_file_patch(snapshot.data, patch)
        if len(updated) > MAX_NARRATIVE_BYTES:
            raise _proposal_error(f"resulting page {patch.uri} is {len(updated)} bytes; limit is {MAX_NARRATIVE_BYTES}")
        try:
            parse_page(patch.uri, updated, base.now())
        except (InvalidUsageError, ValueError) as error:
            raise _proposal_error(f"resulting page {patch.uri} cannot be parsed: {error}") from error
        mode = snapshot.mode if snapshot.exists else BASE_FILE_MODE
        updates.append(_LearnUpdate(patch.uri, snapshot.absolute, updated, mode))
        snapshots[patch.uri] = snapshot
        layers.add(layer)
    ordered_layers = tuple(layer for layer in (Layer.WIKI, Layer.PROJECTS) if layer in layers)
    return tuple(updates), snapshots, ordered_layers


def _snapshots_equal(left: _LearnSnapshot, right: _LearnSnapshot) -> bool:
    return left.exists == right.exists and left.mode == right.mode and left.data == right.data


def _verify_update_snapshot(update: _LearnUpdate, snapshots: dict[str, _LearnSnapshot]) -> None:
    expected = snapshots.get(update.uri)
    if expected is None:
        raise _proposal_error(f"target {update.uri} has no approved snapshot")
    current = _snapshot_absolute(update.uri, update.absolute, MAX_NARRATIVE_BYTES)
    if not _snapshots_equal(current, expected):
        raise _proposal_error(f"target {update.uri} changed after the proposal was prepared")


def _write_updates(
    updates: tuple[_LearnUpdate, ...],
    snapshots: dict[str, _LearnSnapshot],
    published: dict[str, _LearnSnapshot],
    cancel: Cancellation | None,
) -> None:
    for update in updates:
        _verify_update_snapshot(update, snapshots)
    for update in updates:
        _check_cancel(cancel)
        _verify_update_snapshot(update, snapshots)
        try:
            atomic_write(update.absolute, update.data, mode=update.mode)
        except OSError as error:
            raise OSError(f"apply {update.uri}: {error}") from error
        snapshot = snapshots[update.uri]
        published[update.uri] = _LearnSnapshot(
            update.absolute,
            True,
            update.data,
            update.mode,
            snapshot.limit,
        )


def _restore_snapshots(
    snapshots: dict[str, _LearnSnapshot], published: dict[str, _LearnSnapshot]
) -> BaseException | None:
    failures: list[str] = []
    for uri in sorted(snapshots):
        snapshot = snapshots[uri]
        expected = published.get(uri)
        if expected is None:
            failures.append(f"refuse to restore {uri} without a published snapshot")
            continue
        try:
            current = _snapshot_absolute(uri, snapshot.absolute, snapshot.limit)
        except BaseException as error:
            failures.append(f"inspect {uri} before restore: {error}")
            continue
        if _snapshots_equal(current, snapshot):
            continue
        if not _snapshots_equal(current, expected):
            failures.append(f"refuse to restore changed file {uri}")
            continue
        try:
            if snapshot.exists:
                atomic_write(snapshot.absolute, snapshot.data, mode=snapshot.mode)
            else:
                snapshot.absolute.unlink(missing_ok=True)
        except OSError as error:
            verb = "restore" if snapshot.exists else "remove newly created"
            failures.append(f"{verb} {uri}: {error}")
    return OSError("; ".join(failures)) if failures else None


def _error_with_rollback(cause: BaseException, rollback: BaseException) -> BaseException:
    message = f"{cause}; rollback learn proposal: {rollback}"
    if isinstance(cause, CommandCanceledError):
        return CommandCanceledError(message, cause=cause)
    if isinstance(cause, InvalidUsageError):
        return LearnProposalError(message, cause=cause)
    if isinstance(cause, FKFError):
        return OperationalError(message, cause=cause)
    if isinstance(cause, ValueError):
        return ValueError(message)
    return OSError(message)


def _validation_error(report: ValidationReport) -> LearnProposalError:
    if not report.issues:
        return _proposal_error(f"strict {report.layer} validation failed")
    issue = report.issues[0]
    return _proposal_error(f"strict {report.layer} validation failed at {issue.uri}: {issue.message}")


def _validate_updates(
    base: Base,
    layers: tuple[Layer, ...],
    cancel: Cancellation | None,
) -> tuple[ValidationReport, ...]:
    validations: list[ValidationReport] = []
    for layer in layers:
        _check_cancel(cancel)
        report = validate_markdown_layer(
            base,
            layer,
            require_status=layer is Layer.PROJECTS,
            strict=True,
            cancel=cancel,
        )
        validations.append(report)
        if not report.ok:
            raise _validation_error(report)
    _check_cancel(cancel)
    # Graph extraction is read-only and exercises identity and addressable-child rules
    # before the authored bytes become an approved transaction.
    extract_edges(base, cancel=cancel)
    return tuple(validations)


def _execute_application(
    base: Base,
    updates: tuple[_LearnUpdate, ...],
    snapshots: dict[str, _LearnSnapshot],
    layers: tuple[Layer, ...],
    source: Path,
    destination: Path,
    applied_directory: Path,
    proposal_id: str,
    proposal_data: bytes,
    cancel: Cancellation | None,
) -> tuple[ValidationReport, ...]:
    published = dict(snapshots)
    try:
        _write_updates(updates, snapshots, published, cancel)
        validations = _validate_updates(base, layers, cancel)
        _check_cancel(cancel)
        _move_validated_proposal(source, destination, proposal_id, proposal_data)
        try:
            _sync_move(source.parent, applied_directory)
        except BaseException as error:
            try:
                _restore_move(destination, source)
            except BaseException as restore_error:
                raise OSError(
                    f"sync applied proposal archive: {error}; restore proposal move: {restore_error}"
                ) from error
            raise OSError(f"sync applied proposal archive: {error}") from error
    except BaseException as error:
        rollback = _restore_snapshots(snapshots, published)
        if rollback is not None:
            raise _error_with_rollback(error, rollback) from error
        raise
    return validations


def _active_proposal_for_apply(
    base: Base, proposal_id: str
) -> tuple[LearnProposal | None, Path | None, bytes | None, LearnActionReport | None]:
    try:
        proposal, source, data = _read_proposal(base, "", proposal_id, include_diff=False)
    except FileNotFoundError:
        if applied := _terminal_proposal(base, "applied", proposal_id):
            return (
                None,
                None,
                None,
                LearnActionReport(
                    proposal_id,
                    "already-applied",
                    applied.path,
                    applied.files,
                ),
            )
        if _terminal_proposal(base, "rejected", proposal_id) is not None:
            raise _proposal_error(f"{proposal_id} was rejected and cannot be applied") from None
        raise FileNotFoundError(f"learn proposal {proposal_id} does not exist") from None
    return proposal, source, data, None


def _rebuild_learn_caches(
    base: Base,
    report: LearnActionReport,
    cancel: Cancellation | None,
) -> LearnActionReport:
    try:
        _check_cancel(cancel)
        built = build_if_stale(base, cancel=cancel)
        completed = replace(report, build=built)
        if cancel is not None and cancel.is_set():
            raise LearnAppliedCanceledError(replace(completed, rebuild_error="command canceled"))
        return completed
    except LearnAppliedCanceledError:
        raise
    except CanceledError as error:
        raise LearnAppliedCanceledError(replace(report, rebuild_error=str(error)), error) from error
    except Exception as error:
        failed = replace(report, rebuild_error=str(error))
        raise LearnRebuildError(failed, error) from error


def apply_learn(
    base: Base,
    proposal_id: str,
    *,
    cancel: Cancellation | None = None,
) -> LearnActionReport:
    """Approve one queued diff atomically, then repair caches under the caller's lock."""

    _check_cancel(cancel)
    normalized = _normalize_proposal_id(proposal_id)
    proposal, source, proposal_data, archived = _active_proposal_for_apply(base, normalized)
    if archived is not None:
        return _rebuild_learn_caches(base, archived, cancel)
    if proposal is None or source is None or proposal_data is None:
        raise RuntimeError("active learn proposal resolution returned no proposal")
    _validate_proposal_digest(normalized, proposal_data)
    patches = _parse_diff(proposal_data)
    updates, snapshots, layers = _prepare_updates(base, patches, cancel)
    applied_directory, destination = _prepare_archive(base, "applied", normalized)
    validations = _execute_application(
        base,
        updates,
        snapshots,
        layers,
        source,
        destination,
        applied_directory,
        normalized,
        proposal_data,
        cancel,
    )
    report = LearnActionReport(
        normalized,
        "applied",
        _proposal_path("applied", normalized),
        proposal.files,
        validations,
    )
    return _rebuild_learn_caches(base, report, cancel)


__all__ = [
    "LEARN_PROPOSAL_RELATIVE",
    "MAX_LEARN_PATCH_FILES",
    "MAX_LEARN_PATCH_HUNKS",
    "MAX_LEARN_PROPOSAL_BYTES",
    "LearnActionReport",
    "LearnAppliedCanceledError",
    "LearnCandidate",
    "LearnProposal",
    "LearnProposalError",
    "LearnProposalReport",
    "LearnRebuildError",
    "LearnReview",
    "apply_learn",
    "propose_learn",
    "reject_learn",
    "review_learn",
]
