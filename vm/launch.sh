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

source "${SANDBOX_ROOT}/lib/common.sh"
ensure_global_compiled
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

# Load compiled agent config
ensure_compiled "${AGENT_TYPE}"
AGENT_CONF="${SANDBOX_ROOT}/build/${AGENT_TYPE}/agent.conf"
if [[ ! -f "${AGENT_CONF}" ]]; then
    echo "ERROR: Unknown agent type '${AGENT_TYPE}'"
    echo "Available: $(list_agent_types | tr '\n' ' ')"
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
ALLOWLIST_FILE="${SANDBOX_ROOT}/build/${AGENT_TYPE}/allowlist.txt"
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

# Use jailer if available (production), fall back to raw firecracker (dev)
USE_JAILER=0
if [[ -x "${JAILER_BIN:-/usr/local/bin/jailer}" ]] && [[ "${NO_JAILER:-0}" != "1" ]]; then
    USE_JAILER=1
fi

if [[ ${USE_JAILER} -eq 1 ]]; then
    # Jailer chroots Firecracker and runs it with its working directory at the
    # chroot root, so the kernel, rootfs and config must live INSIDE the chroot
    # and be referenced by chroot-relative paths (not the host paths in
    # ${FC_CONFIG}). Stage them here.
    JAILER_DIR="/srv/jailer"
    JAIL_ID="${INSTANCE_ID}"
    JAIL_ROOT="${JAILER_DIR}/firecracker/${JAIL_ID}/root"
    JAIL_UID=$((10000 + SLOT))
    JAIL_GID=$((10000 + SLOT))

    # Dedicated unprivileged uid/gid per VM (jailer drops to it)
    id -u "fc-${SLOT}" &>/dev/null 2>&1 || \
        useradd -r -u "${JAIL_UID}" -s /usr/sbin/nologin "fc-${SLOT}" 2>/dev/null || true

    # Fresh chroot tree (jailer copies the firecracker binary in itself)
    rm -rf "${JAILER_DIR}/firecracker/${JAIL_ID}"
    mkdir -p "${JAIL_ROOT}"

    # Kernel: copy in. Rootfs: hard-link (shares the inode with the state copy,
    # stays read-write), falling back to a copy across filesystems.
    cp --reflink=auto "${KERNEL_PATH}" "${JAIL_ROOT}/vmlinux"
    ln -f "${VM_STATE_DIR}/rootfs.ext4" "${JAIL_ROOT}/rootfs.ext4" 2>/dev/null \
        || cp --reflink=auto "${VM_STATE_DIR}/rootfs.ext4" "${JAIL_ROOT}/rootfs.ext4"

    # Firecracker's own log + metrics live in the chroot. The guest serial
    # console still goes to the jailer process stdout (${LOG_FILE}.stdout).
    : > "${JAIL_ROOT}/firecracker.log"
    : > "${JAIL_ROOT}/metrics"

    # Jailer-local config with chroot-relative paths.
    sed \
        -e "s|__KERNEL_PATH__|/vmlinux|g" \
        -e "s|__ROOTFS_PATH__|/rootfs.ext4|g" \
        -e "s|__VCPUS__|${VCPUS}|g" \
        -e "s|__MEM_MB__|${MEM_MB}|g" \
        -e "s|__TAP_NAME__|${TAP_NAME}|g" \
        -e "s|__GUEST_MAC__|${GUEST_MAC}|g" \
        -e "s|__VM_IP__|${VM_IP}|g" \
        -e "s|__GATEWAY_IP__|${GATEWAY_IP}|g" \
        -e "s|__DNS_IP__|${DNS_IP}|g" \
        -e "s|__HOSTNAME__|${VM_NAME}|g" \
        -e "s|__NO_AGENT__|${NO_AGENT}|g" \
        -e "s|__LOG_PATH__|/firecracker.log|g" \
        -e "s|__METRICS_PATH__|/metrics|g" \
        "${SANDBOX_ROOT}/vm/config-template.json" > "${JAIL_ROOT}/vm-config.json"

    # Everything the dropped-privilege process touches must be owned by it.
    chown -R "${JAIL_UID}:${JAIL_GID}" "${JAILER_DIR}/firecracker/${JAIL_ID}"

    # Jailer creates the API socket inside the chroot; record it for stop.sh.
    FC_SOCKET="${JAIL_ROOT}/run/firecracker.socket"

    ${JAILER_BIN} \
        --id "${JAIL_ID}" \
        --exec-file "${FIRECRACKER_BIN}" \
        --uid "${JAIL_UID}" \
        --gid "${JAIL_GID}" \
        --chroot-base-dir "${JAILER_DIR}" \
        --cgroup-version 2 \
        -- \
        --config-file vm-config.json \
        &>"${LOG_FILE}.stdout" &
    FC_PID=$!
else
    # Development mode: raw firecracker (no jailer)
    echo "  (jailer not available or NO_JAILER=1 — using raw firecracker)"
    ${FIRECRACKER_BIN} \
        --api-sock "${FC_SOCKET}" \
        --config-file "${FC_CONFIG}" \
        &>"${LOG_FILE}.stdout" &
    FC_PID=$!
fi

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
