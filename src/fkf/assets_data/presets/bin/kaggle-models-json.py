#!/usr/bin/env python3
"""Collect every public Kaggle model owned by one account through finite token pagination."""

from __future__ import annotations

import json
import re
import subprocess
import sys
from typing import Any

MAX_PROVIDER_BYTES = 64 << 20
MAX_OUTPUT_BYTES = 64 << 20
MAX_PAGES = 1000
PAGE_SIZE = 200
MAX_RECORDS = 100_000
OWNER = re.compile(r"^[A-Za-z0-9_-]+$")


def page(arguments: list[str]) -> tuple[str | None, list[Any]]:
    with subprocess.Popen(
        ["kaggle", *arguments],
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
    text = raw.decode()
    lines = text.splitlines(keepends=True)
    token = None
    if lines and lines[0].startswith("Next Page Token = "):
        token = lines.pop(0).removeprefix("Next Page Token = ").strip()
    body = "".join(lines).strip()
    if body == "No models found":
        return token, []
    value = json.loads(body)
    if not isinstance(value, list):
        raise TypeError
    return token, value


def main(arguments: list[str]) -> int:
    if arguments[:1] in (["--version"], ["-v"]):
        sys.stdout.write("kaggle-models-json.py (fkf base helper)\n")
        return 0
    if len(arguments) != 1:
        sys.stderr.write("usage: kaggle-models-json.py <owner>\n")
        return 2
    owner = arguments[0]
    if OWNER.fullmatch(owner) is None:
        sys.stderr.write(f"kaggle-models-json.py: invalid owner: {owner}\n")
        return 2
    try:
        token: str | None = None
        seen_tokens: set[str] = set()
        records: list[dict[str, Any]] = []
        for _ in range(MAX_PAGES):
            command = ["models", "list", "--owner", owner, "--page-size", str(PAGE_SIZE)]
            if token is not None:
                command.extend(("--page-token", token))
            command.extend(("--format", "json"))
            next_token, items = page(command)
            if len(items) > PAGE_SIZE:
                raise RuntimeError(f"model page exceeds {PAGE_SIZE} items")
            for item in items:
                if not isinstance(item, dict):
                    raise TypeError
                identifier = item.get("id")
                reference = item.get("ref")
                if (
                    not isinstance(identifier, (int, str))
                    or not str(identifier)
                    or not isinstance(reference, str)
                    or not reference
                ):
                    raise ValueError
                if len(records) >= MAX_RECORDS:
                    raise RuntimeError(f"model record bound exceeds {MAX_RECORDS}")
                records.append(
                    {
                        "id": str(identifier),
                        "ref": reference,
                        "title": item.get("title", reference),
                        "subtitle": item.get("subtitle"),
                        "author": item.get("author"),
                    }
                )
            if not next_token:
                break
            if next_token in seen_tokens:
                raise RuntimeError("Kaggle repeated a page token")
            seen_tokens.add(next_token)
            token = next_token
        else:
            raise RuntimeError("more than 1000 page tokens; refusing an unbounded provider loop")
        identifiers = [record["id"] for record in records]
        if len(identifiers) != len(set(identifiers)):
            raise RuntimeError("provider returned duplicate model ids")
        records.sort(key=lambda record: record["id"])
        output = (json.dumps(records, ensure_ascii=False, separators=(",", ":")) + "\n").encode()
        if len(output) > MAX_OUTPUT_BYTES:
            raise RuntimeError(f"output bound exceeds {MAX_OUTPUT_BYTES} bytes")
    except RuntimeError as error:
        sys.stderr.write(f"kaggle-models-json.py: {error}\n")
        return 1
    except OSError, UnicodeError, TypeError, ValueError, json.JSONDecodeError:
        sys.stderr.write("kaggle-models-json.py: Kaggle returned an invalid model page\n")
        return 1
    sys.stdout.buffer.write(output)
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
