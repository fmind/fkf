#!/bin/sh
# github-generic-list-json.sh <endpoint> [gh-api-args...] — exhaust one array-valued GitHub REST listing.
# A short page proves completion; a full final page fails rather than returning a prefix.
set -eu

case "${1:-}" in
  --version | -v) echo "github-generic-list-json.sh (fkf base helper)"; exit 0 ;;
  '') echo "usage: github-generic-list-json.sh <endpoint> [gh-api-args...]" >&2; exit 2 ;;
  *) ;;
esac

endpoint=$1
shift
case "${endpoint}" in
  -* | *[!A-Za-z0-9_./-]*)
    echo "github-generic-list-json.sh: invalid REST endpoint" >&2
    exit 2
    ;;
  *) ;;
esac

page_size=100
max_pages=100
work_dir=$(mktemp -d "${TMPDIR:-/tmp}/fkf-github-list.XXXXXX")
page_file=${work_dir}/page.json
pages=${work_dir}/pages.ndjson
: > "${pages}"
trap 'exit 1' HUP INT TERM
trap 'rm -rf "${work_dir}"' 0

page=1
complete=false
while [ "${page}" -le "${max_pages}" ]; do
  if ! gh api --method GET "${endpoint}" "$@" \
    -f "per_page=${page_size}" -f "page=${page}" > "${page_file}"; then
    echo "github-generic-list-json.sh: GitHub listing failed on page ${page}" >&2
    exit 1
  fi
  if ! count=$(jq -er '
    if type == "array" then length else error("expected an array") end
  ' "${page_file}"); then
    echo "github-generic-list-json.sh: GitHub returned a non-array page" >&2
    exit 1
  fi
  if [ "${count}" -gt "${page_size}" ]; then
    echo "github-generic-list-json.sh: GitHub returned more than ${page_size} rows on one page" >&2
    exit 1
  fi
  jq -c '.' "${page_file}" >> "${pages}"
  if [ "${count}" -lt "${page_size}" ]; then
    complete=true
    break
  fi
  page=$((page + 1))
done

if [ "${complete}" != true ]; then
  echo "github-generic-list-json.sh: page ${max_pages} was full; refusing a potentially truncated listing" >&2
  exit 1
fi

jq -s 'add // []' "${pages}"
