#!/bin/bash
#
# Takes a read-only btrfs snapshot of the Time Machine subvolume and prunes the
# oldest beyond the retention count.
#
# Usage: timemachine-snapshot.sh <subvolume> <snapshot-dir> <retention>
#
# Time Machine keeps its own history, so this is not for recovering a file. It
# is for the two things Time Machine cannot survive: something on the Mac
# encrypting the share over SMB, and Time Machine corrupting its own
# sparsebundle. A read-only snapshot is reachable by neither.
set -euo pipefail

# Configuration comes from the unit rather than being templated in, so this
# stays a plain shell script: bash's ${#array[@]} reads as a Jinja comment.
SUBVOL="$1"
SNAPDIR="$2"
RETAIN="$3"
TEXTFILE_DIR=/var/lib/node_exporter/textfile_collector
METRICS_FILE="${TEXTFILE_DIR}/timemachine.prom"

mkdir -p "$SNAPDIR"
btrfs subvolume snapshot -r "$SUBVOL" "${SNAPDIR}/tm-$(date +%Y%m%d-%H%M%S)" >/dev/null

# Oldest first, delete everything past the retention count. The names sort
# chronologically because the timestamp is fixed-width.
mapfile -t snaps < <(find "$SNAPDIR" -maxdepth 1 -mindepth 1 -name 'tm-*' -printf '%f\n' | sort)
if [ "${#snaps[@]}" -gt "$RETAIN" ]; then
  for old in "${snaps[@]:0:$(( ${#snaps[@]} - RETAIN ))}"; do
    btrfs subvolume delete "${SNAPDIR}/${old}" >/dev/null
  done
fi

count=$(find "$SNAPDIR" -maxdepth 1 -mindepth 1 -name 'tm-*' | wc -l)
used=$(du -sb "$SUBVOL" 2>/dev/null | cut -f1)

# Average extents across a sample of sparsebundle bands. Every band is the same
# size, so the average is comparable over time; sampling keeps this cheap on a
# share that holds tens of thousands of them.
# head closes the pipe once it has enough lines, which kills find with SIGPIPE
# and makes the pipeline exit 141. Under pipefail that aborts the script, so
# this one pipeline runs without it. The first run passed only because the
# share was empty and find never wrote enough to be cut off.
sample=$(set +o pipefail; find "$SUBVOL" -path '*/bands/*' -type f 2>/dev/null | head -200)
extents=0
files=0
if [ -n "$sample" ]; then
  while read -r f; do
    e=$(filefrag "$f" 2>/dev/null | grep -oE '[0-9]+ extent' | cut -d' ' -f1) || continue
    [ -n "$e" ] || continue
    extents=$(( extents + e ))
    files=$(( files + 1 ))
  done <<< "$sample"
fi
if [ "$files" -gt 0 ]; then
  avg=$(( extents / files ))
else
  avg=0
fi

# Written to a temporary file and renamed, because a rename within the same
# filesystem is atomic: node_exporter can never scrape a half-written file.
mkdir -p "$TEXTFILE_DIR"
cat > "${METRICS_FILE}.tmp" <<METRICS
# HELP timemachine_snapshot_last_success_timestamp_seconds Unix time of the last successful snapshot.
# TYPE timemachine_snapshot_last_success_timestamp_seconds gauge
timemachine_snapshot_last_success_timestamp_seconds $(date +%s)
# HELP timemachine_snapshot_count Snapshots currently retained.
# TYPE timemachine_snapshot_count gauge
timemachine_snapshot_count ${count}
# HELP timemachine_bytes_used Apparent size of the Time Machine subvolume.
# TYPE timemachine_bytes_used gauge
timemachine_bytes_used ${used:-0}
# HELP timemachine_band_extents_avg Mean extents per sparsebundle band, over a sample.
# TYPE timemachine_band_extents_avg gauge
timemachine_band_extents_avg ${avg}
# HELP timemachine_band_sample_size Bands measured for the average above.
# TYPE timemachine_band_sample_size gauge
timemachine_band_sample_size ${files}
METRICS
mv "${METRICS_FILE}.tmp" "${METRICS_FILE}"

# Said out loud so a run that reaches the end is distinguishable in the journal
# from one that died partway with nothing to show for it.
echo "snapshot taken, ${count} retained, ${files} bands sampled averaging ${avg} extents"
