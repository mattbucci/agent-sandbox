#!/usr/bin/env bash
# =============================================================================
# lib/common.sh — Shared functions for agent-sandbox
# =============================================================================

# Ensure sbin dirs are on PATH. Under `sudo` with a restrictive secure_path
# (common on Arch), /usr/sbin may be absent, which breaks chroot commands like
# locale-gen and useradd that live in /usr/sbin inside the (Debian/Ubuntu) guest.
case ":${PATH}:" in
    *:/usr/sbin:*) ;;
    *) export PATH="${PATH}:/usr/local/sbin:/usr/sbin:/sbin" ;;
esac

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

allocate_slot() {
    local SLOTS_FILE="${STATE_DIR}/.slots"
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
# One agent type per <type>.yaml in config/agents/ (published) or
# config/local/agents/ (private, gitignored; see docs/local-config.md). A local
# file with a published agent's name is laid over it by agentconf.py.
local_config_dir() {
    echo "${SANDBOX_LOCAL_CONFIG:-${SANDBOX_ROOT}/config/local}"
}

list_agent_types() {
    local f
    for f in "${SANDBOX_ROOT}"/config/agents/*.yaml "$(local_config_dir)"/agents/*.yaml; do
        [[ -f "${f}" ]] || continue
        basename "${f}" .yaml
    done | sort -u
}

agent_type_exists() {
    [[ -f "${SANDBOX_ROOT}/config/agents/$1.yaml" || -f "$(local_config_dir)/agents/$1.yaml" ]]
}

validate_agent_type() {
    local agent_type="$1"
    if ! agent_type_exists "${agent_type}"; then
        log_error "Unknown agent type: ${agent_type}"
        echo "Available types:"
        list_agent_types | sed 's/^/  /'
        return 1
    fi
}

# Ensure agent is compiled (call agentconf.py compile if build/ is stale)
ensure_compiled() {
    local agent_type="$1"
    local build_dir="${SANDBOX_ROOT}/build/${agent_type}"
    local stale="" src

    # Recompile if the build dir is missing, or anything the compiled output is
    # made from is newer than it: the agent's YAML (published or local), the
    # presets and install scripts it may pull in, and the pinned versions
    # (inlined into customize.sh).
    if [[ ! -f "${build_dir}/agent.conf" ]]; then
        stale=1
    else
        for src in "${SANDBOX_ROOT}/config/agents/${agent_type}.yaml" \
                   "$(local_config_dir)/agents/${agent_type}.yaml" \
                   "${SANDBOX_ROOT}/config/versions.env" "${SANDBOX_ROOT}/lib/versions.sh"; do
            [[ "${src}" -nt "${build_dir}/agent.conf" ]] && stale=1
        done
        for src in "${SANDBOX_ROOT}/config/presets" "${SANDBOX_ROOT}/config/install-scripts" \
                   "$(local_config_dir)/presets" "$(local_config_dir)/install-scripts"; do
            [[ -d "${src}" ]] || continue
            [[ -n "$(find "${src}" -type f -newer "${build_dir}/agent.conf" -print -quit)" ]] && stale=1
        done
    fi
    if [[ -n "${stale}" ]]; then
        log_info "Compiling agent config: ${agent_type}"
        python3 "${SANDBOX_ROOT}/lib/agentconf.py" compile "${agent_type}"
    fi
}

ensure_global_compiled() {
    local build_conf="${SANDBOX_ROOT}/build/sandbox.conf"
    local global_yaml="${SANDBOX_ROOT}/config/sandbox.yaml"

    if [[ -f "${global_yaml}" ]]; then
        if [[ ! -f "${build_conf}" ]] || [[ "${global_yaml}" -nt "${build_conf}" ]] \
            || [[ "$(local_config_dir)/sandbox.yaml" -nt "${build_conf}" ]]; then
            python3 "${SANDBOX_ROOT}/lib/agentconf.py" compile-global 2>/dev/null
        fi
    fi

    # Source the compiled config if it exists, otherwise set defaults
    if [[ -f "${build_conf}" ]]; then
        source "${build_conf}"
        # Try a Firecracker build without installing it system-wide, e.g.
        #   sudo SANDBOX_FIRECRACKER_BIN=/tmp/fc/firecracker \
        #        SANDBOX_JAILER_BIN=/tmp/fc/jailer bin/sandbox-ctl launch coder
        FIRECRACKER_BIN="${SANDBOX_FIRECRACKER_BIN:-${FIRECRACKER_BIN}}"
        JAILER_BIN="${SANDBOX_JAILER_BIN:-${JAILER_BIN}}"
    else
        # Minimal defaults so scripts don't crash during bootstrap
        STATE_DIR="${SANDBOX_ROOT}/state"
        LOG_DIR="${SANDBOX_ROOT}/state/logs"
    fi
}

# pid_alive <pid> — is the process running? Unlike `kill -0`, this also works
# for a non-root caller looking at another user's process (jailed VMs run as
# per-VM fc-N users), so `sandbox-ctl status` is right without sudo.
pid_alive() {
    [[ "${1:-}" =~ ^[0-9]+$ ]] && [[ -d "/proc/$1" ]]
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
        if pid_alive "${pid}"; then
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
