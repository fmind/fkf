"""Nanosecond-preserving Go duration and provider timestamp primitives."""

from __future__ import annotations

import re
from dataclasses import dataclass
from datetime import UTC, date, datetime, timedelta

MAX_DURATION_NS = (1 << 63) - 1
MIN_DURATION_NS = -(1 << 63)

_NANOSECOND = 1
_MICROSECOND = 1_000
_MILLISECOND = 1_000_000
_SECOND = 1_000_000_000
_MINUTE = 60 * _SECOND
_HOUR = 60 * _MINUTE
_UNITS: dict[str, int] = {
    "ns": _NANOSECOND,
    "us": _MICROSECOND,
    "µs": _MICROSECOND,
    "μs": _MICROSECOND,
    "ms": _MILLISECOND,
    "s": _SECOND,
    "m": _MINUTE,
    "h": _HOUR,
}


class DurationNS(int):
    """A signed Go-compatible duration represented as integer nanoseconds."""

    def __new__(cls, value: int) -> DurationNS:
        if isinstance(value, bool) or not isinstance(value, int):
            raise TypeError("duration nanoseconds must be an integer")
        if value < MIN_DURATION_NS or value > MAX_DURATION_NS:
            raise OverflowError("duration exceeds the signed 64-bit nanosecond range")
        return super().__new__(cls, value)

    @property
    def seconds(self) -> float:
        """Return seconds for Python timeout APIs at their necessarily lower precision."""
        return int(self) / _SECOND


def _bounded_integer(raw: str) -> int:
    significant = raw.lstrip("0")
    if len(significant) > 19:
        raise ValueError("duration integer overflows")
    value = int(raw or "0")
    if value > 1 << 63:
        raise ValueError("duration integer overflows")
    return value


def parse_duration(raw: str) -> DurationNS:
    """Parse Go's signed sequence of decimal values and duration units."""
    original = raw
    negative = False
    if raw.startswith(("+", "-")):
        negative = raw[0] == "-"
        raw = raw[1:]
    if raw == "0":
        return DurationNS(0)
    if not raw:
        raise ValueError(f"invalid duration {original!r}")

    total = 0
    position = 0
    while position < len(raw):
        integer_start = position
        while position < len(raw) and raw[position].isascii() and raw[position].isdigit():
            position += 1
        integer_digits = raw[integer_start:position]

        fraction_digits = ""
        if position < len(raw) and raw[position] == ".":
            position += 1
            fraction_start = position
            while position < len(raw) and raw[position].isascii() and raw[position].isdigit():
                position += 1
            fraction_digits = raw[fraction_start:position]
        if not integer_digits and not fraction_digits:
            raise ValueError(f"invalid duration {original!r}")

        unit_start = position
        while (
            position < len(raw) and raw[position] != "." and not (raw[position].isascii() and raw[position].isdigit())
        ):
            position += 1
        unit_name = raw[unit_start:position]
        if not unit_name:
            raise ValueError(f"missing unit in duration {original!r}")
        unit = _UNITS.get(unit_name)
        if unit is None:
            raise ValueError(f"unknown unit {unit_name!r} in duration {original!r}")

        try:
            integer = _bounded_integer(integer_digits)
        except ValueError as error:
            raise ValueError(f"invalid duration {original!r}") from error
        if integer > (1 << 63) // unit:
            raise ValueError(f"invalid duration {original!r}")
        component = integer * unit

        if fraction_digits:
            # More than 18 decimal places cannot alter a nanosecond, even for hours.
            significant_fraction = fraction_digits[:18]
            numerator = int(significant_fraction)
            component += numerator * unit // (10 ** len(significant_fraction))
        total += component
        if total > 1 << 63:
            raise ValueError(f"invalid duration {original!r}")

    signed = -total if negative else total
    if signed < MIN_DURATION_NS or signed > MAX_DURATION_NS:
        raise ValueError(f"invalid duration {original!r}")
    return DurationNS(signed)


def _format_fractional(value: int, unit: int, precision: int) -> str:
    whole, remainder = divmod(value, unit)
    if remainder == 0:
        return str(whole)
    fraction = f"{remainder:0{precision}d}".rstrip("0")
    return f"{whole}.{fraction}"


def format_duration(duration: DurationNS | int) -> str:
    """Format a signed nanosecond duration like Go's ``time.Duration.String``."""
    value = int(duration)
    if value < MIN_DURATION_NS or value > MAX_DURATION_NS:
        raise OverflowError("duration exceeds the signed 64-bit nanosecond range")
    negative = value < 0
    absolute = -value if negative else value
    sign = "-" if negative else ""

    if absolute < _SECOND:
        if absolute == 0:
            return "0s"
        if absolute < _MICROSECOND:
            return f"{sign}{absolute}ns"
        if absolute < _MILLISECOND:
            return f"{sign}{_format_fractional(absolute, _MICROSECOND, 3)}µs"
        return f"{sign}{_format_fractional(absolute, _MILLISECOND, 6)}ms"

    seconds, nanoseconds = divmod(absolute, _SECOND)
    hours, remainder = divmod(seconds, 3600)
    minutes, seconds = divmod(remainder, 60)
    second_text = _format_fractional(seconds * _SECOND + nanoseconds, _SECOND, 9)
    if hours:
        return f"{sign}{hours}h{minutes}m{second_text}s"
    if minutes:
        return f"{sign}{minutes}m{second_text}s"
    return f"{sign}{second_text}s"


_EPOCH = datetime(1970, 1, 1, tzinfo=UTC)
_EPOCH_ORDINAL = date(1970, 1, 1).toordinal()
_RFC3339_PATTERN = re.compile(
    r"(?P<date>[0-9]{4}-[0-9]{2}-[0-9]{2})T"
    r"(?P<hour>[0-9]{2}):(?P<minute>[0-9]{2}):(?P<second>[0-9]{2})"
    r"(?:[.,](?P<fraction>[0-9]+))?(?P<zone>Z|[+-][0-9]{2}:[0-9]{2})\Z"
)
_JIRA_PATTERN = re.compile(
    r"(?P<date>[0-9]{4}-[0-9]{2}-[0-9]{2})T"
    r"(?P<hour>[0-9]{2}):(?P<minute>[0-9]{2}):(?P<second>[0-9]{2})"
    r"(?:\.(?P<fraction>[0-9]{3}))?(?P<zone>[+-][0-9]{4})\Z"
)
_SPACE_PATTERN = re.compile(
    r"(?P<date>[0-9]{4}-[0-9]{2}-[0-9]{2}) "
    r"(?P<hour>[0-9]{2}):(?P<minute>[0-9]{2}):(?P<second>[0-9]{2})"
    r"(?P<zone>Z|[+-][0-9]{2}:[0-9]{2})\Z"
)
_DATE_PATTERN = re.compile(r"[0-9]{4}-[0-9]{2}-[0-9]{2}\Z")
_EPOCH_PATTERN = re.compile(r"[+-]?[0-9]+\Z")


@dataclass(frozen=True, order=True, slots=True)
class Instant:
    """A UTC instant represented without losing sub-microsecond precision."""

    unix_nanoseconds: int

    @property
    def nanosecond(self) -> int:
        """Return the fractional nanosecond within the UTC second."""
        return self.unix_nanoseconds % _SECOND

    @classmethod
    def from_datetime(cls, value: datetime) -> Instant:
        """Convert an aware datetime, preserving all precision it can represent."""
        if value.tzinfo is None or value.utcoffset() is None:
            raise ValueError("timestamp datetime must have an explicit offset")
        utc = value.astimezone(UTC)
        delta = utc - _EPOCH
        nanoseconds = (delta.days * 86_400 + delta.seconds) * _SECOND + delta.microseconds * _MICROSECOND
        return cls(nanoseconds)

    def to_datetime(self) -> datetime:
        """Convert to UTC datetime, truncating only precision below one microsecond."""
        seconds, nanoseconds = divmod(self.unix_nanoseconds, _SECOND)
        return _EPOCH + timedelta(seconds=seconds, microseconds=nanoseconds // _MICROSECOND)


def _zone_offset_seconds(zone: str) -> int:
    if zone == "Z":
        return 0
    sign = -1 if zone[0] == "-" else 1
    digits = zone[1:].replace(":", "")
    hour = int(digits[:2])
    minute = int(digits[2:])
    if hour > 23 or minute > 59:
        raise ValueError("timestamp has an invalid UTC offset")
    return sign * (hour * 3600 + minute * 60)


def _instant_from_match(match: re.Match[str]) -> Instant:
    parsed_date = date.fromisoformat(match.group("date"))
    hour = int(match.group("hour"))
    minute = int(match.group("minute"))
    second = int(match.group("second"))
    if hour > 23 or minute > 59 or second > 59:
        raise ValueError("timestamp has an invalid clock time")
    fraction = match.groupdict().get("fraction") or ""
    nanosecond = int(fraction[:9].ljust(9, "0")) if fraction else 0
    local_seconds = (parsed_date.toordinal() - _EPOCH_ORDINAL) * 86_400 + hour * 3600 + minute * 60 + second
    utc_seconds = local_seconds - _zone_offset_seconds(match.group("zone"))
    return Instant(utc_seconds * _SECOND + nanosecond)


def parse_rfc3339(value: str) -> Instant:
    """Parse RFC 3339 with an explicit offset, truncating fractions to nanoseconds."""
    match = _RFC3339_PATTERN.fullmatch(value)
    if match is None:
        raise ValueError(f"timestamp {value!r} is not RFC 3339 with an explicit offset")
    try:
        return _instant_from_match(match)
    except ValueError as error:
        raise ValueError(f"timestamp {value!r} is invalid: {error}") from error


def parse_record_time(value: str) -> Instant:
    """Parse the finite timestamp shapes emitted by FKF's supported provider CLIs."""
    trimmed = value.strip()
    if not trimmed:
        raise ValueError("timestamp is empty")

    for pattern in (_RFC3339_PATTERN, _JIRA_PATTERN, _SPACE_PATTERN):
        match = pattern.fullmatch(trimmed)
        if match is not None:
            try:
                return _instant_from_match(match)
            except ValueError:
                break

    if _DATE_PATTERN.fullmatch(trimmed) is not None:
        try:
            parsed_date = date.fromisoformat(trimmed)
        except ValueError:
            pass
        else:
            seconds = (parsed_date.toordinal() - _EPOCH_ORDINAL) * 86_400
            return Instant(seconds * _SECOND)

    if _EPOCH_PATTERN.fullmatch(trimmed) is not None:
        try:
            epoch = int(trimmed)
        except ValueError:
            pass
        else:
            if MIN_DURATION_NS <= epoch <= MAX_DURATION_NS:
                magnitude = abs(epoch)
                if magnitude >= 100_000_000_000_000_000:
                    return Instant(epoch)
                if magnitude >= 100_000_000_000_000:
                    return Instant(epoch * _MICROSECOND)
                if magnitude >= 100_000_000_000:
                    return Instant(epoch * _MILLISECOND)
                return Instant(epoch * _SECOND)

    raise ValueError(
        f"timestamp {value!r} matches no known layout (RFC 3339 with an explicit offset, YYYY-MM-DD, or a Unix epoch)"
    )


def format_rfc3339(value: Instant) -> str:
    """Format one instant in UTC using RFC 3339's shortest nanosecond form."""
    utc = value.to_datetime()
    fraction = value.nanosecond
    suffix = f".{fraction:09d}".rstrip("0") if fraction else ""
    return f"{utc.year:04d}-{utc.month:02d}-{utc.day:02d}T{utc.hour:02d}:{utc.minute:02d}:{utc.second:02d}{suffix}Z"


__all__ = [
    "MAX_DURATION_NS",
    "MIN_DURATION_NS",
    "DurationNS",
    "Instant",
    "format_duration",
    "format_rfc3339",
    "parse_duration",
    "parse_record_time",
    "parse_rfc3339",
]
