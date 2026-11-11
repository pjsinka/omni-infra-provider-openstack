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

# --- apt: refresh indexes + resolve runner-image package drift ------------------
# The GitHub runner image ships packages from image-build time. When the
# Ubuntu archive has moved on (security/point updates), stack.sh's
# "apt-get install" transaction fails on exact-version dependencies
# (observed: apache2-bin preinstalled one point-release behind the archive
# apache2 that depends on it; postgresql-client-18 blocked likewise).
# dist-upgrade brings every preinstalled package to current archive
# versions first; safe on a throwaway runner, ~2 min.
# Firefox must go first: on Ubuntu the firefox deb is a transitional stub
# whose postinst installs the Firefox SNAP from the snap store; the store
# is unreachable on GitHub runners and the postinst then retries every
# minute for 30 minutes. DevStack needs no browser.
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

APP_CRED_JSON="$(openstack application credential create "$CLOUD_NAME" -f json)"
APP_CRED_ID="$(jq -r .id <<<"$APP_CRED_JSON")"
APP_CRED_SECRET="$(jq -r .secret <<<"$APP_CRED_JSON")"
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

openstack token issue -f value -c id > /dev/null
openstack flavor list -f value -c Name | grep -qx m1.small
echo "devstack ready: cloud '$CLOUD_NAME' authenticates, m1.small present, clouds file: $CLOUDS_FILE"

# --- export cloud env for subsequent workflow steps -------------------------------
if [[ -n "${GITHUB_ENV:-}" ]]; then
  {
    echo "OS_CLOUD=$CLOUD_NAME"
    echo "OS_CLIENT_CONFIG_FILE=$CLOUDS_FILE"
  } >> "$GITHUB_ENV"
fi
