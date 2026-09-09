#!/usr/bin/env python3
"""Collect bounded metadata for the authenticated user's Kaggle kernels."""

from __future__ import annotations

import importlib
import json
import os
import shutil
import sys
from itertools import islice
from pathlib import Path
from typing import Any

PAGE_SIZE = 100
MAX_PAGES = 50
MAX_RECORDS = PAGE_SIZE * MAX_PAGES
MAX_OUTPUT_BYTES = 64 << 20


class InvariantError(Exception):
    """The provider response cannot prove a complete safe inventory."""


def text(value: Any) -> str:
    return value if isinstance(value, str) else ""


def timestamp(value: Any) -> str | None:
    if value is None or isinstance(value, str):
        return value
    if hasattr(value, "isoformat"):
        return str(value.isoformat())
    raise InvariantError("invalid-last-run-time")


def provider_records() -> list[dict[str, Any]]:
    module = importlib.import_module("kaggle.api.kaggle_api_extended")
    api = module.KaggleApi()
    api.authenticate()
    records: list[dict[str, Any]] = []
    redacted: list[dict[str, Any]] = []
    complete = False
    for page in range(1, MAX_PAGES + 1):
        response = api.kernels_list_with_response(page=page, page_size=PAGE_SIZE, mine=True, sort_by="dateCreated")
        if response is None:
            raise InvariantError("provider-no-response")
        provider_items = response.kernels
        items = list(islice(provider_items if provider_items is not None else (), PAGE_SIZE + 1))
        if len(items) > PAGE_SIZE:
            raise InvariantError("page-size-exceeded")
        if len(records) + len(redacted) + len(items) > MAX_RECORDS:
            raise InvariantError("record-bound-exceeded")
        for item in items:
            if item is None:
                raise InvariantError("null-kernel")
            metadata = {
                "ref": text(getattr(item, "ref", None)),
                "title": text(getattr(item, "title", None)),
                "author": text(getattr(item, "author", None)),
                "lastRunTime": timestamp(getattr(item, "last_run_time", None)),
                "totalVotes": getattr(item, "total_votes", 0) or 0,
            }
            if metadata["ref"]:
                records.append({"id": metadata["ref"], "kind": "kernel", **metadata})
            else:
                redacted.append(metadata)
        if len(items) < PAGE_SIZE:
            complete = True
            break
    if not complete:
        raise InvariantError("page-limit-reached")
    identifiers = [record["id"] for record in records]
    if len(identifiers) != len(set(identifiers)):
        raise InvariantError("duplicate-visible-ref")
    if redacted:
        shapes = {json.dumps(record, sort_keys=True, separators=(",", ":")) for record in redacted}
        if len(shapes) != 1:
            raise InvariantError("redacted-metadata-diverged")
        metadata = redacted[0]
        records.append(
            {
                "id": "private-redacted",
                "kind": "private-redacted",
                "redacted_count": len(redacted),
                "title": "Private redacted kernels",
                "provider_title": metadata["title"],
                "author": metadata["author"],
                "lastRunTime": metadata["lastRunTime"],
                "totalVotes": metadata["totalVotes"],
            }
        )
    records.sort(key=lambda record: record["id"])
    return records


def kaggle_interpreter() -> Path:
    executable = shutil.which("kaggle")
    if executable is None:
        raise InvariantError("kaggle-is-required")
    try:
        with Path(executable).open(encoding="utf-8") as stream:
            first = stream.readline().rstrip("\n")
    except OSError as error:
        raise InvariantError("cannot-resolve-kaggle-python") from error
    interpreter = Path(first.removeprefix("#!")) if first.startswith("#!/") else Path()
    if not interpreter.is_absolute() or not os.access(interpreter, os.X_OK):
        raise InvariantError("cannot-resolve-kaggle-python")
    return interpreter


def main(arguments: list[str]) -> int:
    if arguments[:1] in (["--version"], ["-v"]):
        sys.stdout.write("kaggle-kernels-json.py (fkf base helper)\n")
        return 0
    if arguments:
        sys.stderr.write("usage: kaggle-kernels-json.py\n")
        return 2
    try:
        interpreter = kaggle_interpreter()
        if Path(sys.executable).resolve() != interpreter.resolve():
            # A fixed env launcher keeps the provider interpreter explicit and scanner-visible.
            os.execv(
                "/usr/bin/env",
                ["/usr/bin/env", os.fspath(interpreter), "-I", os.fspath(Path(__file__).resolve())],
            )
        output = (json.dumps(provider_records(), ensure_ascii=False, separators=(",", ":")) + "\n").encode()
        if len(output) > MAX_OUTPUT_BYTES:
            raise InvariantError("output-bound-exceeded")
    except InvariantError as error:
        sys.stderr.write(f"kaggle-kernels-json.py: collection failed ({error})\n")
        return 1
    except (AttributeError, ImportError, OSError, RuntimeError, TypeError, ValueError) as error:
        sys.stderr.write(f"kaggle-kernels-json.py: collection failed (provider-{type(error).__name__})\n")
        return 1
    sys.stdout.buffer.write(output)
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
