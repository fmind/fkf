#!/bin/sh
# github-commits-json.sh <start> <end> — collect every commit authored by the active GitHub user.
#
# GitHub Search returns at most 1,000 rows for one query. Saturated half-open RFC3339 windows
# are therefore bisected until every slice is provably below that ceiling. A saturated
# one-second slice fails instead of filing partial data as a complete day.
set -eu

case "${1:-}" in
  --version | -v) echo "github-commits-json.sh (fkf base helper)"; exit 0 ;;
  *) ;;
esac

[ "$#" -eq 2 ] || {
  echo "usage: github-commits-json.sh <start> <end>" >&2
  exit 2
}

start=$1
end=$2

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
  echo "github-commits-json.sh: start is not an RFC3339 timestamp: ${start}" >&2
  exit 2
fi
if ! end_epoch=$(rfc3339_to_epoch "${end}" 2>/dev/null); then
  echo "github-commits-json.sh: end is not an RFC3339 timestamp: ${end}" >&2
  exit 2
fi
if [ "${start_epoch}" -ge "${end_epoch}" ]; then
  echo "github-commits-json.sh: start must be before end" >&2
  exit 2
fi

work_dir=$(mktemp -d "${TMPDIR:-/tmp}/fkf-github-commits.XXXXXX")
records=${work_dir}/records.ndjson
: > "${records}"
trap 'exit 1' HUP INT TERM
trap 'rm -rf "$work_dir"' 0

collect_range() {
  range_start=$(epoch_to_rfc3339 "$1")
  # GitHub's date range is inclusive, so one second is removed from fkf's exclusive end.
  range_end=$(epoch_to_rfc3339 "$(( $2 - 1 ))")
  page=$(mktemp "${work_dir}/page.XXXXXX")
  if ! gh search commits --author=@me "--author-date=${range_start}..${range_end}" \
    --json sha,repository,commit,url --limit 1000 > "${page}"; then
    echo "github-commits-json.sh: GitHub search failed for [${range_start}, ${range_end}]" >&2
    return 1
  fi
  if ! count=$(jq -er 'if type == "array" then length else error("expected an array") end' "${page}"); then
    echo "github-commits-json.sh: GitHub search did not return one JSON array" >&2
    return 1
  fi
  if ! jq -e '
    (type == "array") and all(.[];
      (.url | type == "string" and length > 0) and
      (.sha | type == "string" and length > 0) and
      (.commit.author.date | type == "string" and length > 0))
  ' "${page}" >/dev/null; then
    echo "github-commits-json.sh: every result must carry a URL, SHA, and author time" >&2
    return 1
  fi

  if [ "${count}" -lt 1000 ]; then
    jq -c '
      def fkf_identity: @uri | gsub("%3A"; ":") | gsub("%2F"; "/")
        | gsub("%40"; "@") | gsub("%2B"; "+") | gsub("~"; "%7E");
      def participant_uri:
        ascii_downcase as $email
        | (try ($email | capture("^(?:[0-9]+\\+)?(?<login>[a-z0-9](?:[a-z0-9-]{0,37}[a-z0-9])?)@users\\.noreply\\.github\\.com$")) catch null) as $github
        | if $github == null then "person:email/" + ($email | fkf_identity)
          else "actor:github.com/" + $github.login
          end;
      .[] | . + {
      repository_uri: ("repo:github.com/" + .repository.fullName),
      participant_uris: (if (.commit.author.email // "") == "" then []
        else [(.commit.author.email | participant_uri)] end)
    }' "${page}" >> "${records}"
    return
  fi
  if [ "$(( $2 - $1 ))" -le 1 ]; then
    echo "github-commits-json.sh: one-second slice [${range_start}, ${range_end}] returned 1000 results; cannot prove completeness" >&2
    return 1
  fi

  collect_range "$1" "$(( $1 + ($2 - $1) / 2 ))"
  collect_range "$(( $1 + ($2 - $1) / 2 ))" "$2"
}

collect_range "${start_epoch}" "${end_epoch}"
jq -s 'sort_by(.url) | unique_by(.url)' "${records}"
