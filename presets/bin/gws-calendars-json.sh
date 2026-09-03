#!/bin/sh
# gws-calendars-json.sh <start> <end> <date> <next-date> — collect event-start metadata
# across every visible calendar in one exact half-open window.
set -eu

case "${1:-}" in
  --version | -v) echo "gws-calendars-json.sh (fkf base helper)"; exit 0 ;;
  *) ;;
esac

start=${1:?usage: gws-calendars-json.sh <start> <end> <date> <next-date>}
end=${2:?usage: gws-calendars-json.sh <start> <end> <date> <next-date>}
date=${3:?usage: gws-calendars-json.sh <start> <end> <date> <next-date>}
next_date=${4:?usage: gws-calendars-json.sh <start> <end> <date> <next-date>}

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# Use the installed API-qualified command only. Falling back after any error would turn an
# authentication, network, or provider failure into a misleading command-compatibility retry.
gws-page-json.sh items gws calendar calendarList list \
  --params '{"maxResults":250,"fields":"nextPageToken,items(id,summary,accessRole,primary,selected,hidden,timeZone)"}' \
  --page-all --page-limit 100 > "${tmp}/calendar-pages.json"
jq -s -c '[.[].items[]? | {
  id, summary, accessRole, primary, selected, hidden, timeZone
} | with_entries(select(.value != null))]' "${tmp}/calendar-pages.json" > "${tmp}/calendars.json"
jq -e 'type == "array" and length > 0 and all(.[]; .id | type == "string" and length > 0)' \
  "${tmp}/calendars.json" >/dev/null || {
  echo "gws-calendars-json.sh: the account returned no valid calendar" >&2
  exit 1
}
jq -r '.[] | @base64' "${tmp}/calendars.json" > "${tmp}/calendars"
: > "${tmp}/fragments.json"

while IFS= read -r encoded; do
  calendar=$(printf '%s' "${encoded}" | jq -cR '@base64d | fromjson')
  calendar_id=$(printf '%s' "${calendar}" | jq -r '.id')
  params=$(jq -cn --arg calendar_id "${calendar_id}" --arg start "${start}" --arg end "${end}" \
    '{calendarId:$calendar_id,timeMin:$start,timeMax:$end,singleEvents:true}')
  gws-page-json.sh items gws calendar events list --params "${params}" \
    --page-all --page-limit 100 > "${tmp}/event-pages.json"

  jq -s -c --arg start "${start}" --arg end "${end}" --arg date "${date}" \
    --arg next_date "${next_date}" --argjson calendar "${calendar}" '
    def epoch:
      capture("^(?<stamp>[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2})(?:\\.[0-9]+)?(?<zone>Z|[+-][0-9]{2}:[0-9]{2})$") as $parsed
      | ($parsed.stamp + "Z" | fromdateiso8601) as $local
      | if $parsed.zone == "Z" then $local
        else ($parsed.zone | capture("^(?<sign>[+-])(?<hour>[0-9]{2}):(?<minute>[0-9]{2})$")) as $offset
        | (($offset.hour | tonumber) * 3600 + ($offset.minute | tonumber) * 60) as $seconds
        | if $offset.sign == "+" then $local - $seconds else $local + $seconds end
        end;
    def compact: with_entries(select(.value != null));
    # Calendar summaries are optional and may carry invisible formatting copied from clients.
    def clean_title:
      if type == "string" then
        gsub("\\p{Cf}"; "") | gsub("[\\p{Cc}\\s]+"; " ") | gsub("^ +| +$"; "")
      else "" end;
    def fkf_identity: @uri | gsub("%3A"; ":") | gsub("%2F"; "/")
      | gsub("%40"; "@") | gsub("%2B"; "+") | gsub("~"; "%7E");
    ($start | epoch) as $start_epoch
    | ($end | epoch) as $end_epoch
    | [.[].items[]?
        | (.start.dateTime // .start.date) as $at
        | select(
            if .start.date != null then $at >= $date and $at < $next_date
            else (($at | epoch) >= $start_epoch and ($at | epoch) < $end_epoch)
            end)
        | {
            id, uid: ($calendar.id + "~" + .id), status, eventType, created, updated,
            summary: ((.summary | clean_title) as $summary
              | if $summary == "" then "Calendar event " + (.id | @uri) else $summary end),
            htmlLink, iCalUID, recurringEventId, at: $at, transparency, visibility,
            calendar: $calendar,
            start: (.start | {date, dateTime, timeZone} | compact),
            end: (.end | {date, dateTime, timeZone} | compact),
            creator: ((.creator // {}) | {email, self} | compact),
            organizer: ((.organizer // {}) | {email, self} | compact),
            attendees: [(.attendees // [])[]
              | {email, responseStatus, self, optional, resource} | compact],
            attachments: [(.attachments // [])[]
              | {fileId, fileUrl, title} | compact],
            attachment_uris: [(.attachments // [])[].fileId
              | select(type == "string" and length > 0)
              | "document:drive.google.com/" + fkf_identity]
              | unique,
            conference_id: .conferenceData.conferenceId,
            participant_uris: ([.organizer.email, (.attendees // [])[].email]
              | map(select(type == "string" and length > 0)
                | "person:email/" + (ascii_downcase | fkf_identity))
              | unique)
          }
        | compact]
    | sort_by(.at, .uid)
  ' "${tmp}/event-pages.json" >> "${tmp}/fragments.json"
done < "${tmp}/calendars"

jq -s -c 'add // [] | unique_by(.uid) | sort_by(.at, .uid)' "${tmp}/fragments.json"
