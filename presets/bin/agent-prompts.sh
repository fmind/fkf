#!/bin/sh
# agent-prompts.sh <start> <end> [preview-chars] — collect durable request metadata.
#
# Prints one JSON array with one record per REQUEST YOU WROTE in the half-open UTC window,
# read from ~/.agents/sessions/v1 — the append-only lineage store `dot agent session sync`
# writes.
#
# Why that store and not the harnesses' own directories: ~/.config/dot.yaml prunes
# ~/.claude/projects, ~/.codex/sessions, ~/.copilot/session-store.db, the Antigravity brain,
# ~/.grok/sessions, and opencode.db after 7 days. A backfill older than a week finds nothing
# there and says nothing about it. The v1 store is the durable copy, already normalized to
# {ts, agent, sid, role, content, cwd} across every harness, so one reader covers all of them.
#
# METADATA ONLY BY DEFAULT. What is stored is when you asked, which harness and model, which
# working directory and repository, how long the request was, and a bounded preview — the same
# shape the google-gmail source stores (subject and snippet, never the mail). The full text is
# never written into events/: `body:` fetches it on demand through agent-prompt-body.sh.
#
#   preview-chars   what to store of the request itself. Three modes:
#                     N       an N-character preview as the title. Default 200.
#                     0       no request text at all. Records keep every other field and stay
#                             addressable, but lexical retrieval can then only match on cwd,
#                             repo, harness, and model.
#                     full    the complete request in a `text` field, PLUS the 200-character
#                             title — the title stays bounded so a listing stays readable and
#                             so ranking is not dominated by whichever prompt was longest.
#
# TWO THINGS THIS SCRIPT MUST DO, because the raw store gets both wrong:
#
#   1. Deduplicate. A lineage holds one directory per ingested snapshot of the same
#      conversation, and every snapshot repeats the turns before it. The key is (agent, sid,
#      ts): two requests in one session cannot share a millisecond. (agent, sid, turn) is NOT a
#      key, because turn numbering shifts between snapshots of the same session.
#
#   2. Drop what the harness wrote for you. `role: "user"` covers far more than your prompts:
#      slash-command echoes, background task notifications, injected environment blocks, and
#      keep-alive nudges all arrive as user turns. Everything below is removed structurally
#      where it is tagged, and by a short documented prefix list where the harness sends plain
#      text. Both lists are yours to edit.
#
# Assistant turns are skipped: they are the model's words, not yours, and three times the
# volume. Change the `.role == "user"` test below to keep them.
#
# The id is <agent>-<sid>-<compact ts>: the deduplication key itself, so uniqueness is
# structural rather than lucky, and it is derived from the turn rather than from the snapshot
# it was read out of, so re-collecting the day yields the same id forever — which is what makes
# a URI to a prompt permanent.
set -eu

# `fkf status --probe` runs each present binary with --version and nothing else.
case "${1:-}" in --version | -v) echo "agent-prompts.sh (fkf preset helper)"; exit 0 ;; esac

start=${1:?usage: agent-prompts.sh <start> <end> [preview-chars]}
end=${2:?usage: agent-prompts.sh <start> <end> [preview-chars]}
preview=${3:-200}
# `full` is the preview mode that also keeps the body. The title is still bounded at 200.
store_text=false
if [ "$preview" = full ]; then
  store_text=true
  preview=200
fi
case "$preview" in
  '' | *[!0-9]*) echo "agent-prompts.sh: preview-chars must be a number or 'full'" >&2; exit 2 ;;
esac

command -v jq > /dev/null 2>&1 || { echo "agent-prompts.sh: jq is required" >&2; exit 1; }
find=$(command -v find)

case "$0" in
  */*) script_path=$0 ;;
  *) script_path=$(command -v "$0") ;;
esac
filter_dir=$(CDPATH='' cd -- "$(dirname -- "${script_path}")" && pwd -P)
[ -r "${filter_dir}/agent-prompt-filter.jq" ] || {
  echo "agent-prompts.sh: shared prompt filter is missing" >&2
  exit 1
}

store=$HOME/.agents/sessions/v1
[ -d "$store" ] || { printf '[]\n'; exit 0; }

raw=$(mktemp)
map=$(mktemp)
files=$(mktemp)
modified_marker=$(mktemp)
trap 'rm -f "$raw" "$map" "$map.next" "$files" "$modified_marker"' EXIT

# POSIX and BSD find have no -newermt. Use the portable -newer predicate against a UTC marker
# one second before the lower bound; the timestamp filter below keeps the logical window exact.
marker_stamp=$(jq -nr --arg time "$start" '$time | fromdateiso8601 - 1 | strftime("%Y%m%d%H%M.%S")')
TZ=UTC touch -t "$marker_stamp" "$modified_marker"

# No upper mtime bound: a transcript appended to after midnight can still hold the day's turns.
# Filter depth before jq opens anything so transcript-looking files outside the owned
# agent/lineage/session layout remain outside this source's privacy boundary.
"$find" "$store" -type f -name transcript.jsonl -newer "$modified_marker" \
  -exec sh -c '
    root=$1
    shift
    for file do
      relative=${file#"$root"/}
      depth=1
      remainder=$relative
      while [ "${remainder#*/}" != "$remainder" ]; do
        depth=$((depth + 1))
        remainder=${remainder#*/}
      done
      [ "$depth" -eq 4 ] && printf "%s\0" "$file"
    done
    exit 0
  ' sh "$store" {} + 2> /dev/null > "$files"
: > "$raw"

# Thousands of snapshot files are normal in the append-only lineage store. One jq per file
# spends most of the source budget starting processes; xargs batches the same deterministic
# filter over as many files as the platform's argument bound permits. input_filename recovers
# the harness/lineage/session metadata without shell interpolation.
if [ -s "$files" ]; then
  xargs -0 jq -L "$filter_dir" -cn \
    --arg start "$start" --arg end "$end" \
    --argjson preview "$preview" --argjson storeText "$store_text" '
      include "agent-prompt-filter";

      inputs
      | input_filename as $filename
      | ($filename | split("/")) as $path
      | $path[-4] as $rawAgent
      # Keep the raw harness label in the permanent id because changing it would break stored
      # prompt URIs. Expose one canonical agent name for joins with agent-sessions.
      | ($rawAgent | if . == "agy" then "antigravity" else . end) as $agent
      | $path[-3] as $lineage
      | $path[-2] as $session
      | input_line_number as $n
      | . as $r
      | select($r.role == "user")
      | select(($r.ts // "") >= $start and ($r.ts // "") < $end)
      | (($r.content // "") | tostring | normalize_agent_prompt) as $trimmed
      | {
          # The id IS the deduplication key (agent, sid, ts), so uniqueness is structural
          # rather than lucky. A shortened session id is not safe because harness-generated
          # subagent ids can share long prefixes.
          id: ($rawAgent + "-" + ($r.sid // "nosid") + "-"
               + (($r.ts // "") | gsub("[-:.]"; ""))),
          time: $r.ts,
          agent: $agent,
          sid: $r.sid,
          lineage: $lineage,
          session: $session,
          turn: $n,
          cwd: ($r.cwd // null),
          model: ($r.model // null),
          # `/` splits on a LITERAL string. The regex form, [splits("\\s+")], cost two CPU
          # minutes a day here because it materialises every word of every prompt.
          chars: ($trimmed | length),
          lines: (($trimmed / "\n") | length),
        }
      + (if $preview > 0
         # SLICE BEFORE normalising whitespace. Running gsub over the whole text and slicing
         # after was the single most expensive operation in this script.
         then { title: ($trimmed[0:($preview * 4)] | gsub("\\s+"; " ") | .[0:$preview]) }
         else { title: ($agent + " prompt in " + (($r.cwd // "?") | split("/") | last)) }
         end)
      + (if $storeText then { text: $trimmed } else {} end)
    ' < "$files" > "$raw"
fi

# origin_of <dir> prints the clone's origin URL, or nothing when the directory is not a clone.
origin_of() {
  [ -n "$1" ] && [ -d "$1" ] && git -C "$1" config --get remote.origin.url 2> /dev/null || true
}

# repo is transcribed from the clone the request was made in, never guessed from the path. The
# lookup runs once per DISTINCT directory rather than once per turn — a day holds hundreds of
# turns in a handful of directories, and a subprocess per turn made this the slowest source in
# the base.
printf '{}\n' > "$map"
jq -r '.cwd // empty' < "$raw" | sort -u | while IFS= read -r cwd; do
  repo=$(origin_of "$cwd" | sed -E 's#\.git$##; s#^.*[:/]([^/]+/[^/]+)$#\1#')
  [ -n "$repo" ] || continue
  jq -c --arg cwd "$cwd" --arg repo "$repo" '. + {($cwd): $repo}' < "$map" > "$map.next"
  mv "$map.next" "$map"
done

# Deduplicate last, on (agent, sid, time). sort_by(.session) makes the surviving record a pure
# function of the store's contents rather than of the order find walked it in.
jq -s -c --slurpfile repos "$map" '
  ($repos[0] // {}) as $byCwd
  | map(. + (($byCwd[.cwd // ""] // "") | if . == "" then {} else {repo: .} end))
  | group_by(.agent + "|" + (.sid // "") + "|" + (.time // ""))
  | map(sort_by(.session) | .[0])
  | sort_by(.time)
' < "$raw"
