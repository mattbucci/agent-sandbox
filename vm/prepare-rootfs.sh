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
source "${SANDBOX_ROOT}/lib/network.sh"   # resolve_ipv4
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

# --- Resolve the LLM host to an IP (host-side) ------------------------------
# The VM's dnsmasq only forwards to PUBLIC upstreams, so an internal name like
# simple-llm-router.ph.ca would NXDOMAIN inside the VM. Resolve it on the host
# (same getent-based resolution the nftables LLM passthrough uses) and pin the
# result into the VM's /etc/hosts (and, for hermes, the container's --add-host),
# so the friendly DNS name keeps working inside the sandbox.
LLM_HOST=$(echo "${LLM_API_BASE}" | sed -E 's|https?://||; s|:[0-9]+.*||; s|/.*||')
LLM_HOST_IP=""
if [[ -n "${LLM_HOST}" ]]; then
    LLM_HOST_IP=$(resolve_ipv4 "${LLM_HOST}") || true
    if [[ -z "${LLM_HOST_IP}" ]]; then
        echo "WARNING: could not resolve LLM host '${LLM_HOST}' host-side; the VM may not reach the LLM endpoint by name" >&2
    fi
fi

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
LLM_HOST="${LLM_HOST}"
LLM_HOST_IP="${LLM_HOST_IP}"
GATEWAY_ENABLED="${GATEWAY_ENABLED:-1}"
GATEWAY_PORT="${GATEWAY_PORT:-8642}"
API_SERVER_KEY="${API_SERVER_KEY:-}"
HARNESS="${HARNESS:-deepagents}"
EOF

# /etc/hosts entry for the internal LLM endpoint. agent-init rewrites /etc/hosts
# at boot, so we stage extras in /etc/agent-hosts which it appends. Skip when the
# host is already an IP literal or could not be resolved.
if [[ -n "${LLM_HOST_IP}" && "${LLM_HOST}" != "${LLM_HOST_IP}" ]]; then
    echo "${LLM_HOST_IP} ${LLM_HOST}" > "${MOUNT_POINT}/etc/agent-hosts"
fi

# --- Hermes harness: inject the container's LLM + docker config ---
# Only the hermes backend needs these files baked into the instance rootfs.
if [[ "${HARNESS:-deepagents}" == "hermes" ]]; then
    echo "Injecting hermes container config (LLM endpoint + docker daemon)..."

    # LLM provider config consumed by the hermes-agent container at /opt/data.
    mkdir -p "${MOUNT_POINT}/opt/hermes/data"
    cat > "${MOUNT_POINT}/opt/hermes/data/config.yaml" <<EOF
model:
  default: ${LLM_MODEL}
  provider: custom
  base_url: ${LLM_API_BASE}
  api_key: ${LLM_API_KEY}
EOF

    # Disable docker's iptables management so dockerd comes up on the stock
    # guest kernel (no 'raw' table). Must exist before dockerd starts at boot.
    # This OVERWRITES /etc/docker/daemon.json, deliberately re-specifying the
    # build-time settings from config/install-scripts/docker-compose.sh
    # (storage-driver=overlay2, userland-proxy=false) so they are preserved — a
    # vfs fallback would copy every layer and exhaust even the enlarged hermes
    # rootfs (config/sandbox.yaml rootfs.per_agent.hermes). Keep these keys in
    # sync with docker-compose.sh if it ever changes them.
    mkdir -p "${MOUNT_POINT}/etc/docker"
    cat > "${MOUNT_POINT}/etc/docker/daemon.json" <<EOF
{"iptables": false, "userland-proxy": false, "storage-driver": "overlay2"}
EOF
fi

# Write compiled system prompt
mkdir -p "${MOUNT_POINT}/etc/agent"
cp "${BUILD_DIR}/system-prompt.md" "${MOUNT_POINT}/etc/agent/system-prompt.md"

# Set hostname
echo "${AGENT_TYPE}-${INSTANCE_ID}" > "${MOUNT_POINT}/etc/hostname"

# Unmount
cleanup
trap - EXIT

echo "Rootfs prepared: ${INSTANCE_ROOTFS}"
