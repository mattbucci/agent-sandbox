#!/usr/bin/env bash
# =============================================================================
# launch.sh — Launch a Firecracker microVM for an agent
#
# Usage: launch.sh <agent-type> [--name NAME] [--vcpus N] [--mem MB]
#
# Orchestrates:
#   1. Assign slot number
#   2. Prepare rootfs (COW clone + config injection)
#   3. Create TAP device, firewall rules and the Squid egress ACL
#   4. Launch Firecracker (under the jailer when installed)
#   5. Start websockify for noVNC
#   6. Record state
#
# Steps 3-5 live in lib/vm.sh, shared with the boot-time restore
# (bin/agent-sandbox-boot.sh) so both paths behave identically.
# =============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
SANDBOX_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

source "${SANDBOX_ROOT}/lib/common.sh"
ensure_global_compiled
source "${SANDBOX_ROOT}/lib/network.sh"
source "${SANDBOX_ROOT}/lib/vm.sh"

AGENT_TYPE="${1:?Usage: launch.sh <agent-type> [--name NAME] [--vcpus N] [--mem MB]}"
shift

# Parse optional args
VM_NAME=""
VCPUS=""
MEM_MB=""
NO_AGENT=0
while [[ $# -gt 0 ]]; do
    case "$1" in
        --name)     VM_NAME="$2"; shift 2 ;;
        --vcpus)    VCPUS="$2"; shift 2 ;;
        --mem)      MEM_MB="$2"; shift 2 ;;
        --no-agent) NO_AGENT=1; shift ;;
        *)          echo "Unknown option: $1"; exit 1 ;;
    esac
done

# Load compiled agent config
ensure_compiled "${AGENT_TYPE}"
AGENT_CONF="${SANDBOX_ROOT}/build/${AGENT_TYPE}/agent.conf"
if [[ ! -f "${AGENT_CONF}" ]]; then
    echo "ERROR: Unknown agent type '${AGENT_TYPE}'"
    echo "Available: $(list_agent_types | tr '\n' ' ')"
    exit 1
fi
# agent.conf carries the agent type's own VCPUS/MEM_MB; explicit --vcpus/--mem
# on the command line win over it, and the global defaults fill in the rest.
CLI_VCPUS="${VCPUS}"
CLI_MEM_MB="${MEM_MB}"
VCPUS=""
MEM_MB=""
source "${AGENT_CONF}"

VCPUS="${CLI_VCPUS:-${VCPUS:-${DEFAULT_VCPUS:-4}}}"
MEM_MB="${CLI_MEM_MB:-${MEM_MB:-${DEFAULT_MEM_MB:-8192}}}"

# --- 1. Assign slot ---
SLOT=$(allocate_slot)
INSTANCE_ID="${AGENT_TYPE}-${SLOT}"
VM_NAME="${VM_NAME:-${INSTANCE_ID}}"

echo "=== Launching VM: ${VM_NAME} ==="
echo "  Type: ${AGENT_TYPE}"
echo "  Slot: ${SLOT}"
echo "  vCPUs: ${VCPUS}, RAM: ${MEM_MB}MB"

# Create state directory
VM_STATE_DIR="${STATE_DIR}/vms/${INSTANCE_ID}"
mkdir -p "${VM_STATE_DIR}"

# Network addressing is derived from the slot; compute it here (before rootfs
# prep) so prepare-rootfs can bake the per-VM gateway IP into agent.conf — the
# OTel collector lives on the host and the VM reaches it at its gateway IP.
TAP_NAME="tap-vm${SLOT}"
VM_IP="${VM_SUBNET_PREFIX}.${SLOT}.2"
GATEWAY_IP="${VM_SUBNET_PREFIX}.${SLOT}.1"
DNS_IP="${GATEWAY_IP}"

# --- 2. Prepare rootfs ---
echo "[1/4] Preparing rootfs..."
GATEWAY_IP="${GATEWAY_IP}" "${SANDBOX_ROOT}/vm/prepare-rootfs.sh" "${AGENT_TYPE}" "${INSTANCE_ID}" "${VM_STATE_DIR}"

# --- 3-5. TAP device, firewall rules, Squid egress ACL ---
echo "[2/4] Configuring network (TAP, firewall, egress ACL)..."
vm_network_up "${SLOT}" "${TAP_NAME}" "${GATEWAY_IP}" "${AGENT_TYPE}"
squid -k reconfigure 2>/dev/null || true

# --- 6-7. Launch Firecracker (jailed when the jailer is installed) ---
echo "[3/4] Starting Firecracker..."
GUEST_MAC=$(printf "AA:FC:00:00:00:%02X" "${SLOT}")
LOG_FILE="${LOG_DIR}/${INSTANCE_ID}.log"
vm_start_firecracker

# Wait briefly to check it didn't crash immediately
sleep 2
if ! kill -0 "${FC_PID}" 2>/dev/null; then
    echo "ERROR: Firecracker process died immediately. Check logs:"
    echo "  ${LOG_FILE}"
    echo "  ${LOG_FILE}.stdout"
    cat "${LOG_FILE}.stdout" 2>/dev/null || true
    cleanup_vm_network "${SLOT}" "${TAP_NAME}"
    vm_remove_jail "${INSTANCE_ID}"
    exit 1
fi

echo "  Firecracker running (PID ${FC_PID})"

# --- 8. Start websockify for noVNC ---
echo "[4/4] Starting noVNC proxy..."
NOVNC_PORT=$((NOVNC_BASE_PORT + SLOT))
vm_start_novnc "${NOVNC_PORT}" "${VM_IP}"

# --- Record state ---
# llm_host_ip: the LLM address this VM was launched against (also pinned into
# its /etc/hosts); the boot-time restore falls back to it if DNS is not up yet.
LLM_HOST_IP="$(resolve_ipv4 "$(llm_host_from_config)" 2>/dev/null || true)"
cat > "${VM_STATE_DIR}/info.json" <<EOF
{
  "instance_id": "${INSTANCE_ID}",
  "name": "${VM_NAME}",
  "agent_type": "${AGENT_TYPE}",
  "slot": ${SLOT},
  "vm_ip": "${VM_IP}",
  "gateway_ip": "${GATEWAY_IP}",
  "tap_device": "${TAP_NAME}",
  "guest_mac": "${GUEST_MAC}",
  "llm_host_ip": "${LLM_HOST_IP}",
  "vcpus": ${VCPUS},
  "mem_mb": ${MEM_MB},
  "firecracker_pid": ${FC_PID},
  "websockify_pid": ${WS_PID},
  "novnc_port": ${NOVNC_PORT},
  "ssh_port": $((SSH_BASE_PORT + SLOT)),
  "fc_socket": "${FC_SOCKET}",
  "log_file": "${LOG_FILE}",
  "started_at": "$(date -Iseconds)"
}
EOF

echo ""
echo "=== VM ${VM_NAME} launched successfully ==="
echo ""
echo "  Instance ID:  ${INSTANCE_ID}"
echo "  VM IP:        ${VM_IP}"
echo "  noVNC:        http://$(hostname -f 2>/dev/null || hostname):${NOVNC_PORT}/vnc.html?autoconnect=true"
echo "  Serial log:   ${LOG_FILE}"
echo "  SSH:          ssh agent@${VM_IP} (password: agent)"
echo ""
