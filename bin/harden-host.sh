#!/usr/bin/env bash
# =============================================================================
# harden-host.sh — Apply Firecracker production host hardening
#
# Based on: https://github.com/firecracker-microvm/firecracker/blob/main/docs/prod-host-setup.md
#
# Usage:
#   sudo bin/harden-host.sh          # apply all hardening
#   sudo bin/harden-host.sh audit    # check current state without changes
# =============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
SANDBOX_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; NC='\033[0m'
AUDIT_ONLY=0
[[ "${1:-}" == "audit" ]] && AUDIT_ONLY=1
FIXES=0; WARNINGS=0; OK=0

check_pass()  { echo -e "  ${GREEN}[OK]${NC} $1"; OK=$((OK + 1)); }
check_warn()  { echo -e "  ${YELLOW}[WARN]${NC} $1"; WARNINGS=$((WARNINGS + 1)); }
check_fail()  { echo -e "  ${RED}[FAIL]${NC} $1"; FIXES=$((FIXES + 1)); }
check_fixed() { echo -e "  ${GREEN}[FIXED]${NC} $1"; FIXES=$((FIXES + 1)); }

if [[ $EUID -ne 0 ]]; then
    echo "ERROR: Must run as root."
    exit 1
fi

echo "=========================================="
if [[ $AUDIT_ONLY -eq 1 ]]; then
    echo "  Firecracker Production Host Audit"
else
    echo "  Firecracker Production Host Hardening"
fi
echo "  Ref: firecracker docs/prod-host-setup.md"
echo "=========================================="
echo ""

# =========================================================================
# 1. Kernel Samepage Merging (KSM) — must be disabled
# =========================================================================
echo "--- 1. Kernel Samepage Merging (KSM) ---"
KSM=$(cat /sys/kernel/mm/ksm/run 2>/dev/null || echo "N/A")
if [[ "$KSM" == "0" ]]; then
    check_pass "KSM disabled"
elif [[ "$KSM" == "1" ]]; then
    if [[ $AUDIT_ONLY -eq 0 ]]; then
        echo 0 > /sys/kernel/mm/ksm/run
        check_fixed "KSM disabled (was enabled)"
    else
        check_fail "KSM is ENABLED — side-channel risk"
        echo "         Fix: echo 0 > /sys/kernel/mm/ksm/run"
    fi
fi
echo ""

# =========================================================================
# 2. Swap — must be disabled
# =========================================================================
echo "--- 2. Swap ---"
SWAP_COUNT=$(cat /proc/swaps | tail -n+2 | wc -l)
if [[ "$SWAP_COUNT" -eq 0 ]]; then
    check_pass "No swap partitions active"
else
    if [[ $AUDIT_ONLY -eq 0 ]]; then
        swapoff -a
        check_fixed "Swap disabled (was ${SWAP_COUNT} partition(s))"
    else
        check_fail "${SWAP_COUNT} swap partition(s) active"
        echo "         Fix: swapoff -a"
    fi
fi
echo ""

# =========================================================================
# 3. SMT (Simultaneous Multi-Threading / Hyperthreading)
# =========================================================================
echo "--- 3. SMT (Hyperthreading) ---"
SMT=$(cat /sys/devices/system/cpu/smt/active 2>/dev/null || echo "N/A")
if [[ "$SMT" == "0" ]]; then
    check_pass "SMT disabled"
elif [[ "$SMT" == "1" ]]; then
    check_warn "SMT is ENABLED — recommended to disable for tenant isolation"
    echo "         Note: disabling halves available vCPUs"
    echo "         Fix: echo off > /sys/devices/system/cpu/smt/control"
    echo "         Or add 'nosmt' to kernel command line"
fi
echo ""

# =========================================================================
# 4. KVM timer mitigation (x86)
# =========================================================================
echo "--- 4. KVM Timer Period ---"
if [[ -f /sys/module/kvm/parameters/min_timer_period_us ]]; then
    TIMER=$(cat /sys/module/kvm/parameters/min_timer_period_us)
    check_pass "kvm.min_timer_period_us = ${TIMER}"
else
    check_warn "kvm.min_timer_period_us not available"
fi
echo ""

# =========================================================================
# 5. cgroup v2 boot regression fix
# =========================================================================
echo "--- 5. cgroup Configuration ---"
if mount | grep -q "cgroup2"; then
    echo "  cgroup version: v2"
    if [[ $AUDIT_ONLY -eq 0 ]]; then
        mount -o remount,favordynmods /sys/fs/cgroup 2>/dev/null && \
            check_fixed "cgroup v2 remounted with favordynmods" || \
            check_warn "could not remount cgroup with favordynmods (may already be set)"
    else
        check_warn "ensure 'favordynmods' mount option on /sys/fs/cgroup"
    fi
else
    echo "  cgroup version: v1"
    check_pass "cgroup v1 (check cgroup_favordynmods=true in kernel cmdline)"
fi
echo ""

# =========================================================================
# 6. Log rotation for Firecracker logs
# =========================================================================
echo "--- 6. Log Rotation ---"
LOGROTATE_CONF="/etc/logrotate.d/agent-sandbox"
if [[ -f "$LOGROTATE_CONF" ]]; then
    check_pass "logrotate configured for agent-sandbox"
else
    if [[ $AUDIT_ONLY -eq 0 ]]; then
        cat > "$LOGROTATE_CONF" <<'EOF'
/home/letsrtfm/AI/agent-sandbox/state/logs/*.log {
    daily
    rotate 7
    compress
    delaycompress
    missingok
    notifempty
    maxsize 100M
}

/var/log/otel/*.jsonl {
    daily
    rotate 7
    compress
    delaycompress
    missingok
    notifempty
    maxsize 100M
}

/var/log/squid/*.log {
    daily
    rotate 7
    compress
    delaycompress
    missingok
    notifempty
    postrotate
        squid -k rotate 2>/dev/null || true
    endscript
}
EOF
        check_fixed "logrotate configured for VM logs, OTel traces, and Squid"
    else
        check_fail "no logrotate config for agent-sandbox"
        echo "         Fix: run this script without 'audit'"
    fi
fi
echo ""

# =========================================================================
# 7. Network rate limiting
# =========================================================================
echo "--- 7. Network Rate Limiting ---"
# Check if any TAP devices have rate limiting
HAS_TC=0
for tap in $(ip link show type tuntap 2>/dev/null | grep -oP 'tap-vm\d+' || true); do
    tc_out=$(tc qdisc show dev "$tap" 2>/dev/null)
    if echo "$tc_out" | grep -qE "htb|tbf"; then
        HAS_TC=1
    fi
done
if [[ $HAS_TC -eq 1 ]]; then
    check_pass "tc rate limiting on TAP devices"
else
    check_warn "no tc rate limiting on TAP devices"
    echo "         Consider: tc qdisc add dev tap-vmN root tbf rate 100mbit burst 32kbit latency 50ms"
    echo "         Or use Firecracker's built-in rate_limiter in VM config"
fi
echo ""

# =========================================================================
# 8. /dev/kvm permissions
# =========================================================================
echo "--- 8. /dev/kvm Permissions ---"
if [[ -e /dev/kvm ]]; then
    KVM_PERMS=$(stat -c '%a' /dev/kvm)
    if [[ "$KVM_PERMS" == "666" ]]; then
        check_warn "/dev/kvm is world-readable/writable (666)"
        echo "         Production: use a dedicated group and set 660"
    else
        check_pass "/dev/kvm permissions: ${KVM_PERMS}"
    fi
else
    check_fail "/dev/kvm not found"
fi
echo ""

# =========================================================================
# 9. Jailer directory permissions
# =========================================================================
echo "--- 9. Jailer Paths ---"
JAILER_BIN="/usr/local/bin/jailer"
if [[ -f "$JAILER_BIN" ]]; then
    check_pass "jailer binary exists at ${JAILER_BIN}"
    JAILER_PERMS=$(stat -c '%a' "$JAILER_BIN")
    if [[ "$JAILER_PERMS" == "755" ]]; then
        check_pass "jailer permissions: ${JAILER_PERMS}"
    else
        check_warn "jailer permissions: ${JAILER_PERMS} (expected 755)"
    fi
else
    check_fail "jailer binary not found"
fi
echo ""

# =========================================================================
# 10. Host kernel console logging
# =========================================================================
echo "--- 10. Kernel Log Level ---"
LOGLEVEL=$(cat /proc/sys/kernel/printk | awk '{print $1}')
if [[ "$LOGLEVEL" -le 1 ]]; then
    check_pass "kernel log level: ${LOGLEVEL} (quiet)"
else
    check_warn "kernel log level: ${LOGLEVEL} (recommended: 1 or lower)"
    echo "         Fix: add 'quiet loglevel=1' to kernel command line"
fi
echo ""

# =========================================================================
# Summary
# =========================================================================
TOTAL=$((OK + WARNINGS + FIXES))
echo "=========================================="
echo -e "  ${GREEN}Passing:   ${OK}${NC}"
echo -e "  ${YELLOW}Warnings:  ${WARNINGS}${NC}"
echo -e "  ${RED}Issues:    ${FIXES}${NC}"
echo -e "  Total:     ${TOTAL}"
echo "=========================================="

if [[ $AUDIT_ONLY -eq 1 ]] && [[ $FIXES -gt 0 ]]; then
    echo ""
    echo "Run 'sudo bin/harden-host.sh' (without 'audit') to apply fixes."
fi
