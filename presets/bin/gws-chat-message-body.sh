#!/bin/sh
# gws-chat-message-body.sh <message-name> — fetch one Chat message body on explicit demand.
set -eu

case "${1:-}" in
  --version | -v) echo "gws-chat-message-body.sh (fkf base helper)"; exit 0 ;;
  *) ;;
esac

[ "$#" -eq 1 ] || {
  echo "usage: gws-chat-message-body.sh <message-name>" >&2
  exit 2
}

message=$1
if ! printf '%s\n' "${message}" | grep -Eq '^spaces/[A-Za-z0-9_-]+/messages/[A-Za-z0-9_.-]+$'; then
  echo "gws-chat-message-body.sh: invalid message resource name" >&2
  exit 2
fi

params=$(jq -nc --arg name "${message}" '{name: $name}')
raw=$(mktemp "${TMPDIR:-/tmp}/fkf-chat-body.XXXXXX")
trap 'rm -f "${raw}"' 0

if ! gws chat spaces messages get --params "${params}" > "${raw}"; then
  echo "gws-chat-message-body.sh: cannot fetch ${message}" >&2
  exit 1
fi
if ! jq -e --arg name "${message}" '
  type == "object" and .name == $name and ((.formattedText // .text // "") | type == "string")
' "${raw}" >/dev/null; then
  echo "gws-chat-message-body.sh: provider returned the wrong message or an invalid body" >&2
  exit 1
fi
jq -r '.formattedText // .text // ""' "${raw}"
