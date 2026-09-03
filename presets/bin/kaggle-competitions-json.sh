#!/bin/sh
# kaggle-competitions-json.sh — paginate, validate, and project entered competitions.
set -eu

case "${1:-}" in --version | -v) echo "kaggle-competitions-json.sh (fkf preset helper)"; exit 0 ;; esac

work_dir=$(mktemp -d "${TMPDIR:-/tmp}/fkf-kaggle-competitions.XXXXXX")
raw=$work_dir/raw.json
trap 'exit 1' HUP INT TERM
trap 'rm -rf "$work_dir"' 0

kaggle-json.sh --all-pages competitions list --group entered --format json > "$raw"
jq -c 'map({ref, deadline, category, reward, teamCount, userHasEntered, userRank})
  | if (all(.[]; ((.ref | type) == "string" and (.ref | length) > 0))
      and length == ([.[].ref] | unique | length))
    then sort_by(.ref) else error("competitions must have unique nonempty refs") end' "$raw"
