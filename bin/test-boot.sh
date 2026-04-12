#!/usr/bin/env bash
# =============================================================================
# test-boot.sh — Boot all 5 agent VMs in desktop-only mode (no LLM needed)
#
# Validates:
#   1. Firecracker boots each VM successfully
#   2. Network (TAP + IP) is configured
#   3. VNC is reachable (XFCE desktop running)
#   4. SSH is reachable
#   5. Egress filtering is active (optional, requires Squid)
#
# Usage:
#   sudo ./bin/test-boot.sh              # Boot all 5 agents
#   sudo ./bin/test-boot.sh debugger     # Boot just one
#   sudo ./bin/test-boot.sh --teardown   # Stop all test VMs
# =============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
SANDBOX_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
CTL="${SANDBOX_ROOT}/bin/sandbox-ctl"

source "${SANDBOX_ROOT}/lib/common.sh"
ensure_global_compiled
source "${SANDBOX_ROOT}/lib/common.sh"

if [[ $EUID -ne 0 ]]; then
    echo "ERROR: Must run as root. Use: sudo $0"
    exit 1
fi

# Handle --teardown
if [[ "${1:-}" == "--teardown" ]]; then
    echo "=== Tearing down all test VMs ==="
    "${CTL}" stop-all
    "${CTL}" cleanup
    echo "Done."
    exit 0
fi

# Which agents to test
if [[ $# -gt 0 ]]; then
    AGENTS=("$@")
else
    AGENTS=($(list_agent_types))
fi

echo "=========================================="
echo "  Agent Sandbox Boot Test (no LLM)"
echo "=========================================="
echo ""
echo "Agents to test: ${AGENTS[*]}"
echo "Mode: desktop-only (--no-agent)"
echo ""

# Check prerequisites
check_prereqs() {
    local ok=1

    echo "--- Prerequisites ---"

    if command -v firecracker &>/dev/null; then
        echo "  [OK] firecracker: $(firecracker --version 2>/dev/null | head -1)"
    else
        echo "  [MISSING] firecracker — run: sudo sandbox-ctl setup"
        ok=0
    fi

    if [[ -f "${SANDBOX_ROOT}/kernel/vmlinux" ]]; then
        echo "  [OK] kernel: $(du -h "${SANDBOX_ROOT}/kernel/vmlinux" | cut -f1)"
    else
        echo "  [MISSING] kernel — run: ./kernel/fetch-kernel.sh"
        ok=0
    fi

    if [[ -f "${SANDBOX_ROOT}/rootfs/base.ext4" ]]; then
        echo "  [OK] base rootfs: $(du -h "${SANDBOX_ROOT}/rootfs/base.ext4" | cut -f1)"
    else
        echo "  [MISSING] base rootfs — run: sudo sandbox-ctl build-base"
        ok=0
    fi

    local missing_agents=()
    for agent in "${AGENTS[@]}"; do
        if [[ -f "${SANDBOX_ROOT}/rootfs/agents/${agent}/rootfs.ext4" ]]; then
            echo "  [OK] ${agent} rootfs"
        else
            echo "  [MISSING] ${agent} rootfs — run: sudo sandbox-ctl build-agent ${agent}"
            missing_agents+=("${agent}")
            ok=0
        fi
    done

    if [[ -e /dev/kvm ]]; then
        echo "  [OK] /dev/kvm"
    else
        echo "  [MISSING] /dev/kvm — KVM not available"
        ok=0
    fi

    echo ""

    if [[ ${ok} -eq 0 ]]; then
        echo "Some prerequisites are missing. Quick setup:"
        echo ""
        echo "  sudo sandbox-ctl setup       # Install firecracker + kernel + network"
        echo "  sudo sandbox-ctl build-base  # Build base rootfs (~15 min)"
        echo "  sudo sandbox-ctl build-all   # Build all agent rootfs (~5 min each)"
        echo ""
        echo "Or build just what you need:"
        for agent in "${missing_agents[@]+"${missing_agents[@]}"}"; do
            echo "  sudo sandbox-ctl build-agent ${agent}"
        done
        echo ""
        read -rp "Continue anyway (will skip missing agents)? [y/N] " confirm
        if [[ "${confirm}" != "y" && "${confirm}" != "Y" ]]; then
            exit 1
        fi
    fi
}

check_prereqs

# Launch VMs
LAUNCHED=()
FAILED=()

for agent in "${AGENTS[@]}"; do
    if [[ ! -f "${SANDBOX_ROOT}/rootfs/agents/${agent}/rootfs.ext4" ]]; then
        echo "  [SKIP] ${agent} (no rootfs)"
        FAILED+=("${agent}")
        continue
    fi

    echo "--- Launching: ${agent} (--no-agent) ---"
    if "${CTL}" launch "${agent}" --no-agent 2>&1; then
        LAUNCHED+=("${agent}")
    else
        echo "  [FAIL] ${agent} failed to launch"
        FAILED+=("${agent}")
    fi
    echo ""
done

if [[ ${#LAUNCHED[@]} -eq 0 ]]; then
    echo "No VMs launched. Nothing to test."
    exit 1
fi

# Wait for VMs to boot
echo "=== Waiting 15s for VMs to boot... ==="
sleep 15

# Test each VM
echo ""
echo "=== Running Tests ==="
echo ""

PASS=0
TOTAL=0

test_result() {
    local name="$1" result="$2"
    ((TOTAL++))
    if [[ "${result}" == "pass" ]]; then
        echo "  [PASS] ${name}"
        ((PASS++))
    else
        echo "  [FAIL] ${name}"
    fi
}

for agent in "${LAUNCHED[@]}"; do
    # Find this VM's info
    local_info=""
    for info_file in "${STATE_DIR}"/vms/*/info.json; do
        [[ -f "${info_file}" ]] || continue
        atype=$(jq -r '.agent_type' "${info_file}" 2>/dev/null)
        if [[ "${atype}" == "${agent}" ]]; then
            local_info="${info_file}"
            break
        fi
    done

    if [[ -z "${local_info}" ]]; then
        echo "--- ${agent}: cannot find state ---"
        continue
    fi

    vm_ip=$(jq -r '.vm_ip' "${local_info}")
    novnc_port=$(jq -r '.novnc_port' "${local_info}")
    fc_pid=$(jq -r '.firecracker_pid' "${local_info}")
    slot=$(jq -r '.slot' "${local_info}")

    echo "--- ${agent} (slot ${slot}, IP ${vm_ip}) ---"

    # Test 1: Firecracker process alive
    if kill -0 "${fc_pid}" 2>/dev/null; then
        test_result "${agent}: firecracker process running" "pass"
    else
        test_result "${agent}: firecracker process running" "fail"
        continue
    fi

    # Test 2: TAP device exists
    if ip link show "tap-vm${slot}" &>/dev/null; then
        test_result "${agent}: TAP device tap-vm${slot}" "pass"
    else
        test_result "${agent}: TAP device tap-vm${slot}" "fail"
    fi

    # Test 3: VM responds to ping
    if ping -c 1 -W 3 "${vm_ip}" &>/dev/null; then
        test_result "${agent}: ping ${vm_ip}" "pass"
    else
        test_result "${agent}: ping ${vm_ip}" "fail"
    fi

    # Test 4: SSH port open
    if timeout 3 bash -c "echo >/dev/tcp/${vm_ip}/22" 2>/dev/null; then
        test_result "${agent}: SSH port 22 open" "pass"
    else
        test_result "${agent}: SSH port 22 open" "fail"
    fi

    # Test 5: VNC port open (inside VM)
    if timeout 3 bash -c "echo >/dev/tcp/${vm_ip}/5900" 2>/dev/null; then
        test_result "${agent}: VNC port 5900 open" "pass"
    else
        test_result "${agent}: VNC port 5900 open" "fail"
    fi

    # Test 6: websockify/noVNC port open (on host)
    if timeout 3 bash -c "echo >/dev/tcp/127.0.0.1/${novnc_port}" 2>/dev/null; then
        test_result "${agent}: noVNC port ${novnc_port} open" "pass"
    else
        test_result "${agent}: noVNC port ${novnc_port} open" "fail"
    fi

    # Test 7: Can SSH and run a command
    if ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
           -o ConnectTimeout=5 -o BatchMode=no \
           agent@"${vm_ip}" "echo hello" 2>/dev/null | grep -q "hello"; then
        test_result "${agent}: SSH command execution" "pass"
    else
        test_result "${agent}: SSH command execution (may need sshpass)" "fail"
    fi

    echo ""
done

# Summary
echo "=========================================="
echo "  Results: ${PASS}/${TOTAL} tests passed"
echo "  Launched: ${#LAUNCHED[@]} VMs"
if [[ ${#FAILED[@]} -gt 0 ]]; then
    echo "  Failed to launch: ${FAILED[*]}"
fi
echo "=========================================="
echo ""

# Print access info
echo "=== Access Your VMs ==="
echo ""
HOST=$(hostname -f 2>/dev/null || hostname)
"${CTL}" status
echo ""
echo "noVNC URLs:"
for agent in "${LAUNCHED[@]}"; do
    "${CTL}" vnc "${agent}" 2>/dev/null || true
done
echo ""
echo "To stop all test VMs:"
echo "  sudo $0 --teardown"
echo ""
echo "To SSH into a VM:"
echo "  sandbox-ctl ssh <agent-type>   (password: agent)"
