#!/usr/bin/env python3
"""Fetch one Gmail message and print its preferred non-attachment text body."""

from __future__ import annotations

import base64
import json
import subprocess
import sys
from email import policy
from email.message import EmailMessage
from email.parser import BytesParser
from html.parser import HTMLParser
from typing import ClassVar

MAX_BODY_BYTES = 4 << 20
MAX_PROVIDER_BYTES = 64 << 20


class _HTMLText(HTMLParser):
    """Keep visible HTML text without adding a parser dependency to the trusted helper."""

    _BREAKS: ClassVar[frozenset[str]] = frozenset({"br", "div", "li", "p", "tr"})
    _HIDDEN: ClassVar[frozenset[str]] = frozenset({"script", "style"})

    def __init__(self) -> None:
        super().__init__(convert_charrefs=True)
        self.parts: list[str] = []
        self.hidden_depth = 0

    def handle_starttag(self, tag: str, attrs: list[tuple[str, str | None]]) -> None:
        del attrs
        if tag in self._HIDDEN:
            self.hidden_depth += 1
        elif tag in self._BREAKS and self.hidden_depth == 0:
            self.parts.append("\n")

    def handle_endtag(self, tag: str) -> None:
        if tag in self._HIDDEN and self.hidden_depth > 0:
            self.hidden_depth -= 1
        elif tag in self._BREAKS and self.hidden_depth == 0:
            self.parts.append("\n")

    def handle_data(self, data: str) -> None:
        if self.hidden_depth == 0:
            self.parts.append(data)

    def text(self) -> str:
        lines = (" ".join(line.split()) for line in "".join(self.parts).splitlines())
        return "\n".join(line for line in lines if line)


def _fail(message: str, code: int = 1) -> int:
    print(f"gmail-body.py: {message}", file=sys.stderr)
    return code


def _fetch(message_id: str) -> dict[str, object]:
    params = json.dumps(
        {"userId": "me", "id": message_id, "format": "raw"},
        separators=(",", ":"),
    )
    # Exact trusted argv keeps the provider call outside a shell.
    with subprocess.Popen(
        ["gws", "gmail", "users", "messages", "get", "--params", params],
        stdout=subprocess.PIPE,
        stderr=subprocess.DEVNULL,
    ) as process:
        if process.stdout is None:  # pragma: no cover - guaranteed by stdout=PIPE
            raise RuntimeError("cannot capture the Gmail response")
        # Read limit-plus-one while the child runs; checking after subprocess.run would first
        # buffer an arbitrarily large provider response in this trusted helper.
        output = process.stdout.read(MAX_PROVIDER_BYTES + 1)
        if len(output) > MAX_PROVIDER_BYTES:
            process.kill()
            process.wait()
            raise RuntimeError("provider response exceeds FKF's 64 MiB command bound")
        if process.wait() != 0:
            raise RuntimeError("cannot fetch the Gmail message")
    try:
        value = json.loads(output)
    except (UnicodeDecodeError, json.JSONDecodeError) as error:
        raise RuntimeError("provider returned invalid JSON") from error
    if not isinstance(value, dict) or value.get("id") != message_id:
        raise RuntimeError("provider returned the wrong Gmail message")
    return value


def _decode_message(value: dict[str, object]) -> EmailMessage:
    raw = value.get("raw")
    if not isinstance(raw, str) or not raw:
        raise RuntimeError("provider returned no raw message")
    try:
        encoded = raw + "=" * (-len(raw) % 4)
        message_bytes = base64.b64decode(
            encoded.encode("ascii"), altchars=b"-_", validate=True
        )
        parsed = BytesParser(policy=policy.default).parsebytes(message_bytes)
    except (UnicodeEncodeError, ValueError) as error:
        raise RuntimeError("provider returned invalid base64url mail") from error
    if not isinstance(parsed, EmailMessage):
        raise TypeError("mail parser returned an unsupported message")
    return parsed


def _body_text(message: EmailMessage) -> str:
    body = message.get_body(preferencelist=("plain", "html"))
    if body is None and message.get_content_maintype() == "text":
        body = message
    if body is None or body.get_content_disposition() == "attachment":
        raise RuntimeError("message has no non-attachment text body")
    try:
        content = body.get_content()
    except (LookupError, UnicodeError) as error:
        raise RuntimeError("message body has an unsupported text encoding") from error
    if not isinstance(content, str):
        raise TypeError("message body is not text")
    if body.get_content_subtype() == "html":
        parser = _HTMLText()
        parser.feed(content)
        parser.close()
        content = parser.text()
    return content.strip()


def main() -> int:
    if len(sys.argv) == 2 and sys.argv[1] in {"--version", "-v"}:
        print("gmail-body.py (fkf base helper)")
        return 0
    if len(sys.argv) != 2 or not sys.argv[1] or sys.argv[1].startswith("-"):
        return _fail("usage: gmail-body.py <message-id>", 2)
    try:
        body = _body_text(_decode_message(_fetch(sys.argv[1])))
    except (RuntimeError, TypeError) as error:
        return _fail(str(error))
    encoded = body.encode("utf-8")
    if len(encoded) > MAX_BODY_BYTES:
        return _fail(
            f"decoded body is {len(encoded)} bytes; FKF cache limit is {MAX_BODY_BYTES}"
        )
    sys.stdout.buffer.write(encoded)
    if encoded and not encoded.endswith(b"\n"):
        sys.stdout.buffer.write(b"\n")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
