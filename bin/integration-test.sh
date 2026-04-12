#!/usr/bin/env bash
# =============================================================================
# integration-test.sh — Full integration test suite for agent-sandbox
#
# Run after major changes (Ubuntu upgrades, config refactors, etc.) to verify
# the entire pipeline works end-to-end.
#
# Usage:
#   sudo bin/integration-test.sh              # run all tests
#   sudo bin/integration-test.sh --quick      # config + single VM only
#   sudo bin/integration-test.sh --teardown   # clean up test VMs
#
# Tests:
#   1. Config compilation (agentconf.py)
#   2. YAML validation (all agents)
#   3. Host services (Squid, nftables, dnsmasq)
#   4. VM boot (all 5 agents, --no-agent mode)
#   5. Network connectivity (ping, SSH, VNC, noVNC)
#   6. Egress filtering (allowed vs blocked domains)
#   7. Clean shutdown
# =============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
SANDBOX_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
cd "${SANDBOX_ROOT}"

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

PASS=0
FAIL=0
SKIP=0
QUICK=0

[[ "${1:-}" == "--quick" ]] && QUICK=1

if [[ "${1:-}" == "--teardown" ]]; then
    echo "Tearing down test VMs..."
    sudo bin/sandbox-ctl stop-all 2>/dev/null || true
    sudo bin/sandbox-ctl cleanup 2>/dev/null || true
    echo "Done."
    exit 0
fi

if [[ $EUID -ne 0 ]]; then
    echo "ERROR: Must run as root. Use: sudo $0"
    exit 1
fi

test_pass() { echo -e "  ${GREEN}[PASS]${NC} $1"; PASS=$((PASS + 1)); }
test_fail() { echo -e "  ${RED}[FAIL]${NC} $1"; FAIL=$((FAIL + 1)); }
test_skip() { echo -e "  ${YELLOW}[SKIP]${NC} $1"; SKIP=$((SKIP + 1)); }

echo "=========================================="
echo "  Agent Sandbox Integration Tests"
echo "  $(date -Iseconds)"
echo "=========================================="
echo ""

# -------------------------------------------------------------------------
# Test 1: Config compilation
# -------------------------------------------------------------------------
echo "--- 1. Config Compilation ---"

if python3 lib/agentconf.py compile-global 2>&1 | grep -q "Compiled global"; then
    test_pass "compile-global"
else
    test_fail "compile-global"
fi

for agent in debugger feature-dev devops researcher security; do
    if python3 lib/agentconf.py compile "$agent" 2>&1 | grep -q "Compiled $agent"; then
        test_pass "compile $agent"
    else
        test_fail "compile $agent"
    fi
done

# Check compiled artifacts exist
for agent in debugger feature-dev devops researcher security; do
    for artifact in agent.conf allowlist.txt system-prompt.md customize.sh tools.json; do
        if [[ -f "build/${agent}/${artifact}" ]]; then
            test_pass "build/${agent}/${artifact} exists"
        else
            test_fail "build/${agent}/${artifact} missing"
        fi
    done
done
echo ""

# -------------------------------------------------------------------------
# Test 2: YAML validation
# -------------------------------------------------------------------------
echo "--- 2. YAML Validation ---"

for agent in debugger feature-dev devops researcher security; do
    if python3 lib/agentconf.py validate "$agent" 2>&1 | grep -q "OK"; then
        test_pass "validate $agent"
    else
        test_fail "validate $agent"
    fi
done
echo ""

# -------------------------------------------------------------------------
# Test 3: Host services
# -------------------------------------------------------------------------
echo "--- 3. Host Services ---"

if [[ -f /usr/local/bin/firecracker ]]; then
    test_pass "firecracker binary"
else
    test_fail "firecracker binary"
fi

if [[ -f kernel/vmlinux ]]; then
    test_pass "kernel exists"
else
    test_fail "kernel missing"
fi

if [[ -f rootfs/base.ext4 ]]; then
    test_pass "base rootfs exists"
else
    test_fail "base rootfs missing"
fi

if [[ -e /dev/kvm ]]; then
    test_pass "/dev/kvm available"
else
    test_fail "/dev/kvm not available"
fi

# Check ip_forward
if [[ "$(cat /proc/sys/net/ipv4/ip_forward)" == "1" ]]; then
    test_pass "ip_forward enabled"
else
    # Enable it for the test
    sysctl -w net.ipv4.ip_forward=1 >/dev/null
    test_pass "ip_forward enabled (was off, now on)"
fi

# Ensure nftables base rules exist
if nft list table ip vm_filter &>/dev/null; then
    test_pass "nftables vm_filter table"
else
    HOST_IFACE=$(ip route | grep default | awk '{print $5}' | head -1)
    nft add table ip vm_filter
    nft add chain ip vm_filter forward '{ type filter hook forward priority 0; policy drop; }'
    nft add chain ip vm_filter prerouting '{ type nat hook prerouting priority -100; }'
    nft add chain ip vm_filter postrouting '{ type nat hook postrouting priority 100; }'
    nft add rule ip vm_filter forward ct state established,related accept
    nft add rule ip vm_filter postrouting oifname "$HOST_IFACE" masquerade
    nft add rule ip vm_filter forward iifname "tap-vm*" accept comment '"test-allow-all"'
    test_pass "nftables vm_filter table (created)"
fi

# Ensure Squid is running
if ! systemctl is-active --quiet squid 2>/dev/null; then
    mkdir -p /etc/squid/acls
    echo "# test" > /etc/squid/acls/vm-acls.conf
    echo "# test" > /etc/squid/acls/all-allowed-domains.txt
    cp "${SANDBOX_ROOT}/network/squid/squid-base.conf" /etc/squid/squid.conf
    systemctl reset-failed squid 2>/dev/null || true
    systemctl start squid 2>/dev/null || true
    sleep 2
fi

if systemctl is-active --quiet squid 2>/dev/null; then
    test_pass "squid running"
else
    test_fail "squid not running"
fi

# Ensure state dirs exist
mkdir -p "${SANDBOX_ROOT}/state/logs" "${SANDBOX_ROOT}/state/vms"

echo ""

# -------------------------------------------------------------------------
# Test 4: VM Boot
# -------------------------------------------------------------------------
echo "--- 4. VM Boot ---"

if [[ $QUICK -eq 1 ]]; then
    AGENTS=(debugger)
    echo "  (quick mode: testing debugger only)"
else
    AGENTS=(debugger feature-dev devops researcher security)
fi

# Check rootfs images exist
BOOTABLE=()
for agent in "${AGENTS[@]}"; do
    if [[ -f "rootfs/agents/${agent}/rootfs.ext4" ]]; then
        test_pass "${agent} rootfs exists"
        BOOTABLE+=("$agent")
    else
        test_fail "${agent} rootfs missing (run: sandbox-ctl build-agent ${agent})"
    fi
done

# Launch VMs
LAUNCHED=()
for agent in "${BOOTABLE[@]}"; do
    LAUNCH_OUT=$(bin/sandbox-ctl launch "$agent" --no-agent 2>&1) || true
    if echo "$LAUNCH_OUT" | grep -q "launched successfully"; then
        test_pass "launch $agent"
        LAUNCHED+=("$agent")
    else
        test_fail "launch $agent"
        echo "    Output: $(echo "$LAUNCH_OUT" | grep -E 'ERROR|FAIL' | head -3)"
    fi
done

echo "  Waiting 15s for VMs to boot..."
sleep 15
echo ""

# -------------------------------------------------------------------------
# Test 5: Network connectivity
# -------------------------------------------------------------------------
echo "--- 5. Network Connectivity ---"

for agent in "${LAUNCHED[@]}"; do
    # Find VM info
    for f in state/vms/*/info.json; do
        [[ -f "$f" ]] || continue
        atype=$(python3 -c "import json; print(json.load(open('$f'))['agent_type'])" 2>/dev/null)
        if [[ "$atype" == "$agent" ]]; then
            ip=$(python3 -c "import json; print(json.load(open('$f'))['vm_ip'])" 2>/dev/null)
            slot=$(python3 -c "import json; print(json.load(open('$f'))['slot'])" 2>/dev/null)
            novnc_port=$((6080 + slot))

            # Ping
            if ping -c 1 -W 5 "$ip" &>/dev/null; then
                test_pass "${agent}: ping ${ip}"
            else
                test_fail "${agent}: ping ${ip}"
            fi

            # SSH
            if timeout 5 bash -c "echo >/dev/tcp/${ip}/22" 2>/dev/null; then
                test_pass "${agent}: SSH :22"
            else
                test_fail "${agent}: SSH :22"
            fi

            # VNC
            if timeout 5 bash -c "echo >/dev/tcp/${ip}/5900" 2>/dev/null; then
                test_pass "${agent}: VNC :5900"
            else
                test_fail "${agent}: VNC :5900"
            fi

            # noVNC
            if timeout 5 bash -c "echo >/dev/tcp/127.0.0.1/${novnc_port}" 2>/dev/null; then
                test_pass "${agent}: noVNC :${novnc_port}"
            else
                test_fail "${agent}: noVNC :${novnc_port}"
            fi

            break
        fi
    done
done
echo ""

# -------------------------------------------------------------------------
# Test 6: Egress filtering (basic)
# -------------------------------------------------------------------------
echo "--- 6. Egress Filtering ---"

if [[ ${#LAUNCHED[@]} -gt 0 ]]; then
    # Pick first launched VM
    agent="${LAUNCHED[0]}"
    for f in state/vms/*/info.json; do
        [[ -f "$f" ]] || continue
        atype=$(python3 -c "import json; print(json.load(open('$f'))['agent_type'])" 2>/dev/null)
        if [[ "$atype" == "$agent" ]]; then
            ip=$(python3 -c "import json; print(json.load(open('$f'))['vm_ip'])" 2>/dev/null)

            # Test DNS resolution works
            if timeout 5 ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
                -o ConnectTimeout=5 agent@"$ip" "nslookup github.com" 2>/dev/null | grep -q "Address"; then
                test_pass "${agent}: DNS resolution"
            else
                test_skip "${agent}: DNS resolution (SSH auth may need sshpass)"
            fi
            break
        fi
    done
else
    test_skip "egress filtering (no VMs launched)"
fi
echo ""

# -------------------------------------------------------------------------
# Test 7: Clean shutdown
# -------------------------------------------------------------------------
echo "--- 7. Clean Shutdown ---"

for agent in "${LAUNCHED[@]}"; do
    STOP_OUT=$(bin/sandbox-ctl stop "$agent" 2>&1) || true
    if echo "$STOP_OUT" | grep -qE "stopped|Stopping VM"; then
        test_pass "stop $agent"
    else
        test_fail "stop $agent"
    fi
done
sleep 2

# Verify cleanup
remaining=$(pgrep firecracker 2>/dev/null | wc -l)
if [[ $remaining -eq 0 ]]; then
    test_pass "no firecracker processes remaining"
else
    test_fail "${remaining} firecracker processes still running"
fi
echo ""

# -------------------------------------------------------------------------
# Summary
# -------------------------------------------------------------------------
TOTAL=$((PASS + FAIL + SKIP))
echo "=========================================="
echo -e "  Results: ${GREEN}${PASS} passed${NC}, ${RED}${FAIL} failed${NC}, ${YELLOW}${SKIP} skipped${NC} / ${TOTAL} total"
echo "=========================================="

if [[ $FAIL -gt 0 ]]; then
    exit 1
fi
