#!/bin/bash
# Reports container images that have a newer version tag available, by
# comparing the tag you are running against the registry's tag list.
#
# Why this exists rather than Diun: Diun tracks every tag as its own entry and
# fetches a manifest per tag on every run, which costs roughly two registry
# requests per tracked tag, forever. Docker Hub allows 100 requests per hour
# per IP, and these images have ~11,200 tags between them -- Diun exhausted the
# quota without finishing its second repository. Listing a repository's tags is
# ONE request and answers the only question being asked here: is there
# something newer than what I pinned. This script therefore costs about one
# request per image, seventeen per run, instead of thousands.
#
# Which images are checked comes from container labels, so the watch list
# cannot drift from what is actually running:
#   imagewatch.enable        = "true"     opt in
#   imagewatch.exclude_tags  = "re1;re2"  semicolon-separated regexes
#
# Tags are excluded, never included. An exclude pattern that is wrong produces
# visible noise; an include pattern that is wrong produces silence, and a
# release that never arrives never announces itself.

set -uo pipefail

ALERTMANAGER_URL="${ALERTMANAGER_URL:-http://localhost:9093/api/v2/alerts}"

# Registry calls retry before giving up. A large repository is paginated, so one
# image can take several requests, and a single transient 5xx or reset would
# otherwise mark that image as unchecked and raise ImageWatchFailed -- which is
# what happened on the first scheduled run, for a repository that was reachable
# again seconds later. --retry-all-errors is needed because curl only retries
# transient HTTP codes by default, not connection resets.
RETRY=(--retry 3 --retry-delay 2 --retry-all-errors)
LABEL_ENABLE="imagewatch.enable"
LABEL_EXCLUDE="imagewatch.exclude_tags"

# Applied to every image, on top of whatever the container's label adds.
# General rules live here; repository-specific ones live on the label.
#
# These two drop anything that is not shaped like a version. `sort -V` ranks
# alphabetic tags ABOVE numeric ones, so without this the newest tag reported
# for caddy was "windowsservercore-ltsc2025", for plex "public", and for
# prometheus a release candidate -- all technically newer in sort order, none
# of them an answer to "is there a newer release".
#
# The third drops architecture-suffixed tags. plexinc/pms-docker publishes
# 1.43.3.10896-cb3ebc72d-armhf alongside the plain tag, and because plex's build
# hash means no tag ever shares its shape, the fallback reported that ARM build
# as an available update. An architecture suffix is never an upgrade of the
# version it is attached to, so excluding it cannot hide a real release.
#
# This is the one place an include-shaped rule is used rather than an exclude,
# and it is deliberate: every release convention in use here starts with a
# digit, or a v followed by a digit (2.11.4, v3.1.0, 16-alpine, v0.9.0.2-ls120,
# 1.43.3.10896-cb3ebc72d, 26.8.1), while none of the noise does (latest, beta,
# public, plexpass, alpine, bookworm, windowsservercore). A release tagged
# without a leading digit would be missed, which is why the rule is stated here
# in one visible place rather than scattered across eighteen labels.
BASELINE_EXCLUDE='^[^0-9v];^v[^0-9];-(amd64|arm64|armhf|armv7|i386|ppc64le|s390x)$'

# ---------------------------------------------------------------------------
# Pure helpers. These are unit-tested offline by tests/check-image-update.sh,
# which sources this file with IMAGEWATCH_LIB_ONLY=1 set.
# ---------------------------------------------------------------------------

# Split a container image reference into "registry repository tag".
#
#   caddy:2.11.4@sha256:ab...              -> docker.io library/caddy 2.11.4
#   docker.io/redis:6.2-alpine@sha256:...  -> docker.io library/redis 6.2-alpine
#   ghcr.io/immich-app/immich-server:v3.1.0 -> ghcr.io immich-app/immich-server v3.1.0
#   gcr.io/cadvisor/cadvisor:v0.55.1       -> gcr.io cadvisor/cadvisor v0.55.1
#
# A leading segment counts as a registry only if it contains a dot or a colon;
# otherwise it is a Docker Hub namespace. That is the same rule the Docker CLI
# uses, and it is why "linuxserver/kavita" is not treated as a registry.
parse_image_ref() {
  local ref="$1"
  ref="${ref%%@*}"

  local registry repo tag first rest
  if [[ "$ref" == */* ]]; then
    first="${ref%%/*}"
    rest="${ref#*/}"
  else
    first=""
    rest="$ref"
  fi

  if [[ -n "$first" && ( "$first" == *.* || "$first" == *:* ) ]]; then
    registry="$first"
    repo="$rest"
  else
    registry="docker.io"
    repo="$ref"
  fi

  # Split the tag off the LAST path segment, so a registry port (host:5000)
  # is never mistaken for a tag.
  local lastseg="${repo##*/}"
  if [[ "$lastseg" == *:* ]]; then
    tag="${lastseg##*:}"
    repo="${repo%:*}"
  else
    tag="latest"
  fi

  # Docker Hub official images live under library/.
  if [[ "$registry" == "docker.io" && "$repo" != */* ]]; then
    repo="library/${repo}"
  fi

  printf '%s %s %s\n' "$registry" "$repo" "$tag"
}

# Given the current tag on stdin's list, print every tag that sorts strictly
# above it. The current tag is injected into the list before sorting so that a
# tag which has been deleted upstream still yields a sane comparison instead of
# silently producing nothing.
#
# sort -V understands the conventions in use here: 2.11.4 sorts below 2.11.10
# (which a lexical sort gets wrong), and v0.9.0.2-ls120, 16-alpine and
# 1.43.3.10896-cb3ebc72d all order sensibly against their own siblings.
newer_tags() {
  local current="$1"
  { printf '%s\n' "$current"; cat; } \
    | grep -v '^[[:space:]]*$' \
    | sort -V -u \
    | awk -v c="$current" 'seen { print } $0 == c { seen = 1 }'
}

# Reduce a tag to its shape by replacing every run of digits with '#'.
#
#   2.11.4                          -> #.#.#
#   2.11.4-windowsservercore-ltsc2025 -> #.#.#-windowsservercore-ltsc#
#   16-alpine                       -> #-alpine
#   v0.9.0.2-ls120                  -> v#.#.#.#-ls#
#
# Comparing only same-shaped tags is what stops "you are running caddy 2.11.4"
# being answered with "2.11.4-windowsservercore-ltsc2025". Variant tags sort
# above the plain version because they share its prefix and are longer, so
# without this the headline is almost always a platform variant of the version
# already installed rather than a newer release.
tag_shape() {
  printf '%s' "$1" | sed -e 's/[0-9][0-9]*/#/g'
}

# Keep only tags whose shape matches the reference tag's shape.
same_shape_as() {
  local want; want="$(tag_shape "$1")"
  local t
  while IFS= read -r t; do
    [[ -z "$t" ]] && continue
    [[ "$(tag_shape "$t")" == "$want" ]] && printf '%s\n' "$t"
  done
}

# Drop tags matching any of the semicolon-separated regexes.
apply_exclusions() {
  local patterns="$1"
  if [[ -z "$patterns" ]]; then cat; return; fi
  local filtered; filtered=$(cat)
  local IFS=';' pat
  for pat in $patterns; do
    [[ -z "$pat" ]] && continue
    # -e is required, not stylistic: a pattern beginning with a hyphen, such as
    # the architecture-suffix rule, is otherwise parsed by grep as options and
    # silently matches nothing. The exclusion then appears configured while
    # doing absolutely nothing.
    filtered=$(printf '%s\n' "$filtered" | grep -Ev -e "$pat" || true)
  done
  printf '%s\n' "$filtered"
}

# Escape for Telegram's HTML parse_mode, which drops the whole message on a
# stray angle bracket or ampersand. Ampersand first, or the entities below get
# re-escaped.
html_escape() {
  sed -e 's/&/\&amp;/g' -e 's/</\&lt;/g' -e 's/>/\&gt;/g'
}

if [[ -n "${IMAGEWATCH_LIB_ONLY:-}" ]]; then
  return 0 2>/dev/null || exit 0
fi

# ---------------------------------------------------------------------------
# Registry access
# ---------------------------------------------------------------------------

registry_host() {
  case "$1" in
    docker.io) echo "registry-1.docker.io" ;;
    *)         echo "$1" ;;
  esac
}

auth_token() {
  local registry="$1" repo="$2"
  case "$registry" in
    docker.io) curl -sf "${RETRY[@]}" --max-time 30 "https://auth.docker.io/token?service=registry.docker.io&scope=repository:${repo}:pull" ;;
    ghcr.io)   curl -sf "${RETRY[@]}" --max-time 30 "https://ghcr.io/token?service=ghcr.io&scope=repository:${repo}:pull" ;;
    gcr.io)    curl -sf "${RETRY[@]}" --max-time 30 "https://gcr.io/v2/token?service=gcr.io&scope=repository:${repo}:pull" ;;
    *)         echo '{}' ;;
  esac | jq -r '.token // empty'
}

# Print every tag in a repository. Paginated via the registry's Link header;
# each page is one request, so even a 2883-tag repository costs a handful.
list_tags() {
  local registry="$1" repo="$2" token="$3"
  local host; host="$(registry_host "$registry")"
  local url="https://${host}/v2/${repo}/tags/list?n=1000"
  local hdr body next

  while [[ -n "$url" ]]; do
    hdr="$(mktemp)"
    body="$(curl -sf "${RETRY[@]}" --max-time 60 -D "$hdr" -H "Authorization: Bearer ${token}" "$url")" || {
      rm -f "$hdr"; return 1
    }
    printf '%s\n' "$body" | jq -r '.tags[]? // empty'
    next="$(grep -i '^link:' "$hdr" | sed -n 's/.*<\([^>]*\)>.*/\1/p' | head -1)"
    rm -f "$hdr"
    if [[ -n "$next" ]]; then url="https://${host}${next}"; else url=""; fi
  done
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

STARTS_AT="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
# An available update is a state, not a moment, but this script is the only
# thing that reports it and it runs daily. A 26-hour expiry means the alert
# stays firing while the update is still available and self-resolves about a
# day after the update is taken, without the script needing to send an explicit
# resolve. It comfortably outlives the info route's 5m group_wait, which an
# alert must survive to be delivered at all.
ENDS_AT="$(date -u -d '+26 hours' +%Y-%m-%dT%H:%M:%SZ)"

alerts="[]"
checked=0
failed=0

while read -r name image; do
  [[ -z "$name" ]] && continue
  checked=$((checked + 1))

  read -r registry repo tag <<<"$(parse_image_ref "$image")"

  excludes="$(docker inspect "$name" --format "{{index .Config.Labels \"${LABEL_EXCLUDE}\"}}" 2>/dev/null)"

  token="$(auth_token "$registry" "$repo")"
  if [[ -z "$token" ]]; then
    echo "WARN: no auth token for ${registry}/${repo}" >&2
    failed=$((failed + 1))
    continue
  fi

  tags="$(list_tags "$registry" "$repo" "$token")"
  if [[ -z "$tags" ]]; then
    echo "WARN: no tags listed for ${registry}/${repo}" >&2
    failed=$((failed + 1))
    continue
  fi

  filtered="$(printf '%s\n' "$tags" \
    | apply_exclusions "$BASELINE_EXCLUDE" \
    | apply_exclusions "$excludes")"
  newer="$(printf '%s\n' "$filtered" | newer_tags "$tag")"
  [[ -z "$newer" ]] && continue

  # Prefer tags shaped like the one actually installed, but only trust that
  # emptiness means "up to date" when the shape is a convention the repository
  # actually uses.
  #
  # Two very different situations produce no same-shaped NEWER tag:
  #
  #   caddy 2.11.4 -- the repository is full of #.#.# tags and none is newer,
  #   so 2.11.4 genuinely is the latest release. Reporting anything here is
  #   noise, and previously meant announcing 2.11.4-windowsservercore-ltsc2025
  #   as an "update" to the version already installed.
  #
  #   plex 1.43.3.10896-cb3ebc72d -- the embedded build hash means no other
  #   tag ever shares its shape, so emptiness proves nothing and staying quiet
  #   would hide every future release.
  #
  # Distinguishing them is the whole point: does the repository contain ANY
  # tag of this shape? If it does, the shape is a real convention and silence
  # is a genuine "nothing newer". If it does not, fall back to every newer tag,
  # so an unusual convention costs noise rather than silence.
  # The current tag is excluded from this check, and that exclusion is
  # load-bearing. The installed tag trivially matches its own shape, so
  # including it would make every convention look valid -- and plex, whose tag
  # embeds a build hash (1.43.3.10896-cb3ebc72d), would then be treated as a
  # convention with "nothing newer" forever, because a future release carries a
  # different hash and so a different shape. That is a silent miss, the exact
  # failure this design exists to avoid. Asking whether any OTHER tag shares
  # the shape is what separates a real convention from a one-off.
  shape_exists="$(printf '%s\n' "$filtered" | grep -vxF "$tag" | same_shape_as "$tag")"
  primary="$(printf '%s\n' "$newer" | same_shape_as "$tag")"

  if [[ -n "$shape_exists" ]]; then
    considered="$primary"
    shape_note=""
  else
    considered="$newer"
    shape_note=" (no tag in this repository shares the shape of ${tag}, so these are every newer tag rather than same-convention releases)"
  fi

  [[ -z "$considered" ]] && continue

  count="$(printf '%s\n' "$considered" | grep -c . )"
  newest="$(printf '%s\n' "$considered" | tail -1)"

  # Tab-separated, for tools/bump-patch. Emitted here rather than reimplemented
  # there, so the two can never disagree about what the newest tag is.
  if [[ "${IMAGEWATCH_REPORT:-}" == "1" ]]; then
    printf '%s\t%s\t%s\t%s\n' "$name" "${registry}/${repo}" "$tag" "$newest"
  fi
  sample="$(printf '%s\n' "$considered" | tail -5 | tr '\n' ' ' | sed 's/ $//')"

  e_image="$(printf '%s' "${registry}/${repo}:${tag}" | html_escape)"
  e_newest="$(printf '%s' "$newest" | html_escape)"
  e_tag="$(printf '%s' "$tag" | html_escape)"
  e_sample="$(printf '%s' "$sample" | html_escape)"

  alerts="$(printf '%s' "$alerts" | jq \
    --arg name "$name" \
    --arg image "$e_image" \
    --arg newest "$e_newest" \
    --arg sample "$e_sample" \
    --arg count "$count" \
    --arg tag "$e_tag" \
    --arg note "$shape_note" \
    --arg starts "$STARTS_AT" \
    --arg ends "$ENDS_AT" \
    '. + [{
        labels: {
          alertname: "ImageUpdateAvailable",
          severity: "info",
          instance: "xps",
          job: "imagewatch",
          container: $name,
          current_tag: $tag
        },
        annotations: {
          summary: ($name + ": " + $tag + " \u2192 " + $newest),
          description: ("Running " + $image + ". " + $count + " newer tag(s) available" + $note + "; most recent: " + $sample + ". Bump the pinned tag and digest in the Ansible role that owns this container.")
        },
        startsAt: $starts,
        endsAt: $ends
      }]')"
done < <(docker ps --format '{{.Names}}' | while read -r c; do
           enabled="$(docker inspect "$c" --format "{{index .Config.Labels \"${LABEL_ENABLE}\"}}" 2>/dev/null)"
           [[ "$enabled" == "true" ]] || continue
           printf '%s %s\n' "$c" "$(docker inspect "$c" --format '{{.Config.Image}}')"
         done)

n="$(printf '%s' "$alerts" | jq 'length')"
# To stderr, so that dry-run stdout is pure JSON and can be piped to jq.
echo "checked ${checked} image(s), ${failed} failed, ${n} with updates available" >&2

if [[ "${IMAGEWATCH_DRYRUN:-}" == "1" ]]; then
  printf '%s\n' "$alerts" | jq .
  exit 0
fi

if [[ "$n" -gt 0 ]]; then
  printf '%s' "$alerts" | curl -sf --max-time 30 \
    -H "Content-Type: application/json" \
    --data @- "$ALERTMANAGER_URL" \
    || { echo "ERROR: failed to post alerts to ${ALERTMANAGER_URL}" >&2; exit 1; }
fi

# A failure to reach a registry must not look like success. Nothing else would
# notice: node-exporter's systemd collector is not enabled, so a failed unit
# alerts nobody. The script therefore reports its own failure through the same
# path, rather than relying on a supervisor that is not watching.
# A pushed alert stays firing until its endsAt passes, so a run that fixes the
# problem has to say so explicitly. Without this an ImageWatchFailed raised by a
# single transient error would keep firing for a full day after the next run
# succeeded -- which is exactly what happened the first time it fired.
#
# Alertmanager treats an alert whose endsAt is in the past as resolved, and
# matches it to the existing one by its label set, so these labels must stay
# identical to the ones raised below.
# An ImageUpdateAvailable alert names the tag the container was running when it
# was raised. Once that container is upgraded the alert is answering a question
# nobody is asking, but a pushed alert stays firing until its endsAt passes --
# so an upgrade done today is still reported as outstanding for up to a day
# afterwards. It surfaces when an unrelated new alert joins the group and
# Alertmanager re-sends every member, dragging the answered ones along.
#
# Any active alert whose current_tag no longer matches what its container runs,
# or whose container has gone, is resolved by re-sending its exact label set
# with endsAt in the past. The labels must match to the letter or Alertmanager
# treats it as a different alert and raises it instead.
resolve_stale_update_alerts() {
  local running active
  running="$(docker ps --format '{{.Names}} {{.Config.Image}}' 2>/dev/null \
    || docker ps --format '{{.Names}}' | while read -r c; do
         printf '%s %s\n' "$c" "$(docker inspect "$c" --format '{{.Config.Image}}' 2>/dev/null)"
       done)"

  active="$(curl -sf --max-time 30 "${ALERTMANAGER_URL}?active=true" 2>/dev/null)" || return 0

  local stale
  stale="$(printf '%s' "$active" | jq -c --arg running "$running" '
    ($running | split("\n") | map(select(length > 0) | split(" ")
      | {key: .[0], value: (.[1] // "" | split("@")[0] | split("/") | last
        | if test(":") then split(":") | last else "latest" end)}) | from_entries) as $now
    | [ .[]
        | select(.labels.alertname == "ImageUpdateAvailable")
        | select(.status.state != "suppressed")
        | select($now[.labels.container] == null or $now[.labels.container] != .labels.current_tag)
        | {labels: .labels, annotations: .annotations} ]')" || return 0

  [[ "$(printf '%s' "$stale" | jq 'length')" -gt 0 ]] || return 0

  printf '%s' "$stale" | jq \
    --arg starts "$(date -u -d '-1 hour' +%Y-%m-%dT%H:%M:%SZ)" \
    --arg ends "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    'map(. + {startsAt: $starts, endsAt: $ends})' \
    | curl -sf "${RETRY[@]}" --max-time 30 \
        -H "Content-Type: application/json" \
        --data @- "$ALERTMANAGER_URL" >/dev/null || true

  echo "resolved $(printf '%s' "$stale" | jq 'length') answered update alert(s)" >&2
}

resolve_failure_alert() {
  # startsAt is an hour back, not this run's start: Alertmanager rejects an
  # alert whose endsAt precedes its startsAt, and a fast run would produce
  # exactly that. This worked only because a full check takes over a minute.
  jq -n \
    --arg starts "$(date -u -d '-1 hour' +%Y-%m-%dT%H:%M:%SZ)" \
    --arg ends "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    '[{
        labels: {
          alertname: "ImageWatchFailed",
          severity: "warning",
          instance: "xps",
          job: "imagewatch"
        },
        startsAt: $starts,
        endsAt: $ends
      }]' \
    | curl -sf "${RETRY[@]}" --max-time 30 \
        -H "Content-Type: application/json" \
        --data @- "$ALERTMANAGER_URL" >/dev/null || true
}

if [[ "$failed" -eq 0 ]]; then
  resolve_failure_alert
  # Only when every image was checked. A run that could not reach a registry
  # does not know whether an alert it cannot see is still true.
  resolve_stale_update_alerts
fi

if [[ "$failed" -gt 0 ]]; then
  fail_alert="$(jq -n \
    --arg failed "$failed" \
    --arg checked "$checked" \
    --arg starts "$STARTS_AT" \
    --arg ends "$ENDS_AT" \
    '[{
        labels: {
          alertname: "ImageWatchFailed",
          severity: "warning",
          instance: "xps",
          job: "imagewatch"
        },
        annotations: {
          summary: ($failed + " of " + $checked + " image checks failed"),
          description: ("image-update-check.sh could not reach the registry for " + $failed + " image(s), so those images are NOT being checked for updates. Run `journalctl -u image-update-check` for the failing repositories. A common cause is Docker Hub rate limiting (100 requests per hour per IP).")
        },
        startsAt: $starts,
        endsAt: $ends
      }]')"
  printf '%s' "$fail_alert" | curl -sf --max-time 30 \
    -H "Content-Type: application/json" \
    --data @- "$ALERTMANAGER_URL" >/dev/null || true
  exit 1
fi
exit 0
