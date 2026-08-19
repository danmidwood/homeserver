# Alerting Increment 1 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the host-native Prometheus with a digest-pinned container, stand up Alertmanager delivering host-health alerts to Telegram, and prove the whole chain with an off-box dead-man's-switch.

**Architecture:** Prometheus and Alertmanager run as pinned containers on the existing `caddy_network`, addressing each other by container name. node-exporter stays a pacman package on the host and is scraped across the one remaining host/container boundary via `host.docker.internal`. Alert rules are static YAML files unit-tested with `promtool` before anything is deployed.

**Tech Stack:** Ansible (`community.docker`), Docker, Prometheus v3.14.0, Alertmanager v0.34.0, promtool, Telegram Bot API, Healthchecks.io

**Spec:** `docs/superpowers/specs/2026-08-19-observability-alerting-design.md`

## Global Constraints

- **Every container image is pinned by digest**, in the form `name:tag@sha256:...`. Never deploy a floating tag.
- **Prometheus image:** `prom/prometheus:v3.14.0@sha256:5ce7540c3c00ef4ab0c9d2c995c6a5b9c421f44b4a115d97a2c7af3b1c21cbb0`
- **Alertmanager image:** `prom/alertmanager:v0.34.0@sha256:690c7b525f4367aa91f73e2f91c632206d32e97c6384bdbf2fb7a861b420340d`
- **Digests must be re-verified before use** (Task 1, Step 1). If they no longer resolve, resolve the current ones and use those consistently everywhere.
- **Secrets come from `user_passwords.yml`** (gitignored) via the keys `telegram_bot_token`, `telegram_chat_id`, `heartbeat_ping_url`. Never write a real credential into any committed file, including this plan and the spec.
- **All new containers join `caddy_network`** and are absent from the Caddyfile. Nothing new is publicly exposed.
- **Prometheus TSDB lives on local disk** at `/mnt/storage/config/prometheus/data`, never in a Docker named volume (those are bind-mounted to the external array).
- **node-exporter stays native.** Do not containerise it.
- **Every role must be idempotent:** a second `ansible-playbook` run reports zero changed tasks.
- **Ansible target:** host `xps` (`xps.fritz.box`), user `daniel`, via `ansible-playbook -i inventory/hosts.ini playbooks/xps.yml`.
- **Jinja2 and Go templates collide.** Alertmanager and Prometheus use Go templating (`{{ .Labels.foo }}`, `{{ $value }}`). In any Ansible `template:` task these MUST be wrapped in `{% raw %}...{% endraw %}` or Jinja will try to evaluate them and fail. Prefer `copy:` with a static file in `files/` wherever no Ansible variable is needed.

---

## File Structure

**Created:**

| Path | Responsibility |
|---|---|
| `tests/run-promtool.sh` | Runs `promtool check`/`test` against the rules, using a local promtool if present, otherwise the pinned Prometheus image |
| `tests/rules/host_test.yml` | Unit tests for the host-health rules |
| `tests/rules/watchdog_test.yml` | Unit test asserting Watchdog always fires |
| `roles/prometheus/files/rules/host.yml` | Host-health alert rules |
| `roles/prometheus/files/rules/watchdog.yml` | The always-firing dead-man's-switch rule |
| `roles/prometheus/templates/prometheus.yml.j2` | Prometheus config: scrape targets, rule file glob, Alertmanager address |
| `roles/alertmanager/tasks/main.yml` | Alertmanager role |
| `roles/alertmanager/templates/alertmanager.yml.j2` | Routing tree and receivers |
| `roles/grafana/files/datasource-prometheus.yml` | Provisioned Grafana datasource |

**Modified:**

| Path | Change |
|---|---|
| `roles/prometheus/tasks/main.yml` | Rewritten: remove pacman package, run container, keep node-exporter native, add textfile-collector directory |
| `roles/prometheus/handlers/main.yml` | Handler restarts the container, not the service |
| `roles/prometheus/defaults/main.yml` | New file: rule thresholds |
| `roles/grafana/tasks/main.yml` | Mount the provisioning directory |
| `playbooks/xps.yml` | Add `alertmanager` before `grafana` |

**Deleted:**

| Path | Reason |
|---|---|
| `roles/prometheus/files/prometheus.yml` | Replaced by the template |

---

### Task 1: Test harness and the first alert rule

Builds the ability to test rules before building rules. Ends with one working, tested rule and nothing deployed.

**Files:**
- Create: `tests/run-promtool.sh`
- Create: `tests/rules/host_test.yml`
- Create: `roles/prometheus/files/rules/host.yml`

**Interfaces:**
- Consumes: nothing
- Produces: `tests/run-promtool.sh` — run with no arguments from the repo root; exits non-zero if any rule file is invalid or any test fails. Later tasks add files matching `roles/prometheus/files/rules/*.yml` and `tests/rules/*_test.yml`, which it picks up automatically.

- [ ] **Step 1: Verify the pinned digests still resolve**

```bash
docker buildx imagetools inspect prom/prometheus:v3.14.0 --format '{{.Manifest.Digest}}'
docker buildx imagetools inspect prom/alertmanager:v0.34.0 --format '{{.Manifest.Digest}}'
```

Expected: the two digests in Global Constraints. If they differ, upstream re-pushed the tag — use the values printed here and update Global Constraints in this plan before continuing.

- [ ] **Step 2: Write the test harness**

Create `tests/run-promtool.sh`:

```bash
#!/usr/bin/env bash
# Validates and unit-tests the Prometheus alert rules.
# Uses a local promtool if one is installed, otherwise runs the pinned
# Prometheus image so the tool version matches what is deployed.
set -euo pipefail

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
```

Make it executable:

```bash
chmod +x tests/run-promtool.sh
```

- [ ] **Step 3: Write the failing test**

Create `tests/rules/host_test.yml`. Rule file paths are resolved relative to this file, hence `../../`.

```yaml
rule_files:
  - ../../roles/prometheus/files/rules/host.yml

evaluation_interval: 1m

tests:
  # 1% free on /mnt/storage, held steady for half an hour.
  - interval: 1m
    input_series:
      - series: 'node_filesystem_avail_bytes{instance="xps.fritz.box:9100",job="node",device="/dev/sda2",fstype="ext4",mountpoint="/mnt/storage"}'
        values: '1000+0x30'
      - series: 'node_filesystem_size_bytes{instance="xps.fritz.box:9100",job="node",device="/dev/sda2",fstype="ext4",mountpoint="/mnt/storage"}'
        values: '100000+0x30'
    alert_rule_test:
      # Must NOT fire before the 15m `for:` has elapsed.
      - eval_time: 10m
        alertname: DiskSpaceCritical
        exp_alerts: []
      # Must fire once it has.
      - eval_time: 16m
        alertname: DiskSpaceCritical
        exp_alerts:
          - exp_labels:
              severity: critical
              instance: xps.fritz.box:9100
              job: node
              device: /dev/sda2
              fstype: ext4
              mountpoint: /mnt/storage

  # 40% free is healthy and must never fire.
  - interval: 1m
    input_series:
      - series: 'node_filesystem_avail_bytes{instance="xps.fritz.box:9100",job="node",device="/dev/sda2",fstype="ext4",mountpoint="/mnt/storage"}'
        values: '40000+0x30'
      - series: 'node_filesystem_size_bytes{instance="xps.fritz.box:9100",job="node",device="/dev/sda2",fstype="ext4",mountpoint="/mnt/storage"}'
        values: '100000+0x30'
    alert_rule_test:
      - eval_time: 30m
        alertname: DiskSpaceCritical
        exp_alerts: []
```

- [ ] **Step 4: Run the test to verify it fails**

Run: `./tests/run-promtool.sh`

Expected: FAIL. `promtool check rules` errors because `roles/prometheus/files/rules/host.yml` does not exist yet.

- [ ] **Step 5: Write the minimal rule**

Create `roles/prometheus/files/rules/host.yml`. This is a `files/` entry copied verbatim by Ansible, so Go template expressions need no escaping.

```yaml
groups:
  - name: host
    rules:
      - alert: DiskSpaceCritical
        expr: |
          node_filesystem_avail_bytes{fstype!~"tmpfs|ramfs|overlay|squashfs"}
            / node_filesystem_size_bytes{fstype!~"tmpfs|ramfs|overlay|squashfs"}
            < 0.05
        for: 15m
        labels:
          severity: critical
        annotations:
          summary: "{{ $labels.mountpoint }} has under 5% free"
          description: "{{ $labels.mountpoint }} on {{ $labels.instance }} is at {{ $value | humanizePercentage }} free."
```

- [ ] **Step 6: Run the test to verify it passes**

Run: `./tests/run-promtool.sh`

Expected: PASS, with `SUCCESS` reported for `tests/rules/host_test.yml`.

If instead it fails reporting an annotation mismatch, this promtool version
compares annotations as well as labels and treats an omitted `exp_annotations`
as "expect none". Add the rendered annotations to each expected alert, copying
the exact text from the failure output rather than predicting it — templated
values like `{{ $value | humanizePercentage }}` render to forms such as `1%`
that are easy to get subtly wrong:

```yaml
          - exp_labels:
              severity: critical
              instance: xps.fritz.box:9100
              job: node
              device: /dev/sda2
              fstype: ext4
              mountpoint: /mnt/storage
            exp_annotations:
              summary: "/mnt/storage has under 5% free"
              description: "/mnt/storage on xps.fritz.box:9100 is at 1% free."
```

Apply the same addition to every expected alert in every test file if so.

- [ ] **Step 7: Commit**

```bash
git add tests/run-promtool.sh tests/rules/host_test.yml roles/prometheus/files/rules/host.yml
git commit -m "Add promtool test harness and the DiskSpaceCritical rule

Rules are unit-tested before deployment, so a mistyped metric name or an
unreachable 'for:' duration is caught locally rather than by silence in
production."
```

---

### Task 2: The remaining host-health rules

**Files:**
- Modify: `roles/prometheus/files/rules/host.yml`
- Modify: `tests/rules/host_test.yml`

**Interfaces:**
- Consumes: `tests/run-promtool.sh` from Task 1
- Produces: alerts named `FilesystemReadOnly`, `DiskSpaceLow`, `DiskWillFillSoon`, `MemoryPressure`, `CPUThermalThrottle`, `HostRebooted`, each carrying a `severity` label of `critical`, `warning` or `info`. Task 4's Alertmanager routing tree matches on exactly those three `severity` values.

- [ ] **Step 1: Write the failing tests**

Append to the `tests:` list in `tests/rules/host_test.yml`:

```yaml
  # A read-only filesystem fires almost immediately.
  - interval: 1m
    input_series:
      - series: 'node_filesystem_readonly{instance="xps.fritz.box:9100",job="node",device="/dev/sda2",fstype="ext4",mountpoint="/mnt/storage"}'
        values: '1+0x10'
    alert_rule_test:
      - eval_time: 6m
        alertname: FilesystemReadOnly
        exp_alerts:
          - exp_labels:
              severity: critical
              instance: xps.fritz.box:9100
              job: node
              device: /dev/sda2
              fstype: ext4
              mountpoint: /mnt/storage

  # 12% free is low but not critical: warning fires, critical does not.
  - interval: 1m
    input_series:
      - series: 'node_filesystem_avail_bytes{instance="xps.fritz.box:9100",job="node",device="/dev/sda2",fstype="ext4",mountpoint="/mnt/storage"}'
        values: '12000+0x70'
      - series: 'node_filesystem_size_bytes{instance="xps.fritz.box:9100",job="node",device="/dev/sda2",fstype="ext4",mountpoint="/mnt/storage"}'
        values: '100000+0x70'
    alert_rule_test:
      - eval_time: 61m
        alertname: DiskSpaceLow
        exp_alerts:
          - exp_labels:
              severity: warning
              instance: xps.fritz.box:9100
              job: node
              device: /dev/sda2
              fstype: ext4
              mountpoint: /mnt/storage
      - eval_time: 61m
        alertname: DiskSpaceCritical
        exp_alerts: []

  # Under 10% memory available fires MemoryPressure.
  - interval: 1m
    input_series:
      - series: 'node_memory_MemAvailable_bytes{instance="xps.fritz.box:9100",job="node"}'
        values: '500+0x30'
      - series: 'node_memory_MemTotal_bytes{instance="xps.fritz.box:9100",job="node"}'
        values: '10000+0x30'
    alert_rule_test:
      - eval_time: 16m
        alertname: MemoryPressure
        exp_alerts:
          - exp_labels:
              severity: warning
              instance: xps.fritz.box:9100
              job: node

  # A sustained 90C package temperature fires CPUThermalThrottle.
  - interval: 1m
    input_series:
      - series: 'node_hwmon_temp_celsius{instance="xps.fritz.box:9100",job="node",chip="platform_coretemp_0",sensor="temp1"}'
        values: '90+0x20'
    alert_rule_test:
      - eval_time: 11m
        alertname: CPUThermalThrottle
        exp_alerts:
          - exp_labels:
              severity: warning
              instance: xps.fritz.box:9100
              job: node
              chip: platform_coretemp_0
              sensor: temp1

  # A box that booted two minutes ago is news; one up for days is not.
  - interval: 1m
    input_series:
      - series: 'node_time_seconds{instance="xps.fritz.box:9100",job="node"}'
        values: '1000000+60x10'
      - series: 'node_boot_time_seconds{instance="xps.fritz.box:9100",job="node"}'
        values: '999900+0x10'
    alert_rule_test:
      - eval_time: 1m
        alertname: HostRebooted
        exp_alerts:
          - exp_labels:
              severity: info
              instance: xps.fritz.box:9100
              job: node
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `./tests/run-promtool.sh`

Expected: FAIL, reporting unexpected-empty alert results for `FilesystemReadOnly`, `DiskSpaceLow`, `MemoryPressure`, `CPUThermalThrottle` and `HostRebooted`, because none of those rules exist yet.

- [ ] **Step 3: Add the rules**

Append to the `rules:` list in `roles/prometheus/files/rules/host.yml`:

```yaml
      - alert: FilesystemReadOnly
        expr: node_filesystem_readonly{fstype!~"tmpfs|ramfs|overlay|squashfs"} == 1
        for: 5m
        labels:
          severity: critical
        annotations:
          summary: "{{ $labels.mountpoint }} has gone read-only"
          description: "A filesystem remounting itself read-only is usually the first visible sign of a failing disk. Check dmesg on {{ $labels.instance }}."

      - alert: DiskSpaceLow
        expr: |
          node_filesystem_avail_bytes{fstype!~"tmpfs|ramfs|overlay|squashfs"}
            / node_filesystem_size_bytes{fstype!~"tmpfs|ramfs|overlay|squashfs"}
            < 0.15
        for: 1h
        labels:
          severity: warning
        annotations:
          summary: "{{ $labels.mountpoint }} has under 15% free"
          description: "{{ $labels.mountpoint }} on {{ $labels.instance }} is at {{ $value | humanizePercentage }} free."

      - alert: DiskWillFillSoon
        expr: |
          predict_linear(
            node_filesystem_avail_bytes{fstype!~"tmpfs|ramfs|overlay|squashfs"}[6h],
            4 * 24 * 3600
          ) < 0
        for: 1h
        labels:
          severity: warning
        annotations:
          summary: "{{ $labels.mountpoint }} is projected to fill within 4 days"
          description: "Based on the last 6 hours of usage, {{ $labels.mountpoint }} on {{ $labels.instance }} runs out of space within 4 days."

      - alert: MemoryPressure
        expr: |
          node_memory_MemAvailable_bytes / node_memory_MemTotal_bytes < 0.10
        for: 15m
        labels:
          severity: warning
        annotations:
          summary: "Under 10% memory available"
          description: "{{ $labels.instance }} has {{ $value | humanizePercentage }} of memory available."

      - alert: CPUThermalThrottle
        expr: node_hwmon_temp_celsius > 85
        for: 10m
        labels:
          severity: warning
        annotations:
          summary: "CPU running at {{ $value }}C"
          description: "The server is a laptop; sustained high temperature suggests blocked airflow or a failing fan."

      - alert: HostRebooted
        expr: (node_time_seconds - node_boot_time_seconds) < 300
        labels:
          severity: info
        annotations:
          summary: "{{ $labels.instance }} rebooted"
          description: "Uptime is under five minutes. A server that reboots on its own is worth knowing about."
```

Note there is no `for:` on `HostRebooted` — the condition is only true for the first five minutes, so a `for:` duration would make it unreachable. This is exactly the class of bug the tests exist to catch.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `./tests/run-promtool.sh`

Expected: PASS for all test groups.

Note `DiskWillFillSoon` has no unit test. `predict_linear` over a 6-hour window needs 6 hours of synthetic samples to exercise meaningfully, which makes for a slow, brittle test of a rule whose logic is a single library call. `promtool check rules` still validates its syntax. This is a deliberate omission, not an oversight.

- [ ] **Step 5: Commit**

```bash
git add roles/prometheus/files/rules/host.yml tests/rules/host_test.yml
git commit -m "Add the remaining host-health alert rules

Filesystem read-only, low disk, projected disk exhaustion, memory
pressure, thermal throttling and unexpected reboot, each unit-tested
except the predict_linear projection."
```

---

### Task 3: The Watchdog rule

The dead-man's-switch. An alert that always fires, so that its *absence* downstream is the signal.

**Files:**
- Create: `roles/prometheus/files/rules/watchdog.yml`
- Create: `tests/rules/watchdog_test.yml`

**Interfaces:**
- Consumes: `tests/run-promtool.sh` from Task 1
- Produces: an alert named `Watchdog` carrying the label `severity: none`. Task 4's routing tree matches `alertname="Watchdog"` and must match it before any severity-based route.

- [ ] **Step 1: Write the failing test**

Create `tests/rules/watchdog_test.yml`:

```yaml
rule_files:
  - ../../roles/prometheus/files/rules/watchdog.yml

evaluation_interval: 1m

tests:
  # No input series at all: Watchdog must still fire, because its whole
  # purpose is to be independent of anything being scraped.
  - interval: 1m
    input_series: []
    alert_rule_test:
      - eval_time: 1m
        alertname: Watchdog
        exp_alerts:
          - exp_labels:
              severity: none
      - eval_time: 30m
        alertname: Watchdog
        exp_alerts:
          - exp_labels:
              severity: none
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `./tests/run-promtool.sh`

Expected: FAIL. `promtool check rules` errors because `roles/prometheus/files/rules/watchdog.yml` does not exist.

- [ ] **Step 3: Write the rule**

Create `roles/prometheus/files/rules/watchdog.yml`:

```yaml
groups:
  - name: watchdog
    rules:
      # Always fires, by construction. Routed to a webhook that pings
      # Healthchecks.io; if Prometheus stops evaluating, Alertmanager stops
      # routing, or the machine dies, the pings stop and Healthchecks
      # raises the alarm from outside the server. This is the only rule
      # that can report the failure of the monitoring itself.
      - alert: Watchdog
        expr: vector(1)
        labels:
          severity: none
        annotations:
          summary: "Monitoring pipeline heartbeat"
          description: "This alert is always firing. If it stops arriving at Healthchecks.io, the monitoring chain or the host itself has failed."
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `./tests/run-promtool.sh`

Expected: PASS for both `host_test.yml` and `watchdog_test.yml`.

- [ ] **Step 5: Commit**

```bash
git add roles/prometheus/files/rules/watchdog.yml tests/rules/watchdog_test.yml
git commit -m "Add the Watchdog dead-man's-switch rule

An always-firing alert routed to an off-box heartbeat, so that the
failure of the monitoring stack or the machine itself is detectable."
```

---

### Task 4: The Alertmanager role

**Files:**
- Create: `roles/alertmanager/tasks/main.yml`
- Create: `roles/alertmanager/templates/alertmanager.yml.j2`
- Create: `roles/alertmanager/handlers/main.yml`

**Interfaces:**
- Consumes: the `severity` label values `critical`, `warning`, `info`, `none` produced by Tasks 2 and 3; the secrets `telegram_bot_token`, `telegram_chat_id`, `heartbeat_ping_url` from `user_passwords.yml`
- Produces: a container named `alertmanager` on `caddy_network`, reachable as `alertmanager:9093` by other containers and on `:9093` from the LAN. Task 5's `prometheus.yml.j2` points its `alertmanagers:` block at that name.

- [ ] **Step 1: Write the config template**

Create `roles/alertmanager/templates/alertmanager.yml.j2`. The `{% raw %}` blocks are mandatory: everything inside them is Go template syntax that Jinja must not touch.

```yaml
global:
  resolve_timeout: 5m

templates: []

route:
  receiver: telegram-warning
  group_by: ['alertname']
  group_wait: 30s
  group_interval: 5m
  repeat_interval: 24h

  routes:
    # The Watchdog is matched first and never reaches Telegram.
    - matchers:
        - alertname = "Watchdog"
      receiver: heartbeat
      group_wait: 0s
      group_interval: 1m
      repeat_interval: 5m
      continue: false

    - matchers:
        - severity = "critical"
      receiver: telegram-critical
      group_wait: 30s
      group_interval: 5m
      repeat_interval: 4h

    - matchers:
        - severity = "info"
      receiver: telegram-info
      group_wait: 5m
      group_interval: 1h
      repeat_interval: 168h

receivers:
  - name: heartbeat
    webhook_configs:
      - url: {{ heartbeat_ping_url }}
        send_resolved: false

  - name: telegram-critical
    telegram_configs:
      - bot_token_file: /etc/alertmanager/telegram_token
        chat_id: {{ telegram_chat_id }}
        parse_mode: HTML
        send_resolved: true
        message: |
{% raw %}
          {{ if eq .Status "firing" }}🔴 <b>CRITICAL</b>{{ else }}✅ <b>RESOLVED</b>{{ end }}
          {{ range .Alerts }}
          <b>{{ .Labels.alertname }}</b>
          {{ .Annotations.summary }}
          {{ .Annotations.description }}
          {{ end }}
{% endraw %}

  - name: telegram-warning
    telegram_configs:
      - bot_token_file: /etc/alertmanager/telegram_token
        chat_id: {{ telegram_chat_id }}
        parse_mode: HTML
        send_resolved: true
        message: |
{% raw %}
          {{ if eq .Status "firing" }}⚠️ <b>WARNING</b>{{ else }}✅ <b>RESOLVED</b>{{ end }}
          {{ range .Alerts }}
          <b>{{ .Labels.alertname }}</b>
          {{ .Annotations.summary }}
          {{ .Annotations.description }}
          {{ end }}
{% endraw %}

  - name: telegram-info
    telegram_configs:
      - bot_token_file: /etc/alertmanager/telegram_token
        chat_id: {{ telegram_chat_id }}
        parse_mode: HTML
        send_resolved: false
        message: |
{% raw %}
          ℹ️ <b>INFO</b>
          {{ range .Alerts }}
          <b>{{ .Labels.alertname }}</b>
          {{ .Annotations.summary }}
          {{ end }}
{% endraw %}
```

- [ ] **Step 2: Write the role tasks**

Create `roles/alertmanager/tasks/main.yml`:

```yaml
- name: Ensure Telegram and heartbeat credentials are configured
  ansible.builtin.assert:
    that:
      - telegram_bot_token | default('') | length > 0
      - telegram_chat_id | default('') | length > 0
      - heartbeat_ping_url | default('') | length > 0
    fail_msg: "telegram_bot_token, telegram_chat_id and heartbeat_ping_url must all be set in user_passwords.yml"

- name: Create Alertmanager config directory
  ansible.builtin.file:
    path: /mnt/storage/config/alertmanager
    state: directory
    owner: root
    group: root
    mode: '0755'

- name: Create Alertmanager data directory
  ansible.builtin.file:
    path: /mnt/storage/config/alertmanager/data
    state: directory
    # Upstream image runs as uid 65534 (nobody); it must own its silence
    # and notification-log state or it fails to start.
    owner: '65534'
    group: '65534'
    mode: '0755'

- name: Write the Telegram bot token
  ansible.builtin.copy:
    content: "{{ telegram_bot_token }}"
    dest: /mnt/storage/config/alertmanager/telegram_token
    owner: '65534'
    group: '65534'
    mode: '0600'
  no_log: true
  notify: Restart Alertmanager

- name: Template Alertmanager configuration
  ansible.builtin.template:
    src: alertmanager.yml.j2
    dest: /mnt/storage/config/alertmanager/alertmanager.yml
    owner: '65534'
    group: '65534'
    mode: '0640'
  notify: Restart Alertmanager

- name: Pull the Alertmanager image
  community.docker.docker_image_pull:
    name: "prom/alertmanager:v0.34.0@sha256:690c7b525f4367aa91f73e2f91c632206d32e97c6384bdbf2fb7a861b420340d"
    pull: not_present

- name: Run Alertmanager container
  community.docker.docker_container:
    name: alertmanager
    image: "prom/alertmanager:v0.34.0@sha256:690c7b525f4367aa91f73e2f91c632206d32e97c6384bdbf2fb7a861b420340d"
    state: started
    restart_policy: always
    networks:
      - name: caddy_network
    ports:
      - "9093:9093"
    volumes:
      - /mnt/storage/config/alertmanager/alertmanager.yml:/etc/alertmanager/alertmanager.yml:ro
      - /mnt/storage/config/alertmanager/telegram_token:/etc/alertmanager/telegram_token:ro
      - /mnt/storage/config/alertmanager/data:/alertmanager
    command:
      - "--config.file=/etc/alertmanager/alertmanager.yml"
      - "--storage.path=/alertmanager"
```

Create `roles/alertmanager/handlers/main.yml`:

```yaml
- name: Restart Alertmanager
  community.docker.docker_container:
    name: alertmanager
    state: started
    restart: true
```

- [ ] **Step 3: Add the role to the playbook**

In `playbooks/xps.yml`, insert `alertmanager` immediately before `grafana` in the `roles:` list, so it reads:

```yaml
    - caddy
    - plex
    - alertmanager
    - grafana
```

`grafana` depends on `prometheus` via `roles/grafana/meta/main.yml`, so placing `alertmanager` ahead of it means the whole metrics stack comes up in a sensible order.

- [ ] **Step 4: Deploy and validate the configuration**

```bash
ansible-playbook -i inventory/hosts.ini playbooks/xps.yml --tags all
```

Then on the server, validate what actually landed:

```bash
ssh daniel@xps.fritz.box \
  'docker run --rm -v /mnt/storage/config/alertmanager:/cfg:ro \
     --entrypoint amtool \
     prom/alertmanager:v0.34.0@sha256:690c7b525f4367aa91f73e2f91c632206d32e97c6384bdbf2fb7a861b420340d \
     check-config /cfg/alertmanager.yml'
```

Expected: `SUCCESS` with the receivers listed and no parse errors.

If it reports an unknown field `bot_token_file`, this Alertmanager version predates it. Fall back to inlining `bot_token: {{ telegram_bot_token }}` in each `telegram_configs` block, drop the token file and its volume mount, and keep the rendered config at mode `0600`.

- [ ] **Step 5: Prove the delivery chain end to end**

**Prerequisite:** send `/start` to the bot from your own Telegram account first. Bots cannot open a conversation, so until you do this, delivery fails with "chat not found" despite a valid token and chat id.

```bash
ssh daniel@xps.fritz.box \
  'docker exec alertmanager amtool --alertmanager.url=http://localhost:9093 \
     alert add TestAlert severity=critical \
     --annotation=summary="Delivery chain test" \
     --annotation=description="Sent by hand from amtool during increment 1."'
```

Expected: a Telegram message arrives within about 30 seconds, formatted with the 🔴 CRITICAL header.

If nothing arrives, check `docker logs alertmanager` — the Telegram API reports the reason (`chat not found`, `unauthorized`) in the log line.

- [ ] **Step 6: Verify idempotency**

```bash
ansible-playbook -i inventory/hosts.ini playbooks/xps.yml
```

Expected: `changed=0` for every task in the `alertmanager` role.

- [ ] **Step 7: Commit**

```bash
git add roles/alertmanager playbooks/xps.yml
git commit -m "Add Alertmanager with Telegram delivery and heartbeat webhook

Routing tree sends critical, warning and info severities to Telegram
with distinct repeat intervals, and diverts the Watchdog alert to a
Healthchecks.io webhook. The bot token is supplied via a file rather
than inlined so it stays out of config parse errors in the log."
```

---

### Task 5: Containerise Prometheus

The one irreversible task. The pacman package and its data are removed; the spec records that neither is wanted.

**Files:**
- Modify: `roles/prometheus/tasks/main.yml` (full rewrite)
- Modify: `roles/prometheus/handlers/main.yml`
- Create: `roles/prometheus/templates/prometheus.yml.j2`
- Delete: `roles/prometheus/files/prometheus.yml`

**Interfaces:**
- Consumes: `alertmanager:9093` from Task 4; the rule files from Tasks 1–3
- Produces: a container named `prometheus` on `caddy_network`, reachable as `prometheus:9090` by other containers and on `:9090` from the LAN. Task 6's Grafana datasource points at that name. Also produces the textfile-collector directory `/var/lib/node_exporter/textfile_collector`, which increments 2 and 3 write into.

- [ ] **Step 1: Write the config template**

Create `roles/prometheus/templates/prometheus.yml.j2`:

```yaml
global:
  scrape_interval: 15s
  evaluation_interval: 15s

alerting:
  alertmanagers:
    - static_configs:
        - targets: ["alertmanager:9093"]

rule_files:
  - /etc/prometheus/rules/*.yml

scrape_configs:
  - job_name: "prometheus"
    static_configs:
      - targets: ["localhost:9090"]

  # node-exporter stays native on the host, so this is the one place the
  # container/host boundary is crossed. host.docker.internal is provided by
  # the etc_hosts entry on the container (Ansible's name for --add-host).
  - job_name: "node"
    static_configs:
      - targets: ["host.docker.internal:9100"]
```

- [ ] **Step 2: Rewrite the role**

Replace the entire contents of `roles/prometheus/tasks/main.yml`:

```yaml
- name: Check whether the packaged Prometheus unit is present
  ansible.builtin.stat:
    path: /usr/lib/systemd/system/prometheus.service
  register: packaged_prometheus_unit

- name: Stop and disable the packaged Prometheus
  ansible.builtin.systemd:
    name: prometheus
    state: stopped
    enabled: false
  when: packaged_prometheus_unit.stat.exists

- name: Remove the packaged Prometheus
  # Replaced by a digest-pinned container; a rolling-release package would
  # move underneath us on every pacman -Syu.
  community.general.pacman:
    name: prometheus
    state: absent

- name: Remove the packaged Prometheus configuration directory
  ansible.builtin.file:
    path: /etc/prometheus
    state: absent

- name: Ensure node-exporter is installed
  ansible.builtin.package:
    name: prometheus-node-exporter
    state: present

- name: Create the node-exporter textfile collector directory
  ansible.builtin.file:
    path: /var/lib/node_exporter/textfile_collector
    state: directory
    owner: root
    group: root
    mode: '0755'

- name: Enable the node-exporter textfile collector
  ansible.builtin.copy:
    content: |
      NODE_EXPORTER_ARGS="--collector.textfile.directory=/var/lib/node_exporter/textfile_collector"
    dest: /etc/conf.d/prometheus-node-exporter
    owner: root
    group: root
    mode: '0644'
  notify: Restart node-exporter

- name: Start and enable node-exporter
  ansible.builtin.systemd:
    name: prometheus-node-exporter
    enabled: true
    state: started
    daemon_reload: true

- name: Create Prometheus config directory
  ansible.builtin.file:
    path: /mnt/storage/config/prometheus
    state: directory
    owner: root
    group: root
    mode: '0755'

- name: Create Prometheus rules directory
  ansible.builtin.file:
    path: /mnt/storage/config/prometheus/rules
    state: directory
    owner: root
    group: root
    mode: '0755'

- name: Create Prometheus data directory
  ansible.builtin.file:
    # On local disk deliberately: Docker named volumes are bind-mounted to
    # the external array, and Prometheus writes continuously.
    path: /mnt/storage/config/prometheus/data
    state: directory
    owner: '65534'
    group: '65534'
    mode: '0755'

- name: Template Prometheus configuration
  ansible.builtin.template:
    src: prometheus.yml.j2
    dest: /mnt/storage/config/prometheus/prometheus.yml
    owner: root
    group: root
    mode: '0644'
  notify: Restart Prometheus

- name: Copy Prometheus alert rules
  ansible.builtin.copy:
    src: "{{ item }}"
    dest: "/mnt/storage/config/prometheus/rules/{{ item | basename }}"
    owner: root
    group: root
    mode: '0644'
  with_fileglob:
    - "rules/*.yml"
  notify: Restart Prometheus

- name: Pull the Prometheus image
  community.docker.docker_image_pull:
    name: "prom/prometheus:v3.14.0@sha256:5ce7540c3c00ef4ab0c9d2c995c6a5b9c421f44b4a115d97a2c7af3b1c21cbb0"
    pull: not_present

- name: Run Prometheus container
  community.docker.docker_container:
    name: prometheus
    image: "prom/prometheus:v3.14.0@sha256:5ce7540c3c00ef4ab0c9d2c995c6a5b9c421f44b4a115d97a2c7af3b1c21cbb0"
    state: started
    restart_policy: always
    networks:
      - name: caddy_network
    ports:
      - "9090:9090"
    etc_hosts:
      host.docker.internal: host-gateway
    volumes:
      - /mnt/storage/config/prometheus/prometheus.yml:/etc/prometheus/prometheus.yml:ro
      - /mnt/storage/config/prometheus/rules:/etc/prometheus/rules:ro
      - /mnt/storage/config/prometheus/data:/prometheus
    command:
      - "--config.file=/etc/prometheus/prometheus.yml"
      - "--storage.tsdb.path=/prometheus"
```

Replace `roles/prometheus/handlers/main.yml`:

```yaml
- name: Restart Prometheus
  community.docker.docker_container:
    name: prometheus
    state: started
    restart: true

- name: Restart node-exporter
  ansible.builtin.systemd:
    name: prometheus-node-exporter
    state: restarted
```

Delete the obsolete static config:

```bash
git rm roles/prometheus/files/prometheus.yml
```

- [ ] **Step 3: Deploy**

```bash
ansible-playbook -i inventory/hosts.ini playbooks/xps.yml
```

- [ ] **Step 4: Verify the node-exporter flag actually took**

The Arch unit is expected to read `$NODE_EXPORTER_ARGS` from `/etc/conf.d/prometheus-node-exporter`. Confirm rather than assume:

```bash
ssh daniel@xps.fritz.box 'systemctl cat prometheus-node-exporter | grep -E "ExecStart|EnvironmentFile"'
ssh daniel@xps.fritz.box 'systemctl show prometheus-node-exporter -p ExecStart | grep textfile'
```

Expected: the `ExecStart` line references `$NODE_EXPORTER_ARGS`, and the resolved `ExecStart` contains `--collector.textfile.directory`.

If the unit does not reference that variable, replace the "Enable the node-exporter textfile collector" task with a systemd drop-in instead:

```yaml
- name: Create the node-exporter drop-in directory
  ansible.builtin.file:
    path: /etc/systemd/system/prometheus-node-exporter.service.d
    state: directory
    owner: root
    group: root
    mode: '0755'

- name: Enable the node-exporter textfile collector
  ansible.builtin.copy:
    content: |
      [Service]
      ExecStart=
      ExecStart=/usr/bin/prometheus-node-exporter --collector.textfile.directory=/var/lib/node_exporter/textfile_collector
    dest: /etc/systemd/system/prometheus-node-exporter.service.d/textfile.conf
    owner: root
    group: root
    mode: '0644'
  notify: Restart node-exporter
```

- [ ] **Step 5: Verify Prometheus is healthy and wired to Alertmanager**

```bash
# Both scrape targets up.
curl -s http://xps.fritz.box:9090/api/v1/targets | jq '.data.activeTargets[] | {job: .labels.job, health}'

# Rules loaded.
curl -s http://xps.fritz.box:9090/api/v1/rules | jq '.data.groups[].name'

# Alertmanager discovered.
curl -s http://xps.fritz.box:9090/api/v1/alertmanagers | jq '.data.activeAlertmanagers'
```

Expected: both `prometheus` and `node` targets report `health: "up"`; the rule groups `host` and `watchdog` are listed; one active Alertmanager at `http://alertmanager:9093/api/v2/alerts`.

- [ ] **Step 6: Verify the Watchdog is reaching Healthchecks**

Open the Healthchecks.io dashboard. Expected: the check has received a ping within the last five minutes and shows as up.

If it has not, check `docker logs alertmanager` for webhook errors, and confirm the Watchdog alert is firing:

```bash
curl -s http://xps.fritz.box:9090/api/v1/alerts | jq '.data.alerts[] | select(.labels.alertname=="Watchdog")'
```

- [ ] **Step 7: Verify idempotency**

```bash
ansible-playbook -i inventory/hosts.ini playbooks/xps.yml
```

Expected: `changed=0` across the `prometheus` role. The pacman removal task is the one most likely to misreport; if it shows changed on a second run, the package name is wrong.

- [ ] **Step 8: Commit**

```bash
git add roles/prometheus playbooks/xps.yml
git commit -m "Replace the packaged Prometheus with a pinned container

Removes the pacman package, whose rolling-release updates sat outside
the digest-pinning convention, and runs Prometheus as a container on
caddy_network with its TSDB on local disk. node-exporter stays native
and gains a textfile collector directory for later increments."
```

---

### Task 6: Provision the Grafana datasource

Moves the datasource out of Grafana's database and into git, which is only possible now that Prometheus is addressable by container name.

**Files:**
- Create: `roles/grafana/files/datasource-prometheus.yml`
- Modify: `roles/grafana/tasks/main.yml`

**Interfaces:**
- Consumes: `prometheus:9090` from Task 5
- Produces: nothing consumed by later tasks

- [ ] **Step 1: Write the provisioning file**

Create `roles/grafana/files/datasource-prometheus.yml`:

```yaml
apiVersion: 1

# Removes any hand-created datasource of the same name first; provisioning
# fails outright if a datasource with this name already exists under a
# different uid, which it will, because this one was created by hand.
deleteDatasources:
  - name: Prometheus
    orgId: 1

datasources:
  - name: Prometheus
    uid: prometheus
    type: prometheus
    access: proxy
    orgId: 1
    url: http://prometheus:9090
    isDefault: true
    editable: false
```

- [ ] **Step 2: Mount it into the container**

In `roles/grafana/tasks/main.yml`, add before the "Run Grafana container" task:

```yaml
- name: Create Grafana datasource provisioning directory
  ansible.builtin.file:
    path: /mnt/storage/config/grafana/provisioning/datasources
    state: directory
    owner: root
    group: root
    mode: '0755'

- name: Provision the Prometheus datasource
  ansible.builtin.copy:
    src: datasource-prometheus.yml
    dest: /mnt/storage/config/grafana/provisioning/datasources/prometheus.yml
    owner: root
    group: root
    mode: '0644'
  notify: Restart Grafana
```

Add the volume mount to the existing "Run Grafana container" task's `volumes:` list:

```yaml
    volumes:
      - grafana_data:/var/lib/grafana
      - /mnt/storage/config/grafana/provisioning/datasources:/etc/grafana/provisioning/datasources:ro
```

Create `roles/grafana/handlers/main.yml`:

```yaml
- name: Restart Grafana
  community.docker.docker_container:
    name: grafana
    state: started
    restart: true
```

- [ ] **Step 3: Deploy**

```bash
ansible-playbook -i inventory/hosts.ini playbooks/xps.yml
```

- [ ] **Step 4: Verify the datasource works**

Open `https://grafana.home.danmidwood.com`, go to Connections → Data sources → Prometheus, and click "Save & test".

Expected: "Successfully queried the Prometheus API." The datasource shows as provisioned and its fields are not editable.

Then confirm a query returns data — in Explore, run `up`. Expected: two series, one per scrape job.

- [ ] **Step 5: Verify idempotency**

```bash
ansible-playbook -i inventory/hosts.ini playbooks/xps.yml
```

Expected: `changed=0` across the `grafana` role.

- [ ] **Step 6: Commit**

```bash
git add roles/grafana
git commit -m "Provision the Grafana Prometheus datasource from a file

Moves the datasource definition out of Grafana's database and into
version control, now that Prometheus is reachable by container name."
```

---

### Task 7: Fault-injection verification

Everything so far proves the happy path. This proves the alerts actually fire on real conditions and, just as importantly, that they clear afterwards.

**Files:**
- Modify: `docs/superpowers/specs/2026-08-19-observability-alerting-design.md` (record increment 1 as delivered)

**Interfaces:**
- Consumes: the deployed stack from Tasks 4–6
- Produces: nothing

- [ ] **Step 1: Confirm a real alert fires and resolves**

`MemoryPressure` is the safest rule to provoke, because it reverses instantly
and needs no disk manipulation.

Do not provoke it by actually exhausting memory — that risks the OOM killer
choosing one of the real services. Lower the bar instead of raising the load,
by editing the *deployed* rule on the server only. The repository copy stays
untouched, so the next Ansible run restores the real threshold:

```bash
ssh daniel@xps.fritz.box \
  'sudo sed -i "s|node_memory_MemTotal_bytes < 0.10|node_memory_MemTotal_bytes < 0.99|" \
     /mnt/storage/config/prometheus/rules/host.yml && \
   docker kill -s HUP prometheus'
```

Expected: within roughly 15 minutes plus the 30-second group wait, a ⚠️ WARNING Telegram message for `MemoryPressure`.

- [ ] **Step 2: Confirm it resolves**

Revert the deployed rule by re-running Ansible, which restores the real threshold:

```bash
ansible-playbook -i inventory/hosts.ini playbooks/xps.yml
```

Expected: a ✅ RESOLVED Telegram message follows. An alert that fires but never clears trains the reader to ignore it, so this half matters as much as the first.

- [ ] **Step 3: Confirm the heartbeat fails closed**

```bash
ssh daniel@xps.fritz.box 'docker stop alertmanager'
```

Wait for the Healthchecks grace period to elapse.

Expected: Healthchecks.io emails to say the check is down. This is the single most important verification in the increment — it proves that the failure of the monitoring itself is detectable.

```bash
ssh daniel@xps.fritz.box 'docker start alertmanager'
```

Expected: Healthchecks returns to up within five minutes.

- [ ] **Step 4: Record the increment as delivered**

In the spec's build-order table, mark increment 1 complete by appending ` — delivered 2026-08-19` to its row.

- [ ] **Step 5: Commit**

```bash
git add docs/superpowers/specs/2026-08-19-observability-alerting-design.md
git commit -m "Mark alerting increment 1 as delivered

Fault injection confirmed MemoryPressure fires and resolves, and that
stopping Alertmanager trips the off-box heartbeat."
```

---

## What increment 1 deliberately leaves out

Each has its own plan, written once this one has landed:

| Increment | Content |
|---|---|
| 2 | Backup integrity: textfile metrics from `restic-backup.sh`, `BackupStale`, `BackupMetricMissing`, `OnFailure=` handler |
| 3 | Disk health: smartmontools, the SMART textfile script and timer, SMART rules |
| 4 | Container health: cAdvisor and its rules, templated from `monitored_containers` |
| 5 | Reachability and TLS: blackbox exporter and its rules, templated from `monitored_endpoints` |
| 6 | Diun image-update notifications |

The `TargetDown` meta-rule is deferred to increment 4, since with only two scrape targets — both of which would take Prometheus itself down with them — it has nothing useful to watch until cAdvisor and blackbox exist.
