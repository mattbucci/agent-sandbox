#!/usr/bin/env bash
# =============================================================================
# unit-shell.sh — rootless tests for the shell libraries and config compiler.
#
# Covers the pieces whose failures only show up at boot or during an image
# build: hostname resolution, the Squid ACL generator, checksum-verified
# downloads, the versions manifest and the compiled customize.sh.
#
# Usage: test/unit-shell.sh        (no root, no network, touches nothing live)
# =============================================================================
set -uo pipefail

SANDBOX_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
export SANDBOX_ROOT
WORK="$(mktemp -d)"
trap 'rm -rf "${WORK}"' EXIT

PASS=0
FAIL=0
ok()   { PASS=$((PASS + 1)); echo "  ok    $1"; }
fail() { FAIL=$((FAIL + 1)); echo "  FAIL  $1"; }
check() { if "${@:2}" >/dev/null 2>&1; then ok "$1"; else fail "$1"; fi; }
check_not() { if "${@:2}" >/dev/null 2>&1; then fail "$1"; else ok "$1"; fi; }

source "${SANDBOX_ROOT}/lib/common.sh"
source "${SANDBOX_ROOT}/lib/network.sh"
source "${SANDBOX_ROOT}/lib/versions.sh"
source "${SANDBOX_ROOT}/lib/vm.sh"

echo "== lib/network.sh"
[[ "$(resolve_ipv4 192.0.2.7)" == "192.0.2.7" ]] && ok "IPv4 literal passes through" || fail "IPv4 literal passes through"
check_not "unresolvable host fails" resolve_ipv4 does-not-exist.invalid
[[ -z "$(resolve_ipv4 does-not-exist.invalid 2>/dev/null)" ]] && ok "unresolvable host prints nothing" || fail "unresolvable host prints nothing"
# dig reports errors on stdout; they must never be returned as an address.
mkdir -p "${WORK}/bin"
cat > "${WORK}/bin/dig" <<'DIG'
#!/bin/sh
echo ";; UDP setup with 8.8.8.8#53(8.8.8.8) for x failed: network unreachable."
DIG
cat > "${WORK}/bin/getent" <<'GETENT'
#!/bin/sh
exit 2
GETENT
chmod +x "${WORK}/bin/dig" "${WORK}/bin/getent"
out="$(PATH="${WORK}/bin:${PATH}" resolve_ipv4 llm.internal 2>/dev/null)"; rc=$?
[[ ${rc} -ne 0 && -z "${out}" ]] && ok "dig error text is not returned as an IP" || fail "dig error text is not returned as an IP (got '${out}')"
[[ "$(LLM_API_BASE=http://llm.internal:8080/v1 llm_host_from_config)" == "llm.internal" ]] && ok "LLM host parsed from api_base" || fail "LLM host parsed from api_base"

echo "== lib/common.sh"
types="$(list_agent_types | tr '\n' ' ')"
[[ "${types}" != *.conf* && "${types}" == *coder* ]] && ok "list_agent_types lists only YAML agents" || fail "list_agent_types lists only YAML agents (${types})"

echo "== lib/vm.sh"
JAILER_BASE_DIR="${WORK}/jail"; FIRECRACKER_BIN=/usr/local/bin/firecracker
mkdir -p "${WORK}/jail/firecracker/coder-3/root" "${WORK}/jail/firecracker/coder-4/root"
vm_remove_jail coder-3
[[ ! -d "${WORK}/jail/firecracker/coder-3" && -d "${WORK}/jail/firecracker/coder-4" ]] && ok "vm_remove_jail removes only that VM's chroot" || fail "vm_remove_jail removes only that VM's chroot"
vm_remove_jail ""
[[ -d "${WORK}/jail/firecracker/coder-4" ]] && ok "vm_remove_jail ignores an empty id" || fail "vm_remove_jail ignores an empty id"
VCPUS=2 MEM_MB=512 TAP_NAME=tap-vm3 GUEST_MAC=AA:FC:00:00:00:03 VM_IP=10.0.3.2 GATEWAY_IP=10.0.3.1 VM_NAME=t NO_AGENT=0 \
    vm_render_config "${WORK}/vm.json" /vmlinux /rootfs.ext4 /fc.log /metrics
check "rendered VM config is valid JSON" jq -e '.["machine-config"].vcpu_count == 2 and .drives[0].path_on_host == "/rootfs.ext4"' "${WORK}/vm.json"
check_not "rendered VM config has no placeholders left" grep -q '__[A-Z_]*__' "${WORK}/vm.json"

echo "== lib/versions.sh"
printf 'payload' > "${WORK}/artifact"
good="$(sha256sum "${WORK}/artifact" | awk '{print $1}')"
check "fetch_verified accepts a matching checksum" fetch_verified "file://${WORK}/artifact" "${good}" "${WORK}/out-good"
check_not "fetch_verified rejects a mismatch" fetch_verified "file://${WORK}/artifact" "${good/[0-9a-f]/x}" "${WORK}/out-bad"
[[ ! -e "${WORK}/out-bad" ]] && ok "mismatching artifact is never installed" || fail "mismatching artifact is never installed"
check_not "fetch_verified refuses an empty checksum" fetch_verified "file://${WORK}/artifact" "" "${WORK}/out-none"
bad_pins="$(grep -E '^[A-Z_]+_SHA256=' "${SANDBOX_ROOT}/config/versions.env" | grep -vcE '="[0-9a-f]{64}"$' || true)"
[[ "${bad_pins}" == "0" ]] && ok "every *_SHA256 pin is a 64-hex digest" || fail "every *_SHA256 pin is a 64-hex digest"
moving="$(grep -rnE 'curl[^|]*\|[[:space:]]*(sudo[[:space:]]+)?(ba)?sh' --include='*.sh' \
    "${SANDBOX_ROOT}/config/install-scripts" "${SANDBOX_ROOT}/rootfs/build-base.sh" \
    "${SANDBOX_ROOT}/bin" "${SANDBOX_ROOT}/otel" "${SANDBOX_ROOT}/novnc" "${SANDBOX_ROOT}/kernel" \
    | grep -vE '^[^:]+:[0-9]+:[[:space:]]*#' | grep -v '/bin/.*-test\.sh' || true)"
[[ -z "${moving}" ]] && ok "no installer pipes a download into a shell" || fail "installer pipes a download into a shell: ${moving}"

echo "== network/squid/gen-acl.sh"
export SQUID_ACL_DIR="${WORK}/acls"
printf '.github.com\napi.github.com\npypi.org\n' > "${WORK}/allow.txt"
check "generate ACL for slot 3" "${SANDBOX_ROOT}/network/squid/gen-acl.sh" 3 coder "${WORK}/allow.txt"
check "generate ACL for slot 4" "${SANDBOX_ROOT}/network/squid/gen-acl.sh" 4 debugger "${WORK}/allow.txt"
check_not "entry covered by a wildcard is dropped" grep -qx 'api.github.com' "${SQUID_ACL_DIR}/vm3-domains.txt"
check "remove ACL for slot 3" "${SANDBOX_ROOT}/network/squid/gen-acl.sh" --remove 3
check_not "combined file no longer references slot 3" grep -q 'vm3_' "${SQUID_ACL_DIR}/vm-acls.conf"
check "combined file still references slot 4" grep -q 'vm4_' "${SQUID_ACL_DIR}/vm-acls.conf"

echo "== lib/agentconf.py"
for agent in $(list_agent_types); do
    check "validate ${agent}" python3 "${SANDBOX_ROOT}/lib/agentconf.py" validate "${agent}"
    python3 - "${agent}" > "${WORK}/customize-${agent}.sh" <<'PY'
import sys, os
sys.path.insert(0, os.path.join(os.environ["SANDBOX_ROOT"], "lib"))
import agentconf
print(agentconf.compile_customize_sh(agentconf.load_agent(sys.argv[1])))
PY
    check "customize.sh for ${agent} is valid bash" bash -n "${WORK}/customize-${agent}.sh"
    check "customize.sh for ${agent} carries the pins" grep -q '^fetch_verified()' "${WORK}/customize-${agent}.sh"
done

echo "== memory config"
check "memory backend, access policy + mental-model plan (test/test_memory_config.py)" \
    python3 "${SANDBOX_ROOT}/test/test_memory_config.py"
check "per-agent access policy logic (test/test_memory_policy.py)" \
    python3 "${SANDBOX_ROOT}/test/test_memory_policy.py"
check "Hindsight extension compiles" \
    python3 -m py_compile "${SANDBOX_ROOT}/lib/hindsight_ext/sandbox_memory_policy.py"

echo ""
echo "Results: ${PASS} passed, ${FAIL} failed"
[[ ${FAIL} -eq 0 ]]
