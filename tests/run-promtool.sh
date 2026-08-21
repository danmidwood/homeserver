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
# prometheus.yml.j2 contains Jinja (the blackbox job's endpoint loop over
# monitored_endpoints, added when the reachability increment's Task 1
# introduced blackbox probing) so promtool cannot parse the template file
# directly as YAML — the "no Jinja" assumption this check used to rest on
# expired the moment that loop landed, and the check has been failing (and
# so validating nothing) since. Render it first with the role's own
# defaults, the same variables Ansible would supply, so this validates the
# actual deployed shape rather than an approximation of it.
#
# amtool check-config against Alertmanager's config still cannot run here:
# that template is Jinja containing secrets, and only its rendered form on
# the server can be checked.
#
# Ansible's bundled Python is used to render, not the system python3: this
# machine's system python3 has neither jinja2 nor yaml installed, while the
# interpreter behind ansible-playbook always has both (Ansible depends on
# them itself). If that interpreter can't be found or lacks jinja2, this
# fails loudly rather than skipping the check — a harness that silently
# stops validating something is how this regression went unnoticed in the
# first place.
PYTHON="$(head -1 "$(command -v ansible-playbook)" | sed 's|^#!||')"
if [ -z "$PYTHON" ] || [ ! -x "$PYTHON" ]; then
  echo "ERROR: could not locate the Python interpreter behind ansible-playbook. Cannot render prometheus.yml.j2 to validate it." >&2
  exit 1
fi
if ! "$PYTHON" -c "import jinja2, yaml" 2>/dev/null; then
  echo "ERROR: $PYTHON lacks jinja2 and/or yaml. Cannot render prometheus.yml.j2 to validate it." >&2
  exit 1
fi

RENDERED_CONFIG="$(mktemp "$REPO_ROOT/tests/.prometheus.rendered.XXXXXX")"
trap 'rm -f "$RENDERED_CONFIG"' EXIT

"$PYTHON" - "$RENDERED_CONFIG" <<'PYEOF'
import sys
import jinja2
import yaml

out_path = sys.argv[1]

with open("roles/prometheus/defaults/main.yml") as f:
    variables = yaml.safe_load(f) or {}

with open("roles/prometheus/templates/prometheus.yml.j2") as f:
    template_src = f.read()

rendered = jinja2.Environment().from_string(template_src).render(**variables)

with open(out_path, "w") as f:
    f.write(rendered)
PYEOF

promtool_run check config "${RENDERED_CONFIG#"$REPO_ROOT"/}"

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
