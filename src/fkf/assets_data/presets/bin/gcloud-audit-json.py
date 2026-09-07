#!/usr/bin/env python3
"""Collect bounded, privacy-projected Cloud Audit activity metadata."""

from __future__ import annotations

import json
import subprocess
import sys
from typing import Any
from urllib.parse import quote

MAX_PROVIDER_BYTES = 64 << 20
MAX_OUTPUT_BYTES = 64 << 20
MAX_RECORDS = 10_000


def run(arguments: list[str]) -> bytes:
    if arguments[:1] != ["gcloud"]:
        raise RuntimeError("unexpected provider executable")
    with subprocess.Popen(
        ["gcloud", *arguments[1:]],
        stdout=subprocess.PIPE,
    ) as process:
        if process.stdout is None:  # pragma: no cover
            raise RuntimeError("cannot capture gcloud output")
        output = process.stdout.read(MAX_PROVIDER_BYTES + 1)
        if len(output) > MAX_PROVIDER_BYTES:
            process.kill()
            process.wait()
            raise RuntimeError("invalid response or 10000-item safety limit reached; cannot prove completeness")
        if process.wait() != 0:
            raise RuntimeError("gcloud logging read failed")
    return output


def compact(value: dict[str, Any]) -> dict[str, Any]:
    return {key: item for key, item in value.items() if item is not None}


def identity(value: str) -> str:
    return quote(value, safe="/:@+").replace("~", "%7E")


def projected(record: Any) -> dict[str, Any]:
    if not isinstance(record, dict):
        raise TypeError
    resource = record.get("resource") or {}
    proto = record.get("protoPayload") or {}
    operation = record.get("operation") or {}
    if not all(isinstance(value, dict) for value in (resource, proto, operation)):
        raise TypeError
    labels = resource.get("labels") or {}
    auth = proto.get("authenticationInfo") or {}
    status = proto.get("status") or {}
    authorization = proto.get("authorizationInfo") or []
    if not isinstance(labels, dict) or not isinstance(auth, dict) or not isinstance(status, dict):
        raise TypeError
    if not isinstance(authorization, list) or any(not isinstance(item, dict) for item in authorization):
        raise ValueError
    project = labels.get("project_id", "unknown")
    insert_id = record.get("insertId")
    timestamp = record.get("timestamp")
    if not all(isinstance(value, str) and value for value in (project, insert_id, timestamp)):
        raise ValueError
    email = auth.get("principalEmail")
    return compact(
        {
            "uid": f"{project}@{insert_id}@{timestamp}",
            **{key: record.get(key) for key in ("insertId", "timestamp", "receiveTimestamp", "severity", "logName")},
            "resource": compact(
                {
                    "type": resource.get("type"),
                    "labels": compact(
                        {
                            key: labels.get(key)
                            for key in ("project_id", "location", "zone", "cluster_name", "namespace_name")
                        }
                    ),
                }
            ),
            "protoPayload": compact(
                {
                    **{key: proto.get(key) for key in ("serviceName", "methodName", "resourceName")},
                    "authenticationInfo": compact(
                        {
                            **{
                                key: auth.get(key)
                                for key in ("principalEmail", "principalSubject", "serviceAccountKeyName")
                            },
                            "principal_uri": (
                                f"person:email/{identity(email.lower())}" if isinstance(email, str) and email else None
                            ),
                        }
                    ),
                    "authorizationInfo": [
                        compact({key: item.get(key) for key in ("resource", "permission", "granted")})
                        for item in authorization
                    ],
                    "status": compact({"code": status.get("code")}),
                }
            ),
            "operation": compact({key: operation.get(key) for key in ("id", "producer", "first", "last")}),
        }
    )


def main(arguments: list[str]) -> int:
    if len(arguments) != 2:
        sys.stderr.write("usage: gcloud-audit-json.py <start> <end>\n")
        return 2
    start, end = arguments
    filter_value = f'timestamp>="{start}" AND timestamp<"{end}" AND log_id("cloudaudit.googleapis.com/activity")'
    try:
        raw = run(["gcloud", "logging", "read", filter_value, "--limit=10001", "--format=json"])
        value = json.loads(raw)
        if not isinstance(value, list) or len(value) > MAX_RECORDS:
            raise ValueError
        output = (
            json.dumps([projected(record) for record in value], ensure_ascii=False, separators=(",", ":")) + "\n"
        ).encode()
        if len(output) > MAX_OUTPUT_BYTES:
            raise RuntimeError(f"output bound exceeds {MAX_OUTPUT_BYTES} bytes")
    except (OSError, RuntimeError, UnicodeError, TypeError, ValueError, json.JSONDecodeError) as error:
        message = str(error) or "invalid response or 10000-item safety limit reached; cannot prove completeness"
        if isinstance(error, (ValueError, json.JSONDecodeError)):
            message = "invalid response or 10000-item safety limit reached; cannot prove completeness"
        sys.stderr.write(f"gcloud-audit-json.py: {message}\n")
        return 1
    sys.stdout.buffer.write(output)
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
