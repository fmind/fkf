#!/bin/sh
# github-events-json.sh <start> <end> — guarded activity for the authenticated GitHub user.
#
# GitHub retains at most 300 events and at most 30 days. The helper accepts a completed-day
# window only when that whole window is still observable. A saturated feed whose oldest event
# is newer than the requested start fails rather than overwriting a previously collected day
# with a plausible-looking prefix. Payloads are intentionally excluded from stored metadata.
set -eu

case "${1:-}" in
  --version | -v) echo "github-events-json.sh (fkf base helper)"; exit 0 ;;
  *) ;;
esac

[ "$#" -eq 2 ] || {
  echo "usage: github-events-json.sh <start> <end>" >&2
  exit 2
}

start=$1
end=$2

if ! user=$(gh api /user --jq .login); then
  echo "github-events-json.sh: cannot identify the authenticated GitHub user" >&2
  exit 1
fi

case "${user}" in
  *[!A-Za-z0-9-]* | -*) echo "github-events-json.sh: invalid GitHub user: ${user}" >&2; exit 2 ;;
  *) ;;
esac

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

if ! start_epoch=$(rfc3339_to_epoch "${start}" 2>/dev/null); then
  echo "github-events-json.sh: start is not an RFC3339 timestamp: ${start}" >&2
  exit 2
fi
if ! end_epoch=$(rfc3339_to_epoch "${end}" 2>/dev/null); then
  echo "github-events-json.sh: end is not an RFC3339 timestamp: ${end}" >&2
  exit 2
fi
if [ "${start_epoch}" -ge "${end_epoch}" ]; then
  echo "github-events-json.sh: start must be before end" >&2
  exit 2
fi

now_epoch=$(jq -nr 'now | floor')
if [ "${start_epoch}" -lt "$((now_epoch - 30 * 86400))" ]; then
  echo "github-events-json.sh: requested start predates GitHub's 30-day event retention" >&2
  exit 1
fi
# GitHub documents event latency of up to six hours. Completed days are safely beyond it.
if [ "${end_epoch}" -gt "$((now_epoch - 6 * 3600))" ]; then
  echo "github-events-json.sh: requested end is inside GitHub's documented six-hour latency window" >&2
  exit 1
fi

work_dir=$(mktemp -d "${TMPDIR:-/tmp}/fkf-github-events.XXXXXX")
page_file=${work_dir}/page.json
pages=${work_dir}/pages.ndjson
raw=${work_dir}/events.json
: > "${pages}"
trap 'rm -rf "${work_dir}"' 0

# The events endpoint has a hard three-page/300-row ceiling and rejects page four. Fetch that
# boundary explicitly so a full page three can be classified by its cutoff instead of being
# misreported as a generic pagination failure.
page=1
while [ "${page}" -le 3 ]; do
  if ! gh api --method GET "/users/${user}/events" \
    -f per_page=100 -f "page=${page}" > "${page_file}"; then
    echo "github-events-json.sh: cannot list events for ${user} on page ${page}" >&2
    exit 1
  fi
  if ! count=$(jq -er '
    if type == "array" and length <= 100 then length
    else error("expected an event array of at most 100 rows")
    end
  ' "${page_file}"); then
    echo "github-events-json.sh: GitHub returned an invalid event page" >&2
    exit 1
  fi
  jq -c '.' "${page_file}" >> "${pages}"
  [ "${count}" -eq 100 ] || break
  page=$((page + 1))
done
jq -s 'add // []' "${pages}" > "${raw}"
count=$(jq -r 'length' "${raw}")
if ! jq -e '
  . as $events
  | ($events | length) == ($events | map(.id) | unique | length)
    and all($events[];
      (.id | type == "string" and length > 0) and
      (.type | type == "string" and length > 0) and
      (.created_at | type == "string") and
      ((try (.created_at | fromdateiso8601) catch null) | type == "number") and
      (.public | type == "boolean") and
      (.actor.login | type == "string" and length > 0) and
      (.repo.name | type == "string" and length > 0) and
      ((.org == null) or (.org.login | type == "string" and length > 0)))
' "${raw}" >/dev/null; then
  echo "github-events-json.sh: every event must be unique and carry the complete metadata projection" >&2
  exit 1
fi

if [ "${count}" -ge 300 ]; then
  # GitHub can surface an older PR event inside a newer activity page. Coverage is therefore
  # bounded by the final cursor item, not by the minimum timestamp among all returned rows.
  cutoff=$(jq -er '.[-1].created_at' "${raw}")
  if ! cutoff_epoch=$(rfc3339_to_epoch "${cutoff}" 2>/dev/null); then
    echo "github-events-json.sh: feed cutoff event has an invalid timestamp" >&2
    exit 1
  fi
  if [ "${cutoff_epoch}" -ge "${start_epoch}" ]; then
    echo "github-events-json.sh: the 300-event feed cuts off at ${cutoff}, at or after requested start ${start}; completeness cannot be proved" >&2
    exit 1
  fi
fi

jq --arg start "${start}" --arg end "${end}" '
  [ .[]
    | select(.created_at >= $start and .created_at < $end)
    | {
        id,
        type,
        title: (.type + " in " + .repo.name),
        created_at,
        public,
        actor: .actor.login,
        repo: .repo.name,
        repository_uri: ("repo:github.com/" + .repo.name),
        participant_uris: [("actor:github.com/" + .actor.login)],
        org: (.org.login // null)
      }
  ]
  | sort_by(.created_at, .id)
' "${raw}"
