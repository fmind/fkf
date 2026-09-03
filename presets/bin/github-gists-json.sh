#!/bin/sh
# github-gists-json.sh — collect and project the authenticated user's complete gist list.
set -eu

case "${1:-}" in --version | -v) echo "github-gists-json.sh (fkf preset helper)"; exit 0 ;; esac

work_dir=$(mktemp -d "${TMPDIR:-/tmp}/fkf-github-gists.XXXXXX")
raw=$work_dir/raw.json
trap 'exit 1' HUP INT TERM
trap 'rm -rf "$work_dir"' 0

github-generic-list-json.sh gists > "$raw"
# GitHub prose may contain invisible Unicode that FKF deliberately refuses in a subject line.
jq -c '
  def compact: with_entries(select(.value != null));
  def clean_title:
    if type == "string" then
      gsub("\\p{Cf}"; "") | gsub("[\\p{Cc}\\s]+"; " ") | gsub("^ +| +$"; "")
    else "" end;
  .[]
  | ((.description // "") | clean_title) as $description
  | {id, description: (if $description == "" then "Gist " + (.id | tostring) else $description end),
  url: .html_url, updated_at, public, files: (.files | keys)} | compact' "$raw"
