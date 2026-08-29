#!/bin/sh
# github-list-json.sh <notifications start end|user-repositories|org-repositories org> — fetch a
# complete GitHub REST list through explicit pages. `gh api --paginate` has no request ceiling;
# this helper follows Link rel=next itself and fails before emitting anything if 100 pages do
# not exhaust the collection.
set -eu

mode=${1:-}
case "$mode:$#" in
  notifications:3) start=$2; end=$3 ;;
  user-repositories:1) ;;
  org-repositories:2)
    org=$2
    case "$org" in "" | *[!A-Za-z0-9_.-]*) echo "github-list-json.sh: invalid organization" >&2; exit 2 ;; esac
    ;;
  *)
    echo "usage: github-list-json.sh <notifications start end|user-repositories|org-repositories org>" >&2
    exit 2
    ;;
esac

max_pages=100
page_size=100
work_dir=$(mktemp -d "${TMPDIR:-/tmp}/fkf-github-list.XXXXXX")
response=$work_dir/response
headers=$work_dir/headers
body=$work_dir/body
records=$work_dir/records.ndjson
: > "$records"
trap 'exit 1' HUP INT TERM
trap 'rm -rf "$work_dir"' 0

page_number=1
while [ "$page_number" -le "$max_pages" ]; do
  case "$mode" in
    notifications)
      if ! gh api --method GET --include /notifications \
        -F all=true -f since="$start" -f before="$end" \
        -F per_page="$page_size" -F page="$page_number" > "$response"; then
        echo "github-list-json.sh: GitHub notifications page $page_number failed" >&2
        exit 1
      fi
      ;;
    user-repositories)
      if ! gh api --method GET --include /user/repos \
        -f sort=updated -f direction=desc \
        -F per_page="$page_size" -F page="$page_number" > "$response"; then
        echo "github-list-json.sh: GitHub repository page $page_number failed" >&2
        exit 1
      fi
      ;;
    org-repositories)
      if ! gh api --method GET --include "/orgs/$org/repos" \
        -f type=all -f sort=updated -f direction=desc \
        -F per_page="$page_size" -F page="$page_number" > "$response"; then
        echo "github-list-json.sh: GitHub organization repository page $page_number failed" >&2
        exit 1
      fi
      ;;
  esac

  # gh --include prints one HTTP header block, a blank line, then the JSON body. The Link header
  # is the provider's completeness signal; array length is not proof that another page is absent.
  sed -n '1,/^[[:space:]]*$/p' "$response" > "$headers"
  sed '1,/^[[:space:]]*$/d' "$response" > "$body"
  if ! jq -e 'type == "array"' "$body" >/dev/null; then
    echo "github-list-json.sh: GitHub page $page_number was not a JSON array" >&2
    exit 1
  fi
  case "$mode" in
    notifications)
      if ! jq -e '
        all(.[];
          (.updated_at | type) == "string" and
          ((try (.updated_at | fromdateiso8601) catch null) | type) == "number")
      ' "$body" >/dev/null; then
        echo "github-list-json.sh: every notification must carry a valid updated_at timestamp" >&2
        exit 1
      fi
      jq -c --arg start "$start" --arg end "$end" '
        def compact: with_entries(select(.value != null));
        .[]
        | select((.updated_at | type) == "string" and .updated_at >= $start and .updated_at < $end)
        | {
          id, unread, reason, updated_at, last_read_at,
          subject: ((.subject // {})
            | {title, url, latest_comment_url, type} | compact),
          repository: ((.repository // {})
            | {full_name, html_url} | compact),
          repository_uri: (if (.repository.full_name // "") == "" then null else ("repo:github.com/" + .repository.full_name) end)
        } | compact
      ' "$body" >> "$records"
      ;;
    *)
      jq -c '.[] | {
        nameWithOwner: .full_name,
        url: .html_url,
        repository_uri: ("repo:github.com/" + .full_name),
        updatedAt: .updated_at,
        isArchived: .archived
      }' "$body" >> "$records"
      ;;
  esac

  if sed -n '/^[Ll][Ii][Nn][Kk]: .*rel="next"/p' "$headers" | sed -n '1p' | read -r _; then
    if [ "$page_number" -eq "$max_pages" ]; then
      echo "github-list-json.sh: reached the 100-page safety limit with rel=next; cannot prove completeness" >&2
      exit 1
    fi
    page_number=$((page_number + 1))
    continue
  fi
  cat "$records"
  exit 0
done

echo "github-list-json.sh: pagination ended without a terminal page; cannot prove completeness" >&2
exit 1
