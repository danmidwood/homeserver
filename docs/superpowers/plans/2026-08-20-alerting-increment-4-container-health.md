# Alerting Increment 4: Container Health Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Find out when a container dies, restart-loops or is OOM-killed, instead of discovering it when a service does not respond.

**Architecture:** cAdvisor runs as a digest-pinned container publishing per-container metrics for Prometheus to scrape. The list of containers that *ought* to be running is written by Ansible into node-exporter's textfile directory as a metric, so the rules stay static and testable rather than being templated. Five rules watch the pair.

**Tech Stack:** Ansible, Docker, cAdvisor v0.55.1, Prometheus, node-exporter textfile collector, promtool

**Spec:** `docs/superpowers/specs/2026-08-19-observability-alerting-design.md` (the "Container health — cAdvisor" section)

## Global Constraints

- **Every container image is pinned by digest**, form `name:tag@sha256:...`. Never deploy a floating tag.
- **cAdvisor image:** `gcr.io/cadvisor/cadvisor:v0.55.1@sha256:3de2bd5203120b866d74a9b283b2ffb8ec382fbf9dc321814700c6ea6f44ec57`
- **Alert `severity` must be exactly one of** `critical`, `warning`, `info`, `none`. The deployed Alertmanager routes on precisely these; anything else is silently delivered mislabelled as WARNING via the default route.
- **Rule files live in `roles/prometheus/files/rules/`** and are copied verbatim by Ansible, so Go template expressions in annotations (`{{ $labels.name }}`, `{{ $value }}`) need NO escaping. Files under `templates/` are Jinja-rendered and DO need `{% raw %}`.
- **Never put a rule file under `templates/`.** The test harness globs `roles/prometheus/files/rules/*.yml`; a templated rule would be untested and unvalidatable. This is why the expected-container list is a metric rather than templated rule text.
- **Beware `{#` in any `.j2` file.** Jinja reads it as a comment opener and raises `TemplateSyntaxError`. `${#arr[@]}` in a shell template is the trap; `${!arr[@]}` and `{name=` are fine.
- **Every rule gets a unit test asserting both firing AND silence.** A test asserting only firing cannot catch a rule mutated to fire always.
- **The test harness is `./tests/run-promtool.sh`**, run from the repo root.
- **Roles must be idempotent:** a second `ansible-playbook -i inventory/hosts.ini playbooks/xps.yml` reports zero changed tasks.
- **Never write a credential into a committed file.**
- **Ansible target:** host `xps` (`xps.fritz.box`), user `daniel`.
- Commit messages end with: `Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>`

---

## Verified facts this plan is built on

Gathered from the live host. Do not re-derive them, but re-verify anything that looks wrong.

| Fact | Value |
|---|---|
| Containers running | 16, listed in Task 1 Step 2 |
| Docker / kernel | 27.3.1 / 6.12.6-arch1-1 |
| Port 8080 | free — nothing published on it |
| `/dev/kmsg` | present, `crw-r--r-- root root` — cAdvisor reads it for OOM events |
| Published host ports in use | 21, 80, 443, 3005, 8000, 8324, 9000, 9090, 9093, 32400, 32410, 32412, 32469 |
| cAdvisor image | `v0.55.1` — the newest tag actually PUBLISHED at gcr.io. The v0.60.5 GitHub release is not there; pulling it fails with `manifest unknown`. |
| Textfile dir | `/var/lib/node_exporter/textfile_collector`, root:root 0755, already read by node-exporter |
| Existing guard | `TextfileCollectorError` already alerts if any `.prom` file fails to parse |

## File Structure

**Created:**

| Path | Responsibility |
|---|---|
| `roles/cadvisor/tasks/main.yml` | Runs cAdvisor as a pinned container |
| `roles/prometheus/templates/container_expected.prom.j2` | Writes the expected-container list as a metric |
| `roles/prometheus/files/rules/container.yml` | The container-health alert rules |
| `tests/rules/container_test.yml` | Unit tests for those rules |

**Modified:**

| Path | Change |
|---|---|
| `playbooks/xps.yml` | Add the `cadvisor` role |
| `roles/prometheus/templates/prometheus.yml.j2` | Scrape cAdvisor |
| `roles/prometheus/defaults/main.yml` | Add `monitored_containers` |
| `roles/prometheus/tasks/main.yml` | Deploy the expected-container metric file |
| `docs/superpowers/specs/2026-08-19-observability-alerting-design.md` | Record the metric inventory findings; mark increment 4 delivered |

---

### Task 1: cAdvisor and the expected-container metric

Produces both metric sources. No rules yet — the rules in Task 2 are written against metric names verified here rather than assumed.

**Files:**
- Create: `roles/cadvisor/tasks/main.yml`
- Create: `roles/prometheus/templates/container_expected.prom.j2`
- Modify: `playbooks/xps.yml`
- Modify: `roles/prometheus/templates/prometheus.yml.j2`
- Modify: `roles/prometheus/defaults/main.yml`
- Modify: `roles/prometheus/tasks/main.yml`

**Interfaces:**
- Consumes: the existing `caddy_network`, and node-exporter's textfile directory created by the `prometheus` role.
- Produces: a `cadvisor` scrape job, and these metrics for Task 2's rules — `container_last_seen`, `container_start_time_seconds`, `container_oom_events_total` (all from cAdvisor, labelled `name`), and `container_expected{name="..."}` from the textfile.

- [ ] **Step 1: Verify the pinned digest still resolves**

The digest is already resolved and the image already pulled. `docker buildx` is NOT installed on this server, so confirm by inspecting:

```bash
ssh daniel@xps.fritz.box "docker inspect --format '{{index .RepoDigests 0}}' gcr.io/cadvisor/cadvisor:v0.55.1"
```

Expected: `gcr.io/cadvisor/cadvisor@sha256:3de2bd5203120b866d74a9b283b2ffb8ec382fbf9dc321814700c6ea6f44ec57`

If it differs, upstream re-pushed the tag — use the printed value and update this plan's Global Constraints first. Do not substitute a different tag or registry: `v0.60.5` exists as a GitHub release but NOT as an image at gcr.io, and pulling it fails.

- [ ] **Step 2: Declare the expected container list**

Add to `roles/prometheus/defaults/main.yml`, below the existing `smart_devices`:

```yaml
# The containers this host is configured to run. Written into the textfile
# collector as container_expected{name="..."} so ContainerMissing can compare
# what should be running against what cAdvisor actually reports.
#
# This is data rather than templated rule text deliberately: a templated rule
# file lives under templates/, where the test harness never looks and promtool
# cannot validate it. Adding a service to the playbook means adding a line
# here, which extends alert coverage automatically.
monitored_containers:
  - actual_budget
  - alertmanager
  - cadvisor
  - caddy
  - ftp_server
  - grafana
  - immich-machine-learning
  - immich-postgres
  - immich-redis
  - immich-server
  - kavita
  - planka
  - planka-postgres
  - plex
  - portainer
  - prometheus
  - vaultwarden
```

- [ ] **Step 3: Write the expected-container metric template**

Create `roles/prometheus/templates/container_expected.prom.j2`:

```
# HELP container_expected 1 for each container this host is configured to run.
# TYPE container_expected gauge
{% for container in monitored_containers %}
container_expected{name="{{ container }}"} 1
{% endfor %}
```

The `{name="` sequence is safe — only `{#` collides with Jinja, and `{{ container }}` is the intended substitution.

- [ ] **Step 4: Write the cAdvisor role**

Create `roles/cadvisor/tasks/main.yml`, using the digest verified in Step 1:

```yaml
- name: Pull the cAdvisor image
  community.docker.docker_image_pull:
    name: "gcr.io/cadvisor/cadvisor:v0.55.1@sha256:3de2bd5203120b866d74a9b283b2ffb8ec382fbf9dc321814700c6ea6f44ec57"
    pull: not_present

- name: Run cAdvisor container
  community.docker.docker_container:
    name: cadvisor
    image: "gcr.io/cadvisor/cadvisor:v0.55.1@sha256:3de2bd5203120b866d74a9b283b2ffb8ec382fbf9dc321814700c6ea6f44ec57"
    state: started
    restart_policy: always
    networks:
      - name: caddy_network
    ports:
      - "8080:8080"
    volumes:
      - /:/rootfs:ro
      - /var/run:/var/run:ro
      - /sys:/sys:ro
      - /var/lib/docker/:/var/lib/docker:ro
      - /dev/disk/:/dev/disk:ro
    devices:
      # cAdvisor reads OOM kill events from the kernel log. Without this
      # container_oom_events_total exists but never increments, so
      # ContainerOOMKilled would be silently unarmed.
      - /dev/kmsg:/dev/kmsg:r
    command:
      # Only the metrics the rules actually use. The default set is large
      # and most of it goes unread, which costs scrape time and cardinality.
      - "--docker_only=true"
      - "--housekeeping_interval=30s"
```

Deliberately NOT `privileged: true`. cAdvisor's documentation commonly suggests it, but this project removed an unauthenticated shutdown endpoint on the same reasoning: do not grant more than the job needs. Step 8 verifies whether the unprivileged configuration actually produces the metrics; if it does not, that becomes a decision to surface rather than a flag to add silently.

- [ ] **Step 5: Deploy the expected-container metric from the prometheus role**

In `roles/prometheus/tasks/main.yml`, immediately after the existing "Enable the node-exporter textfile collector" task, add:

```yaml
- name: Write the expected-container list for ContainerMissing
  ansible.builtin.template:
    src: container_expected.prom.j2
    dest: /var/lib/node_exporter/textfile_collector/container_expected.prom
    owner: root
    group: root
    mode: '0644'
```

- [ ] **Step 6: Scrape cAdvisor**

In `roles/prometheus/templates/prometheus.yml.j2`, add after the existing `node` job:

```yaml
  - job_name: "cadvisor"
    static_configs:
      - targets: ["cadvisor:8080"]
        labels:
          instance: "xps"
```

cAdvisor is on `caddy_network`, so Prometheus reaches it by container name — no `host.docker.internal` needed. The explicit `instance` label matches the convention the other jobs use.

- [ ] **Step 7: Add the role to the playbook**

In `playbooks/xps.yml`, add `cadvisor` immediately after `caddy` and before `alertmanager`, so it reads:

```yaml
    - caddy
    - cadvisor
    - alertmanager
    - prometheus
    - grafana
```

`caddy` creates `caddy_network`, which cAdvisor joins, so it must come first.

- [ ] **Step 8: Deploy and inventory what cAdvisor actually exports**

```bash
ansible-playbook -i inventory/hosts.ini playbooks/xps.yml
```

Then check each metric name Task 2's rules depend on. **This step is the reason the rules come second — do not skip or summarise it.**

```bash
ssh daniel@xps.fritz.box 'curl -s http://localhost:8080/metrics | grep -c "^container_last_seen"'
ssh daniel@xps.fritz.box 'curl -s http://localhost:8080/metrics | grep -c "^container_start_time_seconds"'
ssh daniel@xps.fritz.box 'curl -s http://localhost:8080/metrics | grep -c "^container_oom_events_total"'
```

Expected: a non-zero count for each. Record the actual numbers.

**If `container_oom_events_total` is absent or zero-count, STOP and report it rather than proceeding.** It is the one metric whose availability depends on the kernel and the container's access to `/dev/kmsg`, and Task 2 must not write a rule against a metric that does not exist — that produces a rule which matches nothing forever while its unit tests pass.

Also confirm the `name` label is present and carries readable container names, since every rule joins on it:

```bash
ssh daniel@xps.fritz.box 'curl -s http://localhost:8080/metrics | grep "^container_last_seen" | head -3'
```

Expected: entries like `container_last_seen{...,name="vaultwarden",...}`.

- [ ] **Step 9: Confirm Prometheus scrapes both sources**

```bash
ssh daniel@xps.fritz.box 'curl -s http://localhost:9090/api/v1/targets | jq -r ".data.activeTargets[] | \"\(.labels.job) \(.health)\""'
```

Expected: `cadvisor up` alongside `node up` and `prometheus up`.

```bash
ssh daniel@xps.fritz.box 'curl -s "http://localhost:9090/api/v1/query?query=count(container_last_seen)" | jq -r ".data.result[0].value[1]"'
ssh daniel@xps.fritz.box 'curl -s "http://localhost:9090/api/v1/query?query=count(container_expected)" | jq -r ".data.result[0].value[1]"'
```

Expected: the first is at least 17 (cAdvisor reports the pause/infra containers too), the second is exactly 17.

**Confirm the two agree on naming** — this is the join the whole design rests on:

```bash
ssh daniel@xps.fritz.box 'curl -s "http://localhost:9090/api/v1/query?query=container_expected%20unless%20on(name)%20container_last_seen" | jq -r ".data.result[] | .metric.name"'
```

Expected: **empty**. Every expected container should currently be reported by cAdvisor. Any name printed here is a mismatch between `monitored_containers` and reality — fix the list, do not proceed with a broken join.

- [ ] **Step 10: Confirm the textfile file still parses**

```bash
ssh daniel@xps.fritz.box 'curl -s http://localhost:9100/metrics | grep node_textfile_scrape_error'
```

Expected: `0`. A `1` means the new `.prom` file is malformed and node-exporter has dropped **every** textfile metric, including the backup and SMART ones.

- [ ] **Step 11: Verify idempotency**

```bash
ansible-playbook -i inventory/hosts.ini playbooks/xps.yml
```

Expected: `changed=0`.

- [ ] **Step 12: Commit**

```bash
git add roles/cadvisor roles/prometheus/templates/container_expected.prom.j2 roles/prometheus/defaults/main.yml roles/prometheus/tasks/main.yml roles/prometheus/templates/prometheus.yml.j2 playbooks/xps.yml
git commit -m "Add cAdvisor and the expected-container metric

The list of containers that ought to be running is written as a metric
rather than templated into a rule file. A templated rule lives under
templates/, where the test harness never looks and promtool cannot
validate it — one untested rule among tested ones.

cAdvisor runs unprivileged with /dev/kmsg mounted for OOM detection,
rather than the privileged mode its documentation suggests.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

### Task 2: The container-health rules

**Files:**
- Create: `roles/prometheus/files/rules/container.yml`
- Create: `tests/rules/container_test.yml`

**Interfaces:**
- Consumes: `container_last_seen`, `container_start_time_seconds`, `container_oom_events_total` (from cAdvisor, labelled `name`) and `container_expected` (from the textfile), all verified to exist in Task 1 Step 8.
- Produces: five alerts — `ContainerMissing`, `ContainerRestartLoop`, `ContainerOOMKilled`, `ContainerMetricsMissing`, `ContainerExpectedMissing`.

- [ ] **Step 1: Write the failing tests**

Create `tests/rules/container_test.yml`:

```yaml
rule_files:
  - ../../roles/prometheus/files/rules/container.yml

evaluation_interval: 1m

tests:
  # Everything expected is being reported: silence.
  - interval: 1m
    input_series:
      - series: 'container_expected{name="vaultwarden"}'
        values: '1+0x30'
      - series: 'container_expected{name="plex"}'
        values: '1+0x30'
      - series: 'container_last_seen{name="vaultwarden",instance="xps",job="cadvisor"}'
        values: '1000+60x30'
      - series: 'container_last_seen{name="plex",instance="xps",job="cadvisor"}'
        values: '1000+60x30'
    alert_rule_test:
      - eval_time: 20m
        alertname: ContainerMissing
        exp_alerts: []
      - eval_time: 20m
        alertname: ContainerMetricsMissing
        exp_alerts: []
      - eval_time: 20m
        alertname: ContainerExpectedMissing
        exp_alerts: []

  # One expected container stops being reported while others continue.
  # This is the case the whole increment exists for.
  - interval: 1m
    input_series:
      - series: 'container_expected{name="vaultwarden"}'
        values: '1+0x30'
      - series: 'container_expected{name="plex"}'
        values: '1+0x30'
      - series: 'container_last_seen{name="plex",instance="xps",job="cadvisor"}'
        values: '1000+60x30'
    alert_rule_test:
      - eval_time: 3m
        alertname: ContainerMissing
        exp_alerts: []
      - eval_time: 8m
        alertname: ContainerMissing
        exp_alerts:
          - exp_labels:
              severity: critical
              name: vaultwarden

  # cAdvisor itself down: container_last_seen absent entirely. ContainerMissing
  # must NOT fire seventeen times; ContainerMetricsMissing fires once instead.
  - interval: 1m
    input_series:
      - series: 'container_expected{name="vaultwarden"}'
        values: '1+0x30'
      - series: 'container_expected{name="plex"}'
        values: '1+0x30'
    alert_rule_test:
      - eval_time: 20m
        alertname: ContainerMissing
        exp_alerts: []
      - eval_time: 20m
        alertname: ContainerMetricsMissing
        exp_alerts:
          - exp_labels:
              severity: warning

  # The expected list itself missing leaves ContainerMissing matching
  # nothing, so it needs its own alert.
  - interval: 1m
    input_series:
      - series: 'container_last_seen{name="plex",instance="xps",job="cadvisor"}'
        values: '1000+60x70'
    alert_rule_test:
      - eval_time: 30m
        alertname: ContainerExpectedMissing
        exp_alerts: []
      - eval_time: 65m
        alertname: ContainerExpectedMissing
        exp_alerts:
          - exp_labels:
              severity: warning

  # A container restarting repeatedly. start_time changes on each restart.
  - interval: 1m
    input_series:
      - series: 'container_start_time_seconds{name="planka",instance="xps",job="cadvisor"}'
        values: '100 200 300 400 500 600 700 800 900 1000 1100 1200'
    alert_rule_test:
      - eval_time: 11m
        alertname: ContainerRestartLoop
        exp_alerts:
          - exp_labels:
              severity: warning
              name: planka
              instance: xps
              job: cadvisor

  # A container that started once and stayed up must never fire it.
  - interval: 1m
    input_series:
      - series: 'container_start_time_seconds{name="planka",instance="xps",job="cadvisor"}'
        values: '100+0x70'
    alert_rule_test:
      - eval_time: 65m
        alertname: ContainerRestartLoop
        exp_alerts: []

  # An OOM kill.
  - interval: 1m
    input_series:
      - series: 'container_oom_events_total{name="immich-machine-learning",instance="xps",job="cadvisor"}'
        values: '0 0 0 1 1 1 1 1 1 1 1 1'
    alert_rule_test:
      - eval_time: 11m
        alertname: ContainerOOMKilled
        exp_alerts:
          - exp_labels:
              severity: warning
              name: immich-machine-learning
              instance: xps
              job: cadvisor

  # No OOM kills must never fire it.
  - interval: 1m
    input_series:
      - series: 'container_oom_events_total{name="immich-machine-learning",instance="xps",job="cadvisor"}'
        values: '0+0x30'
    alert_rule_test:
      - eval_time: 25m
        alertname: ContainerOOMKilled
        exp_alerts: []
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `./tests/run-promtool.sh`

Expected: FAIL. `promtool check rules` errors because `roles/prometheus/files/rules/container.yml` does not exist.

- [ ] **Step 3: Write the rules**

Create `roles/prometheus/files/rules/container.yml`:

```yaml
groups:
  - name: container
    rules:
      # Sixteen services run on this host as containers. Before these rules
      # existed, one dying was invisible until someone tried to use it.

      - alert: ContainerMissing
        # The expected list comes from a metric Ansible writes, so this rule
        # stays static and testable. The trailing guard matters: without it,
        # cAdvisor being down would make every expected container look
        # missing at once and produce a wall of alerts, when the real fault
        # is a single dead sensor. ContainerMetricsMissing covers that case.
        expr: |
          (container_expected == 1 unless on(name) container_last_seen)
            and on() (count(container_last_seen) > 0)
        for: 5m
        labels:
          severity: critical
        annotations:
          summary: "Container {{ $labels.name }} is not running"
          description: "{{ $labels.name }} is on the expected list for this host but cAdvisor has not reported it for over 5 minutes. Check `docker ps -a` and `docker logs {{ $labels.name }}`."

      - alert: ContainerRestartLoop
        # container_start_time_seconds changes value on every start, so
        # counting its changes counts restarts.
        expr: changes(container_start_time_seconds{name!=""}[1h]) > 3
        for: 5m
        labels:
          severity: warning
        annotations:
          summary: "Container {{ $labels.name }} has restarted {{ $value }} times in an hour"
          description: "{{ $labels.name }} keeps restarting, which usually means it is crashing on startup. Docker's restart policy will keep retrying indefinitely, so this can continue unnoticed. Check `docker logs {{ $labels.name }}`."

      - alert: ContainerOOMKilled
        expr: increase(container_oom_events_total{name!=""}[10m]) > 0
        for: 0m
        labels:
          severity: warning
        annotations:
          summary: "Container {{ $labels.name }} was OOM-killed"
          description: "The kernel killed a process in {{ $labels.name }} for running the host out of memory. The container may have restarted and appear healthy while having lost work."

      - alert: ContainerMetricsMissing
        # cAdvisor is a scrape target, so TargetDown catches it going away
        # entirely. This catches the subtler case where cAdvisor is up and
        # scrapeable but reporting no containers — a lost Docker socket or a
        # permissions change — which TargetDown cannot see.
        expr: absent(container_last_seen)
        for: 10m
        labels:
          severity: warning
        annotations:
          summary: "cAdvisor is reporting no containers"
          description: "No container_last_seen metric exists, so ContainerMissing cannot fire and no container on this host is being watched. Check `docker logs cadvisor` and that it can still read the Docker socket."

      - alert: ContainerExpectedMissing
        # Without the expected list, ContainerMissing's join matches nothing
        # and it is silently unarmed — the same shape as BackupMetricMissing.
        expr: absent(container_expected)
        for: 1h
        labels:
          severity: warning
        annotations:
          summary: "The expected-container list is not being exported"
          description: "No container_expected metric exists, so ContainerMissing has nothing to compare cAdvisor's output against and cannot fire. Check that /var/lib/node_exporter/textfile_collector/container_expected.prom exists and that node_textfile_scrape_error is 0."
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `./tests/run-promtool.sh`

Expected: PASS, with SUCCESS for `container_test.yml` alongside the existing files.

If promtool reports an annotation mismatch, this version compares annotations and treats an omitted `exp_annotations` as "expect none". Add `exp_annotations` blocks copying the exact rendered text from promtool's failure output rather than predicting it.

- [ ] **Step 5: Verify the tests constrain the rules (mutation testing)**

Apply each mutation, confirm the suite FAILS, revert, confirm it passes. Report before and after for each.

| Mutation | Expected |
|---|---|
| Delete the `and on() (count(container_last_seen) > 0)` guard from `ContainerMissing` | FAIL — the cAdvisor-down case would fire per container |
| `container_expected == 1` → `container_expected == 0` | FAIL |
| `changes(...) > 3` → `> 99999` | FAIL |
| `changes(...) > 3` → `>= 0` | FAIL |
| `increase(container_oom_events_total{name!=""}[10m]) > 0` → `> 99999` | FAIL |
| `absent(container_last_seen)` → `absent(nonexistent_metric)` | FAIL |
| `absent(container_expected)` → `absent(nonexistent_metric)` | FAIL |
| `ContainerMissing` severity `critical` → `warning` | FAIL |

A mutation that still passes means that rule's assertions are not doing their job — strengthen them before continuing.

- [ ] **Step 6: Deploy and confirm the rules load**

```bash
ansible-playbook -i inventory/hosts.ini playbooks/xps.yml
ssh daniel@xps.fritz.box 'curl -s http://localhost:9090/api/v1/rules | jq -r ".data.groups[] | select(.name==\"container\") | .rules[] | \"\(.name) \(.health)\""'
```

Expected: five rules, all `ok`.

```bash
ssh daniel@xps.fritz.box 'curl -s http://localhost:9090/api/v1/alerts | jq -r ".data.alerts[].labels.alertname" | sort -u'
```

Expected: only `Watchdog`. Any container alert firing now means either a real problem or a mismatch between `monitored_containers` and reality — investigate rather than adjusting the list to silence it.

- [ ] **Step 7: Verify idempotency**

```bash
ansible-playbook -i inventory/hosts.ini playbooks/xps.yml
```

Expected: `changed=0`.

- [ ] **Step 8: Commit**

```bash
git add roles/prometheus/files/rules/container.yml tests/rules/container_test.yml
git commit -m "Add container health alert rules

Sixteen services run here as containers and one dying was invisible
until someone tried to use it.

ContainerMissing carries a guard so that cAdvisor going down produces a
single ContainerMetricsMissing rather than a wall of one alert per
container — the fault is a dead sensor, not sixteen dead services.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

### Task 3: Fault injection and spec update

**Files:**
- Modify: `docs/superpowers/specs/2026-08-19-observability-alerting-design.md`

**Interfaces:**
- Consumes: everything from Tasks 1 and 2.
- Produces: nothing.

- [ ] **Step 1: Prove ContainerMissing fires on a real stopped container**

Record the Telegram counter first:

```bash
ssh daniel@xps.fritz.box 'curl -s http://localhost:9093/metrics | grep -E "^alertmanager_notifications_total\{integration=\"telegram\""'
```

Stop a container whose absence costs nothing. **Use `ftp_server`** — it is the least critical service on the host, and unlike Plex or Immich nobody is likely to be using it.

```bash
ssh daniel@xps.fritz.box 'docker stop ftp_server'
```

Poll until `ContainerMissing` reaches `firing`. Do NOT use a local `sleep`; sleep on the remote host inside the ssh command, e.g.
`ssh daniel@xps.fritz.box 'sleep 120; curl -s http://localhost:9090/api/v1/alerts | jq -r ".data.alerts[] | \"\(.labels.alertname) \(.labels.name) \(.state)\""'`

Expected within about 7 minutes: `ContainerMissing ftp_server firing`, and a 🔴 CRITICAL Telegram message naming `ftp_server`. Confirm from the sending side that the counter incremented with no matching failure.

- [ ] **Step 2: Confirm it clears**

```bash
ssh daniel@xps.fritz.box 'docker start ftp_server'
```

Poll until `ContainerMissing` disappears from the alerts API.

Expected: a ✅ RESOLVED Telegram message. An alert that fires but never clears trains the reader to ignore it, so this half matters as much as the first.

Confirm the container is genuinely healthy again:

```bash
ssh daniel@xps.fritz.box 'docker ps --filter name=ftp_server --format "{{.Names}} {{.Status}}"'
```

- [ ] **Step 3: Prove the cAdvisor-down case produces ONE alert, not sixteen**

This is the behaviour the guard on `ContainerMissing` exists for.

```bash
ssh daniel@xps.fritz.box 'docker stop cadvisor'
```

Poll for at least 12 minutes.

Expected: `ContainerMetricsMissing` fires. `ContainerMissing` must NOT fire for any container. `TargetDown` may also fire for the cadvisor job, which is correct.

**If you see ContainerMissing firing per container, report it as a finding** — the guard is not working and the rule would produce a wall of alerts every time the sensor restarts.

```bash
ssh daniel@xps.fritz.box 'docker start cadvisor'
```

Confirm both alerts clear.

- [ ] **Step 4: Update the spec**

Record what Task 1 Step 8 found about cAdvisor's metrics — specifically whether `container_oom_events_total` is exported and whether the unprivileged configuration was sufficient — as a sentence in the container-health section.

Add `ContainerMetricsMissing` to the section's alert table, which currently lists four alerts while five ship:

```
| `ContainerMetricsMissing` | cAdvisor is up but reporting no containers | warning |
```

Then mark increment 4 delivered in the build-order table by appending ` — delivered 2026-08-20` to its row, matching increments 1 to 3.

- [ ] **Step 5: Commit**

```bash
git add docs/superpowers/specs/2026-08-19-observability-alerting-design.md
git commit -m "Mark alerting increment 4 as delivered

Fault injection confirmed ContainerMissing fires on a genuinely stopped
container and clears when it returns, and that cAdvisor going down
produces a single ContainerMetricsMissing rather than one alert per
container.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

## Deliberately out of scope

- **Per-container memory and CPU alerting.** cAdvisor exports it, but almost no container here has a memory limit set, so any threshold would be a guess about normal rather than a statement about abnormal. `MemoryPressure` already covers the host running out.
- **Privileged cAdvisor.** The unprivileged configuration is deployed and verified first. If a needed metric turns out to require privilege, that becomes a decision to surface rather than a flag added quietly.
- **Alerting on `ftp_server`'s unpinned image.** `fauria/vsftpd:latest` is the only floating tag on the host and is drift from this repo's pinning convention, but it belongs with the Diun increment rather than here.
