#!/usr/bin/env bash
# =============================================================================
# agent-sandbox-boot.sh — Called by systemd on boot to restore sandbox state
#
# Restores:
#   1. IP forwarding + the base nftables ruleset
#   2. Squid proxy and dnsmasq
#   3. Every VM that was running before shutdown (from state/vms/), through the
#      same lib/vm.sh path a fresh launch uses (jailer, compiled egress ACL)
#   4. The host services VMs and clients depend on: mnemosyne and the gateway
#
# Safe to re-run on a live host: running VMs are left alone and only get their
# firewall rules re-applied.
# =============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$(readlink -f "$0")")" && pwd)"
SANDBOX_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
export SANDBOX_ROOT

source "${SANDBOX_ROOT}/lib/common.sh"
ensure_global_compiled
source "${SANDBOX_ROOT}/lib/network.sh"
source "${SANDBOX_ROOT}/lib/vm.sh"

LOG="/var/log/agent-sandbox-boot.log"
exec > >(tee -a "${LOG}") 2>&1

log_info "=== Agent Sandbox boot service starting ==="
log_info "$(date -Iseconds)"

# --- 1. IP forwarding + base firewall ---
sysctl -w net.ipv4.ip_forward=1
apply_base_nftables "${HOST_IFACE:?network.host_iface is not set in config/sandbox.yaml}"

# --- 2. Squid + dnsmasq ---
# Squid's config is installed by network/setup-host-network.sh; only make sure
# the ACL placeholders exist so it can start before any VM is restored.
mkdir -p /etc/squid/acls
[[ -f /etc/squid/acls/vm-acls.conf ]] || echo "# No VMs yet" > /etc/squid/acls/vm-acls.conf
[[ -f /etc/squid/acls/all-allowed-domains.txt ]] || echo "# No VMs yet" > /etc/squid/acls/all-allowed-domains.txt
systemctl start squid 2>/dev/null || /usr/sbin/squid &
log_info "Squid started."

systemctl start dnsmasq 2>/dev/null || /usr/sbin/dnsmasq &
log_info "dnsmasq started."

# The per-VM LLM passthrough rule needs the LLM host's address. This service
# can run before DNS is reachable, so give resolution a chance first; VMs fall
# back to the address recorded at launch if it still fails.
LLM_HOST="$(llm_host_from_config)"
if [[ -n "${LLM_HOST}" ]]; then
    if LLM_IP=$(resolve_ipv4_wait "${LLM_HOST}" 60); then
        log_info "LLM host ${LLM_HOST} -> ${LLM_IP}"
    else
        log_warn "LLM host ${LLM_HOST} did not resolve within 60s; using per-VM recorded addresses"
    fi
fi

# --- 3. Restore VMs from saved state ---
# Best-effort: the firewall (the essential persistence) is already applied, so
# one VM failing to restore must not fail the service or the VMs after it.
RESTORED=0
FAILED=0
for info_file in "${STATE_DIR}"/vms/*/info.json; do
    [[ -f "${info_file}" ]] || continue
    if ( vm_restore "${info_file}" ); then
        RESTORED=$((RESTORED + 1))
    else
        FAILED=$((FAILED + 1))
    fi
done

# Reload Squid with the regenerated ACLs
squid -k reconfigure 2>/dev/null || true

# --- 4. Host services ---
# Both are optional and best-effort. Pidfiles from before the reboot are stale
# (and their PIDs may have been reused), so drop any that do not point at the
# right process before asking sandbox-ctl to start.
drop_stale_pidfile() {
    local pidfile="$1" expect="$2" pid
    [[ -f "${pidfile}" ]] || return 0
    pid=$(cat "${pidfile}" 2>/dev/null || true)
    if [[ "${pid}" =~ ^[0-9]+$ ]] && grep -qa "${expect}" "/proc/${pid}/cmdline" 2>/dev/null; then
        return 0
    fi
    rm -f "${pidfile}"
}

if [[ "${MEMORY_ENABLED:-0}" == "1" && "${MEMORY_BACKEND:-mnemosyne}" == "hindsight" \
      && -x "${STATE_DIR}/hindsight/venv/bin/hindsight-api" ]]; then
    "${SANDBOX_ROOT}/bin/sandbox-ctl" memory start \
        || log_warn "Hindsight failed to start — see ${LOG_DIR}/hindsight.log"
elif [[ "${MEMORY_ENABLED:-0}" == "1" && -x "${STATE_DIR}/mnemosyne/venv/bin/mnemosyne" ]]; then
    drop_stale_pidfile "${STATE_DIR}/mnemosyne/mnemosyne.pid" mnemosyne
    "${SANDBOX_ROOT}/bin/sandbox-ctl" mnemosyne start \
        || log_warn "mnemosyne failed to start — see ${LOG_DIR}/mnemosyne.log"
fi

# The gateway is restored only if it was set up on this host before (config
# compiled + binary built); a host that never ran it stays that way.
if [[ -f "${STATE_DIR}/gateway/gateway.json" && -x "${STATE_DIR}/gateway/hermes-gateway" ]]; then
    drop_stale_pidfile "${STATE_DIR}/gateway/gateway.pid" hermes-gateway
    "${SANDBOX_ROOT}/bin/sandbox-ctl" gateway start \
        || log_warn "gateway router failed to start — see ${LOG_DIR}/gateway.log"
fi

log_info "=== Boot complete: ${RESTORED} VM(s) up, ${FAILED} failed ==="
