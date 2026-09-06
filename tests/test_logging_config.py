from __future__ import annotations

import io
import logging

from fkf.logging_config import configure_logging


def test_logging_configuration_emits_one_slog_shaped_structured_line() -> None:
    stream = io.StringIO()
    logger = logging.getLogger("fkf")
    previous = (logger.handlers[:], logger.level, logger.propagate)
    try:
        configure_logging(stream)
        logging.getLogger("fkf.mcp_server").info(
            "fkf mcp call",
            extra={
                "tool": "find",
                "base": "brain",
                "items": 2,
                "elapsed_ms": 3,
                "input_digest": "0123456789ab",
                "bytes": 42,
            },
        )
    finally:
        logger.handlers = previous[0]
        logger.setLevel(previous[1])
        logger.propagate = previous[2]

    line = stream.getvalue()
    assert line.count("\n") == 1
    assert "level=INFO" in line
    assert 'msg="fkf mcp call"' in line
    assert "tool=find" in line
    assert "base=brain" in line
    assert "items=2" in line
    assert "elapsed_ms=3" in line
    assert "input_digest=0123456789ab" in line
    assert "bytes=42" in line
