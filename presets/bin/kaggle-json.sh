#!/bin/sh
# kaggle-json.sh [--all-pages] <kaggle arguments>... — bounded Kaggle JSON listings.
#
# Runs `kaggle ... --format json` and guarantees a JSON array on stdout.
#
# Two things the Kaggle CLI does that fkf cannot accept as-is:
#
#   1. An empty listing prints the sentence "No models found" — plain text, exit status 0 —
#      and fkf correctly fails a day whose command emitted something it cannot decode. Without
#      this wrapper every Kaggle source fails until its listing happens to be non-empty, which
#      is backwards: an empty listing is a fact, not an error.
#
#   2. A listing is capped at 100 items per page whatever --page-size says, with no marker on
#      the result to say more exists. `kernels list -m` returns 100 items on page 1, a
#      different 100 on page 2, and a different 100 on page 3. A single call therefore files a
#      third of the truth as though it were all of it.
#
# --all-pages walks pages until one comes back short, which is the only honest stopping
# condition the API offers. If the cap below is reached while pages are still coming back full,
# the script FAILS rather than returning a prefix: a truncated day that looks complete is the
# one failure this base is built to exclude.
#
# Anything that is neither valid JSON nor a recognised empty-listing sentence is passed through
# unchanged and fails the day, which is what should happen to a real error.
#
# The credential is the CLI's own: KAGGLE_API_TOKEN in your environment, or ~/.config/kaggle.
# fkf neither reads nor stores it.
set -eu

case "${1:-}" in --version | -v) echo "kaggle-json.sh (fkf preset helper)"; exit 0 ;; esac

command -v jq > /dev/null 2>&1 || { echo "kaggle-json.sh: jq is required" >&2; exit 1; }
command -v kaggle > /dev/null 2>&1 || { echo "kaggle-json.sh: kaggle is required (mise use -g pipx:kaggle@latest)" >&2; exit 1; }

PAGE_SIZE=100
MAX_PAGES=50

all_pages=false
if [ "${1:-}" = --all-pages ]; then
  all_pages=true
  shift
fi

raw=$(mktemp)
decoded=$(mktemp)
page_json=$(mktemp)
all=$(mktemp)
trap 'rm -f "$raw" "$decoded" "$page_json" "$all"' EXIT

# decode <file> prints the page as a compact JSON array, or nothing when the page is an empty
# listing. Any other output is a real error and stops the run.
decode() {
  first=$(sed -n '1p' "$1")
  case "$first" in
    'Next Page Token = '* | 'Next page token: '*) sed '1d' "$1" > "$decoded" ;;
    *) cp "$1" "$decoded" ;;
  esac
  if jq -e 'type == "array"' < "$decoded" > /dev/null 2>&1; then
    jq -c '.' < "$decoded"
  elif jq -e 'type == "object"' < "$decoded" > /dev/null 2>&1; then
    jq -c '[.]' < "$decoded"
  elif grep -qiE '^No [a-z ]+ (found|available)' "$decoded" || [ ! -s "$decoded" ]; then
    printf '[]\n'
  else
    cat "$decoded" >&2
    echo "kaggle-json.sh: kaggle printed something that is neither JSON nor an empty listing" >&2
    exit 1
  fi
}

if [ "$all_pages" = false ]; then
  kaggle "$@" > "$raw"
  decode "$raw"
  exit 0
fi

page=1
: > "$all"
while [ "$page" -le "$MAX_PAGES" ]; do
  kaggle "$@" --page-size "$PAGE_SIZE" -p "$page" > "$raw"
  decode "$raw" > "$page_json"
  count=$(jq -r 'length' "$page_json")
  if [ "$count" -gt "$PAGE_SIZE" ]; then
    echo "kaggle-json.sh: page $page exceeds the requested page size" >&2
    exit 1
  fi
  cat "$page_json" >> "$all"
  [ "$count" -eq "$PAGE_SIZE" ] || break
  page=$((page + 1))
done

if [ "$page" -gt "$MAX_PAGES" ]; then
  echo "kaggle-json.sh: still receiving full pages after $MAX_PAGES; raise MAX_PAGES rather than filing a prefix" >&2
  exit 1
fi

# Pages are concatenated as separate arrays; add flattens them, and unique_by guards against an
# item shifting across a page boundary between two requests. A record with no ref — a private
# notebook has one — falls back to its whole content, so several of them stay several records
# instead of collapsing into one.
jq -s -c 'add // [] | unique_by(if ((.ref // "") | length) > 0 then .ref else tojson end)' < "$all"
