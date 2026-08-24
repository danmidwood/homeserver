#!/usr/bin/env bash
# Unit-tests the pure logic in image-update-check.sh: image reference parsing
# and version comparison. Runs entirely offline against fixtures, so it is fast
# and deterministic, and it covers the two places a bug would be silent -- a
# misparsed reference queries the wrong repository, and a bad comparison hides
# a release that exists.
set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
IMAGEWATCH_LIB_ONLY=1 source "$REPO_ROOT/roles/imagewatch/files/image-update-check.sh"

pass=0; fail=0

check() {
  local desc="$1" expected="$2" actual="$3"
  if [[ "$expected" == "$actual" ]]; then
    pass=$((pass + 1))
  else
    fail=$((fail + 1))
    echo "  FAIL: $desc"
    echo "        expected: $expected"
    echo "        actual:   $actual"
  fi
}

echo "==> parse_image_ref"

# Every image reference actually in use in this repo, plus the shapes that
# would break a naive parser.
check "hub official, tag+digest" \
  "docker.io library/caddy 2.11.4" \
  "$(parse_image_ref 'caddy:2.11.4@sha256:844f60b64e4724a5aa8245e019dace0d3f199f7433ce6c57676cb30a920dbad9')"

check "hub official, explicit docker.io" \
  "docker.io library/redis 6.2-alpine" \
  "$(parse_image_ref 'docker.io/redis:6.2-alpine@sha256:eaba718fecd1196d88533de7ba49bf903ad33664a92debb24660a922ecd9cac8')"

check "hub namespaced (not a registry)" \
  "docker.io linuxserver/kavita v0.9.0.2-ls120" \
  "$(parse_image_ref 'linuxserver/kavita:v0.9.0.2-ls120@sha256:abc')"

check "ghcr, nested repo" \
  "ghcr.io immich-app/immich-server v3.1.0" \
  "$(parse_image_ref 'ghcr.io/immich-app/immich-server:v3.1.0@sha256:abc')"

check "gcr" \
  "gcr.io cadvisor/cadvisor v0.55.1" \
  "$(parse_image_ref 'gcr.io/cadvisor/cadvisor:v0.55.1@sha256:abc')"

check "tag containing dots and dashes" \
  "docker.io plexinc/pms-docker 1.43.3.10896-cb3ebc72d" \
  "$(parse_image_ref 'plexinc/pms-docker:1.43.3.10896-cb3ebc72d@sha256:abc')"

check "no tag defaults to latest" \
  "docker.io library/postgres latest" \
  "$(parse_image_ref 'postgres')"

check "registry with a port is not mistaken for a tag" \
  "registry.local:5000 team/app 1.2.3" \
  "$(parse_image_ref 'registry.local:5000/team/app:1.2.3')"

echo "==> newer_tags"

check "finds a newer patch release" \
  "2.11.5
2.11.6" \
  "$(printf '2.11.3\n2.11.4\n2.11.5\n2.11.6\n' | newer_tags 2.11.4)"

check "already newest yields nothing" \
  "" \
  "$(printf '2.11.3\n2.11.4\n' | newer_tags 2.11.4)"

# The bug a lexical sort would introduce: 2.11.10 is NEWER than 2.11.9, but
# sorts BELOW it as a string, so a lexical comparison would hide the release.
check "double-digit patch sorts above single digit" \
  "2.11.10" \
  "$(printf '2.11.8\n2.11.9\n2.11.10\n' | newer_tags 2.11.9)"

check "minor version bump ordered correctly" \
  "2.9.0
2.10.0" \
  "$(printf '2.8.0\n2.9.0\n2.10.0\n' | newer_tags 2.8.0)"

check "linuxserver -ls suffix convention" \
  "v0.9.0.2-ls121
v0.9.0.3-ls122" \
  "$(printf 'v0.9.0.2-ls120\nv0.9.0.2-ls121\nv0.9.0.3-ls122\n' | newer_tags v0.9.0.2-ls120)"

check "current tag missing upstream still compares" \
  "2.11.5" \
  "$(printf '2.11.3\n2.11.5\n' | newer_tags 2.11.4)"

echo "==> apply_exclusions"

check "drops sha- commit builds, keeps releases" \
  "2.11.4
2.11.5" \
  "$(printf 'sha-abc1234\n2.11.4\nsha-def5678-alpine\n2.11.5\n' | apply_exclusions '^sha-')"

check "multiple semicolon-separated patterns" \
  "2.11.4" \
  "$(printf 'sha-abc1234\nmain\n2.11.4\ndeadbeef1234567\n' | apply_exclusions '^sha-;^[0-9a-f]{7,40}$;^(main|master|edge|nightly|dev|develop|canary)([.-]|$)')"

check "empty pattern list is a passthrough" \
  "1.0
2.0" \
  "$(printf '1.0\n2.0\n' | apply_exclusions '')"

# The exclusions must not eat any real tag convention in use here.
check "real conventions survive the production exclusions" \
  "1.43.3.10896-cb3ebc72d
16-alpine
2.11.4
v0.9.0.2-ls120
v3.1.0" \
  "$(printf '2.11.4\nv3.1.0\n16-alpine\nv0.9.0.2-ls120\n1.43.3.10896-cb3ebc72d\nsha-abc1234\nmain\n' \
     | apply_exclusions '^sha-;^[0-9a-f]{7,40}$;^(main|master|edge|nightly|dev|develop|canary)([.-]|$)' | sort)"

# The baseline rule that keeps the reported "newest" tag meaningful. sort -V
# ranks alphabetic tags above numeric ones, so without this the newest caddy
# tag is windowsservercore-ltsc2025 and the newest plex tag is "public".
check "baseline drops non-version tags, keeps every real convention" \
  "1.43.3.10896-cb3ebc72d
16-alpine
2.11.4
26.8.1
v0.9.0.2-ls120
v3.1.0" \
  "$(printf '2.11.4\nv3.1.0\n16-alpine\nv0.9.0.2-ls120\n1.43.3.10896-cb3ebc72d\n26.8.1\nlatest\nbeta\npublic\nplexpass\nalpine\nbookworm\nwindowsservercore-ltsc2025\nedge\n' \
     | apply_exclusions '^[^0-9v];^v[^0-9]' | sort)"

# An architecture suffix is never an upgrade of the version it hangs off, and
# plex's build-hash tags meant the fallback kept reporting -armhf as available.
check "baseline drops architecture-suffixed tags, keeps real versions" \
  "1.43.3.10896-cb3ebc72d
16-alpine
2.11.4
v3.1.0" \
  "$(printf '2.11.4\nv3.1.0\n16-alpine\n1.43.3.10896-cb3ebc72d\n1.43.3.10896-cb3ebc72d-armhf\n1.43.3.10896-cb3ebc72d-amd64\n2.11.4-arm64\nv3.1.0-ppc64le\n' \
     | apply_exclusions '^[^0-9v];^v[^0-9];-(amd64|arm64|armhf|armv7|i386|ppc64le|s390x)$' | sort)"

echo "==> tag_shape / same_shape_as"

check "shape of a plain version" "#.#.#" "$(tag_shape 2.11.4)"
check "shape keeps variant words" "#.#.#-windowsservercore-ltsc#" "$(tag_shape 2.11.4-windowsservercore-ltsc2025)"
check "shape of a base-image variant" "#-alpine" "$(tag_shape 16-alpine)"
check "shape of the linuxserver convention" "v#.#.#.#-ls#" "$(tag_shape v0.9.0.2-ls120)"

# The bug this exists to prevent: running caddy 2.11.4, the newest tag by
# version sort is 2.11.4-windowsservercore-ltsc2025 -- a Windows build of the
# version already installed, not an upgrade.
check "same shape keeps real upgrades, drops platform variants" \
  "2.11.5
2.12.0" \
  "$(printf '2.11.5\n2.11.4-windowsservercore-ltsc2025\n2.12.0\n2.11.4-alpine\n' | same_shape_as 2.11.4)"

check "same shape respects a base-image variant" \
  "17-alpine
18-alpine" \
  "$(printf '17-alpine\n17-bookworm\n18-alpine\n18beta1-trixie\n' | same_shape_as 16-alpine)"

check "unmatched shape yields nothing, so the caller can fall back" \
  "" \
  "$(printf '1.44.0.12345-abcdef123\n' | same_shape_as 1.43.3.10896-cb3ebc72d)"

echo "==> html_escape"

check "escapes ampersand first" \
  "a&amp;b&lt;c&gt;d" \
  "$(printf 'a&b<c>d' | html_escape)"

echo
if [[ "$fail" -gt 0 ]]; then
  echo "FAILED: $fail failed, $pass passed"
  exit 1
fi
echo "All $pass image-update checks passed"
