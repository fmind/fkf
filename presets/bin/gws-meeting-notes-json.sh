#!/bin/sh
# gws-meeting-notes-json.sh <start> <end> <date> <next-date> <name-prefix> <base>
# Collect Google Docs created in an exact window whose names are Gemini notes or carry the
# owner-chosen prefix, then relate them to the calendar event that attached or named them.
set -eu

case "${1:-}" in
  --version | -v) echo "gws-meeting-notes-json.sh (fkf base helper)"; exit 0 ;;
  *) ;;
esac
[ "$#" -eq 6 ] || {
  echo "usage: gws-meeting-notes-json.sh <start> <end> <date> <next-date> <name-prefix> <base>" >&2
  exit 2
}

start=$1
end=$2
date=$3
next_date=$4
prefix=$5
base=$6
case "${base}" in
  /*) ;;
  *) echo "gws-meeting-notes-json.sh: base must be absolute" >&2; exit 2 ;;
esac
single_quote="'"
backslash="\\"
case "${prefix}" in
  *"${single_quote}"* | *"${backslash}"*) echo "gws-meeting-notes-json.sh: name prefix may not contain quote or backslash" >&2; exit 2 ;;
  *) ;;
esac

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

query="mimeType='application/vnd.google-apps.document' and trashed=false and createdTime >= '${start}' and createdTime < '${end}' and (name contains ' - Notes by Gemini' or name contains '${prefix}')"
params=$(jq -cn --arg query "${query}" '{q:$query,pageSize:1000,orderBy:"createdTime asc",fields:"nextPageToken,files(id,name,createdTime,modifiedTime,webViewLink,owners(emailAddress,me))"}')
gws-page-json.sh files gws drive files list --params "${params}" \
  --page-all --page-limit 100 > "${tmp}/drive-pages.json"
gws-calendars-json.sh "${start}" "${end}" "${date}" "${next_date}" > "${tmp}/calendar-events.json"

jq -s -c --arg start "${start}" --arg end "${end}" --arg date "${date}" '
  def epoch:
    capture("^(?<stamp>[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2})(?:\\.[0-9]+)?(?<zone>Z|[+-][0-9]{2}:[0-9]{2})$") as $parsed
    | ($parsed.stamp + "Z" | fromdateiso8601) as $local
    | if $parsed.zone == "Z" then $local
      else ($parsed.zone | capture("^(?<sign>[+-])(?<hour>[0-9]{2}):(?<minute>[0-9]{2})$")) as $offset
      | (($offset.hour | tonumber) * 3600 + ($offset.minute | tonumber) * 60) as $seconds
      | if $offset.sign == "+" then $local - $seconds else $local + $seconds end
      end;
  def absolute: if . < 0 then -. else . end;
  def fkf_fragment: @uri | gsub("%3A"; ":") | gsub("%2F"; "/")
    | gsub("%40"; "@") | gsub("%2B"; "+") | gsub("~"; "%7E");
  def person_uri: "person:email/" + (ascii_downcase | fkf_fragment);
  def attachment_uri: "document:drive.google.com/" + fkf_fragment;
  def event_uri:
    # Window sources may collect several days once; derive the actual local event bucket
    # instead of binding every relation to the first {{date}} in the span. All-day events
    # already carry that local day and have no timestamp to convert.
    "events/" + (if (.at | test("^[0-9]{4}-[0-9]{2}-[0-9]{2}$")) then .at
      else ((.at | epoch) | strflocaltime("%Y-%m-%d")) end) +
      "/google-calendar-events.json#" + (.uid | fkf_fragment);
  (.[-1]) as $events
  | ([.[0:-1][].files[]?] | unique_by(.id))
  | map(select(.createdTime >= $start and .createdTime < $end))
  | map(. as $file
      | ([$events[] | select(any(.attachments[]?; .fileId == $file.id))]) as $attached
      | (if ($attached | length) > 0 then $attached
         else (($file.createdTime | epoch) as $created
           | [$events[] as $event
             | select(($event.attachments // [] | length) == 0)
             | select(($event.summary // "") != "" and
                 ($file.name | startswith(($event.summary // "") + " - ")))
             | select(($event.at // "") | test("T"))
             | $event + {fkf_start_distance: (($event.at | epoch) - $created | absolute)}]
           | sort_by(.fkf_start_distance, .uid)
           | if length > 0 and .[0].fkf_start_distance <= 21600 then [.[0]] else [] end)
         end) as $meetings
      | {
          id: $file.id,
          at: $file.createdTime,
          modified: $file.modifiedTime,
          title: $file.name,
          url: $file.webViewLink,
          owner_uris: [$file.owners[]?.emailAddress | select(type == "string" and length > 0) | person_uri] | unique,
          attachment_uris: [($file.id | attachment_uri)],
          meeting_uris: [$meetings[] | event_uri] | unique
        }
      | with_entries(select(.value != null)))
  | sort_by(.at, .id)
' "${tmp}/drive-pages.json" "${tmp}/calendar-events.json" > "${tmp}/meeting-notes.json"

# A live calendar response is join input, not durable evidence. Require the separately
# collected calendar document before emitting its record URI, so enabling notes alone cannot
# create a relation that stored reads and the timeline can never resolve.
jq -r '[.[].meeting_uris[]?] | unique[] | [split("#")[0], split("#")[1]] | @tsv' \
  "${tmp}/meeting-notes.json" > "${tmp}/calendar-records.tsv"
tab=$(printf '\t')
while IFS="${tab}" read -r relative fragment; do
  [ -n "${relative}" ] || continue
  case "${relative}" in
    events/[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]/google-calendar-events.json) ;;
    *) echo "gws-meeting-notes-json.sh: invalid calendar relation path" >&2; exit 1 ;;
  esac
  calendar=${base}/${relative}
  day_dir=${calendar%/*}
  if [ ! -f "${calendar}" ] || [ -L "${base}/events" ] || [ -L "${day_dir}" ] || [ -L "${calendar}" ] ||
    ! jq -e --arg fragment "${fragment}" '
      def fkf_fragment: @uri | gsub("%3A"; ":") | gsub("%2F"; "/")
        | gsub("%40"; "@") | gsub("%2B"; "+") | gsub("~"; "%7E");
      .fkf == 1 and .source == "google-calendar-events" and .fields.id == ".uid" and
        any(.records[]?; (.uid | type) == "string" and ((.uid | fkf_fragment) == $fragment))
    ' "${calendar}" > /dev/null; then
    echo "gws-meeting-notes-json.sh: enable and sync google-calendar-events before meeting-notes" >&2
    exit 1
  fi
done < "${tmp}/calendar-records.tsv"

cat "${tmp}/meeting-notes.json"
