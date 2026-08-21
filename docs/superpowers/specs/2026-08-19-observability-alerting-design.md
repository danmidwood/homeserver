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

### Configuration delivery

**Never bind-mount a single config file into a container.** Docker pins such a
mount to one inode, while Ansible's `copy:` and `template:` write atomically
via a temporary file and a rename — which unlinks that inode and leaves the
container reading a file the host no longer has (`Links: 0`). The change looks
applied on the host and is invisible inside the container.

Every config directory is therefore bind-mounted as a *directory*, with the
config file inside it, and TSDB or state directories kept out of that directory
so they are not exposed under a second path.

Config changes are then delivered by reload, not restart:

| Component | Mechanism | Why |
|---|---|---|
| Prometheus | `promtool check config`, then `SIGHUP` | A restart punches a gap in every graph and replays the WAL. `--web.enable-lifecycle` is omitted, so SIGHUP is the only reload path |
| Alertmanager | `amtool check-config`, then `SIGHUP` | A restart discards in-flight notification grouping and repeat timers |
| Caddy | `caddy reload` (validates internally) | Fronts all seven services; a restart on a bad config crash-loops every one of them |
| blackbox | `SIGHUP` | Ships no validator; stateless, so a restart would also have been cheap |
| Grafana | restart | Has no reload mechanism — provisioning is read at startup. Already mounts directories, so it was never exposed to the inode trap |

The validation step is not optional. On SIGHUP with a bad config these
components log the error and silently keep running the previous config, so an
unchecked reload would let Ansible report success for a change that never took
effect — the one respect in which reload is weaker than restart, closed by
checking first and failing the play.

### One-shot events need explicit expiry

Alertmanager's model is that an alert is firing until it stops being sent. A
backup failure or a new image tag is a moment, not a state; pushed naively it
would re-notify indefinitely. Both event paths therefore set an explicit
`endsAt` a few minutes in the future so the alert self-resolves.

### What Diun watches, given digest pinning

Every image in this repo is pinned to both a version tag and a digest, as
`caddy:2.11.4@sha256:...`. That freezes the digest by definition, so Diun's
default mode — watch a reference and report when its digest moves — would
report essentially nothing. The useful signal is that a newer tag exists, which
is `watch_repo` mode: Diun lists the repository's tags and reports ones it has
not seen.

The exception is any image whose tag is itself moving. `fauria/vsftpd:latest`
is watched for digest changes rather than by repository, because repository
watching there would report every unrelated tag in the repo while missing the
thing that actually changes.

**No tag filtering initially.** Filtering is deferred until there is a week of
real traffic to write it from. The asymmetry decides it: excess notifications
are visible and easily tuned away, whereas a tag pattern that fails to match a
repository's convention produces no error and no signal, so a missed release
never announces itself. Several of these repositories use conventions a
guessed pattern would get wrong — `linuxserver/kavita` appends `-ls120`,
`plexinc/pms-docker` uses long build strings, and the Immich Postgres image
encodes extension versions in its tag. Those are precisely the ones where a
silent miss would matter, so they are observed rather than predicted.

Diun checks daily rather than hourly. Unfiltered repository watching pages the
full tag list, and some of these repositories publish thousands of tags; a
daily schedule bounds registry API load without affecting what is reported.
Diun's `first_check_notif` stays at its default of false, so the initial run
records the existing tags silently instead of announcing all of them.

Images are discovered through the Docker provider, with per-image settings
carried as container labels rather than a static list. The image reference and
its watch rule then live together in the role that owns them, so a service
added later is watched the moment it ships and the watch list cannot drift from
what is actually running.

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
| `CPUThermalThrottle` | over 85°C for 10m on `chip="platform_coretemp_0"`, `sensor="temp1"` (the package sensor) — the server is a laptop | warning |
| `HostRebooted` | uptime under 5m — a server that reboots by itself is news | info |
| `ClockNotSynchronised` | the kernel reports the clock is not disciplined by NTP, for 30m | warning |
| `TextfileCollectorError` | node-exporter cannot parse a textfile metric file, for 15m | warning |

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
| `SmartOfflineUncorrectable` | sectors unreadable even with error correction, above zero | critical |
| `SmartReallocatedSectors` | reallocated sector count above zero | warning |
| `SmartDiskTemperature` | drive above 55°C for 15m | warning |
| `NvmeCriticalWarning` | NVMe `critical_warning` bitfield non-zero | critical |
| `NvmeMediaErrors` | NVMe media and data integrity errors above zero | critical |
| `NvmeWearHigh` | NVMe `percentage_used` above 80% | warning |
| `SmartCollectorFailed` | the collector script itself failed | warning |
| `SmartMetricsMissing` | no SMART metrics are exported at all | warning |
| `SmartMetricsStale` | the collector's last-run timestamp is over an hour old | warning |
| `SmartSeriesMissing` | an attribute family the collector should export is absent | warning |
| `MdArrayFailed` | `md0` has zero active disks | critical |

The four collector-health rules are not redundant with one another.
`SmartCollectorFailed` watches a value — the gauge the collector itself
writes on every run — so it only speaks once the collector has run and
recorded a failure. `SmartMetricsMissing` watches for the series being
absent entirely, catching the collector never having run in the first
place. `SmartMetricsStale` catches the case that defeats both: a collector
that silently stopped running, whose last written file keeps being served
forever still saying success — no failed value to trip `SmartCollectorFailed`,
no absent series to trip `SmartMetricsMissing`, only a last-run timestamp
quietly falling behind that nothing else would notice.

The first three all watch the collector's own success gauge — its value,
its absence, and its age — and none of them can see the fourth failure
mode: a device that answers its own health check but yields no attribute
table. `SmartSeriesMissing` watches whether the metrics the collector is
supposed to produce actually exist, not what the collector believes about
itself. Without it, that device leaves the collector reporting success
with a fresh timestamp while the reallocated, pending and uncorrectable
series are silently gone — so the three rules that best predict the array
disk's failure watch nothing.

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

cAdvisor runs as a digest-pinned container with `/var/run/docker.sock`
bind-mounted `:ro`, scraped by Prometheus over `caddy_network` at
`cadvisor:8080`. The container also publishes 8080 on the host, but nothing
in this repo scrapes that; it is unused. The `:ro` on the socket mount is
worth no confidence: it constrains the bind mount, not the socket
underneath, and cAdvisor runs as uid 0, so it has full read/write access to
the Docker API regardless — equivalent to host root. It is the only sensor
in this design with no native package worth using.

The Docker socket alone is not enough for cAdvisor to see any containers.
Two more things are required, both discovered by redeploying and checking
the series count rather than by reasoning about what "should" be needed —
reasoning got both of these wrong the first time:

- **`/run/containerd/containerd.sock`.** cAdvisor's Docker container
  factory registers itself through containerd (`/var/run` is a symlink to
  `/run` on Arch). Without it, cAdvisor starts, serves `/metrics`, and logs
  a factory-registration failure once at startup, then silently reports
  nothing but the machine cgroup (`container_last_seen{id="/"}`) forever
  after.
- **`/var/lib/docker`, read-only.** cAdvisor's Docker factory reads
  `/var/lib/docker/image/overlay2/layerdb/mounts/<id>/mount-id` to resolve
  each container's read-write overlay2 layer. Without it, every running
  container logs "failed to identify the read-write layer ID" and is
  dropped — again leaving only the machine cgroup reported. This one was
  believed unnecessary (the working theory was that it fed only
  filesystem-partition discovery, which no rule reads) and removed in an
  earlier pass of this same increment; it broke container discovery
  outright and had to be restored.

Both failures look identical at a glance — a healthy-looking endpoint
serving one series instead of eighteen — and neither is visible from
cAdvisor's exit status or HTTP status code. This is why any change to this
mount list has to be verified by series count against a live redeploy,
never by reasoning alone:

```
curl -s http://cadvisor:8080/metrics | grep -c '^container_last_seen'   # expect 18
```

| Alert | Condition | Severity |
|---|---|---|
| `ContainerMissing` | a container on the expected list is no longer reported, for 5m | critical |
| `ContainerRestartLoop` | more than 3 starts in an hour | warning |
| `ContainerOOMKilled` | an OOM kill event | warning |
| `ContainerMetricsMissing` | `absent(container_last_seen{name!=""})` — cAdvisor is reporting no containers, whether because it lost the Docker socket or because it is gone entirely (a test asserts the latter case, and it has been observed live) | warning |
| `ContainerExpectedMissing` | the expected-container list itself is not being exported | warning |

cAdvisor is granted what its Docker/containerd factory needs to enumerate
containers and report the four metrics the rules read: `/sys` for cgroup
data, the Docker and containerd sockets, `/var/lib/docker` for overlay2
layer resolution (above), and `/dev/kmsg` plus `CAP_SYSLOG` for OOM events
(below). It does not get `/:/rootfs` or `/dev/disk`: neither is read by
cAdvisor's container factories, both fed only filesystem-partition
discovery, which no rule reads and which fails noisily on this host's
btrfs layout regardless, and `/rootfs` specifically handed a read-only view
of the whole host filesystem — every service's data directory included —
to a container that already has root-equivalent Docker API access.
`/var/run` is narrowed to the two sockets cAdvisor actually uses rather
than mounting the whole directory, which would otherwise expose every
other daemon's socket on the host too.

**The expected list is data, not rule text.** `ContainerMissing` has to know
what ought to be running, and the obvious approach — templating the rule file
from a `monitored_containers` variable — was rejected. A templated rule file
lives under `templates/`, where the test harness never sees it, because that
harness globs `roles/prometheus/files/rules/*.yml`. It is also not valid YAML
until rendered, so `promtool check rules` cannot validate it, and every Go
template expression in its annotations would need `{% raw %}` to survive
Jinja. The result would be one untested, unvalidatable rule sitting among
tested ones.

Instead Ansible writes the expected set into node-exporter's textfile
directory as a metric:

```
container_expected{name="vaultwarden"} 1
```

The rule then stays static, testable and free of Jinja:

```
container_expected == 1 unless on(name) container_last_seen
```

Adding a service to the playbook adds a line to the list, which adds a
metric, which extends coverage automatically — the property the templated
approach was reaching for, without its costs. The expected-list file is
itself covered by `TextfileCollectorError`, and `ContainerExpectedMissing`
catches the case where the list stops being exported entirely, which would
otherwise leave `ContainerMissing` matching nothing and silently unarmed.

**Rules are written after cAdvisor is deployed, not before.** Elsewhere in
this design the rules come first, because the collector emitting their
metrics is written here too. cAdvisor is third-party: it decides its own
metric names, and whether `container_oom_events_total` exists at all depends
on the kernel and the cAdvisor version. Writing rules against assumed names
risks a rule that matches nothing while its unit tests pass, so the first
task deploys cAdvisor and inventories what it genuinely exports. That
inventory found `container_oom_events_total` present but registered
regardless of whether its source actually works — mounting `/dev/kmsg` gets
the series to exist, not to increment. This host also sets
`kernel.dmesg_restrict=1`, which requires `CAP_SYSLOG` to open `/dev/kmsg`
at all; without it cAdvisor logs "disabling OOM events" at startup and the
series stays at zero forever, series count alone cannot distinguish that
from a working collector. The container is granted `capabilities: [SYSLOG]`
for this reason — a narrower grant than `privileged: true`, which was
rejected both here and for the mount reductions above.

### Reachability and TLS — blackbox exporter

The blackbox exporter runs as a digest-pinned container on `caddy_network`.
Prometheus scrapes it once per endpoint, passing the target as a parameter, so
the endpoint list lives in the scrape config rather than in a rule. The rules
themselves stay static and testable — `probe_success == 0` needs no knowledge
of which endpoints exist.

| Alert | Condition | Severity |
|---|---|---|
| `EndpointDown` | `probe_success == 0` for 5m | critical |
| `CertExpiringSoon` | under 14 days of certificate validity left | warning |
| `BlackboxMetricsMissing` | no probe results are being exported at all | warning |

The certificate alert is a renewal-is-broken detector rather than an expiry
detector: Caddy renews at 30 days, so reaching 14 means something has gone
wrong. `BlackboxMetricsMissing` exists for the same reason as its siblings
elsewhere in this design — a comparison cannot fire on an absent series, so
without it a prober that stopped running would leave `EndpointDown` silently
unarmed.

**What the probe actually tests, and what it does not.** The hostnames resolve
to a public address, and the server can reach its own public IP because the
router supports NAT hairpinning — verified. So a probe originating on the
server still traverses DNS, the router's port forwarding, Caddy, TLS
termination and the upstream container. That is the full path a visitor takes,
with one exception: NAT loopback works whether or not the internet connection
is up, so this **cannot distinguish "the ISP is down" from "everything is
fine"**. Only an off-box prober could, and the Healthchecks heartbeat covers
the machine being alive rather than the services being reachable.

**Plex needs a different path from the others.** `https://plex.home.danmidwood.com/`
returns 401 — its root requires authentication — so a default probe would
report it permanently down and `EndpointDown` would fire forever, which is the
fastest way to teach a reader to ignore Telegram. Plex exposes `/identity`
unauthenticated, returning 200 with a `machineIdentifier`, and that is what is
probed. Grafana redirects `/` to `/login`; the probe module follows redirects,
so it resolves to 200 without special handling.

**The `instance` label deviates here, deliberately.** Everywhere else in this
design `instance` is `xps`, because everything else describes one machine.
Blackbox's convention makes `instance` the probed URL, which is both necessary
— each target needs its own identity — and better for reading an alert, since
the message names the endpoint that is down rather than the host doing the
probing.

This bundle catches the failure class that container metrics miss entirely:
the container is running and healthy while the application inside it is
wedged, or Caddy has lost its upstream, or a certificate has quietly stopped
renewing.

**Verified by fault injection.** Pointing Caddy at a dead upstream port for
`kavita` while the container kept running fired `EndpointDown` for
`https://books.home.danmidwood.com` within its 5-minute window, and
`ContainerMissing` stayed silent throughout — cAdvisor still saw a healthy,
running container, because from the container's point of view nothing was
wrong. Whether `ContainerMissing` accompanies `EndpointDown` is therefore
itself a diagnostic signal: both firing together means the container died;
`EndpointDown` alone means the container is fine and something in front of
or inside it is broken.

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
  Alertmanager, watching by repository with no tag filter (see "What Diun
  watches, given digest pinning")

### Modified roles

- `prometheus` — stops, disables and removes the pacman `prometheus` package;
  runs a pinned container with its TSDB bind-mounted to
  `/mnt/storage/config/prometheus/data`; keeps `prometheus-node-exporter`
  native and adds its textfile-collector directory plus the
  `--collector.textfile.directory` flag; installs `smartmontools`, and
  templates `roles/prometheus/templates/smart-metrics.sh.j2` to
  `/usr/local/bin/smart-metrics.sh` along with its timer; owns all alert rule
  files.
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
| 3 | Disk health: smartmontools, textfile script and timer, SMART rules | none — delivered 2026-08-20 |
| 4 | Container health: cAdvisor and rules | cAdvisor — delivered 2026-08-20 |
| 5 | Reachability and TLS: blackbox exporter and rules | blackbox — delivered 2026-08-21 |
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
- **A missing or typo'd `severity` label routes to the default Telegram
  receiver**, which is `telegram-warning`, so a future rule that gets its
  severity wrong is delivered silently mislabelled as WARNING rather than
  being obviously misrouted or dropped.
- **`/var` under 5% free double-fires.** It satisfies both `DiskSpaceCritical`
  (which does not exclude `/var`) and `VarSpaceCritical`, so one condition
  sends two Telegram messages.
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
- **The device list is pinned to specific disks by id**, so a replacement
  drive is not monitored until the list is updated — deliberately failing
  loudly rather than silently matching whatever occupies a kernel name.
- **One 55°C temperature threshold covers both a spinning disk and an
  NVMe**, and NVMe composite temperatures routinely run hotter under load,
  so this may need splitting by metric family before it becomes a chronic
  warning.
- **Nothing detects a container that is running but absent from
  `monitored_containers`.** The join is one-way: `ContainerMissing` only
  ever compares the expected list against what cAdvisor reports, never the
  other direction. A service added to the playbook and forgotten in the
  list is unmonitored forever, with every indicator green. The reverse —
  a container removed from the list but still running — is self-announcing
  and needs no rule.
- **On a fresh-host rebuild the expected-container list would exist before
  most of the containers do.** The `prometheus` role that writes
  `container_expected` runs at position 14 of 23 in the playbook, while
  twelve of the seventeen containers are created by roles that run after
  it. `ContainerMissing` would fire critical for up to twelve containers
  before they exist. Moving that task to `post_tasks` would fix it.
- **A slow crash-loop is invisible.** `ContainerRestartLoop` needs four
  restarts within an hour; a container dying every twenty minutes never
  reaches that rate, and each individual absence is too brief to reach
  `ContainerMissing`'s 5-minute `for:` either.
- **Docker `HEALTHCHECK` state is invisible.** A container that is
  running, reported by cAdvisor, and permanently unhealthy looks perfect
  to all five container rules — none of them read health-check status.
- **Every join in `container.yml` is `on(name)` with no `instance`.**
  Adding a second host to this Prometheus would let one host's container
  satisfy another host's expected entry, silently defeating
  `ContainerMissing` for both.

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
