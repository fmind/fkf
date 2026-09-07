from __future__ import annotations

from datetime import UTC, datetime

import pytest

from fkf.timeutil import (
    MAX_DURATION_NS,
    MIN_DURATION_NS,
    DurationNS,
    Instant,
    format_duration,
    format_rfc3339,
    parse_duration,
    parse_record_time,
    parse_rfc3339,
)


@pytest.mark.parametrize(
    ("raw", "want"),
    [
        ("0", 0),
        ("+0", 0),
        ("1h30m", 5_400_000_000_000),
        ("1.5s", 1_500_000_000),
        (".25ms", 250_000),
        ("1.s", 1_000_000_000),
        ("1us", 1_000),
        ("1µs", 1_000),
        ("1μs", 1_000),
        ("-2m3.5s", -123_500_000_000),
        ("2562047h47m16.854775807s", MAX_DURATION_NS),
        ("-2562047h47m16.854775808s", MIN_DURATION_NS),
    ],
)
def test_parse_duration_matches_go_nanosecond_semantics(raw: str, want: int) -> None:
    parsed = parse_duration(raw)

    assert isinstance(parsed, DurationNS)
    assert int(parsed) == want


@pytest.mark.parametrize(
    "raw",
    ["", " ", "1", "1d", "1h-30m", ".s", "1e3s", "2562047h47m16.854775808s", "-2562047h47m16.854775809s"],
)
def test_parse_duration_refuses_invalid_or_overflowing_values(raw: str) -> None:
    with pytest.raises(ValueError, match="duration"):
        parse_duration(raw)


@pytest.mark.parametrize(
    ("value", "want"),
    [
        (DurationNS(0), "0s"),
        (DurationNS(1), "1ns"),
        (DurationNS(1_200), "1.2µs"),
        (DurationNS(1_250_000), "1.25ms"),
        (DurationNS(1_500_000_000), "1.5s"),
        (DurationNS(3_723_004_000_000), "1h2m3.004s"),
        (DurationNS(-60_000_000_000), "-1m0s"),
    ],
)
def test_format_duration_matches_go(value: DurationNS, want: str) -> None:
    assert format_duration(value) == want


def test_rfc3339_retains_nanoseconds_and_normalizes_to_utc() -> None:
    instant = parse_rfc3339("2026-05-04T11:00:00.123456789+02:00")

    assert instant.nanosecond == 123_456_789
    assert instant.unix_nanoseconds == 1_777_885_200_123_456_789
    assert format_rfc3339(instant) == "2026-05-04T09:00:00.123456789Z"


@pytest.mark.parametrize(
    ("raw", "want"),
    [
        ("2026-05-04T09:00:00.1234567899Z", "2026-05-04T09:00:00.123456789Z"),
        ("2026-05-04T09:00:00,5Z", "2026-05-04T09:00:00.5Z"),
    ],
)
def test_rfc3339_matches_go_fraction_parsing(raw: str, want: str) -> None:
    assert format_rfc3339(parse_rfc3339(raw)) == want


@pytest.mark.parametrize(
    ("raw", "want"),
    [
        ("2026-05-04T09:00:00Z", "2026-05-04T09:00:00Z"),
        ("2026-05-04T11:00:00+02:00", "2026-05-04T09:00:00Z"),
        ("2026-05-04T11:00:00.000+0200", "2026-05-04T09:00:00Z"),
        ("2026-05-04 11:00:00+02:00", "2026-05-04T09:00:00Z"),
        ("2026-05-04", "2026-05-04T00:00:00Z"),
        ("1777928400", "2026-05-04T21:00:00Z"),
        ("1777928400000", "2026-05-04T21:00:00Z"),
        ("1777928400000000", "2026-05-04T21:00:00Z"),
        ("1777928400000000000", "2026-05-04T21:00:00Z"),
    ],
)
def test_parse_record_time_supports_only_declared_provider_shapes(raw: str, want: str) -> None:
    assert format_rfc3339(parse_record_time(raw)) == want


@pytest.mark.parametrize(
    "raw",
    ["", " ", "last tuesday", "2026-05-04T09:00:00", "2026-05-04 09:00:00", "2026-05-04T09:00Z"],
)
def test_parse_record_time_refuses_invented_or_offset_free_timestamps(raw: str) -> None:
    with pytest.raises(ValueError, match="timestamp"):
        parse_record_time(raw)


def test_instant_datetime_conversion_is_explicit_about_microsecond_precision() -> None:
    instant = Instant.from_datetime(datetime(2026, 5, 4, 9, 0, 0, 123456, tzinfo=UTC))

    assert instant.to_datetime() == datetime(2026, 5, 4, 9, 0, 0, 123456, tzinfo=UTC)
    assert instant.nanosecond == 123_456_000
