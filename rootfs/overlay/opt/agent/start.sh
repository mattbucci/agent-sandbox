#!/usr/bin/env bash
# Agent start script — runs inside the VM as the 'agent' user
# Reads config from /etc/agent.conf and launches the DeepAgents runtime
set -euo pipefail

LOG="/var/log/agent.log"

log() {
    echo "[agent] $(date '+%Y-%m-%d %H:%M:%S') $*" | tee -a "${LOG}"
}

# Source configuration
if [[ -f /etc/agent.conf ]]; then
    set -a
    source /etc/agent.conf
    set +a
fi

# Defaults
LLM_API_BASE="${LLM_API_BASE:-http://localhost:4000/v1}"
LLM_API_KEY="${LLM_API_KEY:-sk-default}"
LLM_MODEL="${LLM_MODEL:-default-model}"
AGENT_TYPE="${AGENT_TYPE:-generic}"
AGENT_NAME="${AGENT_NAME:-Agent}"

# Gateway server (in-VM OpenAI-compatible server) settings
GATEWAY_ENABLED="${GATEWAY_ENABLED:-1}"
GATEWAY_PORT="${GATEWAY_PORT:-8642}"
API_SERVER_KEY="${API_SERVER_KEY:-}"

# Harness selects the in-VM backend: "deepagents" (default) or "hermes"
HARNESS="${HARNESS:-deepagents}"

export LLM_API_BASE LLM_API_KEY LLM_MODEL AGENT_TYPE AGENT_NAME
export GATEWAY_ENABLED GATEWAY_PORT API_SERVER_KEY HARNESS
export OPENAI_API_KEY="${LLM_API_KEY}"
export OPENAI_API_BASE="${LLM_API_BASE}"

# OTel tracing — collector runs on host (gateway IP)
GATEWAY_IP=$(ip route | grep default | awk '{print $3}')
export OTEL_EXPORTER_OTLP_ENDPOINT="${OTEL_EXPORTER_OTLP_ENDPOINT:-http://${GATEWAY_IP}:4318}"
export OTEL_SERVICE_NAME="agent-sandbox-${AGENT_TYPE}"

log "Starting ${AGENT_NAME} (type: ${AGENT_TYPE})"
log "LLM endpoint: ${LLM_API_BASE}"
log "Model: ${LLM_MODEL}"
log "Harness: ${HARNESS}"

# Harness dispatch — the hermes backend runs a pre-baked Docker container
# instead of the DeepAgents Python runtime. Branch before any venv setup.
if [[ "${HARNESS}" == "hermes" ]]; then
    log "Dispatching to hermes harness (/opt/agent/run-hermes.sh)..."
    exec /opt/agent/run-hermes.sh
fi

# Ensure workspace exists
mkdir -p /home/agent/workspace /home/agent/tasks
cd /home/agent/workspace

# Agent virtual environment
AGENT_DIR="/opt/agent"
VENV="${AGENT_DIR}/.venv"
PYTHON="${VENV}/bin/python"

# Install Python dependencies if venv doesn't exist
if [[ ! -f "${PYTHON}" ]]; then
    log "Creating agent venv with uv..."
    uv venv --python python3.12 "${VENV}" 2>&1 | tee -a "${LOG}"
    uv pip install --python "${PYTHON}" -r "${AGENT_DIR}/requirements.txt" 2>&1 | tee -a "${LOG}"
fi

# Run the agent
if [[ "${GATEWAY_ENABLED}" == "1" ]]; then
    log "Launching in-VM gateway server on 0.0.0.0:${GATEWAY_PORT} (auth: $([[ -n "${API_SERVER_KEY}" ]] && echo on || echo off))..."
    exec "${PYTHON}" "${AGENT_DIR}/gateway_server.py" 2>&1 | tee -a "${LOG}"
else
    log "Launching DeepAgents file-watch runtime (legacy)..."
    exec "${PYTHON}" "${AGENT_DIR}/agent.py" 2>&1 | tee -a "${LOG}"
fi
