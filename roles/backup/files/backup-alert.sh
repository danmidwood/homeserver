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

# journalctl -u returns the last N lines of the UNIT's journal, spanning
# every invocation ever logged -- not the lines from the run that just
# failed. A backup that fails after a run of successful days would open
# the alert with yesterday's "snapshot saved" / "Finished Restic backup"
# lines, burying the actual error in the middle and reading as a false
# alarm. Scope to the invocation that OnFailure= is reacting to instead.
INVOCATION_ID=$(systemctl show restic-backup.service -p InvocationID --value)
if [ -n "$INVOCATION_ID" ]; then
  JOURNAL_CMD=(journalctl "_SYSTEMD_INVOCATION_ID=$INVOCATION_ID" --no-pager -o cat)
else
  # Some context is better than none: fall back to the previous
  # unit-scoped tail rather than sending an alert with no log text at all.
  JOURNAL_CMD=(journalctl -u restic-backup.service -n 20 --no-pager -o cat)
fi

# Telegram's HTML parse_mode (used by the telegram-critical receiver) drops
# the entire message if the text contains a stray '<', '>' or '&' -- and
# restic's S3/B2 error bodies (XML, e.g. <Error><Code>AccessDenied</Code>...)
# and pg_dump output routinely contain exactly that. Escape before it becomes
# message text. Order matters: '&' must be replaced first, or the ampersands
# introduced by the &lt;/&gt; substitutions would themselves get re-escaped
# into &amp;lt; / &amp;gt;.
LOG_TAIL=$("${JOURNAL_CMD[@]}" | tail -c 800 | sed -e 's/&/\&amp;/g' -e 's/</\&lt;/g' -e 's/>/\&gt;/g')

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

# jq failing (bad substitution, etc.) leaves PAYLOAD as an empty string --
# set -u does not catch this, the variable is set, just empty. curl would
# then POST an empty body, Alertmanager would reject it with 400, and
# plain curl exits 0 on HTTP error responses, so the unit would report
# SUCCESS while the alert silently never went out. Catch it explicitly.
if [ -z "$PAYLOAD" ]; then
  echo "backup-alert.sh: PAYLOAD is empty (jq failed?), refusing to POST an empty body" >&2
  exit 1
fi

# --fail-with-body: without it, curl exits 0 on a 4xx/5xx response too,
# which is the other way this alert can vanish without trace. With it, an
# HTTP error becomes a non-zero curl exit (unit goes `failed`, visible in
# `systemctl status`) and the response body still lands in the journal.
curl --silent --show-error --max-time 20 --fail-with-body \
  --header 'Content-Type: application/json' \
  --data "$PAYLOAD" \
  "$ALERTMANAGER_URL"
