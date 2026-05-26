#!/usr/bin/env bash
# =============================================================================
# agent-sandbox-boot.sh — Called by systemd on boot to restore sandbox state
#
# Restores:
#   1. IP forwarding + nftables rules
#   2. Squid proxy
#   3. dnsmasq
#   4. Any VMs that were running before shutdown (from state/vms/)
# =============================================================================
set -euo pipefail

SANDBOX_ROOT="/home/letsrtfm/AI/agent-sandbox"
export SANDBOX_ROOT

source "${SANDBOX_ROOT}/lib/common.sh"
ensure_global_compiled
source "${SANDBOX_ROOT}/lib/common.sh"
source "${SANDBOX_ROOT}/lib/network.sh"

LOG="/var/log/agent-sandbox-boot.log"
exec > >(tee -a "${LOG}") 2>&1

log_info "=== Agent Sandbox boot service starting ==="
log_info "$(date -Iseconds)"

# --- 1. IP forwarding ---
sysctl -w net.ipv4.ip_forward=1

# --- 2. nftables base rules ---
nft delete table ip vm_filter 2>/dev/null || true

HOST_IFACE="${HOST_IFACE:-enp12s0}"

nft -f - <<NFT
table ip vm_filter {
    chain input {
        type filter hook input priority 0; policy accept;
        # VMs reach ONLY Squid/DNS/OTel on the host; drop everything else.
        iifname "tap-vm*" ct state established,related accept
        iifname "tap-vm*" udp dport 53 accept
        iifname "tap-vm*" tcp dport { 3128, 3129 } accept
        iifname "tap-vm*" tcp dport { 4317, 4318 } accept
        iifname "tap-vm*" drop
    }

    chain forward {
        type filter hook forward priority 0; policy drop;
        ct state established,related accept
    }
    chain prerouting {
        type nat hook prerouting priority -100;
    }
    chain postrouting {
        type nat hook postrouting priority 100;
        oifname "${HOST_IFACE}" masquerade
    }
}
NFT

# --- 3. Start Squid ---
# Ensure Squid config and ACL placeholders are in place
mkdir -p /etc/squid/acls
cp "${SANDBOX_ROOT}/network/squid/squid-base.conf" /etc/squid/squid.conf
[[ -f /etc/squid/acls/vm-acls.conf ]] || echo "# No VMs yet" > /etc/squid/acls/vm-acls.conf
[[ -f /etc/squid/acls/all-allowed-domains.txt ]] || echo "# No VMs yet" > /etc/squid/acls/all-allowed-domains.txt
systemctl start squid 2>/dev/null || /usr/sbin/squid &
log_info "Squid started."

# --- 4. Start dnsmasq ---
systemctl start dnsmasq 2>/dev/null || /usr/sbin/dnsmasq &
log_info "dnsmasq started."

# --- 5. Relaunch VMs from saved state ---
if [[ ! -d "${STATE_DIR}/vms" ]]; then
    log_info "No VMs to restore."
    exit 0
fi

RESTORED=0
# VM restore is best-effort: networking (the essential persistence) is already
# applied above, so a single VM failing to restore must not fail the service.
set +e
for info_file in "${STATE_DIR}"/vms/*/info.json; do
    [[ -f "${info_file}" ]] || continue

    instance_id=$(jq -r '.instance_id' "${info_file}")
    agent_type=$(jq -r '.agent_type' "${info_file}")
    slot=$(jq -r '.slot' "${info_file}")
    vm_ip=$(jq -r '.vm_ip' "${info_file}")
    gateway_ip=$(jq -r '.gateway_ip' "${info_file}")
    tap_name=$(jq -r '.tap_device' "${info_file}")
    vcpus=$(jq -r '.vcpus' "${info_file}")
    mem_mb=$(jq -r '.mem_mb' "${info_file}")
    guest_mac=$(jq -r '.guest_mac' "${info_file}")
    novnc_port=$(jq -r '.novnc_port' "${info_file}")
    vm_state_dir="$(dirname "${info_file}")"

    # Check if rootfs still exists
    if [[ ! -f "${vm_state_dir}/rootfs.ext4" ]]; then
        log_warn "Skipping ${instance_id}: rootfs missing"
        continue
    fi

    log_info "Restoring VM: ${instance_id} (${agent_type}, slot ${slot})"

    # Recreate TAP device
    create_tap "${tap_name}" "${gateway_ip}"

    # Add nftables rules
    add_vm_nftables "${slot}" "${tap_name}" "${gateway_ip}"

    # Regenerate Squid ACL if allowlist exists
    ALLOWLIST_FILE="${SANDBOX_ROOT}/rootfs/agents/${agent_type}/allowlist.txt"
    if [[ -f "${ALLOWLIST_FILE}" ]]; then
        "${SANDBOX_ROOT}/network/squid/gen-acl.sh" "${slot}" "${agent_type}" "${ALLOWLIST_FILE}" || true
    fi

    # Generate Firecracker config
    FC_CONFIG="${vm_state_dir}/vm-config.json"
    FC_SOCKET="${vm_state_dir}/firecracker.sock"
    LOG_FILE="${LOG_DIR}/${instance_id}.log"

    rm -f "${FC_SOCKET}"

    sed \
        -e "s|__KERNEL_PATH__|${KERNEL_PATH}|g" \
        -e "s|__ROOTFS_PATH__|${vm_state_dir}/rootfs.ext4|g" \
        -e "s|__VCPUS__|${vcpus}|g" \
        -e "s|__MEM_MB__|${mem_mb}|g" \
        -e "s|__TAP_NAME__|${tap_name}|g" \
        -e "s|__GUEST_MAC__|${guest_mac}|g" \
        -e "s|__VM_IP__|${vm_ip}|g" \
        -e "s|__GATEWAY_IP__|${gateway_ip}|g" \
        -e "s|__DNS_IP__|${gateway_ip}|g" \
        -e "s|__HOSTNAME__|${instance_id}|g" \
        -e "s|__NO_AGENT__|0|g" \
        -e "s|__LOG_PATH__|${LOG_FILE}|g" \
        -e "s|__METRICS_PATH__|${vm_state_dir}/metrics.fifo|g" \
        "${SANDBOX_ROOT}/vm/config-template.json" > "${FC_CONFIG}"

    # Launch Firecracker
    mkfifo "${vm_state_dir}/metrics.fifo" 2>/dev/null || true
    /usr/local/bin/firecracker \
        --api-sock "${FC_SOCKET}" \
        --config-file "${FC_CONFIG}" \
        &>"${LOG_FILE}.stdout" &
    FC_PID=$!

    # Update PID in state
    jq --argjson pid "${FC_PID}" '.firecracker_pid = $pid' "${info_file}" > "${info_file}.tmp"
    mv "${info_file}.tmp" "${info_file}"

    # Start websockify
    websockify --web /opt/novnc 0.0.0.0:${novnc_port} ${vm_ip}:5900 &>/dev/null &
    WS_PID=$!
    jq --argjson pid "${WS_PID}" '.websockify_pid = $pid' "${info_file}" > "${info_file}.tmp"
    mv "${info_file}.tmp" "${info_file}"

    RESTORED=$((RESTORED + 1))
    log_info "  ${instance_id} restored (FC PID ${FC_PID}, noVNC :${novnc_port})"
done
set -e  # re-enable after best-effort restore loop

# Reload Squid with new ACLs
squid -k reconfigure 2>/dev/null || true

log_info "=== Boot complete: ${RESTORED} VMs restored ==="
