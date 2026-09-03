#!/bin/sh
# chromium-pages.sh <start> <end> [profile...] — written into <base>/bin by
# `fkf init --preset personal`.
#
# With no profile argument, reads every Chromium-family profile it finds. Name one or more to
# restrict collection; `chromium-pages.sh --profiles` lists the stable <browser>/<profile> labels.
# A requested profile that is absent fails loudly rather than filing a misleading empty day.
#
# Each locked History database is copied through a SQLite online backup before it is read;
# that snapshot includes committed rows still present only in the browser's live WAL file.
# Only HTTPS origins and paths leave the machine: authority userinfo, query strings, and
# fragments are discarded, while malformed and non-HTTPS URLs become null. Output is one JSON
# array over the half-open UTC window.
set -eu

list_only=false
if [ "${1:-}" = --profiles ]; then
  list_only=true
  start=1970-01-01T00:00:00Z
  end=1970-01-01T00:00:00Z
  shift
else
  start=${1:?usage: chromium-pages.sh <start> <end> [profile...]}
  end=${2:?usage: chromium-pages.sh <start> <end> [profile...]}
  shift 2
fi

# A browser keeps one History database per profile. Enumerating exactly one directory below
# each product root is portable across Linux and macOS and includes Default, Profile N, Guest,
# and any other profile name without assuming what the browser called it.
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

databases=$(mktemp)
trap 'rm -f "$databases"' EXIT
printf '%s\n' "$roots" | while IFS= read -r root; do
  [ -n "$root" ] && [ -d "$root" ] || continue
  for history_db in "$root"/*/History; do
    [ -f "$history_db" ] && printf '%s\n' "$history_db"
  done
done | sort -u > "$databases"

if [ ! -s "$databases" ]; then
  echo "chromium-pages.sh: no Chromium-family profile found" >&2
  exit 1
fi

# label_of <History path> prints the stable provenance and identity prefix carried by every
# record. It contains no home path or signed-in account.
label_of() {
  profile_dir=${1%/History}
  echo "$(basename "$(dirname "$profile_dir")")/$(basename "$profile_dir")"
}

# account_of is for the explicit --profiles inventory only and is never collected.
account_of() {
  root=$(dirname "$(dirname "$1")")
  name=$(basename "$(dirname "$1")")
  [ -f "$root/Local State" ] || { echo "-"; return; }
  jq -r --arg n "$name" '.profile.info_cache[$n].user_name // "-"' "$root/Local State" 2> /dev/null || echo "-"
}

if [ "$list_only" = true ]; then
  while IFS= read -r db; do
    [ -n "$db" ] || continue
    printf '%s\t%s\n' "$(label_of "$db")" "$(account_of "$db")"
  done < "$databases"
  exit 0
fi

# Restrict to named labels. A typo must fail instead of making a complete-looking empty source.
if [ "$#" -gt 0 ]; then
  selected=$(mktemp)
  for wanted in "$@"; do
    found=false
    while IFS= read -r db; do
      [ -n "$db" ] || continue
      if [ "$(label_of "$db")" = "$wanted" ]; then
        printf '%s\n' "$db" >> "$selected"
        found=true
      fi
    done < "$databases"
    if [ "$found" = false ]; then
      echo "chromium-pages.sh: no profile labelled '$wanted'; run 'chromium-pages.sh --profiles'" >&2
      rm -f "$selected"
      exit 1
    fi
  done
  mv "$selected" "$databases"
fi

out=$(mktemp)
copy=$(mktemp)
rows=$(mktemp)
trap 'rm -f "$databases" "$out" "$copy" "$rows"' EXIT
: > "$out"

while IFS= read -r history_db; do
  [ -n "$history_db" ] || continue
  profile=$(label_of "$history_db")

  # A main-file copy silently drops visits committed in Chrome's live History-wal. SQLite's
  # online backup reads one consistent snapshot across the main database and its WAL/SHM.
  rm -f "$copy"
  sqlite3 -init /dev/null -readonly "$history_db" ".backup '$copy'"

  # Chromium stores WebKit time: microseconds since 1601-01-01. Cast bounds to INTEGER because
  # strftime returns TEXT and SQLite sorts every integer before every string. -init /dev/null
  # prevents a personal ~/.sqliterc from corrupting the JSON stream.
  sqlite3 -init /dev/null -json "$copy" "
    select v.id as visit,
           u.url as url,
           u.title as title,
           case when v.originator_cache_guid = '' then 'local' else 'synced' end as origin,
           v.visit_duration / 1000000 as seconds,
           v.transition & 255 as transition,
           u.visit_count as visit_count,
           u.typed_count as typed_count,
           v.from_visit as from_visit,
           strftime('%Y-%m-%dT%H:%M:%SZ', v.visit_time/1000000 - 11644473600, 'unixepoch') as time
    from visits v join urls u on u.id = v.url
    where v.visit_time/1000000 - 11644473600 >= CAST(strftime('%s','$start') AS INTEGER)
      and v.visit_time/1000000 - 11644473600 <  CAST(strftime('%s','$end') AS INTEGER)
    order by v.visit_time" > "$rows"

  # Keep SQLite and jq in separate commands: POSIX pipelines report only the last exit status,
  # which could otherwise turn a corrupt or incompatible History database into a successful [].
  jq -c --arg profile "$profile" '
      def safe_url:
        if type != "string" then null
        else ([capture("^https://(?<authority>[^/?#]+)(?<path>/[^?#]*)?(?:[?#].*)?$")] | first) as $url
          | if $url == null then null
            else ($url.authority | split("@") | last) as $authority
            | if ($authority | test("^([A-Za-z0-9.-]+|\\[[0-9A-Fa-f:.]+\\])(:[0-9]+)?$"))
              then "https://" + $authority + ($url.path // "")
              else null
              end
            end
        end;
      .[]? | . + {
        url: (.url | safe_url),
        title: (if (.title // "") == "" then null else .title end),
        profile: $profile,
        uid: ($profile + "~" + (.visit | tostring))
      }
    ' "$rows" >> "$out"
done < "$databases"

jq -s -c 'sort_by(.time, .uid)' < "$out"
