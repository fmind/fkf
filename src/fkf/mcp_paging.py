"""Restart-stable, query- and snapshot-bound MCP continuation cursors."""

from __future__ import annotations

import base64
import binascii
import hashlib
import json
import re
import sys
from dataclasses import dataclass, replace
from typing import Any, Final

from fkf.base import Base
from fkf.find import (
    FIND_PHASE_PAGE,
    FIND_PHASE_RECORD,
    FIND_PHASE_VOLUME,
    FindFilter,
    FindPosition,
    compact_find_result,
    find_bounded,
)
from fkf.jsoncodec import dumps
from fkf.process import Cancellation

_CURSOR_VERSION: Final = 1
_SHA256_PATTERN: Final = re.compile(r"^[0-9a-f]{64}$")
_BASE64URL_PATTERN: Final = re.compile(r"^[A-Za-z0-9_-]+$")


def _invalid(message: str) -> ValueError:
    return ValueError(f"invalid cursor: {message}")


def _json_sha256(value: object) -> str:
    return hashlib.sha256(dumps(value)).hexdigest()


def _encode_cursor(value: dict[str, object], *, maximum: int, name: str) -> str:
    # Cursor fields are ASCII in normal use. ensure_ascii=False also matches Go's UTF-8 JSON
    # representation if a valid URI ever carries non-ASCII text.
    encoded = json.dumps(value, ensure_ascii=False, separators=(",", ":")).encode()
    token = base64.urlsafe_b64encode(encoded).rstrip(b"=").decode("ascii")
    if len(token) > maximum:
        raise ValueError(f"{name} continuation cursor is {len(token)} bytes, over the {maximum}-byte MCP bound")
    return token


def _decode_cursor(raw: str, *, maximum: int, shape: str) -> dict[str, object]:
    if len(raw) > maximum:
        raise _invalid(f"is {len(raw)} bytes; expected at most {maximum}")
    if not raw or _BASE64URL_PATTERN.fullmatch(raw) is None:
        raise _invalid("is not unpadded base64url")
    try:
        encoded = base64.urlsafe_b64decode(raw + "=" * (-len(raw) % 4))
    except (binascii.Error, ValueError) as error:
        raise _invalid("is not unpadded base64url") from error
    if base64.urlsafe_b64encode(encoded).rstrip(b"=").decode("ascii") != raw:
        raise _invalid("is not canonical unpadded base64url")
    try:
        value = json.loads(encoded)
    except (json.JSONDecodeError, UnicodeDecodeError) as error:
        raise _invalid(f"does not hold the published {shape} cursor shape") from error
    if not isinstance(value, dict):
        raise _invalid(f"does not hold the published {shape} cursor shape")
    return value


def _validate_common(value: dict[str, object], *, tool: str, query_sha256: str, keys: set[str]) -> None:
    if set(value) != keys:
        raise _invalid("does not hold the published cursor shape")
    version = value.get("v")
    if type(version) is not int or version != _CURSOR_VERSION:
        raise _invalid(f"has version {version!r}; expected {_CURSOR_VERSION}")
    owner = value.get("tool")
    if not isinstance(owner, str):
        raise _invalid("does not hold the published cursor shape")
    if owner != tool:
        raise _invalid(f"belongs to {owner}, not {tool}")
    cursor_query = value.get("query_sha256")
    snapshot = value.get("snapshot_sha256")
    if (
        not isinstance(cursor_query, str)
        or _SHA256_PATTERN.fullmatch(cursor_query) is None
        or not isinstance(snapshot, str)
        or _SHA256_PATTERN.fullmatch(snapshot) is None
    ):
        raise _invalid("holds an invalid digest")
    if cursor_query != query_sha256:
        raise _invalid("does not match this effective query; repeat it or restart without cursor")


@dataclass(frozen=True, slots=True)
class PageCursor:
    """One stateless offset continuation for a stable ordered result."""

    tool: str
    query_sha256: str
    snapshot_sha256: str = ""
    offset: int = 0
    continued: bool = False
    maximum: int = 512

    @classmethod
    def open(cls, raw: str, *, tool: str, query: object, maximum: int = 512) -> PageCursor:
        query_sha256 = _json_sha256(query)
        if not raw:
            return cls(tool=tool, query_sha256=query_sha256, maximum=maximum)
        value = _decode_cursor(raw, maximum=maximum, shape="page")
        _validate_common(
            value,
            tool=tool,
            query_sha256=query_sha256,
            keys={"v", "tool", "query_sha256", "snapshot_sha256", "offset"},
        )
        offset = value.get("offset")
        if type(offset) is not int or offset <= 0 or offset > sys.maxsize - 100:
            raise _invalid("holds an invalid offset")
        return cls(
            tool=tool,
            query_sha256=query_sha256,
            snapshot_sha256=str(value["snapshot_sha256"]),
            offset=offset,
            continued=True,
            maximum=maximum,
        )

    def bind_snapshot(self, snapshot_sha256: str) -> None:
        if self.continued and self.snapshot_sha256 != snapshot_sha256:
            raise ValueError("cursor is stale because the result changed; restart without cursor")

    def next(self, snapshot_sha256: str, offset: int) -> str:
        if _SHA256_PATTERN.fullmatch(snapshot_sha256) is None or offset <= 0:
            raise ValueError(f"encode the {self.tool} cursor: invalid snapshot or offset")
        return _encode_cursor(
            {
                "v": _CURSOR_VERSION,
                "tool": self.tool,
                "query_sha256": self.query_sha256,
                "snapshot_sha256": snapshot_sha256,
                "offset": offset,
            },
            maximum=self.maximum,
            name=self.tool,
        )


def _page_object(value: Any, field: str, selected: tuple[object, ...], next_cursor: str) -> object:
    if not hasattr(value, field):
        raise TypeError(f"paged result has no {field!r} sequence")
    page = replace(value, **{field: selected})
    decoded = json.loads(dumps(page))
    if not isinstance(decoded, dict):
        raise TypeError("a paged result must encode as a JSON object")
    if next_cursor:
        decoded["next_cursor"] = next_cursor
    return decoded


def offset_page(
    value: object,
    field: str,
    *,
    tool: str,
    query: object,
    limit: int,
    cursor: str,
) -> tuple[object, int]:
    """Page one ordered dataclass sequence while binding the cursor to all result bytes."""

    if limit <= 0:
        raise ValueError("page limit must be positive")
    values = getattr(value, field, None)
    if not isinstance(values, tuple):
        raise TypeError(f"paged result {field!r} must be a tuple")
    snapshot = _json_sha256(value)
    state = PageCursor.open(cursor, tool=tool, query=query)
    state.bind_snapshot(snapshot)
    if state.continued and state.offset >= len(values):
        raise _invalid(f"offset {state.offset} is outside the {len(values)}-item result")
    end = min(state.offset + limit, len(values))
    selected = values[state.offset : end]
    next_cursor = state.next(snapshot, end) if end < len(values) else ""
    return _page_object(value, field, selected, next_cursor), len(selected)


@dataclass(frozen=True, slots=True)
class _FindCursor:
    query_sha256: str
    snapshot_sha256: str = ""
    position: FindPosition | None = None
    continued: bool = False

    @classmethod
    def open(cls, raw: str, *, query: object) -> _FindCursor:
        query_sha256 = _json_sha256(query)
        if not raw:
            return cls(query_sha256=query_sha256)
        value = _decode_cursor(raw, maximum=4096, shape="find")
        _validate_common(
            value,
            tool="find",
            query_sha256=query_sha256,
            keys={"v", "tool", "query_sha256", "snapshot_sha256", "position"},
        )
        position = _parse_find_position(value.get("position"))
        return cls(
            query_sha256=query_sha256,
            snapshot_sha256=str(value["snapshot_sha256"]),
            position=position,
            continued=True,
        )

    def bind_snapshot(self, snapshot_sha256: str) -> None:
        if self.continued and self.snapshot_sha256 != snapshot_sha256:
            raise ValueError("cursor is stale because the result changed; restart without cursor")

    def next(self, snapshot_sha256: str, position: FindPosition) -> str:
        if _SHA256_PATTERN.fullmatch(snapshot_sha256) is None:
            raise ValueError("encode the find cursor: invalid snapshot or position")
        return _encode_cursor(
            {
                "v": _CURSOR_VERSION,
                "tool": "find",
                "query_sha256": self.query_sha256,
                "snapshot_sha256": snapshot_sha256,
                "position": position.as_json(),
            },
            maximum=4096,
            name="find",
        )


def _parse_find_position(raw: object) -> FindPosition:
    if not isinstance(raw, dict) or not set(raw) <= {"phase", "score", "time", "uri", "date"}:
        raise _invalid("holds an invalid find continuation position")
    phase = raw.get("phase")
    score = raw.get("score", 0)
    time = raw.get("time", "")
    uri = raw.get("uri", "")
    date = raw.get("date", "")
    if (
        not isinstance(phase, str)
        or type(score) is not int
        or not isinstance(time, str)
        or not isinstance(uri, str)
        or not isinstance(date, str)
    ):
        raise _invalid("holds an invalid find continuation position")
    position = FindPosition(phase, score, time, uri, date)
    if phase == FIND_PHASE_PAGE and score > 0 and uri and not time and not date:
        return position
    if phase == FIND_PHASE_RECORD and score == 0 and uri and not date:
        return position
    if phase == FIND_PHASE_VOLUME and score == 0 and date and not time and not uri:
        return position
    raise _invalid("holds an invalid find continuation position")


def find_page(
    base: Base,
    filters: FindFilter,
    *,
    counting: bool,
    limit: int,
    cursor: str,
    query: object,
    cancel: Cancellation | None = None,
) -> tuple[object, int]:
    """Return one semantic find page without materializing every match."""

    state = _FindCursor.open(cursor, query=query)
    bounded = find_bounded(
        base,
        filters,
        counting=counting,
        limit=limit,
        after=state.position or FindPosition(),
        cancel=cancel,
    )
    state.bind_snapshot(bounded.snapshot_sha256)
    result = bounded.result
    compact_find_result(result)
    next_cursor = state.next(bounded.snapshot_sha256, bounded.next) if bounded.next is not None else ""
    decoded = json.loads(dumps(result))
    if not isinstance(decoded, dict):
        raise TypeError("a paged result must encode as a JSON object")
    if next_cursor:
        decoded["next_cursor"] = next_cursor
    return decoded, len(result.pages) + len(result.records) + len(result.volumes)


__all__ = ["PageCursor", "find_page", "offset_page"]
