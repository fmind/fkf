#!/bin/sh
# github-stars-json.sh — collect and project the authenticated user's complete starred list.
set -eu

case "${1:-}" in --version | -v) echo "github-stars-json.sh (fkf preset helper)"; exit 0 ;; esac

work_dir=$(mktemp -d "${TMPDIR:-/tmp}/fkf-github-stars.XXXXXX")
raw=$work_dir/raw.json
trap 'exit 1' HUP INT TERM
trap 'rm -rf "$work_dir"' 0

github-generic-list-json.sh user/starred -H 'Accept: application/vnd.github.star+json' > "$raw"
# GitHub prose may contain invisible Unicode that FKF deliberately refuses in a subject line.
jq -c '
  def clean_title:
    if type == "string" then
      gsub("\\p{Cf}"; "") | gsub("[\\p{Cc}\\s]+"; " ") | gsub("^ +| +$"; "")
    else "" end;
  .[]
  | (.repo.description | clean_title) as $description
  | {starred_at, full_name: .repo.full_name, url: .repo.html_url,
  repository_uri: ("repo:github.com/" + .repo.full_name),
  language: .repo.language, topics: .repo.topics, stars: .repo.stargazers_count,
  archived: .repo.archived,
  title: (.repo.full_name + (if $description == "" then "" else ": " + $description end))}' "$raw"
