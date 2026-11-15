#!/usr/bin/env bash
# Download, verify and install the pinned Firecracker + Jailer binaries.
#
# Idempotent: exits early when the pinned version is already installed, and
# upgrades in place otherwise. Running VMs keep the binary they were started
# with (install replaces the file, it does not rewrite it), so an upgrade only
# takes effect for VMs launched afterwards.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
SANDBOX_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
source "${SANDBOX_ROOT}/lib/versions.sh"

FC_VERSION="${FIRECRACKER_VERSION}"
ARCH="$(uname -m)"
INSTALL_DIR="${INSTALL_DIR:-/usr/local/bin}"

RELEASE_URL="https://github.com/firecracker-microvm/firecracker/releases/download/${FC_VERSION}/firecracker-${FC_VERSION}-${ARCH}.tgz"

echo "=== Firecracker ${FC_VERSION} Installer ==="

installed_version() {
    "${INSTALL_DIR}/$1" --version 2>/dev/null | head -1 | grep -oE 'v[0-9]+\.[0-9]+\.[0-9]+' || true
}

if [[ "$(installed_version firecracker)" == "${FC_VERSION}" && "$(installed_version jailer)" == "${FC_VERSION}" ]]; then
    echo "Firecracker and jailer ${FC_VERSION} already installed in ${INSTALL_DIR}."
    exit 0
fi

if [[ ! -w "${INSTALL_DIR}" ]]; then
    echo "ERROR: cannot write to ${INSTALL_DIR} — run as root: sudo $0"
    exit 1
fi

if [[ ! -e /dev/kvm ]]; then
    echo "WARNING: /dev/kvm not found. Firecracker requires KVM support."
    echo "Check that your CPU supports virtualization and it's enabled in BIOS."
fi

WORK_DIR="$(mktemp -d)"
trap 'rm -rf "${WORK_DIR}"' EXIT

echo "Downloading Firecracker ${FC_VERSION} (sha256 verified)..."
fetch_verified "${RELEASE_URL}" "$(pinned_sha "${FIRECRACKER_SHA256}")" "${WORK_DIR}/firecracker.tgz"

echo "Extracting..."
tar -xzf "${WORK_DIR}/firecracker.tgz" -C "${WORK_DIR}"
FC_DIR="${WORK_DIR}/release-${FC_VERSION}-${ARCH}"

PREVIOUS="$(installed_version firecracker)"
echo "Installing to ${INSTALL_DIR}${PREVIOUS:+ (replacing ${PREVIOUS})}..."
install -m 0755 "${FC_DIR}/firecracker-${FC_VERSION}-${ARCH}" "${INSTALL_DIR}/firecracker"
install -m 0755 "${FC_DIR}/jailer-${FC_VERSION}-${ARCH}" "${INSTALL_DIR}/jailer"

# Set KVM device permissions
if [[ -e /dev/kvm && $EUID -eq 0 ]]; then
    chmod 666 /dev/kvm
    echo "Set /dev/kvm permissions to 666"
fi

echo ""
echo "Installed:"
echo "  firecracker: $("${INSTALL_DIR}/firecracker" --version 2>/dev/null | head -1)"
echo "  jailer:      $("${INSTALL_DIR}/jailer" --version 2>/dev/null | head -1)"
if [[ -n "${PREVIOUS}" ]]; then
    echo ""
    echo "NOTE: VMs that are already running keep ${PREVIOUS} until they are relaunched."
fi
echo "Done."
