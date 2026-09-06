"""Shared date-window and closed temporal-query grammar."""

from __future__ import annotations

import re
from dataclasses import dataclass, field
from datetime import date, datetime, timedelta
from typing import Final

_ISO_DATE_PATTERN: Final = re.compile(r"^[0-9]{4}-[0-9]{2}-[0-9]{2}$")
_RELATIVE_FACTORS: Final = {"d": 1, "w": 7, "m": 30, "y": 365}
_DAY_KEYWORDS: Final = {"today": 0, "yesterday": -1}
_WEEKDAYS: Final = {
    "monday": 0,
    "tuesday": 1,
    "wednesday": 2,
    "thursday": 3,
    "friday": 4,
    "saturday": 5,
    "sunday": 6,
}
_MAX_RELATIVE_DAYS: Final = 10_000 * 366


@dataclass(frozen=True, slots=True)
class Window:
    """Inclusive date bounds for a listing or retrieval query."""

    since: str = field(default="", metadata={"json": "since,omitempty"})
    until: str = field(default="", metadata={"json": "until,omitempty"})
    derived_from: str = field(default="", metadata={"json": "derived_from,omitempty"})

    def contains(self, value: str) -> bool:
        """Return whether an ISO date falls inside the inclusive bounds."""
        return (not self.since or value >= self.since) and (not self.until or value <= self.until)


@dataclass(frozen=True, slots=True)
class TemporalQuery:
    """A lexical query with at most one parsed boundary expression."""

    query: str
    window: Window = Window()
    newest: bool = False


@dataclass(frozen=True, slots=True)
class _TemporalExpression:
    words: int = 0
    phrase: str = ""
    window: Window = Window()
    newest: bool = False


def _date_at(now: datetime, offset: int) -> str:
    try:
        return (now.date() + timedelta(days=offset)).isoformat()
    except OverflowError as error:
        raise ValueError("window resolves outside the supported YYYY-MM-DD range") from error


def _parse_iso_date(value: str) -> str:
    try:
        parsed = date.fromisoformat(value)
    except ValueError as error:
        raise ValueError(f"{value!r} is not a valid YYYY-MM-DD date") from error
    if parsed.isoformat() != value:
        raise ValueError(f"{value!r} is not a valid YYYY-MM-DD date")
    return value


def _parse_relative_window(value: str) -> int:
    def invalid() -> ValueError:
        return ValueError(f"{value!r} is not a window; use today, yesterday, 7d, 6w, 3m, or YYYY-MM-DD")

    if len(value) < 2 or value[0] == "0":
        raise invalid()
    digits, unit = value[:-1], value[-1]
    if not digits.isascii() or not digits.isdigit() or unit not in _RELATIVE_FACTORS:
        raise invalid()
    count = int(digits)
    factor = _RELATIVE_FACTORS[unit]
    if count > _MAX_RELATIVE_DAYS // factor:
        raise invalid()
    return count * factor


def parse_window(since: str, until: str, now: datetime) -> Window:
    """Resolve absolute, named, and relative inclusive date bounds from one clock read."""

    resolved: list[str] = []
    for raw, flag, shift in ((since, "--since", -1), (until, "--until", 1)):
        value = raw.strip()
        if not value:
            resolved.append("")
            continue
        keyword = _DAY_KEYWORDS.get(value.lower())
        if keyword is not None:
            resolved.append(_date_at(now, keyword))
            continue
        if _ISO_DATE_PATTERN.fullmatch(value):
            try:
                resolved.append(_parse_iso_date(value))
            except ValueError as error:
                raise ValueError(
                    f"{flag} must be YYYY-MM-DD, today, yesterday, or a window like 7d: {error}"
                ) from error
            continue
        days = _parse_relative_window(value)
        try:
            resolved.append(_date_at(now, shift * days))
        except ValueError as error:
            raise ValueError(f"{flag} window {value!r} resolves outside the supported YYYY-MM-DD range") from error
    window = Window(*resolved)
    if window.since and window.until and window.since > window.until:
        raise ValueError(f"--since {window.since} is after --until {window.until}")
    return window


def _normalize_temporal_word(value: str) -> str:
    return value.lower().strip("?!,.")


def _exact_expression(phrase: str, value: str) -> _TemporalExpression:
    return _TemporalExpression(1, phrase, Window(value, value))


def _previous_weekday(now: datetime, wanted: int, *, include_today: bool) -> str:
    days = (now.weekday() - wanted) % 7
    if days == 0 and not include_today:
        days = 7
    return _date_at(now, -days)


def _parse_two_word_temporal(first: str, second: str, now: datetime, today: str) -> _TemporalExpression | None:
    phrase = f"{first} {second}"
    if first == "last" and second == "week":
        start = now.date() - timedelta(days=now.weekday() + 7)
        return _TemporalExpression(2, phrase, Window(start.isoformat(), (start + timedelta(days=6)).isoformat()))
    if first == "this" and second == "week":
        start = now.date() - timedelta(days=now.weekday())
        return _TemporalExpression(2, phrase, Window(start.isoformat(), today))
    if first == "since":
        try:
            parsed = _parse_iso_date(second)
        except ValueError as error:
            raise ValueError(f"temporal expression {phrase!r}: {error}") from error
        return _TemporalExpression(2, phrase, Window(parsed, today))
    if first == "last" and second in _WEEKDAYS:
        value = _previous_weekday(now, _WEEKDAYS[second], include_today=False)
        return _TemporalExpression(2, phrase, Window(value, value))
    return None


def _looks_like_temporal_month(value: str) -> bool:
    return len(value) == 7 and value[4] == "-" and value[:4].isascii() and value[:4].isdigit() and value[5:].isdigit()


def _parse_single_word_temporal(value: str, now: datetime, today: str) -> _TemporalExpression | None:
    if value == "today":
        return _exact_expression(value, today)
    if value == "yesterday":
        return _exact_expression(value, _date_at(now, -1))
    if value == "last":
        return _TemporalExpression(1, value, newest=True)
    if value in _WEEKDAYS:
        return _exact_expression(value, _previous_weekday(now, _WEEKDAYS[value], include_today=True))
    if len(value) == 7:
        try:
            month = date.fromisoformat(f"{value}-01")
        except ValueError:
            if _looks_like_temporal_month(value):
                raise ValueError(f"temporal expression {value!r}: invalid YYYY-MM month") from None
        else:
            if month.strftime("%Y-%m") == value:
                next_month = date(month.year + (month.month == 12), month.month % 12 + 1, 1)
                end = next_month - timedelta(days=1)
                return _TemporalExpression(1, value, Window(month.isoformat(), end.isoformat()))
    if _ISO_DATE_PATTERN.fullmatch(value):
        try:
            parsed = _parse_iso_date(value)
        except ValueError as error:
            raise ValueError(f"temporal expression {value!r}: {error}") from error
        return _exact_expression(value, parsed)
    return None


def _parse_temporal_boundary(words: list[str], *, at_start: bool, now: datetime) -> _TemporalExpression | None:
    if not words:
        return None
    today = _date_at(now, 0)
    if len(words) >= 2:
        selected = words[:2] if at_start else words[-2:]
        pair = [_normalize_temporal_word(word) for word in selected]
        if expression := _parse_two_word_temporal(pair[0], pair[1], now, today):
            return expression
    selected = words[0] if at_start else words[-1]
    return _parse_single_word_temporal(_normalize_temporal_word(selected), now, today)


def parse_temporal_query(query: str, now: datetime) -> TemporalQuery:
    """Remove one closed temporal expression from exactly one query boundary."""

    words = query.split()
    if not words:
        return TemporalQuery(query.strip())
    prefix = _parse_temporal_boundary(words, at_start=True, now=now)
    remaining = words[prefix.words :] if prefix is not None else words
    suffix = _parse_temporal_boundary(remaining, at_start=False, now=now)
    if prefix is not None and suffix is not None:
        raise ValueError(
            "ambiguous temporal query: use one date expression at the start or end, "
            f"not {prefix.phrase!r} and {suffix.phrase!r}"
        )
    expression = prefix or suffix
    if suffix is not None:
        remaining = remaining[: -suffix.words]
    if expression is None:
        return TemporalQuery(query.strip())
    for at_start in (True, False):
        extra = _parse_temporal_boundary(remaining, at_start=at_start, now=now)
        if extra is not None:
            raise ValueError(
                "ambiguous temporal query: use one date expression at the start or end, "
                f"not {expression.phrase!r} and {extra.phrase!r}"
            )
    clean = " ".join(remaining)
    if not clean:
        raise ValueError(f"temporal expression {expression.phrase!r} leaves no query terms")
    window = Window(expression.window.since, expression.window.until, expression.phrase)
    return TemporalQuery(clean, window, expression.newest)


__all__ = ["TemporalQuery", "Window", "parse_temporal_query", "parse_window"]
