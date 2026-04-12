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

export LLM_API_BASE LLM_API_KEY LLM_MODEL AGENT_TYPE AGENT_NAME
export OPENAI_API_KEY="${LLM_API_KEY}"
export OPENAI_API_BASE="${LLM_API_BASE}"

log "Starting ${AGENT_NAME} (type: ${AGENT_TYPE})"
log "LLM endpoint: ${LLM_API_BASE}"
log "Model: ${LLM_MODEL}"

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
log "Launching DeepAgents runtime..."
exec "${PYTHON}" "${AGENT_DIR}/agent.py" 2>&1 | tee -a "${LOG}"
