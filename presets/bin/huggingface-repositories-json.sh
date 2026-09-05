#!/bin/sh
# huggingface-repositories-json.sh — bounded, projected metadata for every visible owned repo.
set -eu

case "${1:-}" in
  --version | -v) echo "huggingface-repositories-json.sh (fkf base helper)"; exit 0 ;;
  '') ;;
  *) echo "usage: huggingface-repositories-json.sh" >&2; exit 2 ;;
esac

work_dir=$(mktemp -d "${TMPDIR:-/tmp}/fkf-huggingface-repositories.XXXXXX")
trap 'rm -rf -- "${work_dir}"' EXIT
trap 'exit 1' HUP INT TERM
raw=${work_dir}/raw.json
projected=${work_dir}/projected.json

# Capture the native CLI completely before validating or printing anything. A provider failure
# after partial stdout must never become durable prefix evidence.
if ! hf repos ls --limit 10001 --format json >"${raw}"; then
  echo "huggingface-repositories-json.sh: cannot prove a complete repository inventory" >&2
  exit 1
fi

if ! jq -ce '
  def prefix:
    if . == "model" then ""
    elif . == "dataset" then "datasets/"
    elif . == "space" then "spaces/"
    elif . == "bucket" then "buckets/"
    else error("unknown repository type")
    end;
  if type != "array" or length > 10000 then
    error("repository ceiling reached")
  else
    map(
      if (.id | type != "string" or length == 0)
        or (.type | type != "string")
        or ((.updated? // "") | type != "string")
        or ((.visibility? // "") | type != "string")
      then error("invalid repository metadata")
      else {
        uid: (.type + ":" + .id),
        id,
        type,
        updated: (.updated // ""),
        visibility: (.visibility // ""),
        url: ("https://huggingface.co/" + (.type | prefix) + .id)
      }
      end
    )
    | if ([.[].uid] | length) != ([.[].uid] | unique | length)
      then error("duplicate repository identity")
      else sort_by(.id, .type)
      end
  end
' "${raw}" >"${projected}"; then
  echo "huggingface-repositories-json.sh: cannot prove a complete repository inventory" >&2
  exit 1
fi

cat "${projected}"
