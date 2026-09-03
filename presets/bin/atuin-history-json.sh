#!/bin/sh
# atuin-history-json.sh <start> <end> <database> — collect undeleted shell activity metadata
# without exposing command arguments. SQLite opens read-only, and SQLite and jq run as separate
# fail-fast stages.
set -eu

start=${1:?usage: atuin-history-json.sh <start> <end> <database>}
end=${2:?usage: atuin-history-json.sh <start> <end> <database>}
database=${3:?usage: atuin-history-json.sh <start> <end> <database>}
work_dir=$(mktemp -d "${TMPDIR:-/tmp}/fkf-atuin.XXXXXX")
rows=$work_dir/rows.ndjson
trap 'exit 1' HUP INT TERM
trap 'rm -rf "$work_dir"' 0

sqlite3 -init /dev/null -batch -readonly -json "$database" \
  "select id, cwd, exit, duration, command as _command, strftime('%Y-%m-%dT%H:%M:%SZ', timestamp/1000000000, 'unixepoch') as time from history where deleted_at is null and timestamp >= strftime('%s','$start')*1000000000 and timestamp < strftime('%s','$end')*1000000000 order by timestamp" \
  > "$rows"

# Raw commands exist only inside this mode-0700 temporary directory. Emit a tool and verb only
# when both come from reviewed allowlists; arbitrary arguments, assignments, URLs, and heredoc
# contents never cross the source boundary.
jq -s -c '
  def tools: ["atuin", "bash", "bun", "cargo", "claude", "codex", "curl", "d2", "docker",
    "dprint", "fd", "fkf", "gcloud", "gh", "git", "go", "gofmt", "grok", "gws", "helm",
    "jq", "kaggle", "kubectl", "mise", "npm", "npx", "opencode", "podman", "python",
    "python3", "rg", "ruff", "shellcheck", "terraform", "tofu", "uv", "xh", "yq"];
  def actions: ["add", "build", "check", "clone", "commit", "context", "deploy", "diff",
    "doctor", "find", "fmt", "format", "get", "graph", "init", "install", "list", "log",
    "pull", "push", "read", "release", "run", "search", "status", "sync", "test", "trust",
    "update", "upgrade", "validate", "view"];
  add // []
  | map(
      ((._command // "") | gsub("^\\s+|\\s+$"; "") | [splits("\\s+")]) as $parts
      | (($parts[0] // "") | split("/") | last) as $candidate_tool
      | (if tools | index($candidate_tool) then $candidate_tool else null end) as $tool
      | (if $tool != null and (actions | index($parts[1] // ""))
         then $parts[1] else null end) as $action
      | del(._command)
      | . + {
          subject: (if $tool == null then
                      ((if ((.cwd // "") | length) > 0
                        then "shell in " + (.cwd | split("/") | last)
                        else "shell activity" end) + " at " + .time)
                    elif $action == null then $tool
                    else ($tool + " " + $action) end)
        }
      + (if $tool == null then {} else {tool: $tool} end)
      + (if $action == null then {} else {action: $action} end))
' "$rows"
