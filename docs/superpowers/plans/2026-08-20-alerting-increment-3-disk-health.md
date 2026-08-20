# Alerting Increment 3: Disk Health Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Find out that a disk is failing before it takes the photo and document libraries with it.

**Architecture:** A collector script runs `smartctl --json` against an explicit list of devices on a 15-minute systemd timer and writes metrics to node-exporter's existing textfile directory. Eleven rules watch those metrics plus the RAID array state node-exporter already exports. No new services.

**Tech Stack:** Ansible, smartmontools 7.4, jq 1.7, bash 5.2, systemd timers, node-exporter textfile collector, Prometheus, promtool

**Spec:** `docs/superpowers/specs/2026-08-19-observability-alerting-design.md` (the "Disk health — SMART via textfile collector" section)

## Global Constraints

- **Alert `severity` must be exactly one of** `critical`, `warning`, `info`, `none`. The deployed Alertmanager routes on precisely these; any other value is silently delivered mislabelled as WARNING via the default route.
- **The device list is explicit, never scanned.** `smartctl --scan` on this host returns `/dev/sda` and `/dev/nvme0` and misses `/dev/sdb` — the 8TB array disk holding the photo library and the only disk whose failure loses data. An auto-scanning collector reports healthy forever while never looking at it.
- **`/dev/sda` is excluded.** Its USB bridge rejects SMART commands (`Read Device Identity failed: scsi error unsupported field in scsi command`) and the drive is being unplugged.
- **Rule files live in `roles/prometheus/files/rules/`** and are copied verbatim by Ansible, so Go template expressions in annotations (`{{ $labels.device }}`, `{{ $value }}`) need NO escaping. Files under `templates/` are Jinja-rendered and DO need `{% raw %}`.
- **Every rule gets a unit test asserting both firing AND silence.** A test asserting only firing cannot catch a rule mutated to fire always. Increment 1's review proved four such rules shipped green while arbitrarily broken.
- **The test harness is `./tests/run-promtool.sh`**, run from the repo root, using the pinned image `prom/prometheus:v3.14.0@sha256:5ce7540c3c00ef4ab0c9d2c995c6a5b9c421f44b4a115d97a2c7af3b1c21cbb0`.
- **The metric file is written via a temporary file and an atomic rename**, and the temporary name must NOT end in `.prom` or node-exporter would scrape it half-written.
- **Roles must be idempotent:** a second `ansible-playbook -i inventory/hosts.ini playbooks/xps.yml` reports zero changed tasks.
- **Never write a credential into a committed file.**
- **Ansible target:** host `xps` (`xps.fritz.box`), user `daniel`.
- Commit messages end with: `Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>`

---

## Verified facts this plan is built on

Gathered from the live host; do not re-derive them, but do re-verify anything that looks wrong.

| Fact | Value |
|---|---|
| Array disk | `/dev/sdb`, `ST8000NT001-3LZ101`, readable **only** with `-d sat` |
| System disk | `/dev/nvme0n1`, `THNSN5512GPUK NVMe TOSHIBA 512GB`, `-d nvme` |
| Excluded | `/dev/sda` — bridge rejects SMART; being unplugged |
| `smartctl --scan` | returns `/dev/sda -d sat` and `/dev/nvme0 -d nvme`; **misses `/dev/sdb`** |
| smartmontools | 7.4-2, already installed |
| jq / bash | 1.7.1 / 5.2.37 — namerefs and modern jq available |
| Textfile dir | `/var/lib/node_exporter/textfile_collector`, root:root 0755, already read by node-exporter |
| RAID | `md0`, raid1, `[2/1] [U_]` — **intentionally** single-disk until a second is added |
| md metrics | node-exporter already exports `node_md_disks{device="md0",state="active"}` and `node_md_disks_required` |
| Current health | sdb: PASSED, 0 realloc, 0 pending, 0 uncorrectable, 38°C. nvme: PASSED, critical_warning 0, 11% used, 0 media errors, 39°C |

## File Structure

**Created:**

| Path | Responsibility |
|---|---|
| `roles/prometheus/files/rules/disk.yml` | The eleven disk-health alert rules |
| `tests/rules/disk_test.yml` | Unit tests for those rules |
| `roles/prometheus/templates/smart-metrics.sh.j2` | The collector; templated because the device list is an Ansible variable |
| `roles/prometheus/files/smart-metrics.service` | Oneshot unit running the collector |
| `roles/prometheus/files/smart-metrics.timer` | 15-minute timer |
| `roles/prometheus/defaults/main.yml` | `smart_devices` — the explicit device list |

**Modified:**

| Path | Change |
|---|---|
| `roles/prometheus/tasks/main.yml` | Install smartmontools, deploy the collector, unit and timer, enable the timer |
| `docs/superpowers/specs/2026-08-19-observability-alerting-design.md` | Add `SmartMetricsMissing` to the table; mark increment 3 delivered |

---

### Task 1: The disk-health alert rules

Entirely local — nothing is deployed and no server is touched.

**Files:**
- Create: `roles/prometheus/files/rules/disk.yml`
- Create: `tests/rules/disk_test.yml`

**Interfaces:**
- Consumes: `./tests/run-promtool.sh`, which globs `roles/prometheus/files/rules/*.yml` and `tests/rules/*_test.yml`, so new files need no harness change. Also consumes `node_md_disks`, which node-exporter already exports.
- Produces: the metric-name contract Task 2 must emit **character for character**:
  `smart_collector_success` (no labels), and these carrying labels `device` and `model`:
  `smart_device_health_ok`, `smart_temperature_celsius`, `smart_ata_reallocated_sectors`, `smart_ata_pending_sectors`, `smart_ata_offline_uncorrectable`, `smart_nvme_critical_warning`, `smart_nvme_percentage_used`, `smart_nvme_media_errors`.

- [ ] **Step 1: Write the failing tests**

Create `tests/rules/disk_test.yml`:

```yaml
rule_files:
  - ../../roles/prometheus/files/rules/disk.yml

evaluation_interval: 1m

tests:
  # A healthy ATA disk must produce no alerts at all.
  - interval: 1m
    input_series:
      - series: 'smart_device_health_ok{device="/dev/sdb",model="ST8000NT001-3LZ101"}'
        values: '1+0x30'
      - series: 'smart_ata_pending_sectors{device="/dev/sdb",model="ST8000NT001-3LZ101"}'
        values: '0+0x30'
      - series: 'smart_ata_reallocated_sectors{device="/dev/sdb",model="ST8000NT001-3LZ101"}'
        values: '0+0x30'
      - series: 'smart_temperature_celsius{device="/dev/sdb",model="ST8000NT001-3LZ101"}'
        values: '38+0x30'
    alert_rule_test:
      - eval_time: 20m
        alertname: SmartHealthFailed
        exp_alerts: []
      - eval_time: 20m
        alertname: SmartPendingSectors
        exp_alerts: []
      - eval_time: 20m
        alertname: SmartReallocatedSectors
        exp_alerts: []
      - eval_time: 20m
        alertname: SmartDiskTemperature
        exp_alerts: []

  # The drive's own health self-assessment failing is the strongest signal
  # a disk gives that it is dying.
  - interval: 1m
    input_series:
      - series: 'smart_device_health_ok{device="/dev/sdb",model="ST8000NT001-3LZ101"}'
        values: '0+0x30'
    alert_rule_test:
      - eval_time: 3m
        alertname: SmartHealthFailed
        exp_alerts: []
      - eval_time: 6m
        alertname: SmartHealthFailed
        exp_alerts:
          - exp_labels:
              severity: critical
              device: /dev/sdb
              model: ST8000NT001-3LZ101

  # A single pending sector is critical: it means data could not be read.
  - interval: 1m
    input_series:
      - series: 'smart_ata_pending_sectors{device="/dev/sdb",model="ST8000NT001-3LZ101"}'
        values: '1+0x30'
    alert_rule_test:
      - eval_time: 6m
        alertname: SmartPendingSectors
        exp_alerts:
          - exp_labels:
              severity: critical
              device: /dev/sdb
              model: ST8000NT001-3LZ101

  # Reallocated sectors are a warning: the drive coped, but it is degrading.
  - interval: 1m
    input_series:
      - series: 'smart_ata_reallocated_sectors{device="/dev/sdb",model="ST8000NT001-3LZ101"}'
        values: '4+0x30'
    alert_rule_test:
      - eval_time: 6m
        alertname: SmartReallocatedSectors
        exp_alerts:
          - exp_labels:
              severity: warning
              device: /dev/sdb
              model: ST8000NT001-3LZ101

  # Offline uncorrectable sectors mean reads have already failed.
  - interval: 1m
    input_series:
      - series: 'smart_ata_offline_uncorrectable{device="/dev/sdb",model="ST8000NT001-3LZ101"}'
        values: '2+0x30'
    alert_rule_test:
      - eval_time: 6m
        alertname: SmartOfflineUncorrectable
        exp_alerts:
          - exp_labels:
              severity: critical
              device: /dev/sdb
              model: ST8000NT001-3LZ101

  # Temperature: 60C sustained fires, 50C never does.
  - interval: 1m
    input_series:
      - series: 'smart_temperature_celsius{device="/dev/sdb",model="ST8000NT001-3LZ101"}'
        values: '60+0x30'
    alert_rule_test:
      - eval_time: 10m
        alertname: SmartDiskTemperature
        exp_alerts: []
      - eval_time: 16m
        alertname: SmartDiskTemperature
        exp_alerts:
          - exp_labels:
              severity: warning
              device: /dev/sdb
              model: ST8000NT001-3LZ101
  - interval: 1m
    input_series:
      - series: 'smart_temperature_celsius{device="/dev/sdb",model="ST8000NT001-3LZ101"}'
        values: '50+0x30'
    alert_rule_test:
      - eval_time: 25m
        alertname: SmartDiskTemperature
        exp_alerts: []

  # NVMe: healthy disk is silent, then each failure signal fires.
  - interval: 1m
    input_series:
      - series: 'smart_nvme_critical_warning{device="/dev/nvme0n1",model="THNSN5512GPUK NVMe TOSHIBA 512GB"}'
        values: '0+0x30'
      - series: 'smart_nvme_media_errors{device="/dev/nvme0n1",model="THNSN5512GPUK NVMe TOSHIBA 512GB"}'
        values: '0+0x30'
      - series: 'smart_nvme_percentage_used{device="/dev/nvme0n1",model="THNSN5512GPUK NVMe TOSHIBA 512GB"}'
        values: '11+0x70'
    alert_rule_test:
      - eval_time: 20m
        alertname: NvmeCriticalWarning
        exp_alerts: []
      - eval_time: 20m
        alertname: NvmeMediaErrors
        exp_alerts: []
      - eval_time: 65m
        alertname: NvmeWearHigh
        exp_alerts: []

  - interval: 1m
    input_series:
      - series: 'smart_nvme_critical_warning{device="/dev/nvme0n1",model="THNSN5512GPUK NVMe TOSHIBA 512GB"}'
        values: '1+0x30'
    alert_rule_test:
      - eval_time: 6m
        alertname: NvmeCriticalWarning
        exp_alerts:
          - exp_labels:
              severity: critical
              device: /dev/nvme0n1
              model: THNSN5512GPUK NVMe TOSHIBA 512GB

  - interval: 1m
    input_series:
      - series: 'smart_nvme_media_errors{device="/dev/nvme0n1",model="THNSN5512GPUK NVMe TOSHIBA 512GB"}'
        values: '3+0x30'
    alert_rule_test:
      - eval_time: 6m
        alertname: NvmeMediaErrors
        exp_alerts:
          - exp_labels:
              severity: critical
              device: /dev/nvme0n1
              model: THNSN5512GPUK NVMe TOSHIBA 512GB

  - interval: 1m
    input_series:
      - series: 'smart_nvme_percentage_used{device="/dev/nvme0n1",model="THNSN5512GPUK NVMe TOSHIBA 512GB"}'
        values: '85+0x70'
    alert_rule_test:
      - eval_time: 65m
        alertname: NvmeWearHigh
        exp_alerts:
          - exp_labels:
              severity: warning
              device: /dev/nvme0n1
              model: THNSN5512GPUK NVMe TOSHIBA 512GB

  # The collector reporting failure, and the collector having vanished
  # entirely, are separate rules for the same reason BackupStale and
  # BackupMetricMissing are: a comparison cannot fire on an absent series.
  - interval: 1m
    input_series:
      - series: 'smart_collector_success'
        values: '0+0x40'
    alert_rule_test:
      - eval_time: 20m
        alertname: SmartCollectorFailed
        exp_alerts: []
      - eval_time: 35m
        alertname: SmartCollectorFailed
        exp_alerts:
          - exp_labels:
              severity: warning
  - interval: 1m
    input_series:
      - series: 'smart_collector_success'
        values: '1+0x40'
    alert_rule_test:
      - eval_time: 35m
        alertname: SmartCollectorFailed
        exp_alerts: []
      - eval_time: 35m
        alertname: SmartMetricsMissing
        exp_alerts: []
  - interval: 1m
    input_series: []
    alert_rule_test:
      - eval_time: 30m
        alertname: SmartMetricsMissing
        exp_alerts: []
      - eval_time: 65m
        alertname: SmartMetricsMissing
        exp_alerts:
          - exp_labels:
              severity: warning

  # The array losing its last disk. A merely degraded array is the intended
  # state today and must stay silent.
  - interval: 1m
    input_series:
      - series: 'node_md_disks{device="md0",state="active",instance="xps",job="node"}'
        values: '1+0x30'
    alert_rule_test:
      - eval_time: 20m
        alertname: MdArrayFailed
        exp_alerts: []
  - interval: 1m
    input_series:
      - series: 'node_md_disks{device="md0",state="active",instance="xps",job="node"}'
        values: '0+0x30'
    alert_rule_test:
      - eval_time: 3m
        alertname: MdArrayFailed
        exp_alerts: []
      - eval_time: 6m
        alertname: MdArrayFailed
        exp_alerts:
          - exp_labels:
              severity: critical
              device: md0
              state: active
              instance: xps
              job: node
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `./tests/run-promtool.sh`

Expected: FAIL. `promtool check rules` errors because `roles/prometheus/files/rules/disk.yml` does not exist.

- [ ] **Step 3: Write the rules**

Create `roles/prometheus/files/rules/disk.yml`:

```yaml
groups:
  - name: disk
    rules:
      # The array behind /mnt/tmdas is deliberately running as a single disk
      # until a second one is added, so there is no redundancy to absorb a
      # failure. SMART warning is the only notice that arrives before data
      # starts failing to read.

      - alert: SmartHealthFailed
        expr: smart_device_health_ok == 0
        for: 5m
        labels:
          severity: critical
        annotations:
          summary: "{{ $labels.device }} reports SMART health FAILED"
          description: "{{ $labels.model }} at {{ $labels.device }} has failed its own health self-assessment. This is the strongest warning a drive gives. Treat the disk as dying: verify the restic backup is current before doing anything else."

      - alert: SmartPendingSectors
        # Sectors the drive could not read and has not yet remapped. Unlike
        # reallocated sectors, these represent data that is currently
        # unreadable, not damage the drive has already worked around.
        expr: smart_ata_pending_sectors > 0
        for: 5m
        labels:
          severity: critical
        annotations:
          summary: "{{ $labels.device }} has {{ $value }} sectors pending reallocation"
          description: "{{ $labels.model }} at {{ $labels.device }} could not read {{ $value }} sectors. Any file occupying them is already damaged. Check the restic backup covers this disk's contents."

      - alert: SmartReallocatedSectors
        # The drive coped by remapping to spare sectors, so nothing is lost
        # yet — but the count only ever rises, and a rising count predicts
        # failure.
        expr: smart_ata_reallocated_sectors > 0
        for: 5m
        labels:
          severity: warning
        annotations:
          summary: "{{ $labels.device }} has reallocated {{ $value }} sectors"
          description: "{{ $labels.model }} at {{ $labels.device }} has remapped {{ $value }} bad sectors to spares. Not urgent on its own, but the count never goes down — watch whether it climbs."

      - alert: SmartOfflineUncorrectable
        expr: smart_ata_offline_uncorrectable > 0
        for: 5m
        labels:
          severity: critical
        annotations:
          summary: "{{ $labels.device }} has {{ $value }} uncorrectable sectors"
          description: "{{ $labels.model }} at {{ $labels.device }} hit {{ $value }} sectors it could not read even with error correction. Data in them is gone."

      - alert: SmartDiskTemperature
        expr: smart_temperature_celsius > 55
        for: 15m
        labels:
          severity: warning
        annotations:
          summary: "{{ $labels.device }} running at {{ $value }}C"
          description: "{{ $labels.model }} at {{ $labels.device }} has been above 55C for 15 minutes. Sustained heat shortens drive life; check enclosure airflow."

      - alert: NvmeCriticalWarning
        # A bitfield: any non-zero bit means spare exhaustion, a temperature
        # excursion, degraded reliability, read-only fallback, or failed
        # volatile-memory backup. One rule covers all of them.
        expr: smart_nvme_critical_warning > 0
        for: 5m
        labels:
          severity: critical
        annotations:
          summary: "{{ $labels.device }} raised an NVMe critical warning"
          description: "{{ $labels.model }} at {{ $labels.device }} set critical_warning to {{ $value }}. Run `smartctl -a {{ $labels.device }}` to see which condition: spare exhausted, overheating, degraded reliability, or read-only fallback."

      - alert: NvmeMediaErrors
        expr: smart_nvme_media_errors > 0
        for: 5m
        labels:
          severity: critical
        annotations:
          summary: "{{ $labels.device }} reported {{ $value }} media errors"
          description: "{{ $labels.model }} at {{ $labels.device }} logged {{ $value }} media and data integrity errors — data it could not return correctly."

      - alert: NvmeWearHigh
        expr: smart_nvme_percentage_used > 80
        for: 1h
        labels:
          severity: warning
        annotations:
          summary: "{{ $labels.device }} is {{ $value }}% through its endurance"
          description: "{{ $labels.model }} at {{ $labels.device }} reports {{ $value }}% of its rated write endurance consumed. Not a failure, but plan a replacement."

      - alert: SmartCollectorFailed
        expr: smart_collector_success == 0
        for: 30m
        labels:
          severity: warning
        annotations:
          summary: "The SMART collector could not read every disk"
          description: "smart-metrics.sh ran but failed to read at least one configured device, so some disks are unmonitored. Check `journalctl -u smart-metrics.service`."

      - alert: SmartMetricsMissing
        # Not redundant with SmartCollectorFailed: that compares a value, and
        # an absent series has no value to compare. Without this, a collector
        # that never runs at all leaves every SMART rule silently unarmed.
        expr: absent(smart_collector_success)
        for: 1h
        labels:
          severity: warning
        annotations:
          summary: "SMART metrics are missing entirely"
          description: "No smart_collector_success metric is being exported, so every disk-health rule is unarmed. Either the timer is not running or the textfile collector is broken — check `systemctl status smart-metrics.timer` and that /var/lib/node_exporter/textfile_collector/smart.prom exists."

      - alert: MdArrayFailed
        # Deliberately NOT an alert on a degraded array: md0 intentionally
        # runs as a single disk until a second is added, so a degraded-array
        # alert would fire permanently. This fires only when the array has no
        # working disk left at all. TIGHTEN THIS when the second disk is
        # added, to compare against node_md_disks_required.
        expr: node_md_disks{state="active"} == 0
        for: 5m
        labels:
          severity: critical
        annotations:
          summary: "RAID array {{ $labels.device }} has no active disks"
          description: "{{ $labels.device }} has lost every member disk. The filesystem on it is unreadable. Only paths covered by the restic backup to B2 survive."
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `./tests/run-promtool.sh`

Expected: PASS, with SUCCESS for `disk_test.yml` alongside the existing files.

If promtool reports an annotation mismatch, this version compares annotations and treats an omitted `exp_annotations` as "expect none". Add `exp_annotations` blocks copying the exact rendered text from promtool's failure output rather than predicting it.

- [ ] **Step 5: Verify the tests constrain the rules (mutation testing)**

Apply each mutation, confirm the suite FAILS, revert, confirm it passes. Report before and after for each.

| Mutation | Expected |
|---|---|
| `smart_device_health_ok == 0` → `== 1` | FAIL |
| `smart_ata_pending_sectors > 0` → `> 99999` | FAIL |
| `smart_ata_reallocated_sectors > 0` → `>= 0` | FAIL |
| `smart_temperature_celsius > 55` → `> 5` | FAIL |
| `smart_nvme_critical_warning > 0` → `> 99999` | FAIL |
| `smart_nvme_percentage_used > 80` → `> 5` | FAIL |
| `absent(smart_collector_success)` → `absent(nonexistent_metric)` | FAIL |
| `node_md_disks{state="active"} == 0` → `>= 0` | FAIL |
| `SmartHealthFailed` severity `critical` → `warning` | FAIL |

A mutation that still passes means that rule's assertions are not doing their job — strengthen them before continuing.

- [ ] **Step 6: Commit**

```bash
git add roles/prometheus/files/rules/disk.yml tests/rules/disk_test.yml
git commit -m "Add disk health alert rules

The array behind /mnt/tmdas deliberately runs as a single disk until a
second is added, so there is no redundancy to absorb a failure and SMART
warning is the only notice that arrives before data stops being readable.

MdArrayFailed fires only on total array failure, not on a degraded array:
the single-disk state is intentional and a degraded-array alert would fire
permanently, which is the fastest way to train a reader to ignore Telegram.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

### Task 2: The SMART collector

**Files:**
- Create: `roles/prometheus/defaults/main.yml`
- Create: `roles/prometheus/templates/smart-metrics.sh.j2`
- Create: `roles/prometheus/files/smart-metrics.service`
- Create: `roles/prometheus/files/smart-metrics.timer`
- Modify: `roles/prometheus/tasks/main.yml`

**Interfaces:**
- Consumes: the metric names fixed by Task 1. They must match character for character or the rules watch nothing while their tests still pass.
- Produces: `/var/lib/node_exporter/textfile_collector/smart.prom` on the server, scraped by the existing `node` job with labels `instance="xps"`, `job="node"`.

- [ ] **Step 1: Declare the device list**

Create `roles/prometheus/defaults/main.yml`:

```yaml
# Explicit, not scanned. `smartctl --scan` on this host returns /dev/sda and
# /dev/nvme0 and misses /dev/sdb entirely — the 8TB array disk holding the
# photo library, which answers SMART only when told `-d sat`. Scanning would
# monitor the wrong disks and report healthy forever.
#
# /dev/sda is omitted deliberately: its USB bridge rejects SMART commands.
smart_devices:
  - device: /dev/sdb
    type: sat
  - device: /dev/nvme0n1
    type: nvme
```

- [ ] **Step 2: Write the collector**

Create `roles/prometheus/templates/smart-metrics.sh.j2`:

```bash
#!/bin/bash
# Writes SMART health metrics for an explicit list of devices into
# node-exporter's textfile collector directory.
#
# No `set -e`: one unreadable disk must not stop the others being reported,
# and it must not stop smart_collector_success being written — that gauge is
# how a broken collector becomes visible instead of looking like healthy
# silence.
set -uo pipefail

TEXTFILE_DIR=/var/lib/node_exporter/textfile_collector
OUT="${TEXTFILE_DIR}/smart.prom"
# Must not end in .prom, or node-exporter would scrape it half-written.
TMP="${OUT}.tmp"

DEVICES=(
{% for d in smart_devices %}
  "{{ d.device }}|{{ d.type }}"
{% endfor %}
)

declare -A MODEL HEALTH TEMP REALLOC PENDING UNCORR NCRIT NUSED NMEDIA
COLLECTOR_SUCCESS=1

for entry in "${DEVICES[@]}"; do
  dev="${entry%%|*}"
  dtype="${entry##*|}"

  # smartctl's exit status is a bitfield that is non-zero for benign
  # conditions too, so success is judged on the JSON parsing instead.
  json=$(smartctl --json=c -d "$dtype" -i -H -A "$dev" 2>/dev/null)
  if ! printf '%s' "$json" | jq -e 'has("smart_status")' >/dev/null 2>&1; then
    COLLECTOR_SUCCESS=0
    continue
  fi

  MODEL[$dev]=$(printf '%s' "$json" | jq -r '.model_name // "unknown"')
  HEALTH[$dev]=$(printf '%s' "$json" | jq -r 'if .smart_status.passed then 1 else 0 end')
  TEMP[$dev]=$(printf '%s' "$json" | jq -r '.temperature.current // empty')

  if [ "$dtype" = "nvme" ]; then
    NCRIT[$dev]=$(printf '%s' "$json" | jq -r '.nvme_smart_health_information_log.critical_warning // empty')
    NUSED[$dev]=$(printf '%s' "$json" | jq -r '.nvme_smart_health_information_log.percentage_used // empty')
    NMEDIA[$dev]=$(printf '%s' "$json" | jq -r '.nvme_smart_health_information_log.media_errors // empty')
  else
    REALLOC[$dev]=$(printf '%s' "$json" | jq -r '[.ata_smart_attributes.table[]? | select(.id == 5) | .raw.value] | first // empty')
    PENDING[$dev]=$(printf '%s' "$json" | jq -r '[.ata_smart_attributes.table[]? | select(.id == 197) | .raw.value] | first // empty')
    UNCORR[$dev]=$(printf '%s' "$json" | jq -r '[.ata_smart_attributes.table[]? | select(.id == 198) | .raw.value] | first // empty')
  fi
done

# Samples for one metric name must be contiguous in the exposition format,
# so each family is emitted in full before the next begins.
emit() {
  local name=$1 help=$2 arrname=$3
  local -n values=$arrname
  [ "${#values[@]}" -eq 0 ] && return 0
  printf '# HELP %s %s\n' "$name" "$help"
  printf '# TYPE %s gauge\n' "$name"
  local dev
  for dev in "${!values[@]}"; do
    # An attribute the drive does not report is skipped rather than written
    # with an empty value, which would make the whole file unparseable.
    [ -z "${values[$dev]}" ] && continue
    printf '%s{device="%s",model="%s"} %s\n' "$name" "$dev" "${MODEL[$dev]}" "${values[$dev]}"
  done
}

{
  emit smart_device_health_ok "1 if the drive passes its own SMART health self-assessment." HEALTH
  emit smart_temperature_celsius "Current drive temperature in Celsius." TEMP
  emit smart_ata_reallocated_sectors "ATA attribute 5: sectors remapped to spares after a failure." REALLOC
  emit smart_ata_pending_sectors "ATA attribute 197: unreadable sectors awaiting reallocation." PENDING
  emit smart_ata_offline_uncorrectable "ATA attribute 198: sectors that could not be read even with error correction." UNCORR
  emit smart_nvme_critical_warning "NVMe critical warning bitfield; non-zero means spare exhaustion, temperature excursion, degraded reliability or read-only fallback." NCRIT
  emit smart_nvme_percentage_used "NVMe rated write endurance consumed, as a percentage." NUSED
  emit smart_nvme_media_errors "NVMe media and data integrity errors." NMEDIA
  printf '# HELP smart_collector_success 1 if SMART was read successfully for every configured device.\n'
  printf '# TYPE smart_collector_success gauge\n'
  printf 'smart_collector_success %s\n' "$COLLECTOR_SUCCESS"
} > "$TMP"

mv "$TMP" "$OUT"
```

- [ ] **Step 3: Write the unit and timer**

Create `roles/prometheus/files/smart-metrics.service`:

```ini
[Unit]
Description=Collect SMART disk health metrics for Prometheus

[Service]
Type=oneshot
ExecStart=/usr/local/bin/smart-metrics.sh
# smartctl needs root to issue device commands.
User=root
```

Create `roles/prometheus/files/smart-metrics.timer`:

```ini
[Unit]
Description=Collect SMART disk health metrics every 15 minutes

[Timer]
OnBootSec=5min
OnUnitActiveSec=15min
Persistent=true

[Install]
WantedBy=timers.target
```

- [ ] **Step 4: Wire it into the role**

In `roles/prometheus/tasks/main.yml`, change the node-exporter install task from:

```yaml
- name: Ensure node-exporter is installed
  ansible.builtin.package:
    name: prometheus-node-exporter
    state: present
```

to:

```yaml
- name: Ensure node-exporter and smartmontools are installed
  # smartmontools provides smartctl, which the SMART collector runs.
  ansible.builtin.package:
    name:
      - prometheus-node-exporter
      - smartmontools
    state: present
```

Then add these tasks immediately after the existing "Start and enable node-exporter" task:

```yaml
- name: Template the SMART collector
  ansible.builtin.template:
    src: smart-metrics.sh.j2
    dest: /usr/local/bin/smart-metrics.sh
    owner: root
    group: root
    mode: '0755'

- name: Install the SMART collector unit
  ansible.builtin.copy:
    src: smart-metrics.service
    dest: /etc/systemd/system/smart-metrics.service
    owner: root
    group: root
    mode: '0644'
  notify: Reload systemd for SMART collector

- name: Install the SMART collector timer
  ansible.builtin.copy:
    src: smart-metrics.timer
    dest: /etc/systemd/system/smart-metrics.timer
    owner: root
    group: root
    mode: '0644'
  notify: Reload systemd for SMART collector

- name: Flush handlers so systemd sees the new units before enabling the timer
  ansible.builtin.meta: flush_handlers

- name: Enable and start the SMART collector timer
  ansible.builtin.systemd:
    name: smart-metrics.timer
    enabled: true
    state: started
    daemon_reload: true
```

Add to `roles/prometheus/handlers/main.yml`:

```yaml
- name: Reload systemd for SMART collector
  ansible.builtin.systemd:
    daemon_reload: true
```

- [ ] **Step 5: Deploy**

```bash
ansible-playbook -i inventory/hosts.ini playbooks/xps.yml
```

- [ ] **Step 6: Run the collector once and inspect its output**

```bash
ssh daniel@xps.fritz.box 'sudo systemctl start smart-metrics.service'
ssh daniel@xps.fritz.box 'systemctl show smart-metrics.service -p Result --value'
```

Expected: `success`.

```bash
ssh daniel@xps.fritz.box 'cat /var/lib/node_exporter/textfile_collector/smart.prom'
```

Expected: metrics for BOTH `/dev/sdb` and `/dev/nvme0n1`, `smart_collector_success 1`, and — critically — samples for each metric name grouped together rather than interleaved by device.

**If `/dev/sdb` is missing from the output, STOP and report.** That disk is the entire point of this increment; a file containing only the NVMe is the exact failure the explicit device list exists to prevent.

```bash
ssh daniel@xps.fritz.box 'ls /var/lib/node_exporter/textfile_collector/'
```

Expected: `restic_backup.prom` and `smart.prom`, no `.tmp`.

- [ ] **Step 7: Verify node-exporter parses it and Prometheus scrapes it**

```bash
ssh daniel@xps.fritz.box 'curl -s http://localhost:9100/metrics | grep node_textfile_scrape_error'
```

Expected: `node_textfile_scrape_error 0`. A `1` means the file is malformed and node-exporter has dropped **all** textfile metrics, including the backup ones.

```bash
ssh daniel@xps.fritz.box 'curl -s "http://localhost:9090/api/v1/query?query=smart_device_health_ok" | jq -c ".data.result[] | {device:.metric.device, model:.metric.model, value:.value[1]}"'
```

Expected: two results, one per disk, both value `1`.

- [ ] **Step 8: Verify the timer is scheduled**

```bash
ssh daniel@xps.fritz.box 'systemctl list-timers smart-metrics.timer --no-pager'
```

Expected: a next elapse within 15 minutes.

- [ ] **Step 9: Verify idempotency**

```bash
ansible-playbook -i inventory/hosts.ini playbooks/xps.yml
```

Expected: `changed=0`.

- [ ] **Step 10: Commit**

```bash
git add roles/prometheus/defaults/main.yml roles/prometheus/templates/smart-metrics.sh.j2 roles/prometheus/files/smart-metrics.service roles/prometheus/files/smart-metrics.timer roles/prometheus/tasks/main.yml roles/prometheus/handlers/main.yml
git commit -m "Collect SMART metrics from an explicit device list

smartctl --scan misses /dev/sdb, the 8TB array disk holding the photo and
document libraries, because its USB bridge answers only to -d sat. A
scanning collector would have monitored a dead bridge and the system SSD,
reported healthy forever, and never looked at the disk whose failure loses
data.

smart_collector_success makes a broken collector visible rather than
leaving it as healthy-looking silence.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

### Task 3: Fault injection and spec update

**Files:**
- Modify: `docs/superpowers/specs/2026-08-19-observability-alerting-design.md`

**Interfaces:**
- Consumes: everything from Tasks 1 and 2.
- Produces: nothing.

- [ ] **Step 1: Prove a SMART alert fires**

Record the Telegram counter first:

```bash
ssh daniel@xps.fritz.box 'curl -s http://localhost:9093/metrics | grep -E "^alertmanager_notifications_total\{integration=\"telegram\""'
```

The disks are healthy, so provoke the alert by editing the DEPLOYED metric file rather than the rules — the repo copy and the thresholds stay untouched, and the next collector run overwrites it:

```bash
ssh daniel@xps.fritz.box 'sudo sed -i "s|^smart_ata_reallocated_sectors\(.*\) 0$|smart_ata_reallocated_sectors\1 7|" /var/lib/node_exporter/textfile_collector/smart.prom'
ssh daniel@xps.fritz.box 'grep reallocated /var/lib/node_exporter/textfile_collector/smart.prom'
```

Expected: the value now reads 7.

Wait for the scrape and the 5-minute `for:` to elapse, polling. Do NOT use a local `sleep`; sleep on the remote host inside the ssh command, for example
`ssh daniel@xps.fritz.box 'sleep 120; curl -s http://localhost:9090/api/v1/alerts | jq -r ".data.alerts[].labels.alertname"'`

Expected within about 7 minutes: `SmartReallocatedSectors` firing, and a ⚠️ WARNING Telegram message naming the model and device.

- [ ] **Step 2: Confirm it clears**

```bash
ssh daniel@xps.fritz.box 'sudo systemctl start smart-metrics.service'
```

This rewrites the file with the real value of 0. Poll until `SmartReallocatedSectors` disappears from the alerts API.

Expected: a ✅ RESOLVED Telegram message. An alert that fires but never clears trains the reader to ignore it, so this half matters as much as the first.

- [ ] **Step 3: Prove the collector-failure path**

Point the collector at a device that does not exist, so it fails for a real reason rather than a simulated one:

```bash
ssh daniel@xps.fritz.box 'sudo sed -i "s|/dev/nvme0n1|nvme9n9|" /usr/local/bin/smart-metrics.sh'
ssh daniel@xps.fritz.box 'sudo systemctl start smart-metrics.service'
ssh daniel@xps.fritz.box 'grep smart_collector_success /var/lib/node_exporter/textfile_collector/smart.prom'
```

Expected: `smart_collector_success 0`, and `/dev/sdb`'s metrics still present — one bad device must not suppress the others.

Restore immediately by re-running Ansible, which rewrites the script from the template:

```bash
ansible-playbook -i inventory/hosts.ini playbooks/xps.yml
ssh daniel@xps.fritz.box 'sudo systemctl start smart-metrics.service'
ssh daniel@xps.fritz.box 'grep smart_collector_success /var/lib/node_exporter/textfile_collector/smart.prom'
```

Expected: `smart_collector_success 1`.

`SmartCollectorFailed` has a 30-minute `for:`, so it will not have fired during this — that is intended, and the point of the step is that the gauge flips, not that a message arrives.

- [ ] **Step 4: Update the spec**

Add the two alerts the plan ships beyond the spec's table, in the same style as the others:

```
| `SmartOfflineUncorrectable` | sectors unreadable even with error correction, above zero | critical |
| `SmartMetricsMissing` | no SMART metrics are exported at all | warning |
```

with a sentence after the table explaining it is not redundant with `SmartCollectorFailed`: that rule compares a value, and an absent series has no value to compare, so without `SmartMetricsMissing` a collector that never runs leaves every disk rule silently unarmed.

Then mark increment 3 delivered in the build-order table by appending ` — delivered 2026-08-20` to its row, matching increments 1 and 2.

- [ ] **Step 5: Commit**

```bash
git add docs/superpowers/specs/2026-08-19-observability-alerting-design.md
git commit -m "Mark alerting increment 3 as delivered

Fault injection confirmed SmartReallocatedSectors fires on a real metric
change and clears when the collector rewrites the true value, and that a
device that cannot be read flips smart_collector_success without
suppressing the disks that can.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

## Deliberately out of scope

- **Alerting on a degraded array.** `md0` intentionally runs as a single disk, so a degraded-array alert would fire permanently. `MdArrayFailed` covers total loss instead. The spec records that this must be tightened when the second disk is added.
- **SMART self-tests.** `smartctl -t short` on a schedule would surface problems the passive attributes miss, but it needs its own timer, its own result-parsing, and a decision about running long tests on a disk holding live data. Worth its own increment if the passive metrics ever prove insufficient.
- **`/dev/sda`.** Its USB bridge rejects SMART outright and the drive is being unplugged.
