#!/usr/bin/env python3
"""Exhaust one array-valued GitHub REST listing through finite numbered pages."""

from __future__ import annotations

import json
import re
import subprocess
import sys
from typing import Any

PAGE_SIZE = 100
MAX_PAGES = 100
MAX_PROVIDER_BYTES = 64 << 20
MAX_OUTPUT_BYTES = 64 << 20
MAX_RECORDS = PAGE_SIZE * MAX_PAGES
ENDPOINT = re.compile(r"^[A-Za-z0-9_./-]+$")


def invoke(arguments: list[str]) -> bytes:
    with subprocess.Popen(
        ["gh", *arguments],
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
    return raw


def main(arguments: list[str]) -> int:
    if arguments[:1] in (["--version"], ["-v"]):
        sys.stdout.write("github-generic-list-json.py (fkf base helper)\n")
        return 0
    if not arguments:
        sys.stderr.write("usage: github-generic-list-json.py <endpoint> [gh-api-args...]\n")
        return 2
    endpoint, *extra = arguments
    if endpoint.startswith("-") or ENDPOINT.fullmatch(endpoint) is None:
        sys.stderr.write("github-generic-list-json.py: invalid REST endpoint\n")
        return 2
    records: list[Any] = []
    try:
        complete = False
        for page in range(1, MAX_PAGES + 1):
            try:
                raw = invoke(
                    ["api", "--method", "GET", endpoint, *extra, "-f", f"per_page={PAGE_SIZE}", "-f", f"page={page}"]
                )
            except RuntimeError as error:
                raise RuntimeError(f"GitHub listing failed on page {page}") from error
            value = json.loads(raw)
            if not isinstance(value, list):
                raise TypeError("GitHub returned a non-array page")
            if len(value) > PAGE_SIZE:
                raise RuntimeError(f"GitHub returned more than {PAGE_SIZE} rows on one page")
            if len(records) + len(value) > MAX_RECORDS:
                raise RuntimeError(f"listing record bound exceeds {MAX_RECORDS}")
            records.extend(value)
            if len(value) < PAGE_SIZE:
                complete = True
                break
        if not complete:
            raise RuntimeError(f"page {MAX_PAGES} was full; refusing a potentially truncated listing")
        output = (json.dumps(records, ensure_ascii=False, separators=(",", ":")) + "\n").encode()
        if len(output) > MAX_OUTPUT_BYTES:
            raise RuntimeError(f"output bound exceeds {MAX_OUTPUT_BYTES} bytes")
    except (OSError, RuntimeError, UnicodeError, TypeError, ValueError, json.JSONDecodeError) as error:
        sys.stderr.write(f"github-generic-list-json.py: {error}\n")
        return 1
    sys.stdout.buffer.write(output)
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
