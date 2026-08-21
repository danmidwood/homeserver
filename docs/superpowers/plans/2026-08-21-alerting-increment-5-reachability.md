# Alerting Increment 5: Reachability and TLS Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Find out when a service stops answering from outside, or a certificate stops renewing, instead of discovering it when someone tries to use it.

**Architecture:** The blackbox exporter runs as a digest-pinned container on `caddy_network`. Prometheus scrapes it once per endpoint, passing the target as a URL parameter, so the endpoint list lives in the scrape config and the rules stay static and testable. Three rules watch the results.

**Tech Stack:** Ansible, Docker, blackbox exporter v0.28.0, Prometheus relabeling, promtool

**Spec:** `docs/superpowers/specs/2026-08-19-observability-alerting-design.md` (the "Reachability and TLS — blackbox exporter" section)

## Global Constraints

- **Every container image is pinned by digest**, form `name:tag@sha256:...`.
- **blackbox image:** `prom/blackbox-exporter:v0.28.0@sha256:e753ff9f3fc458d02cca5eddab5a77e1c175eee484a8925ac7d524f04366c2fc`
- **Alert `severity` must be exactly one of** `critical`, `warning`, `info`, `none`. The deployed Alertmanager routes on precisely these; anything else is silently delivered mislabelled as WARNING via the default route.
- **Rule files live in `roles/prometheus/files/rules/`**, copied verbatim, so Go template expressions in annotations need NO escaping. Never put a rule file under `templates/` — the harness globs `files/rules/*.yml` and would never see it.
- **Beware `{#` in any `.j2` file.** Jinja reads it as a comment opener and raises `TemplateSyntaxError`, failing the whole playbook run.
- **Every rule gets a unit test asserting both firing AND silence.**
- **The test harness is `./tests/run-promtool.sh`**, run from the repo root.
- **Verify a sensor by the observable that would change if it were broken**, not by whether its metric name exists. Increment 4 shipped a rule that could never fire because a series count was mistaken for a working data source.
- **Roles must be idempotent:** a second `ansible-playbook -i inventory/hosts.ini playbooks/xps.yml` reports zero changed tasks.
- **Never write a credential into a committed file.**
- **Ansible target:** host `xps` (`xps.fritz.box`), user `daniel`.
- Commit messages end with: `Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>`

---

## Verified facts this plan is built on

Gathered from the live host and from outside it. Do not re-derive; re-verify anything that looks wrong.

| Fact | Value |
|---|---|
| Hostnames resolve to | `217.155.163.253` — a public address |
| NAT hairpinning | **Works** — the server reaches its own public IP, HTTP 302 in 76ms |
| Port 9115 | free |
| blackbox image | `v0.28.0`, digest verified by pulling and inspecting on the server |
| `budget`, `photos`, `books`, `projectboard`, `passwords` | return 200 |
| `grafana` | returns 302 to `/login`, follows to 200 |
| **`plex` root** | **returns 401** — must be probed at `/identity`, which returns 200 unauthenticated |
| Certificate | Let's Encrypt, currently expiring 16 Oct 2026 |
| IPv6 | endpoints publish no AAAA record; server has a global v6 address but no working v6 route |

## File Structure

**Created:**

| Path | Responsibility |
|---|---|
| `roles/blackbox/tasks/main.yml` | Runs the blackbox exporter |
| `roles/blackbox/files/blackbox.yml` | Its probe module definition |
| `roles/prometheus/files/rules/probe.yml` | The reachability and TLS rules |
| `tests/rules/probe_test.yml` | Unit tests for those rules |

**Modified:**

| Path | Change |
|---|---|
| `playbooks/xps.yml` | Add the `blackbox` role |
| `roles/prometheus/templates/prometheus.yml.j2` | Scrape blackbox once per endpoint |
| `roles/prometheus/defaults/main.yml` | Add `monitored_endpoints` |
| `docs/superpowers/specs/2026-08-19-observability-alerting-design.md` | Mark increment 5 delivered |

---

### Task 1: The blackbox exporter and its scrape config

Produces the metrics. No rules yet — Task 2 writes them against names verified here.

**Files:**
- Create: `roles/blackbox/tasks/main.yml`
- Create: `roles/blackbox/files/blackbox.yml`
- Modify: `playbooks/xps.yml`
- Modify: `roles/prometheus/templates/prometheus.yml.j2`
- Modify: `roles/prometheus/defaults/main.yml`

**Interfaces:**
- Consumes: the existing `caddy_network`, created by the `caddy` role.
- Produces: a `blackbox` scrape job, and these metrics for Task 2 — `probe_success`, `probe_ssl_earliest_cert_expiry`, `probe_http_status_code`, `probe_duration_seconds`, each labelled `instance="<probed url>"` and `job="blackbox"`.

- [ ] **Step 1: Verify the pinned digest**

The image is already pulled on the server. `docker buildx` is NOT installed there, so confirm by inspecting:

```bash
ssh daniel@xps.fritz.box "docker inspect --format '{{index .RepoDigests 0}}' prom/blackbox-exporter:v0.28.0"
```

Expected: `prom/blackbox-exporter@sha256:e753ff9f3fc458d02cca5eddab5a77e1c175eee484a8925ac7d524f04366c2fc`

If it differs, upstream re-pushed the tag — use the printed value and update this plan's Global Constraints before continuing.

- [ ] **Step 2: Write the probe module config**

Create `roles/blackbox/files/blackbox.yml`:

```yaml
modules:
  http_2xx:
    prober: http
    timeout: 10s
    http:
      method: GET
      # Grafana redirects / to /login, so the probe must follow to reach a
      # 2xx. Without this it would report Grafana permanently down.
      follow_redirects: true
      # Empty means "any 2xx". Anything wider would let a 500 page count as
      # a healthy service.
      valid_status_codes: []
      # Verified: these hostnames publish no AAAA record, and the server
      # has a global IPv6 address but no working IPv6 route. So IPv6 is
      # never attempted today. This is set explicitly anyway, so that
      # someone adding an AAAA record later cannot silently change which
      # path is being probed without noticing.
      preferred_ip_protocol: ip4
```

- [ ] **Step 3: Declare the endpoints**

Add to `roles/prometheus/defaults/main.yml`, below the existing `monitored_containers`:

```yaml
# The URLs blackbox probes, matching the Caddyfile's hostnames.
#
# plex is probed at /identity rather than /: its root returns 401 because it
# requires authentication, so a default probe would report it permanently
# down and EndpointDown would fire forever. /identity is unauthenticated and
# returns 200 with the server's machineIdentifier.
monitored_endpoints:
  - https://books.home.danmidwood.com
  - https://budget.home.danmidwood.com
  - https://grafana.home.danmidwood.com
  - https://passwords.home.danmidwood.com
  - https://photos.home.danmidwood.com
  - https://plex.home.danmidwood.com/identity
  - https://projectboard.home.danmidwood.com
```

- [ ] **Step 4: Write the blackbox role**

Create `roles/blackbox/tasks/main.yml`:

```yaml
- name: Create blackbox config directory
  ansible.builtin.file:
    path: /mnt/storage/config/blackbox
    state: directory
    owner: root
    group: root
    mode: '0755'

- name: Write the blackbox probe module config
  ansible.builtin.copy:
    src: blackbox.yml
    dest: /mnt/storage/config/blackbox/blackbox.yml
    owner: root
    group: root
    mode: '0644'
  notify: Restart blackbox

- name: Pull the blackbox exporter image
  community.docker.docker_image_pull:
    name: "prom/blackbox-exporter:v0.28.0@sha256:e753ff9f3fc458d02cca5eddab5a77e1c175eee484a8925ac7d524f04366c2fc"
    pull: not_present

- name: Run blackbox exporter container
  community.docker.docker_container:
    name: blackbox
    image: "prom/blackbox-exporter:v0.28.0@sha256:e753ff9f3fc458d02cca5eddab5a77e1c175eee484a8925ac7d524f04366c2fc"
    state: started
    restart_policy: always
    networks:
      - name: caddy_network
    volumes:
      - /mnt/storage/config/blackbox/blackbox.yml:/etc/blackbox_exporter/config.yml:ro
    command:
      - "--config.file=/etc/blackbox_exporter/config.yml"
```

Create `roles/blackbox/handlers/main.yml`:

```yaml
- name: Restart blackbox
  community.docker.docker_container:
    name: blackbox
    state: started
    restart: true
```

No host port is published: Prometheus reaches it by container name over `caddy_network`, and nothing else needs it. This is deliberately narrower than the `prometheus` and `alertmanager` roles, which publish ports for their web UIs.

- [ ] **Step 5: Scrape blackbox once per endpoint**

In `roles/prometheus/templates/prometheus.yml.j2`, add after the existing `cadvisor` job:

```yaml
  # Blackbox is scraped once per endpoint. The relabeling passes each target
  # to the exporter as a URL parameter, then rewrites the address so
  # Prometheus actually talks to blackbox rather than to the endpoint.
  - job_name: "blackbox"
    metrics_path: /probe
    params:
      module: [http_2xx]
    static_configs:
      - targets:
{% for endpoint in monitored_endpoints %}
          - "{{ endpoint }}"
{% endfor %}
    relabel_configs:
      - source_labels: [__address__]
        target_label: __param_target
      - source_labels: [__param_target]
        target_label: instance
      - target_label: __address__
        replacement: blackbox:9115
```

Note `instance` becomes the probed URL rather than `xps`. That deviates from every other job here and is correct: each probe needs its own identity, and an alert naming the failing URL is more use than one naming the host that did the probing.

- [ ] **Step 6: Add the role to the playbook**

In `playbooks/xps.yml`, add `    - blackbox` immediately after the `    - cadvisor` line.

**INSERT ONE LINE. Do not rewrite the surrounding block.** The playbook currently has 23 role entries; after this edit it must have exactly 24, and every existing role must still be present. `caddy` creates `caddy_network`, which blackbox joins, so blackbox must come after it — `cadvisor` already does.

- [ ] **Step 7: Deploy**

```bash
ansible-playbook -i inventory/hosts.ini playbooks/xps.yml
```

- [ ] **Step 8: Verify every endpoint probes successfully**

This is the step that matters. Check each of the seven, not a sample:

```bash
ssh daniel@xps.fritz.box 'curl -s --get http://localhost:9090/api/v1/query --data-urlencode "query=probe_success" | jq -r ".data.result[] | \"\(.metric.instance) \(.value[1])\"" | sort'
```

Expected: seven results, every value `1`.

**If any endpoint reports 0, STOP and report which one.** Do not add it to an exclusion list and do not widen `valid_status_codes` to make it pass — either would ship a probe that cannot detect the thing it exists to detect. The known trap is plex: if `https://plex.home.danmidwood.com/identity` is not what got configured, its root returns 401 and the probe fails.

Confirm the certificate metric is present too, since a rule depends on it:

```bash
ssh daniel@xps.fritz.box 'curl -s --get http://localhost:9090/api/v1/query --data-urlencode "query=probe_ssl_earliest_cert_expiry" | jq -r ".data.result | length"'
```

Expected: 7.

- [ ] **Step 9: Confirm the probe is testing what it should**

A probe that silently resolved to something local would pass Step 8 while testing nothing. Confirm the exporter is really reaching the public path:

```bash
ssh daniel@xps.fritz.box 'curl -s "http://localhost:9115/probe?target=https://books.home.danmidwood.com&module=http_2xx&debug=true" | grep -iE "probe_ip_addr_hash|Resolved target address|status code" | head -5'
```

Expected: the debug output shows the resolved address as the public IP `217.155.163.253`, not a private one, and a 200 status.

- [ ] **Step 10: Confirm the scrape target is healthy**

```bash
ssh daniel@xps.fritz.box 'curl -s http://localhost:9090/api/v1/targets | jq -r ".data.activeTargets[] | \"\(.labels.job) \(.labels.instance) \(.health)\"" | sort'
```

Expected: `blackbox` appears seven times, all `up`, alongside the existing `cadvisor`, `node` and `prometheus` targets.

- [ ] **Step 11: Verify idempotency**

```bash
ansible-playbook -i inventory/hosts.ini playbooks/xps.yml
```

Expected: `changed=0`.

- [ ] **Step 12: Commit**

```bash
git add roles/blackbox roles/prometheus/defaults/main.yml roles/prometheus/templates/prometheus.yml.j2 playbooks/xps.yml
git commit -m "Add the blackbox exporter and probe the Caddy endpoints

Container metrics cannot see an application that is running but wedged,
or a certificate that has stopped renewing. These probes traverse the
real external path: DNS, the router's port forwarding, Caddy, TLS and
the upstream container.

plex is probed at /identity rather than /, because its root returns 401
and a default probe would have reported it permanently down.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

### Task 2: The reachability and TLS rules

**Files:**
- Create: `roles/prometheus/files/rules/probe.yml`
- Create: `tests/rules/probe_test.yml`

**Interfaces:**
- Consumes: `probe_success` and `probe_ssl_earliest_cert_expiry`, both labelled `instance="<url>"` and `job="blackbox"`, verified to exist in Task 1 Step 8.
- Produces: three alerts — `EndpointDown`, `CertExpiringSoon`, `BlackboxMetricsMissing`.

- [ ] **Step 1: Write the failing tests**

Create `tests/rules/probe_test.yml`:

```yaml
rule_files:
  - ../../roles/prometheus/files/rules/probe.yml

evaluation_interval: 1m

tests:
  # A healthy endpoint with a distant certificate expiry: complete silence.
  # promtool's clock starts at zero, so a certificate expiring at 2000000
  # seconds is comfortably more than 14 days (1209600s) away at these
  # evaluation times.
  - interval: 1m
    input_series:
      - series: 'probe_success{instance="https://books.home.danmidwood.com",job="blackbox"}'
        values: '1+0x80'
      - series: 'probe_ssl_earliest_cert_expiry{instance="https://books.home.danmidwood.com",job="blackbox"}'
        values: '2000000+0x80'
    alert_rule_test:
      - eval_time: 70m
        alertname: EndpointDown
        exp_alerts: []
      - eval_time: 70m
        alertname: CertExpiringSoon
        exp_alerts: []
      - eval_time: 70m
        alertname: BlackboxMetricsMissing
        exp_alerts: []

  # An endpoint that stops answering.
  - interval: 1m
    input_series:
      - series: 'probe_success{instance="https://passwords.home.danmidwood.com",job="blackbox"}'
        values: '0+0x30'
    alert_rule_test:
      - eval_time: 3m
        alertname: EndpointDown
        exp_alerts: []
      - eval_time: 8m
        alertname: EndpointDown
        exp_alerts:
          - exp_labels:
              severity: critical
              instance: https://passwords.home.danmidwood.com
              job: blackbox

  # A certificate inside the 14-day window while the endpoint still works.
  - interval: 1m
    input_series:
      - series: 'probe_success{instance="https://photos.home.danmidwood.com",job="blackbox"}'
        values: '1+0x80'
      - series: 'probe_ssl_earliest_cert_expiry{instance="https://photos.home.danmidwood.com",job="blackbox"}'
        values: '1000000+0x80'
    alert_rule_test:
      # NOT coverage: the condition is already true here, so this only
      # confirms the 1h `for:` has not been removed. The genuine
      # stays-silent case is the first test's distant expiry at 70m.
      - eval_time: 30m
        alertname: CertExpiringSoon
        exp_alerts: []
      - eval_time: 70m
        alertname: CertExpiringSoon
        exp_alerts:
          - exp_labels:
              severity: warning
              instance: https://photos.home.danmidwood.com
              job: blackbox

  # A near-expiry certificate on an endpoint that is DOWN must not add a
  # second alert: EndpointDown already covers it, and a failed TLS handshake
  # is why the expiry looks alarming in the first place.
  - interval: 1m
    input_series:
      - series: 'probe_success{instance="https://plex.home.danmidwood.com/identity",job="blackbox"}'
        values: '0+0x80'
      - series: 'probe_ssl_earliest_cert_expiry{instance="https://plex.home.danmidwood.com/identity",job="blackbox"}'
        values: '1000000+0x80'
    alert_rule_test:
      - eval_time: 70m
        alertname: CertExpiringSoon
        exp_alerts: []

  # No probe results at all — the exporter stopped or was never scraped.
  - interval: 1m
    input_series: []
    alert_rule_test:
      - eval_time: 30m
        alertname: BlackboxMetricsMissing
        exp_alerts: []
      - eval_time: 70m
        alertname: BlackboxMetricsMissing
        exp_alerts:
          - exp_labels:
              severity: warning

  # Probes being returned keeps BlackboxMetricsMissing quiet well past its
  # for:, so the assertion can actually fail if the rule were inverted.
  - interval: 1m
    input_series:
      - series: 'probe_success{instance="https://books.home.danmidwood.com",job="blackbox"}'
        values: '1+0x80'
    alert_rule_test:
      - eval_time: 70m
        alertname: BlackboxMetricsMissing
        exp_alerts: []
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `./tests/run-promtool.sh`

Expected: FAIL. `promtool check rules` errors because `roles/prometheus/files/rules/probe.yml` does not exist.

- [ ] **Step 3: Write the rules**

Create `roles/prometheus/files/rules/probe.yml`:

```yaml
groups:
  - name: probe
    rules:
      # These probes traverse the real external path — DNS, the router's
      # port forwarding, Caddy, TLS, and the upstream container — so they
      # catch the failure container metrics cannot see: the container is
      # running and healthy while the application inside it is wedged.

      - alert: EndpointDown
        expr: probe_success == 0
        for: 5m
        labels:
          severity: critical
        annotations:
          summary: "{{ $labels.instance }} is not answering"
          description: "The probe to {{ $labels.instance }} has failed for over 5 minutes. The container may be running while the application inside it is wedged, or Caddy may have lost its upstream. Check `docker ps` and Caddy's logs."

      - alert: CertExpiringSoon
        # A renewal-is-broken detector, not an expiry detector: Caddy renews
        # at 30 days, so reaching 14 means renewal has been failing for a
        # fortnight.
        #
        # The probe_success guard matters. When a probe fails the expiry
        # metric can read zero, which looks like a certificate that expired
        # in 1970 and would fire this rule alongside EndpointDown for the
        # same underlying fault. One alert per fault.
        expr: |
          (probe_ssl_earliest_cert_expiry - time() < 14 * 24 * 3600)
            and on(instance) (probe_success == 1)
        for: 1h
        labels:
          severity: warning
        annotations:
          summary: "{{ $labels.instance }} certificate expires soon"
          description: "The certificate for {{ $labels.instance }} has under 14 days left. Caddy renews at 30 days, so renewal has already been failing for two weeks. Check `docker logs caddy` for ACME errors."

      - alert: BlackboxMetricsMissing
        # Not redundant with EndpointDown: that compares a value, and an
        # absent series has no value to compare. Without this, an exporter
        # that stopped being scraped would leave every endpoint unwatched
        # while nothing fired.
        expr: absent(probe_success)
        for: 1h
        labels:
          severity: warning
        annotations:
          summary: "No endpoint probes are being returned"
          description: "No probe_success metric exists, so EndpointDown cannot fire and no endpoint is being checked. Check that the blackbox container is running and that Prometheus's blackbox job is up."
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `./tests/run-promtool.sh`

Expected: PASS, with SUCCESS for `probe_test.yml` alongside the existing files.

If promtool reports an annotation mismatch, this version compares annotations and treats an omitted `exp_annotations` as "expect none". Add `exp_annotations` blocks copying the exact rendered text from promtool's failure output rather than predicting it.

- [ ] **Step 5: Verify the tests constrain the rules (mutation testing)**

Apply each mutation, confirm the suite FAILS, revert, confirm it passes. Report before and after for each.

| Mutation | Expected |
|---|---|
| `probe_success == 0` → `== 1` | FAIL |
| `EndpointDown` `for: 5m` → `for: 999h` | FAIL |
| `< 14 * 24 * 3600` → `< 0` | FAIL |
| `< 14 * 24 * 3600` → `< 99999999` | FAIL |
| Delete the `and on(instance) (probe_success == 1)` guard | FAIL — the down-endpoint case would raise a second alert |
| `absent(probe_success)` → `absent(nonexistent_metric)` | FAIL |
| `EndpointDown` severity `critical` → `warning` | FAIL |

A mutation that still passes means that rule's assertions are not doing their job — strengthen them before continuing.

- [ ] **Step 6: Deploy and confirm the rules load**

```bash
ansible-playbook -i inventory/hosts.ini playbooks/xps.yml
ssh daniel@xps.fritz.box 'curl -s http://localhost:9090/api/v1/rules | jq -r ".data.groups[] | select(.name==\"probe\") | .rules[] | \"\(.name) \(.health)\""'
```

Expected: three rules, all `ok`.

```bash
ssh daniel@xps.fritz.box 'curl -s http://localhost:9090/api/v1/alerts | jq -r ".data.alerts[].labels.alertname" | sort -u'
```

Expected: only `Watchdog`. Any probe alert firing now means either a real problem or a misconfigured endpoint — investigate rather than adjusting the list to silence it.

- [ ] **Step 7: Verify idempotency**

```bash
ansible-playbook -i inventory/hosts.ini playbooks/xps.yml
```

Expected: `changed=0`.

- [ ] **Step 8: Commit**

```bash
git add roles/prometheus/files/rules/probe.yml tests/rules/probe_test.yml
git commit -m "Add reachability and TLS alert rules

EndpointDown catches an application that is running but wedged, which no
container metric can see. CertExpiringSoon is a renewal-is-broken
detector: Caddy renews at 30 days, so 14 means renewal has been failing
for a fortnight.

CertExpiringSoon is guarded on probe_success so a failed probe does not
raise two alerts for one fault.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

### Task 3: Fault injection and spec update

**Files:**
- Modify: `docs/superpowers/specs/2026-08-19-observability-alerting-design.md`

**Interfaces:**
- Consumes: everything from Tasks 1 and 2.
- Produces: nothing.

- [ ] **Step 1: Prove EndpointDown fires on a genuinely unreachable service**

Record the Telegram counter first:

```bash
ssh daniel@xps.fritz.box 'curl -s http://localhost:9093/metrics | grep -E "^alertmanager_notifications_total\{integration=\"telegram\""'
```

Stop the service behind the least critical endpoint. **Use `kavita`**, which serves `books.home.danmidwood.com` — it is a book reader, nobody is mid-stream on it, and unlike Vaultwarden or Immich its brief absence costs nothing.

```bash
ssh daniel@xps.fritz.box 'docker stop kavita'
```

Poll until `EndpointDown` reaches `firing`. Do NOT use a local `sleep`; sleep on the remote host inside the ssh command, e.g.
`ssh daniel@xps.fritz.box 'sleep 120; curl -s http://localhost:9090/api/v1/alerts | jq -r ".data.alerts[] | \"\(.labels.alertname) \(.labels.instance) \(.state)\""'`

Expected within about 7 minutes: `EndpointDown` firing for `https://books.home.danmidwood.com`, and a 🔴 CRITICAL Telegram message naming that URL. Confirm the counter incremented with no matching failure.

**Note what else should happen:** `ContainerMissing` should also fire for `kavita`, because increment 4 watches the container while this increment watches the endpoint. Two alerts for one fault is correct here — they are different observations, and seeing both is how you tell "the container died" from "the container is fine but the app is wedged". Record whether both fired.

- [ ] **Step 2: Confirm it clears**

```bash
ssh daniel@xps.fritz.box 'docker start kavita'
```

Poll until both `EndpointDown` and `ContainerMissing` disappear from the alerts API.

Expected: ✅ RESOLVED messages. Confirm `books.home.danmidwood.com` serves again:

```bash
ssh daniel@xps.fritz.box 'curl -sL -o /dev/null -w "%{http_code}\n" --max-time 10 https://books.home.danmidwood.com'
```

Expected: 200.

- [ ] **Step 3: Prove the wedged-application case, which is the one container metrics miss**

Stopping a container proves little that increment 4 did not already prove. This step proves the distinct thing: an endpoint failing while its container is perfectly healthy.

Point Caddy at a dead upstream port for one host only, on the server's deployed copy:

```bash
ssh daniel@xps.fritz.box 'sudo sed -i "s|reverse_proxy kavita:5000|reverse_proxy kavita:5999|" /mnt/storage/config/caddy/Caddyfile'
ssh daniel@xps.fritz.box 'docker exec caddy caddy reload --config /etc/caddy/Caddyfile'
```

Poll until `EndpointDown` fires for `https://books.home.danmidwood.com`.

Expected: `EndpointDown` fires, and `ContainerMissing` does NOT — kavita is running throughout. That difference is the entire justification for this increment.

Restore by re-running Ansible, which rewrites the Caddyfile from the repo:

```bash
ansible-playbook -i inventory/hosts.ini playbooks/xps.yml
```

Confirm the repo copy was never edited (`git status` clean) and that the endpoint serves again.

- [ ] **Step 4: Update the spec**

Mark increment 5 delivered in the build-order table by appending ` — delivered 2026-08-21` to its row, matching increments 1 to 4.

Record in the reachability section what Step 3 demonstrated: that `EndpointDown` fires on a wedged upstream while `ContainerMissing` stays silent, which is the failure class this bundle was added for.

- [ ] **Step 5: Commit**

```bash
git add docs/superpowers/specs/2026-08-19-observability-alerting-design.md
git commit -m "Mark alerting increment 5 as delivered

Fault injection confirmed EndpointDown fires on a stopped service and
clears, and — the case container metrics cannot see — that it fires on a
wedged upstream while the container stays healthy and ContainerMissing
stays silent.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

## Deliberately out of scope

- **Probing from outside the network.** NAT hairpinning means these probes traverse the real external path, but they work whether or not the ISP is up, so they cannot distinguish a dead internet connection from a healthy one. Closing that needs an off-box prober, which is a different kind of dependency and belongs in its own increment.
- **Response-time alerting.** `probe_duration_seconds` is collected and useful in Grafana, but no rule watches it: there is no evidence yet of what normal looks like for these services, and a threshold picked today would be a guess.
- **Probing services with no Caddy hostname.** `portainer`, `prometheus`, `alertmanager` and `cadvisor` are deliberately LAN-only and absent from the Caddyfile, so they have no external path to probe. Container health already covers them.
