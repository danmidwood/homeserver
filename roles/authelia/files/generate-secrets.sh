#!/bin/bash
#
# Generates every secret Authelia needs, creating only what is missing.
#
# Nothing here is ever overwritten. storage_encryption_key in particular must
# survive: replacing it leaves the registered passkeys in the database
# undecryptable. Nothing is derived from anything else either, so a lost file
# cannot be reconstructed, which is why the secrets directory is backed up.
#
# Creating only what is missing is also what lets a client be added to
# authelia_clients later: its secret is minted on the next run while every
# existing one is left alone.
#
# Usage: generate-secrets.sh <secrets-dir> <authelia-image> <client-id>...
set -euo pipefail

SECRETS_DIR="$1"
IMAGE="$2"
shift 2

umask 077
mkdir -p "${SECRETS_DIR}/clients"

# Authelia's own crypto tooling ships inside the image, so the hashes are
# produced by the code that verifies them.
authelia_rand() {
  docker run --rm "${IMAGE}" authelia crypto rand --length 72 --charset alphanumeric \
    | sed 's/^Random Value: //'
}

authelia_hash() {
  docker run --rm "${IMAGE}" authelia crypto hash generate pbkdf2 --password "$1" \
    | sed 's/^Digest: //'
}

for name in session_secret storage_encryption_key jwt_secret oidc_hmac_secret; do
  [ -s "${SECRETS_DIR}/${name}" ] && continue
  authelia_rand > "${SECRETS_DIR}/${name}"
done

# The key that signs ID tokens. Applications fetch the public half from
# Authelia's JWKS endpoint.
[ -s "${SECRETS_DIR}/oidc.key" ] || openssl genrsa -out "${SECRETS_DIR}/oidc.key" 4096 2>/dev/null

# Two forms of every client secret: the plaintext an application is configured
# with, and the digest Authelia stores.
for client in "$@"; do
  secret_file="${SECRETS_DIR}/clients/${client}.secret"
  hash_file="${SECRETS_DIR}/clients/${client}.hash"
  [ -s "$secret_file" ] && [ -s "$hash_file" ] && continue

  secret="$(authelia_rand)"
  printf '%s' "${secret}" > "$secret_file"
  authelia_hash "${secret}" > "$hash_file"
done

chmod 700 "${SECRETS_DIR}" "${SECRETS_DIR}/clients"
chmod 600 "${SECRETS_DIR}"/* "${SECRETS_DIR}"/clients/* 2>/dev/null || true
