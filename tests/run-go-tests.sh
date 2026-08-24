#!/usr/bin/env bash
# Runs the Telegram bot's Go tests. Offline: every test uses httptest, so no
# network, no real bot token, and no server access are needed.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SRC="$REPO_ROOT/roles/telegrambot/files/src"

echo "==> go vet"
(cd "$SRC" && GOPROXY=off go vet ./...)

echo "==> go test"
# -count=1 defeats the build cache. Without it a mutation-tested change can
# report a cached pass from before the edit, which looks exactly like a test
# that failed to catch the bug.
(cd "$SRC" && GOPROXY=off go test -count=1 ./...)

echo "==> confirming there are no third-party dependencies"
if grep -q "^require" "$SRC/go.mod"; then
  echo "  FAIL: go.mod has a require block; this project is stdlib-only"
  exit 1
fi
if [ -f "$SRC/go.sum" ]; then
  echo "  FAIL: go.sum exists; this project is stdlib-only"
  exit 1
fi
echo "  OK: stdlib only"
