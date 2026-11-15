#!/usr/bin/env bash
# Fetch the pinned pre-built Firecracker guest kernel (sha256 verified).
#
# Firecracker's CI kernels lack the netfilter tables Docker's bridge driver
# needs; agents with the `docker` capability want kernel/build-kernel.sh.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
SANDBOX_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
source "${SANDBOX_ROOT}/lib/versions.sh"

KERNEL_OUT="${SCRIPT_DIR}/vmlinux"
ARCH="$(uname -m)"
# The S3 path uses the Firecracker minor series (v1.15), not a patch release.
KERNEL_URL="https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/${GUEST_KERNEL_CI_SERIES}/${ARCH}/vmlinux-${GUEST_KERNEL_VERSION}"

if [[ -f "${KERNEL_OUT}" ]]; then
    echo "Kernel already exists at ${KERNEL_OUT}"
    echo "Remove it first to re-download."
    exit 0
fi

echo "Downloading Firecracker guest kernel ${GUEST_KERNEL_VERSION} (sha256 verified)..."
echo "URL: ${KERNEL_URL}"
fetch_verified "${KERNEL_URL}" "$(pinned_sha "${GUEST_KERNEL_SHA256}")" "${KERNEL_OUT}"

chmod 644 "${KERNEL_OUT}"
echo "Kernel downloaded to ${KERNEL_OUT}"
echo "Size: $(du -h "${KERNEL_OUT}" | cut -f1)"
