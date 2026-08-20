#!/usr/bin/env bash
# Validates the provisioned Grafana dashboards.
#
# A dashboard with a bad datasource uid still deploys cleanly and still
# appears in the UI — it just renders "Datasource not found" on every panel.
# That failure has happened here before, so it is worth catching in the repo
# rather than in the browser.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

DASHBOARD_DIR="roles/grafana/files/dashboards"
# Must match the uid in roles/grafana/files/datasource-prometheus.yml.
EXPECTED_DS_UID="prometheus"

echo "==> Checking dashboards in $DASHBOARD_DIR"

shopt -s nullglob
dashboards=("$DASHBOARD_DIR"/*.json)
if [ ${#dashboards[@]} -eq 0 ]; then
  echo "ERROR: no dashboards found in $DASHBOARD_DIR" >&2
  exit 1
fi

failed=0
for dashboard in "${dashboards[@]}"; do
  echo "--- $dashboard"
  if ! python3 - "$dashboard" "$EXPECTED_DS_UID" <<'PY'
import json, sys

path, expected_uid = sys.argv[1], sys.argv[2]

try:
    with open(path) as handle:
        dash = json.load(handle)
except json.JSONDecodeError as exc:
    print(f"  INVALID JSON: {exc}")
    sys.exit(1)

problems = []

# A provisioned dashboard must not carry a numeric id: Grafana assigns its
# own, and an id exported from another instance can collide with an
# existing dashboard.
if dash.get("id") is not None:
    problems.append(f"carries id {dash['id']!r}; provisioned dashboards must omit it")

if not dash.get("uid"):
    problems.append("has no uid, so every provisioning run recreates it")

panels = dash.get("panels", [])
if not panels:
    problems.append("has no panels")

# Walk every datasource reference, including those nested inside targets.
def walk(node, where):
    if isinstance(node, dict):
        ds = node.get("datasource")
        if isinstance(ds, dict) and "uid" in ds and ds["uid"] != expected_uid:
            problems.append(f"{where}: datasource uid {ds['uid']!r}, expected {expected_uid!r}")
        elif isinstance(ds, str) and ds != expected_uid:
            problems.append(f"{where}: legacy string datasource {ds!r}, expected {expected_uid!r}")
        for key, value in node.items():
            walk(value, f"{where}.{key}")
    elif isinstance(node, list):
        for index, value in enumerate(node):
            walk(value, f"{where}[{index}]")

for index, panel in enumerate(panels):
    walk(panel, f"panel[{index}] {panel.get('title', 'untitled')!r}")

if problems:
    for problem in problems:
        print(f"  FAIL: {problem}")
    sys.exit(1)

print(f"  OK: {len(panels)} panels, uid {dash['uid']!r}, all datasources {expected_uid!r}")
PY
  then
    failed=1
  fi
done

if [ "$failed" -ne 0 ]; then
  echo "==> Dashboard checks FAILED" >&2
  exit 1
fi

echo "==> All dashboard checks passed"
