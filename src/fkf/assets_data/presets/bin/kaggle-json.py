#!/usr/bin/env python3
"""Normalize and finitely paginate Kaggle CLI JSON listings."""

from __future__ import annotations

import json
import re
import subprocess
import sys
from typing import Any

MAX_PROVIDER_BYTES = 64 << 20
MAX_OUTPUT_BYTES = 64 << 20
PAGE_SIZE = 100
MAX_PAGES = 50
MAX_RECORDS = PAGE_SIZE * MAX_PAGES
EMPTY_LISTING = re.compile(r"^No [a-z ]+ (?:found|available)", re.IGNORECASE)


def decode(raw: bytes) -> list[Any]:
    """Decode one CLI page, including Kaggle's text protocol markers."""
    try:
        text = raw.decode("utf-8")
    except UnicodeDecodeError as error:
        raise ValueError("invalid UTF-8") from error
    lines = text.splitlines(keepends=True)
    if lines and (lines[0].startswith("Next Page Token = ") or lines[0].startswith("Next page token: ")):
        text = "".join(lines[1:])
    stripped = text.strip()
    if not stripped or EMPTY_LISTING.match(stripped):
        return []
    try:
        value = json.loads(stripped)
    except json.JSONDecodeError as error:
        if text:
            sys.stderr.write(text)
            if not text.endswith("\n"):
                sys.stderr.write("\n")
        raise ValueError("kaggle printed something that is neither JSON nor an empty listing") from error
    if isinstance(value, list):
        return value
    if isinstance(value, dict):
        return [value]
    raise ValueError("kaggle printed something that is neither JSON nor an empty listing")


def invoke(arguments: list[str]) -> bytes:
    try:
        process = subprocess.Popen(
            ["kaggle", *arguments],
            stdout=subprocess.PIPE,
        )
    except FileNotFoundError:
        sys.stderr.write("kaggle-json.py: kaggle is required (mise use -g pipx:kaggle@latest)\n")
        raise SystemExit(1) from None
    with process:
        if process.stdout is None:  # pragma: no cover
            raise RuntimeError
        raw = process.stdout.read(MAX_PROVIDER_BYTES + 1)
        if len(raw) > MAX_PROVIDER_BYTES:
            process.kill()
            process.wait()
            raise ValueError("provider response exceeds FKF's 64 MiB command bound")
        returncode = process.wait()
    if returncode != 0:
        raise SystemExit(returncode)
    return raw


def identity(record: Any) -> str:
    """Deduplicate stable refs while retaining distinct redacted records."""
    if isinstance(record, dict) and isinstance(record.get("ref"), str) and record["ref"]:
        return f"ref:{record['ref']}"
    return "value:" + json.dumps(record, ensure_ascii=False, sort_keys=True, separators=(",", ":"))


def main(arguments: list[str]) -> int:
    if arguments[:1] in (["--version"], ["-v"]):
        sys.stdout.write("kaggle-json.py (fkf preset helper)\n")
        return 0

    all_pages = arguments[:1] == ["--all-pages"]
    command = arguments[1:] if all_pages else arguments
    if not command:
        sys.stderr.write("usage: kaggle-json.py [--all-pages] <kaggle arguments>...\n")
        return 2

    try:
        if not all_pages:
            records = decode(invoke(command))
            if len(records) > MAX_RECORDS:
                raise ValueError(f"record bound exceeds {MAX_RECORDS}")
        else:
            records = []
            complete = False
            for page in range(1, MAX_PAGES + 1):
                current = decode(invoke([*command, "--page-size", str(PAGE_SIZE), "-p", str(page)]))
                if len(current) > PAGE_SIZE:
                    raise ValueError(f"page {page} exceeds the requested page size")
                if len(records) + len(current) > MAX_RECORDS:
                    raise ValueError(f"record bound exceeds {MAX_RECORDS}")
                records.extend(current)
                if len(current) < PAGE_SIZE:
                    complete = True
                    break
            if not complete:
                raise ValueError(
                    f"still receiving full pages after {MAX_PAGES}; raise MAX_PAGES rather than filing a prefix"
                )
    except ValueError as error:
        sys.stderr.write(f"kaggle-json.py: {error}\n")
        return 1

    unique: dict[str, Any] = {}
    for record in records:
        unique.setdefault(identity(record), record)
    output = (json.dumps(list(unique.values()), ensure_ascii=False, separators=(",", ":")) + "\n").encode()
    if len(output) > MAX_OUTPUT_BYTES:
        sys.stderr.write(f"kaggle-json.py: output bound exceeds {MAX_OUTPUT_BYTES} bytes\n")
        return 1
    sys.stdout.buffer.write(output)
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
