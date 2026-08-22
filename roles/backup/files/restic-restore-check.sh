#!/bin/bash
# Proves the backups can actually be restored, rather than merely that they ran.
#
# Everything else in this repo checks that a backup STARTED and FINISHED. None
# of it checks that what landed in B2 can be read back and is correct. A backup
# nobody has ever restored from is a hypothesis.
#
# Two independent checks, because they fail for different reasons:
#
#   1. Restore a canary file and compare its hash to the live copy. This
#      exercises the whole restore path -- credentials, repository access,
#      decryption, extraction -- and would catch a repository that is present
#      but unreadable, or a wrong password.
#
#   2. restic check --read-data-subset, which downloads a sample of real pack
#      files and verifies their hashes against the index. This is what catches
#      silent corruption in data that nothing has touched for months. The
#      canary alone would not, because it is small, recent, and likely cached.
#
# Failure is reported three ways: this script pushes its own alert carrying the
# detail, the metric goes stale (which RestoreDrillStale catches even if this
# script never runs at all), and the systemd unit enters the failed state.
#
# It pushes its own alert rather than using OnFailure=backup-alert.service,
# because that handler reads `systemctl show restic-backup.service` by name and
# would report a backup failure for a restore-drill failure -- the wrong unit,
# with the wrong journal attached.

set -uo pipefail

ALERTMANAGER_URL="${ALERTMANAGER_URL:-http://localhost:9093/api/v2/alerts}"
TEXTFILE_DIR=/var/lib/node_exporter/textfile_collector
METRICS_FILE="${TEXTFILE_DIR}/restic_restore.prom"
CANARY=/mnt/storage/backup/canary/canary.txt
WORKDIR=$(mktemp -d /tmp/restic-restore-check.XXXXXX)
trap 'rm -rf "$WORKDIR"' EXIT

set -a
. /etc/restic/env
set +a

START=$(date +%s)
failures=0
log=""

# Collected as the checks run so the alert can say what actually went wrong,
# rather than sending the reader to the journal for every failure.
note() {
  echo "$1"
  log="${log}${1}"$'\n'
}

echo "==> restoring the canary from the latest snapshot"
if ! restic restore latest --include "$CANARY" --target "$WORKDIR" 2>&1; then
  note "restore failed"
  failures=$((failures + 1))
else
  restored="${WORKDIR}${CANARY}"
  if [ ! -f "$restored" ]; then
    note "the canary is not present in the latest snapshot"
    failures=$((failures + 1))
  else
    live_hash=$(sha256sum "$CANARY" | awk '{print $1}')
    rest_hash=$(sha256sum "$restored" | awk '{print $1}')
    if [ "$live_hash" != "$rest_hash" ]; then
      note "restored canary does not match the live file (live ${live_hash:0:16}, restored ${rest_hash:0:16})"
      failures=$((failures + 1))
    else
      echo "  canary restored and matches: $live_hash"
    fi
  fi
fi

echo "==> verifying a sample of stored data"
# 1% weekly covers the whole repository over about two years, at a bandwidth
# cost of a few hundred megabytes a week. Raising it costs B2 egress; lowering
# it lengthens the time before corruption anywhere would be noticed.
if ! restic check --read-data-subset=1% 2>&1; then
  note "restic check reported a problem with the stored data"
  failures=$((failures + 1))
fi

END=$(date +%s)

if [ "$failures" -gt 0 ]; then
  echo "restore drill FAILED with ${failures} problem(s)"

  # Telegram's HTML parse mode drops the entire message on a stray angle
  # bracket or ampersand, and restic's error output can contain both.
  safe_log=$(printf '%s' "$log" | sed -e 's/&/\&amp;/g' -e 's/</\&lt;/g' -e 's/>/\&gt;/g' | tail -c 800)

  jq -n \
    --arg starts "$(date -u -d "@${START}" +%Y-%m-%dT%H:%M:%SZ)" \
    --arg ends "$(date -u -d '+26 hours' +%Y-%m-%dT%H:%M:%SZ)" \
    --arg count "$failures" \
    --arg detail "$safe_log" \
    '[{
        labels: {
          alertname: "RestoreDrillFailed",
          severity: "critical",
          instance: "xps",
          job: "backup"
        },
        annotations: {
          summary: ("The backups could not be verified: " + $count + " check(s) failed"),
          description: ($detail + " The nightly backup may still be running fine -- what has failed is proving that what it wrote can be read back. Run `journalctl -u restic-restore-check` on the host.")
        },
        startsAt: $starts,
        endsAt: $ends
      }]' \
    | curl -sf --retry 3 --retry-delay 2 --retry-all-errors --max-time 30 \
        -H "Content-Type: application/json" \
        --data @- "$ALERTMANAGER_URL" >/dev/null || true

  exit 1
fi

# A pushed alert fires until its endsAt passes, so a run that succeeds has to
# retract an earlier failure explicitly. Same labels, endsAt in the past.
jq -n \
  --arg starts "$(date -u -d "@${START}" +%Y-%m-%dT%H:%M:%SZ)" \
  --arg ends "$(date -u -d '-1 minute' +%Y-%m-%dT%H:%M:%SZ)" \
  '[{
      labels: {
        alertname: "RestoreDrillFailed",
        severity: "critical",
        instance: "xps",
        job: "backup"
      },
      startsAt: $starts,
      endsAt: $ends
    }]' \
  | curl -sf --retry 3 --retry-delay 2 --retry-all-errors --max-time 30 \
      -H "Content-Type: application/json" \
      --data @- "$ALERTMANAGER_URL" >/dev/null || true

# Only written on complete success, so the timestamp genuinely means "the
# backups were last proven restorable at this moment".
#
# Written to a temporary file and renamed, because a rename within the same
# filesystem is atomic: node_exporter can never scrape a half-written file. The
# temporary name must not end in .prom or node_exporter would read it.
cat > "${METRICS_FILE}.tmp" <<METRICS
# HELP restic_restore_last_success_timestamp_seconds Unix time of the last successful restore drill and data check.
# TYPE restic_restore_last_success_timestamp_seconds gauge
restic_restore_last_success_timestamp_seconds ${END}
# HELP restic_restore_duration_seconds Wall-clock seconds taken by the last successful restore drill.
# TYPE restic_restore_duration_seconds gauge
restic_restore_duration_seconds $((END - START))
METRICS
mv "${METRICS_FILE}.tmp" "${METRICS_FILE}"

echo "restore drill passed in $((END - START))s"
