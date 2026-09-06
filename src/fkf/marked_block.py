"""Fail-closed replacement of FKF-owned regions in authored text files."""

from __future__ import annotations

from dataclasses import dataclass

from fkf.errors import InvalidUsageError

BLOCK_MARKER_PREFIX = "<!-- >>> fkf managed block"
BLOCK_END_MARKER = "<!-- <<< fkf managed block -->"


class MarkedBlockError(InvalidUsageError):
    """A generated or existing managed-block topology is ambiguous."""


def block_begin_marker(command: str) -> str:
    """Return the canonical begin marker naming its regeneration command."""

    return f"{BLOCK_MARKER_PREFIX} — regenerate with `{command}`; edits between the markers are lost -->"


@dataclass(frozen=True, slots=True)
class MarkedBlockMarkers:
    begin: str
    begin_prefix: str
    end: str
    end_prefix: str


@dataclass(frozen=True, slots=True)
class MarkedBlockRegion:
    begin: int = 0
    end: int = 0
    present: bool = False


def parse_marked_block_region(content: str, markers: MarkedBlockMarkers) -> MarkedBlockRegion:
    """Locate exactly one canonical marker pair or reject ambiguous topology."""

    begins: list[int] = []
    ends: list[int] = []
    noncanonical_begin = False
    noncanonical_end = False
    offset = 0
    for chunk in _split_after_lf(content):
        # Go's parser removes LF only. A CRLF marker is deliberately non-canonical.
        line = chunk.removesuffix("\n")
        line_end = offset + len(line)
        if markers.begin_prefix in line:
            if line == markers.begin:
                begins.append(offset)
            else:
                noncanonical_begin = True
        if markers.end_prefix in line:
            if line == markers.end:
                ends.append(line_end)
            else:
                noncanonical_end = True
        offset += len(chunk)

    if noncanonical_begin:
        raise MarkedBlockError(f"managed block has a non-canonical begin marker; replace it with {markers.begin!r}")
    if noncanonical_end:
        raise MarkedBlockError(f"managed block has a non-canonical end marker; replace it with {markers.end!r}")
    if len(begins) > 1:
        raise MarkedBlockError("managed block has more than one canonical begin marker")
    if len(ends) > 1:
        raise MarkedBlockError("managed block has more than one canonical end marker")
    if not begins and len(ends) == 1:
        raise MarkedBlockError(f"managed block end marker {markers.end!r} has no matching begin marker")
    if not begins:
        return MarkedBlockRegion()
    if not ends:
        raise MarkedBlockError(f"managed block begin marker has no matching end marker {markers.end!r}")
    if ends[0] < begins[0] + len(markers.begin):
        raise MarkedBlockError(f"managed block end marker {markers.end!r} has no matching begin marker before it")
    return MarkedBlockRegion(begin=begins[0], end=ends[0], present=True)


def _split_after_lf(content: str) -> list[str]:
    chunks: list[str] = []
    start = 0
    while (line_end := content.find("\n", start)) >= 0:
        chunks.append(content[start : line_end + 1])
        start = line_end + 1
    if start < len(content) or not chunks:
        chunks.append(content[start:])
    return chunks


def replace_marked_block(existing: str, block: str, default_heading: str) -> str:
    """Replace only the canonical generated region and preserve all authored bytes."""

    begin_marker, separator, _ = block.partition("\n")
    if not separator or not begin_marker.startswith(BLOCK_MARKER_PREFIX):
        raise MarkedBlockError("generated block has no canonical begin marker")
    markers = MarkedBlockMarkers(
        begin=begin_marker,
        begin_prefix=BLOCK_MARKER_PREFIX,
        end=BLOCK_END_MARKER,
        end_prefix=BLOCK_END_MARKER,
    )
    try:
        generated = parse_marked_block_region(block, markers)
    except MarkedBlockError as error:
        raise MarkedBlockError("generated block does not contain exactly one canonical marker pair") from error
    if not generated.present or generated.begin != 0 or block[generated.end :].strip():
        raise MarkedBlockError("generated block does not contain exactly one canonical marker pair")

    region = parse_marked_block_region(existing, markers)
    if not region.present:
        if not existing.strip():
            existing = default_heading
        if not existing.endswith("\n"):
            existing += "\n"
        return existing + "\n" + block

    tail = existing[region.end :].removeprefix("\n")
    return existing[: region.begin] + block + tail


__all__ = [
    "BLOCK_END_MARKER",
    "BLOCK_MARKER_PREFIX",
    "MarkedBlockError",
    "MarkedBlockMarkers",
    "MarkedBlockRegion",
    "block_begin_marker",
    "parse_marked_block_region",
    "replace_marked_block",
]
