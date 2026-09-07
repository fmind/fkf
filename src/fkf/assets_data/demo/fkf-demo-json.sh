#!/bin/sh
set -eu

[ "$#" -eq 2 ] || { echo "usage: fkf-demo-json.sh <source> <date>" >&2; exit 2; }
source=$1
date=$2
case "$source" in
  github-pull-requests|google-calendar-events|google-gmail-emails|jira-issues|git-commits|shell-commands) ;;
  *) echo "unknown synthetic source: $source" >&2; exit 2 ;;
esac
case "$date" in
  ????-??-??) ;;
  *) echo "date must be YYYY-MM-DD" >&2; exit 2 ;;
esac
case "$date" in
  *[!0-9-]*) echo "date must be YYYY-MM-DD" >&2; exit 2 ;;
esac

printf '[{"id":"%s-%s-0","time":"%s","title":"Synthetic %s activity","url":"https://example.test/demo/%s/%s","repo":"repo:github.com/fmind/fkf","author":"person:email/demo@example.test","attendees":["person:email/demo@example.test"],"from":"person:email/demo@example.test","to":["person:email/reader@example.test"],"assignee":"person:email/demo@example.test","ticket":"ticket:DEMO-1"}]\n' \
  "$source" "$date" "$date" "$source" "$source" "$date"
