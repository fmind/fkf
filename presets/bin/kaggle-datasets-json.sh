#!/bin/sh
# kaggle-datasets-json.sh — paginate, validate, and project owned datasets.
set -eu

case "${1:-}" in --version | -v) echo "kaggle-datasets-json.sh (fkf preset helper)"; exit 0 ;; esac

work_dir=$(mktemp -d "${TMPDIR:-/tmp}/fkf-kaggle-datasets.XXXXXX")
raw=$work_dir/raw.json
trap 'exit 1' HUP INT TERM
trap 'rm -rf "$work_dir"' 0

kaggle-json.sh --all-pages datasets list -m --format json > "$raw"
jq -c 'map({ref, title: (.title // .ref), totalBytes, lastUpdated,
  downloadCount, voteCount, usabilityRating})
  | if (all(.[]; ((.ref | type) == "string" and (.ref | length) > 0))
      and length == ([.[].ref] | unique | length))
    then sort_by(.ref) else error("datasets must have unique nonempty refs") end' "$raw"
