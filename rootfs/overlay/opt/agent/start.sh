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

# Install Python dependencies if not already done
AGENT_DIR="/opt/agent"
if [[ -f "${AGENT_DIR}/requirements.txt" ]]; then
    if ! python3 -c "import deepagents" 2>/dev/null; then
        log "Installing agent runtime dependencies..."
        pip3 install -r "${AGENT_DIR}/requirements.txt" 2>&1 | tee -a "${LOG}"
    fi
fi

# Install playwright browsers if needed
if python3 -c "import playwright" 2>/dev/null; then
    if [[ ! -d /home/agent/.cache/ms-playwright ]]; then
        log "Installing Playwright browsers..."
        python3 -m playwright install chromium 2>&1 | tee -a "${LOG}"
    fi
fi

# Run the agent
log "Launching DeepAgents runtime..."
exec python3 "${AGENT_DIR}/agent.py" 2>&1 | tee -a "${LOG}"
