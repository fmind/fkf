#!/bin/sh
# agent-sessions.sh <start> <end> — written into <base>/bin by `fkf init --preset personal`.
#
# Prints one JSON array with one record per coding-agent session that has recorded activity in
# the exact half-open UTC window, read from the state each harness keeps — one block per harness:
#   Claude Code      ~/.claude/projects/<cwd>/<session>.jsonl               one object per line
#   Codex CLI        ~/.codex/sessions/YYYY/MM/DD/rollout-*.jsonl           session_meta first
#   Gemini CLI       ~/.gemini/tmp/<project>/chats/session-*.json[l]        one document or a header
#   OpenCode         ~/.local/share/opencode/opencode.db                    SQLite: session, message
#   Copilot CLI      ~/.copilot/session-store.db                            SQLite: sessions, turns
#   Antigravity CLI  ~/.gemini/antigravity-cli/history.jsonl                one object per line
# A record is metadata only: the session id, when it was active that day, the harness, the
# working directory and branch, a safe owner/name projected from the repository its clone points
# at, and the harness's own title line when it keeps one. No prompt or response text is ever read
# out, and subagent threads are skipped because they belong to a session already listed.
#
# Needs POSIX find, touch, jq, and sqlite3 for the two SQLite-backed harnesses. After init writes
# it, it is yours: add a block for another harness, or delete the ones you do not use.
set -eu

start=${1:?usage: agent-sessions.sh <start> <end>}
end=${2:?usage: agent-sessions.sh <start> <end>}
command -v jq >/dev/null 2>&1 || { echo "agent-sessions.sh: jq is required" >&2; exit 1; }

# Provider timestamps use RFC3339 with optional fractional seconds. Comparing those strings to
# fkf's whole-second bounds is incorrect (`...00.001Z` sorts before `...00Z`), so every JSON
# block uses this one parser. Offset timestamps are accepted even though current harnesses emit Z.
rfc3339='
def instant:
  try (
    ([capture("^(?<whole>[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2})(?<fraction>\\.[0-9]+)?(?<zone>Z|[+-][0-9]{2}:[0-9]{2})$")] | first) as $part
    | if $part == null then null
      else (($part.whole + "Z" | fromdateiso8601)
            + (("0" + ($part.fraction // "")) | tonumber)
            - (if $part.zone == "Z" then 0
               else (((($part.zone[1:3] | tonumber) * 60) + ($part.zone[4:6] | tonumber)) * 60)
                    * (if $part.zone[0:1] == "+" then 1 else -1 end)
               end))
      end
  ) catch null;
'

# Two files rather than one pipeline: a failure in any block must fail the day, and a POSIX
# pipeline reports only its last command.
raw=$(mktemp); out=$(mktemp); paths=$(mktemp); payload=$(mktemp)
modified_marker=$(mktemp); candidates=$(mktemp)
trap 'rm -f "$raw" "$out" "$paths" "$payload" "$modified_marker" "$candidates"' EXIT
marker_stamp=$(jq -nr --arg time "$start" '$time | fromdateiso8601 - 1 | strftime("%Y%m%d%H%M.%S")')
TZ=UTC touch -t "$marker_stamp" "$modified_marker"

# modified_since <dir> <pattern> <max-depth> lists candidate files touched at or after the window
# opened. The one-second overlap makes the inclusive lower bound honest on fractional filesystems;
# inner timestamp checks discard the over-read. No upper mtime bound on purpose: a transcript
# updated on a later day can still hold activity from this day.
modified_since() {
  [ -d "$1" ] || return 0
  find "$1" -type f -name "$2" -newer "$modified_marker" > "$candidates" 2>/dev/null
  while IFS= read -r file; do
    relative=${file#"$1"/}
    depth=1
    remainder=$relative
    while [ "${remainder#*/}" != "$remainder" ]; do
      depth=$((depth + 1))
      remainder=${remainder#*/}
    done
    [ "$depth" -le "$3" ] && printf '%s\n' "$file"
  done < "$candidates"
  return 0
}

# sql <db> <query> reads one metadata query from a harness database and never a content
# column. -init /dev/null keeps a personal ~/.sqliterc from changing the output format.
sql() {
  command -v sqlite3 >/dev/null 2>&1 || { echo "agent-sessions.sh: sqlite3 is required to read $1" >&2; exit 1; }
  sqlite3 -init /dev/null -batch -readonly -json "$1" "$2"
}

# origin_of <dir> prints the clone's origin URL, or nothing when the directory is not a clone.
origin_of() {
  [ -n "$1" ] && [ -d "$1" ] && git -C "$1" config --get remote.origin.url 2>/dev/null || true
}

# repo_name accepts only an exact owner/name from a plain identifier, a URL path, or an
# SCP-style remote. Authority userinfo and malformed paths never cross the metadata boundary.
repo_name() {
  candidate=$1
  case "$candidate" in
    *://*)
      scheme=${candidate%%://*}
      case "$scheme" in "" | [!A-Za-z]* | *[!A-Za-z0-9+.-]*) return 0 ;; esac
      authority_and_path=${candidate#*://}
      authority=${authority_and_path%%/*}
      case "$authority_and_path" in */*) [ -n "$authority" ] || return 0; path=${authority_and_path#*/} ;; *) return 0 ;; esac
      ;;
    *:*/*)
      scp_host=${candidate%%:*}
      case "$scp_host" in "" | */*) return 0 ;; esac
      path=${candidate#*:}
      ;;
    *) path=$candidate ;;
  esac
  case "$path" in *\?* | *\#*) return 0 ;; esac
  path=${path%.git}
  case "$path" in */*/* | /* | */) return 0 ;; esac
  case "$path" in */*) owner=${path%%/*}; name=${path#*/} ;; *) return 0 ;; esac
  case "$owner" in "" | . | .. | *[!A-Za-z0-9._-]*) return 0 ;; esac
  case "$name" in "" | . | .. | *[!A-Za-z0-9._-]*) return 0 ;; esac
  printf '%s/%s\n' "$owner" "$name"
}

# with_repo adds `repo: owner/name`, projected from what the harness recorded or from the
# clone's own configuration — never guessed from the path. No safe identifier, no repo field.
with_repo() {
  while IFS= read -r record; do
    printf '%s\n' "$record" > "$payload"
    candidate=$(jq -r '.repo // empty' < "$payload")
    repo=$(repo_name "$candidate")
    if [ -z "$repo" ]; then
      cwd=$(jq -r '.cwd // empty' < "$payload")
      remote=$(jq -r '.remote // empty' < "$payload")
      [ -n "$remote" ] || remote=$(origin_of "$cwd")
      repo=$(repo_name "$remote")
    fi
    printf '%s\n' "$record" > "$payload"
    jq -c --arg repo "$repo" '
      .id = (.agent + ":" + (.id | tostring))
      | del(.remote) | del(.repo)
      + (if $repo == "" then {} else {repo: $repo} end)' < "$payload"
  done
}

{
  # Claude Code: one pass per transcript, keeping only the first turn in the window and the
  # last title line. Subagent transcripts sit one level deeper and are not listed.
  modified_since "$HOME/.claude/projects" '*.jsonl' 2 > "$paths"
  while IFS= read -r file; do
    jq -cn --arg start "$start" --arg end "$end" "$rfc3339"'
      ($start | instant) as $since
      | ($end | instant) as $until
      |
      reduce inputs as $line ({};
        (($line.timestamp // "") | instant) as $at
        | if ($line.type == "user" or $line.type == "assistant")
             and $at != null and $at >= $since and $at < $until
             and (.first_instant == null or $at < .first_instant)
        then .first = $line | .first_instant = $at
        elif $line.type == "ai-title" then .title = $line.aiTitle
        else . end)
      | select(.first != null)
      | { id: (.first.sessionId // input_filename), time: .first.timestamp, agent: "claude",
          title: (.title // ("claude in " + ((.first.cwd // "?") | split("/") | last))),
          cwd: .first.cwd, branch: (.first.gitBranch // null) }' "$file"
  done < "$paths"

  # Codex CLI: session_meta identifies the main session; top-level event timestamps prove
  # activity in this exact day. Payload fields are never projected or written.
  modified_since "$HOME/.codex/sessions" 'rollout-*.jsonl' 4 > "$paths"
  while IFS= read -r file; do
    jq -cn --arg start "$start" --arg end "$end" "$rfc3339"'
      ($start | instant) as $since
      | ($end | instant) as $until
      |
      reduce inputs as $line ({meta: null, first: null};
        (if .meta == null and $line.type == "session_meta" then .meta = $line.payload else . end)
        | (($line.timestamp
            // (if $line.type == "session_meta" then $line.payload.timestamp else null end)
            // "") as $at
           | ($at | instant) as $at_instant
           | if $at_instant != null and $at_instant >= $since and $at_instant < $until
                and (.first_instant == null or $at_instant < .first_instant)
             then .first = $at | .first_instant = $at_instant else . end))
      | .meta as $meta
      | select($meta != null and $meta.parent_thread_id == null and .first != null)
      | { id: $meta.id, time: .first, agent: "codex",
          title: ("codex in " + (($meta.cwd // "?") | split("/") | last)),
          cwd: $meta.cwd, branch: ($meta.git.branch // null), remote: ($meta.git.repository_url // null) }' \
      "$file"
  done < "$paths"

  # Gemini CLI: older JSON documents carry messages; newer JSONL files carry one header then
  # timestamped events. startTime counts only on its own day; otherwise a message/event must
  # prove activity. The .project_root marker beside chats supplies the working directory.
  modified_since "$HOME/.gemini/tmp" 'session-*.json*' 3 > "$paths"
  while IFS= read -r file; do
    root=$(cat "$(dirname "$(dirname "$file")")/.project_root" 2>/dev/null || true)
    case "$file" in
      *.jsonl)
        jq -cn --arg start "$start" --arg end "$end" --arg root "$root" "$rfc3339"'
          ($start | instant) as $since
          | ($end | instant) as $until
          |
          reduce inputs as $line ({header: null, first: null};
            (if .header == null and $line.sessionId != null then .header = $line else . end)
            | (($line.timestamp
                // (if $line.sessionId != null then $line.startTime else null end)
                // "") as $at
               | ($at | instant) as $at_instant
               | if $at_instant != null and $at_instant >= $since and $at_instant < $until
                    and (.first_instant == null or $at_instant < .first_instant)
                 then .first = $at | .first_instant = $at_instant else . end))
          | .header as $header
          | select($header != null and ($header.kind // "main") == "main" and .first != null)
          | { id: $header.sessionId, time: .first, agent: "gemini",
              title: ($header.summary // ("gemini in " + (($root | split("/") | last) // "?"))),
              cwd: (if $root == "" then null else $root end), branch: null }' "$file"
        ;;
      *)
        jq -c --arg start "$start" --arg end "$end" --arg root "$root" "$rfc3339"'
          ($start | instant) as $since
          | ($end | instant) as $until
          |
          ([.startTime, (.messages[]?.timestamp)]
           | map(select(type == "string")
                 | . as $text
                 | (instant) as $at
                 | select($at != null and $at >= $since and $at < $until)
                 | {text: $text, instant: $at})
           | min_by(.instant)) as $first
          | select(.sessionId != null and (.kind // "main") == "main" and $first != null)
          | { id: .sessionId, time: $first.text, agent: "gemini",
              title: (.summary // ("gemini in " + (($root | split("/") | last) // "?"))),
              cwd: (if $root == "" then null else $root end), branch: null }' "$file"
        ;;
    esac
  done < "$paths"

  # OpenCode: session creation and message timestamps are the activity evidence, in epoch
  # milliseconds. time_updated alone is only an envelope and cannot place an idle middle day.
  db=$HOME/.local/share/opencode/opencode.db
  if [ -f "$db" ]; then
    since=$(jq -rn --arg t "$start" '$t | fromdate * 1000')
    until=$(jq -rn --arg t "$end" '$t | fromdate * 1000')
    sql "$db" "with activity(session_id, activity_time) as (
                 select id, time_created from session
                 union all select session_id, time_created from message
               )
               select s.id, s.title, s.directory, min(a.activity_time) as activity_time
               from session s join activity a on a.session_id = s.id
               where s.parent_id is null and a.activity_time >= $since and a.activity_time < $until
               group by s.id, s.title, s.directory" > "$payload"
    jq -c '.[]
        | { id, time: (.activity_time / 1000 | floor | todate),
            agent: "opencode", title: (.title // ("opencode in " + ((.directory // "?") | split("/") | last))),
            cwd: .directory, branch: null }' < "$payload"
  fi

  # Copilot CLI: session creation and turn timestamps prove activity; datetime() normalises
  # their timestamp spellings before the exact half-open comparison.
  db=$HOME/.copilot/session-store.db
  if [ -f "$db" ]; then
    sql "$db" "with activity(session_id, activity_time) as (
                 select id, created_at from sessions
                 union all select session_id, timestamp from turns
               )
               select s.id, min(datetime(a.activity_time)) as activity, s.summary, s.cwd, s.branch, s.repository
               from sessions s join activity a on a.session_id = s.id
               where datetime(a.activity_time) >= datetime('$start')
                 and datetime(a.activity_time) < datetime('$end')
               group by s.id, s.summary, s.cwd, s.branch, s.repository" > "$payload"
    jq -c '.[]
        | { id, time: (.activity | sub(" "; "T") + "Z"),
            agent: "copilot", title: ((.summary | select(. != "")) // ("copilot in " + ((.cwd // "?") | split("/") | last))),
            cwd: .cwd, branch: (.branch | select(. != "")), repo: (.repository | select(. != "")) }' < "$payload"
  fi

  # Antigravity CLI: history.jsonl, one line per interaction, grouped into a session per
  # conversation. The older conversation_summaries.db may stop updating without an error;
  # because an empty result writes an empty-but-complete day, that gap would be invisible.
  # `display` holds the user's prompt text and is deliberately never projected: this source is
  # metadata, and the prompt bodies are a separate opt-in source.
  history=$HOME/.gemini/antigravity-cli/history.jsonl
  if [ -f "$history" ]; then
    jq -c --arg start "$start" --arg end "$end" -s '
      map(select(.conversationId != null and .timestamp != null)
          | { id: .conversationId,
              time: (.timestamp / 1000 | todateiso8601),
              cwd: (.workspace // null) })
      | map(select(.time >= $start and .time < $end))
      | group_by(.id)
      | map({ id: .[0].id, time: (map(.time) | min), agent: "antigravity",
              cwd: (map(.cwd) | map(select(. != null)) | first),
              branch: null })
      | map(. + { title: ("antigravity in " + ((.cwd // "") | split("/") | last | select(. != "") // "a workspace")) })
      | .[]' < "$history"
  fi
} > "$raw"

with_repo < "$raw" > "$out"
jq -s 'map(. + {
  repository_uri: (if (.repo // "") == "" then null else ("repo:github.com/" + .repo) end)
})' < "$out"
