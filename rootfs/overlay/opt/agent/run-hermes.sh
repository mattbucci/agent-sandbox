#!/usr/bin/env bash
# run-hermes.sh — hermes harness runner (inside the VM, as the 'agent' user)
#
# Loads and runs the pre-baked NousResearch/hermes-agent container, exposing its
# OpenAI-compatible API on the VM's 0.0.0.0:${GATEWAY_PORT} via --network host so
# the host Go router can reach it at <vm_ip>:8642. Config (LLM endpoint, docker
# daemon.json) is injected into the rootfs by vm/prepare-rootfs.sh at launch.
set -euo pipefail

LOG="/var/log/agent.log"

log() {
    echo "[hermes] $(date '+%Y-%m-%d %H:%M:%S') $*" | tee -a "${LOG}"
}

# Pinned image — 'latest' is NOT this build. Pre-baked tar is docker-loaded below.
HERMES_IMAGE="nousresearch/hermes-agent:v2026.6.19"
HERMES_TAR="/opt/hermes/images/hermes-agent-v2026.6.19.tar"
HERMES_DATA="/opt/hermes/data"

# Defaults (start.sh exports these, but be self-contained if run directly).
GATEWAY_PORT="${GATEWAY_PORT:-8642}"
API_SERVER_KEY="${API_SERVER_KEY:-}"

# --- Wait for dockerd (agent-init starts it after start.sh is backgrounded) ---
log "Waiting for /var/run/docker.sock..."
i=0
while [ "${i}" -lt 60 ]; do
    [ -S /var/run/docker.sock ] && break
    i=$((i + 1))
    sleep 1
done
if [ ! -S /var/run/docker.sock ]; then
    log "ERROR: dockerd socket never appeared (see /var/log/dockerd.log); cannot start hermes"
    exit 1
fi
log "dockerd is up."

# --- Load the pre-baked image, then reclaim the tar's space ---
if [[ -f "${HERMES_TAR}" ]]; then
    # Verify there's room to load the image alongside the still-present tar.
    # docker load writes the decompressed image into /var/lib/docker while the
    # tar remains on the same filesystem (deleted only after load completes), so
    # peak usage needs roughly twice the tar size of free space. If the rootfs
    # was not grown enough (config/sandbox.yaml rootfs.per_agent.hermes), fail
    # loudly here instead of producing a half-loaded image / OOD-space container.
    tar_kb=$(du -k "${HERMES_TAR}" | cut -f1)
    free_kb=$(df -Pk /var/lib/docker | awk 'NR==2 {print $4}')
    need_kb=$((tar_kb * 2))
    if [ "${free_kb}" -lt "${need_kb}" ]; then
        log "ERROR: insufficient free space to load ${HERMES_IMAGE}: $((free_kb / 1024))MB free, need ~$((need_kb / 1024))MB (tar is $((tar_kb / 1024))MB). Increase rootfs.per_agent.hermes in config/sandbox.yaml and rebuild the hermes rootfs."
        exit 1
    fi
    log "Loading pre-baked image from ${HERMES_TAR} ($((tar_kb / 1024))MB; $((free_kb / 1024))MB free)..."
    docker load -i "${HERMES_TAR}" 2>&1 | tee -a "${LOG}"
    # The tar and its parent /opt/hermes/images are baked root:root (mode 755) by
    # cmd_build_agent with no chown, so the unprivileged 'agent' user (which runs
    # this script) lacks write on the directory and a plain `rm` fails with EACCES.
    # Under `set -e` that would abort the script *before* `docker run` below, so the
    # container would never start. Remove it as root via the agent's NOPASSWD sudo
    # (granted in rootfs/build-base.sh) to reclaim the tar's space on the rootfs.
    sudo rm -f "${HERMES_TAR}"
    log "Image loaded; removed tar to reclaim space."
else
    log "WARNING: pre-baked image tar not found at ${HERMES_TAR} — expecting ${HERMES_IMAGE} to already be present (docker run will fail if it is not)."
fi

# --- Ensure the container's data dir exists (config.yaml lives here) ---
mkdir -p "${HERMES_DATA}"

# Map the internal LLM endpoint for the container. Even with --network host,
# Docker gives the container its own /etc/hosts, so the VM's /etc/hosts entry is
# NOT inherited — pass it explicitly. LLM_HOST/LLM_HOST_IP come from
# /etc/agent.conf (resolved host-side by prepare-rootfs) via start.sh's env.
ADD_HOST=()
if [[ -n "${LLM_HOST:-}" && -n "${LLM_HOST_IP:-}" && "${LLM_HOST}" != "${LLM_HOST_IP}" ]]; then
    ADD_HOST=(--add-host "${LLM_HOST}:${LLM_HOST_IP}")
    log "Mapping ${LLM_HOST} -> ${LLM_HOST_IP} for the container."
fi

# --- Run the container ---
# --network host => binds the VM's 0.0.0.0:${GATEWAY_PORT} directly (avoids the
# docker0 bridge / iptables 'raw' table problem on the stock guest kernel).
# API_SERVER_MODEL_NAME is the configured LLM model: the host router rewrites the
# request 'model' (the agent id "hermes") to this value before forwarding, so the
# API server accepts it and inference uses the model in /opt/hermes/data/config.yaml.
log "Starting hermes container on 0.0.0.0:${GATEWAY_PORT} (auth: $([[ -n "${API_SERVER_KEY}" ]] && echo on || echo off))..."
exec docker run --rm --name hermes-agent --network host \
    "${ADD_HOST[@]}" \
    -e API_SERVER_ENABLED=true \
    -e API_SERVER_HOST=0.0.0.0 \
    -e API_SERVER_PORT="${GATEWAY_PORT}" \
    -e API_SERVER_KEY="${API_SERVER_KEY}" \
    -e API_SERVER_MODEL_NAME="${LLM_MODEL:-gemma}" \
    -v "${HERMES_DATA}:/opt/data" \
    "${HERMES_IMAGE}" gateway run 2>&1 | tee -a "${LOG}"
