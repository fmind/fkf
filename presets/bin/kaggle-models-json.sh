#!/bin/sh
# kaggle-models-json.sh <owner> — page through every public model owned by one Kaggle account.
#
# kaggle-cli prints the next page token as a text line before its JSON array. This helper
# separates that protocol marker, validates every page, follows tokens until exhaustion, and
# fails on repeated tokens or an implausibly long chain rather than returning a prefix.
set -eu

case "${1:-}" in
  --version | -v) echo "kaggle-models-json.sh (fkf base helper)"; exit 0 ;;
  *) ;;
esac

[ "$#" -eq 1 ] || {
  echo "usage: kaggle-models-json.sh <owner>" >&2
  exit 2
}

owner=$1
case "${owner}" in
  *[!A-Za-z0-9_-]*) echo "kaggle-models-json.sh: invalid owner: ${owner}" >&2; exit 2 ;;
  *) ;;
esac

work_dir=$(mktemp -d "${TMPDIR:-/tmp}/fkf-kaggle-models.XXXXXX")
raw=${work_dir}/raw
page=${work_dir}/page
pages=${work_dir}/pages.ndjson
seen=${work_dir}/seen
: > "${pages}"
: > "${seen}"
trap 'exit 1' HUP INT TERM
trap 'rm -rf "${work_dir}"' 0

token=
page_number=0
while :; do
  page_number=$((page_number + 1))
  if [ "${page_number}" -gt 1000 ]; then
    echo "kaggle-models-json.sh: more than 1000 page tokens; refusing an unbounded provider loop" >&2
    exit 1
  fi

  if [ -n "${token}" ]; then
    kaggle models list --owner "${owner}" --page-size 200 --page-token "${token}" \
      --format json > "${raw}"
  else
    kaggle models list --owner "${owner}" --page-size 200 --format json > "${raw}"
  fi

  first=$(sed -n '1p' "${raw}")
  next_token=
  case "${first}" in
    'Next Page Token = '*)
      next_token=${first#Next Page Token = }
      sed '1d' "${raw}" > "${page}"
      ;;
    'No models found')
      printf '[]\n' > "${page}"
      ;;
    *)
      cp "${raw}" "${page}"
      ;;
  esac

  if ! jq -e '
    type == "array" and all(.[];
      ((.id | type) == "number" or (.id | type) == "string") and
      ((.id | tostring) | length > 0) and
      (.ref | type == "string" and length > 0))
  ' "${page}" >/dev/null; then
    echo "kaggle-models-json.sh: Kaggle returned an invalid model page" >&2
    exit 1
  fi
  jq -c '[.[] | {
    id: (.id | tostring), ref, title: (.title // .ref), subtitle, author
  }]' "${page}" >> "${pages}"

  [ -n "${next_token}" ] || break
  if grep -Fqx -- "${next_token}" "${seen}"; then
    echo "kaggle-models-json.sh: Kaggle repeated a page token" >&2
    exit 1
  fi
  printf '%s\n' "${next_token}" >> "${seen}"
  token=${next_token}
done

jq -s '
  add // []
  | if length == ([.[].id] | unique | length)
    then sort_by(.id)
    else error("provider returned duplicate model ids")
    end
' "${pages}"
