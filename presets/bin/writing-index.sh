#!/bin/sh
# writing-index.sh <file>... — index explicitly declared authored documents.
#
# Prints one JSON array with one record per authored page, read from the front matter the file
# already carries. Deliberately not specific to any one site: it takes paths, so the source
# line is a glob and moving or adding a repository is an edit to fkf.yaml rather than to this
# script.
#
#   writing-index.sh ~/writing/articles/*.md
#   writing-index.sh ~/writing/articles/*.md ~/notes/drafts/*/draft.md
#
# Both delimiters are read: `+++` (TOML, what Hugo writes here) and `---` (YAML). A file with
# no front matter is still indexed, from its filename and modification time, so drafts that
# have not grown a header yet are not silently invisible.
#
# The id is the basename without its extension — the slug, which is what a URL and a wiki link
# use. When that basename is a GENERIC one, the parent directory carries the identity instead:
# a drafts pipeline stores eighteen articles as eighteen `draft.txt`, and keying on the file
# name would collapse them all. Two files that still reduce to the same slug are a collision the
# base cannot express, so the script fails and names both rather than dropping one.
set -eu

# Names that identify a role in a directory rather than a piece of work.
generic_slug() {
  case "$1" in
    draft | article | index | README | readme | announce | HANDOFF | main | content) return 0 ;;
    *) return 1 ;;
  esac
}

case "${1:-}" in
  --version | -v) echo "writing-index.sh (fkf preset helper)"; exit 0 ;;
  *) ;;
esac

[ "$#" -gt 0 ] || { echo "usage: writing-index.sh <file>..." >&2; exit 1; }
command -v jq > /dev/null 2>&1 || { echo "writing-index.sh: jq is required" >&2; exit 1; }
command -v yq > /dev/null 2>&1 || { echo "writing-index.sh: yq is required (mise use -g yq@latest)" >&2; exit 1; }

out=$(mktemp)
seen=$(mktemp)
front_source=$(mktemp)
trap 'rm -f "$out" "$seen" "$front_source"' EXIT

for file in "$@"; do
  [ -f "${file}" ] || {
    echo "writing-index.sh: '${file}' is not a regular file; check for an unmatched declared glob" >&2
    exit 1
  }

  slug=${file##*/}
  slug=${slug%.*}
  if generic_slug "${slug}"; then
    parent=${file%/*}
    slug=${parent##*/}
  fi

  if grep -qxF "${slug}" "${seen}" 2> /dev/null; then
    echo "writing-index.sh: two files reduce to the slug '${slug}'; the second is ${file}" >&2
    exit 1
  fi
  printf '%s\n' "${slug}" >> "${seen}"

  # The delimiter is decided by the first line, and the block is everything up to the next
  # line equal to it. awk rather than sed so the closing delimiter is matched exactly.
  delimiter=$(head -n 1 "${file}")
  case "${delimiter}" in
    '+++') format=toml ;;
    '---') format=yaml ;;
    *) format=none ;;
  esac

  if [ "${format}" = none ]; then
    # No front matter means no authored metadata. Do not promote a body line into stored
    # collector output: the slug is the deterministic, privacy-preserving title fallback.
    front='{}'
  else
    # A missing closing delimiter is malformed front matter even when the bytes before EOF
    # happen to parse. Extract first so yq's error status and diagnostics remain authoritative.
    if awk -v d="${delimiter}" '
      NR == 1 { next }
      $0 == d { closed = 1; exit }
      { print }
      END { if (!closed) exit 2 }
    ' "${file}" > "${front_source}"; then
      :
    else
      echo "writing-index.sh: ${file} has no closing ${delimiter} front-matter delimiter" >&2
      exit 1
    fi
    if front=$(yq -p="${format}" -o=json < "${front_source}"); then
      :
    else
      status=$?
      echo "writing-index.sh: malformed ${format} front matter in ${file}" >&2
      exit "${status}"
    fi
    [ -n "${front}" ] || front='{}'
  fi

  if front=$(printf '%s' "${front}" | jq -c '
    if . == null then {}
    elif type == "object" then .
    else error("front matter must decode to an object")
    end
  '); then
    :
  else
    status=$?
    echo "writing-index.sh: front matter in ${file} is not a metadata object" >&2
    exit "${status}"
  fi

  # The body is what remains after the block; its size is the only content fact stored.
  if [ "${format}" = none ]; then
    body_chars=$(wc -c < "${file}" | tr -d ' ')
  else
    body_chars=$(awk -v d="${delimiter}" 'NR==1 {next} !seen && $0==d {seen=1; next} seen {print}' "${file}" | wc -c | tr -d ' ')
  fi

  modified=$(date -u -r "${file}" +%Y-%m-%dT%H:%M:%SZ 2> /dev/null || echo '')
  # A path under $HOME is stored home-relative so the record survives a different machine.
  relative=$(printf '%s' "${file}" | sed "s#^${HOME}/#~/#")
  # repo is transcribed from the clone the file sits in, never guessed from the path.
  repo=$(git -C "$(dirname "${file}")" config --get remote.origin.url 2> /dev/null |
    sed -E 's#\.git$##; s#^.*[:/]([^/]+/[^/]+)$#\1#' || true)

  printf '%s' "${front}" | jq -c \
    --arg slug "${slug}" --arg path "${relative}" --arg modified "${modified}" \
    --arg repo "${repo}" --argjson chars "${body_chars:-0}" '
    . as $f
    | {
        id: $slug,
        slug: ($f.slug // $slug),
        title: ($f.title // $slug),
        description: ($f.description // null),
        date: ($f.date // null),
        # A Hugo date can be a string or a parsed timestamp; tostring keeps one shape.
        tags: (($f.tags // []) | if type == "array" then map(tostring) else [tostring] end),
        draft: ($f.draft // false),
        path: $path,
        modified: $modified,
        chars: $chars,
      }
    + (if $repo == "" then {} else {repo: $repo} end)
    + (if ($f.url // $f.permalink) then {url: ($f.url // $f.permalink)} else {} end)
  ' >> "${out}"
done

jq -s -c 'sort_by(.date // .modified // "") | reverse' < "${out}"
