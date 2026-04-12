#!/usr/bin/env bash
# Fetch a pre-built Firecracker-compatible Linux kernel
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
KERNEL_OUT="${SCRIPT_DIR}/vmlinux"

# Firecracker v1.12+ compatible kernel
# Using the official Firecracker CI kernel builds
FC_VERSION="v1.12.0"
ARCH="x86_64"
KERNEL_URL="https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/${FC_VERSION}/${ARCH}/vmlinux-6.1.102"

if [[ -f "${KERNEL_OUT}" ]]; then
    echo "Kernel already exists at ${KERNEL_OUT}"
    echo "Remove it first to re-download."
    exit 0
fi

echo "Downloading Firecracker-compatible kernel (6.1.x)..."
echo "URL: ${KERNEL_URL}"

if command -v curl &>/dev/null; then
    curl -fSL -o "${KERNEL_OUT}" "${KERNEL_URL}"
elif command -v wget &>/dev/null; then
    wget -O "${KERNEL_OUT}" "${KERNEL_URL}"
else
    echo "ERROR: Neither curl nor wget found. Install one and retry."
    exit 1
fi

chmod 644 "${KERNEL_OUT}"
echo "Kernel downloaded to ${KERNEL_OUT}"
echo "Size: $(du -h "${KERNEL_OUT}" | cut -f1)"
