#!/bin/sh
# jira-issues-json.sh <site> <project> <filter-jql> — collect one bounded Jira project snapshot.
set -eu

case "${1:-}" in --version | -v) echo "jira-issues-json.sh (fkf preset helper)"; exit 0 ;; esac
[ "$#" -eq 3 ] || {
  echo "usage: jira-issues-json.sh <site.atlassian.net> <PROJECT> <filter-jql>" >&2
  exit 2
}

site=$1
project=$2
filter=$3
case "$site" in
  *[!a-z0-9.-]* | .* | *..* | *-.* | *.-*)
    echo "jira-issues-json.sh: site must be a lowercase *.atlassian.net host" >&2; exit 2 ;;
  *.atlassian.net) ;;
  *) echo "jira-issues-json.sh: site must be a lowercase *.atlassian.net host" >&2; exit 2 ;;
esac
case "$project" in '' | *[!A-Z0-9_]* | [0-9_]*) echo "jira-issues-json.sh: invalid Jira project key" >&2; exit 2 ;; esac
[ "${#filter}" -le 512 ] || { echo "jira-issues-json.sh: filter JQL exceeds 512 bytes" >&2; exit 2; }
case "$filter" in '' | *"
"* | *""* | *ORDER[Bb][Yy]*)
  echo "jira-issues-json.sh: filter JQL must be one non-empty expression without ORDER BY" >&2
  exit 2
  ;;
esac

work_dir=$(mktemp -d "${TMPDIR:-/tmp}/fkf-jira-issues.XXXXXX")
raw=$work_dir/raw.json
projected=$work_dir/projected.json
trap 'exit 1' HUP INT TERM
trap 'rm -rf "$work_dir"' 0

# Limit-plus-one makes the completeness ceiling observable without ACLI's unbounded --paginate.
jql="project = \"$project\" AND ($filter) ORDER BY key ASC"
if ! acli jira workitem search --jql "$jql" \
  --fields key,summary,status,assignee,url --limit 10001 --json > "$raw"; then
  echo "jira-issues-json.sh: Jira search failed for project $project" >&2
  exit 1
fi

if ! jq -e --arg project "$project" --arg site "$site" '
  (if type == "array" then .
   elif type == "object" and (.issues | type == "array")
     and ((.nextPageToken? // "") == "") then .issues
   else error("expected an array or an issues array") end) as $issues
  | if ($issues | length) > 10000 then error("project exceeds the 10000-row completeness ceiling") else $issues end
  | map(. as $issue
      | ($issue.fields // $issue) as $fields
      | ($issue.key // $fields.key) as $key
      | ($fields.summary // $issue.summary) as $summary
      | ($fields.status // $issue.status) as $status
      | ($fields.assignee // $issue.assignee) as $assignee
      | ($fields.url // $issue.url // ("https://" + $site + "/browse/" + ($key // ""))) as $url
      | if ($key | type != "string" or test("^" + $project + "-[1-9][0-9]*$") | not)
        then error("result outside declared project") else . end
      | if ($summary | type != "string" or length == 0) then error("missing summary") else . end
      | if ($url | type != "string" or startswith("https://" + $site + "/") | not)
        then error("unsafe issue URL") else . end
      | {id: $key, title: $summary, url: $url,
         status: (if $status == null then null elif ($status | type) == "object" then $status.name else $status end),
         assignee: (if $assignee == null then null elif ($assignee | type) == "object" then ($assignee.displayName // $assignee.accountId) else $assignee end),
         project_uri: ("project:jira/" + $project), ticket_uri: ("ticket:jira/" + $key)})
  | if (map(.id) | unique | length) != length then error("duplicate issue keys") else . end
  | all(.[]; (.status == null or (.status | type == "string")) and (.assignee == null or (.assignee | type == "string")))
  ' "$raw" >/dev/null; then
  echo "jira-issues-json.sh: Jira returned malformed, duplicate, excessive, or out-of-scope results" >&2
  exit 1
fi

jq --arg project "$project" --arg site "$site" '
  (if type == "array" then . else .issues end)
  | map(. as $issue
      | ($issue.fields // $issue) as $fields
      | ($issue.key // $fields.key) as $key
      | ($fields.status // $issue.status) as $status
      | ($fields.assignee // $issue.assignee) as $assignee
      | {id: $key, title: ($fields.summary // $issue.summary),
         url: ($fields.url // $issue.url // ("https://" + $site + "/browse/" + $key)),
         status: (if $status == null then null elif ($status | type) == "object" then $status.name else $status end),
         assignee: (if $assignee == null then null elif ($assignee | type) == "object" then ($assignee.displayName // $assignee.accountId) else $assignee end),
         project_uri: ("project:jira/" + $project), ticket_uri: ("ticket:jira/" + $key)})
  | sort_by(.id)' "$raw" > "$projected"
cat "$projected"
