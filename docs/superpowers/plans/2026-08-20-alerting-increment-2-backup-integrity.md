# Alerting Increment 2: Backup Integrity Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Find out when the offsite backup fails, and — more importantly — when it silently stops running at all.

**Architecture:** `restic-backup.sh` writes a success timestamp and duration into node-exporter's textfile-collector directory, which Prometheus already scrapes. Three rules watch it: one for an outright failure pushed the instant it happens, one for a backup that has not succeeded recently, and one for the metric being absent entirely. No new services.

**Tech Stack:** Ansible, restic, systemd, node-exporter textfile collector, Prometheus, Alertmanager, promtool

**Spec:** `docs/superpowers/specs/2026-08-19-observability-alerting-design.md` (the "Backup integrity" section)

## Global Constraints

- **Alert `severity` must be exactly one of** `critical`, `warning`, `info`, `none`. The deployed Alertmanager routes on precisely these; any other value is silently delivered mislabelled as WARNING via the default route.
- **The `instance` label is `xps`** on every alert this project produces. Increment 1 set it explicitly on both scrape targets; pushed alerts must match so grouping and reading stay consistent.
- **Rule files live in `roles/prometheus/files/rules/`** and are copied verbatim by Ansible. Go template expressions in annotations (`{{ $labels.x }}`, `{{ $value }}`) need NO escaping there. Files under `templates/` are Jinja-rendered and DO need `{% raw %}`.
- **Every rule gets a unit test that asserts both firing AND silence.** A test asserting only that an alert fires cannot catch a rule mutated to fire always. This is not optional — increment 1's review proved four such rules could be arbitrarily broken while the suite stayed green.
- **The test harness is `./tests/run-promtool.sh`**, run from the repo root. It always uses the pinned image `prom/prometheus:v3.14.0@sha256:5ce7540c3c00ef4ab0c9d2c995c6a5b9c421f44b4a115d97a2c7af3b1c21cbb0`.
- **Roles must be idempotent:** a second `ansible-playbook -i inventory/hosts.ini playbooks/xps.yml` reports zero changed tasks.
- **Never write a credential into a committed file.** Secrets live in the gitignored `user_passwords.yml`.
- **Ansible target:** host `xps` (`xps.fritz.box`), user `daniel`.
- Commit messages end with: `Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>`

---

## Existing state this builds on

- `restic-backup.service` is `Type=oneshot`, runs `/usr/local/bin/restic-backup.sh`, has **no** `OnFailure=`.
- `restic-backup.timer` is `OnCalendar=daily`, `Persistent=true`. Last run succeeded at 00:00 today.
- `/var/lib/node_exporter/textfile_collector/` exists, is empty, is owned `root:root 0755`, and node-exporter reads it — `node_textfile_scrape_error 0` confirms the collector is live.
- The backup script uses `set -euo pipefail`, so any failing step aborts it with a non-zero exit. This is what makes both the `OnFailure=` handler and the "only write the metric on success" approach work.

## File Structure

**Created:**

| Path | Responsibility |
|---|---|
| `roles/prometheus/files/rules/backup.yml` | The three backup alert rules |
| `tests/rules/backup_test.yml` | Unit tests for those rules |
| `roles/backup/files/backup-alert.sh` | Pushes a one-shot failure alert into Alertmanager |
| `roles/backup/files/backup-alert.service` | The oneshot unit `OnFailure=` triggers |

**Modified:**

| Path | Change |
|---|---|
| `roles/backup/templates/restic-backup.sh.j2` | Writes success timestamp and duration to the textfile directory |
| `roles/backup/files/restic-backup.service` | Gains `OnFailure=backup-alert.service` |
| `roles/backup/tasks/main.yml` | Installs `jq`, deploys the alert script and unit |
| `docs/superpowers/specs/2026-08-19-observability-alerting-design.md` | Mark increment 2 delivered |

---

### Task 1: The backup alert rules

Pure local work — nothing is deployed and no server is touched. The rules are written and tested before anything produces the metric they read.

**Files:**
- Create: `roles/prometheus/files/rules/backup.yml`
- Create: `tests/rules/backup_test.yml`

**Interfaces:**
- Consumes: `./tests/run-promtool.sh` from increment 1; it globs `roles/prometheus/files/rules/*.yml` and `tests/rules/*_test.yml`, so new files are picked up with no harness change.
- Produces: alerts named `BackupStale` and `BackupMetricMissing`, both `severity: critical`. Also establishes the metric names `restic_backup_last_success_timestamp_seconds` and `restic_backup_duration_seconds`, which Task 2 must emit **exactly**.

- [ ] **Step 1: Write the failing tests**

Create `tests/rules/backup_test.yml`:

```yaml
rule_files:
  - ../../roles/prometheus/files/rules/backup.yml

evaluation_interval: 1m

tests:
  # A backup that succeeded 30 hours ago is stale.
  # promtool's clock starts at 0, so the series carries a large absolute
  # unix timestamp and we evaluate far enough in for time() to exceed it.
  - interval: 1h
    input_series:
      - series: 'restic_backup_last_success_timestamp_seconds{instance="xps",job="node"}'
        values: '1000000+0x40'
    alert_rule_test:
      # At 30h of evaluation time, time() is 108000 and the recorded
      # timestamp is 1000000 — not yet stale, because the recorded value
      # is in the future relative to promtool's clock. See Step 3 for why
      # the rule is written to tolerate this.
      - eval_time: 30h
        alertname: BackupStale
        exp_alerts: []

  # A backup whose timestamp is 27 hours behind promtool's clock IS stale.
  - interval: 1h
    input_series:
      - series: 'restic_backup_last_success_timestamp_seconds{instance="xps",job="node"}'
        values: '0+0x40'
    alert_rule_test:
      # 25h elapsed: under the 26h threshold, must stay silent.
      - eval_time: 25h
        alertname: BackupStale
        exp_alerts: []
      # 27h elapsed: over the threshold, must fire.
      - eval_time: 27h
        alertname: BackupStale
        exp_alerts:
          - exp_labels:
              severity: critical
              instance: xps
              job: node

  # The metric existing at all keeps BackupMetricMissing quiet.
  - interval: 1h
    input_series:
      - series: 'restic_backup_last_success_timestamp_seconds{instance="xps",job="node"}'
        values: '0+0x40'
    alert_rule_test:
      - eval_time: 2h
        alertname: BackupMetricMissing
        exp_alerts: []

  # No series at all: BackupMetricMissing fires once its `for:` elapses.
  - interval: 1h
    input_series: []
    alert_rule_test:
      # Before the 1h `for:` has elapsed, it must stay silent.
      - eval_time: 30m
        alertname: BackupMetricMissing
        exp_alerts: []
      - eval_time: 2h
        alertname: BackupMetricMissing
        exp_alerts:
          - exp_labels:
              severity: critical
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `./tests/run-promtool.sh`

Expected: FAIL. `promtool check rules` errors because `roles/prometheus/files/rules/backup.yml` does not exist.

- [ ] **Step 3: Write the rules**

Create `roles/prometheus/files/rules/backup.yml`:

```yaml
groups:
  - name: backup
    rules:
      # The offsite backup to Backblaze B2 is the reason this whole
      # monitoring stack exists. These three rules cover the three ways it
      # can let you down: it failed, it stopped running, or it was never
      # being watched in the first place.

      - alert: BackupStale
        # 26 hours, not 24: the timer is OnCalendar=daily, so a run that
        # starts late or takes a while must not trip this.
        expr: time() - restic_backup_last_success_timestamp_seconds > 26 * 3600
        labels:
          severity: critical
        annotations:
          summary: "No successful backup in over 26 hours"
          description: "The last successful restic backup to B2 finished {{ $value | humanizeDuration }} ago. The daily timer may have failed to run at all — check `systemctl list-timers restic-backup.timer` and `journalctl -u restic-backup.service`."

      - alert: BackupMetricMissing
        # Not redundant with BackupStale. BackupStale compares a timestamp;
        # if the textfile was never written there is no timestamp to
        # compare and that rule silently never fires. absent() catches
        # exactly that — the case where the backup looks monitored and is
        # not. The 1h `for:` stops it firing during a Prometheus restart.
        expr: absent(restic_backup_last_success_timestamp_seconds)
        for: 1h
        labels:
          severity: critical
        annotations:
          summary: "Backup metrics are missing entirely"
          description: "node_exporter is not exporting restic_backup_last_success_timestamp_seconds, so BackupStale cannot fire and the backup is effectively unmonitored. Either no backup has succeeded since this was deployed, or the textfile collector is broken — check /var/lib/node_exporter/textfile_collector/restic_backup.prom exists and node_textfile_scrape_error is 0."
```

Note on the first test case: `time()` in promtool starts at zero and counts evaluation time, so a series carrying a real-world unix timestamp is always "in the future" and the subtraction goes negative. That is why the realistic test uses `0` as the recorded timestamp and lets promtool's clock advance past the threshold. The rule needs no special handling for this — it is a property of the test harness, not the rule.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `./tests/run-promtool.sh`

Expected: PASS, with SUCCESS for `backup_test.yml` alongside the existing files.

If promtool reports an annotation mismatch, this version compares annotations too — add `exp_annotations` blocks copying the exact rendered text from the failure output rather than predicting it.

- [ ] **Step 5: Verify the tests actually constrain the rules (mutation testing)**

Apply each mutation, confirm the suite FAILS, then revert it and confirm it passes again. Report before and after for each.

| Mutation | File | Expected |
|---|---|---|
| `> 26 * 3600` → `> 0` | `roles/prometheus/files/rules/backup.yml` | Suite FAILS |
| `> 26 * 3600` → `> 99999999` | same | Suite FAILS |
| `absent(...)` → `absent(nonexistent_metric_name)` | same | Suite FAILS |
| `for: 1h` → `for: 999h` | same | Suite FAILS |

A mutation that still passes means the corresponding assertion is not doing its job — strengthen it before continuing.

- [ ] **Step 6: Commit**

```bash
git add roles/prometheus/files/rules/backup.yml tests/rules/backup_test.yml
git commit -m "Add backup integrity alert rules

BackupStale catches a backup that stopped running; BackupMetricMissing
catches the case where the metric was never written at all, which
BackupStale structurally cannot see because it has no timestamp to
compare against.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

### Task 2: Emit the metrics from the backup script

**Files:**
- Modify: `roles/backup/templates/restic-backup.sh.j2`

**Interfaces:**
- Consumes: the metric names established in Task 1 — `restic_backup_last_success_timestamp_seconds` and `restic_backup_duration_seconds`. These must match exactly or the rules watch nothing.
- Produces: `/var/lib/node_exporter/textfile_collector/restic_backup.prom` on the server, scraped into Prometheus via the existing `node` job.

- [ ] **Step 1: Modify the script template**

`roles/backup/templates/restic-backup.sh.j2` currently reads:

```bash
#!/bin/bash
set -euo pipefail

DUMP_DIR=/mnt/storage/backup/db-dumps

docker exec planka-postgres pg_dump -U postgres planka > "${DUMP_DIR}/planka.sql"
docker exec -e PGPASSWORD="{{ immich_db_password }}" immich-postgres pg_dump -U immich immich > "${DUMP_DIR}/immich.sql"

# The B2 key used here has no delete capability (ransomware protection - see README),
# so this only ever adds snapshots. Pruning old ones requires the admin key kept off-server.
restic backup \
{% for path in backup_paths %}
  "{{ path }}" \
{% endfor %}
  --exclude-caches \
  --tag automated
```

Change it to:

```bash
#!/bin/bash
set -euo pipefail

DUMP_DIR=/mnt/storage/backup/db-dumps
TEXTFILE_DIR=/var/lib/node_exporter/textfile_collector
METRICS_FILE="${TEXTFILE_DIR}/restic_backup.prom"

START=$(date +%s)

docker exec planka-postgres pg_dump -U postgres planka > "${DUMP_DIR}/planka.sql"
docker exec -e PGPASSWORD="{{ immich_db_password }}" immich-postgres pg_dump -U immich immich > "${DUMP_DIR}/immich.sql"

# The B2 key used here has no delete capability (ransomware protection - see README),
# so this only ever adds snapshots. Pruning old ones requires the admin key kept off-server.
restic backup \
{% for path in backup_paths %}
  "{{ path }}" \
{% endfor %}
  --exclude-caches \
  --tag automated

END=$(date +%s)

# Only reached when everything above succeeded, because set -e aborts the
# script on any failure — so the timestamp genuinely means "last success",
# which is what BackupStale depends on.
#
# Written to a temporary file and renamed, because a rename within the same
# filesystem is atomic: node_exporter can never scrape a half-written file.
# The temporary name must not end in .prom or node_exporter would read it.
cat > "${METRICS_FILE}.tmp" <<METRICS
# HELP restic_backup_last_success_timestamp_seconds Unix time of the last successful restic backup to B2.
# TYPE restic_backup_last_success_timestamp_seconds gauge
restic_backup_last_success_timestamp_seconds ${END}
# HELP restic_backup_duration_seconds Wall-clock seconds taken by the last successful restic backup.
# TYPE restic_backup_duration_seconds gauge
restic_backup_duration_seconds $((END - START))
METRICS
mv "${METRICS_FILE}.tmp" "${METRICS_FILE}"
```

The heredoc delimiter is `METRICS` rather than `EOF` purely to avoid confusion with any other heredoc; the content contains no Jinja `{{ }}`, so it passes through the template unchanged.

- [ ] **Step 2: Deploy**

```bash
ansible-playbook -i inventory/hosts.ini playbooks/xps.yml
```

Expected: the `Template backup script` task reports `changed`.

- [ ] **Step 3: Run a backup by hand to produce the metric**

```bash
ssh daniel@xps.fritz.box 'sudo systemctl start restic-backup.service'
```

This runs a real backup to B2. It may take several minutes. Wait for it, then confirm it succeeded:

```bash
ssh daniel@xps.fritz.box 'systemctl show restic-backup.service -p Result --value'
```

Expected: `success`. If it reports anything else, STOP and report — do not proceed to verify a metric that a failed run would not have written.

- [ ] **Step 4: Verify the metric file and its contents**

```bash
ssh daniel@xps.fritz.box 'cat /var/lib/node_exporter/textfile_collector/restic_backup.prom'
```

Expected: both HELP/TYPE blocks and two metric lines with plausible values — a timestamp close to now, and a duration of at least a few seconds.

Confirm no stale temporary file was left behind:

```bash
ssh daniel@xps.fritz.box 'ls /var/lib/node_exporter/textfile_collector/'
```

Expected: exactly `restic_backup.prom`, no `.tmp`.

- [ ] **Step 5: Verify Prometheus is scraping it**

```bash
ssh daniel@xps.fritz.box 'curl -s "http://localhost:9090/api/v1/query?query=restic_backup_last_success_timestamp_seconds" | jq ".data.result"'
```

Expected: one result carrying `instance="xps"` and `job="node"`, with a value matching the file.

Also confirm the collector reports no error:

```bash
ssh daniel@xps.fritz.box 'curl -s http://localhost:9100/metrics | grep node_textfile_scrape_error'
```

Expected: `node_textfile_scrape_error 0`. A `1` means node_exporter could not parse the file.

- [ ] **Step 6: Confirm BackupMetricMissing has cleared**

```bash
ssh daniel@xps.fritz.box 'curl -s http://localhost:9090/api/v1/alerts | jq -r ".data.alerts[] | \"\(.labels.alertname) \(.state)\""'
```

Expected: no `BackupMetricMissing` and no `BackupStale`. Note that between deploying Task 1's rules and running this backup, `BackupMetricMissing` will have been pending or firing — that is correct behaviour, not a fault.

- [ ] **Step 7: Verify idempotency**

```bash
ansible-playbook -i inventory/hosts.ini playbooks/xps.yml
```

Expected: `changed=0`.

- [ ] **Step 8: Commit**

```bash
git add roles/backup/templates/restic-backup.sh.j2
git commit -m "Export backup success timestamp for monitoring

Written only after every step succeeded, via a temp file and an atomic
rename so node_exporter never scrapes a partial write. This is the metric
BackupStale watches — without it that rule has nothing to compare.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

### Task 3: Push an alert the instant a backup fails

`BackupStale` notices within 26 hours. This notices within seconds, and carries the actual error text.

**Files:**
- Create: `roles/backup/files/backup-alert.sh`
- Create: `roles/backup/files/backup-alert.service`
- Modify: `roles/backup/files/restic-backup.service`
- Modify: `roles/backup/tasks/main.yml`

**Interfaces:**
- Consumes: Alertmanager on `localhost:9093`, deployed in increment 1.
- Produces: an alert named `BackupFailed` with `severity: critical` and `instance: xps`, pushed to Alertmanager's `/api/v2/alerts`. It carries an explicit `endsAt` so it self-resolves.

- [ ] **Step 1: Write the alert push script**

Create `roles/backup/files/backup-alert.sh`:

```bash
#!/bin/bash
# Invoked by restic-backup.service's OnFailure=. Pushes a one-shot alert
# straight into Alertmanager, carrying the exit status and the tail of the
# journal so the Telegram message says what actually went wrong.
#
# No `set -e`: this runs when something has ALREADY failed, and a failure
# to collect one optional detail must not stop the alert being sent.
set -uo pipefail

ALERTMANAGER_URL="http://localhost:9093/api/v2/alerts"

RESULT=$(systemctl show restic-backup.service -p Result --value)
EXIT_CODE=$(systemctl show restic-backup.service -p ExecMainStatus --value)
LOG_TAIL=$(journalctl -u restic-backup.service -n 20 --no-pager -o cat | tail -c 800)

STARTS_AT=$(date -u +%Y-%m-%dT%H:%M:%SZ)
# Alertmanager treats an alert as firing until it stops being sent. A backup
# failure is a moment, not a state, so an explicit endsAt makes it
# self-resolve instead of re-notifying forever.
ENDS_AT=$(date -u -d '+15 minutes' +%Y-%m-%dT%H:%M:%SZ)

PAYLOAD=$(jq -n \
  --arg starts "$STARTS_AT" \
  --arg ends "$ENDS_AT" \
  --arg result "$RESULT" \
  --arg code "$EXIT_CODE" \
  --arg log "$LOG_TAIL" \
  '[{
      labels: {
        alertname: "BackupFailed",
        severity: "critical",
        instance: "xps",
        job: "backup"
      },
      annotations: {
        summary: ("restic backup to B2 failed: " + $result + " (exit " + $code + ")"),
        description: $log
      },
      startsAt: $starts,
      endsAt: $ends
    }]')

curl --silent --show-error --max-time 20 \
  --header 'Content-Type: application/json' \
  --data "$PAYLOAD" \
  "$ALERTMANAGER_URL"
```

- [ ] **Step 2: Write the unit that runs it**

Create `roles/backup/files/backup-alert.service`:

```ini
[Unit]
Description=Push a restic backup failure alert to Alertmanager

[Service]
Type=oneshot
ExecStart=/usr/local/bin/backup-alert.sh
```

- [ ] **Step 3: Wire it to the backup unit**

`roles/backup/files/restic-backup.service` currently reads:

```ini
[Unit]
Description=Restic backup to Backblaze B2
Wants=network-online.target
After=network-online.target docker.service

[Service]
Type=oneshot
Environment=HOME=/root
EnvironmentFile=/etc/restic/env
ExecStart=/usr/local/bin/restic-backup.sh
```

Add the `OnFailure=` line to the `[Unit]` section so it reads:

```ini
[Unit]
Description=Restic backup to Backblaze B2
Wants=network-online.target
After=network-online.target docker.service
OnFailure=backup-alert.service

[Service]
Type=oneshot
Environment=HOME=/root
EnvironmentFile=/etc/restic/env
ExecStart=/usr/local/bin/restic-backup.sh
```

- [ ] **Step 4: Deploy the script and unit from Ansible**

In `roles/backup/tasks/main.yml`, add `jq` to the package installation. The task currently reads:

```yaml
- name: Install restic
  pacman:
    name: restic
    state: present
```

Change it to:

```yaml
- name: Install restic and jq
  # jq builds the Alertmanager JSON payload in backup-alert.sh.
  pacman:
    name:
      - restic
      - jq
    state: present
```

Then, immediately after the existing "Template backup script" task, add:

```yaml
- name: Install the backup failure alert script
  copy:
    src: backup-alert.sh
    dest: /usr/local/bin/backup-alert.sh
    owner: root
    group: root
    mode: '0755'

- name: Install the backup failure alert unit
  copy:
    src: backup-alert.service
    dest: /etc/systemd/system/backup-alert.service
    owner: root
    group: root
    mode: '0644'
  notify: Reload systemd
```

The role already has a `Reload systemd` handler and already flushes handlers before enabling the timer, so no new handler is needed.

- [ ] **Step 5: Deploy**

```bash
ansible-playbook -i inventory/hosts.ini playbooks/xps.yml
```

Expected: changed tasks for the package, the two new files, and the modified `restic-backup.service`.

- [ ] **Step 6: Verify systemd sees the wiring**

```bash
ssh daniel@xps.fritz.box 'systemctl show restic-backup.service -p OnFailure --value'
```

Expected: `backup-alert.service`. An empty result means the unit file did not reload — run `sudo systemctl daemon-reload` and re-check.

- [ ] **Step 7: Verify idempotency**

```bash
ansible-playbook -i inventory/hosts.ini playbooks/xps.yml
```

Expected: `changed=0`.

- [ ] **Step 8: Commit**

```bash
git add roles/backup/files/backup-alert.sh roles/backup/files/backup-alert.service roles/backup/files/restic-backup.service roles/backup/tasks/main.yml
git commit -m "Alert immediately when a backup fails

BackupStale notices within 26 hours; this notices within seconds and
carries the exit status and journal tail, so the Telegram message says
what went wrong rather than just that something did.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

### Task 4: Fault-injection verification

Everything so far proves the happy path. This proves the alerts fire on real conditions.

**Files:**
- Modify: `docs/superpowers/specs/2026-08-19-observability-alerting-design.md`

**Interfaces:**
- Consumes: everything deployed in Tasks 1–3.
- Produces: nothing.

- [ ] **Step 1: Prove BackupFailed fires with a real failure**

Record the Telegram counter first:

```bash
ssh daniel@xps.fritz.box 'curl -s http://localhost:9093/metrics | grep -E "^alertmanager_notifications_total\{integration=\"telegram\""'
```

Then break the real service's credentials temporarily, so the genuine
`OnFailure=` wiring is exercised rather than a simulation of it. Back the file
up first — it holds the only copy of the restic password and the B2 keys:

```bash
ssh daniel@xps.fritz.box 'sudo cp /etc/restic/env /etc/restic/env.faulttest-backup && sudo ls -l /etc/restic/env.faulttest-backup'
```

Append an override. systemd's `EnvironmentFile` applies assignments in order,
so a later line wins:

```bash
ssh daniel@xps.fritz.box 'sudo sh -c "echo RESTIC_PASSWORD=deliberately-wrong >> /etc/restic/env"'
```

Now run the real unit:

```bash
ssh daniel@xps.fritz.box 'sudo systemctl start restic-backup.service' 2>&1 | tail -3
ssh daniel@xps.fritz.box 'systemctl show restic-backup.service -p Result --value'
```

Expected: `Result=exit-code`. The database dumps succeed (they do not use the
restic password) and `restic backup` then fails to open the repository, which
is a realistic failure rather than a contrived one.

**Restore the credentials immediately, before doing anything else:**

```bash
ssh daniel@xps.fritz.box 'sudo mv /etc/restic/env.faulttest-backup /etc/restic/env'
ssh daniel@xps.fritz.box 'sudo grep -c deliberately-wrong /etc/restic/env || echo "clean"'
```

Expected: `clean`. If the override is still present, STOP — the backup is
broken until it is removed, and every subsequent run will fail.

Expected within about 30 seconds of the failure: a 🔴 CRITICAL `BackupFailed`
message in Telegram carrying the restic error text. Confirm from the sending side:

```bash
ssh daniel@xps.fritz.box 'curl -s http://localhost:9093/metrics | grep -E "^alertmanager_notifications(_failed)?_total\{integration=\"telegram\"" | awk "\$NF != 0"'
```

Expected: the success counter has incremented, and the failed counter has not.

- [ ] **Step 2: Confirm it self-resolves**

Wait 15 minutes, then:

```bash
ssh daniel@xps.fritz.box 'curl -s http://localhost:9093/api/v2/alerts | jq -r ".[] | \"\(.labels.alertname) \(.status.state)\""'
```

Expected: `BackupFailed` is no longer active. This proves the explicit `endsAt` works — without it the alert would re-notify indefinitely.

- [ ] **Step 3: Prove BackupStale fires**

Backdate the deployed metric file by 30 hours:

```bash
ssh daniel@xps.fritz.box 'sudo sh -c "printf \"# HELP restic_backup_last_success_timestamp_seconds Unix time of the last successful restic backup to B2.\n# TYPE restic_backup_last_success_timestamp_seconds gauge\nrestic_backup_last_success_timestamp_seconds \$(( \$(date +%s) - 108000 ))\n\" > /var/lib/node_exporter/textfile_collector/restic_backup.prom"'
```

Within about a minute Prometheus scrapes the new value. Confirm the alert:

```bash
ssh daniel@xps.fritz.box 'curl -s http://localhost:9090/api/v1/alerts | jq -r ".data.alerts[] | \"\(.labels.alertname) \(.state)\""'
```

Expected: `BackupStale firing`, and a 🔴 CRITICAL Telegram message.

- [ ] **Step 4: Restore the real metric and confirm it clears**

```bash
ssh daniel@xps.fritz.box 'sudo systemctl start restic-backup.service'
```

Wait for it to finish, then confirm:

```bash
ssh daniel@xps.fritz.box 'systemctl show restic-backup.service -p Result --value'
ssh daniel@xps.fritz.box 'curl -s http://localhost:9090/api/v1/alerts | jq -r ".data.alerts[] | \"\(.labels.alertname) \(.state)\""'
```

Expected: `success`, no `BackupStale`, and a ✅ RESOLVED message in Telegram. An alert that fires but never clears trains the reader to ignore it, so this half matters as much as the first.

- [ ] **Step 5: Mark the increment delivered**

In the spec's build-order table, append ` — delivered 2026-08-20` to the increment 2 row, matching how increment 1 is marked.

- [ ] **Step 6: Commit**

```bash
git add docs/superpowers/specs/2026-08-19-observability-alerting-design.md
git commit -m "Mark alerting increment 2 as delivered

Fault injection confirmed BackupFailed fires with the real error text and
self-resolves, and that BackupStale fires on a backdated timestamp and
clears when a real backup succeeds.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

## Deliberately out of scope

- **Repository size metrics.** `restic stats` against B2 is slow and costs API calls on every run. If the unbounded-growth signal is wanted, it belongs on a weekly timer, not on every backup — and the underlying problem (the B2 key cannot delete, so nothing prunes) needs the admin key kept off the server. Tracked as a known gap in the spec.
- **Backup duration alerting.** `restic_backup_duration_seconds` is exported because it is free to collect and useful in Grafana, but no rule watches it. There is no evidence yet of what a normal duration looks like on this host; a threshold picked today would be a guess.
