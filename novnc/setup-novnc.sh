#!/usr/bin/env bash
# =============================================================================
# setup-novnc.sh — Install the pinned noVNC and websockify on the host
# =============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
SANDBOX_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
source "${SANDBOX_ROOT}/lib/versions.sh"

NOVNC_DIR="/opt/novnc"

echo "=== noVNC Setup ==="

# Install websockify (distro package preferred; pinned pip install otherwise)
if ! command -v websockify &>/dev/null; then
    echo "Installing websockify ${WEBSOCKIFY_VERSION}..."
    pip3 install "websockify==${WEBSOCKIFY_VERSION}" 2>/dev/null \
        || pip install "websockify==${WEBSOCKIFY_VERSION}"
fi

# noVNC at the pinned tag, verified against the pinned commit so a moved tag
# cannot change what is served to the operator's browser.
if [[ ! -d "${NOVNC_DIR}/.git" ]]; then
    echo "Cloning noVNC ${NOVNC_VERSION}..."
    git clone --depth 1 --branch "${NOVNC_VERSION}" https://github.com/novnc/noVNC.git "${NOVNC_DIR}"
elif [[ "$(git -C "${NOVNC_DIR}" rev-parse HEAD)" != "${NOVNC_COMMIT}" ]]; then
    echo "Updating noVNC to ${NOVNC_VERSION}..."
    git -C "${NOVNC_DIR}" fetch --depth 1 origin "refs/tags/${NOVNC_VERSION}:refs/tags/${NOVNC_VERSION}"
    git -C "${NOVNC_DIR}" checkout -q "${NOVNC_VERSION}"
else
    echo "noVNC ${NOVNC_VERSION} already installed at ${NOVNC_DIR}"
fi

HEAD_COMMIT="$(git -C "${NOVNC_DIR}" rev-parse HEAD)"
if [[ "${HEAD_COMMIT}" != "${NOVNC_COMMIT}" ]]; then
    echo "ERROR: noVNC ${NOVNC_VERSION} resolved to ${HEAD_COMMIT}, expected ${NOVNC_COMMIT}" >&2
    exit 1
fi

echo ""
echo "noVNC installed at ${NOVNC_DIR} (${NOVNC_VERSION})"
echo "websockify: $(command -v websockify)"
echo ""
echo "Per-VM websockify instances are started by lib/vm.sh"
echo "Access VMs at http://<host>:<port>/vnc.html?autoconnect=true"
