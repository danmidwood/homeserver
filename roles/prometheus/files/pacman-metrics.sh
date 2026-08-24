#!/bin/bash
# Reports how long it has been since a full system upgrade.
#
# Arch has no supported partial upgrade: packages are built against whatever
# versions of their dependencies are current, so upgrading some but not others
# is how an Arch system breaks. That makes "how long since -Syu" the only
# meaningful staleness measure, and a long gap the actual risk -- a monthly
# upgrade is twenty packages, a twenty-month one is four hundred and includes
# the kernel, libc and the container runtime.
#
# Read from pacman's own log rather than by counting pending updates, which
# would need pacman-contrib installed and a database sync. This needs neither,
# which matters because installing anything is itself unwise while the system
# is behind.
set -uo pipefail

TEXTFILE_DIR=/var/lib/node_exporter/textfile_collector
METRICS_FILE="${TEXTFILE_DIR}/pacman.prom"
LOG=/var/log/pacman.log

last=$(grep -F "starting full system upgrade" "$LOG" 2>/dev/null | tail -1 | cut -c2-17)
if [ -z "$last" ]; then
  ts=0
else
  ts=$(date -d "$(echo "$last" | tr 'T' ' ')" +%s 2>/dev/null || echo 0)
fi

# Atomic write: node_exporter must never scrape a half-written file, and the
# temporary name must not end in .prom or it would read that too.
cat > "${METRICS_FILE}.tmp" <<METRICS
# HELP pacman_last_full_upgrade_timestamp_seconds Unix time of the last pacman -Syu, from pacman's log.
# TYPE pacman_last_full_upgrade_timestamp_seconds gauge
pacman_last_full_upgrade_timestamp_seconds ${ts}
METRICS
mv "${METRICS_FILE}.tmp" "${METRICS_FILE}"
