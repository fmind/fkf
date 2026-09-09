#!/usr/bin/env python3
"""Fetch one validated Google Chat message body on explicit demand."""

from __future__ import annotations

import json
import re
import subprocess
import sys

MAX_PROVIDER_BYTES = 64 << 20
RESOURCE = re.compile(r"^spaces/[A-Za-z0-9_-]+/messages/[A-Za-z0-9_.-]+$")


def main(arguments: list[str]) -> int:
    if arguments[:1] in (["--version"], ["-v"]):
        sys.stdout.write("gws-chat-message-body.py (fkf base helper)\n")
        return 0
    if len(arguments) != 1:
        sys.stderr.write("usage: gws-chat-message-body.py <message-name>\n")
        return 2
    message = arguments[0]
    if RESOURCE.fullmatch(message) is None:
        sys.stderr.write("gws-chat-message-body.py: invalid message resource name\n")
        return 2
    params = json.dumps({"name": message}, separators=(",", ":"))
    try:
        with subprocess.Popen(
            ["gws", "chat", "spaces", "messages", "get", "--params", params],
            stdout=subprocess.PIPE,
        ) as process:
            if process.stdout is None:  # pragma: no cover
                raise RuntimeError
            raw = process.stdout.read(MAX_PROVIDER_BYTES + 1)
            if len(raw) > MAX_PROVIDER_BYTES:
                process.kill()
                process.wait()
                raise RuntimeError
            if process.wait() != 0:
                raise RuntimeError
        value = json.loads(raw)
        if not isinstance(value, dict) or value.get("name") != message:
            raise TypeError
        body = value.get("formattedText", value.get("text", ""))
        if not isinstance(body, str):
            raise TypeError
    except OSError, RuntimeError:
        sys.stderr.write(f"gws-chat-message-body.py: cannot fetch {message}\n")
        return 1
    except UnicodeError, TypeError, ValueError, json.JSONDecodeError:
        sys.stderr.write("gws-chat-message-body.py: provider returned the wrong message or an invalid body\n")
        return 1
    sys.stdout.write(body + "\n")
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
