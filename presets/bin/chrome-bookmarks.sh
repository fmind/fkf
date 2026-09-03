#!/bin/sh
# chrome-bookmarks.sh — prints every bookmark from every detected Chromium-family profile as one
# JSON array: a profile-qualified stable guid, the title, the URL, the folder path that holds it,
# and when it was added. It backs a `kind: index` source, because a bookmark list is a curated
# point-in-time snapshot and a per-day document of it would be noise. Each file is plain JSON
# that the browser rewrites whole, so unlike the History database it needs no copy.
set -eu

# `fkf status --probe` runs each present binary with --version and nothing else. Answering it
# keeps a shipped helper from reporting "probe failed" on a healthy base.
case "${1:-}" in --version | -v) echo "chrome-bookmarks.sh (fkf preset helper)"; exit 0 ;; esac

roots="
$HOME/.config/chromium
$HOME/.config/google-chrome
$HOME/.config/google-chrome-beta
$HOME/.config/microsoft-edge
$HOME/.config/BraveSoftware/Brave-Browser
$HOME/.config/vivaldi
$HOME/Library/Application Support/Chromium
$HOME/Library/Application Support/Google/Chrome
$HOME/Library/Application Support/Microsoft Edge
$HOME/Library/Application Support/BraveSoftware/Brave-Browser
$HOME/Library/Application Support/Vivaldi
"

bookmarks_files=$(mktemp)
out=$(mktemp)
trap 'rm -f "$bookmarks_files" "$out"' EXIT
printf '%s\n' "$roots" | while IFS= read -r root; do
  [ -n "$root" ] && [ -d "$root" ] || continue
  for bookmarks in "$root"/*/Bookmarks; do
    [ -f "$bookmarks" ] && printf '%s\n' "$bookmarks"
  done
done | sort -u > "$bookmarks_files"
[ -s "$bookmarks_files" ] || {
  echo "chrome-bookmarks.sh: no Chromium-family bookmark profile found" >&2
  exit 1
}

# Chromium stores WebKit time: microseconds since 1601-01-01, which is 11644473600 seconds
# before the Unix epoch. Folders recurse carrying their names, so each bookmark records the
# path that holds it — the closest thing a bookmark has to tags.
: > "$out"
while IFS= read -r bookmarks; do
  profile_dir=${bookmarks%/Bookmarks}
  profile="$(basename "$(dirname "$profile_dir")")/$(basename "$profile_dir")"
  jq -c --arg profile "$profile" '
    def node($path):
      if .type == "url" then
        if (.guid | type) != "string" or .guid == "" then error("bookmark has no stable guid")
        else
          { guid: .guid, title: .name, url: .url, folder: ($path | join("/")),
            added: ((.date_added | tonumber) / 1000000 - 11644473600 | floor | todate) }
        end
      elif .type == "folder" then .name as $name | (.children // [])[] | node($path + [$name])
      else empty end;
    .roots[] | objects | node([])
    | . + {profile: $profile, uid: ($profile + "~" + .guid)}
  ' "$bookmarks" >> "$out"
done < "$bookmarks_files"

jq -s -c 'sort_by(.profile, .folder, .title, .uid)' "$out"
