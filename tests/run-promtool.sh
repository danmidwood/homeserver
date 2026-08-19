#!/usr/bin/env bash
# Validates and unit-tests the Prometheus alert rules.
# Always runs promtool from the pinned Prometheus image, never a local
# `promtool` on PATH, so the tool version matches what is deployed exactly.
# A stale Homebrew build silently used instead would defeat the point of
# pinning.
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

promtool_run() {
  docker run --rm -v "$REPO_ROOT:/repo" -w /repo --entrypoint promtool "$IMAGE" "$@"
}

echo "==> Using promtool from pinned image $IMAGE"
promtool_run --version

echo "==> Checking rule syntax"
promtool_run check rules roles/prometheus/files/rules/*.yml

echo "==> Checking Prometheus configuration"
# prometheus.yml.j2 contains no Jinja, so it is valid promtool input as-is.
# amtool check-config against Alertmanager's config cannot run here: that
# template is Jinja containing secrets, and only its rendered form on the
# server can be checked.
promtool_run check config roles/prometheus/templates/prometheus.yml.j2

echo "==> Running rule unit tests"
failed=0
for test_file in tests/rules/*_test.yml; do
  echo "--- $test_file"
  output="$(promtool_run test rules "$test_file" 2>&1)" && status=0 || status=$?
  echo "$output"
  if [ "$status" -ne 0 ]; then
    failed=1
  fi
  if grep -q "WARNING: no file match" <<<"$output"; then
    echo "ERROR: $test_file's rule_files: entry matched no files" >&2
    failed=1
  fi
done

if [ "$failed" -ne 0 ]; then
  echo "==> Rule checks FAILED" >&2
  exit 1
fi

echo "==> All rule checks passed"
