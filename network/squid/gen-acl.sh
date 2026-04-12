#!/usr/bin/env bash
# =============================================================================
# gen-acl.sh — Generate Squid ACL files from agent allowlists
#
# Usage: gen-acl.sh <slot> <agent-type> <allowlist-file>
#
# Creates:
#   /etc/squid/acls/vm{slot}-domains.txt   — domain list for this VM
#   /etc/squid/acls/vm-{slot}.conf         — Squid ACL rules for this VM
#
# Also regenerates /etc/squid/acls/all-allowed-domains.txt (union of all VMs)
# =============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
SANDBOX_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

source "${SANDBOX_ROOT}/config/sandbox.conf"

SLOT="${1:?Usage: gen-acl.sh <slot> <agent-type> <allowlist-file>}"
AGENT_TYPE="${2:?Usage: gen-acl.sh <slot> <agent-type> <allowlist-file>}"
ALLOWLIST_FILE="${3:?Usage: gen-acl.sh <slot> <agent-type> <allowlist-file>}"

ACL_DIR="/etc/squid/acls"
mkdir -p "${ACL_DIR}"

SUBNET="${VM_SUBNET_PREFIX:-10.0}.${SLOT}.0/24"
DOMAINS_FILE="${ACL_DIR}/vm${SLOT}-domains.txt"
CONF_FILE="${ACL_DIR}/vm-${SLOT}.conf"

# --- Extract LiteLLM host from config (always allowed) ---
LLM_HOST=""
if [[ -n "${LLM_API_BASE:-}" ]]; then
    LLM_HOST=$(echo "${LLM_API_BASE}" | sed -E 's|https?://||; s|:[0-9]+.*||; s|/.*||')
fi

# --- Generate domain list ---
echo "# Allowed domains for VM slot ${SLOT} (${AGENT_TYPE})" > "${DOMAINS_FILE}"
echo "# Generated: $(date -Iseconds)" >> "${DOMAINS_FILE}"

# Add domains from allowlist
if [[ -f "${ALLOWLIST_FILE}" ]]; then
    # Strip comments and empty lines, add each domain
    grep -v '^\s*#' "${ALLOWLIST_FILE}" | grep -v '^\s*$' >> "${DOMAINS_FILE}"
fi

# Always allow LiteLLM host
if [[ -n "${LLM_HOST}" ]]; then
    echo "${LLM_HOST}" >> "${DOMAINS_FILE}"
fi

# --- Generate Squid ACL config for this VM ---
cat > "${CONF_FILE}" <<EOF
# VM Slot ${SLOT}: ${AGENT_TYPE}
# Source subnet: ${SUBNET}
acl vm${SLOT}_src src ${SUBNET}
acl vm${SLOT}_domains ssl::server_name "${DOMAINS_FILE}"
acl vm${SLOT}_http_domains dstdomain "${DOMAINS_FILE}"
http_access allow vm${SLOT}_src vm${SLOT}_http_domains
http_access allow CONNECT vm${SLOT}_src vm${SLOT}_domains
EOF

# --- Regenerate combined allowed domains list ---
echo "# Combined allowed domains (all VMs)" > "${ACL_DIR}/all-allowed-domains.txt"
echo "# Generated: $(date -Iseconds)" >> "${ACL_DIR}/all-allowed-domains.txt"
cat "${ACL_DIR}"/vm*-domains.txt 2>/dev/null | grep -v '^\s*#' | grep -v '^\s*$' | sort -u \
    >> "${ACL_DIR}/all-allowed-domains.txt"

echo "ACL generated for VM slot ${SLOT} (${AGENT_TYPE}): ${CONF_FILE}"
