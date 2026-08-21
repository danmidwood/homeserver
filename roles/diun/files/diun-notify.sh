#!/bin/sh
# Invoked by Diun's script notifier, once per image entry, inside Diun's own
# container. Pushes a one-shot alert into Alertmanager so image updates arrive
# through the same Telegram path as every other alert.
#
# The container is Alpine busybox: there is no jq, no curl and no bash here.
# The JSON is therefore assembled by hand and posted with busybox wget, and
# every interpolated value has to be escaped explicitly.
#
# No `set -e`: a failure to read one optional field must not stop the alert
# being sent. `set -u` stays on, and every variable below has a default.
set -u

ALERTMANAGER_URL="${ALERTMANAGER_URL:-http://alertmanager:9093/api/v2/alerts}"

# Escape a value for use inside a JSON string literal.
json_escape() {
  printf '%s' "$1" | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g'
}

# Escape for Telegram's HTML parse_mode, which silently drops the whole
# message if the text contains a stray '<', '>' or '&'. Ampersand must be
# replaced first, or the entities introduced below get re-escaped.
html_escape() {
  printf '%s' "$1" | sed -e 's/&/\&amp;/g' -e 's/</\&lt;/g' -e 's/>/\&gt;/g'
}

# Order matters: HTML-escape for the human, then JSON-escape so the result
# survives being embedded in the payload.
IMAGE=$(json_escape "$(html_escape "${DIUN_ENTRY_IMAGE:-unknown}")")
STATUS=$(json_escape "$(html_escape "${DIUN_ENTRY_STATUS:-unknown}")")
HUBLINK=$(json_escape "$(html_escape "${DIUN_ENTRY_HUBLINK:-none}")")
DIGEST=$(json_escape "$(html_escape "${DIUN_ENTRY_DIGEST:-unknown}")")
PLATFORM=$(json_escape "$(html_escape "${DIUN_ENTRY_PLATFORM:-unknown}")")
CREATED=$(json_escape "$(html_escape "${DIUN_ENTRY_CREATED:-unknown}")")

STARTS_AT=$(date -u +%Y-%m-%dT%H:%M:%SZ)

# Alertmanager treats an alert as firing until it stops being sent. A new
# image tag is a moment, not a state, so an explicit endsAt makes it
# self-resolve instead of re-notifying forever.
#
# 30 minutes, not 5: the info route holds alerts for a 5m group_wait before
# notifying, and an alert whose endsAt fell inside that window could resolve
# before it was ever delivered.
#
# Busybox date has no GNU relative arithmetic -- `date -d "+30 minutes"` is
# rejected outright -- so the epoch is computed and reformatted instead.
ENDS_AT=$(date -u -d "@$(( $(date -u +%s) + 1800 ))" +%Y-%m-%dT%H:%M:%SZ)

PAYLOAD=$(cat <<JSON
[{
  "labels": {
    "alertname": "ImageUpdateAvailable",
    "severity": "info",
    "instance": "xps",
    "job": "diun",
    "image": "${IMAGE}"
  },
  "annotations": {
    "summary": "New image available: ${IMAGE}",
    "description": "Diun reports status '${STATUS}' for ${IMAGE} on ${PLATFORM}, built ${CREATED}. Digest ${DIGEST}. Registry page: ${HUBLINK} . Bump the pinned digest in the Ansible role that owns this image when you are ready."
  },
  "startsAt": "${STARTS_AT}",
  "endsAt": "${ENDS_AT}"
}]
JSON
)

wget --quiet --output-document=/dev/null \
     --header="Content-Type: application/json" \
     --post-data="${PAYLOAD}" \
     "${ALERTMANAGER_URL}"
