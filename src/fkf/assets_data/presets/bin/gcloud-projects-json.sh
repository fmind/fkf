#!/bin/sh
# gcloud-projects-json.sh — collect a complete bounded project snapshot. Provider and projection
# stages are separate so a failed listing can never become a successful empty JSON result.
set -eu

work_dir=$(mktemp -d "${TMPDIR:-/tmp}/fkf-gcloud-projects.XXXXXX")
projects=$work_dir/projects.json
trap 'exit 1' HUP INT TERM
trap 'rm -rf "$work_dir"' 0

gcloud projects list --limit=10001 --format=json > "$projects"
jq -e '
  def compact: with_entries(select(.value != null));
  if type != "array" then
    error("gcloud-projects: invalid response")
  elif length > 10000 then
    error("gcloud-projects: 10000-item safety limit reached; cannot prove completeness")
  else
    map({projectId,name,projectNumber,lifecycleState,createTime,
      parent:(if .parent == null then null else (.parent | {type,id} | compact) end)} | compact)
  end
' "$projects"
