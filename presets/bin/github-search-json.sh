#!/bin/sh
# github-search-json.sh <prs|issues> assignee <start> <end> — written into
# <base>/bin by `fkf init --preset personal|team`.
#
# GitHub Search exposes at most 1,000 results for one query, and a timed-out search can return a
# partial success with incomplete_results=true. `gh search --json` renders only the items, hiding
# that completeness signal. This helper reads the REST envelopes directly, recursively bisects a
# saturated half-open window, and accepts a leaf only when every page is complete and its item
# count equals total_count. Canonical URLs are de-duplicated across split boundaries.
set -eu

[ "$#" -eq 4 ] || {
  echo "usage: github-search-json.sh <prs|issues> assignee <start> <end>" >&2
  exit 2
}

kind=$1
mode=$2
start=$3
end=$4

case "$kind:$mode" in
  prs:assignee) type_qualifier=is:pr ;;
  issues:assignee) type_qualifier=is:issue ;;
  *)
    echo "github-search-json.sh: expected prs assignee or issues assignee" >&2
    exit 2
    ;;
esac

# fkf renders UTC timestamps, while accepting an explicit offset here keeps the helper honest
# when somebody invokes the installed script directly. GitHub receives normalized UTC seconds.
rfc3339_to_epoch() {
  jq -ner --arg timestamp "$1" '
    ($timestamp
      | capture("^(?<stamp>[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2})(?<zone>Z|[+-][0-9]{2}:[0-9]{2})$")) as $parsed
    | ($parsed.stamp + "Z" | fromdateiso8601) as $local
    | if $parsed.zone == "Z" then $local
      else ($parsed.zone
        | capture("^(?<sign>[+-])(?<hour>[0-9]{2}):(?<minute>[0-9]{2})$")) as $offset
      | ($offset.hour | tonumber) as $hour
      | ($offset.minute | tonumber) as $minute
      | if $hour > 23 or $minute > 59 then error("invalid RFC3339 offset")
        else (($hour * 60 + $minute) * 60) as $seconds
        | if $offset.sign == "+" then $local - $seconds else $local + $seconds end
        end
      end
  '
}

epoch_to_rfc3339() {
  jq -nr --argjson epoch "$1" '$epoch | todateiso8601'
}

if ! start_epoch=$(rfc3339_to_epoch "$start" 2>/dev/null); then
  echo "github-search-json.sh: start is not an RFC3339 timestamp: $start" >&2
  exit 2
fi
if ! end_epoch=$(rfc3339_to_epoch "$end" 2>/dev/null); then
  echo "github-search-json.sh: end is not an RFC3339 timestamp: $end" >&2
  exit 2
fi
if [ "$start_epoch" -ge "$end_epoch" ]; then
  echo "github-search-json.sh: start must be before end" >&2
  exit 2
fi

work_dir=$(mktemp -d "${TMPDIR:-/tmp}/fkf-github-search.XXXXXX")
records=$work_dir/records.ndjson
max_pages=10 # GitHub search caps one query at 1,000 rows and each page requests 100.
: > "$records"
trap 'exit 1' HUP INT TERM
trap 'rm -rf "$work_dir"' 0

collect_range() {
  range_start=$(epoch_to_rfc3339 "$1")
  # GitHub's range syntax is inclusive; subtracting one second preserves fkf's [start, end).
  range_end=$(epoch_to_rfc3339 "$(( $2 - 1 ))")
  query="$type_qualifier assignee:@me updated:$range_start..$range_end"
  raw_page=$(mktemp "$work_dir/raw-page.XXXXXX")
  page=$(mktemp "$work_dir/page.XXXXXX")
  : > "$raw_page"
  # Search items include bodies. Project the metadata inside gh before anything reaches disk.
  # Explicit page numbers keep the provider loop finite; total_count proves when it is complete.
  projection='{total_count,incomplete_results,items:[.items[] | {number,title,html_url,updated_at,repository_url,state,user:(if .user == null then null else {login:.user.login} end),assignees:[.assignees[]? | {login}]}]}'
  page_number=1
  retrieved_so_far=0
  while [ "$page_number" -le "$max_pages" ]; do
    response=$(mktemp "$work_dir/response.XXXXXX")
    if ! gh api --method GET /search/issues \
      -H 'Accept: application/vnd.github+json' \
      -H 'X-GitHub-Api-Version: 2026-03-10' \
      -f "q=$query" -f sort=updated -f order=asc -F per_page=100 -F page="$page_number" \
      --jq "$projection" > "$response"; then
      echo "github-search-json.sh: GitHub REST search page $page_number failed for [$range_start, $range_end]" >&2
      return 1
    fi
    if ! jq -e '
      type == "object"
      and (.total_count | type == "number" and . >= 0 and floor == .)
      and (.incomplete_results | type == "boolean")
      and (.items | type == "array")' "$response" >/dev/null; then
      echo "github-search-json.sh: GitHub REST search returned an invalid page for [$range_start, $range_end]" >&2
      return 1
    fi
    if jq -e '.incomplete_results' "$response" >/dev/null; then
      echo "github-search-json.sh: GitHub REST search returned incomplete_results=true for [$range_start, $range_end]; cannot prove completeness" >&2
      return 1
    fi
    cat "$response" >> "$raw_page"
    response_total=$(jq -r '.total_count' "$response")
    if [ "$response_total" -ge 1000 ]; then
      break
    fi
    page_count=$(jq -r '.items | length' "$response")
    retrieved_so_far=$((retrieved_so_far + page_count))
    if [ "$retrieved_so_far" -ge "$response_total" ] || [ "$page_count" -eq 0 ]; then
      break
    fi
    page_number=$((page_number + 1))
  done
  if ! jq -s '.' "$raw_page" > "$page"; then
    echo "github-search-json.sh: GitHub REST search did not return JSON envelopes for [$range_start, $range_end]" >&2
    return 1
  fi

  if ! jq -e '
    type == "array" and length > 0
    and all(.[];
      type == "object"
      and (.total_count | type == "number" and . >= 0 and floor == .)
      and (.incomplete_results | type == "boolean")
      and (.items | type == "array"))' "$page" >/dev/null; then
    echo "github-search-json.sh: GitHub REST search returned an invalid paginated envelope for [$range_start, $range_end]" >&2
    return 1
  fi
  if jq -e 'any(.[]; .incomplete_results)' "$page" >/dev/null; then
    echo "github-search-json.sh: GitHub REST search returned incomplete_results=true for [$range_start, $range_end]; cannot prove completeness" >&2
    return 1
  fi
  if ! total=$(jq -er '
    ([.[].total_count] | unique) as $totals
    | if ($totals | length) == 1 then $totals[0]
      else error("total_count changed between pages")
      end' "$page"); then
    echo "github-search-json.sh: GitHub REST search changed total_count between pages for [$range_start, $range_end]; cannot prove completeness" >&2
    return 1
  fi

  if [ "$total" -ge 1000 ]; then
    if [ "$(( $2 - $1 ))" -le 1 ]; then
      echo "github-search-json.sh: one-second slice [$range_start, $range_end] reported $total results; cannot prove completeness" >&2
      return 1
    fi

    # Function positional parameters are restored after each recursive call, so recomputing the
    # midpoint avoids mutable global range state in portable POSIX shell.
    collect_range "$1" "$(( $1 + ($2 - $1) / 2 ))"
    collect_range "$(( $1 + ($2 - $1) / 2 ))" "$2"
    return
  fi

  if ! jq -e '
    all(.[].items[];
      (.number | type == "number")
      and (.title | type == "string")
      and (.html_url | type == "string" and length > 0)
      and (.updated_at | type == "string" and length > 0)
      and (.repository_url | type == "string" and test("/repos/[^/]+/[^/]+$"))
      and (.state | type == "string")
      and (.user == null or (.user.login | type == "string" and length > 0))
      and ((.assignees // []) | type == "array" and all(.[].login; type == "string" and length > 0)))' \
    "$page" >/dev/null; then
    echo "github-search-json.sh: GitHub REST search returned an invalid issue item for [$range_start, $range_end]" >&2
    return 1
  fi
  retrieved=$(jq -r '[.[].items[]] | length' "$page")
  unique_urls=$(jq -r '[.[].items[].html_url] | unique | length' "$page")
  if [ "$retrieved" -ne "$total" ]; then
    echo "github-search-json.sh: retrieved $retrieved of $total results for [$range_start, $range_end]; cannot prove completeness" >&2
    return 1
  fi
  if [ "$unique_urls" -ne "$total" ]; then
    echo "github-search-json.sh: retrieved $retrieved results but only $unique_urls unique URLs for [$range_start, $range_end]; cannot prove completeness" >&2
    return 1
  fi

  jq -c '.[].items[]
    | { number, title, url: .html_url, updatedAt: .updated_at,
        repository: {nameWithOwner: (.repository_url | capture("/repos/(?<repo>[^/]+/[^/]+)$").repo)},
        repository_uri: ("repo:github.com/" + (.repository_url | capture("/repos/(?<repo>[^/]+/[^/]+)$").repo)),
        state, author: (if .user == null then null else {login: .user.login} end),
        assignee_uris: [(.assignees // [])[].login | "actor:github.com/" + .],
        participant_uris: ([(if .user == null then empty else .user.login end), (.assignees // [])[].login]
          | unique | map("actor:github.com/" + .)) }' \
    "$page" >> "$records"
}

collect_range "$start_epoch" "$end_epoch"
jq -s 'sort_by(.url) | unique_by(.url)' "$records"
