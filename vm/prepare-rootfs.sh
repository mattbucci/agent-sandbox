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
source "${SANDBOX_ROOT}/lib/versions.sh"  # HERMES_AGENT_VERSION
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
HERMES_AGENT_VERSION="${HERMES_AGENT_VERSION}"
MNEMOSYNE_ENABLED="${MNEMOSYNE_ENABLED:-0}"
MNEMOSYNE_PORT="${MNEMOSYNE_PORT:-8077}"
MNEMOSYNE_TOKEN="${MNEMOSYNE_TOKEN:-}"
MNEMOSYNE_URL="${MNEMOSYNE_URL:-}"
HINDSIGHT_ENABLED="${HINDSIGHT_ENABLED:-0}"
HINDSIGHT_TOKEN="${HINDSIGHT_TOKEN:-}"
HINDSIGHT_BANK="${HINDSIGHT_BANK:-agent-sandbox}"
HINDSIGHT_EXTRA_BANKS="${HINDSIGHT_EXTRA_BANKS:-}"
EOF
# Optional model limits (only present when configured).
[[ -n "${LLM_CONTEXT_WINDOW:-}" ]] && echo "LLM_CONTEXT_WINDOW=\"${LLM_CONTEXT_WINDOW}\"" >> "${MOUNT_POINT}/etc/agent.conf"
[[ -n "${LLM_MAX_OUTPUT_TOKENS:-}" ]] && echo "LLM_MAX_OUTPUT_TOKENS=\"${LLM_MAX_OUTPUT_TOKENS}\"" >> "${MOUNT_POINT}/etc/agent.conf"

# OTel: the collector runs on the host; the VM reaches it at its gateway IP.
# GATEWAY_IP is exported by launch.sh. Without this, agent.py's init_tracing()
# finds no endpoint and disables tracing (the collector + in-VM span code exist
# but never received anything). Firewall allows VM->host on 4317/4318.
if [[ -n "${GATEWAY_IP:-}" ]]; then
    cat >> "${MOUNT_POINT}/etc/agent.conf" <<EOF
OTEL_EXPORTER_OTLP_ENDPOINT="http://${GATEWAY_IP}:4318"
EOF
    # Hindsight also runs on the host, reached at the same gateway IP.
    if [[ "${HINDSIGHT_ENABLED:-0}" == "1" ]]; then
        echo "HINDSIGHT_URL=\"http://${GATEWAY_IP}:${HINDSIGHT_PORT:-8890}\"" >> "${MOUNT_POINT}/etc/agent.conf"
    fi
fi

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
    # context_length: hermes-agent refuses models advertising <64K context, and
    # the router does not report a real window for custom providers (defaults to
    # 8192). Override to the backing model's real window (gemma 3 = 128K). Adjust
    # via HERMES_CONTEXT_LENGTH if you point hermes at a smaller-context model.
    cat > "${MOUNT_POINT}/opt/hermes/data/config.yaml" <<EOF
model:
  default: ${LLM_MODEL}
  provider: custom
  base_url: ${LLM_API_BASE}
  api_key: ${LLM_API_KEY}
  context_length: ${HERMES_CONTEXT_LENGTH:-131072}
EOF
    # Completion budget. Without it hermes asks for 65536 output tokens and a
    # local model server answers 400 ("max_completion_tokens is too large").
    if [[ -n "${LLM_MAX_OUTPUT_TOKENS:-}" ]]; then
        echo "  max_tokens: ${LLM_MAX_OUTPUT_TOKENS}" >> "${MOUNT_POINT}/opt/hermes/data/config.yaml"
    fi

    # Shared memory for the hermes container.
    #
    # Hindsight: hermes-agent has its own Hindsight memory provider
    # (plugins/memory/hindsight), so it is configured natively rather than
    # through MCP: `memory.provider: hindsight` selects it, and
    # $HERMES_HOME/hindsight/config.json points it at the sandbox's service in
    # "local_external" mode. The provider then recalls before each turn, retains
    # after it, and exposes hindsight_recall / _retain / _reflect tools — the same
    # behaviour gateway_server.py gives the other harnesses. The key is this
    # agent's own token; what it can reach is decided by the access policy.
    #
    # mnemosyne: an MCP server in config.yaml. Even with --network host the
    # container gets its own /etc/hosts, so mnemosyne.host is also pinned via
    # --add-host in run-hermes.sh.
    if [[ "${HINDSIGHT_ENABLED:-0}" == "1" && ( -z "${HINDSIGHT_TOKEN:-}" || -z "${GATEWAY_IP:-}" ) ]]; then
        echo "WARNING: HINDSIGHT_ENABLED=1 but the agent token or gateway IP is missing; hermes runs without shared memory." >&2
    elif [[ "${HINDSIGHT_ENABLED:-0}" == "1" ]]; then
        echo "Wiring the Hindsight memory provider into the hermes container..."
        cat >> "${MOUNT_POINT}/opt/hermes/data/config.yaml" <<EOF
memory:
  provider: hindsight
EOF
        mkdir -p "${MOUNT_POINT}/opt/hermes/data/hindsight"
        # Written with python so the values are JSON-escaped, and readable only
        # by the container's user: the file holds the agent's token.
        HS_CONF="${MOUNT_POINT}/opt/hermes/data/hindsight/config.json" \
        HS_URL="http://${GATEWAY_IP}:${HINDSIGHT_PORT:-8890}" \
        HS_TOKEN="${HINDSIGHT_TOKEN}" HS_BANK="${HINDSIGHT_BANK:-agent-sandbox}" HS_AGENT="${AGENT_TYPE}" \
        python3 - <<'HSCONF'
import json, os
conf = {
    "mode": "local_external",
    "api_url": os.environ["HS_URL"],
    "api_key": os.environ["HS_TOKEN"],
    "bank_id": os.environ["HS_BANK"],
    # Recall and tools injected by the provider; retention in the background.
    "memory_mode": "hybrid",
    "auto_recall": True,
    "auto_retain": True,
    "retain_async": True,
    "recall_budget": "low",
    "recall_max_tokens": 1024,
    # All three layers below the mental models, not only observations (the
    # provider's default): facts are useful before they have been consolidated.
    "recall_types": "observation,world,experience",
    "retain_tags": ["agent:" + os.environ["HS_AGENT"]],
    "retain_context": "conversation with the " + os.environ["HS_AGENT"] + " agent",
}
path = os.environ["HS_CONF"]
fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
with os.fdopen(fd, "w") as f:
    json.dump(conf, f, indent=2)
    f.write("\n")
HSCONF
        # The container's entrypoint drops to its `hermes` user (uid 10000 unless
        # remapped) and only chowns the top level of the data dir, so a root-owned
        # 0600 file here is unreadable to the provider and it stays inactive.
        chown -R "${HERMES_CONTAINER_UID:-10000}:${HERMES_CONTAINER_GID:-${HERMES_CONTAINER_UID:-10000}}" \
            "${MOUNT_POINT}/opt/hermes/data/hindsight"
    elif [[ "${MNEMOSYNE_ENABLED:-0}" == "1" && -z "${MNEMOSYNE_TOKEN:-}" ]]; then
        echo "WARNING: MNEMOSYNE_ENABLED=1 but MNEMOSYNE_TOKEN is empty; skipping mcp block (would 401)." >&2
    elif [[ "${MNEMOSYNE_ENABLED:-0}" == "1" ]]; then
        echo "Wiring mnemosyne MCP server into hermes config.yaml..."
        cat >> "${MOUNT_POINT}/opt/hermes/data/config.yaml" <<EOF
mcp:
  servers:
    mnemosyne:
      url: http://mnemosyne.host:${MNEMOSYNE_PORT:-8077}/sse
      transport: sse
      headers: { Authorization: "Bearer ${MNEMOSYNE_TOKEN}" }
EOF
    fi

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
