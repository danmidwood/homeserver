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
#
# Locate ansible-playbook and read its shebang in two separate steps rather
# than one command substitution: under `set -e`, a `command -v` that finds
# nothing feeds `head` an empty path, `head` fails, and the *pipeline's*
# failure would trip `set -e` before our own "could not locate" message is
# reached — the user would see bash's raw `head: : No such file or
# directory` instead. Splitting it lets each failure be caught and reported
# with our own message.
ANSIBLE_PLAYBOOK_PATH="$(command -v ansible-playbook || true)"
if [ -z "$ANSIBLE_PLAYBOOK_PATH" ]; then
  echo "ERROR: could not locate ansible-playbook on PATH. Cannot render prometheus.yml.j2 to validate it." >&2
  exit 1
fi
PYTHON="$(head -1 "$ANSIBLE_PLAYBOOK_PATH" 2>/dev/null | sed 's|^#!||' || true)"
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

# StrictUndefined turns a missing/renamed variable into a raised error
# during render instead of silent empty output. Without it, a variable like
# monitored_endpoints being renamed or removed from defaults/main.yml would
# render the blackbox job with an empty targets list rather than failing —
# a config that promtool would happily call valid while probing nothing.
# It does NOT catch a variable that is merely defined-but-empty (e.g.
# monitored_endpoints: []); that case is caught below, structurally.
env = jinja2.Environment(undefined=jinja2.StrictUndefined)
try:
    rendered = env.from_string(template_src).render(**variables)
except jinja2.exceptions.UndefinedError as e:
    print(f"ERROR: rendering prometheus.yml.j2 failed: {e}", file=sys.stderr)
    sys.exit(1)

with open(out_path, "w") as f:
    f.write(rendered)
PYEOF

# promtool check config treats an empty or comment-only file as
# syntactically valid ("valid prometheus config file syntax") — it has no
# opinion on content it never received, and a render that produces nothing,
# or nothing but comments, would otherwise sail through it. A prior version
# of this check tried to catch that with a `grep -qF 'scrape_configs:'`
# substring match, but a substring is not a structural claim: a render of
# the single line `# scrape_configs:` satisfies it while being a comment,
# and it cannot see that a job's static_configs declared zero targets
# either (a defined-but-empty monitored_endpoints: [] renders a valid
# `targets: []`, which StrictUndefined never touches since the variable is
# not undefined, only empty). So the check now PARSES the rendered YAML
# and asserts its actual shape, rather than guessing from its text.
if [ ! -s "$RENDERED_CONFIG" ]; then
  echo "ERROR: rendering prometheus.yml.j2 produced an empty file at $RENDERED_CONFIG. promtool treats an empty file as a valid config, so this is checked explicitly rather than left to promtool." >&2
  exit 1
fi

"$PYTHON" - "$RENDERED_CONFIG" <<'PYEOF'
import sys
import yaml

path = sys.argv[1]


def fail(assertion):
    print(f"ERROR: rendered config at {path} failed structural check: {assertion}", file=sys.stderr)
    sys.exit(1)


with open(path) as f:
    doc = yaml.safe_load(f)

if not isinstance(doc, dict):
    fail("parsed document is not a mapping (empty, a comment-only file, a scalar, or a list all parse this way)")

scrape_configs = doc.get("scrape_configs")
if not isinstance(scrape_configs, list) or len(scrape_configs) == 0:
    fail("scrape_configs is missing, not a list, or empty")

target_count = 0
for job in scrape_configs:
    if not isinstance(job, dict) or not job.get("job_name"):
        fail(f"a scrape_configs entry has no non-empty job_name: {job!r}")
    for static_config in job.get("static_configs", []):
        targets = static_config.get("targets")
        if not isinstance(targets, list) or len(targets) == 0:
            fail(f"job {job.get('job_name')!r} has a static_configs entry with an empty or missing targets list")
        target_count += len(targets)

print(f"Structural check passed: {len(scrape_configs)} scrape_configs job(s), {target_count} total static targets across all jobs")
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
