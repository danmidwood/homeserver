#!/usr/bin/env bash
# Runs roles/diun/files/diun-notify.sh inside the pinned Diun image, captures
# the HTTP body it posts, and checks it is valid JSON with the labels
# Alertmanager routes on. This is the only test that can catch a quoting bug
# in the hand-built JSON before it reaches a live Alertmanager.
set -euo pipefail

export PATH="/Applications/Docker.app/Contents/Resources/bin:$PATH"

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
IMAGE="crazymax/diun:4.33.0@sha256:e324b793eb32dfb7f74d3a39421ebf090141caaadee69b8f78da63112408ee25"
SCRIPT="$REPO_ROOT/roles/diun/files/diun-notify.sh"

if [ ! -f "$SCRIPT" ]; then
  echo "  FAIL: $SCRIPT does not exist"
  exit 1
fi

echo "==> Capturing the payload diun-notify.sh posts"

# The image name deliberately contains a '&' and a '<' so the test fails if
# HTML escaping is dropped, and a '"' so it fails if JSON escaping is dropped.
CAPTURED=$(docker run --rm \
  -v "$SCRIPT:/notify.sh:ro" \
  -e 'DIUN_ENTRY_IMAGE=ghcr.io/example/img:1.2.3&<"test' \
  -e 'DIUN_ENTRY_STATUS=new' \
  -e 'DIUN_ENTRY_HUBLINK=https://example.invalid/img' \
  -e 'DIUN_ENTRY_DIGEST=sha256:0000000000000000000000000000000000000000000000000000000000000000' \
  -e 'DIUN_ENTRY_PLATFORM=linux/amd64' \
  -e 'DIUN_ENTRY_CREATED=2026-08-21T00:00:00Z' \
  -e 'ALERTMANAGER_URL=http://127.0.0.1:9999/api/v2/alerts' \
  --entrypoint sh "$IMAGE" -c '
    nc -l -p 9999 > /tmp/capture 2>/dev/null &
    NCPID=$!
    sleep 1
    sh /notify.sh || true
    sleep 1
    kill $NCPID 2>/dev/null || true
    # Strip the HTTP request headers, leaving the body.
    sed -n "/^\r\?$/,\$p" /tmp/capture | tail -n +2
  ')

if [ -z "$CAPTURED" ]; then
  echo "  FAIL: the script posted nothing"
  exit 1
fi

echo "$CAPTURED" | python3 -c '
import json, sys
from datetime import datetime

body = sys.stdin.read().strip()
try:
    alerts = json.loads(body)
except json.JSONDecodeError as e:
    print("  FAIL: payload is not valid JSON: %s" % e)
    print("  body was: %r" % body[:400])
    sys.exit(1)

def check(cond, msg):
    if not cond:
        print("  FAIL: %s" % msg)
        sys.exit(1)

check(isinstance(alerts, list) and len(alerts) == 1, "expected a one-element array")
a = alerts[0]
labels = a["labels"]
check(labels.get("alertname") == "ImageUpdateAvailable", "alertname wrong: %r" % labels.get("alertname"))
check(labels.get("severity") == "info", "severity must be one of critical/warning/info/none, got %r" % labels.get("severity"))
check(labels.get("job") == "diun", "job wrong: %r" % labels.get("job"))
check(labels.get("instance") == "xps", "instance wrong: %r" % labels.get("instance"))
check(bool(labels.get("image")), "the image label must be populated")
check(a["startsAt"] < a["endsAt"], "endsAt must be after startsAt")

# The alert must outlive the info route group_wait of 5m, or it can resolve
# before it is ever notified.
fmt = "%Y-%m-%dT%H:%M:%SZ"
delta = datetime.strptime(a["endsAt"], fmt) - datetime.strptime(a["startsAt"], fmt)
check(delta.total_seconds() >= 600, "endsAt must be at least 10m out, got %s" % delta)

# HTML escaping must have happened: no raw < or & may survive into text that
# Telegram parses as HTML.
text = a["annotations"]["summary"] + a["annotations"]["description"]
check("&amp;" in text or "&lt;" in text, "HTML escaping was not applied")
check("&<" not in text, "raw < survived escaping")

print("  OK: valid JSON, %d alert, labels and escaping correct" % len(alerts))
'
echo "==> diun-notify.sh checks passed"
