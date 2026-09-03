#!/bin/sh
# agent-prompt-body.sh <id> — fetch one durable request body on explicit demand.
#
# Prints the full text of one request collected by agent-prompts.sh. This is the `body:` command
# of the agent-prompts source: fkf runs it as argv with no shell, prints what it returns, and
# stores nothing. That split is the point — events/ holds a bounded preview, and the words you
# actually wrote stay in ~/.agents/sessions/v1 until something asks for one of them.
#
# The id is <agent>-<sid>-<compact ts>, exactly as agent-prompts.sh built it. Neither half is a
# path, so this script searches rather than opens: the compact timestamp is expanded back into
# the ISO form the transcripts store, grep narrows the store to the files that mention it, and
# jq confirms the session id before printing. A prompt is therefore still reachable after its
# lineage has been re-ingested under a new snapshot directory.
set -eu

case "${1:-}" in
  --version | -v) echo "agent-prompt-body.sh (fkf preset helper)"; exit 0 ;;
  *) ;;
esac

id=${1:?usage: agent-prompt-body.sh <agent>-<sid>-<compact-ts>}
command -v jq > /dev/null 2>&1 || { echo "agent-prompt-body.sh: jq is required" >&2; exit 1; }

case "$0" in
  */*) script_path=$0 ;;
  *) script_path=$(command -v "$0") ;;
esac
filter_dir=$(CDPATH='' cd -- "$(dirname -- "${script_path}")" && pwd -P)
[ -r "${filter_dir}/agent-prompt-filter.jq" ] || {
  echo "agent-prompt-body.sh: shared prompt filter is missing" >&2
  exit 1
}

# A session id may itself contain hyphens — a UUID does, and Claude Code's subagent ids do —
# so the id is split from BOTH ends rather than left to right: the harness name is the first
# component and the compact timestamp is the last, because it is the one field that never
# contains a hyphen. Everything between them is the session id.
agent=${id%%-*}
stamp=${id##*-}
sid=${id#"${agent}"-}
sid=${sid%-"${stamp}"}
[ -n "${agent}" ] && [ -n "${sid}" ] || {
  echo "agent-prompt-body.sh: '${id}' does not contain both a harness and a session id" >&2
  exit 2
}

# 20260822T000200Z -> 2026-08-22T00:02:00Z, while 20260822T000200507Z preserves
# millisecond precision. agent-prompts.sh removes punctuation but does not invent a fraction, so
# the body resolver accepts exactly those two forms and reconstructs the transcript's clock.
timestamp_error() {
  echo "agent-prompt-body.sh: '${stamp}' is not a compact timestamp; expected YYYYMMDDTHHMMSSZ or YYYYMMDDTHHMMSSmmmZ" >&2
  exit 2
}

case "${stamp}" in
  ????????T??????Z) precision=seconds ;;
  ????????T?????????Z) precision=milliseconds ;;
  *) timestamp_error ;;
esac
date_part=${stamp%T*}
clock=${stamp#*T}
clock=${clock%Z}
case "${date_part}${clock}" in
  *[!0-9]*) timestamp_error ;;
  *) ;;
esac
year=$(printf '%s' "${date_part}" | cut -c1-4)
month=$(printf '%s' "${date_part}" | cut -c5-6)
day=$(printf '%s' "${date_part}" | cut -c7-8)
hour=$(printf '%s' "${clock}" | cut -c1-2)
minute=$(printf '%s' "${clock}" | cut -c3-4)
second=$(printf '%s' "${clock}" | cut -c5-6)
case "${precision}" in
  seconds) fraction= ;;
  milliseconds)
    millisecond=$(printf '%s' "${clock}" | cut -c7-9)
    fraction=.${millisecond}
    ;;
  *) timestamp_error ;;
esac
iso="${year}-${month}-${day}T${hour}:${minute}:${second}${fraction}Z"

store=${HOME}/.agents/sessions/v1/${agent}
[ -d "${store}" ] || { echo "agent-prompt-body.sh: no transcripts for harness '${agent}'" >&2; exit 1; }

# Resolve before printing so a duplicate snapshot, a malformed transcript, or an oversized
# body can never produce a plausible partial answer. The 64 MiB limit is fkf's subprocess
# stdout bound; `jq -j` adds no trailing byte that would push an exactly bounded body over it.
candidates=$(mktemp)
matches=$(mktemp)
unique_matches=$(mktemp)
body=$(mktemp)
trap 'rm -f "${candidates}" "${matches}" "${unique_matches}" "${body}"' EXIT

# -l stops at the first timestamp hit per file, which matters for the append-only store. A
# grep read error is different from "no candidate" and must fail rather than look like absence.
if grep -rl --include=transcript.jsonl -F "${iso}" "${store}" > "${candidates}"; then
  :
else
  status=$?
  if [ "${status}" -ne 1 ]; then
    echo "agent-prompt-body.sh: failed to search transcripts for ${agent}" >&2
    exit "${status}"
  fi
fi

: > "${matches}"
while IFS= read -r file; do
  [ -n "${file}" ] || continue
  if jq -L "${filter_dir}" -c --arg iso "${iso}" --arg sid "${sid}" '
    include "agent-prompt-filter";
    select(.role == "user")
    | select((.ts // "") == $iso)
    | select((.sid // "") == $sid)
    | if (.content | type) == "string"
      then {content: (.content | normalize_agent_prompt)}
      else error("matching user turn has non-string content")
      end
  ' < "${file}" >> "${matches}"; then
    :
  else
    status=$?
    echo "agent-prompt-body.sh: jq could not read ${file}" >&2
    exit "${status}"
  fi
done < "${candidates}"

# A lineage snapshot repeats earlier turns. Collapse byte-identical copies of the same exact
# tuple, but reject conflicting bodies rather than selecting whichever file grep found first.
if jq -s -c 'unique_by(.content)' < "${matches}" > "${unique_matches}"; then
  :
else
  status=$?
  echo "agent-prompt-body.sh: jq could not reconcile matching snapshot copies" >&2
  exit "${status}"
fi
count=$(jq 'length' < "${unique_matches}")
case "${count}" in
  1) ;;
  0)
    echo "agent-prompt-body.sh: no turn for harness '${agent}', session '${sid}', timestamp '${iso}'" >&2
    exit 1
    ;;
  *)
    echo "agent-prompt-body.sh: ${count} distinct bodies match harness '${agent}', session '${sid}', timestamp '${iso}'; refusing an ambiguous body" >&2
    exit 1
    ;;
esac

if jq -j '.[0].content' < "${unique_matches}" > "${body}"; then
  :
else
  status=$?
  echo "agent-prompt-body.sh: jq could not decode the matched body" >&2
  exit "${status}"
fi

max_body_bytes=67108864
body_bytes=$(wc -c < "${body}" | tr -d ' ')
if [ "${body_bytes}" -gt "${max_body_bytes}" ]; then
  echo "agent-prompt-body.sh: body is ${body_bytes} bytes; fkf read allows at most ${max_body_bytes}" >&2
  exit 1
fi
cat "${body}"
