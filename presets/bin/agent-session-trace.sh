#!/bin/sh
# agent-session-trace.sh <start> <end> -- emit completed normalized sessions as bounded JSON.
#
# The only input is ~/.agents/sessions/v1, the harness-independent append-only store shared by
# the supported agent integrations. The helper does not know a harness's native transcript
# format and makes no model call. It selects the newest complete generation of each session
# whose last recorded turn falls in the exact half-open window, then projects enough evidence
# for fkf to write one deterministic TASKS.md skeleton.
#
# Requests and the last assistant message are bounded excerpts. Git contributes only its
# porcelain status paths at collection time; no file content, diff, credential, or environment
# value is read. Commands are merely exact command-looking lines seen in the last assistant
# message, and the generated trace labels them that way rather than claiming they ran.
set -eu

case "${1:-}" in --version | -v) echo "agent-session-trace.sh (fkf preset helper)"; exit 0 ;; esac

start=${1:?usage: agent-session-trace.sh <start> <end>}
end=${2:?usage: agent-session-trace.sh <start> <end>}
command -v jq >/dev/null 2>&1 || { echo "agent-session-trace.sh: jq is required" >&2; exit 1; }
command -v find >/dev/null 2>&1 || { echo "agent-session-trace.sh: find is required" >&2; exit 1; }
command -v git >/dev/null 2>&1 || { echo "agent-session-trace.sh: git is required" >&2; exit 1; }

case "${HOME:-}" in /*) ;; *) echo "agent-session-trace.sh: HOME must be absolute" >&2; exit 1 ;; esac
store=$HOME/.agents/sessions/v1
[ -d "$store" ] || { printf '[]\n'; exit 0; }
[ ! -L "$store" ] || { echo "agent-session-trace.sh: session store must not be a symlink" >&2; exit 1; }

case "$0" in
  */*) script_path=$0 ;;
  *) script_path=$(command -v "$0") ;;
esac
filter_dir=$(CDPATH='' cd -- "$(dirname -- "$script_path")" && pwd -P)
[ -r "$filter_dir/agent-prompt-filter.jq" ] || {
  echo "agent-session-trace.sh: shared prompt filter is missing" >&2
  exit 1
}

manifests=$(mktemp)
candidates=$(mktemp)
latest=$(mktemp)
traces=$(mktemp)
session=$(mktemp)
files=$(mktemp)
links=$(mktemp)
repo_cache=$(mktemp -d)
trap 'rm -f "$manifests" "$candidates" "$latest" "$traces" "$session" "$files" "$links"; rm -rf "$repo_cache"' EXIT

# The store owns hash-only path segments, but still refuse links before opening any manifest.
# `find` does not follow links by default; this explicit audit makes a linked generation an
# error instead of a silently missing session.
find "$store" -type l -print >"$links"
[ ! -s "$links" ] || { echo "agent-session-trace.sh: session store contains a symlink" >&2; exit 1; }
find "$store" -type f -name manifest.json -size +65536c -print >"$links"
[ ! -s "$links" ] || { echo "agent-session-trace.sh: oversized session manifest" >&2; exit 1; }
find "$store" -type f -name manifest.json -print0 >"$manifests"

manifest_count=$(tr -cd '\000' <"$manifests" | wc -c | tr -d ' ')
[ "$manifest_count" -le 8192 ] || {
  echo "agent-session-trace.sh: session store has more than 8192 generations" >&2
  exit 1
}

# Fractional and offset RFC3339 values must compare as instants, not strings.
rfc3339='
def instant:
  try (
    capture("^(?<whole>[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2})(?<fraction>\\.[0-9]+)?(?<zone>Z|[+-][0-9]{2}:[0-9]{2})$") as $part
    | (($part.whole + "Z" | fromdateiso8601)
       + (("0" + ($part.fraction // "")) | tonumber)
       - (if $part.zone == "Z" then 0
          else (((($part.zone[1:3] | tonumber) * 60) + ($part.zone[4:6] | tonumber)) * 60)
               * (if $part.zone[0:1] == "+" then 1 else -1 end)
          end))
  ) catch null;
'

# repo_name accepts only an exact GitHub owner/name from the clone's configured origin.
# Userinfo, other hosts, queries, fragments, and extra path segments stay out of the trace.
repo_name() {
  candidate=$1
  case "$candidate" in
    https://github.com/* | http://github.com/* | ssh://github.com/* | ssh://*@github.com/*)
      path=${candidate#*://}
      path=${path#*/}
      ;;
    git@github.com:*) path=${candidate#*:} ;;
    *) return 0 ;;
  esac
  case "$path" in *\?* | *\#* | /* | */ | */*/*) return 0 ;; esac
  path=${path%.git}
  case "$path" in */*) owner=${path%%/*}; name=${path#*/} ;; *) return 0 ;; esac
  case "$owner" in "" | . | .. | *[!A-Za-z0-9._-]*) return 0 ;; esac
  case "$name" in "" | . | .. | *[!A-Za-z0-9._-]*) return 0 ;; esac
  printf '%s/%s\n' "$owner" "$name"
}

: >"$candidates"
if [ -s "$manifests" ]; then
  # Batch size checks and JSON parsing: a large append-only store normally has thousands of
  # generations, and one process per manifest made the source slower than the source timeout.
  xargs -0 sh -c '
    root=$1
    shift
    for manifest do
      relative=${manifest#"$root"/}
      case "$relative" in */*/*/manifest.json) ;; *) continue ;; esac
      remainder=${relative%/manifest.json}
      case "${remainder#*/*/}" in */*) continue ;; esac
      printf "%s\0" "$manifest"
    done
  ' sh "$store" <"$manifests" >"$links"

  xargs -0 jq -c "$rfc3339"'
    select(.schema_version == 1 and .parser_version == "1" and .completeness == "complete")
    | select((.agent | type) == "string" and (.session_id | type) == "string"
             and (.ingested_at | type) == "string" and (.record_count | type) == "number")
    | ((.ingested_at // "") | instant) as $ingested
    | ((.high_water_mark // "") | instant) as $last
    | select($ingested != null and $last != null)
    | {agent, session_id, ingested_at, high_water_mark, record_count,
       ingested: $ingested, last: $last,
       path: (input_filename | sub("/manifest\\.json$"; ""))}
  ' <"$links" >"$candidates"
fi

jq -sc --arg start "$start" --arg end "$end" "$rfc3339"'
  ($start | instant) as $since | ($end | instant) as $until
  | sort_by(.agent, .session_id, .ingested, .path)
  | group_by(.agent + "\u0000" + .session_id)
  | map(last | select(.last >= $since and .last < $until) | del(.ingested, .last))
' "$candidates" >"$latest"
selected=$(jq 'length' <"$latest")
[ "$selected" -le 1024 ] || {
  # The caller separately caps captured stdout at 64 MiB; this count keeps a malformed store
  # bounded while admitting hundreds of sessions from a busy multi-harness day.
  echo "agent-session-trace.sh: more than 1024 completed sessions fall in the requested window" >&2
  exit 1
}

: >"$traces"
jq -c '.[]' <"$latest" | while IFS= read -r candidate; do
  generation=$(printf '%s\n' "$candidate" | jq -r '.path')
  transcript=$generation/transcript.jsonl
  [ -f "$transcript" ] && [ ! -L "$transcript" ] || {
    echo "agent-session-trace.sh: session transcript is missing or linked" >&2
    exit 1
  }
  transcript_bytes=$(wc -c <"$transcript" | tr -d ' ')
  [ "$transcript_bytes" -le 8388608 ] || {
    # Completed harness sessions commonly cross 4 MiB; keep a finite per-session ceiling while
    # the source count, command timeout, and FKF's stdout limit bound aggregate work.
    echo "agent-session-trace.sh: session transcript exceeds 8 MiB" >&2
    exit 1
  }

  agent=$(printf '%s\n' "$candidate" | jq -r '.agent')
  sid=$(printf '%s\n' "$candidate" | jq -r '.session_id')
  high_water=$(printf '%s\n' "$candidate" | jq -r '.high_water_mark')
  jq -L "$filter_dir" -c --arg agent "$agent" --arg sid "$sid" --arg completed "$high_water" '
    include "agent-prompt-filter";

    def utf8_prefix($maximum):
      . as $value
      | def search($low; $high):
          if $low >= $high then $low
          else (($low + $high + 1) / 2 | floor) as $middle
          | if ($value[0:$middle] | utf8bytelength) <= $maximum
            then search($middle; $high)
            else search($low; $middle - 1)
            end
          end;
      # FKF validates stored-field bytes, while jq slices strings by Unicode code point.
      $value[0:search(0; $value | length)];
    def safe_layout:
      gsub("\\p{Cf}"; "")
      | gsub("[\u0000-\u0008\u000b\u000c\u000e-\u001f\u007f]"; "")
      | utf8_prefix(6000);
    def command_lines:
      split("\n")
      | map(
          sub("^[[:space:]]*[-*][[:space:]]*"; "")
          | sub("^`"; "") | sub("`[.]*$"; "")
          | select(length <= 240)
          | select(test("^(mise run|go test|go vet|go build|golangci-lint|git diff|git status|shellcheck|dprint|npm test|pnpm test|pytest|uv run pytest)([[:space:]]|$)"))
        )
      | unique | .[0:20];

    # jq binds the first JSONL record to `.`, so reduce both it and `inputs`; reducing only
    # inputs would silently drop the first request in a session.
    reduce (., inputs) as $record (
      {first_at: null, last_at: null, cwd: null, model: null, requests: [], last_assistant: null};
      if $record.agent != $agent or $record.sid != $sid then .
      else
        .first_at = (if .first_at == null then $record.ts else .first_at end)
        | .last_at = ($record.ts // .last_at)
        | .cwd = ($record.cwd // .cwd)
        | .model = ($record.model // .model)
        | if $record.role == "user" and (.requests | length) < 20 then
          ([($record.content // "" | tostring | normalize_agent_prompt?)] | first // "" | safe_layout) as $request
            | if $request == "" then . else .requests += [$request] end
          elif $record.role == "assistant" then
            .last_assistant = (($record.content // "") | tostring | safe_layout)
          else . end
      end
    )
    | select(.first_at != null and (.requests | length) > 0)
    | .last_at = $completed
    | .verification = ((.last_assistant // "") | command_lines)
    | . + {id: ($agent + ":" + $sid), harness: $agent, sid: $sid}
  ' "$transcript" >"$session"

  [ -s "$session" ] || continue
  cwd=$(jq -r '.cwd // empty' <"$session")
  case "$cwd" in
    /*)
      case "$cwd" in *"$(printf '\nX')"*) cwd= ;; esac
      ;;
    *) cwd= ;;
  esac
  : >"$files"
  repo=
  if [ -n "$cwd" ] && [ -d "$cwd" ] && git -c core.fsmonitor=false --no-optional-locks -C "$cwd" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
    cache_key=$(printf '%s' "$cwd" | git hash-object --stdin)
    cached_files=$repo_cache/$cache_key.files
    cached_repo=$repo_cache/$cache_key.repo
    if [ ! -f "$cached_files" ]; then
      # Every historical trace observes the same collection-time worktree. Cache by cwd so a
      # busy harness day does not run the identical git status hundreds of times.
      GIT_OPTIONAL_LOCKS=0 git -c core.fsmonitor=false --no-optional-locks -C "$cwd" -c core.quotePath=true \
        status --short --untracked-files=normal --ignore-submodules=all >"$cached_files"
      file_bytes=$(wc -c <"$cached_files" | tr -d ' ')
      [ "$file_bytes" -le 1048576 ] || {
        echo "agent-session-trace.sh: git status exceeds 1 MiB" >&2
        exit 1
      }
      git -c core.fsmonitor=false --no-optional-locks -C "$cwd" config --get remote.origin.url \
        >"$cached_repo" 2>/dev/null || : >"$cached_repo"
    fi
    cp "$cached_files" "$files"
    repo=$(sed -n '1p' "$cached_repo")

    first_at=$(jq -r '.first_at' <"$session")
    last_at=$(jq -r '.last_at' <"$session")
    # A session may commit before its trace is collected, leaving a clean worktree. Git history
    # recovers path metadata inside the session window without reading any file contents.
    if git -c core.fsmonitor=false --no-optional-locks -C "$cwd" rev-parse --verify HEAD >/dev/null 2>&1; then
      git -c core.fsmonitor=false --no-optional-locks -C "$cwd" -c core.quotePath=true \
        log --since="$first_at" --until="$last_at" --format= --name-only --no-renames -- >>"$files"
    fi
    file_bytes=$(wc -c <"$files" | tr -d ' ')
    [ "$file_bytes" -le 1048576 ] || {
      echo "agent-session-trace.sh: combined git path evidence exceeds 1 MiB" >&2
      exit 1
    }
  fi

  # Keep the remote only when it reduces to an owner/name identifier. The service treats it as
  # display metadata; it never becomes a command or path.
  repo=$(repo_name "$repo")
  jq -c --arg repo "$repo" --rawfile files "$files" '
    . + (if $repo == "" then {} else {repo: $repo} end)
      + {files: ($files | split("\n") | map(select(. != "")) | unique | .[0:200])}
  ' "$session" >>"$traces"
done

jq -sc 'sort_by(.last_at, .harness, .sid)' "$traces"
