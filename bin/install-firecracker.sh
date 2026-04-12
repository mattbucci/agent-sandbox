#!/usr/bin/env bash
# Download and install Firecracker + Jailer binaries
set -euo pipefail

FC_VERSION="v1.12.0"
ARCH="x86_64"
INSTALL_DIR="/usr/local/bin"

RELEASE_URL="https://github.com/firecracker-microvm/firecracker/releases/download/${FC_VERSION}/firecracker-${FC_VERSION}-${ARCH}.tgz"

echo "=== Firecracker ${FC_VERSION} Installer ==="

# Check if already installed
if command -v firecracker &>/dev/null; then
    CURRENT_VERSION=$(firecracker --version 2>/dev/null | head -1 || echo "unknown")
    echo "Firecracker already installed: ${CURRENT_VERSION}"
    read -rp "Reinstall? [y/N] " confirm
    if [[ "${confirm}" != "y" && "${confirm}" != "Y" ]]; then
        exit 0
    fi
fi

# Check for root
if [[ $EUID -ne 0 ]]; then
    echo "ERROR: This script must be run as root (need to write to ${INSTALL_DIR})"
    echo "Run: sudo $0"
    exit 1
fi

# Check KVM
if [[ ! -e /dev/kvm ]]; then
    echo "WARNING: /dev/kvm not found. Firecracker requires KVM support."
    echo "Check that your CPU supports virtualization and it's enabled in BIOS."
fi

TMPDIR=$(mktemp -d)
trap "rm -rf ${TMPDIR}" EXIT

echo "Downloading Firecracker ${FC_VERSION}..."
curl -fSL -o "${TMPDIR}/firecracker.tgz" "${RELEASE_URL}"

echo "Extracting..."
tar -xzf "${TMPDIR}/firecracker.tgz" -C "${TMPDIR}"

# Find the extracted binaries (they're in a subdirectory)
FC_DIR="${TMPDIR}/release-${FC_VERSION}-${ARCH}"

echo "Installing to ${INSTALL_DIR}..."
install -m 0755 "${FC_DIR}/firecracker-${FC_VERSION}-${ARCH}" "${INSTALL_DIR}/firecracker"
install -m 0755 "${FC_DIR}/jailer-${FC_VERSION}-${ARCH}" "${INSTALL_DIR}/jailer"

# Set KVM device permissions
if [[ -e /dev/kvm ]]; then
    chmod 666 /dev/kvm
    echo "Set /dev/kvm permissions to 666"
fi

echo ""
echo "Installed:"
echo "  firecracker: $(firecracker --version 2>/dev/null | head -1)"
echo "  jailer:      $(jailer --version 2>/dev/null | head -1)"
echo ""
echo "Done."
