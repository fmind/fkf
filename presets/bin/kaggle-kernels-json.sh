#!/bin/sh
# kaggle-kernels-json.sh — bounded metadata for the authenticated user's kernels.
# Kaggle exposes no cursor and withholds individual identity metadata for some private rows, so
# use explicit pages, address visible rows by ref, and retain indistinguishable redacted rows as
# one count-bearing aggregate rather than fabricating position-derived identities.
set -eu

case "${1:-}" in
  --version | -v) echo "kaggle-kernels-json.sh (fkf base helper)"; exit 0 ;;
  '') ;;
  *) echo "usage: kaggle-kernels-json.sh" >&2; exit 2 ;;
esac

kaggle_bin=$(command -v kaggle) || {
  echo "kaggle-kernels-json.sh: kaggle is required" >&2
  exit 1
}
python=$(sed -n '1s/^#!//p' "${kaggle_bin}")
case "${python}" in
  /*) ;;
  *) echo "kaggle-kernels-json.sh: cannot resolve kaggle's Python interpreter" >&2; exit 1 ;;
esac
[ -x "${python}" ] || {
  echo "kaggle-kernels-json.sh: kaggle's Python interpreter is not executable" >&2
  exit 1
}

"${python}" - <<'PY'
import json
import sys

from kaggle.api.kaggle_api_extended import KaggleApi

PAGE_SIZE = 100
MAX_PAGES = 50


class InvariantError(Exception):
    pass


def text(value):
    return value if isinstance(value, str) else ""


def timestamp(value):
    if value is None:
        return None
    if isinstance(value, str):
        return value
    if hasattr(value, "isoformat"):
        return value.isoformat()
    raise InvariantError("invalid-last-run-time")


try:
    api = KaggleApi()
    api.authenticate()
    records = []
    redacted = []
    complete = False

    for page in range(1, MAX_PAGES + 1):
        response = api.kernels_list_with_response(
            page=page,
            page_size=PAGE_SIZE,
            mine=True,
            sort_by="dateCreated",
        )
        if response is None:
            raise InvariantError("provider-no-response")
        items = list(response.kernels or [])
        if len(items) > PAGE_SIZE:
            raise InvariantError("page-size-exceeded")
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

    ids = [record["id"] for record in records]
    if len(ids) != len(set(ids)):
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
    json.dump(records, sys.stdout, separators=(",", ":"))
    sys.stdout.write("\n")
except InvariantError as error:
    print(f"kaggle-kernels-json.sh: collection failed ({error})", file=sys.stderr)
    raise SystemExit(1) from None
except Exception as error:
    print(f"kaggle-kernels-json.sh: collection failed (provider-{type(error).__name__})", file=sys.stderr)
    raise SystemExit(1) from None
PY
