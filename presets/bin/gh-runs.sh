#!/bin/sh
# gh-runs.sh <start> <end> [owner...] — collect Actions runs from owned repositories.
#
# GitHub has no cross-repository workflow-runs endpoint. Repositories are therefore enumerated
# with the paginated REST API, and each repository is queried for the exact half-open RFC3339
# window. A filtered endpoint exposes at most 1,000 runs, so saturated windows are recursively
# bisected. Any repository or run-query failure fails the collection rather than looking empty.
set -eu

case "${1:-}" in
  --version | -v) echo "gh-runs.sh (fkf base helper)"; exit 0 ;;
  *) ;;
esac

[ "$#" -ge 2 ] || {
  echo "usage: gh-runs.sh <start> <end> [owner...]" >&2
  exit 2
}

start=$1
end=$2
shift 2

command -v jq >/dev/null 2>&1 || { echo "gh-runs.sh: jq is required" >&2; exit 1; }
command -v gh >/dev/null 2>&1 || { echo "gh-runs.sh: gh is required" >&2; exit 1; }

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

if ! start_epoch=$(rfc3339_to_epoch "${start}" 2>/dev/null); then
  echo "gh-runs.sh: start is not an RFC3339 timestamp: ${start}" >&2
  exit 2
fi
if ! end_epoch=$(rfc3339_to_epoch "${end}" 2>/dev/null); then
  echo "gh-runs.sh: end is not an RFC3339 timestamp: ${end}" >&2
  exit 2
fi
if [ "${start_epoch}" -ge "${end_epoch}" ]; then
  echo "gh-runs.sh: start must be before end" >&2
  exit 2
fi

work_dir=$(mktemp -d "${TMPDIR:-/tmp}/fkf-github-runs.XXXXXX")
repos=${work_dir}/repos
records=${work_dir}/records.ndjson
repo_page=${work_dir}/repo-page.json
: > "${repos}"
: > "${records}"
trap 'exit 1' HUP INT TERM
trap 'rm -rf "$work_dir"' 0

if ! viewer=$(gh api /user --jq .login); then
  echo "gh-runs.sh: cannot identify the authenticated GitHub user" >&2
  exit 1
fi

# The shipped preset is account-neutral. With no explicit owners, use only the authenticated
# user's owned repositories; organizations remain an intentional local configuration choice.
[ "$#" -gt 0 ] || set -- "${viewer}"

for owner in "$@"; do
  case "${owner}" in
    *[!A-Za-z0-9-]* | -*)
      echo "gh-runs.sh: invalid GitHub owner: ${owner}" >&2
      exit 2
      ;;
    *) ;;
  esac

  if jq -en --arg owner "${owner}" --arg viewer "${viewer}" \
    '($owner | ascii_downcase) == ($viewer | ascii_downcase)' >/dev/null; then
    if ! github-generic-list-json.sh /user/repos -f affiliation=owner > "${repo_page}"; then
      echo "gh-runs.sh: cannot enumerate repositories owned by ${owner}" >&2
      exit 1
    fi
  else
    if ! github-generic-list-json.sh "/orgs/${owner}/repos" -f type=all > "${repo_page}"; then
      echo "gh-runs.sh: cannot enumerate repositories owned by ${owner}" >&2
      exit 1
    fi
  fi
  if ! jq -e '
    type == "array" and all(.[]; (.full_name | type == "string" and length > 0))
  ' "${repo_page}" >/dev/null; then
    echo "gh-runs.sh: repository listing contained an invalid name" >&2
    exit 1
  fi
  jq -r '.[].full_name' "${repo_page}" >> "${repos}"
done

sort -u "${repos}" -o "${repos}"

collect_repo_range() {
  range_start=$(epoch_to_rfc3339 "$2")
  # GitHub's created range is inclusive; subtract one second from fkf's exclusive end.
  range_end=$(epoch_to_rfc3339 "$(( $3 - 1 ))")
  page=$(mktemp "${work_dir}/page.XXXXXX")
  page_part=$(mktemp "${work_dir}/page-part.XXXXXX")
  page_stream=$(mktemp "${work_dir}/page-stream.XXXXXX")
  : > "${page_stream}"
  page_number=1
  while [ "${page_number}" -le 10 ]; do
    if ! gh api --method GET "/repos/$1/actions/runs" -f per_page=100 \
      -f "page=${page_number}" -f "created=${range_start}..${range_end}" > "${page_part}"; then
      echo "gh-runs.sh: cannot collect $1 for [${range_start}, ${range_end}]" >&2
      return 1
    fi
    if ! page_count=$(jq -er '
      if type == "object" and (.workflow_runs | type == "array")
      then .workflow_runs | length
      else error("expected a workflow-runs object")
      end
    ' "${page_part}"); then
      echo "gh-runs.sh: GitHub returned an invalid workflow-runs document for $1" >&2
      return 1
    fi
    jq -c '.' "${page_part}" >> "${page_stream}"
    [ "${page_count}" -eq 100 ] || break
    page_number=$((page_number + 1))
  done
  jq -s '.' "${page_stream}" > "${page}"
  if ! count=$(jq -er '
    if type == "array" and all(.[]; type == "object" and (.workflow_runs | type == "array"))
    then [.[].workflow_runs[]] | length
    else error("expected paginated workflow-runs objects")
    end
  ' "${page}"); then
    echo "gh-runs.sh: GitHub returned an invalid workflow-runs document for $1" >&2
    return 1
  fi

  if [ "${count}" -lt 1000 ]; then
    range_exclusive_end=$(epoch_to_rfc3339 "$3")
    jq -c --arg repo "$1" --arg start "${range_start}" --arg end "${range_exclusive_end}" '
      .[].workflow_runs[]
      | select(.created_at >= $start and .created_at < $end)
      | {
          databaseId: .id,
          workflowName: .name,
          displayTitle: (.display_title // .name // "workflow run"),
          status,
          conclusion,
          createdAt: .created_at,
          updatedAt: .updated_at,
          headBranch: .head_branch,
          headSha: .head_sha,
          event,
          url: .html_url,
          repo: $repo,
          repository_uri: ("repo:github.com/" + $repo)
        }
    ' "${page}" >> "${records}"
    return
  fi
  if [ "$(( $3 - $2 ))" -le 1 ]; then
    echo "gh-runs.sh: one-second slice for $1 [${range_start}, ${range_end}] returned 1000 runs; cannot prove completeness" >&2
    return 1
  fi

  collect_repo_range "$1" "$2" "$(( $2 + ($3 - $2) / 2 ))"
  collect_repo_range "$1" "$(( $2 + ($3 - $2) / 2 ))" "$3"
}

while IFS= read -r repo; do
  [ -n "${repo}" ] || continue
  collect_repo_range "${repo}" "${start_epoch}" "${end_epoch}"
done < "${repos}"

jq -s '
  sort_by(.url)
  | unique_by(.url)
  | sort_by(.createdAt, .repo, .databaseId)
' "${records}"
