#!/usr/bin/env bash
# =============================================================================
# lib/network.sh — Network helper functions for agent-sandbox
# =============================================================================

# Create a TAP device for a VM
# Usage: create_tap <tap-name> <host-ip>
create_tap() {
    local tap_name="$1"
    local host_ip="$2"

    # Remove existing TAP if present
    ip link del "${tap_name}" 2>/dev/null || true

    ip tuntap add dev "${tap_name}" mode tap
    ip addr add "${host_ip}/24" dev "${tap_name}"
    ip link set "${tap_name}" up

    # Add this address to dnsmasq listen list
    # dnsmasq with bind-dynamic will pick up new interfaces
    # Just ensure it can bind to this address
}

# Remove a TAP device
# Usage: remove_tap <tap-name>
remove_tap() {
    local tap_name="$1"
    ip link set "${tap_name}" down 2>/dev/null || true
    ip tuntap del dev "${tap_name}" mode tap 2>/dev/null || true
}

# Add nftables rules for a VM
# Usage: add_vm_nftables <slot> <tap-name> <gateway-ip>
add_vm_nftables() {
    local slot="$1"
    local tap_name="$2"
    local gateway_ip="$3"

    local squid_http="${SQUID_HTTP_PORT:-3128}"
    local squid_https="${SQUID_HTTPS_PORT:-3129}"

    # Extract LiteLLM host IP for direct access rule
    local llm_host=""
    local llm_port=""
    if [[ -n "${LLM_API_BASE:-}" ]]; then
        llm_host=$(echo "${LLM_API_BASE}" | sed -E 's|https?://||; s|:[0-9]+.*||; s|/.*||')
        llm_port=$(echo "${LLM_API_BASE}" | grep -oP ':\K[0-9]+' | head -1)
        llm_port="${llm_port:-443}"
    fi

    # Transparent proxy redirect: VM HTTP/HTTPS → Squid
    nft add rule ip vm_filter prerouting \
        iifname "${tap_name}" tcp dport 443 dnat to "${gateway_ip}:${squid_https}" \
        comment "\"vm${slot}-https-redirect\""

    nft add rule ip vm_filter prerouting \
        iifname "${tap_name}" tcp dport 80 dnat to "${gateway_ip}:${squid_http}" \
        comment "\"vm${slot}-http-redirect\""

    # Allow VM → host Squid
    nft add rule ip vm_filter forward \
        iifname "${tap_name}" ip daddr "${gateway_ip}" tcp dport "{${squid_http},${squid_https}}" accept \
        comment "\"vm${slot}-squid\""

    # Allow VM → host DNS
    nft add rule ip vm_filter forward \
        iifname "${tap_name}" ip daddr "${gateway_ip}" udp dport 53 accept \
        comment "\"vm${slot}-dns\""

    # Allow VM → host OTel collector (HTTP 4318, gRPC 4317)
    nft add rule ip vm_filter forward \
        iifname "${tap_name}" ip daddr "${gateway_ip}" tcp dport "{4317,4318}" accept \
        comment "\"vm${slot}-otel\""

    # Allow VM → LiteLLM server (direct, bypasses Squid)
    if [[ -n "${llm_host}" ]]; then
        nft add rule ip vm_filter prerouting \
            iifname "${tap_name}" ip daddr "${llm_host}" tcp dport "${llm_port}" accept \
            comment "\"vm${slot}-llm-passthrough\""
        nft add rule ip vm_filter forward \
            iifname "${tap_name}" ip daddr "${llm_host}" tcp dport "${llm_port}" accept \
            comment "\"vm${slot}-llm\""
    fi

    # Allow Squid outbound to internet on behalf of VMs
    # (This is a general rule — Squid's ACLs handle per-VM filtering)
    nft add rule ip vm_filter forward \
        oifname "${HOST_IFACE}" ct state new,established accept \
        comment "\"squid-outbound\"" 2>/dev/null || true
}

# Remove nftables rules for a VM
# Usage: remove_vm_nftables <slot>
remove_vm_nftables() {
    local slot="$1"

    # Remove all rules with this VM's comment tag
    for chain in forward prerouting postrouting; do
        # List rules with handles, find ones matching our VM, delete them
        nft -a list chain ip vm_filter "${chain}" 2>/dev/null | \
            grep "vm${slot}-" | \
            grep -oP 'handle \K[0-9]+' | \
            while read -r handle; do
                nft delete rule ip vm_filter "${chain}" handle "${handle}" 2>/dev/null || true
            done
    done
}

# Full cleanup for a VM's network
# Usage: cleanup_vm_network <slot> <tap-name>
cleanup_vm_network() {
    local slot="$1"
    local tap_name="$2"

    remove_vm_nftables "${slot}"
    remove_tap "${tap_name}"
}
