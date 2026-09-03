#!/bin/sh
# writing-source-json.sh <file>... — invoke the reviewed authored-document indexer.
set -eu

case "${1:-}" in --version | -v) echo "writing-source-json.sh (fkf preset helper)"; exit 0 ;; esac

[ "$#" -gt 0 ] || {
  echo "usage: writing-source-json.sh <file>..." >&2
  exit 2
}
exec writing-index.sh "$@"
