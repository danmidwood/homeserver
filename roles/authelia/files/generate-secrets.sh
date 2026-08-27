#!/bin/bash
#
# Generates every secret Authelia needs, once. Guarded by `creates:` in the
# role, so a second run never happens -- which matters more here than it looks:
# regenerating storage_encryption_key leaves the registered passkeys and TOTP
# devices in the database undecryptable, forcing every device to be enrolled
# again. Nothing here is derived from anything else, so a lost file cannot be
# reconstructed; the secrets directory is backed up for exactly that reason.
#
# Usage: generate-secrets.sh <secrets-dir> <authelia-image> <client-id>...
set -euo pipefail

SECRETS_DIR="$1"
IMAGE="$2"
shift 2

umask 077
mkdir -p "${SECRETS_DIR}/clients"

# Authelia's own crypto tooling ships inside the image, so the hashes it is
# asked to verify are produced by exactly the code that verifies them. `--rm`
# and no mounts: these invocations only ever write to stdout.
authelia_rand() {
  docker run --rm "${IMAGE}" authelia crypto rand --length 72 --charset alphanumeric \
    | sed 's/^Random Value: //'
}

authelia_hash() {
  docker run --rm "${IMAGE}" authelia crypto hash generate pbkdf2 --password "$1" \
    | sed 's/^Digest: //'
}

for name in session_secret storage_encryption_key jwt_secret oidc_hmac_secret; do
  authelia_rand > "${SECRETS_DIR}/${name}"
done

# The key that signs ID tokens. Applications fetch the public half from
# Authelia's JWKS endpoint, so only the private half is stored.
openssl genrsa -out "${SECRETS_DIR}/oidc.key" 4096 2>/dev/null

# Two forms of every client secret: the plaintext an application is configured
# with, and the digest Authelia stores. Authelia would accept the plaintext in
# its own configuration, but there is no reason to keep a recoverable copy in
# two files when the image can hash it.
for client in "$@"; do
  secret="$(authelia_rand)"
  printf '%s' "${secret}" > "${SECRETS_DIR}/clients/${client}.secret"
  authelia_hash "${secret}" > "${SECRETS_DIR}/clients/${client}.hash"
done

chmod 700 "${SECRETS_DIR}" "${SECRETS_DIR}/clients"
chmod 600 "${SECRETS_DIR}"/* "${SECRETS_DIR}"/clients/* 2>/dev/null || true
