#!/bin/sh
# gws-chat-messages.sh <start> <end> — metadata for every message in every visible Chat space.
#
# Message text, cards, annotations, and attachments are deliberately excluded. The companion
# gws-chat-message-body.sh helper retrieves text only when a trusted human explicitly asks for it.
set -eu

case "${1:-}" in
  --version | -v) echo "gws-chat-messages.sh (fkf base helper)"; exit 0 ;;
  *) ;;
esac

[ "$#" -eq 2 ] || {
  echo "usage: gws-chat-messages.sh <start> <end>" >&2
  exit 2
}

start=$1
end=$2

if ! start_epoch=$(jq -ner --arg timestamp "${start}" '$timestamp | fromdateiso8601'); then
  echo "gws-chat-messages.sh: start must be a UTC RFC3339 timestamp" >&2
  exit 2
fi
if ! end_epoch=$(jq -ner --arg timestamp "${end}" '$timestamp | fromdateiso8601'); then
  echo "gws-chat-messages.sh: end must be a UTC RFC3339 timestamp" >&2
  exit 2
fi
if [ "${start_epoch}" -ge "${end_epoch}" ]; then
  echo "gws-chat-messages.sh: start must be before end" >&2
  exit 2
fi
query_start=$(jq -nr --argjson epoch "$((start_epoch - 1))" '$epoch | todateiso8601')
filter="createTime > \"${query_start}\" AND createTime < \"${end}\""

work_dir=$(mktemp -d "${TMPDIR:-/tmp}/fkf-chat-messages.XXXXXX")
spaces_raw=${work_dir}/spaces.json
spaces=${work_dir}/spaces
page=${work_dir}/page.json
records=${work_dir}/records.ndjson
: > "${records}"
trap 'exit 1' HUP INT TERM
trap 'rm -rf "${work_dir}"' 0

if ! gws-page-json.sh spaces gws chat spaces list --params '{"pageSize":1000}' \
  --page-all --page-limit 100 > "${spaces_raw}"; then
  echo "gws-chat-messages.sh: cannot list Chat spaces" >&2
  exit 1
fi
if ! jq -se 'all(.[]; type == "object" and ((.spaces // []) | type == "array"))' \
  "${spaces_raw}" >/dev/null; then
  echo "gws-chat-messages.sh: spaces listing returned invalid JSON pages" >&2
  exit 1
fi
jq -sr '[.[].spaces[]?.name] | unique[]' "${spaces_raw}" > "${spaces}"

while IFS= read -r space; do
  [ -n "${space}" ] || continue
  params=$(jq -nc --arg parent "${space}" --arg filter "${filter}" \
    '{parent: $parent, pageSize: 1000, filter: $filter, showDeleted: true}')
  if ! gws-page-json.sh messages gws chat spaces messages list --params "${params}" \
    --page-all --page-limit 100 > "${page}"; then
    echo "gws-chat-messages.sh: cannot list messages in ${space}" >&2
    exit 1
  fi
  if ! jq -se 'all(.[]; type == "object" and ((.messages // []) | type == "array"))' \
    "${page}" >/dev/null; then
    echo "gws-chat-messages.sh: ${space} returned invalid message pages" >&2
    exit 1
  fi
  jq -sc --arg space "${space}" --arg start "${start}" --arg end "${end}" '
    .[] | .messages[]?
    | select(.createTime >= $start and .createTime < $end)
    | {
        name,
        createTime,
        lastUpdateTime: (.lastUpdateTime // null),
        deleteTime: (.deleteTime // null),
        sender: {
          name: (.sender.name // null),
          type: (.sender.type // null)
        },
        space: (.space.name // $space),
        thread: (.thread.name // null),
        deleted: has("deleteTime"),
        attachmentCount: ((.attachment // []) | length),
        annotationCount: ((.annotations // []) | length),
        cardCount: ((.cardsV2 // []) | length),
        title: ((.sender.name // "unknown sender") + " in " + (.space.name // $space))
      }
  ' "${page}" >> "${records}"
done < "${spaces}"

jq -s 'sort_by(.name) | unique_by(.name)' "${records}"
