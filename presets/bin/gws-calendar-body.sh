#!/bin/sh
# gws-calendar-body.sh <record-id> — fetch useful event prose.
set -eu

case "${1:-}" in
  --version | -v) echo "gws-calendar-body.sh (fkf base helper)"; exit 0 ;;
  -* | "") echo "usage: gws-calendar-body.sh <record-id>" >&2; exit 2 ;;
  *) ;;
esac
[ "$#" -eq 1 ] || {
  echo "usage: gws-calendar-body.sh <record-id>" >&2
  exit 2
}

record_id=$1
case "${record_id}" in
  *~*) ;;
  *)
    echo "gws-calendar-body.sh: record id has no calendar separator" >&2
    exit 2
    ;;
esac
# Event IDs use Google's base32hex alphabet and cannot contain `~`; calendar IDs are commonly
# email addresses and may. Split at the last separator so the durable composite ID stays usable.
calendar_id=${record_id%~*}
event_id=${record_id##*~}
[ -n "${calendar_id}" ] && [ -n "${event_id}" ] || {
  echo "gws-calendar-body.sh: record id has an empty provider identifier" >&2
  exit 2
}
params=$(jq -cn --arg calendar_id "${calendar_id}" --arg event_id "${event_id}" \
  '{calendarId:$calendar_id,eventId:$event_id}')
scratch=$(mktemp -d)
response=${scratch}/response.json
provider_pipe=${scratch}/provider.pipe
provider_pid=
stop_provider() {
  [ -n "${provider_pid}" ] || return 0
  # A provider that has already crossed the hard output boundary is not allowed a graceful,
  # unbounded shutdown window. SIGKILL makes the following wait finite even if it ignores TERM.
  kill -KILL "${provider_pid}" 2>/dev/null || true
  wait "${provider_pid}" 2>/dev/null || true
  provider_pid=
}
cleanup() {
  stop_provider
  rm -rf "${scratch}"
}
trap cleanup EXIT
trap 'exit 1' HUP INT TERM
mkfifo "${provider_pipe}"

gws calendar events get --params "${params}" >"${provider_pipe}" 2>/dev/null &
provider_pid=$!
# Read limit-plus-one beyond FKF's 64 MiB command ceiling while gws is running. Checking a
# completed redirection would first allow an arbitrary provider response to fill temporary disk.
if ! head -c 67108865 "${provider_pipe}" >"${response}"; then
  stop_provider
  echo "gws-calendar-body.sh: cannot capture the calendar event" >&2
  exit 1
fi
response_bytes=$(wc -c <"${response}" | tr -d ' ')
if [ "${response_bytes}" -gt 67108864 ]; then
  stop_provider
  echo "gws-calendar-body.sh: provider response exceeds FKF's 64 MiB command bound" >&2
  exit 1
fi
if ! wait "${provider_pid}"; then
  echo "gws-calendar-body.sh: cannot fetch the calendar event" >&2
  exit 1
fi
provider_pid=

jq -er --arg event_id "${event_id}" '
  if type != "object" or .id != $event_id then
    error("provider returned the wrong calendar event")
  else
    [
      (.description | select(type == "string" and length > 0)),
      (.location | select(type == "string" and length > 0) | "Location: " + .),
      (.hangoutLink | select(type == "string" and length > 0) | "Conference: " + .)
    ]
    | join("\n\n")
  end
' "${response}"
