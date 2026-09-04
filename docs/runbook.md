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
| Time Machine | Samba on the host, `/mnt/tmdas/timemachine`, snapshotted daily |
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

### `EndpointDown` for `auth.home.danmidwood.com`
Every application's login goes through this one container, so nothing can be
signed into while it is down. If it followed an image change, see
[Rolling an image back](#rolling-an-image-back) -- a downgrade can leave
Authelia unable to read a database a newer version has already migrated.

```bash
docker logs --tail 20 authelia
```

### `TimeMachineSnapshotStale` / `TimeMachineSnapshotNeverRan`
No read-only snapshot of the Time Machine share in over 36 hours. Time Machine
keeps its own history, so this is not about losing a file — the snapshots are
what survives something on a Mac encrypting the share over SMB, or Time Machine
corrupting its own sparsebundle. Until it runs again, neither is guarded.

```bash
systemctl list-timers timemachine-snapshot.timer
sudo systemctl start timemachine-snapshot.service   # runs it now
journalctl -u timemachine-snapshot.service -n 30
```

### `TimeMachineFragmentationHigh`
The sparsebundle bands are fragmented enough to be worth defragmenting. See
[Defragmenting the Time Machine share](#defragmenting-the-time-machine-share)
below — **do not simply run a defrag**, it will multiply the space the snapshots
use.

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

**Upgrade the system**
```bash
ansible-playbook -i inventory/hosts.ini playbooks/upgrade.yml --check   # preview
ansible-playbook -i inventory/hosts.ini playbooks/upgrade.yml           # do it
```
Deliberately a separate playbook: the main one runs many times a day and a
system upgrade must never be a side effect of deploying a config change.

**Arch has no supported partial upgrade.** Packages are built against whatever
versions of their dependencies are current, with no ABI promise between them, so
upgrading some but not others is the classic way to break the system. It is
`-Syu` or nothing — there is no smaller, safer version of this operation.

The consequence, which bites in a non-obvious way: **while the system is behind,
do not add a new package to any role.** `state: present` only acts when a
package is missing, so existing roles are safe to re-run; but installing
something new fetches it built against libraries this host does not have.
`SystemUpgradeOverdue` fires after 60 days precisely so this gap does not
accumulate silently again.

Arch is a rolling release, so a long gap makes the next upgrade large rather
than impossible — but read <https://archlinux.org/news/> first, because Arch
occasionally requires a manual step that no playbook can infer, and skipping it
can leave the machine unbootable. Have a verified backup, and be able to reach
the machine physically: it is a laptop with no remote console, so a kernel that
will not boot means walking to it.

The playbook records the full package list before upgrading. Combined with
`/var/cache/pacman/pkg`, which still holds the old packages, that allows a
targeted downgrade: `pacman -U /var/cache/pacman/pkg/<name>-<old version>-*.zst`.
It never reboots on its own; it reports whether one is needed.

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

**Explain a load or temperature spike**
```bash
./tools/whatspiked                    # the worst spike in the last 24h
./tools/whatspiked "2026-08-23 22:58" # a specific moment
```
Asks Prometheus which container was burning CPU at that moment, with the same
question an hour earlier as a baseline. Faster and more reliable than reading
journals, because cAdvisor has been recording per-container CPU continuously
whether or not anything logged.

Almost every thermal alert on this host so far has been Immich: it saturates
both cores generating thumbnails and transcoding video after a batch upload,
and the machine is a two-core laptop. That is expected behaviour rather than a
fault — the alert is telling you the temperature is real, not that something is
broken.

**Review doorbell footage**
```bash
./tools/doorbell yesterday          # listing with duration and size
./tools/doorbell yesterday --long   # only clips 8s or longer
./tools/doorbell 2026-08-22 15:30   # play the nearest clip
./tools/doorbell yesterday --all    # fetch and queue the day
./tools/doorbell yesterday --long --all
./tools/doorbell yesterday --min 12 # an explicit threshold
./tools/doorbell yesterday --long --thumbs   # inline stills, iTerm2 only
```
The camera uploads a still a few seconds after each clip, and `--thumbs`
builds a labelled grid of them and renders it inline in iTerm2 (via `imgcat`, which ships inside the app bundle
— no shell integration needed). Stills are matched to clips by the timestamp in
the filename rather than by upload time, because upload times drift apart by
however long the video took to transfer. Around 90% of clips have a matching
still; the rest failed to upload one.

`DOORBELL_THUMB_COLS` and `DOORBELL_THUMB_PX` change the grid shape — six
columns makes the sheet wider and shorter, which suits a busy day.
Clip lengths are bimodal: a large spike at two to four seconds, a trough at six
to eight, and a second population from eight upwards. The short clips are
motion crossing the frame; the longer ones are where something actually
happened. `--long` uses eight seconds for that reason and keeps roughly a
quarter of clips.
Roughly one upload in 450 arrives as a zero-byte file — the camera aborting
mid-transfer, which predates the move to pure-ftpd and continues at the same
rate after it. Those are marked `empty upload` in listings and skipped when
choosing something to play.

**Run the tests**
```bash
./tests/run-promtool.sh        # alert rules and their unit tests
./tests/run-go-tests.sh        # the Telegram bot
./tests/check-image-update.sh  # the image update checker
./tests/check-dashboards.sh    # Grafana dashboard JSON
```

---

## Defragmenting the Time Machine share

Time Machine writes randomly into 8 MB sparsebundle bands. On btrfs every one
of those writes is copy-on-write, so the bands fragment steadily. This is
expected and is why `TimeMachineFragmentationHigh` exists.

**Read this before running a defrag.** `btrfs filesystem defragment` rewrites
extents into contiguous ones, and any extent shared with a snapshot is unshared
in the process. Defragmenting a subvolume that has fourteen daily snapshots can
turn a few hundred megabytes of deltas into fourteen near-complete copies. This
is why the defrag is not scheduled: on a filesystem with about 2 TB free and a
5 TB video library beside it, an automatic one could fill the disk.

The safe order is to delete the snapshots first, defragment, then let the timer
rebuild them. That costs the snapshot history, which is the trade being made
deliberately — pick a moment when the last few days of it are not precious.

```bash
# 1. Confirm it is actually worth doing. The metric is a mean over a sample
#    of bands; a handful of fragmented files is not a reason.
grep timemachine_band /var/lib/node_exporter/textfile_collector/timemachine.prom

# 2. Stop Time Machine writing to it, or the defrag chases a moving target.
sudo systemctl stop smb

# 3. Delete every snapshot. They are subvolumes, so rm will not do it.
for s in /mnt/tmdas/timemachine-snapshots/tm-*; do
  sudo btrfs subvolume delete "$s"
done

# 4. Defragment. -t 32M asks for larger extents than the 8 MB bands, which is
#    what actually reduces the count. This takes hours on the array.
sudo btrfs filesystem defragment -r -t 32M /mnt/tmdas/timemachine

# 5. Bring Samba back and take a fresh snapshot to start the history again.
sudo systemctl start smb
sudo systemctl start timemachine-snapshot.service
```

Check the space situation before step 4 — a defrag needs room to write the new
extents before releasing the old ones.

```bash
df -h /mnt/tmdas
sudo btrfs filesystem usage /mnt/tmdas
```

If free space is tight, do it in parts by pointing the defrag at one Mac's
directory at a time rather than the whole subvolume.

---

## Rolling an image back

Reverting the pin in the role and running the playbook is usually enough. It is
not always enough, and Authelia is the example.

**Authelia 4.39.22 migrates its storage schema on first start, and 4.39.21 then
refuses to run against it:**

```
error during schema migrate: current schema version is greater than the latest
known schema version, you must downgrade to schema version 27 before you can
use this version of Authelia
```

The container crash-loops, every application behind it stops accepting logins,
and `EndpointDown` fires about five minutes later. Nothing about the version
numbers says this will happen -- it looks like any other patch release.

To actually go back, the database has to go back with the image:

```bash
# 1. Stop it crash-looping while you work.
docker rm -f authelia

# 2. Restore the database from the night before the upgrade. The whole
#    directory is in backup_paths, so restic has it.
sudo restic restore latest --target / \
  --include /mnt/storage/config/authelia/data

# 3. Revert the pin in roles/authelia/defaults/main.yml, then deploy.
ansible-playbook -i inventory/hosts.ini playbooks/xps.yml
```

Restoring the database loses any second factor registered since that snapshot,
because the registrations live in it. The secrets directory must NOT be
restored selectively alongside a newer one: storage_encryption_key has to match
the database it encrypted, so restore both or neither.

The general rule: check the release notes for a schema migration before
upgrading anything whose data you would need to bring back. `tools/bump-patch`
prints a reminder for the same reason, and deliberately stops at editing pins
rather than deploying.

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
