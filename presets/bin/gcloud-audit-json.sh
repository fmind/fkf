#!/bin/sh
# gcloud-audit-json.sh <start> <end> — collect activity metadata for gcloud's active project.
# Project scope is intentionally explicit in the preset documentation: enumerating every
# accessible project would be a different source with different cost and authorization.
set -eu

start=${1:?usage: gcloud-audit-json.sh <start> <end>}
end=${2:?usage: gcloud-audit-json.sh <start> <end>}
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

filter=$(printf 'timestamp>="%s" AND timestamp<"%s" AND log_id("cloudaudit.googleapis.com/activity")' "${start}" "${end}")
# One overflow record distinguishes a complete bounded day from silent --limit truncation.
gcloud logging read "${filter}" --limit=10001 --format=json > "${tmp}/logs.json"
if ! jq -e 'type == "array" and length <= 10000' "${tmp}/logs.json" >/dev/null; then
  echo "gcloud-audit-json.sh: invalid response or 10000-item safety limit reached; cannot prove completeness" >&2
  exit 1
fi

# Audit entries can carry whole API requests and responses. Keep only reviewed identity,
# operation, resource, status, and routing metadata; request bodies and network metadata stay
# in Cloud Logging and can be fetched there when explicitly needed.
jq -c '
  def compact: with_entries(select(.value != null));
  def fkf_identity: @uri | gsub("%3A"; ":") | gsub("%2F"; "/")
    | gsub("%40"; "@") | gsub("%2B"; "+") | gsub("~"; "%7E");
  map({
    uid: ((.resource.labels.project_id // "unknown") + "@" + .insertId + "@" + .timestamp),
    insertId, timestamp, receiveTimestamp, severity, logName,
    resource: ((.resource // {})
      | {type, labels: ((.labels // {})
          | {project_id, location, zone, cluster_name, namespace_name} | compact)} | compact),
    protoPayload: ((.protoPayload // {})
      | {
          serviceName, methodName, resourceName,
          authenticationInfo: ((.authenticationInfo // {})
            | {principalEmail, principalSubject, serviceAccountKeyName,
               principal_uri: (if (.principalEmail // "") == "" then null
                 else ("person:email/" + (.principalEmail | ascii_downcase | fkf_identity)) end)} | compact),
          authorizationInfo: [(.authorizationInfo // [])[]
            | {resource, permission, granted} | compact],
          status: ((.status // {}) | {code} | compact)
        } | compact),
    operation: ((.operation // {}) | {id, producer, first, last} | compact)
  } | compact)
' "${tmp}/logs.json"
