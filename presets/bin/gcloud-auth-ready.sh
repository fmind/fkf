#!/bin/sh
# gcloud-auth-ready.sh — succeed only when gcloud names an active account. `gcloud auth list`
# exits zero with empty output for a logged-out profile, so its exit status alone is not a probe.
set -eu

account=$(gcloud auth list --filter=status:ACTIVE --format='value(account)' 2>/dev/null) || exit $?
case $account in
*[![:space:]]*) exit 0 ;;
*) exit 1 ;;
esac
