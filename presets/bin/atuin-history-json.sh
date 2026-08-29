#!/bin/sh
# atuin-history-json.sh <start> <end> <database> — collect shell activity metadata without
# exposing the command column. SQLite and jq run as separate fail-fast stages.
set -eu

start=${1:?usage: atuin-history-json.sh <start> <end> <database>}
end=${2:?usage: atuin-history-json.sh <start> <end> <database>}
database=${3:?usage: atuin-history-json.sh <start> <end> <database>}
work_dir=$(mktemp -d "${TMPDIR:-/tmp}/fkf-atuin.XXXXXX")
rows=$work_dir/rows.ndjson
trap 'exit 1' HUP INT TERM
trap 'rm -rf "$work_dir"' 0

sqlite3 -init /dev/null -json "$database" \
  "select id, cwd, exit, duration, strftime('%Y-%m-%dT%H:%M:%SZ', timestamp/1000000000, 'unixepoch') as time from history where timestamp >= strftime('%s','$start')*1000000000 and timestamp < strftime('%s','$end')*1000000000 order by timestamp" \
  > "$rows"
jq -s -c 'add // []' "$rows"
