#!/usr/bin/env python3
"""Collect bounded, projected metadata for visible Hugging Face repositories."""

from __future__ import annotations

import json
import subprocess
import sys
from typing import Any

MAX_PROVIDER_BYTES = 64 << 20
MAX_OUTPUT_BYTES = 64 << 20
PREFIX = {"model": "", "dataset": "datasets/", "space": "spaces/", "bucket": "buckets/"}


def main(arguments: list[str]) -> int:
    if arguments[:1] in (["--version"], ["-v"]):
        sys.stdout.write("huggingface-repositories-json.py (fkf base helper)\n")
        return 0
    if arguments:
        sys.stderr.write("usage: huggingface-repositories-json.py\n")
        return 2
    try:
        with subprocess.Popen(
            ["hf", "repos", "ls", "--limit", "10001", "--format", "json"],
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
        if not isinstance(value, list) or len(value) > 10_000:
            raise ValueError
        records: list[dict[str, Any]] = []
        seen: set[str] = set()
        for item in value:
            if not isinstance(item, dict):
                raise TypeError
            identifier = item.get("id")
            kind = item.get("type")
            updated = item.get("updated", "")
            visibility = item.get("visibility", "")
            if (
                not isinstance(identifier, str)
                or not identifier
                or not isinstance(kind, str)
                or kind not in PREFIX
                or not isinstance(updated, str)
                or not isinstance(visibility, str)
            ):
                raise ValueError
            uid = f"{kind}:{identifier}"
            if uid in seen:
                raise ValueError
            seen.add(uid)
            records.append(
                {
                    "uid": uid,
                    "id": identifier,
                    "type": kind,
                    "updated": updated,
                    "visibility": visibility,
                    "url": f"https://huggingface.co/{PREFIX[kind]}{identifier}",
                }
            )
        records.sort(key=lambda record: (record["id"], record["type"]))
        output = (json.dumps(records, ensure_ascii=False, separators=(",", ":")) + "\n").encode()
        if len(output) > MAX_OUTPUT_BYTES:
            raise RuntimeError
    except OSError, RuntimeError, UnicodeError, TypeError, ValueError, json.JSONDecodeError:
        sys.stderr.write("huggingface-repositories-json.py: cannot prove a complete repository inventory\n")
        return 1
    sys.stdout.buffer.write(output)
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
