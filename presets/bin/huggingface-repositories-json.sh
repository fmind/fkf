#!/bin/sh
# huggingface-repositories-json.sh — bounded, projected metadata for every visible owned repo.
set -eu

case "${1:-}" in
  --version | -v) echo "huggingface-repositories-json.sh (fkf base helper)"; exit 0 ;;
  '') ;;
  *) echo "usage: huggingface-repositories-json.sh" >&2; exit 2 ;;
esac

hf_bin=$(command -v hf) || {
  echo "huggingface-repositories-json.sh: hf is required" >&2
  exit 1
}
python=$(sed -n '1s/^#!//p' "${hf_bin}")
case "${python}" in
  /*) ;;
  *) echo "huggingface-repositories-json.sh: cannot resolve hf's Python interpreter" >&2; exit 1 ;;
esac
[ -x "${python}" ] || {
  echo "huggingface-repositories-json.sh: hf's Python interpreter is not executable" >&2
  exit 1
}

"${python}" - <<'PY'
import itertools
import json
import sys

from huggingface_hub import HfApi

MAX_REPOSITORIES = 10_000
PREFIX = {
    "model": "",
    "dataset": "datasets/",
    "space": "spaces/",
    "bucket": "buckets/",
}

try:
    repositories = list(
        itertools.islice(HfApi().list_user_repos(), MAX_REPOSITORIES + 1)
    )
    if len(repositories) > MAX_REPOSITORIES:
        raise RuntimeError("repository ceiling reached; refusing a prefix")

    records = []
    for repository in repositories:
        if repository.type not in PREFIX:
            raise RuntimeError("provider returned an unknown repository type")
        if not isinstance(repository.id, str) or not repository.id:
            raise RuntimeError("provider returned a repository without an id")
        records.append(
            {
                "uid": repository.type + ":" + repository.id,
                "id": repository.id,
                "type": repository.type,
                "updated": repository.updated_at.date().isoformat(),
                "visibility": repository.visibility,
                "storageBytes": repository.storage,
                "url": "https://huggingface.co/" + PREFIX[repository.type] + repository.id,
            }
        )

    ids = [record["uid"] for record in records]
    if len(ids) != len(set(ids)):
        raise RuntimeError("provider returned duplicate repository ids")
    records.sort(key=lambda record: record["id"])
    json.dump(records, sys.stdout, separators=(",", ":"))
    sys.stdout.write("\n")
except Exception as error:
    print(
        f"huggingface-repositories-json.sh: collection failed ({type(error).__name__})",
        file=sys.stderr,
    )
    raise SystemExit(1) from None
PY
