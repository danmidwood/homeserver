# Inbound Telegram Bot — Design

Phase 2 of the observability work. Phase 1
(`2026-08-19-observability-alerting-design.md`) delivered everything outbound:
Alertmanager pushes alerts to Telegram, and six increments of rules feed it.
This document covers the other direction — asking the server questions and
telling it to do things, from a phone.

Phase 1 already created the bot identity with BotFather and stored its token and
chat id, because Alertmanager's Telegram receiver needs exactly those. This
phase is therefore purely additive: a service that long-polls the same bot.
Nothing in phase 1 is reworked.

## What it does

Four capabilities, all restricted to a single Telegram user id:

| Command | Effect | Privilege needed |
|---|---|---|
| `/status` | Container health, disk, last backup, firing alerts | none |
| `/backup_now` | Starts `restic-backup.service` | one exact sudo rule |
| `/restart <service>` | Restarts one allow-listed app container | one exact sudo rule per container |
| *(send a file)* | Saves it to `/mnt/storage/telegram-inbox` | none |

## Why a host service and not a container

Everything else on this host runs as a digest-pinned container, so a container
would be the consistent choice. It is the wrong one here, for two reasons.

`/restart` needs to talk to the Docker socket. Mounting that socket into a
container is root-equivalent access granted in a form that is harder to reason
about than a sudo rule, and it cannot be narrowed to specific containers — the
socket is all-or-nothing.

More importantly, a containerised bot dies when Docker is unhealthy, which is
exactly the moment `/status` is worth having. A systemd service on the host
survives a wedged Docker daemon and can still report on it.

## Privilege model

This is the part worth being careful about: the bot executes instructions that
arrive from the internet.

**Docker group membership is equivalent to root.** Anyone who can reach the
Docker socket can run `docker run -v /:/host --privileged` and own the machine.
That is not an exploit, it is what the socket is for. The bot user is therefore
**never** in the `docker` group.

The bot runs as a dedicated `telegrambot` system user with no shell. Its
complete privileged capability is a generated sudoers file:

```
telegrambot ALL=(root) NOPASSWD: /usr/bin/systemctl start restic-backup.service
telegrambot ALL=(root) NOPASSWD: /usr/bin/docker restart kavita
telegrambot ALL=(root) NOPASSWD: /usr/bin/docker restart planka
... one exact line per allowed container
```

**No wildcards anywhere.** sudo matches the full command line, so sudo itself is
the allowlist — a bug in the bot cannot widen what it may do, and the total set
of privileged actions is ten lines that fit on one screen. The bot validates the
container name before calling sudo as well, but that is defence in depth, not
the security boundary.

This is safe without a wrapper script because `env_reset` is sudo's compiled-in
default and is not overridden on this host (verified: the only active `env_keep`
is scoped to `visudo`). Environment variables that influence the Docker CLI,
`DOCKER_HOST` chief among them, do not survive into the sudo'd command. If
`env_reset` were ever disabled, this design would need a wrapper that sanitises
its own environment.

**Worst case, with the bot fully compromised:** the attacker can start a backup
and restart nine app containers. Not root, not the proxy, not the databases, not
the monitoring.

### What `/restart` may touch

Allowed — application containers, all recoverable in seconds:

`kavita`, `planka`, `actual_budget`, `immich-server`,
`immich-machine-learning`, `plex`, `portainer`, `vaultwarden`, `ftp_server`

Refused, and the refusal is explained in the reply:

- `caddy` — fronts all seven sites; restarting it takes every one down at once
- `prometheus`, `alertmanager` — restarting these blinds the monitoring, and
  `alertmanager` specifically is what delivers the alert saying something is
  wrong. The bot must not be able to sever its own reporting path.
- `cadvisor`, `blackbox`, `grafana` — monitoring, same argument
- `planka-postgres`, `immich-postgres`, `immich-redis` — data stores; bouncing
  these under load is a different risk class from bouncing an app

Anything on the refused list is done over SSH, deliberately.

## `/status` needs no privilege

Prometheus and Alertmanager already hold everything `/status` reports, so the
bot reads their HTTP APIs on localhost rather than inspecting the host itself.
This is the payoff from the six alerting increments: the bot consumes the
monitoring instead of duplicating it, and the root-equivalent Docker socket
never enters the picture.

| Line | Source |
|---|---|
| containers up / expected | `count(container_last_seen{name!=""})`, `count(container_expected)` |
| disk | `node_filesystem_avail_bytes` / `node_filesystem_size_bytes` for `/`, `/var`, `/mnt/storage` |
| last backup | `(time() - restic_backup_last_success_timestamp_seconds)/3600` |
| firing alerts | Alertmanager `/api/v2/alerts` |

Rendered as, for example:

```
✅ 18/18 containers up
💾 / 18%   /var 39%   /mnt/storage 26%
🕒 backup 21h ago — 33.3 GiB
⚠️ 7 firing: CPUThermalThrottle, ImageUpdateAvailable ×6
```

Messages use Telegram's HTML parse mode, consistent with the Alertmanager
receivers. Everything interpolated is HTML-escaped first: a stray `<`, `>` or
`&` makes Telegram drop the entire message, and alert names and container names
are interpolated.

## Command behaviour

**`/backup_now`** uses `systemctl start --no-block`. Without `--no-block`,
`systemctl start` on a `Type=oneshot` unit blocks until the unit finishes, and
`restic-backup.service` has a six-hour start timeout — the bot would hang for
the length of a backup. It checks `is-active` first and reports that a backup is
already running rather than silently doing nothing.

No completion message is sent. The existing `OnFailure=` handler already raises
`BackupFailed` on failure, and a success updates the metric `/status` reads.

**`/restart <service>`** validates the name against the allowlist, runs the
sudo line, and reports the outcome. There is no confirmation step: the allowed
containers are all recoverable in seconds, and the allowlist already excludes
everything where a mistake would be expensive.

**Unknown commands** get a one-line help reply.

**Messages from any other Telegram user id** are logged and otherwise ignored
entirely. No reply is sent, because replying confirms the bot exists to whoever
is probing.

## File inbox

Files are saved to `/mnt/storage/telegram-inbox`, owned by `telegrambot`.

Telegram's Bot API caps downloads at 20MB. Raising it requires running a local
Bot API server, which is not worth the infrastructure. The cap is checked
against `file_size` in the message **before** downloading, so an oversized file
produces a clear refusal instead of a failed transfer.

Filenames arrive from a remote sender and are treated as hostile:

- basename only — any `/` or `\` component is discarded
- characters outside `[A-Za-z0-9._-]` are replaced
- leading dots stripped, so no hidden files and no `..`
- length capped well below the filesystem limit
- an empty result after sanitising is replaced with a generated name
- collisions are suffixed, never overwritten

Photos carry no filename at all, so they get one built from the date and
Telegram's `file_unique_id`.

### The inbox is backed up

`/mnt/storage/telegram-inbox` is added to `backup_paths`.

The inbox is temporary storage — files live there until they are moved
somewhere permanent — so backing it up looks redundant. It is nearly free, and
it covers the window that would otherwise be uncovered. restic splits files into
content-defined chunks and stores each unique chunk once, so once a file is
moved into an already-backed-up path such as `/mnt/tmdas/Documents`, both
snapshots reference the same chunks. The marginal cost is tree metadata, not a
second copy.

The exception is a file deleted from the inbox without ever being moved into a
backed-up path: its chunks stay in the repository until a `forget` and `prune`
reclaim them. Pruning is run from a separate machine holding a delete-capable B2
key, deliberately kept off this server, so those chunks persist until the next
prune. At 20MB per file that is a slow accumulation rather than a problem.

## Failure modes

**Replayed commands are the dangerous one.** Telegram redelivers updates that
have not been acknowledged, for 24 hours. A bot that restarts without persisting
its `getUpdates` offset re-reads the same messages and executes the commands in
them again — a crash loop could restart containers repeatedly or fire backups.
The offset is therefore persisted to a state file and advanced only after a
command has been handled.

**The bot dying silently.** node-exporter's systemd collector is enabled with a
narrow `unit-include`, and `SystemdUnitFailed` alerts when `telegram-bot.service`
has not been active for fifteen minutes. This works because Alertmanager sends
to Telegram independently of the bot: the broken component is not the one doing
the reporting.

Two alternatives were rejected. A heartbeat written to the node-exporter
textfile collector would need the bot to have write access to a directory that
also holds `restic_backup.prom` and `smart.prom`, letting a compromised bot
forge the backup and disk-health metrics — hiding exactly the failures the
alerting exists to catch. Having the bot serve `/metrics` for Prometheus to
scrape would need a listening socket on all interfaces, since Prometheus is
containerised and reaches the host over the docker bridge rather than loopback,
and it would make liveness depend on the service answering for itself: a wedged
bot still accepting connections would pass a scrape while being useless.

Reading systemd's own view of the unit needs no port, no metrics code in the
bot, and no cooperation from the thing being monitored. The same collector also
covers `restic-backup.service`, `backup-alert.service`,
`image-update-check.service` and `docker.service`, and a second rule,
`SystemdUnitInFailedState`, alerts on any of them entering the failed state.

**Telegram unreachable.** Exponential backoff, not a crash loop. `Restart=always`
on the unit covers a genuine crash.

**No polling conflict.** Only one consumer may call `getUpdates` for a bot.
Alertmanager only ever calls `sendMessage`, so the two never compete, and the
shared token is safe.

## Implementation notes

Written in Go, standard library only. No third-party dependencies means no
`go.sum` supply chain to track and no network access at build time. The Telegram
Bot API is plain HTTP and JSON; four commands do not justify a framework.

The Go toolchain is already installed on the host as a pacman package, so
Ansible builds the binary from source in the repository rather than a binary
being committed. The repository stays the single source of truth.

The Telegram API base URL is injectable, so the entire command path is testable
against a local `httptest` server with no network and no real bot. Unit tests
cover the places where a bug would be silent rather than loud: filename
sanitisation, the authorisation check, the container allowlist, and command
parsing.

## Deliverables

- `roles/telegrambot` — the `telegrambot` system user, the Go source, an Ansible
  build step, the systemd unit, the generated sudoers file, and the inbox
  directory
- `roles/prometheus` — the systemd collector enabled on node-exporter, plus
  `systemd.yml` with `SystemdUnitFailed` and `SystemdUnitInFailedState` and
  their unit tests. The bot itself exposes no network listener.
- `roles/backup` — `/mnt/storage/telegram-inbox` added to `backup_paths`
- nothing new in `user_passwords.yml` — phase 1 already stores
  `telegram_bot_token` and `telegram_chat_id`, and for a private chat the chat
  id *is* the user id, which is exactly what the authorisation check needs

## Deliberately excluded

- **Any command that writes to a service's data.** Reading state and bouncing a
  process are recoverable; mutating application data from a chat message is not.
- **Restarting infrastructure**, per the allowlist above.
- **Multiple users.** One id, checked exactly. Adding a second is a code change,
  not a configuration change, and should stay that way.
- **A local Bot API server** to lift the 20MB cap. Not worth the infrastructure
  for receipts and documents.
