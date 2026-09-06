#!/bin/sh
# gws-doc-text.sh <document-id> — print one Google Doc as plain text for `read --body`.
set -eu

case "${1:-}" in
  --version | -v) echo "gws-doc-text.sh (fkf base helper)"; exit 0 ;;
  -* | "") echo "usage: gws-doc-text.sh <document-id>" >&2; exit 2 ;;
  *) ;;
esac
[ "$#" -eq 1 ] || { echo "usage: gws-doc-text.sh <document-id>" >&2; exit 2; }

document_id=$1
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
params=$(jq -cn --arg document_id "$document_id" '{documentId:$document_id}')
gws docs documents get --params "$params" > "$tmp/document.json"

# The Docs response stores visible text runs in document order across paragraphs, tables,
# headers, and footers. Recurse through the JSON rather than assuming a paragraph-only shape.
jq -er '[.. | objects | .textRun?.content? // empty] | join("")' "$tmp/document.json"
