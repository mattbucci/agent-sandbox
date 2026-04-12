#!/usr/bin/env bash
# =============================================================================
# lib/common.sh — Shared functions for agent-sandbox
# =============================================================================

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

log_info()  { echo -e "${BLUE}[INFO]${NC} $*"; }
log_ok()    { echo -e "${GREEN}[OK]${NC} $*"; }
log_warn()  { echo -e "${YELLOW}[WARN]${NC} $*"; }
log_error() { echo -e "${RED}[ERROR]${NC} $*" >&2; }

# --- Slot allocation ---
# Slots are 0-254. A slot is "taken" if state/vms/*/info.json references it.

SLOTS_FILE="${STATE_DIR}/.slots"

allocate_slot() {
    mkdir -p "${STATE_DIR}"
    touch "${SLOTS_FILE}"

    # Find used slots
    local used_slots=()
    if [[ -d "${STATE_DIR}/vms" ]]; then
        for info in "${STATE_DIR}"/vms/*/info.json; do
            [[ -f "${info}" ]] || continue
            local s
            s=$(jq -r '.slot' "${info}" 2>/dev/null)
            [[ -n "${s}" ]] && used_slots+=("${s}")
        done
    fi

    # Find first free slot
    for slot in $(seq 0 254); do
        local taken=0
        for used in "${used_slots[@]+"${used_slots[@]}"}"; do
            if [[ "${slot}" == "${used}" ]]; then
                taken=1
                break
            fi
        done
        if [[ ${taken} -eq 0 ]]; then
            echo "${slot}"
            return 0
        fi
    done

    log_error "No free slots available (max 255 VMs)"
    return 1
}

release_slot() {
    local slot="$1"
    # Slot is implicitly released when VM state dir is removed
    # This function is a placeholder for any future cleanup
    :
}

# --- Agent type validation ---
list_agent_types() {
    ls "${SANDBOX_ROOT}/config/agents/" 2>/dev/null | sed 's/.conf$//' | sort
}

validate_agent_type() {
    local agent_type="$1"
    if [[ ! -f "${SANDBOX_ROOT}/config/agents/${agent_type}.conf" ]]; then
        log_error "Unknown agent type: ${agent_type}"
        echo "Available types:"
        list_agent_types | sed 's/^/  /'
        return 1
    fi
}

# --- VM listing ---
list_running_vms() {
    if [[ ! -d "${STATE_DIR}/vms" ]]; then
        return
    fi
    for info in "${STATE_DIR}"/vms/*/info.json; do
        [[ -f "${info}" ]] || continue
        local pid
        pid=$(jq -r '.firecracker_pid' "${info}" 2>/dev/null)
        if [[ -n "${pid}" ]] && kill -0 "${pid}" 2>/dev/null; then
            echo "${info}"
        fi
    done
}

get_vm_info() {
    local instance_id="$1"
    local info_file="${STATE_DIR}/vms/${instance_id}/info.json"
    if [[ -f "${info_file}" ]]; then
        cat "${info_file}"
    else
        return 1
    fi
}

# Find VM by name or instance ID
resolve_vm() {
    local query="$1"

    # Direct match by instance ID
    if [[ -f "${STATE_DIR}/vms/${query}/info.json" ]]; then
        echo "${query}"
        return 0
    fi

    # Search by name
    for info in "${STATE_DIR}"/vms/*/info.json; do
        [[ -f "${info}" ]] || continue
        local name
        name=$(jq -r '.name' "${info}" 2>/dev/null)
        if [[ "${name}" == "${query}" ]]; then
            jq -r '.instance_id' "${info}"
            return 0
        fi
    done

    # Search by agent type (return first match)
    for info in "${STATE_DIR}"/vms/*/info.json; do
        [[ -f "${info}" ]] || continue
        local atype
        atype=$(jq -r '.agent_type' "${info}" 2>/dev/null)
        if [[ "${atype}" == "${query}" ]]; then
            jq -r '.instance_id' "${info}"
            return 0
        fi
    done

    log_error "VM not found: ${query}"
    return 1
}

require_root() {
    if [[ $EUID -ne 0 ]]; then
        log_error "This command must be run as root. Use: sudo sandbox-ctl $*"
        exit 1
    fi
}
