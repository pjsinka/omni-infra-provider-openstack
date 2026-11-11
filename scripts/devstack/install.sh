#!/usr/bin/env bash
# Installs a minimal DevStack (Keystone, Glance, Nova, Neutron/OVN, Cinder,
# Placement) for omni e2e and creates an application credential entry in
# $HOME/<cloud-name>-clouds.yaml.
#
# Secrets policy: the admin password and the application credential secret
# are generated per run, written to files (0600) only and never echoed.
# When running on GitHub Actions, OS_CLOUD and OS_CLIENT_CONFIG_FILE are
# exported to subsequent steps via $GITHUB_ENV.
#
# Usage: install.sh --cloud-name devstack-e2e --workdir /tmp/devstack [--branch stable/2026.2]
set -euo pipefail

# Pin: latest stable OpenStack release branch at the time of writing (2026-11).
# Bump deliberately after reviewing release notes of newer releases.
DEFAULT_DEVSTACK_BRANCH="stable/2026.2"

CLOUD_NAME=""
WORKDIR=""
BRANCH="${DEVSTACK_BRANCH:-}"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --cloud-name) CLOUD_NAME="${2:?}" ; shift 2 ;;
    --workdir) WORKDIR="${2:?}" ; shift 2 ;;
    --branch) BRANCH="${2:?}" ; shift 2 ;;
    *)
      echo "unknown argument: $1" >&2
      echo "usage: install.sh --cloud-name <name> --workdir <dir> [--branch <stable/X.Y>]" >&2
      exit 1
      ;;
  esac
done

if [[ -z "$CLOUD_NAME" || -z "$WORKDIR" ]]; then
  echo "usage: install.sh --cloud-name <name> --workdir <dir> [--branch <stable/X.Y>]" >&2
  exit 1
fi
if [[ -z "$BRANCH" ]]; then
  BRANCH="$DEFAULT_DEVSTACK_BRANCH"
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# --- clone devstack (pinned stable branch, shallow) ----------------------------
if [[ ! -d "$WORKDIR/devstack/.git" ]]; then
  git clone --depth 1 -b "$BRANCH" https://opendev.org/openstack/devstack.git "$WORKDIR/devstack"
fi

# --- local.conf: section header + per-run secrets + static template body ------
# Random per-run admin password — nothing secret is committed; the cloud is
# throwaway and only reachable from the runner itself. All service passwords
# share the admin value. They are written as CONCRETE values, not
# $ADMIN_PASSWORD indirection: the [[local|localrc]] section extractor drops
# lines outside the section and localrc is sourced in file order — concrete
# values have no ordering or section-boundary dependency at all.
ADMIN_PASSWORD="$(openssl rand -hex 16)"

# Nested virtualization on GitHub-hosted runners is not guaranteed (mixed
# Intel/AMD fleet). TCG fallback: slow guest boot, but the e2e only waits
# for Nova ACTIVE (instance spawn), never for a fully booted guest.
if [[ -e /dev/kvm ]]; then
  LIBVIRT_TYPE=kvm
else
  LIBVIRT_TYPE=qemu
fi
echo "devstack: branch=$BRANCH libvirt-type=$LIBVIRT_TYPE"

# Mask the generated password before anything else can print it.
if [[ -n "${GITHUB_ACTIONS:-}" ]]; then
  echo "::add-mask::${ADMIN_PASSWORD}"
fi

{
  printf '[[local|localrc]]\n'
  printf '# --- generated per-run by install.sh ---\n'
  printf 'ADMIN_PASSWORD=%s\n' "$ADMIN_PASSWORD"
  printf 'DATABASE_PASSWORD=%s\n' "$ADMIN_PASSWORD"
  printf 'RABBIT_PASSWORD=%s\n' "$ADMIN_PASSWORD"
  printf 'SERVICE_PASSWORD=%s\n' "$ADMIN_PASSWORD"
  printf 'LIBVIRT_TYPE=%s\n\n' "$LIBVIRT_TYPE"
  cat "$SCRIPT_DIR/local.conf"
} > "$WORKDIR/devstack/local.conf"
chmod 600 "$WORKDIR/devstack/local.conf"

# --- strip postgres server-dev from devstack's package lists (we run MySQL) ----
# files/debs/neutron-common requests postgresql-server-dev-all, which drags
# the versioned postgresql-18 chain (client-18, server-dev-18) into the
# apt transaction. On the GitHub runner image that chain deadlocks apt's
# resolver (Runs 5+7: postgresql-client-18 18.6 "not selected for install"
# while 18.3-1 is the second candidate). Devstack with MySQL never needs
# the postgres server packages — psycopg2-style clients use libpq-dev,
# which stays in files/debs/general.
sed -i '/postgresql-server-dev-all/d' "$WORKDIR/devstack/files/debs/neutron-common"

# --- apt: refresh indexes + resolve runner-image package drift ------------------
# The GitHub runner image ships packages from image-build time. When the
# Ubuntu archive has moved on, apt transactions hit exact-version
# dependencies of preinstalled older versions (Run 5: apache2-bin — fixed
# by dist-upgrade; Runs 5+7: the postgres-18 chain — fixed by the strip
# above). Diagnostics below end up in the CI log (holds, sources,
# preinstalled postgres) and unhold clears image-time package holds.
echo "=== apt holds ==="
sudo apt-mark showhold || true
echo "=== apt sources.d ==="
ls -l /etc/apt/sources.list.d/ || true
echo "=== installed postgres packages ==="
dpkg -l | grep -E '^ii +postgresql' || true
sudo apt-mark unhold $(sudo apt-mark showhold) 2>/dev/null || true
# MySQL: the runner image ships a pre-initialized MySQL with an
# image-time root password unknown to us. DevStack's flow relies on a
# FRESH install: it debconf-preseeds mysql-server/root_password with
# $DATABASE_PASSWORD (lib/databases/mysql:206-208) and later logs in with
# it — with the preinstalled server that login fails (Run 8: 'Access
# denied for root@localhost'). Stop, purge and wipe the datadir;
# stack.sh's install_package then does a fresh install with the preseed.
echo "=== installed mysql packages ==="
dpkg -l | grep -E '^ii +mysql' || true
sudo systemctl stop mysql 2>/dev/null || true
sudo apt-get -y purge 'mysql-server*' 2>/dev/null || true
sudo apt-get -y purge 'mysql-client*' 2>/dev/null || true
sudo apt-get -y purge 'mysql-common' 2>/dev/null || true
sudo rm -rf /var/lib/mysql /etc/mysql
# Firefox: its transitional deb installs a SNAP from the store, which is
# unreachable on runners (Run 6 hung 30 min in the retry loop).
sudo apt-get -y purge firefox
sudo apt-get update
sudo DEBIAN_FRONTEND=noninteractive apt-get -y dist-upgrade

# --- run stack.sh ----------------------------------------------------------------
# stack.sh must run as a non-root user with sudo (the GitHub runner user is
# exactly that). Full output goes to stack.log; only the tail is printed on
# failure — the log contains the admin password (masked above) and is never
# dumped in full.
cd "$WORKDIR/devstack"
if ! ./stack.sh > "$WORKDIR/stack.log" 2>&1; then
  echo "stack.sh FAILED, last 100 log lines:" >&2
  tail -n 100 "$WORKDIR/stack.log" >&2
  exit 1
fi

# --- application credential + clouds.yaml -----------------------------------------
# Direct password auth against loopback keystone. auth_url matches the
# catalog entry; gophercloud auto-discovers the versioned endpoint.
export OS_AUTH_URL="http://127.0.0.1/identity"
export OS_USERNAME=admin
export OS_PASSWORD="$ADMIN_PASSWORD"
export OS_PROJECT_NAME=admin
export OS_USER_DOMAIN_NAME=Default
export OS_PROJECT_DOMAIN_NAME=Default
export OS_IDENTITY_API_VERSION=3
export OS_INTERFACE=public
export OS_REGION_NAME=RegionOne

# Prefer devstack's own CLI (proven against this exact cloud — stack.sh
# just drove the whole installation with it); fall back to PATH.
OPENSTACK_BIN=""
for cand in /opt/stack/data/venv/bin/openstack /opt/stack/.venv/bin/openstack /opt/stack/venv/bin/openstack; do
  if [[ -x "$cand" ]]; then OPENSTACK_BIN="$cand"; break; fi
done
[[ -z "$OPENSTACK_BIN" ]] && OPENSTACK_BIN="$(command -v openstack || true)"
echo "openstack binary: ${OPENSTACK_BIN:-MISSING}"
if [[ -n "$OPENSTACK_BIN" ]]; then
  "$OPENSTACK_BIN" --version 2>&1 | head -1 || true
fi

# NOTE: the app credential secret appears ONLY in the create output.
# Diagnostics print exit codes, byte counts and JSON KEYS only — never
# values, never the secret itself.
STDERR_FILE="$(mktemp)"

# Auth smoke test with the resolved binary: separates "auth/cloud broken"
# (token issue fails) from "create broken" (token OK, create fails) in the
# CI log.
set +e
"$OPENSTACK_BIN" token issue -f value -c id > /dev/null 2>"$STDERR_FILE"
TOKEN_EXIT=$?
TOKEN_STDERR="$(cat "$STDERR_FILE")"
set -e
echo "token smoke: exit=$TOKEN_EXIT stderr-bytes=${#TOKEN_STDERR}"
if [[ "$TOKEN_EXIT" -ne 0 ]]; then
  if [[ -n "$TOKEN_STDERR" ]]; then printf 'token stderr: %s\n' "$TOKEN_STDERR"; fi
  echo "auth smoke test failed — keystone/auth problem, not app-cred" >&2
  exit 1
fi

set +e
APP_CRED_JSON="$("$OPENSTACK_BIN" application credential create "$CLOUD_NAME" -f json 2>"$STDERR_FILE")"
OS_EXIT=$?
set -e
OS_STDERR="$(cat "$STDERR_FILE")"
rm -f "$STDERR_FILE"
echo "app-cred create: exit=$OS_EXIT stdout-bytes=${#APP_CRED_JSON} stderr-bytes=${#OS_STDERR}"
if [[ -n "$OS_STDERR" ]]; then printf 'app-cred stderr: %s\n' "$OS_STDERR"; fi
echo "app-cred json keys: $(jq -r 'keys | join(",")' <<<"$APP_CRED_JSON" 2>&1 | head -1)"
APP_CRED_ID="$(jq -r .id <<<"$APP_CRED_JSON" 2>/dev/null || true)"
APP_CRED_SECRET="$(jq -r .secret <<<"$APP_CRED_JSON" 2>/dev/null || true)"
echo "app-cred id-bytes=${#APP_CRED_ID} secret-bytes=${#APP_CRED_SECRET}"
if [[ -z "$APP_CRED_ID" || -z "$APP_CRED_SECRET" || "$APP_CRED_ID" == "null" || "$APP_CRED_SECRET" == "null" ]]; then
  echo "application credential creation failed" >&2
  exit 1
fi

# Mask the app credential secret before anything else can print it.
if [[ -n "${GITHUB_ACTIONS:-}" ]]; then
  echo "::add-mask::${APP_CRED_SECRET}"
fi

CLOUDS_FILE="$HOME/${CLOUD_NAME}-clouds.yaml"
umask 177
cat > "$CLOUDS_FILE" <<EOF
clouds:
  $CLOUD_NAME:
    auth:
      auth_url: http://127.0.0.1/identity
      application_credential_id: $APP_CRED_ID
      application_credential_secret: $APP_CRED_SECRET
    region_name: RegionOne
    interface: public
    identity_api_version: "3"
EOF
chmod 600 "$CLOUDS_FILE"

# --- verify the cloud entry + default flavors -------------------------------------
unset OS_AUTH_URL OS_USERNAME OS_PASSWORD OS_PROJECT_NAME OS_USER_DOMAIN_NAME \
      OS_PROJECT_DOMAIN_NAME OS_IDENTITY_API_VERSION OS_INTERFACE OS_REGION_NAME
export OS_CLOUD="$CLOUD_NAME"
export OS_CLIENT_CONFIG_FILE="$CLOUDS_FILE"

if ! "$OPENSTACK_BIN" token issue -f value -c id > /dev/null 2>/dev/null; then
  echo "verify failed: token issue via app credential (cloud '$CLOUD_NAME')" >&2
  exit 1
fi
if ! "$OPENSTACK_BIN" flavor list -f value -c Name 2>/dev/null | grep -qx m1.small; then
  echo "verify failed: m1.small missing from flavor list (cloud '$CLOUD_NAME')" >&2
  exit 1
fi
echo "devstack ready: cloud '$CLOUD_NAME' authenticates, m1.small present, clouds file: $CLOUDS_FILE"

# --- export cloud env for subsequent workflow steps -------------------------------
if [[ -n "${GITHUB_ENV:-}" ]]; then
  {
    echo "OS_CLOUD=$CLOUD_NAME"
    echo "OS_CLIENT_CONFIG_FILE=$CLOUDS_FILE"
  } >> "$GITHUB_ENV"
fi
