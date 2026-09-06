"""One bounded structured stderr logger for FKF command and MCP accounting."""

from __future__ import annotations

import json
import logging
import re
import sys
from datetime import datetime
from typing import Final, TextIO

_LOGGER_NAME: Final = "fkf"
_FIELDS: Final = (
    "tool",
    "base",
    "items",
    "elapsed_ms",
    "input_digest",
    "bytes",
    "error",
    "status",
    "diagnostic",
    "command",
    "source",
    "date",
    "window_start",
    "window_end",
    "uri",
)
_PLAIN_VALUE = re.compile(r"^[A-Za-z0-9._/@:+-]+$")


def _value(value: object) -> str:
    if isinstance(value, bool):
        return "true" if value else "false"
    if isinstance(value, int | float):
        return str(value)
    rendered = str(value)
    if rendered and _PLAIN_VALUE.fullmatch(rendered):
        return rendered
    # ASCII JSON quoting makes control and invisible directionality characters
    # inert in terminal logs without adding a second evidence transformation.
    return json.dumps(rendered, ensure_ascii=True, separators=(",", ":"))


class _StructuredFormatter(logging.Formatter):
    def format(self, record: logging.LogRecord) -> str:
        timestamp = datetime.fromtimestamp(record.created).astimezone().isoformat(timespec="milliseconds")
        parts = [f"time={timestamp}", f"level={record.levelname}", f"msg={_value(record.getMessage())}"]
        parts.extend(f"{name}={_value(record.__dict__[name])}" for name in _FIELDS if name in record.__dict__)
        return " ".join(parts)


def configure_logging(stream: TextIO | None = None) -> None:
    """Install FKF's one INFO-level structured stderr handler idempotently."""
    logger = logging.getLogger(_LOGGER_NAME)
    handler = logging.StreamHandler(sys.stderr if stream is None else stream)
    handler.setFormatter(_StructuredFormatter())
    logger.handlers = [handler]
    logger.setLevel(logging.INFO)
    logger.propagate = False


__all__ = ["configure_logging"]
