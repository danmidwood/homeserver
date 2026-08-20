# Observability and Alerting Design

Date: 2026-08-19
Status: Approved, ready for implementation planning

## Overview

The home server currently has no alerting. Prometheus and node-exporter are
installed as pacman packages solely to give Grafana something to draw, with
stock configuration: the `alertmanagers:` block is commented out and
`rule_files:` is empty. Nothing watches the containers, nothing watches the
restic backup, and nothing tells Daniel when any of it breaks.

This design adds a complete alerting path that delivers to Telegram, and
replaces the host-native metrics tier with digest-pinned containers so the
whole stack matches the pinning convention established for every other
service.

### Goals

- Find out when the offsite backup fails, and — more importantly — when it
  silently stops running at all.
- Find out when a disk is filling, a filesystem goes read-only, or a disk is
  predicting its own failure.
- Find out when a container has died, is restart-looping, or was OOM-killed.
- Find out when a Caddy-fronted service stops answering, or a certificate
  stops renewing.
- Find out when the server itself is dead, via an off-box dead-man's-switch.
- Keep all of the above declarative and version-controlled in this repository.

### Non-goals

- Inbound Telegram commands (`/status`, `/backup_now`, `/restart`, file
  inbox). Deferred to a separate design; see "Phase 2" below.
- Log aggregation. Metrics only.
- Pruning the restic repository. A real problem — the B2 key deliberately has
  no delete capability, so the repository grows without bound — but it is a
  backup-lifecycle concern, not an observability one, and needs the admin key
  that is kept off the server. Tracked separately.

## Decisions

Each of these was a genuine fork during design; recording the reasoning so a
future reader does not have to re-derive it.

### Prometheus rules plus Alertmanager, rather than Grafana alerting

Grafana has had unified alerting since v8 and can send to Telegram itself,
which would have meant zero new services. It was rejected because Grafana's
alert rules live in Grafana's own database rather than in this repository,
placing them outside the infrastructure-as-code boundary that everything else
here respects, and losing them if the volume is lost. Prometheus rules are
plain YAML files that can be reviewed, tested and version-controlled.

Netdata was also considered — hundreds of sensible alarms out of the box and
by far the shortest path to coverage — but rejected for the same
config-as-code reason, and because it would mean discarding the
industry-standard tooling rather than building on it.

### Containerise the metrics tier

Prometheus, Alertmanager, blackbox exporter and cAdvisor all run as
digest-pinned containers on `caddy_network`, joining Grafana.

The alternative considered was moving Grafana onto the host to sit alongside a
native Prometheus. That was rejected because it does not remove the
host/container boundary, it merely relocates it: with Grafana native, Caddy
(a container) has to reach in to Grafana instead of Grafana reaching out to
Prometheus.

The deciding argument is pinning. Commit d69771a pinned every container image
to a digest. Native pacman packages on a rolling-release distribution are the
opposite of that: `pacman -Syu` moves Prometheus, Alertmanager and Grafana to
whatever upstream shipped that week, with breaking configuration changes
arriving unannounced. Containerising the metrics tier is the choice that is
consistent with the direction the repository has already taken.

Containers address each other by name (`http://prometheus:9090`,
`http://alertmanager:9093`) with no bridge-gateway addressing anywhere in the
web tier.

### node-exporter stays native

Its job is measuring the host. Containerising it means bind-mounting `/proc`,
`/sys` and `/` and fighting namespaces for worse data. It remains a pacman
package under systemd.

This leaves exactly one host/container crossing: the Prometheus container
scraping node-exporter on the host, handled with Ansible's `etc_hosts`
(`host.docker.internal:host-gateway`) and a scrape target in
`prometheus.yml`. That crossing is declared in a file in this repository,
which is the property that matters.

### The existing Prometheus is replaced, not migrated

No historical data is carried across. Daniel confirmed there is nothing in the
existing time-series database worth keeping, which removes the only
complicated step from the migration.

### All notifications route through Alertmanager

Two event sources bypass Prometheus and inject alerts directly into
Alertmanager's API: the backup unit's `OnFailure=` handler, and Diun's
image-update notifications. Sending them straight to Telegram instead would
have been simpler and would fail independently of Alertmanager's health, but
would mean three senders formatting messages three different ways, and
silences that do not apply to events. A single exit to Telegram keeps
formatting, grouping and silencing in one configuration.

### Alertmanager and Prometheus are LAN-only

Both ship with no authentication whatsoever — anyone who can reach
Alertmanager can silence every alert. They publish host ports (`:9093` and
`:9090`) reachable on the LAN and are deliberately absent from the Caddyfile,
following the same "non-public" pattern already chosen for Portainer in
commit b416ef7.

### Prometheus data lives on local disk, not the DAS

Docker's volume directory is bind-mounted to `/mnt/tmdas/dockervolumes`, so a
named volume would place the time-series database on the external array.
Prometheus writes every 15 seconds indefinitely, which would keep those disks
permanently busy. The TSDB is therefore bind-mounted to
`/mnt/storage/config/prometheus/data`, on local disk.

## Architecture

```
SENSING                          DECIDING              DELIVERING

node_exporter  :9100 ─┐
  + textfile collector │
  + SMART metrics      │
cAdvisor       :8080 ─┼─ scrape ─→ Prometheus ─ fire ─→ Alertmanager ──→ Telegram
blackbox       :9115 ─┘             :9090      alerts     :9093       (native receiver)
                                        │                    ▲
                                        │  Watchdog          │
                                        └──  (always ────────┤──→ Healthchecks.io
                                              firing)        │      (webhook)
restic-backup.service ── OnFailure ──→ notify script ────────┤
                                       (POST /api/v2/alerts) │
Diun (container) ──────── on new image tag ──────────────────┘
```

Three sensors feed Prometheus, which evaluates rules. Two event sources inject
alerts directly into Alertmanager. Alertmanager is the single exit point.

### What runs where

| Component | Form | Network | Ports |
|---|---|---|---|
| Prometheus | pinned container | `caddy_network` | `9090` on host, LAN only |
| Alertmanager | pinned container | `caddy_network` | `9093` on host, LAN only |
| cAdvisor | pinned container | `caddy_network` | `8080` on host |
| blackbox exporter | pinned container | `caddy_network` | internal only |
| Diun | pinned container | `caddy_network` | none |
| Grafana | pinned container (existing) | `caddy_network` | via Caddy |
| node-exporter | pacman package (existing) | host | `9100` |

Nothing new is added to the Caddyfile.

### One-shot events need explicit expiry

Alertmanager's model is that an alert is firing until it stops being sent. A
backup failure or a new image tag is a moment, not a state; pushed naively it
would re-notify indefinitely. Both event paths therefore set an explicit
`endsAt` a few minutes in the future so the alert self-resolves.

### Risk: Diun's payload shape

Diun's `webhook` notifier posts Diun's own JSON schema, and it is not
confirmed that it can be templated into the `/api/v2/alerts` format
Alertmanager requires. Diun also ships a `script` notifier that runs a command
with `DIUN_ENTRY_*` environment variables set, which would allow the payload
to be constructed directly.

Resolution during implementation: try the `script` notifier first. If neither
works cleanly, fall back to Diun's built-in Telegram notifier talking directly
to Telegram. That fallback costs grouping on image updates and nothing else,
and does not affect any other part of this design.

## Alert rules

### Host health — node-exporter, no new sensors

| Alert | Condition | Severity |
|---|---|---|
| `FilesystemReadOnly` | `node_filesystem_readonly == 1` (excluding pseudo-filesystems by `fstype`) for 5m — usually the first visible sign of a failing disk | critical |
| `DiskSpaceCritical` | under 5% free for 15m | critical |
| `DiskSpaceLow` | under 15% free for 1h | warning |
| `VarSpaceCritical` | under 15% free on `/var` for 1h | critical |
| `DiskWillFillSoon` | `predict_linear(node_filesystem_avail_bytes[6h], 4*24*3600) < 0` | warning |
| `MemoryPressure` | under 10% `MemAvailable` for 15m | warning |
| `CPUThermalThrottle` | over 85°C for 10m on any sensor under `chip="platform_coretemp_0"` — package and per-core alike, since the server is a laptop | warning |
| `HostRebooted` | uptime under 5m — a server that reboots by itself is news | info |

The disk rules watch every real filesystem rather than an enumerated list, so a
mount added later is covered without editing rules. Pseudo-filesystems are
excluded by `fstype`, and `/mnt/seagate` by mountpoint.

`/var` is carved out of `DiskSpaceLow` and given its own rule at the same
threshold but `critical` severity. A full `/var` stops Docker writing and
journald logging, which takes the services down — a materially worse outcome
than a merely untidy filesystem, and not one worth a `warning` the reader may
sit on.

### Disk health — SMART via textfile collector

A collector script runs `smartctl` on a 15-minute systemd timer and writes
metrics to node-exporter's textfile directory. This matters because
`/mnt/tmdas` holds the photo and document libraries, and the array behind it
is deliberately running as a single disk until a second one is added — so
there is no redundancy to absorb a failure.

**The device list is explicit, not scanned.** `smartctl --scan` on this host
returns `/dev/sda` and `/dev/nvme0`, and misses `/dev/sdb` entirely — the
8TB array disk, the only one whose failure would lose data. That disk answers
SMART only when told `-d sat`. An auto-scanning collector would therefore
report healthy forever while never looking at the disk that matters, which is
the precise failure this project exists to prevent. `/dev/sda` is excluded: its
USB bridge rejects SMART commands outright, and the drive is being unplugged.

The collector uses `smartctl --json` and parses with `jq` rather than scraping
text, and it emits a `smart_collector_success` gauge so a broken collector is
itself detectable — the same reasoning as `BackupMetricMissing`.

The two disks report different attribute families, so each needs its own
rules. NVMe's `critical_warning` is a bitfield covering spare exhaustion,
temperature excursions, degraded reliability and read-only fallback, so one
rule on it replaces several.

| Alert | Condition | Severity |
|---|---|---|
| `SmartHealthFailed` | the drive's own overall health self-assessment fails | critical |
| `SmartPendingSectors` | sectors awaiting reallocation, above zero | critical |
| `SmartReallocatedSectors` | reallocated sector count above zero | warning |
| `SmartDiskTemperature` | drive above 55°C for 15m | warning |
| `NvmeCriticalWarning` | NVMe `critical_warning` bitfield non-zero | critical |
| `NvmeMediaErrors` | NVMe media and data integrity errors above zero | critical |
| `NvmeWearHigh` | NVMe `percentage_used` above 80% | warning |
| `SmartCollectorFailed` | the collector script itself failed or stopped running | warning |
| `MdArrayFailed` | `md0` has zero active disks | critical |

`MdArrayFailed` needs no new sensor: node-exporter already exports
`node_md_disks{device="md0",state="active"}`. It deliberately does NOT alert
on a merely degraded array, because a single-disk array is the current
intended state and such an alert would fire permanently — the fastest way to
train a reader to ignore Telegram. **When the second disk is added, tighten
this to alert on any degradation**, comparing active disks against
`node_md_disks_required`.

### Backup integrity — textfile collector plus event push

| Alert | Condition | Severity |
|---|---|---|
| `BackupFailed` | pushed by `OnFailure=` the instant the unit exits non-zero, carrying exit code and last journal lines | critical |
| `BackupStale` | `time() - restic_backup_last_success_timestamp_seconds > 26*3600` | critical |
| `BackupMetricMissing` | `absent(restic_backup_last_success_timestamp_seconds)` | critical |

`BackupMetricMissing` is not redundant with `BackupStale`. `BackupStale`
compares a timestamp; if the textfile was never written there is no timestamp
to compare and the rule silently never fires. `absent()` catches exactly that
case, which is the failure mode where the backup appears covered and is not.

`restic-backup.sh` writes `restic_backup_last_success_timestamp_seconds` and
`restic_backup_duration_seconds` to the textfile directory, via a temporary
file and an atomic rename so a partially-written file is never scraped.

Repository size is deliberately not exported. `restic stats` against B2 is
slow and costs API calls on every run. If the unbounded-growth signal is
wanted later it belongs on a weekly timer, not on every backup.

### Container health — cAdvisor

| Alert | Condition | Severity |
|---|---|---|
| `ContainerMissing` | an expected container not seen for 5m | critical |
| `ContainerRestartLoop` | more than 3 starts in an hour | warning |
| `ContainerOOMKilled` | any OOM kill event | warning |

`ContainerMissing` needs a list of what is expected. Ansible already knows
every container name because it creates them, so the rule file is templated
from a `monitored_containers` variable. Adding a service to the playbook then
adds its alert automatically rather than depending on someone remembering.

### Reachability and TLS — blackbox exporter

| Alert | Condition | Severity |
|---|---|---|
| `EndpointDown` | `probe_success == 0` for 5m | critical |
| `CertExpiringSoon` | `probe_ssl_earliest_cert_expiry - time() < 14d` | warning |

Templated from a `monitored_endpoints` variable, matching the Caddyfile
hostnames. The certificate alert is a renewal-is-broken detector rather than
an expiry detector: Caddy renews at 30 days, so reaching 14 means something
has gone wrong.

This bundle catches the failure class that container metrics miss entirely —
the container is up and healthy but the application inside is wedged.

### Meta

| Alert | Condition | Severity |
|---|---|---|
| `TargetDown` | `up == 0` for 10m — how a dead scrape target gets noticed. Ships in increment 1, in `host.yml`, covering `node` and `prometheus`; cAdvisor and blackbox join the same rule once increments 4 and 5 add them as targets | warning |
| `Watchdog` | always firing, by construction | n/a |

`Watchdog` is the dead-man's-switch. It is routed to a webhook receiver that
pings Healthchecks.io, which proves the entire chain is alive: node-exporter
scraping, Prometheus evaluating, Alertmanager routing. If any link breaks the
pings stop and Healthchecks sends an email from outside the server.

This is the only mechanism in the design that can report the failure of the
monitoring itself, or of the machine as a whole — power loss, network loss, or
a wedged kernel produce silence from everything else, and silence is
indistinguishable from health.

## Notification routing

Alertmanager routes to Telegram, grouped by alertname, using the bot token and
chat id from `user_passwords.yml`.

| Route | Group wait | Repeat interval |
|---|---|---|
| `severity: critical` | 30s | 4h |
| `severity: warning` | 30s | 24h |
| `severity: info` | 5m | 7d |
| Diun image updates | 5m | 7d |
| `Watchdog` | — | webhook to Healthchecks, continuous |

Diun's long group wait means ten images updating on the same afternoon arrive
as one message rather than ten.

The bot token is supplied to Alertmanager via `bot_token_file` rather than
inlined, which keeps the credential out of the configuration that gets
rendered into logs on a parse error. The file is mode `0600`, owned by the uid
the container runs as, and mounted read-only.

## Repository changes

### New roles

Following the existing one-role-per-service convention, each a digest-pinned
container on `caddy_network`:

- `alertmanager` — container, configuration template, Telegram receiver,
  routing tree, webhook receiver for the Watchdog
- `cadvisor` — container, Docker socket mounted read-only, publishes `:8080`
- `blackbox_exporter` — container, probe module configuration
- `diun` — container, Docker socket mounted read-only, notifier aimed at
  Alertmanager

### Modified roles

- `prometheus` — stops, disables and removes the pacman `prometheus` package;
  runs a pinned container with its TSDB bind-mounted to
  `/mnt/storage/config/prometheus/data`; keeps `prometheus-node-exporter`
  native and adds its textfile-collector directory plus the
  `--collector.textfile.directory` flag; installs `smartmontools`, the
  `smartmon.sh` script and its timer; owns all alert rule files.
- `backup` — `restic-backup.sh` writes success timestamp and duration to the
  textfile directory; `restic-backup.service` gains
  `OnFailure=backup-alert.service`; that new oneshot unit pushes an alert to
  Alertmanager carrying the exit code and last journal lines.
- `grafana` — datasource provisioned from a file pointing at
  `http://prometheus:9090`, moving it out of Grafana's database and into git.
- `caddy` — unchanged. Nothing new is exposed publicly.

### Where rule files live

All rule files live in `roles/prometheus/`, not distributed into the roles
they monitor. Cohesion would argue for keeping backup rules beside the backup
role, but that means one role writing into another's configuration directory
with a cross-role reload handler — subtle coupling for little benefit.
Reviewing the entire alert set as one group is worth more.

Two rule files are templated rather than static: `ContainerMissing` from
`monitored_containers`, and `EndpointDown` from `monitored_endpoints`. Both
lists live in `roles/prometheus/defaults/main.yml`.

### Secrets

Three keys added to `user_passwords.yml` and its committed example:

```yaml
telegram_bot_token: ""
telegram_chat_id: ""
heartbeat_ping_url: ""
```

Real values are never written into this document or any other committed file.
`user_passwords.yml` is gitignored; the example file documents the required
keys with empty values.

## Testing

### Alert rules get unit tests

`promtool test rules` synthesises a metric series and asserts what fires, so a
rule can be verified without deploying anything or waiting for the `for:`
duration to elapse. Every rule above gets a test.

This catches the two mistakes that actually happen in practice: a mistyped
metric name that silently never matches anything, and a `for:` duration that
makes an alert unreachable.

`BackupMetricMissing` is the rule most worth testing, because its entire
purpose is firing when data is absent — a condition that cannot be verified by
inspecting a working system.

### Configuration validation

`promtool check rules`, `promtool test rules` and `promtool check config`
against `prometheus.yml.j2` all run in `tests/run-promtool.sh`, since that
template contains no Jinja and is valid promtool input as-is. These catch the
class of error where the service starts successfully and then quietly does
nothing.

`amtool check-config` against Alertmanager's configuration does not run in
that harness. `alertmanager.yml.j2` is a Jinja template containing secrets
(`heartbeat_ping_url`, `telegram_chat_id`), so only its rendered form on the
server is a real Alertmanager config to check — there is nothing valid to
check locally. This is what actually happened during deployment: `amtool
check-config` was run against the rendered file on the server, not added to
the local harness.

### Where tests run

A `tests/` directory with a script that runs promtool inside the pinned
Prometheus image, so nothing needs installing locally and the tool version
matches the deployed one exactly.

### Ansible

Every new role must be idempotent: a second run reports zero changes. This
matters particularly for the pacman package removal in the `prometheus` role.
Commit d69771a was dedicated to fixing idempotency bugs, so the check has
demonstrated value here.

### End-to-end verification

Each check is performed as the increment that enables it lands.

Prerequisite, easily missed: the bot must be sent a message by hand before
Alertmanager can ever deliver to it. Telegram bots cannot initiate a
conversation, so until `/start` has been sent to the bot from the target
account, delivery fails with "chat not found" despite a valid token and chat
id. For a private chat the chat id is simply the recipient's Telegram user id.

1. `amtool alert add` a synthetic alert, confirm arrival in Telegram. Proves
   the delivery chain before any rule exists.
2. Stop a container by hand; confirm `ContainerMissing` fires, then start it
   and confirm the alert resolves. Resolution matters — an alert that fires
   but never clears trains the reader to ignore it.
3. Backdate the backup timestamp file; confirm `BackupStale` fires.
4. Run `restic-backup.service` with deliberately broken credentials; confirm
   `BackupFailed` arrives carrying the actual error text.
5. Block the Healthchecks URL; confirm Healthchecks emails when pings stop.

Step 4 is worth performing rather than assuming, since it is the alert this
project exists to deliver.

## Build order

Six increments, each independently useful and independently revertable.

| # | Increment | New services |
|---|---|---|
| 1 | Prometheus containerised, Alertmanager, Telegram receiver, host health rules, `TargetDown`, Watchdog and heartbeat | Prometheus (replaced), Alertmanager — delivered 2026-08-19 |
| 2 | Backup integrity: textfile collector, staleness rules, `OnFailure=` handler | none — delivered 2026-08-20 |
| 3 | Disk health: smartmontools, textfile script and timer, SMART rules | none |
| 4 | Container health: cAdvisor and rules | cAdvisor |
| 5 | Reachability and TLS: blackbox exporter and rules | blackbox |
| 6 | Diun image-update notifications | Diun |

Increment 1 proves the entire Telegram path end to end, so every increment
after it is low-risk.

### Rollback

Each increment is a self-contained role plus a playbook line, so backing one
out is a revert and a re-run. The only irreversible step is removing the
pacman Prometheus in increment 1, and its data has already been declared
expendable.

## Known gaps

- **Alertmanager cannot report its own death.** It is not a Prometheus scrape
  target, so `TargetDown` cannot see it either — the off-box Watchdog
  heartbeat to Healthchecks.io is the only thing that covers this, which is
  why it is in increment 1 rather than deferred.
- **The restic repository is never pruned.** Out of scope here, but it will
  eventually cost real money and needs addressing separately.
- **No log aggregation.** Alerts will say a container is restart-looping but
  not why; diagnosis still means SSH and `docker logs`.
- **No clock-drift alert**, though node-exporter already exports
  `node_timex_sync_status` and a rule could be added cheaply.
- **A missing or typo'd `severity` label routes to the default Telegram
  receiver**, which is `telegram-warning`, so a future rule that gets its
  severity wrong is delivered silently mislabelled as WARNING rather than
  being obviously misrouted or dropped.
- **`/var` under 5% free double-fires.** It satisfies both `DiskSpaceCritical`
  (which does not exclude `/var`) and `VarSpaceCritical`, so one condition
  sends two Telegram messages.
- **`heartbeat_ping_url` and `telegram_chat_id` are rendered without
  `no_log`** in `roles/alertmanager/tasks/main.yml`'s "Template Alertmanager
  configuration" task, so they can appear in `--diff` or `-v` output even
  though the bot token itself is protected.
- **`backup-alert.service` has no `OnFailure=` of its own**, so if the
  handler itself dies nothing notifies. A general fix needs node-exporter's
  `systemd` collector, which is not enabled.
- **A `BackupFailed` push is lost outright if Alertmanager or Telegram is
  unavailable during its 15-minute `endsAt` window**; only `BackupStale`
  backstops it, up to 26 hours later.
- **Nothing checks what a backup actually contained.** A snapshot that is
  legitimately near-empty — an unmounted path, a zero-byte database dump
  from a successful `docker exec` — writes a fresh timestamp and raises no
  alert. `restic backup --json` already emits `files_processed` and
  `total_bytes_processed` at no API cost, so a floor on those would close
  it.
- **On a freshly provisioned host, the timer is enabled but the service is
  not run**, so with `OnCalendar=daily` the first backup is next midnight.
  `BackupMetricMissing` fires an hour after Prometheus starts and
  re-notifies every four hours until then — up to six critical messages on
  a healthy new machine.
- **`restic-backup.sh` writes into a directory created by the `prometheus`
  role and does not create it itself.** Playbook ordering makes this safe
  today, but a role-limited run could invert it, producing a `BackupFailed`
  alert for a backup that actually succeeded.
- **The alert sends up to 800 bytes of backup journal output to Telegram.**
  Fine for a private chat; a future change of receiver to a group or shared
  channel would make it a disclosure decision.

## Phase 2: inbound Telegram bot

Deliberately deferred to its own design document. The original plan had the
bot handling both directions, but choosing Alertmanager as the delivery engine
removed its outbound role entirely. What remains is purely inbound: `/status`,
`/backup_now`, `/restart <service>`, and a file inbox.

Phase 1 creates the bot identity with BotFather and stores the token and chat
id, because Alertmanager's Telegram receiver needs exactly those. Phase 2 is
therefore purely additive — a service that long-polls the same bot — with
nothing to rework.

Two findings from this design carry forward into that one:

- **The containers are not systemd units.** Every role uses
  `community.docker.docker_container` with `restart_policy: always`, so Docker
  supervises them. There is no `grafana.service`; `systemctl restart grafana`
  would simply error. Container restart needs a different mechanism, and that
  mechanism is the main privilege-escalation surface in the whole project.
  `/backup_now` is unaffected — `restic-backup.service` is a real unit.
- **Telegram's Bot API caps file downloads at 20MB.** Adequate for documents,
  receipts and screenshots; not for video or large photo dumps. Raising it
  requires running a local Bot API server, which is not worth the
  infrastructure.
