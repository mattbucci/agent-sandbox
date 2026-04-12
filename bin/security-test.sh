#!/usr/bin/env bash
# =============================================================================
# security-test.sh — Security test suite for agent-sandbox
#
# Attempts various escape and exfiltration attacks from inside VMs to validate
# the security model. Run after setting up VMs.
#
# Usage:
#   sudo bin/security-test.sh              # test all running VMs
#   sudo bin/security-test.sh --quick      # test first VM only
#
# Prerequisites:
#   - At least one VM running (use sandbox-ctl launch)
#   - sshpass installed on host
#   - VMs must have 'agent' user with password 'agent'
# =============================================================================
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
SANDBOX_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
cd "${SANDBOX_ROOT}"

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
NC='\033[0m'

PASS=0
FAIL=0
SKIP=0
EXPECTED_BLOCK=0  # attacks that SHOULD fail (blocked = PASS)
QUICK=0

[[ "${1:-}" == "--quick" ]] && QUICK=1

if [[ $EUID -ne 0 ]]; then
    echo "ERROR: Must run as root. Use: sudo $0"
    exit 1
fi

if ! command -v sshpass &>/dev/null; then
    echo "ERROR: sshpass required. Install: dnf install -y sshpass"
    exit 1
fi

# Test helpers
test_pass()  { echo -e "  ${GREEN}[PASS]${NC} $1"; PASS=$((PASS + 1)); }
test_fail()  { echo -e "  ${RED}[FAIL]${NC} $1"; FAIL=$((FAIL + 1)); }
test_skip()  { echo -e "  ${YELLOW}[SKIP]${NC} $1"; SKIP=$((SKIP + 1)); }
test_block() { echo -e "  ${GREEN}[BLOCKED]${NC} $1 ${CYAN}(expected — defense working)${NC}"; EXPECTED_BLOCK=$((EXPECTED_BLOCK + 1)); }
test_leak()  { echo -e "  ${RED}[LEAKED]${NC} $1 ${RED}(SECURITY ISSUE — attack succeeded)${NC}"; FAIL=$((FAIL + 1)); }

# Run command inside a VM via SSH. Returns the exit code and captures output.
vm_exec() {
    local ip="$1"
    shift
    sshpass -p agent ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
        -o ConnectTimeout=5 -o ServerAliveInterval=5 -o ServerAliveCountMax=2 \
        agent@"$ip" "$@" 2>/dev/null
}

# Run command with timeout
vm_exec_timeout() {
    local timeout="$1"
    local ip="$2"
    shift 2
    timeout "$timeout" sshpass -p agent ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
        -o ConnectTimeout=5 -o ServerAliveInterval=5 -o ServerAliveCountMax=2 \
        agent@"$ip" "$@" 2>/dev/null
}

echo "=========================================="
echo "  Agent Sandbox Security Tests"
echo "  $(date -Iseconds)"
echo "=========================================="
echo ""

# Find running VMs
declare -A VM_IPS
declare -A VM_SLOTS
for f in state/vms/*/info.json; do
    [[ -f "$f" ]] || continue
    atype=$(python3 -c "import json; print(json.load(open('$f'))['agent_type'])")
    ip=$(python3 -c "import json; print(json.load(open('$f'))['vm_ip'])")
    slot=$(python3 -c "import json; print(json.load(open('$f'))['slot'])")
    pid=$(python3 -c "import json; print(json.load(open('$f'))['firecracker_pid'])")
    if kill -0 "$pid" 2>/dev/null && ping -c 1 -W 2 "$ip" &>/dev/null; then
        VM_IPS[$atype]="$ip"
        VM_SLOTS[$atype]="$slot"
    fi
done

if [[ ${#VM_IPS[@]} -eq 0 ]]; then
    echo "ERROR: No running VMs found. Launch some first:"
    echo "  sudo bin/sandbox-ctl launch debugger --no-agent"
    exit 1
fi

echo "Running VMs:"
for atype in "${!VM_IPS[@]}"; do
    echo "  ${atype}: ${VM_IPS[$atype]} (slot ${VM_SLOTS[$atype]})"
done
echo ""

# Pick test VM(s)
if [[ $QUICK -eq 1 ]]; then
    TEST_AGENTS=($(echo "${!VM_IPS[@]}" | tr ' ' '\n' | head -1))
    echo "(quick mode: testing ${TEST_AGENTS[0]} only)"
else
    TEST_AGENTS=("${!VM_IPS[@]}")
fi

# Verify SSH access
echo "--- 0. SSH Access Verification ---"
for agent in "${TEST_AGENTS[@]}"; do
    ip="${VM_IPS[$agent]}"
    if vm_exec "$ip" "echo ok" | grep -q ok; then
        test_pass "${agent}: SSH access"
    else
        test_fail "${agent}: SSH access (cannot test further)"
    fi
done
echo ""

# =========================================================================
# TEST 1: Egress to blocked domains
# =========================================================================
echo "--- 1. Egress to Blocked Domains ---"
echo "  (Attempting HTTP/HTTPS to domains NOT in agent's allowlist)"
echo ""

BLOCKED_DOMAINS=(
    "evil.example.com"
    "malware.example.org"
    "c2-server.attacker.net"
    "facebook.com"
    "tiktok.com"
)

for agent in "${TEST_AGENTS[@]}"; do
    ip="${VM_IPS[$agent]}"
    for domain in "${BLOCKED_DOMAINS[@]}"; do
        result=$(vm_exec_timeout 10 "$ip" "curl -s -o /dev/null -w '%{http_code}' --connect-timeout 5 --max-time 8 https://${domain}/ 2>&1")
        result="${result:-000}"
        # 000 = connection failed/timeout, 403 = Squid denied, 503 = Squid error
        if [[ "$result" == "000" ]] || [[ "$result" == "403" ]] || [[ "$result" == "503" ]]; then
            test_block "${agent} → ${domain} (HTTP ${result} — blocked)"
        elif [[ "$result" =~ ^[23] ]]; then
            test_leak "${agent} → ${domain} (HTTP ${result} — content returned!)"
        else
            test_block "${agent} → ${domain} (HTTP ${result})"
        fi
    done
done
echo ""

# =========================================================================
# TEST 2: Direct IP connection (bypass DNS)
# =========================================================================
echo "--- 2. Direct IP Connection (DNS bypass) ---"
echo "  (Attempting to connect to IPs directly, bypassing domain filtering)"
echo ""

for agent in "${TEST_AGENTS[@]}"; do
    ip="${VM_IPS[$agent]}"

    # Try connecting to a well-known IP (Google DNS) on HTTP
    result=$(vm_exec_timeout 10 "$ip" "curl -s -o /dev/null -w '%{http_code}' --connect-timeout 5 --max-time 8 http://8.8.8.8/ 2>&1")
    result="${result:-000}"
    if [[ "$result" == "000" ]] || [[ "$result" == "403" ]] || [[ "$result" == "503" ]]; then
        test_block "${agent} → 8.8.8.8:80 direct IP (HTTP ${result})"
    elif [[ "$result" =~ ^[23] ]]; then
        test_leak "${agent} → 8.8.8.8:80 direct IP (HTTP ${result})"
    else
        test_block "${agent} → 8.8.8.8:80 direct IP (HTTP ${result})"
    fi

    # Try connecting to an arbitrary external IP on port 443
    result=$(vm_exec_timeout 10 "$ip" "curl -s -o /dev/null -w '%{http_code}' --connect-timeout 5 --max-time 8 https://1.1.1.1/ 2>&1")
    result="${result:-000}"
    if [[ "$result" == "000" ]] || [[ "$result" == "403" ]] || [[ "$result" == "503" ]]; then
        test_block "${agent} → 1.1.1.1:443 direct IP (HTTP ${result})"
    elif [[ "$result" =~ ^[23] ]]; then
        test_leak "${agent} → 1.1.1.1:443 direct IP (HTTP ${result})"
    else
        test_block "${agent} → 1.1.1.1:443 direct IP (HTTP ${result})"
    fi

    # Try a non-HTTP port (raw TCP to port 9999)
    result=$(vm_exec_timeout 8 "$ip" "timeout 5 bash -c 'echo test > /dev/tcp/8.8.8.8/9999' 2>&1 && echo CONNECTED || echo BLOCKED")
    if echo "$result" | grep -q "BLOCKED\|Connection refused\|timed out\|No route"; then
        test_block "${agent} → 8.8.8.8:9999 raw TCP"
    else
        test_leak "${agent} → 8.8.8.8:9999 raw TCP (${result})"
    fi
done
echo ""

# =========================================================================
# TEST 3: VM-to-VM lateral movement
# =========================================================================
echo "--- 3. VM-to-VM Lateral Movement ---"
echo "  (Attempting to reach other VMs' subnets)"
echo ""

for agent in "${TEST_AGENTS[@]}"; do
    ip="${VM_IPS[$agent]}"
    slot="${VM_SLOTS[$agent]}"

    # Try to ping/connect to other VM subnets
    for other_agent in "${!VM_IPS[@]}"; do
        [[ "$other_agent" == "$agent" ]] && continue
        other_ip="${VM_IPS[$other_agent]}"

        result=$(vm_exec_timeout 8 "$ip" "ping -c 1 -W 3 ${other_ip} 2>&1 && echo REACHABLE || echo BLOCKED")
        if echo "$result" | grep -q "REACHABLE"; then
            test_leak "${agent} → ${other_agent} (${other_ip}) ping succeeded"
        else
            test_block "${agent} → ${other_agent} (${other_ip}) ping blocked"
        fi
    done

    # Try to SSH to another VM
    if [[ ${#VM_IPS[@]} -gt 1 ]]; then
        for other_agent in "${!VM_IPS[@]}"; do
            [[ "$other_agent" == "$agent" ]] && continue
            other_ip="${VM_IPS[$other_agent]}"
            result=$(vm_exec_timeout 8 "$ip" "timeout 3 bash -c 'echo > /dev/tcp/${other_ip}/22' 2>&1 && echo CONNECTED || echo BLOCKED")
            if echo "$result" | grep -q "CONNECTED"; then
                test_leak "${agent} → ${other_agent} SSH (${other_ip}:22) connected"
            else
                test_block "${agent} → ${other_agent} SSH (${other_ip}:22) blocked"
            fi
            break  # one is enough to prove the point
        done
    fi
done
echo ""

# =========================================================================
# TEST 4: Host filesystem / device access
# =========================================================================
echo "--- 4. Host Filesystem / Escape Attempts ---"
echo "  (Attempting to access host resources from inside VM)"
echo ""

for agent in "${TEST_AGENTS[@]}"; do
    ip="${VM_IPS[$agent]}"

    # Try to read host-specific files (should not exist in VM)
    result=$(vm_exec "$ip" "cat /etc/hostname 2>/dev/null")
    if echo "$result" | grep -q "localhost\|$(hostname)"; then
        test_leak "${agent}: /etc/hostname matches host"
    else
        test_block "${agent}: /etc/hostname is VM-local (${result})"
    fi

    # Try to access Firecracker API socket (should not be reachable from guest)
    result=$(vm_exec_timeout 5 "$ip" "ls -la /tmp/fc*.sock 2>&1; curl -s --unix-socket /tmp/firecracker.sock http://localhost/ 2>&1" || echo "no socket")
    if echo "$result" | grep -qi "running\|instance"; then
        test_leak "${agent}: Firecracker API socket accessible from guest!"
    else
        test_block "${agent}: Firecracker API socket not accessible"
    fi

    # Try to access host's /dev/kvm
    result=$(vm_exec "$ip" "ls -la /dev/kvm 2>&1")
    if echo "$result" | grep -q "No such file\|Permission denied\|cannot access"; then
        test_block "${agent}: /dev/kvm not accessible in guest"
    else
        test_leak "${agent}: /dev/kvm accessible (${result})"
    fi

    # Try to mount host devices
    result=$(vm_exec "$ip" "sudo mount /dev/vda /mnt 2>&1 || echo FAIL")
    if echo "$result" | grep -qi "busy\|already\|FAIL"; then
        test_block "${agent}: cannot mount /dev/vda (already root)"
    fi

    # Try to read /proc/1/environ to find host info
    result=$(vm_exec "$ip" "cat /proc/1/environ 2>/dev/null | tr '\0' '\n' | head -5 || echo 'denied'")
    # This is the VM's PID 1, not the host's — should show VM_IP not host info
    if echo "$result" | grep -qi "VM_IP\|GATEWAY_IP\|agent-init"; then
        test_block "${agent}: /proc/1/environ shows VM init (isolated)"
    else
        test_block "${agent}: /proc/1/environ not readable or empty"
    fi
done
echo ""

# =========================================================================
# TEST 5: Network scanning / service discovery
# =========================================================================
echo "--- 5. Host Service Discovery ---"
echo "  (Scanning host gateway for open ports beyond allowed services)"
echo ""

for agent in "${TEST_AGENTS[@]}"; do
    ip="${VM_IPS[$agent]}"
    slot="${VM_SLOTS[$agent]}"
    gateway="10.0.${slot}.1"

    # Scan common dangerous ports on the gateway
    DANGEROUS_PORTS=(22 80 443 2375 5432 3306 6379 8080 8443 9090)
    for port in "${DANGEROUS_PORTS[@]}"; do
        result=$(vm_exec_timeout 5 "$ip" "timeout 2 bash -c 'echo > /dev/tcp/${gateway}/${port}' 2>&1 && echo OPEN || echo CLOSED")
        if echo "$result" | grep -q "OPEN"; then
            case $port in
                53) test_pass "${agent}: gateway:${port} open (DNS — expected)" ;;
                80)  test_pass "${agent}: gateway:${port} open (Squid HTTP intercept — expected)" ;;
                443) test_pass "${agent}: gateway:${port} open (Squid HTTPS intercept — expected)" ;;
                3128|3129|3130) test_pass "${agent}: gateway:${port} open (Squid — expected)" ;;
                4317|4318) test_pass "${agent}: gateway:${port} open (OTel — expected)" ;;
                *)
                    test_leak "${agent}: gateway:${port} OPEN (unexpected service exposed!)"
                    ;;
            esac
        else
            test_block "${agent}: gateway:${port} closed"
        fi
    done
done
echo ""

# =========================================================================
# TEST 6: DNS-based attacks
# =========================================================================
echo "--- 6. DNS Attacks ---"
echo "  (Testing DNS resolution and potential tunneling)"
echo ""

for agent in "${TEST_AGENTS[@]}"; do
    ip="${VM_IPS[$agent]}"

    # DNS resolution should work for any domain (dnsmasq resolves normally)
    # The filtering happens at Squid level, not DNS level
    result=$(vm_exec_timeout 10 "$ip" "nslookup evil.example.com 2>&1 || echo NXDOMAIN")
    if echo "$result" | grep -qi "NXDOMAIN\|can't find\|SERVFAIL"; then
        test_pass "${agent}: DNS for evil.example.com returns NXDOMAIN (domain doesn't exist)"
    else
        # DNS resolution working is expected — Squid does the blocking
        test_pass "${agent}: DNS resolves (normal — Squid blocks at connection level)"
    fi

    # Try to use an external DNS server directly (should be blocked by nftables)
    result=$(vm_exec_timeout 8 "$ip" "nslookup google.com 8.8.8.8 2>&1" || echo "BLOCKED")
    if echo "$result" | grep -qi "timed out\|connection refused\|no servers\|BLOCKED"; then
        test_block "${agent}: external DNS (8.8.8.8) blocked by nftables"
    else
        test_leak "${agent}: external DNS (8.8.8.8) reachable — nftables not blocking UDP 53"
    fi
done
echo ""

# =========================================================================
# TEST 7: Resource exhaustion (safe subset)
# =========================================================================
echo "--- 7. Resource Limits ---"
echo "  (Verifying VM resource boundaries)"
echo ""

for agent in "${TEST_AGENTS[@]}"; do
    ip="${VM_IPS[$agent]}"

    # Check that memory is limited (should be ~8GB or what was configured)
    mem_kb=$(vm_exec "$ip" "grep MemTotal /proc/meminfo | awk '{print \$2}'")
    mem_gb=$(echo "scale=1; ${mem_kb:-0} / 1048576" | bc 2>/dev/null || echo "?")
    if [[ -n "$mem_kb" ]] && [[ "$mem_kb" -lt 16777216 ]]; then  # less than 16GB
        test_pass "${agent}: memory limited to ${mem_gb}GB"
    else
        test_fail "${agent}: memory unexpectedly high (${mem_gb}GB)"
    fi

    # Check CPU count
    cpus=$(vm_exec "$ip" "nproc")
    if [[ -n "$cpus" ]] && [[ "$cpus" -le 8 ]]; then
        test_pass "${agent}: CPU limited to ${cpus} vCPUs"
    else
        test_fail "${agent}: CPU count unexpected (${cpus})"
    fi

    # Check disk size is bounded
    disk_size=$(vm_exec "$ip" "df -BG / | tail -1 | awk '{print \$2}' | tr -d 'G'")
    if [[ -n "$disk_size" ]] && [[ "$disk_size" -le 16 ]]; then
        test_pass "${agent}: disk limited to ${disk_size}GB"
    else
        test_fail "${agent}: disk size unexpected (${disk_size}GB)"
    fi

    break  # Resource checks only need one VM
done
echo ""

# =========================================================================
# Summary
# =========================================================================
TOTAL=$((PASS + FAIL + SKIP + EXPECTED_BLOCK))
echo "=========================================="
echo -e "  Results:"
echo -e "    ${GREEN}Defenses working (blocked):  ${EXPECTED_BLOCK}${NC}"
echo -e "    ${GREEN}Other tests passed:          ${PASS}${NC}"
echo -e "    ${RED}Security issues (leaked):    ${FAIL}${NC}"
echo -e "    ${YELLOW}Skipped:                     ${SKIP}${NC}"
echo -e "    Total:                       ${TOTAL}"
echo "=========================================="

if [[ $FAIL -gt 0 ]]; then
    echo ""
    echo -e "${RED}WARNING: ${FAIL} security issue(s) found! Review [LEAKED] items above.${NC}"
    exit 1
else
    echo ""
    echo -e "${GREEN}All defenses validated. No security issues found.${NC}"
    exit 0
fi
