#!/bin/sh
# fkf-hook.sh <harness> — written into <base>/bin by `fkf init`. The session-start hook for the base
# this script lives in: it reads the hook's stdin when the harness sends one, finds the repository
# and branch the agent is working in, asks `fkf context` for a budgeted pack about them, and
# prints it in the envelope the harness expects. One script, one line per harness:
#
#   claude       plain text on stdout              Claude Code    SessionStart, UserPromptSubmit
#   codex        plain text on stdout              Codex CLI      SessionStart, UserPromptSubmit
#   kiro         plain text on stdout              Kiro           SessionStart, UserPromptSubmit
#   copilot      {"additionalContext": …}          Copilot CLI    sessionStart
#   gemini       {"hookSpecificOutput": …}         Gemini CLI     SessionStart, BeforeAgent
#   devin        {"hookSpecificOutput": …}         Devin Local    SessionStart, UserPromptSubmit
#   cursor       {"additional_context": …}         Cursor         sessionStart
#   antigravity  {"injectSteps": …}                Antigravity    PreInvocation, first call only
#   cline        {"contextModification": …}        Cline          TaskStart
#
# The base is the directory above this script, so the line that names the script in a harness
# configuration is the disclosure: `~/brain/bin/fkf-hook.sh claude` says which base the session can
# see. The pack is evidence, never instructions; fkf reads no secret and executes nothing to
# produce it. The hook never blocks a session: with no repository, no base, or no fkf on PATH it
# prints an empty envelope and exits 0. The budget is a constant below; raise it for a harness
# that starts every session empty, lower it when the hook also fires on every prompt.
set -u

harness=${1:?usage: fkf-hook.sh <claude|codex|kiro|copilot|gemini|devin|cursor|antigravity|cline>}
budget=1500

# Resolve the base with shell builtins before any command lookup. The inherited PATH is
# untrusted: an agent launched inside a checkout commonly inherits an absolute .venv/bin or
# <checkout>/bin entry, and that checkout must not get to replace git, jq, or fkf for this hook.
script_dir=${0%/*}
[ "${script_dir}" != "$0" ] || script_dir=.
case ${script_dir} in -*) script_dir=./${script_dir} ;; *) ;; esac
base=$(
  unset CDPATH
  cd "${script_dir}/.." 2>/dev/null && pwd -P
) || exit 0

# A GUI-launched harness may not inherit a login shell's PATH. Use a closed list of conventional
# user install locations and OS-managed directories; never carry caller-provided entries across
# this trust boundary. Missing directories are harmless PATH entries, and a HOME containing a
# colon is omitted because POSIX PATH cannot represent it as one entry.
PATH=/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin:/run/current-system/sw/bin:/nix/var/nix/profiles/default/bin
case ${HOME-} in
  /*:*) ;; # A colon cannot be represented inside one PATH entry.
  /*) PATH=$HOME/.local/bin:$HOME/go/bin:$HOME/.local/share/mise/shims:$PATH ;;
esac
export PATH
unset script_dir

# Every JSON envelope needs jq; without it the promise to never block wins over the pack.
command -v jq >/dev/null 2>&1 || case "$harness" in claude | codex | kiro) ;; *) exit 0 ;; esac

# The hook's JSON arrives on stdin for every harness that sends one; a terminal means none.
input=""
if [ ! -t 0 ]; then input=$(cat 2>/dev/null || true); fi
field() { printf '%s' "$input" | jq -r "$1 // empty" 2>/dev/null || true; }

# Nothing to add is a valid answer, spelled the way each harness reads it.
empty() {
  case "$harness" in
    claude | codex | kiro) : ;;
    cline) echo '{"cancel":false}' ;;
    *) echo '{}' ;;
  esac
  exit 0
}

# Antigravity fires PreInvocation before every model call, so only the first one answers.
case "$harness" in
  antigravity) [ "$(field '.invocationNum // 0')" = "0" ] || empty ;;
esac

# Where the agent works: the harness's own field first (user-level hooks often run from the
# configuration directory, not the project), then the environment, then this process's cwd.
dir=$(field '.cwd // .workspace_roots[0] // .workspacePaths[0] // .workspaceRoots[0] // .workspaceInfo.rootPath')
[ -n "$dir" ] || dir=${CLAUDE_PROJECT_DIR:-${GEMINI_PROJECT_DIR:-${DEVIN_PROJECT_DIR:-$PWD}}}

# repo_name accepts only an exact owner/name from a plain identifier, a URL path, or an
# SCP-style remote. Authority userinfo and malformed paths never reach an agent's context query.
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

# owner/name scores as an exact identifier on every record about the repository; the branch is
# an ordinary term. An invalid or credential-only remote contributes no repository term.
remote=$(git -C "$dir" remote get-url origin 2>/dev/null || true)
repo=$(repo_name "$remote")
branch=$(git -C "$dir" branch --show-current 2>/dev/null || true)
query=$(printf '%s %s' "$repo" "$branch" | sed 's/^ *//; s/ *$//')
[ -n "$query" ] || empty
pack=$(fkf context --base "$base" --budget "$budget" --format text -- "$query" 2>/dev/null || true)
[ -n "$pack" ] || empty

case "$harness" in
  claude | codex | kiro) printf '%s\n' "$pack" ;;
  copilot) printf '%s' "$pack" | jq -Rs '{additionalContext: .}' ;;
  gemini | devin)
    event=$(field '.hook_event_name'); event=${event:-SessionStart}
    printf '%s' "$pack" | jq -Rs --arg event "$event" '{hookSpecificOutput: {hookEventName: $event, additionalContext: .}}'
    ;;
  cursor) printf '%s' "$pack" | jq -Rs '{additional_context: .}' ;;
  antigravity) printf '%s' "$pack" | jq -Rs '{injectSteps: [{ephemeralMessage: .}]}' ;;
  cline) printf '%s' "$pack" | jq -Rs '{cancel: false, contextModification: .}' ;;
  *) echo "fkf-hook.sh: unknown harness $harness" >&2; exit 1 ;;
esac
