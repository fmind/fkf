#!/bin/sh
# gmail-json.sh <start> <end> — collect Gmail metadata in the exact half-open RFC3339 window.
# Gmail interprets date operands at midnight Pacific time, so the API query uses epoch seconds.
set -eu

page_validator=$(cd "$(dirname "$0")" && pwd)/gws-page-json.sh

start=${1:?usage: gmail-json.sh <start> <end>}
end=${2:?usage: gmail-json.sh <start> <end>}

# Assignment-only commands carry jq's exit status; set -e therefore stops before gws if either
# controlled RFC3339 bound cannot be converted.
after=$(jq -nr --arg value "$start" '$value | fromdateiso8601')
before=$(jq -nr --arg value "$end" '$value | fromdateiso8601')
# Gmail's after: operator is exclusive. Widen it by one second, then apply the exact half-open
# window to each fetched message's millisecond internalDate below.
search_after=$((after - 1))
start_ms=$(jq -nr --argjson seconds "$after" '$seconds * 1000')
end_ms=$(jq -nr --argjson seconds "$before" '$seconds * 1000')

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
list_params=$(jq -cn --arg after "$search_after" --arg before "$before" \
  '{userId:"me",q:("after:" + $after + " before:" + $before)}')

# Keep every fallible stage separate. A pipeline under POSIX sh reports only its final status;
# sequential files ensure a failed gws or jq command fails the collection instead of filing an
# empty, complete-looking day.
gws gmail users messages list --params "$list_params" --page-all --page-limit 100 > "$tmp/raw-list.json"
"$page_validator" messages < "$tmp/raw-list.json" > "$tmp/list.json"
jq -r '.messages[]?.id' "$tmp/list.json" > "$tmp/ids"
: > "$tmp/messages.json"
while IFS= read -r id; do
  [ -n "$id" ] || continue
  message_params=$(jq -cn --arg id "$id" '{userId:"me",id:$id,format:"metadata"}')
  gws gmail users messages get --params "$message_params" >> "$tmp/messages.json"
done < "$tmp/ids"

jq -c --argjson start_ms "$start_ms" --argjson end_ms "$end_ms" '
  # Mail subjects may contain invisible formatting copied from HTML or a rich-text editor.
  def clean_title:
    if type == "string" then
      gsub("\\p{Cf}"; "") | gsub("[\\p{Cc}\\s]+"; " ") | gsub("^ +| +$"; "")
    else "" end;
  def email:
    if type != "string" then empty
    elif test("<[^>]+>$") then capture("<(?<value>[^>]+)>$").value
    else . end
    | ascii_downcase
    | select(test("^[^[:space:]@]+@[^[:space:]@]+$"));
  def fkf_identity: @uri | gsub("%3A"; ":") | gsub("%2F"; "/")
    | gsub("%40"; "@") | gsub("%2B"; "+") | gsub("~"; "%7E");
  select((.internalDate | tonumber) >= $start_ms and (.internalDate | tonumber) < $end_ms)
  | (.payload.headers | map({key: (.name | ascii_downcase), value}) | from_entries) as $h
  | {id, threadId, internalDate, labelIds, sizeEstimate,
     subject: (($h.subject | clean_title) as $subject
       | if $subject == "" then "Email without subject" else $subject end),
     from: $h.from,
     to: [$h.to, $h.cc | select(. != null) | match("(?:\"[^\"]*\"|[^,])+"; "g").string | gsub("^\\s+|\\s+$"; "")],
     list_id: $h."list-id"}
  | . + {participant_uris: ([.from, .to[]]
      | map(email | "person:email/" + fkf_identity) | unique)}' "$tmp/messages.json"
