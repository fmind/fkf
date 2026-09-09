#!/bin/sh
# mise-tools-json.sh — project the active mise tool inventory without a shell pipeline.
set -eu

case "${1:-}" in --version | -v) echo "mise-tools-json.sh (fkf preset helper)"; exit 0 ;; esac

work_dir=$(mktemp -d "${TMPDIR:-/tmp}/fkf-mise-tools.XXXXXX")
raw=$work_dir/raw.json
trap 'exit 1' HUP INT TERM
trap 'rm -rf "$work_dir"' 0

mise ls --current --json > "$raw"
jq -c 'to_entries | map({id: .key, version: (.value[0].version // null),
  requested: (.value[0].requested_version // null), source: (.value[0].source.path // null),
  installed: ([.value[].installed] | any), active: ([.value[].active] | any),
  title: (.key + " " + (.value[0].version // "?"))})' "$raw"
