#!/usr/bin/env python3
"""Validate and project bounded NDJSON pages emitted by Google Workspace CLI."""

from __future__ import annotations

import json
import subprocess
import sys
from collections.abc import Iterable
from typing import Any
from urllib.parse import quote

COLLECTIONS = frozenset({"spaces", "files", "connections", "items", "messages", "conferenceRecords", "otherContacts"})
USAGE = (
    "gws-page-json.py <spaces|files|connections|items|messages|conferenceRecords|otherContacts> [command [argument...]]"
)
MAX_PROVIDER_BYTES = 64 << 20
MAX_OUTPUT_BYTES = 64 << 20


def compact(value: dict[str, Any]) -> dict[str, Any]:
    """Remove absent provider fields without treating false or empty values as absent."""
    return {key: item for key, item in value.items() if item is not None}


def identity(value: str) -> str:
    """Match FKF's URI component encoding while retaining identity delimiters."""
    return quote(value, safe=":/@+").replace("~", "%7E")


def documents(raw: bytes) -> list[Any]:
    """Decode a whitespace-separated stream of complete JSON documents."""
    text = raw.decode("utf-8")
    decoder = json.JSONDecoder()
    offset = 0
    values: list[Any] = []
    while offset < len(text):
        while offset < len(text) and text[offset].isspace():
            offset += 1
        if offset == len(text):
            break
        value, offset = decoder.raw_decode(text, offset)
        values.append(value)
    return values


def validated_pages(raw: bytes, collection: str) -> list[dict[str, Any]]:
    """Require a closed, unique, finite cursor chain before returning any page."""
    values = documents(raw)
    if not 1 <= len(values) <= 100 or any(not isinstance(value, dict) for value in values):
        raise ValueError
    pages = [value for value in values if isinstance(value, dict)]
    tokens: list[str] = []
    for index, page in enumerate(pages):
        records = page.get(collection)
        token = page.get("nextPageToken")
        if records is not None and not isinstance(records, list):
            raise ValueError
        if token is not None and (not isinstance(token, str) or not token):
            raise ValueError
        if index < len(pages) - 1 and token is None:
            raise ValueError
        if token is not None:
            tokens.append(token)
    if pages[-1].get("nextPageToken") is not None or len(tokens) != len(set(tokens)):
        raise ValueError
    return pages


def projected_owner(value: Any) -> dict[str, Any]:
    if not isinstance(value, dict):
        return {}
    email = value.get("emailAddress")
    return compact(
        {
            "displayName": value.get("displayName"),
            "emailAddress": email,
            "uri": f"person:email/{identity(email.lower())}" if isinstance(email, str) and email else None,
        }
    )


def projected_metadata(value: Any) -> dict[str, Any] | None:
    return compact({"primary": value.get("primary")}) if isinstance(value, dict) else None


def projected_page(page: dict[str, Any], collection: str) -> dict[str, Any]:
    values = page.get(collection) or []
    if collection in {"spaces", "files", "connections"} and any(not isinstance(item, dict) for item in values):
        raise ValueError
    if collection == "spaces":
        fields = (
            "name",
            "displayName",
            "spaceType",
            "spaceThreadingState",
            "lastActiveTime",
            "membershipCount",
            "spaceUri",
        )
        return {"spaces": [compact({field: item.get(field) for field in fields}) for item in values]}
    if collection == "files":
        fields = (
            "id",
            "name",
            "mimeType",
            "webViewLink",
            "modifiedTime",
            "createdTime",
            "size",
            "shared",
            "trashed",
            "parents",
        )
        return {
            "files": [
                compact(
                    {
                        **{field: item.get(field) for field in fields},
                        "owners": [projected_owner(owner) for owner in item.get("owners") or []],
                    }
                )
                for item in values
            ]
        }
    if collection == "connections":
        name_fields = (
            "displayName",
            "displayNameLastFirst",
            "unstructuredName",
            "familyName",
            "givenName",
            "middleName",
            "honorificPrefix",
            "honorificSuffix",
        )
        return {
            "connections": [
                compact(
                    {
                        "resourceName": item.get("resourceName"),
                        "names": [
                            compact(
                                {
                                    **{field: name.get(field) for field in name_fields},
                                    "metadata": projected_metadata(name.get("metadata")),
                                }
                            )
                            for name in item.get("names") or []
                        ],
                        "emailAddresses": [
                            compact(
                                {
                                    **{
                                        field: email.get(field)
                                        for field in ("value", "type", "formattedType", "displayName")
                                    },
                                    "uri": (
                                        f"person:email/{identity(email['value'].lower())}"
                                        if isinstance(email.get("value"), str) and email["value"]
                                        else None
                                    ),
                                    "metadata": projected_metadata(email.get("metadata")),
                                }
                            )
                            for email in item.get("emailAddresses") or []
                        ],
                    }
                )
                for item in values
            ]
        }
    return {key: value for key, value in page.items() if key != "nextPageToken"}


def encode(values: Iterable[dict[str, Any]]) -> bytes:
    output = bytearray()
    for value in values:
        line = (json.dumps(value, ensure_ascii=False, separators=(",", ":")) + "\n").encode()
        if len(output) + len(line) > MAX_OUTPUT_BYTES:
            raise RuntimeError(f"projected output bound exceeds {MAX_OUTPUT_BYTES} bytes")
        output.extend(line)
    return bytes(output)


def main(arguments: list[str]) -> int:
    if not arguments or arguments[0] not in COLLECTIONS:
        sys.stderr.write(f"usage: {USAGE}\n")
        return 2
    collection, *command = arguments
    try:
        if command:
            if command[:1] != ["gws"]:
                raise ValueError
            with subprocess.Popen(
                ["gws", *command[1:]],
                stdout=subprocess.PIPE,
            ) as process:
                if process.stdout is None:  # pragma: no cover
                    raise ValueError
                raw = process.stdout.read(MAX_PROVIDER_BYTES + 1)
                if len(raw) > MAX_PROVIDER_BYTES:
                    process.kill()
                    process.wait()
                    raise ValueError
                status = process.wait()
                if status != 0:
                    return status
        else:
            raw = sys.stdin.buffer.read(MAX_PROVIDER_BYTES + 1)
            if len(raw) > MAX_PROVIDER_BYTES:
                raise ValueError
        pages = validated_pages(raw, collection)
        output = encode(projected_page(page, collection) for page in pages)
    except RuntimeError as error:
        sys.stderr.write(f"gws-page-json.py: {error}\n")
        return 1
    except OSError, UnicodeDecodeError, json.JSONDecodeError, TypeError, ValueError:
        sys.stderr.write("gws-page-json.py: invalid token chain or page limit reached; cannot prove completeness\n")
        return 1
    sys.stdout.buffer.write(output)
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
