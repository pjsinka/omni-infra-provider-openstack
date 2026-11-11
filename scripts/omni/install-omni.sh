#!/usr/bin/env bash
# Downloads omni + omnictl release binaries (linux amd64) into <workdir>/bin,
# verifies checksums, and prepares the ./omnictl download dir required by omni.
# Usage: install-omni.sh --workdir /tmp/omni [--version vX.Y.Z]
# OMNI_VERSION env var overrides the pinned default.
set -euo pipefail

# Pin: latest stable omni release at the time of writing (2026-11-12).
# Bump deliberately after reviewing release notes of newer versions.
DEFAULT_OMNI_VERSION="v1.13.3"

WORKDIR=""
VERSION="${OMNI_VERSION:-}"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --workdir) WORKDIR="${2:?}" ; shift 2 ;;
    --version) VERSION="${2:?}" ; shift 2 ;;
    *)
      echo "unknown argument: $1" >&2
      echo "usage: install-omni.sh --workdir <dir> [--version vX.Y.Z]" >&2
      exit 1
      ;;
  esac
done

if [[ -z "${WORKDIR}" ]]; then
  echo "usage: install-omni.sh --workdir <dir> [--version vX.Y.Z]" >&2
  exit 1
fi
if [[ -z "${VERSION}" ]]; then
  VERSION="${DEFAULT_OMNI_VERSION}"
fi

BASE_URL="https://github.com/siderolabs/omni/releases/download/${VERSION}"

mkdir -p "${WORKDIR}/bin" "${WORKDIR}/omnictl"

# Download + verify into a staging dir so the final names are stable
# (bin/omni, bin/omnictl) while the checksum manifest still matches the
# original asset names.
STAGE="$(mktemp -d)"
trap 'rm -rf "${STAGE}"' EXIT

curl -fsSL -o "${STAGE}/sha256sum.txt" "${BASE_URL}/sha256sum.txt"
for asset in omni-linux-amd64 omnictl-linux-amd64; do
  curl -fsSL -o "${STAGE}/${asset}" "${BASE_URL}/${asset}"
done

# Verify both entries; fail if the manifest does not contain exactly the two
# expected lines (guards against silent no-op verification).
CHECKS="$(grep -E '(omni|omnictl)-linux-amd64' "${STAGE}/sha256sum.txt" || true)"
if [[ "$(printf '%s\n' "${CHECKS}" | grep -c .)" -ne 2 ]]; then
  echo "checksum manifest missing entries for omni/omnictl linux-amd64:" >&2
  cat "${STAGE}/sha256sum.txt" >&2
  exit 1
fi
( cd "${STAGE}" && printf '%s\n' "${CHECKS}" | sha256sum -c - )

install -m 0755 "${STAGE}/omni-linux-amd64" "${WORKDIR}/bin/omni"
install -m 0755 "${STAGE}/omnictl-linux-amd64" "${WORKDIR}/bin/omnictl"

# omni serves omnictl downloads from ./omnictl relative to its working
# directory (server.go: getOmnictlDownloads("./omnictl/")).
install -m 0755 "${STAGE}/omnictl-linux-amd64" "${WORKDIR}/omnictl/omnictl-linux-amd64"

echo "omni ${VERSION}: bin/omni + bin/omnictl installed and checksum-verified in ${WORKDIR}"
