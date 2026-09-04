#!/bin/sh
# rss-json.sh <feed>... [--optional-opml <private-opml>] — prints a current index snapshot of
# each feed and its advertised items as one JSON array. A <feed> is a URL, or the path of an
# OPML file whose outlines carry xmlUrl attributes (the export every feed reader produces).
# The optional OPML is ignored when absent and treated as private when present: its endpoint
# URLs are replaced with deterministic opaque identities before output and never enter
# diagnostics. An OPML folder name travels with each item as `folder`, which is the closest
# thing a feed has to a tag. RSS 2.0, RSS 1.0, and Atom are all accepted: yq turns the XML into
# JSON and jq folds the three shapes into one record — id, time, title, url, feed, folder —
# before fkf sees it.
#
# Every feed must answer. A feed that fails to download or parse fails the snapshot, after every
# other feed was tried so the message names all of them at once. Feeds expose neither cursors nor
# totals, so they cannot prove a historical daily log; the complete HTTP documents they advertise
# now are honestly a point-in-time index. Dates are the hard part: RSS uses RFC 822
# (`Sat, 22 Aug 2026 10:00:00 +0200`), Atom uses RFC 3339 (`2026-08-22T10:00:00+02:00`), and
# jq's mktime ignores the zone that %z parsed, so the offset is read from the string and
# subtracted by hand. An addressable item whose date cannot be read fails the whole collection
# before the requested window is applied. Needs curl, yq (mikefarah, v4), jq, xargs, and either
# sha256sum (Linux) or shasum (macOS).
set -eu

# `fkf status --probe` runs each present binary with --version and nothing else. Answering it
# keeps a shipped helper from reporting "probe failed" on a healthy base.
case "${1:-}" in --version | -v) echo "rss-json.sh (fkf preset helper)"; exit 0 ;; esac

[ "$#" -gt 0 ] || {
  echo "usage: rss-json.sh <feed-url-or-opml>... [--optional-opml <private-opml>]" >&2
  exit 1
}

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

hash_text() {
  if command -v sha256sum >/dev/null 2>&1 && hash_output=$(sha256sum); then
    :
  elif command -v shasum >/dev/null 2>&1 && hash_output=$(shasum -a 256); then
    :
  else
    echo 'rss-json.sh: private feeds require sha256sum or shasum' >&2
    return 1
  fi
  hash_output=${hash_output%% *}
  case "$hash_output" in
    "" | *[!0-9a-f]*)
      echo 'rss-json.sh: the SHA-256 command returned an invalid digest' >&2
      return 1
      ;;
  esac
  [ "${#hash_output}" -eq 64 ] || {
    echo 'rss-json.sh: the SHA-256 command returned an invalid digest' >&2
    return 1
  }
  printf '%s\n' "$hash_output"
}

# Expand inputs into `<visibility> <url> <folder>`. Public paths are required, while the
# explicitly optional OPML is skipped only when it does not exist. Parsing errors stay generic
# so malformed private configuration cannot leak a nearby endpoint through a parser diagnostic.
: > "$tmp/raw-feeds"
opml_number=0
expand_opml() {
  opml=$1
  visibility=$2
  opml_number=$((opml_number + 1))
  json="$tmp/opml-$opml_number.json"
  records="$tmp/opml-$opml_number.feeds"

  if ! yq -p xml -o json '.' "$opml" > "$json" 2> "$tmp/opml-$opml_number.err"; then
    echo "rss-json.sh: cannot parse ${visibility} OPML" >&2
    exit 1
  fi
  if ! jq -r --arg visibility "$visibility" '
    def clean: gsub("[\\t\\r\\n]"; " ");
    def walk($folder):
      if type == "array" then .[] | walk($folder)
      elif type == "object" then
        (select(."+@xmlUrl") | "\($visibility)\t\(."+@xmlUrl")\t\($folder | clean)"),
        ((."+@text" // $folder) as $name | .outline // empty | walk($name))
      else empty end;
    .opml.body | walk("")' "$json" > "$records" 2> "$tmp/opml-$opml_number.err"; then
    echo "rss-json.sh: cannot read ${visibility} OPML outlines" >&2
    exit 1
  fi
  cat "$records" >> "$tmp/raw-feeds"
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --optional-opml)
      shift
      [ "$#" -gt 0 ] || {
        echo "rss-json.sh: --optional-opml requires a path" >&2
        exit 1
      }
      if [ -e "$1" ]; then
        [ -f "$1" ] || {
          echo "rss-json.sh: optional OPML is not a regular file" >&2
          exit 1
        }
        expand_opml "$1" private
      fi
      ;;
    --*)
      echo "rss-json.sh: unknown option: $1" >&2
      exit 1
      ;;
    *)
      # The positional is polymorphic: an existing OPML path or a bare feed URL. Classify it
      # here, or the scheme guard below blames the URL scheme for a public OPML the reader has
      # not created yet, which is the first thing a new base hits.
      if [ -f "$1" ]; then
        expand_opml "$1" public
      else
        case "$1" in
          http://* | https://*) printf 'public\t%s\t\n' "$1" >> "$tmp/raw-feeds" ;;
          *)
            echo "rss-json.sh: not an existing OPML file or an http/https feed URL: $1" >&2
            exit 1
            ;;
        esac
      fi
      ;;
  esac
  shift
done

[ -s "$tmp/raw-feeds" ] || {
  echo "rss-json.sh: the OPML names no feed (no outline carries xmlUrl)" >&2
  exit 1
}

# OPML is reviewed configuration, but keep it from turning the collector into an arbitrary
# local-file reader. Redirects are constrained independently below because a valid HTTP URL can
# otherwise redirect to another protocol. Private feeds get a stable opaque identity from the
# endpoint without ever writing the endpoint into collector output.
feed_number=0
: > "$tmp/feeds"
while IFS="$(printf '\t')" read -r visibility url folder; do
  feed_number=$((feed_number + 1))
  case "$url" in
    http://* | https://*) ;;
    *)
      if [ "$visibility" = private ]; then
        echo "rss-json.sh: private feed #$feed_number must use http or https" >&2
      else
        echo "rss-json.sh: feed URLs must use http or https: $url" >&2
      fi
      exit 1
      ;;
  esac
  if [ "$visibility" = private ]; then
    digest=$(printf '%s' "$url" | hash_text) || exit 1
    feed_identity="private-feed-$digest"
  else
    feed_identity=$url
  fi
  printf '%s\t%s\t%s\t%s\t%s\n' \
    "$feed_number" "$visibility" "$url" "$feed_identity" "$folder" >> "$tmp/feeds"
done < "$tmp/raw-feeds"

# Fetch in parallel, each feed to its own file, a failure to a marker that holds the reason.
# The User-Agent names the tool honestly and still satisfies the sites that refuse a bare one.
cut -f1-3 "$tmp/feeds" | xargs -P 8 -L 1 sh -c '
  tmp=$0; n=$1; visibility=$2; url=$3
  curl --fail --silent --show-error --location --max-time 60 --max-filesize 8388608 \
    --proto "=http,https" --proto-redir "=http,https" --retry 2 --retry-delay 2 \
    --retry-all-errors --user-agent "Mozilla/5.0 (compatible; fkf-rss-json/1.0; +https://fmind.github.io/fkf)" \
    --output "$tmp/$n.xml" "$url" 2> "$tmp/$n.err" || {
      rc=$?
      if [ "$visibility" = private ]; then
        printf "private feed #%s: download failed (curl exit %s)\n" "$n" "$rc" > "$tmp/$n.fail"
      else
        printf "%s: %s\n" "$url" "$(tail -n 1 "$tmp/$n.err")" > "$tmp/$n.fail"
      fi
    }
' "$tmp"

# One jq program folds every feed shape into the same record. yq keeps an element's text in
# "+content" and its attributes in "+@name" when both exist, which is why `text` looks in both.
normalize='
  def text: if type == "object" then (."+content" // ."+@href" // empty) else . end;
  # Publisher titles may carry invisible formatting copied from HTML or a rich-text editor.
  def clean_title:
    if type == "string" then
      gsub("\\p{Cf}"; "") | gsub("[\\p{Cc}\\s]+"; " ") | gsub("^ +| +$"; "")
    else "" end;
  def link: if type == "array" then
      (map(select(type != "object" or ."+@rel" == null or ."+@rel" == "alternate"))[0] // .[0])
    else . end | text;
  # The URI grammar intentionally refuses cleartext external links. Publishers still emit old
  # http permalinks for sites that serve the same page over HTTPS, so normalize at collection.
  def external_url: if type == "string" and startswith("http://") then "https://" + ltrimstr("http://") else . end;
  def scrub_endpoint:
    if $private and type == "string" then
      (($endpoint | external_url) as $normalized
       | split($endpoint) | join($feed) | split($normalized) | join($feed))
    else . end;
  def zone_seconds:
    (capture("(?<sign>[+-])(?<h>[0-9]{2}):?(?<m>[0-9]{2})\\s*$")
      | (if .sign == "-" then -1 else 1 end) * ((.h | tonumber) * 3600 + (.m | tonumber) * 60))
    // (({EST: -5, EDT: -4, CST: -6, CDT: -5, MST: -7, MDT: -6, PST: -8, PDT: -7}
        [(capture("(?<z>[A-Z]{3})\\s*$") // {z: ""}).z] // 0) * 3600);
  def to_time:
    . as $s | ($s | zone_seconds) as $zone
    | ($s | sub("\\.[0-9]+"; "") | sub("\\s*(Z|GMT|UTC|UT|[+-][0-9]{2}:?[0-9]{2}|[A-Z]{3})\\s*$"; "")) as $bare
    | (["%a, %d %b %Y %H:%M:%S", "%d %b %Y %H:%M:%S", "%Y-%m-%dT%H:%M:%S", "%Y-%m-%d %H:%M:%S", "%Y-%m-%d"]
        | map(. as $layout | try ($bare | strptime($layout) | mktime) catch empty) | first)
    | if . == null then null else (. - $zone | todate) end;
  def items: (.rss.channel.item // .feed.entry // ."rdf:RDF".item // []) | if type == "array" then . else [.] end;
  def record:
    ((.guid | text) // (.id | text) // (.link | link)) as $id
    | select($id != null)
    | (($id | tostring) | scrub_endpoint) as $safe_id
    | (((.title | text) // "") | clean_title) as $title
    # RSS GUIDs are only unique inside one feed. Namespace them by the normalized feed URL so
    # unrelated publishers cannot silently overwrite one another in the final unique-id fold.
    | { id: ("item:" + (($feed | external_url) | @uri) + ":" + ($safe_id | @uri)), kind: "item",
      time: ((.pubDate // .published // .updated // ."dc:date") | if . == null then null else (text | to_time) end),
      title: (if $title == "" then "Feed item " + ($safe_id | @uri) else $title end),
      url: (.link | link | external_url),
      feed: ($feed | external_url), folder: $folder,
      visibility: (if $private then "private" else "public" end) };
  [items[] | record] as $items
  | (.rss.channel // .feed // ."rdf:RDF".channel // {}) as $channel
  | {
      feed_record: {
        id: ("feed:" + ($feed | external_url)), kind: "feed",
        title: (((($channel.title | text) // "") | clean_title) as $title
          | if $title != "" then $title
            elif $private then "Private feed"
            else "Feed " + ($feed | @uri)
            end),
        url: (if $private then null else ($feed | external_url) end),
        site_url: (if $private then null else ($channel.link | link | external_url) end), folder: $folder,
        item_count: ($items | length),
        visibility: (if $private then "private" else "public" end)
      },
      items: $items
    }
  | if $private then
      .items |= map(if (.url == $endpoint or .url == ($endpoint | external_url)) then .url = null else . end)
    else . end
  | walk(scrub_endpoint)'  # no guid, id, or link: not an article, nothing to address

while IFS="$(printf '\t')" read -r n visibility url feed_identity folder; do
  [ -f "$tmp/$n.fail" ] && continue
  if [ "$visibility" = private ]; then private=true; label="private feed #$n"; else private=false; label=$url; fi
  yq -p xml -o json '.' "$tmp/$n.xml" > "$tmp/$n.json" 2> "$tmp/$n.err" ||
    { printf '%s: not XML\n' "$label" > "$tmp/$n.fail"; continue; }
  if ! jq -c --arg feed "$feed_identity" --arg endpoint "$url" --argjson private "$private" \
    --arg folder "$folder" "$normalize" \
    "$tmp/$n.json" > "$tmp/$n.records" 2> "$tmp/$n.err"; then
    printf '%s: cannot normalize feed JSON\n' "$label" > "$tmp/$n.fail"
    continue
  fi
  if [ "$visibility" = private ]; then
    # An endpoint credential can reappear in a different GUID or article URL. Hash every private
    # item identity and suppress every private item URL before validation, output, or diagnostics.
    jq -r '.items[].id | @base64' "$tmp/$n.records" > "$tmp/$n.identities"
    : > "$tmp/$n.hashes"
    while IFS= read -r encoded_identity; do
      digest=$(printf '%s' "$encoded_identity" | hash_text) || exit 1
      printf '%s\n' "$digest" >> "$tmp/$n.hashes"
    done < "$tmp/$n.identities"
    jq -R -s 'split("\n") | map(select(length > 0))' \
      "$tmp/$n.hashes" > "$tmp/$n.hashes.json"
    if ! jq -c --arg feed "$feed_identity" --slurpfile hashes "$tmp/$n.hashes.json" '
      ($hashes[0]) as $hashes
      | if (.items | length) != ($hashes | length) then
          error("private item identity count changed during hashing")
        else . end
      | .items = (.items | to_entries | map(
          . as $entry | $entry.value
          | .id = ("item:" + $feed + ":" + $hashes[$entry.key])
          | .url = null))
    ' "$tmp/$n.records" > "$tmp/$n.private-records"; then
      printf '%s: cannot protect private item identities\n' "$label" > "$tmp/$n.fail"
      continue
    fi
    mv "$tmp/$n.private-records" "$tmp/$n.records"
  fi
  if ! jq -e 'all(.items[]; .time != null)' "$tmp/$n.records" >/dev/null; then
    diagnostic=$(jq -r '
      [.items[] | select(.time == null) | {feed, id}]
      | "unparseable date for \(length) addressable item(s); first=" + ((.[0] // {}) | tojson)
    ' "$tmp/$n.records")
    printf '%s: %s\n' "$label" "$diagnostic" > "$tmp/$n.fail"
    continue
  fi
done < "$tmp/feeds"

if ls "$tmp"/*.fail > /dev/null 2>&1; then
  echo "rss-json.sh: $(ls "$tmp"/*.fail | wc -l | tr -d ' ') feed(s) failed; fix or remove them from the list:" >&2
  cat "$tmp"/*.fail >&2
  exit 1
fi

# One record per namespaced id. Exact duplicate declarations from the same feed collapse, while
# identical feed-local GUIDs from different publishers remain distinct. A feed record remains
# even when the valid document currently advertises no item, preserving source coverage.
cat "$tmp"/*.records |
  jq -s '[.[].feed_record] + [.[].items[]]
    | [group_by(.id)[] | .[0]]
    | sort_by(.kind, (.time // ""), (.title // ""))'
