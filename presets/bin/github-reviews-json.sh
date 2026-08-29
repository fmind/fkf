#!/bin/sh
# github-reviews-json.sh <start> <end> — written into <base>/bin by
# `fkf init --preset personal|team`.
#
# GitHub's contribution connection returns actual PullRequestReview nodes and exposes a cursor
# plus totalCount. This helper follows every page, verifies that the count is complete, and
# projects only review metadata. contributionsCollection's upper bound is inclusive, so the
# final projection enforces fkf's exact half-open [start, end) window.
set -eu

[ "$#" -eq 2 ] || {
  echo "usage: github-reviews-json.sh <start> <end>" >&2
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

if ! start_epoch=$(rfc3339_to_epoch "$start" 2>/dev/null); then
  echo "github-reviews-json.sh: start is not an RFC3339 timestamp: $start" >&2
  exit 2
fi
if ! end_epoch=$(rfc3339_to_epoch "$end" 2>/dev/null); then
  echo "github-reviews-json.sh: end is not an RFC3339 timestamp: $end" >&2
  exit 2
fi
if [ "$start_epoch" -ge "$end_epoch" ]; then
  echo "github-reviews-json.sh: start must be before end" >&2
  exit 2
fi
start_utc=$(jq -nr --argjson epoch "$start_epoch" '$epoch | todateiso8601')
end_utc=$(jq -nr --argjson epoch "$end_epoch" '$epoch | todateiso8601')

query='query($from: DateTime!, $to: DateTime!, $cursor: String) {
  viewer {
    contributionsCollection(from: $from, to: $to) {
      pullRequestReviewContributions(first: 100, after: $cursor, orderBy: {direction: ASC}) {
        totalCount
        nodes {
          occurredAt
          user { login }
          repository { nameWithOwner }
          pullRequest { number title url }
          pullRequestReview { id fullDatabaseId submittedAt state url }
        }
        pageInfo { hasNextPage endCursor }
      }
    }
  }
}'

work_dir=$(mktemp -d "${TMPDIR:-/tmp}/fkf-github-reviews.XXXXXX")
page=$work_dir/page.json
records=$work_dir/records.ndjson
review_ids=$work_dir/review-ids.ndjson
: > "$records"
: > "$review_ids"
trap 'exit 1' HUP INT TERM
trap 'rm -rf "$work_dir"' 0

cursor=
seen_count=0
expected_total=
page_number=0
max_pages=100
max_records=10000
while :; do
  page_number=$((page_number + 1))
  if [ "$page_number" -gt "$max_pages" ]; then
    echo "github-reviews-json.sh: reached the 100-page safety limit; cannot prove completeness" >&2
    exit 1
  fi
  if [ -n "$cursor" ]; then
    if ! gh api graphql -f query="$query" -f from="$start_utc" -f to="$end_utc" \
      -f cursor="$cursor" > "$page"; then
      echo "github-reviews-json.sh: GitHub GraphQL page failed" >&2
      exit 1
    fi
  elif ! gh api graphql -f query="$query" -f from="$start_utc" -f to="$end_utc" > "$page"; then
    echo "github-reviews-json.sh: GitHub GraphQL page failed" >&2
    exit 1
  fi

  if ! jq -e '
    def connection: .data.viewer.contributionsCollection.pullRequestReviewContributions;
    ((.errors // []) | length == 0)
    and (connection | type == "object")
    and (connection.nodes | type == "array")
    and (connection.nodes | length <= 100)
    and (connection.totalCount | type == "number" and . >= 0 and floor == .)
    and (connection.pageInfo.hasNextPage | type == "boolean")
    and (connection.nodes | all(.[];
      (.occurredAt | type == "string" and test("^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$"))
      and
      (.pullRequestReview.id | type == "string" and length > 0)
      and (.pullRequestReview.fullDatabaseId | type == "string" and test("^[1-9][0-9]*$"))
      and (.pullRequestReview.submittedAt | type == "string" and test("^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$"))
      and (.pullRequestReview.state | type == "string" and length > 0)
      and (.pullRequestReview.url | type == "string" and startswith("https://"))
      and (.pullRequest.number | type == "number" and . > 0 and floor == .)
      and (.pullRequest.title | type == "string" and length > 0)
      and (.pullRequest.url | type == "string" and startswith("https://"))
      and (.repository.nameWithOwner | type == "string" and test("^[^/]+/[^/]+$"))
      and (.user.login | type == "string" and length > 0)
    ))
  ' "$page" >/dev/null; then
    echo "github-reviews-json.sh: GitHub GraphQL returned an invalid review contribution page" >&2
    exit 1
  fi

  page_total=$(jq -r '.data.viewer.contributionsCollection.pullRequestReviewContributions.totalCount' "$page")
  if [ -z "$expected_total" ]; then
    expected_total=$page_total
    if [ "$expected_total" -gt "$max_records" ]; then
      echo "github-reviews-json.sh: totalCount $expected_total exceeds the 100-page safety limit; cannot prove completeness" >&2
      exit 1
    fi
  elif [ "$page_total" -ne "$expected_total" ]; then
    echo "github-reviews-json.sh: totalCount changed while paginating; cannot prove completeness" >&2
    exit 1
  fi
  page_count=$(jq -r '.data.viewer.contributionsCollection.pullRequestReviewContributions.nodes | length' "$page")
  seen_count=$((seen_count + page_count))
  if [ "$seen_count" -gt "$expected_total" ]; then
    echo "github-reviews-json.sh: received more nodes than totalCount; cannot prove completeness" >&2
    exit 1
  fi
  jq -c '.data.viewer.contributionsCollection.pullRequestReviewContributions.nodes[].pullRequestReview.id' \
    "$page" >> "$review_ids"

  jq -c --arg start "$start_utc" --arg end "$end_utc" '
    .data.viewer.contributionsCollection.pullRequestReviewContributions.nodes[]
    | select(.pullRequestReview.submittedAt >= $start and .pullRequestReview.submittedAt < $end)
    | {
        id: ("repos/" + .repository.nameWithOwner
          + "/pulls/" + (.pullRequest.number | tostring)
          + "/reviews/" + .pullRequestReview.fullDatabaseId),
        reviewId: .pullRequestReview.id,
        occurredAt: .occurredAt,
        submittedAt: .pullRequestReview.submittedAt,
        state: .pullRequestReview.state,
        url: .pullRequestReview.url,
        title: .pullRequest.title,
        pullRequest: {
          number: .pullRequest.number,
          title: .pullRequest.title,
          url: .pullRequest.url
        },
        repo: .repository.nameWithOwner,
        reviewer: .user.login,
        repository_uri: ("repo:github.com/" + .repository.nameWithOwner),
        participant_uris: [("actor:github.com/" + .user.login)]
      }
  ' "$page" >> "$records"

  has_next=$(jq -r '.data.viewer.contributionsCollection.pullRequestReviewContributions.pageInfo.hasNextPage' "$page")
  if [ "$has_next" = false ]; then
    break
  fi
  if [ "$page_count" -eq 0 ]; then
    echo "github-reviews-json.sh: GraphQL returned an empty page with hasNextPage=true; cannot prove completeness" >&2
    exit 1
  fi
  next_cursor=$(jq -er '
    .data.viewer.contributionsCollection.pullRequestReviewContributions.pageInfo.endCursor
    | select(type == "string" and length > 0)
  ' "$page") || {
    echo "github-reviews-json.sh: next GraphQL page has no cursor; cannot prove completeness" >&2
    exit 1
  }
  if [ "$next_cursor" = "$cursor" ]; then
    echo "github-reviews-json.sh: GraphQL pagination cursor did not advance; cannot prove completeness" >&2
    exit 1
  fi
  cursor=$next_cursor
done

if [ "$seen_count" -ne "$expected_total" ]; then
  echo "github-reviews-json.sh: received $seen_count of totalCount $expected_total review contributions" >&2
  exit 1
fi
unique_count=$(jq -s 'unique | length' "$review_ids")
if [ "$unique_count" -ne "$seen_count" ]; then
  echo "github-reviews-json.sh: GraphQL pages contain duplicate review nodes; cannot prove completeness" >&2
  exit 1
fi

jq -s 'sort_by(.submittedAt, .id)' "$records"
