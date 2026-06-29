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

# Resolve a hostname to a single IPv4 address.
# Prints the IPv4 on stdout; prints nothing (and returns 1) on failure.
# A value that is already a dotted-quad IPv4 is returned unchanged.
# Usage: resolve_ipv4 <host-or-ip>
resolve_ipv4() {
    local host="$1"
    local ip=""

    # Already a dotted-quad IPv4? Pass it through untouched.
    if [[ "${host}" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
        echo "${host}"
        return 0
    fi

    # Prefer getent (honours nsswitch: hosts file, DNS, etc.)
    ip=$(getent ahostsv4 "${host}" 2>/dev/null | awk 'NR==1{print $1}')

    # Fall back to dig if getent yields nothing
    if [[ -z "${ip}" ]]; then
        ip=$(dig +short A "${host}" 2>/dev/null | head -1)
    fi

    if [[ -z "${ip}" ]]; then
        return 1
    fi
    echo "${ip}"
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
        # `grep` exits non-zero when the URL has no explicit port; under the
        # caller's `set -o pipefail` that would abort launch, so tolerate it and
        # fall back to 443 below.
        llm_port=$(echo "${LLM_API_BASE}" | grep -oP ':\K[0-9]+' | head -1) || true
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

    # BLOCK VM → host SSH (defense in depth — VMs should not reach host SSH)
    nft add rule ip vm_filter forward \
        iifname "${tap_name}" ip daddr "${gateway_ip}" tcp dport 22 drop \
        comment "\"vm${slot}-block-host-ssh\""

    # Allow VM → LiteLLM server (direct, bypasses Squid)
    if [[ -n "${llm_host}" ]]; then
        # nftables 'ip daddr' requires a literal IPv4 address. LLM_API_BASE may
        # contain a hostname (e.g. simple-llm-router.ph.ca), so resolve it to a single IPv4 at
        # rule-add time. If resolution fails, warn and skip the passthrough rule
        # rather than adding a broken/ambiguous rule.
        local llm_ip
        # resolve_ipv4 exits non-zero on failure; under the caller's `set -e`
        # that would abort launch before the warn-and-skip branch below, so
        # tolerate it and let the empty-check handle failure.
        llm_ip=$(resolve_ipv4 "${llm_host}") || true
        if [[ -z "${llm_ip}" ]]; then
            echo "network.sh: WARNING: could not resolve LLM host '${llm_host}' to an IPv4 address; skipping LLM passthrough rule for vm${slot}" >&2
        else
            nft add rule ip vm_filter prerouting \
                iifname "${tap_name}" ip daddr "${llm_ip}" tcp dport "${llm_port}" accept \
                comment "\"vm${slot}-llm-passthrough\""
            nft add rule ip vm_filter forward \
                iifname "${tap_name}" ip daddr "${llm_ip}" tcp dport "${llm_port}" accept \
                comment "\"vm${slot}-llm\""
        fi
    fi

    # NOTE: Squid runs on the HOST, so its outbound traffic goes through the
    # OUTPUT chain, not FORWARD. We do NOT need a broad forward accept rule.
    # The established,related rule in the forward chain handles return traffic.
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

# Ensure the host accepts inbound TCP to the gateway router port from the LAN.
#
# The hermes-gateway router binds the host LAN IP directly (no DNAT), and
# vm_filter's input chain only drops tap-vm* traffic, so plain LAN->host:<port>
# is already accepted by the kernel. The only thing that can block it is a
# separate host firewall such as firewalld. This helper is therefore a no-op
# unless firewalld is installed AND active, in which case it opens the port.
#
# NOTE: this opens the port at runtime only. Add '--permanent' below (and re-run
# with the runtime form too) if the rule should survive a firewalld reload.
#
# Usage: allow_gateway_ingress [port]   (default port 8642)
allow_gateway_ingress() {
    local port="${1:-8642}"

    # No-op when firewalld is not present.
    if ! command -v firewall-cmd >/dev/null 2>&1; then
        return 0
    fi

    # No-op when firewalld is present but not running.
    if ! firewall-cmd --state >/dev/null 2>&1; then
        return 0
    fi

    # Idempotent: firewalld treats adding an already-open port as success.
    if ! firewall-cmd --add-port="${port}/tcp" >/dev/null 2>&1; then
        echo "network.sh: WARNING: failed to open gateway port ${port}/tcp in firewalld" >&2
        return 1
    fi
}

# Full cleanup for a VM's network
# Usage: cleanup_vm_network <slot> <tap-name>
cleanup_vm_network() {
    local slot="$1"
    local tap_name="$2"

    remove_vm_nftables "${slot}"
    remove_tap "${tap_name}"
}
