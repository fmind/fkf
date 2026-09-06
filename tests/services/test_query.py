from __future__ import annotations

from datetime import datetime
from zoneinfo import ZoneInfo

import pytest

from fkf.query import Window, parse_temporal_query, parse_window

NOW = datetime(2026, 9, 6, 11, 30, tzinfo=ZoneInfo("Europe/Paris"))


@pytest.mark.parametrize(
    ("since", "until", "expected"),
    [
        ("", "", Window()),
        ("today", "yesterday", None),
        ("yesterday", "yesterday", Window("2026-09-05", "2026-09-05")),
        ("7d", "", Window("2026-08-30", "")),
        ("", "7d", Window("", "2026-09-13")),
        ("6w", "", Window("2026-07-26", "")),
        ("3m", "", Window("2026-06-08", "")),
        ("1y", "", Window("2025-09-06", "")),
        ("2024-02-29", "2026-09-06", Window("2024-02-29", "2026-09-06")),
    ],
)
def test_parse_window(since: str, until: str, expected: Window | None) -> None:
    if expected is None:
        with pytest.raises(ValueError, match="is after"):
            parse_window(since, until, NOW)
        return
    assert parse_window(since, until, NOW) == expected


@pytest.mark.parametrize("value", ["0d", "01d", "-1d", "7D", "1h", "d", "999999999999999999999y"])
def test_parse_window_rejects_noncanonical_relative_values(value: str) -> None:
    with pytest.raises(ValueError, match="is not a window"):
        parse_window(value, "", NOW)


def test_window_contains_inclusive_date_bounds() -> None:
    window = Window("2026-09-01", "2026-09-05")
    assert window.contains("2026-09-01")
    assert window.contains("2026-09-05")
    assert not window.contains("2026-08-31")
    assert not window.contains("2026-09-06")


@pytest.mark.parametrize(
    ("query", "clean", "window", "newest"),
    [
        ("today deployments", "deployments", Window("2026-09-06", "2026-09-06", "today"), False),
        ("deployments yesterday", "deployments", Window("2026-09-05", "2026-09-05", "yesterday"), False),
        ("last week incidents", "incidents", Window("2026-08-24", "2026-08-30", "last week"), False),
        ("incidents this week", "incidents", Window("2026-08-31", "2026-09-06", "this week"), False),
        ("since 2026-08-01 reviews", "reviews", Window("2026-08-01", "2026-09-06", "since 2026-08-01"), False),
        ("reviews friday", "reviews", Window("2026-09-04", "2026-09-04", "friday"), False),
        ("last friday reviews", "reviews", Window("2026-09-04", "2026-09-04", "last friday"), False),
        ("2026-08 notes", "notes", Window("2026-08-01", "2026-08-31", "2026-08"), False),
        ("review last", "review", Window(derived_from="last"), True),
        (
            "compare today with yesterday",
            "compare today with",
            Window("2026-09-05", "2026-09-05", "yesterday"),
            False,
        ),
        ("ordinary query", "ordinary query", Window(), False),
    ],
)
def test_parse_temporal_query(
    query: str,
    clean: str,
    window: Window,
    newest: bool,
) -> None:
    parsed = parse_temporal_query(query, NOW)
    assert parsed.query == clean
    assert parsed.window == window
    assert parsed.newest is newest


@pytest.mark.parametrize(
    ("query", "message"),
    [
        ("today", "leaves no query terms"),
        ("today changes yesterday", "ambiguous temporal query"),
        ("since 2026-02-30 changes", "not a valid YYYY-MM-DD"),
        ("2026-13 changes", "invalid YYYY-MM month"),
    ],
)
def test_parse_temporal_query_rejects_ambiguous_or_incomplete_boundaries(query: str, message: str) -> None:
    with pytest.raises(ValueError, match=message):
        parse_temporal_query(query, NOW)
