#!/usr/bin/env bash
# =============================================================================
# prepare-rootfs.sh — Prepare a VM instance rootfs from agent-type image
#
# Usage: prepare-rootfs.sh <agent-type> <instance-id> <vm-state-dir>
#
# 1. Copies the agent-type rootfs (COW if possible)
# 2. Mounts it and injects runtime config:
#    - /etc/agent.conf (LLM endpoint, API key, model, agent type)
#    - /etc/agent/system-prompt.md
#    - /etc/hostname
# 3. Unmounts
# =============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
SANDBOX_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

source "${SANDBOX_ROOT}/lib/common.sh"
ensure_global_compiled

AGENT_TYPE="${1:?Usage: prepare-rootfs.sh <agent-type> <instance-id> <vm-state-dir>}"
INSTANCE_ID="${2:?Usage: prepare-rootfs.sh <agent-type> <instance-id> <vm-state-dir>}"
VM_STATE_DIR="${3:?Usage: prepare-rootfs.sh <agent-type> <instance-id> <vm-state-dir>}"

# Paths — use compiled artifacts
ensure_compiled "${AGENT_TYPE}"
BUILD_DIR="${SANDBOX_ROOT}/build/${AGENT_TYPE}"
AGENT_ROOTFS="${SANDBOX_ROOT}/rootfs/agents/${AGENT_TYPE}/rootfs.ext4"
INSTANCE_ROOTFS="${VM_STATE_DIR}/rootfs.ext4"

# Validate
if [[ ! -f "${AGENT_ROOTFS}" ]]; then
    echo "ERROR: Agent rootfs not found: ${AGENT_ROOTFS}"
    echo "Run 'sandbox-ctl build-agent ${AGENT_TYPE}' first."
    exit 1
fi

# --- Load compiled agent + global config ---
source "${BUILD_DIR}/agent.conf"

# --- Copy rootfs (COW if supported) ---
echo "Copying rootfs for instance ${INSTANCE_ID}..."
cp --reflink=auto --sparse=always "${AGENT_ROOTFS}" "${INSTANCE_ROOTFS}"

# --- Mount and inject config ---
MOUNT_POINT=$(mktemp -d)
mount -o loop "${INSTANCE_ROOTFS}" "${MOUNT_POINT}"

cleanup() {
    umount -lf "${MOUNT_POINT}" 2>/dev/null || true
    rmdir "${MOUNT_POINT}" 2>/dev/null || true
}
trap cleanup EXIT

# Write agent.conf
cat > "${MOUNT_POINT}/etc/agent.conf" <<EOF
# Agent configuration — injected at VM launch time
# Instance: ${INSTANCE_ID}
# Type: ${AGENT_TYPE}
# Generated: $(date -Iseconds)

AGENT_TYPE="${AGENT_TYPE}"
AGENT_NAME="${AGENT_NAME:-${AGENT_TYPE} Agent}"
LLM_API_BASE="${LLM_API_BASE}"
LLM_API_KEY="${LLM_API_KEY}"
LLM_MODEL="${LLM_MODEL}"
EOF

# Write compiled system prompt
mkdir -p "${MOUNT_POINT}/etc/agent"
cp "${BUILD_DIR}/system-prompt.md" "${MOUNT_POINT}/etc/agent/system-prompt.md"

# Set hostname
echo "${AGENT_TYPE}-${INSTANCE_ID}" > "${MOUNT_POINT}/etc/hostname"

# Unmount
cleanup
trap - EXIT

echo "Rootfs prepared: ${INSTANCE_ROOTFS}"
