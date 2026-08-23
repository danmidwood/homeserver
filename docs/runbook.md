# Runbook

What to do when something breaks, and how to rebuild this host from nothing.

Written to be usable when the server is unavailable and you cannot look
anything up on it. Everything here has been done at least once rather than
assumed.

---

## Where things are

| | |
|---|---|
| Host | `xps.fritz.box` — a Dell XPS 9360 laptop running Arch |
| Managed by | Ansible, from this repository, `ansible-playbook -i inventory/hosts.ini playbooks/xps.yml` |
| Secrets | `user_passwords.yml`, gitignored, **not in this repository** |
| Services | Docker containers, supervised by Docker itself, not systemd units |
| Offsite backup | restic to Backblaze B2, nightly at 00:00 |
| Alerts | Prometheus → Alertmanager → Telegram |
| Dead-man's switch | Healthchecks.io, pinged every 5 minutes by an always-firing rule |

**The laptop's battery is a UPS.** `OnBatteryPower` alerts when mains is lost.
It holds about half its design capacity, so runtime is shorter than the
percentage suggests.

---

## The thing most likely to catch you out

**A backup that says it succeeded may still not be restorable.**

`restic check` without `--read-data` compares the index against object
*listings*. It never downloads anything. Backblaze can report an object as
present, at the right size, with a stored SHA-1, and still serve zero bytes
when asked for it — this has happened here, to three objects, affecting 62
files. Structural checks called the repository healthy throughout.

Only a check that reads real data can detect this. That is what
`restic-restore-check.service` exists for, weekly, and why it matters.

---

## Alerts and what they mean

### `BackupFailed`
The nightly backup exited non-zero. The alert carries the tail of the journal.

```bash
systemctl status restic-backup.service
journalctl -u restic-backup.service -n 50
```

### `BackupStale`
No successful backup in over 26 hours. Usually the timer did not fire at all.

```bash
systemctl list-timers restic-backup.timer
sudo systemctl start restic-backup.service   # runs it now
```

### `RestoreDrillFailed` / `RestoreDrillStale`
The weekly proof that backups can be read back has failed, or has not run.
**This is the alert that means your backups may not work.** See
*Repairing a damaged restic repository* below.

### `ImageWatchFailed`
One or more registries could not be reached, so those images are not being
checked for updates. Often transient; the script retries three times before
giving up. Docker Hub allows 100 requests per hour per IP.

### `SystemdUnitFailed` / `SystemdUnitInFailedState`
A watched unit is not running, or sat in the failed state for 15 minutes.
Watched units are `telegram-bot`, `restic-backup`, `restic-restore-check`,
`backup-alert`, `image-update-check` and `docker`.

### `EndpointDown` with `ContainerMissing`
Both firing means the container died. `EndpointDown` **alone** means the
container is running fine and something in front of it is broken — Caddy lost
its upstream, or the application inside is wedged. That distinction is the
whole reason both rules exist.

### `OnBatteryPower`
Mains power is gone. Note that if the whole house lost power the router is down
too and this alert cannot reach you until the network returns; a total outage
shows up instead as the Healthchecks.io watchdog going quiet.

### `Watchdog`
Always firing, by design. It pings Healthchecks.io every five minutes. If it
stops, Healthchecks emails you — that is the only alarm that works when this
host is completely dead.

---

## Repairing a damaged restic repository

The delete-capable B2 key **deliberately does not live on the server**, so
repairs run from the laptop. The server's key cannot delete, which is what
stops ransomware on the server destroying the backup history.

**1. Find out what is damaged.** Full read of every object, about 30 minutes
for 35 GB:

```bash
restic check --read-data
```

For a cheaper periodic check, read one rotating fifty-second of the repository:

```bash
restic check --read-data-subset=18/52     # slice 18 of 52
```

Do **not** use `--read-data-subset=1%`. That reads a *random* one percent, so a
corrupt object has a one-in-a-hundred chance of being seen each run and may
never be seen at all. The `n/t` form is deterministic: restic buckets packs by
the first byte of the pack id, so rotating `n` weekly covers everything in a
year with no gaps.

**2. Work out which files are affected**, if you need to know:

```bash
sudo /usr/local/bin/map-bad-packs.sh     # on the server, reads /tmp/fullcheck.log
```

It reads index and tree metadata only, never the damaged objects.

**3. Remove the bad object.** `restic repair packs` is the documented tool but
it downloads the damaged pack first, to save a local copy, so it hangs forever
on an object that yields nothing. Delete it directly instead:

```bash
aws s3 rm s3://djm-homeserver-backup/data/<first-two-chars>/<full-pack-id> \
  --endpoint-url https://s3.eu-central-003.backblazeb2.com
```

**4. Rebuild the index without it:**

```bash
restic repair index
```

`restic check` will now report snapshots referencing missing blobs. That is the
hole becoming visible, not new damage.

**5. Refill it** — on the server:

```bash
sudo systemctl start restic-backup.service
```

restic addresses blobs by content hash, so re-backing up the same files
produces the same blob ids and the old snapshots heal themselves. This only
works while the affected files still exist locally.

### Restoring for real

```bash
restic snapshots
restic restore latest --target /tmp/restore --include /path/to/thing
restic restore <snapshot-id> --target /tmp/restore
```

Add `--no-lock` for read-only operations if a lock is stuck. Locks are
occasionally left behind by a killed run; `restic unlock` clears them.

---

## Rebuilding the host from nothing

**Prerequisites you must have off-machine:**

1. This repository.
2. `user_passwords.yml` — **not in the repository.** Without it Ansible refuses
   to run. Keep a copy somewhere safe; losing it means recreating every
   credential.
3. The restic repository password. **Without this the backups are
   unreadable and unrecoverable.** It is not stored anywhere on the server that
   would survive the server.
4. B2 credentials.
5. The WiFi password. The network profiles live only in
   `/etc/NetworkManager/system-connections/` on the host and are **not** in this
   repository, so a rebuilt machine has no network until you connect it by hand.

**Order:**

1. Install Arch. `mkiso.sh` and `install.sh` in this repository cover the base
   install.
2. Connect to WiFi manually — `nmcli device wifi connect <ssid> --ask`.
3. Restore `user_passwords.yml` next to the playbook.
4. Run the playbook: `ansible-playbook -i inventory/hosts.ini playbooks/xps.yml`.
   This brings up every container, timer and rule.
5. Restore data from restic into the paths listed in
   `roles/backup/defaults/main.yml`.
6. Start the services and check `/status` from Telegram.

**What is NOT backed up**, and so cannot be restored:

- `/mnt/storage/ftp/data` — 89 GB of doorbell footage, deliberately excluded
- Anything in Docker named volumes not explicitly listed in `backup_paths`,
  including Grafana's database. Dashboards are provisioned from the repository,
  so they come back; anything created in the Grafana UI does not.

---

## Common operations

**Deploy a change**
```bash
ansible-playbook -i inventory/hosts.ini playbooks/xps.yml
```
A second run must report `changed=0`. If it does not, something is not
idempotent and should be fixed rather than tolerated.

**Reload config without restarting**

Prometheus, Alertmanager and blackbox reload on SIGHUP; Caddy reloads with
`caddy reload`. Ansible handlers do this automatically. Never convert Grafana
to a reload — it has no reload mechanism and reads provisioning only at start.

**Never bind-mount a single config file into a container.** Docker pins the
mount to one inode, and Ansible's atomic writes orphan it, so the container
keeps reading a file the host no longer has. Mount the directory.

**Restart a container**
```bash
docker restart <name>
# or from Telegram, for app containers only:
/restart kavita
```

**Review doorbell footage**
```bash
./tools/doorbell yesterday
./tools/doorbell 2026-08-22 15:30
```

**Run the tests**
```bash
./tests/run-promtool.sh        # alert rules and their unit tests
./tests/run-go-tests.sh        # the Telegram bot
./tests/check-image-update.sh  # the image update checker
./tests/check-dashboards.sh    # Grafana dashboard JSON
```

---

## Gotchas discovered the hard way

- **`systemctl start` on a `Type=oneshot` unit blocks until it finishes.** The
  backup unit has a six-hour timeout. Always use `--no-block` from scripts.
- **`NoNewPrivileges=true` breaks sudo**, so the Telegram bot's unit must not
  set it. The failure looks like a permissions bug.
- **Docker group membership is equivalent to root.** Anyone in it can run
  `docker run -v /:/host --privileged`. The bot user is deliberately not in it.
- **gid 50 is `ftp` in Debian containers and `games` on Arch.** That is why
  `daniel` is in the `games` group: it is how the doorbell's uploads are
  readable, and has nothing to do with games.
- **NetworkManager gives up reconnecting after four attempts by default.** After
  a power cut the server boots faster than the router and used to stay offline
  until reconnected by hand. `/etc/NetworkManager/conf.d/autoconnect-retries.conf`
  sets infinite retries.
- **Alertmanager routes only on severity `critical`, `warning`, `info`,
  `none`.** Any other value silently falls through to the default receiver and
  is delivered mislabelled.
- **Pushed alerts do not resolve themselves.** Anything sent to
  `/api/v2/alerts` fires until its `endsAt` passes, so a script that raises an
  alert must also retract it on success — with `startsAt` earlier than `endsAt`,
  or Alertmanager rejects the retraction silently.
