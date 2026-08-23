#!/bin/bash
# Maps unreadable pack files to the files that can no longer be restored.
#
# Read-only and metadata-only: it reads index objects and snapshot trees, and
# never downloads the damaged packs themselves. `restic find --pack` would read
# the pack header and hang on a broken object; --blob searches trees instead.
set -uo pipefail
set -a; . /etc/restic/env; set +a

LOG=/tmp/fullcheck.log
OUT=/tmp/badpacks-report.txt
: > "$OUT"

short=$(grep -oE "Load\(<data/[0-9a-f]+>" "$LOG" | sed "s|.*data/||;s|>||" | sort -u)
if [ -z "$short" ]; then
  echo "No unreadable packs in the sweep log." | tee -a "$OUT"
  exit 0
fi

echo "unreadable packs: $(echo "$short" | wc -l | tr -d  )" | tee -a "$OUT"
echo "$short" | sed "s/^/  /" | tee -a "$OUT"
echo | tee -a "$OUT"

echo "resolving full pack ids..." >&2
allpacks=$(restic list packs --no-lock 2>/dev/null)
allidx=$(restic list index --no-lock 2>/dev/null)

for s in $short; do
  full=$(echo "$allpacks" | grep "^${s}" | head -1)
  [ -z "$full" ] && { echo "pack ${s}: not present in the repository listing" | tee -a "$OUT"; continue; }

  blobs=""
  while read -r i; do
    [ -z "$i" ] && continue
    b=$(restic cat index "$i" --no-lock 2>/dev/null \
        | jq -r --arg p "$full" ".packs[]? | select(.id==\$p) | .blobs[]? | .id" 2>/dev/null)
    [ -n "$b" ] && blobs="${blobs}${b}"$'\n'
  done <<< "$allidx"

  blobs=$(echo "$blobs" | grep -v "^$" | sort -u)
  n=$(echo "$blobs" | grep -c . )
  echo "=== pack ${s} — ${n} blob(s) ===" | tee -a "$OUT"

  if [ "$n" -eq 0 ]; then
    echo "  not referenced by any index: unreferenced junk, no file affected" | tee -a "$OUT"
    echo | tee -a "$OUT"
    continue
  fi

  for b in $blobs; do
    restic find --blob "$b" --no-lock 2>/dev/null \
      | grep -oE "in file /[^ ]+" | sed "s|in file ||"
  done | sort -u | sed "s|^|  |" | tee -a "$OUT"
  echo | tee -a "$OUT"
done

echo "=== distinct files with no working offsite copy ===" | tee -a "$OUT"
grep -oE "^  /.*" "$OUT" | sort -u | wc -l | tr -d " " | tee -a "$OUT"
