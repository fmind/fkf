#!/bin/sh
# agent-memory-body.sh <absolute-memory-file> — print one reviewed harness memory file.
set -eu

case "${1:-}" in
  --version | -v) echo "agent-memory-body.sh (fkf base helper)"; exit 0 ;;
esac
[ "$#" -eq 1 ] || { echo "usage: agent-memory-body.sh <absolute-memory-file>" >&2; exit 2; }
path=$1
case "$path" in
  "$HOME/.claude/projects/"*/memory/*.md) root=$HOME/.claude/projects ;;
  "$HOME/.codex/memories/"*.md | "$HOME/.codex/memories/"*/*.md) root=$HOME/.codex/memories ;;
  "$HOME/.gemini/tmp/"*/memory/*.md) root=$HOME/.gemini/tmp ;;
  "$HOME/.grok/memory/"*.md | "$HOME/.grok/memory/"*/*.md) root=$HOME/.grok/memory ;;
  *) echo "agent-memory-body.sh: path is outside the reviewed harness memory roots" >&2; exit 2 ;;
esac

[ ! -L "$root" ] || { echo "agent-memory-body.sh: memory root may not be a symlink" >&2; exit 1; }
[ -d "$root" ] || { echo "agent-memory-body.sh: memory root does not exist" >&2; exit 1; }
listing=$(mktemp)
trap 'rm -f "$listing"' EXIT
find "$root" -type f -name '*.md' > "$listing"
found=false
while IFS= read -r candidate; do
  if [ "$candidate" = "$path" ]; then
    found=true
    break
  fi
done < "$listing"
[ "$found" = true ] || { echo "agent-memory-body.sh: file is absent, linked, or outside the reviewed roots" >&2; exit 1; }

bytes=$(wc -c < "$path")
[ "$bytes" -le 4194304 ] || { echo "agent-memory-body.sh: file exceeds the 4194304-byte body limit" >&2; exit 1; }
cat "$path"
