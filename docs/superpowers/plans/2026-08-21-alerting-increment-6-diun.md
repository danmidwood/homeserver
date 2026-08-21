# Diun Image-Update Notifications Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Have the server tell you, through the same Telegram path as every other alert, when a newer tag exists for any container image it runs.

**Architecture:** A digest-pinned Diun container watches the Docker socket read-only and discovers images from the containers actually running, controlled by per-container labels. On finding a new tag it runs a shell script inside its own container that POSTs a one-shot alert to Alertmanager, which groups and delivers it to Telegram. No new Prometheus rules and no new Alertmanager route are needed: the alert carries `severity: info`, whose existing route already has exactly the group wait and repeat interval this needs.

**Tech Stack:** Diun v4.33.0 (Alpine/busybox), Alertmanager `/api/v2/alerts`, Ansible, Docker.

**Spec:** `docs/superpowers/specs/2026-08-19-observability-alerting-design.md` — read the sections "What Diun watches, given digest pinning", "Risk: Diun's payload shape", "One-shot events need explicit expiry", and "Configuration delivery".

## Global Constraints

- Every container image is pinned by digest, not just tag.
- Alertmanager routes ONLY on severity values `critical`, `warning`, `info`, `none`. Any other value silently falls through to the default receiver (`telegram-warning`) and is delivered mislabelled. Every alert MUST carry one of those four.
- Never bind-mount a single config file into a container. Mount the containing directory. A single-file bind mount is pinned to one inode that Ansible's atomic rename orphans, leaving the container reading a file the host no longer has.
- No code comment anywhere may mention `/mnt/seagate`.
- Secrets live only in gitignored files. No real credential may appear in any committed file, including specs, plans and tests.
- Telegram receivers use `parse_mode: HTML`, which drops the entire message if the text contains a stray `<`, `>` or `&`. Anything interpolated into an annotation must be HTML-escaped.
- Ansible role tasks must be idempotent: a second run reports `changed=0`.

## Facts Already Verified — Do Not Re-Derive

These were confirmed on the host before this plan was written. Use them verbatim.

| Fact | Value |
|---|---|
| Latest Diun release | `v4.33.0`, published 2026-05-30 |
| Pinned image reference | `crazymax/diun:4.33.0@sha256:e324b793eb32dfb7f74d3a39421ebf090141caaadee69b8f78da63112408ee25` |
| Image entrypoint / cmd | `diun` / `serve` |
| Runs as | `uid=0(root)` — no PUID/PGID handling needed, and the Docker socket is readable without extra groups |
| Baked-in env | `DIUN_DB_PATH=/data/diun.db` |
| Tooling inside image | `sh` ✅, `wget` ✅ (supports `--post-data`), `nc` ✅ — `bash` ❌, `curl` ❌, `jq` ❌ |
| Busybox `date` relative arithmetic | **Rejected.** `date -d "+30 minutes"` → `date: invalid date '+30 minutes'` |
| Busybox `date` epoch form | **Works.** `date -u -d "@1787317226" +%Y-%m-%dT%H:%M:%SZ` → `2026-08-21T13:00:26Z` |
| `fauria/vsftpd` tags | Only `latest`, last pushed 2023-01-27 — there is no version tag to watch |

Diun script-notifier environment variables (exact names): `DIUN_VERSION`, `DIUN_HOSTNAME`, `DIUN_ENTRY_STATUS`, `DIUN_ENTRY_PROVIDER`, `DIUN_ENTRY_IMAGE`, `DIUN_ENTRY_HUBLINK`, `DIUN_ENTRY_MIMETYPE`, `DIUN_ENTRY_DIGEST`, `DIUN_ENTRY_CREATED`, `DIUN_ENTRY_PLATFORM`, `DIUN_ENTRY_METADATA_CTN_*`.

Diun Docker labels (exact names): `diun.enable`, `diun.watch_repo`, `diun.notify_on`, `diun.sort_tags`, `diun.max_tags`, `diun.include_tags`, `diun.exclude_tags`, `diun.hub_link`, `diun.platform`, `diun.regopt`, `diun.metadata.*`.

## File Structure

| File | Responsibility |
|---|---|
| `roles/diun/tasks/main.yml` | Directories, config, script, pinned container |
| `roles/diun/handlers/main.yml` | Restart Diun on config or script change |
| `roles/diun/files/diun.yml` | Diun's own configuration: schedule, provider, notifier |
| `roles/diun/files/diun-notify.sh` | Builds and POSTs the Alertmanager payload, in POSIX sh |
| `tests/check-diun-notify.sh` | Runs the script against a captured endpoint and validates the JSON it emits |
| `playbooks/xps.yml` | One added line: the `diun` role |
| 14 role task files | One `labels:` block per `docker_container` task |

---

### Task 1: The Diun role and its Alertmanager delivery script

**Files:**
- Create: `roles/diun/tasks/main.yml`
- Create: `roles/diun/handlers/main.yml`
- Create: `roles/diun/files/diun.yml`
- Create: `roles/diun/files/diun-notify.sh`
- Create: `tests/check-diun-notify.sh`
- Modify: `playbooks/xps.yml` (insert exactly one line)

**Interfaces:**
- Consumes: the running `alertmanager` container on `caddy_network`, reachable as `http://alertmanager:9093`.
- Produces: alerts with `alertname: ImageUpdateAvailable`, `severity: info`, `job: diun`, `instance: xps`, and an `image` label. Task 2 relies on nothing from this task except that the container exists and reads labels.

- [ ] **Step 1: Write the failing test**

Create `tests/check-diun-notify.sh`. It runs the real script inside the real Diun image with fake `DIUN_ENTRY_*` values, captures what it POSTs using busybox `nc`, and validates the body is well-formed JSON carrying the right labels. It requires Docker on the machine running it (Docker Desktop, same as `tests/run-promtool.sh`).

```bash
#!/usr/bin/env bash
# Runs roles/diun/files/diun-notify.sh inside the pinned Diun image, captures
# the HTTP body it posts, and checks it is valid JSON with the labels
# Alertmanager routes on. This is the only test that can catch a quoting bug
# in the hand-built JSON before it reaches a live Alertmanager.
set -euo pipefail

export PATH="/Applications/Docker.app/Contents/Resources/bin:$PATH"

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
IMAGE="crazymax/diun:4.33.0@sha256:e324b793eb32dfb7f74d3a39421ebf090141caaadee69b8f78da63112408ee25"
SCRIPT="$REPO_ROOT/roles/diun/files/diun-notify.sh"

echo "==> Capturing the payload diun-notify.sh posts"

# The image name deliberately contains a '&' and a '<' so the test fails if
# HTML escaping is dropped, and a '"' so it fails if JSON escaping is dropped.
CAPTURED=$(docker run --rm \
  -v "$SCRIPT:/notify.sh:ro" \
  -e 'DIUN_ENTRY_IMAGE=ghcr.io/example/img:1.2.3&<"test' \
  -e 'DIUN_ENTRY_STATUS=new' \
  -e 'DIUN_ENTRY_HUBLINK=https://example.invalid/img' \
  -e 'DIUN_ENTRY_DIGEST=sha256:0000000000000000000000000000000000000000000000000000000000000000' \
  -e 'DIUN_ENTRY_PLATFORM=linux/amd64' \
  -e 'DIUN_ENTRY_CREATED=2026-08-21T00:00:00Z' \
  -e 'ALERTMANAGER_URL=http://127.0.0.1:9999/api/v2/alerts' \
  --entrypoint sh "$IMAGE" -c '
    nc -l -p 9999 > /tmp/capture 2>/dev/null &
    NCPID=$!
    sleep 1
    sh /notify.sh || true
    sleep 1
    kill $NCPID 2>/dev/null || true
    # Strip HTTP request headers, leaving the body.
    sed -n "/^\r\?$/,\$p" /tmp/capture | tail -n +2
  ')

if [ -z "$CAPTURED" ]; then
  echo "  FAIL: the script posted nothing"
  exit 1
fi

echo "$CAPTURED" | python3 -c '
import json, sys
body = sys.stdin.read().strip()
try:
    alerts = json.loads(body)
except json.JSONDecodeError as e:
    print("  FAIL: payload is not valid JSON: %s" % e)
    print("  body was: %r" % body[:400])
    sys.exit(1)

assert isinstance(alerts, list) and len(alerts) == 1, "expected a one-element array"
a = alerts[0]
labels = a["labels"]
assert labels["alertname"] == "ImageUpdateAvailable", labels
assert labels["severity"] == "info", "severity must be one of critical/warning/info/none"
assert labels["job"] == "diun", labels
assert labels["instance"] == "xps", labels
assert "image" in labels and labels["image"], "the image label must be populated"
assert a["startsAt"] < a["endsAt"], "endsAt must be after startsAt"

# The alert must outlive the info route group_wait of 5m, or it can resolve
# before it is ever notified.
from datetime import datetime
fmt = "%Y-%m-%dT%H:%M:%SZ"
delta = datetime.strptime(a["endsAt"], fmt) - datetime.strptime(a["startsAt"], fmt)
assert delta.total_seconds() >= 600, "endsAt must be at least 10m out, got %s" % delta

# HTML escaping must have happened: no raw < or & may survive into text that
# Telegram parses as HTML.
text = a["annotations"]["summary"] + a["annotations"]["description"]
assert "&amp;" in text or "&lt;" in text, "HTML escaping was not applied"
assert "&<" not in text, "raw < survived escaping"

print("  OK: valid JSON, %d alert, labels and escaping correct" % len(alerts))
'
echo "==> diun-notify.sh checks passed"
```

Make it executable:

```bash
chmod +x tests/check-diun-notify.sh
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `./tests/check-diun-notify.sh`
Expected: FAIL — the script does not exist yet, so `docker run` errors on the missing bind-mount source, or the capture is empty and it reports `FAIL: the script posted nothing`.

- [ ] **Step 3: Write the notification script**

Create `roles/diun/files/diun-notify.sh`:

```sh
#!/bin/sh
# Invoked by Diun's script notifier, once per image entry, inside Diun's own
# container. Pushes a one-shot alert into Alertmanager so image updates arrive
# through the same Telegram path as every other alert.
#
# The container is Alpine busybox: there is no jq, no curl and no bash here.
# The JSON is therefore assembled by hand and posted with busybox wget, and
# every interpolated value has to be escaped explicitly.
#
# No `set -e`: a failure to read one optional field must not stop the alert
# being sent. `set -u` stays on, and every variable below has a default.
set -u

ALERTMANAGER_URL="${ALERTMANAGER_URL:-http://alertmanager:9093/api/v2/alerts}"

# Escape a value for use inside a JSON string literal.
json_escape() {
  printf '%s' "$1" | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g'
}

# Escape for Telegram's HTML parse_mode, which silently drops the whole
# message if the text contains a stray '<', '>' or '&'. Ampersand must be
# replaced first, or the entities introduced below get re-escaped.
html_escape() {
  printf '%s' "$1" | sed -e 's/&/\&amp;/g' -e 's/</\&lt;/g' -e 's/>/\&gt;/g'
}

# Order matters: HTML-escape for the human, then JSON-escape so the result
# survives being embedded in the payload.
IMAGE=$(json_escape "$(html_escape "${DIUN_ENTRY_IMAGE:-unknown}")")
STATUS=$(json_escape "$(html_escape "${DIUN_ENTRY_STATUS:-unknown}")")
HUBLINK=$(json_escape "$(html_escape "${DIUN_ENTRY_HUBLINK:-none}")")
DIGEST=$(json_escape "$(html_escape "${DIUN_ENTRY_DIGEST:-unknown}")")
PLATFORM=$(json_escape "$(html_escape "${DIUN_ENTRY_PLATFORM:-unknown}")")
CREATED=$(json_escape "$(html_escape "${DIUN_ENTRY_CREATED:-unknown}")")

STARTS_AT=$(date -u +%Y-%m-%dT%H:%M:%SZ)

# Alertmanager treats an alert as firing until it stops being sent. A new
# image tag is a moment, not a state, so an explicit endsAt makes it
# self-resolve instead of re-notifying forever.
#
# 30 minutes, not 5: the info route holds alerts for a 5m group_wait before
# notifying, and an alert whose endsAt fell inside that window could resolve
# before it was ever delivered.
#
# Busybox date has no GNU relative arithmetic -- `date -d "+30 minutes"` is
# rejected outright -- so the epoch is computed and reformatted instead.
ENDS_AT=$(date -u -d "@$(( $(date -u +%s) + 1800 ))" +%Y-%m-%dT%H:%M:%SZ)

PAYLOAD=$(cat <<JSON
[{
  "labels": {
    "alertname": "ImageUpdateAvailable",
    "severity": "info",
    "instance": "xps",
    "job": "diun",
    "image": "${IMAGE}"
  },
  "annotations": {
    "summary": "New image available: ${IMAGE}",
    "description": "Diun reports status '${STATUS}' for ${IMAGE} on ${PLATFORM}, built ${CREATED}. Digest ${DIGEST}. Registry page: ${HUBLINK} . Bump the pinned digest in the Ansible role that owns this image when you are ready."
  },
  "startsAt": "${STARTS_AT}",
  "endsAt": "${ENDS_AT}"
}]
JSON
)

wget --quiet --output-document=/dev/null \
     --header="Content-Type: application/json" \
     --post-data="${PAYLOAD}" \
     "${ALERTMANAGER_URL}"
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `./tests/check-diun-notify.sh`
Expected: PASS — `OK: valid JSON, 1 alert, labels and escaping correct`.

- [ ] **Step 5: Mutation-verify the test**

The project requires proof that a test can fail. Temporarily change `"severity": "info"` to `"severity": "urgent"` in `roles/diun/files/diun-notify.sh`, run `./tests/check-diun-notify.sh`, and confirm it FAILS on the severity assertion. Then change it back and confirm it passes again. Record both outcomes in your report.

- [ ] **Step 6: Write Diun's configuration**

Create `roles/diun/files/diun.yml`:

```yaml
# Diun watches the images this host actually runs and reports newer tags.
#
# Every image here is pinned to a tag AND a digest, which freezes the digest by
# definition -- so Diun's default mode of watching one reference for a digest
# change would report essentially nothing. watch_repo (set per container by
# label) is what makes this useful: it lists the repository's tags and reports
# ones it has not seen.
db:
  path: /data/diun.db

watch:
  workers: 10
  # Daily, not hourly. Unfiltered repository watching pages the full tag list,
  # and some of these repositories publish thousands of tags; daily bounds the
  # registry API load without affecting what gets reported.
  schedule: "0 6 * * *"
  # Default, stated explicitly: on the very first run Diun records the existing
  # tag list silently instead of announcing every tag of every image at once.
  firstCheckNotif: false

notif:
  script:
    cmd: "/bin/sh"
    args:
      - "/etc/diun/notify.sh"

providers:
  docker:
    # Nothing is watched unless it carries diun.enable=true, so adding a
    # container to this host is a deliberate decision to watch it, not an
    # accident of it existing.
    watchByDefault: false
    watchStopped: false
```

- [ ] **Step 7: Write the role**

Create `roles/diun/handlers/main.yml`:

```yaml
- name: Restart Diun
  # Restart rather than reload: Diun has no reload mechanism, and it is a
  # background poller with no connections to preserve and no scrape gap to
  # cause. Its state lives in the bolt database on a separate mount, so a
  # restart does not lose the record of which tags it has already seen.
  community.docker.docker_container:
    name: diun
    state: started
    restart: true
```

Create `roles/diun/tasks/main.yml`:

```yaml
- name: Create the Diun state directory
  # Holds diun.db, the record of which tags have already been reported. Kept
  # out of the config directory so it is not exposed under a second path, and
  # so that losing config never means re-announcing every tag.
  ansible.builtin.file:
    path: /mnt/storage/config/diun/data
    state: directory
    owner: root
    group: root
    mode: '0755'

- name: Create the bind-mounted Diun conf directory
  # The DIRECTORY is bind-mounted, not the files inside it. A single-file bind
  # mount pins Docker to one inode that Ansible's atomic rename orphans,
  # leaving the container reading a file the host no longer has.
  ansible.builtin.file:
    path: /mnt/storage/config/diun/conf
    state: directory
    owner: root
    group: root
    mode: '0755'

- name: Write the Diun configuration
  ansible.builtin.copy:
    src: diun.yml
    dest: /mnt/storage/config/diun/conf/diun.yml
    owner: root
    group: root
    mode: '0644'
  notify: Restart Diun

- name: Install the Alertmanager notification script
  ansible.builtin.copy:
    src: diun-notify.sh
    dest: /mnt/storage/config/diun/conf/notify.sh
    owner: root
    group: root
    mode: '0755'
  notify: Restart Diun

- name: Pull the Diun image
  community.docker.docker_image_pull:
    name: "crazymax/diun:4.33.0@sha256:e324b793eb32dfb7f74d3a39421ebf090141caaadee69b8f78da63112408ee25"
    pull: not_present

- name: Run Diun container
  community.docker.docker_container:
    name: diun
    image: "crazymax/diun:4.33.0@sha256:e324b793eb32dfb7f74d3a39421ebf090141caaadee69b8f78da63112408ee25"
    state: started
    restart_policy: always
    networks:
      # Needed to reach alertmanager:9093 by name.
      - name: caddy_network
    volumes:
      - /mnt/storage/config/diun/conf:/etc/diun:ro
      - /mnt/storage/config/diun/data:/data
      # Read-only: Diun only enumerates containers and reads their labels. It
      # never needs to create, stop or modify anything.
      - /var/run/docker.sock:/var/run/docker.sock:ro
    env:
      TZ: "Europe/London"
    command:
      - "serve"
      - "--config"
      - "/etc/diun/diun.yml"
```

- [ ] **Step 8: Add the role to the playbook**

Open `playbooks/xps.yml`. Count the roles under `roles:` and write the number down. Insert **exactly one line**, `    - diun`, immediately after the `    - blackbox` line. Count again and confirm the total went up by exactly one and that no other role name changed or disappeared.

- [ ] **Step 9: Deploy and verify Diun starts and reaches Alertmanager**

```bash
ansible-playbook -i inventory/hosts.ini playbooks/xps.yml
```

Then confirm the container is healthy and its config parsed:

```bash
ssh daniel@xps.fritz.box 'docker ps --filter name=^diun$ --format "{{.Status}}"'
ssh daniel@xps.fritz.box 'docker logs diun 2>&1 | tail -20'
```

Expected: status `Up`, and the log shows Diun starting with the docker provider and **no** config parse errors. Because no container carries `diun.enable=true` yet, it will report that it found 0 images to analyse. That is correct at this stage — Task 2 supplies the labels.

Prove the delivery path works now, without waiting for a real image update, by running the script by hand inside the container with fake values:

```bash
ssh daniel@xps.fritz.box 'docker exec \
  -e DIUN_ENTRY_IMAGE=example/manual-test:1.0.0 \
  -e DIUN_ENTRY_STATUS=new \
  -e DIUN_ENTRY_HUBLINK=https://example.invalid/manual \
  -e DIUN_ENTRY_DIGEST=sha256:deadbeef \
  -e DIUN_ENTRY_PLATFORM=linux/amd64 \
  -e DIUN_ENTRY_CREATED=2026-08-21T00:00:00Z \
  diun sh /etc/diun/notify.sh && echo POSTED'
```

Confirm Alertmanager accepted it:

```bash
ssh daniel@xps.fritz.box 'curl -s http://localhost:9093/api/v2/alerts | \
  jq -r ".[] | select(.labels.alertname==\"ImageUpdateAvailable\") | \
  \"\(.labels.image)  status=\(.status.state)\""'
```

Expected: the alert appears with `image=example/manual-test:1.0.0`.

**The Telegram message will take about 5 minutes to arrive** — the `info` route has a `group_wait` of `5m`. That delay is correct and deliberate, not a fault. Wait for it and confirm it arrives before proceeding. Report the exact text received.

- [ ] **Step 10: Verify idempotency**

Run the playbook a second time. Expected: `changed=0` for the `diun` role and the handler does not fire. If anything reports changed, fix it before committing.

- [ ] **Step 11: Commit**

```bash
git add roles/diun tests/check-diun-notify.sh playbooks/xps.yml
git commit -m "Add Diun, reporting new image tags through Alertmanager

Diun watches the Docker socket read-only and reports newer tags for the images
this host runs. Delivery goes through Alertmanager rather than Diun's own
Telegram notifier, so image updates group and route like every other alert.

The notification script runs inside Diun's Alpine container, which has no jq,
curl or bash, so the payload is assembled in POSIX sh and posted with busybox
wget. Values are HTML-escaped before being JSON-escaped, because the Telegram
receiver parses HTML and drops any message containing a stray angle bracket.

The alert carries severity: info, whose existing route already has the 5m group
wait and 7d repeat interval the design calls for, so no new route was needed.
An explicit endsAt 30 minutes out makes the alert self-resolve; it is
deliberately longer than the route's group wait, which an alert must outlive to
be delivered at all."
```

---

### Task 2: Label the eighteen containers Diun should watch

**Files:**
- Modify: `roles/actualbudget/tasks/main.yml`, `roles/alertmanager/tasks/main.yml`, `roles/blackbox/tasks/main.yml`, `roles/caddy/tasks/main.yml`, `roles/cadvisor/tasks/main.yml`, `roles/ftp/tasks/main.yml`, `roles/grafana/tasks/main.yml`, `roles/immich/tasks/main.yml`, `roles/kavita/tasks/main.yml`, `roles/planka/tasks/main.yml`, `roles/plex/tasks/main.yml`, `roles/portainer/tasks/main.yml`, `roles/prometheus/tasks/main.yml`, `roles/vaultwarden/tasks/main.yml`

**Interfaces:**
- Consumes: the `diun` container from Task 1, and its `watchByDefault: false` setting, which is why every container needs an explicit label.
- Produces: nothing later tasks consume, beyond Diun having 18 images to analyse.

**⚠️ This task restarts every service on the host.** Adding a label changes the container definition, so Docker recreates each container. Downtime is seconds per service, but it is all of them. Do not run this against the live host without confirming that is expected.

- [ ] **Step 1: Add the labels**

Add a `labels:` block to the `community.docker.docker_container` task in each role below. **Add it to the `docker_container` task only** — never to the `docker_image_pull` task, which takes `name:` and has no `labels:` option.

Every image gets the same two labels:

```yaml
    labels:
      # Watched by Diun. watch_repo lists the repository's tags and reports
      # newer ones -- necessary because the digest pin above freezes the
      # digest, so watching this exact reference would report nothing.
      # No include_tags or exclude_tags: a tag pattern that fails to match a
      # repository's convention produces no error and no signal, so a missed
      # release would never announce itself. Filtering follows once there is
      # real traffic to write it from.
      diun.enable: "true"
      diun.watch_repo: "true"
```

Apply that block to all of these:

| Role | Container | Image |
|---|---|---|
| `actualbudget` | actual_budget | `actualbudget/actual-server:26.8.1` |
| `alertmanager` | alertmanager | `prom/alertmanager:v0.34.0` |
| `blackbox` | blackbox | `prom/blackbox-exporter:v0.28.0` |
| `caddy` | caddy | `caddy:2.11.4` |
| `cadvisor` | cadvisor | `gcr.io/cadvisor/cadvisor:v0.55.1` |
| `grafana` | grafana | `grafana/grafana:13.2.0` |
| `immich` | immich-postgres | `ghcr.io/immich-app/postgres:14-vectorchord0.4.3-pgvectors0.2.0` |
| `immich` | immich-redis | `docker.io/redis:6.2-alpine` |
| `immich` | immich-server | `ghcr.io/immich-app/immich-server:v3.1.0` |
| `immich` | immich-machine-learning | `ghcr.io/immich-app/immich-machine-learning:v3.1.0` |
| `kavita` | kavita | `linuxserver/kavita:v0.9.0.2-ls120` |
| `planka` | planka-postgres | `postgres:16-alpine` |
| `planka` | planka | `ghcr.io/plankanban/planka:1.26.3` |
| `plex` | plex | `plexinc/pms-docker:1.43.3.10896-cb3ebc72d` |
| `portainer` | portainer | `portainer/portainer-ce:2.39.6` |
| `prometheus` | prometheus | `prom/prometheus:v3.14.0` |
| `vaultwarden` | vaultwarden | `vaultwarden/server:1.37.1` |

That is 17 containers. The eighteenth is different — see the next step.

- [ ] **Step 2: Label the FTP container differently**

`roles/ftp/tasks/main.yml` runs `fauria/vsftpd:latest`. That repository publishes **only** the `latest` tag, so `watch_repo` there would report every unrelated tag while missing the thing that actually moves. Use digest watching instead — which is Diun's default, so `watch_repo` is simply omitted:

```yaml
    labels:
      # Watched for DIGEST changes, not by repository: this repository
      # publishes only the `latest` tag, so there is no version tag to watch
      # and repo watching would report noise while missing the one thing that
      # can change. In practice this is unlikely ever to fire -- the image was
      # last pushed in January 2023.
      diun.enable: "true"
```

- [ ] **Step 3: Verify the edit count before deploying**

Confirm you added exactly 18 label blocks and did not disturb anything else:

```bash
grep -rc "diun.enable" roles/*/tasks/main.yml | grep -v ":0"
grep -rc "diun.watch_repo" roles/*/tasks/main.yml | grep -v ":0"
git diff --stat
```

Expected: `diun.enable` totals 18 across 14 files; `diun.watch_repo` totals 17 (the `ftp` role has none). `git diff --stat` shows only additions, and only in the 14 role files. If any file shows deletions, you have removed something — investigate before continuing.

- [ ] **Step 4: Deploy**

```bash
ansible-playbook -i inventory/hosts.ini playbooks/xps.yml
```

Every container will be recreated. Confirm they all came back:

```bash
ssh daniel@xps.fritz.box 'docker ps --format "{{.Names}}: {{.Status}}" | sort'
```

Expected: every container `Up`. Nothing in `Restarting` or `Exited`.

- [ ] **Step 5: Verify Diun sees all eighteen**

```bash
ssh daniel@xps.fritz.box 'docker exec diun diun image list 2>&1 | tail -30'
```

Expected: 18 images listed. If Diun reports fewer, a label was missed or malformed — find which and fix it before committing.

Also confirm the services are actually answering, not merely running:

```bash
ssh daniel@xps.fritz.box 'for u in books budget grafana passwords photos projectboard; do
  printf "%-14s %s\n" "$u" "$(curl -sL -o /dev/null -w "%{http_code}" --max-time 10 https://$u.home.danmidwood.com/)"
done'
```

Expected: all `200`.

- [ ] **Step 6: Verify idempotency**

Run the playbook again. Expected: `changed=0`. A second run that still reports changed means a label value is being reformatted on each run — fix it.

- [ ] **Step 7: Commit**

```bash
git add roles/
git commit -m "Tell Diun which images to watch, by container label

Every container that should be watched now carries diun.enable, and the
seventeen with real version tags carry diun.watch_repo so Diun reports newer
tags rather than a digest that pinning has frozen.

The label lives beside the image reference it describes, so a service added
later is watched the moment it ships and the watch list cannot drift from what
is actually running.

The FTP image is deliberately different: fauria/vsftpd publishes only the
latest tag, so it is watched for digest changes instead."
```

---

### Task 3: Prove it end to end, and record what was learned

**Files:**
- Modify: `docs/superpowers/specs/2026-08-19-observability-alerting-design.md`

**Interfaces:**
- Consumes: the deployed Diun from Tasks 1 and 2.
- Produces: nothing — this is the verification and documentation task.

- [ ] **Step 1: Force a real check and observe a real notification**

Diun runs on a daily schedule, so do not wait for it. Delete its record of seen tags for a single image and force an immediate check, which makes Diun treat that image's current tags as newly discovered.

The safest way to force this without touching the database is to run Diun's one-shot analysis against a deliberately outdated reference. Add a temporary file provider entry:

```bash
ssh daniel@xps.fritz.box 'docker exec diun sh -c "cat > /tmp/probe.yml <<EOF
providers:
  file:
    filename: /tmp/probe-images.yml
EOF
cat > /tmp/probe-images.yml <<EOF
- name: caddy:2.0.0
  watch_repo: true
  max_tags: 5
EOF
"'
```

Then check what Diun does with it:

```bash
ssh daniel@xps.fritz.box 'docker logs -f diun 2>&1 | head -40'
```

If forcing a check this way proves awkward, the acceptable alternative is to stop Diun, remove `/mnt/storage/config/diun/data/diun.db`, set `firstCheckNotif: true` temporarily in `roles/diun/files/diun.yml`, redeploy, and let the first check announce everything — then restore `firstCheckNotif: false`, remove the database again, and redeploy so the steady state is unchanged. **This produces one Telegram message per watched tag, so expect a burst.** Say in your report which method you used.

Expected either way: at least one genuine `ImageUpdateAvailable` alert reaches Telegram, generated by Diun itself rather than by hand.

- [ ] **Step 2: Confirm grouping behaves as designed**

When several images are reported at once, they must arrive as one Telegram message, not one per image. Alertmanager's `group_by: ['alertname']` and the info route's `group_wait: 5m` are what produce that.

```bash
ssh daniel@xps.fritz.box 'curl -s http://localhost:9093/api/v2/alerts/groups | \
  jq -r ".[] | select(.labels.alertname==\"ImageUpdateAvailable\") | \
  \"group: \(.labels.alertname)  members: \(.alerts | length)\""'
```

Expected: a single group containing every reported image. Record the member count and confirm the Telegram message you received listed them together.

- [ ] **Step 3: Confirm the alert self-resolves**

Wait 30 minutes after the last notification and confirm the alert clears on its own rather than re-notifying forever:

```bash
ssh daniel@xps.fritz.box 'curl -s http://localhost:9093/api/v2/alerts | \
  jq -r "[.[] | select(.labels.alertname==\"ImageUpdateAvailable\")] | length"'
```

Expected: `0`. If it is still firing, the `endsAt` calculation is wrong — investigate before finishing.

- [ ] **Step 4: Restore any temporary state**

Remove `/tmp/probe.yml` and `/tmp/probe-images.yml` from the container if you created them, confirm `roles/diun/files/diun.yml` still has `firstCheckNotif: false`, and confirm `git status` is clean apart from the spec edit in the next step.

- [ ] **Step 5: Update the spec**

In `docs/superpowers/specs/2026-08-19-observability-alerting-design.md`:

1. Resolve the "Risk: Diun's payload shape" section. The risk is settled: the script notifier works, and the fallback to Diun's built-in Telegram notifier was not needed. Rewrite the section to say what was built — a POSIX sh script inside the container posting to `/api/v2/alerts` — and note that the image ships no jq, curl or bash, and that busybox `date` rejects GNU relative arithmetic, since both constrain anyone editing that script later.
2. In the routing table, note that Diun image updates use the existing `severity: info` route rather than a route of their own, because its group wait and repeat interval already match exactly what the design asked for.
3. Mark increment 6 delivered in the build-order table by appending ` — delivered 2026-08-21` to its row, matching increments 1 to 5.
4. Add a Known gaps entry recording that `fauria/vsftpd` was last published in January 2023, that there is consequently no upstream update Diun can ever report for it, and that an FTP server on a three-year-old base image is a standing exposure needing a decision rather than a watch.

- [ ] **Step 6: Commit**

```bash
git add docs/superpowers/specs/2026-08-19-observability-alerting-design.md
git commit -m "Mark alerting increment 6 as delivered

Diun's payload risk is resolved: the script notifier posts to Alertmanager
directly and the Telegram fallback was not needed. Records the container's
tooling constraints, since they bind anyone editing that script later.

Also records that the FTP image has had no upstream release since January 2023,
so Diun can never report an update for it -- that one needs a decision rather
than monitoring."
```

---

## Self-Review

**Spec coverage.** "What Diun watches, given digest pinning" → Task 2 labels, with the FTP exception in Step 2. "Risk: Diun's payload shape" → Task 1 Steps 3-4, resolved in Task 3 Step 5. "One-shot events need explicit expiry" → the `endsAt` in Task 1 Step 3, verified in Task 3 Step 3. Routing table (5m/7d) → satisfied by `severity: info`, recorded in Task 3 Step 5. Deliverables list `diun` — container, Docker socket read-only → Task 1 Step 7. Configuration delivery rule → the conf directory is mounted as a directory in Task 1 Step 7.

**Deviation from the spec, recorded deliberately.** The spec's routing table lists "Diun image updates" as its own route. No separate route is built, because the existing `severity: info` route already has `group_wait: 5m` and `repeat_interval: 168h` — precisely the values that row specifies. Adding a second route with identical timings would be duplication that could drift. Task 3 Step 5 records this in the spec.

**Placeholder scan.** No TBDs. Every image reference, label name, environment variable name and shell built-in used here was verified on the host before writing, and is listed in "Facts Already Verified".

**Consistency.** The alert name `ImageUpdateAvailable`, the label set (`alertname`, `severity`, `instance`, `job`, `image`) and the `ALERTMANAGER_URL` variable are identical in the script (Task 1 Step 3), the test (Task 1 Step 1) and the verification queries (Task 3 Steps 2-3). The container name `diun` is identical in the role, the handler and every `docker exec` command.
