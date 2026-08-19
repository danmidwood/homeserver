#!/usr/bin/env bash
# Validates and unit-tests the Prometheus alert rules.
# Uses a local promtool if one is installed, otherwise runs the pinned
# Prometheus image so the tool version matches what is deployed.
set -euo pipefail

# Docker Desktop on macOS does not put its CLI on the default PATH. Prepend
# it here (only if it exists) so `docker` and its credential helper can be
# found; this is a no-op on machines where docker is already on PATH.
if [ -d "/Applications/Docker.app/Contents/Resources/bin" ]; then
  PATH="/Applications/Docker.app/Contents/Resources/bin:$PATH"
fi

IMAGE="prom/prometheus:v3.14.0@sha256:5ce7540c3c00ef4ab0c9d2c995c6a5b9c421f44b4a115d97a2c7af3b1c21cbb0"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

cd "$REPO_ROOT"

if command -v promtool >/dev/null 2>&1; then
  promtool_run() { promtool "$@"; }
else
  promtool_run() {
    docker run --rm -v "$REPO_ROOT:/repo" -w /repo --entrypoint promtool "$IMAGE" "$@"
  }
fi

echo "==> Checking rule syntax"
promtool_run check rules roles/prometheus/files/rules/*.yml

echo "==> Running rule unit tests"
for test_file in tests/rules/*_test.yml; do
  echo "--- $test_file"
  promtool_run test rules "$test_file"
done

echo "==> All rule checks passed"
