#!/usr/bin/env bash
# Starts Omni (embedded etcd, auth0-dummy auth, initialServiceAccount bootstrap)
# as root via sudo and waits for readiness.
#
# Bootstrap chain (source-verified against omni v1.13.3, see
# .superpowers/sdd/2026-11-11-devstack-e2e/progress.md):
#   - auth0 dummy config: no IdP is contacted at startup (URL parse only),
#     but validateParams requires an enabled auth provider
#   - --create-initial-service-account: omni creates an Admin service account
#     on first start and writes its key to --initial-service-account-key-path
#   - sudo is required because omni creates a TUN device for the siderolink
#     wireguard interface at startup (no way to disable this)
#
# Generates the etcd PGP keypair (master key slot) and a self-signed API cert
# once per workdir; omni's embedded dex OIDC issuer requires https.
#
# Usage: start-omni.sh --workdir /tmp/omni --key-out /tmp/omni/bootstrap-sa.key
set -euo pipefail

WORKDIR=""
KEY_OUT=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --workdir) WORKDIR="${2:?}" ; shift 2 ;;
    --key-out) KEY_OUT="${2:?}" ; shift 2 ;;
    *)
      echo "unknown argument: $1" >&2
      echo "usage: start-omni.sh --workdir <dir> --key-out <file>" >&2
      exit 1
      ;;
  esac
done

if [[ -z "${WORKDIR}" || -z "${KEY_OUT}" ]]; then
  echo "usage: start-omni.sh --workdir <dir> --key-out <file>" >&2
  exit 1
fi

# omni resolves ./omnictl and its data paths relative to the working directory
cd "${WORKDIR}"

# --- one-time key material ---------------------------------------------------
# etcd master key slot: PGP keypair (armored). omni needs the private key to
# decrypt the master key slot and public key files to encrypt new slots.
if [[ ! -s etcd-private.key || ! -s etcd-public.key ]]; then
  export GNUPGHOME="${WORKDIR}/gnupg"
  mkdir -p "${GNUPGHOME}"
  chmod 700 "${GNUPGHOME}"
  gpg --batch --gen-key <<'EOF'
Key-Type: RSA
Key-Length: 3072
Name-Real: omni-e2e
Name-Email: omni-e2e@localhost
%no-protection
%commit
EOF
  gpg --armor --export omni-e2e@localhost > etcd-public.key
  gpg --armor --export-secret-keys omni-e2e@localhost > etcd-private.key
  chmod 600 etcd-private.key etcd-public.key
fi

# API TLS: self-signed cert is enough; the issuer endpoint is derived from
# --advertised-api-url + /oidc and must be https.
if [[ ! -s api-cert.pem || ! -s api-key.pem ]]; then
  openssl req -x509 -newkey rsa:2048 -keyout api-key.pem -out api-cert.pem \
    -days 1 -nodes -subj "/CN=localhost" \
    -addext "subjectAltName=DNS:localhost,IP:127.0.0.1" 2>/dev/null
fi

# --- start omni as root (TUN/CAP_NET_ADMIN) -----------------------------------
# The log redirect happens in the runner shell, so omni.log stays runner-owned.
sudo -n "${WORKDIR}/bin/omni" \
  --auth-auth0-enabled=true \
  --auth-auth0-domain=dummy.invalid \
  --auth-auth0-client-id=dummy \
  --create-initial-service-account \
  --initial-service-account-key-path="${KEY_OUT}" \
  --initial-service-account-lifetime=24h \
  --etcd-embedded-unsafe-fsync=true \
  --private-key-source="file://${WORKDIR}/etcd-private.key" \
  --public-key-files="${WORKDIR}/etcd-public.key" \
  --sqlite-storage-path="${WORKDIR}/sqlite.db" \
  --eula-accept-name="E2E CI" \
  --eula-accept-email="e2e@localhost" \
  --advertised-api-url="https://localhost:8080" \
  --cert="${WORKDIR}/api-cert.pem" \
  --key="${WORKDIR}/api-key.pem" \
  > "${WORKDIR}/omni.log" 2>&1 &

# --- readiness: healthz + bootstrap SA key file (written by root omni) -------
for _ in $(seq 1 36); do
  if curl -skf https://localhost:8080/healthz >/dev/null 2>&1 \
     && sudo -n test -s "${KEY_OUT}"; then
    echo "omni ready (healthz OK, bootstrap key present)"
    exit 0
  fi
  sleep 5
done

echo "omni did not become ready in time; last log lines:" >&2
tail -n 30 "${WORKDIR}/omni.log" >&2 || true
exit 1
