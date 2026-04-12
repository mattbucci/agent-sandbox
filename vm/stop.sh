#!/usr/bin/env bash
# =============================================================================
# stop.sh — Stop a running Firecracker VM
#
# Usage: stop.sh <instance-id>
#
# 1. Send graceful shutdown via Firecracker API
# 2. Wait for process to exit
# 3. Force kill if necessary
# 4. Stop websockify
# 5. Remove TAP device and nftables rules
# 6. Remove Squid ACL
# 7. Clean up state
# =============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
SANDBOX_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

source "${SANDBOX_ROOT}/config/sandbox.conf"
source "${SANDBOX_ROOT}/lib/common.sh"
source "${SANDBOX_ROOT}/lib/network.sh"

INSTANCE_ID="${1:?Usage: stop.sh <instance-id>}"

VM_STATE_DIR="${STATE_DIR}/vms/${INSTANCE_ID}"
INFO_FILE="${VM_STATE_DIR}/info.json"

if [[ ! -f "${INFO_FILE}" ]]; then
    echo "ERROR: VM state not found: ${INFO_FILE}"
    echo "Is '${INSTANCE_ID}' a valid instance ID?"
    exit 1
fi

# Parse state
SLOT=$(jq -r '.slot' "${INFO_FILE}")
VM_NAME=$(jq -r '.name' "${INFO_FILE}")
TAP_NAME=$(jq -r '.tap_device' "${INFO_FILE}")
FC_PID=$(jq -r '.firecracker_pid' "${INFO_FILE}")
WS_PID=$(jq -r '.websockify_pid' "${INFO_FILE}")
FC_SOCKET=$(jq -r '.fc_socket' "${INFO_FILE}")

echo "=== Stopping VM: ${VM_NAME} (${INSTANCE_ID}) ==="

# --- 1. Graceful shutdown ---
if kill -0 ${FC_PID} 2>/dev/null; then
    echo "[1/5] Sending shutdown signal..."

    # Try CtrlAltDel via API socket
    if [[ -S "${FC_SOCKET}" ]]; then
        curl --silent --unix-socket "${FC_SOCKET}" \
            -X PUT "http://localhost/actions" \
            -H "Content-Type: application/json" \
            -d '{"action_type": "SendCtrlAltDel"}' 2>/dev/null || true
    fi

    # Wait up to 10 seconds for graceful shutdown
    echo "  Waiting for graceful shutdown (10s)..."
    for i in $(seq 1 10); do
        if ! kill -0 ${FC_PID} 2>/dev/null; then
            echo "  VM stopped gracefully after ${i}s."
            break
        fi
        sleep 1
    done

    # Force kill if still running
    if kill -0 ${FC_PID} 2>/dev/null; then
        echo "  Force killing Firecracker (PID ${FC_PID})..."
        kill -9 ${FC_PID} 2>/dev/null || true
        sleep 1
    fi
else
    echo "[1/5] Firecracker process already stopped."
fi

# --- 2. Stop websockify ---
echo "[2/5] Stopping websockify..."
if [[ -n "${WS_PID}" ]] && kill -0 ${WS_PID} 2>/dev/null; then
    kill ${WS_PID} 2>/dev/null || true
fi

# --- 3. Remove network ---
echo "[3/5] Removing network configuration..."
cleanup_vm_network "${SLOT}" "${TAP_NAME}"

# --- 4. Remove Squid ACL ---
echo "[4/5] Removing Squid ACL..."
rm -f "/etc/squid/acls/vm${SLOT}-domains.txt"
rm -f "/etc/squid/acls/vm-${SLOT}.conf"
# Regenerate combined allowed domains
if ls /etc/squid/acls/vm*-domains.txt &>/dev/null 2>&1; then
    cat /etc/squid/acls/vm*-domains.txt 2>/dev/null | grep -v '^\s*#' | grep -v '^\s*$' | sort -u \
        > /etc/squid/acls/all-allowed-domains.txt
else
    echo "# No VMs running" > /etc/squid/acls/all-allowed-domains.txt
fi
squid -k reconfigure 2>/dev/null || true

# --- 5. Release slot and clean up ---
echo "[5/5] Cleaning up state..."
release_slot "${SLOT}"

# Optionally keep state for debugging (controlled by env var)
if [[ "${KEEP_VM_STATE:-0}" == "1" ]]; then
    echo "  State preserved at ${VM_STATE_DIR} (KEEP_VM_STATE=1)"
else
    rm -rf "${VM_STATE_DIR}"
    echo "  State removed."
fi

echo ""
echo "=== VM ${VM_NAME} stopped ==="
