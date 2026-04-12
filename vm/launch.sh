#!/usr/bin/env bash
# =============================================================================
# launch.sh — Launch a Firecracker microVM for an agent
#
# Usage: launch.sh <agent-type> [--name NAME] [--vcpus N] [--mem MB]
#
# Orchestrates:
#   1. Assign slot number
#   2. Prepare rootfs (COW clone + config injection)
#   3. Create TAP device and configure host networking
#   4. Generate Squid ACL for domain filtering
#   5. Generate Firecracker config
#   6. Launch Firecracker process
#   7. Start websockify for noVNC
#   8. Record state
# =============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
SANDBOX_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

source "${SANDBOX_ROOT}/config/sandbox.conf"
source "${SANDBOX_ROOT}/lib/common.sh"
source "${SANDBOX_ROOT}/lib/network.sh"

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

# Load agent config for defaults
AGENT_CONF="${SANDBOX_ROOT}/config/agents/${AGENT_TYPE}.conf"
if [[ ! -f "${AGENT_CONF}" ]]; then
    echo "ERROR: Unknown agent type '${AGENT_TYPE}'"
    echo "Available: $(ls "${SANDBOX_ROOT}/config/agents/" | sed 's/.conf$//' | tr '\n' ' ')"
    exit 1
fi
source "${AGENT_CONF}"

# Apply defaults
VCPUS="${VCPUS:-${DEFAULT_VCPUS:-4}}"
MEM_MB="${MEM_MB:-${DEFAULT_MEM_MB:-8192}}"

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

# --- 2. Prepare rootfs ---
echo "[1/7] Preparing rootfs..."
"${SANDBOX_ROOT}/vm/prepare-rootfs.sh" "${AGENT_TYPE}" "${INSTANCE_ID}" "${VM_STATE_DIR}"

# --- 3. Create TAP device ---
echo "[2/7] Creating TAP device..."
TAP_NAME="tap-vm${SLOT}"
VM_IP="${VM_SUBNET_PREFIX}.${SLOT}.2"
GATEWAY_IP="${VM_SUBNET_PREFIX}.${SLOT}.1"
DNS_IP="${GATEWAY_IP}"

create_tap "${TAP_NAME}" "${GATEWAY_IP}"

# --- 4. Add nftables rules ---
echo "[3/7] Configuring firewall rules..."
add_vm_nftables "${SLOT}" "${TAP_NAME}" "${GATEWAY_IP}"

# --- 5. Generate Squid ACL ---
echo "[4/7] Generating Squid ACL..."
ALLOWLIST_FILE="${SANDBOX_ROOT}/rootfs/agents/${AGENT_TYPE}/allowlist.txt"
"${SANDBOX_ROOT}/network/squid/gen-acl.sh" "${SLOT}" "${AGENT_TYPE}" "${ALLOWLIST_FILE}"

# Reload Squid
squid -k reconfigure 2>/dev/null || true

# --- 6. Generate Firecracker config ---
echo "[5/7] Generating VM config..."
GUEST_MAC=$(printf "AA:FC:00:00:00:%02X" "${SLOT}")
LOG_FILE="${LOG_DIR}/${INSTANCE_ID}.log"
METRICS_FILE="${VM_STATE_DIR}/metrics.fifo"
FC_CONFIG="${VM_STATE_DIR}/vm-config.json"
FC_SOCKET="${VM_STATE_DIR}/firecracker.sock"

# Create log file and metrics FIFO (Firecracker requires these to exist)
mkdir -p "${LOG_DIR}"
touch "${LOG_FILE}"
rm -f "${METRICS_FILE}"
mkfifo "${METRICS_FILE}"

# Generate config from template
sed \
    -e "s|__KERNEL_PATH__|${KERNEL_PATH}|g" \
    -e "s|__ROOTFS_PATH__|${VM_STATE_DIR}/rootfs.ext4|g" \
    -e "s|__VCPUS__|${VCPUS}|g" \
    -e "s|__MEM_MB__|${MEM_MB}|g" \
    -e "s|__TAP_NAME__|${TAP_NAME}|g" \
    -e "s|__GUEST_MAC__|${GUEST_MAC}|g" \
    -e "s|__VM_IP__|${VM_IP}|g" \
    -e "s|__GATEWAY_IP__|${GATEWAY_IP}|g" \
    -e "s|__DNS_IP__|${DNS_IP}|g" \
    -e "s|__HOSTNAME__|${VM_NAME}|g" \
    -e "s|__NO_AGENT__|${NO_AGENT}|g" \
    -e "s|__LOG_PATH__|${LOG_FILE}|g" \
    -e "s|__METRICS_PATH__|${METRICS_FILE}|g" \
    "${SANDBOX_ROOT}/vm/config-template.json" > "${FC_CONFIG}"

# --- 7. Launch Firecracker ---
echo "[6/7] Starting Firecracker..."

# Remove stale socket
rm -f "${FC_SOCKET}"

# Launch Firecracker in background
${FIRECRACKER_BIN} \
    --api-sock "${FC_SOCKET}" \
    --config-file "${FC_CONFIG}" \
    &>"${LOG_FILE}.stdout" &
FC_PID=$!

echo "${FC_PID}" > "${VM_STATE_DIR}/firecracker.pid"

# Wait briefly to check it didn't crash immediately
sleep 2
if ! kill -0 ${FC_PID} 2>/dev/null; then
    echo "ERROR: Firecracker process died immediately. Check logs:"
    echo "  ${LOG_FILE}"
    echo "  ${LOG_FILE}.stdout"
    cat "${LOG_FILE}.stdout" 2>/dev/null || true
    cleanup_vm_network "${SLOT}" "${TAP_NAME}"
    exit 1
fi

echo "  Firecracker running (PID ${FC_PID})"

# --- 8. Start websockify for noVNC ---
echo "[7/7] Starting noVNC proxy..."
NOVNC_PORT=$((NOVNC_BASE_PORT + SLOT))

# websockify connects to the VM's VNC port via the TAP network
websockify \
    --web /opt/novnc \
    0.0.0.0:${NOVNC_PORT} \
    ${VM_IP}:5900 \
    &>/dev/null &
WS_PID=$!
echo "${WS_PID}" > "${VM_STATE_DIR}/websockify.pid"

# --- Record state ---
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
