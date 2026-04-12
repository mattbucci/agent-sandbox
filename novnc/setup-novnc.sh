#!/usr/bin/env bash
# =============================================================================
# setup-novnc.sh — Install noVNC and websockify on the host
# =============================================================================
set -euo pipefail

NOVNC_DIR="/opt/novnc"

echo "=== noVNC Setup ==="

# Install websockify
if ! command -v websockify &>/dev/null; then
    echo "Installing websockify..."
    pip3 install websockify 2>/dev/null || pip install websockify
fi

# Clone noVNC
if [[ ! -d "${NOVNC_DIR}" ]]; then
    echo "Cloning noVNC..."
    git clone --depth 1 https://github.com/novnc/noVNC.git "${NOVNC_DIR}"
else
    echo "noVNC already installed at ${NOVNC_DIR}"
fi

echo ""
echo "noVNC installed at ${NOVNC_DIR}"
echo "websockify: $(which websockify)"
echo ""
echo "Per-VM websockify instances are started by vm/launch.sh"
echo "Access VMs at http://<host>:<port>/vnc.html?autoconnect=true"
