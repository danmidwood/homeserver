#!/bin/bash
# Invoked by restic-backup.service's OnFailure=. Pushes a one-shot alert
# straight into Alertmanager, carrying the exit status and the tail of the
# journal so the Telegram message says what actually went wrong.
#
# No `set -e`: this runs when something has ALREADY failed, and a failure
# to collect one optional detail must not stop the alert being sent.
set -uo pipefail

ALERTMANAGER_URL="http://localhost:9093/api/v2/alerts"

RESULT=$(systemctl show restic-backup.service -p Result --value)
EXIT_CODE=$(systemctl show restic-backup.service -p ExecMainStatus --value)
# Telegram's HTML parse_mode (used by the telegram-critical receiver) drops
# the entire message if the text contains a stray '<', '>' or '&' -- and
# restic's S3/B2 error bodies (XML, e.g. <Error><Code>AccessDenied</Code>...)
# and pg_dump output routinely contain exactly that. Escape before it becomes
# message text. Order matters: '&' must be replaced first, or the ampersands
# introduced by the &lt;/&gt; substitutions would themselves get re-escaped
# into &amp;lt; / &amp;gt;.
LOG_TAIL=$(journalctl -u restic-backup.service -n 20 --no-pager -o cat | tail -c 800 | sed -e 's/&/\&amp;/g' -e 's/</\&lt;/g' -e 's/>/\&gt;/g')

STARTS_AT=$(date -u +%Y-%m-%dT%H:%M:%SZ)
# Alertmanager treats an alert as firing until it stops being sent. A backup
# failure is a moment, not a state, so an explicit endsAt makes it
# self-resolve instead of re-notifying forever.
ENDS_AT=$(date -u -d '+15 minutes' +%Y-%m-%dT%H:%M:%SZ)

PAYLOAD=$(jq -n \
  --arg starts "$STARTS_AT" \
  --arg ends "$ENDS_AT" \
  --arg result "$RESULT" \
  --arg code "$EXIT_CODE" \
  --arg log "$LOG_TAIL" \
  '[{
      labels: {
        alertname: "BackupFailed",
        severity: "critical",
        instance: "xps",
        job: "backup"
      },
      annotations: {
        summary: ("restic backup to B2 failed: " + $result + " (exit " + $code + ")"),
        description: $log
      },
      startsAt: $starts,
      endsAt: $ends
    }]')

curl --silent --show-error --max-time 20 \
  --header 'Content-Type: application/json' \
  --data "$PAYLOAD" \
  "$ALERTMANAGER_URL"
