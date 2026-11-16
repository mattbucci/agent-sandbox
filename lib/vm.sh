#!/usr/bin/env bash
# =============================================================================
# lib/vm.sh — the one implementation of "bring a VM's host side up / down".
#
# Both entry points use these functions, so they cannot drift apart:
#   vm/launch.sh               — a new VM
#   bin/agent-sandbox-boot.sh  — VMs restored from state/vms/ after a reboot
#
# Requires lib/common.sh (ensure_global_compiled already called) and
# lib/network.sh to be sourced first.
# =============================================================================

# Where jailer builds its per-VM chroots. Must be on the same filesystem as
# STATE_DIR for the rootfs hard link (otherwise the rootfs is copied in).
JAILER_BASE_DIR="${JAILER_BASE_DIR:-/srv/jailer}"

# vm_use_jailer — true when VMs should run under the jailer (the default
# whenever the binary exists; NO_JAILER=1 opts out for development).
vm_use_jailer() {
    [[ "${NO_JAILER:-0}" != "1" ]] && [[ -x "${JAILER_BIN:-/usr/local/bin/jailer}" ]]
}

# vm_jail_dir <instance-id> — the jailer directory owned by one VM.
vm_jail_dir() {
    echo "${JAILER_BASE_DIR}/$(basename "${FIRECRACKER_BIN:-firecracker}")/$1"
}

# vm_remove_jail <instance-id> — delete a VM's jailer chroot. The chroot holds
# a hard link to the instance rootfs, so leaving it behind leaks the whole
# image even after the VM state directory is removed.
vm_remove_jail() {
    local jail
    jail="$(vm_jail_dir "$1")"
    # Guard: only ever remove a directory strictly inside the jailer base.
    if [[ -n "$1" && "${jail}" == "${JAILER_BASE_DIR}"/*/* && -d "${jail}" ]]; then
        rm -rf "${jail}"
    fi
}

# vm_render_config <out> <kernel> <rootfs> <log> <metrics>
# Renders vm/config-template.json. The VM identity comes from the caller's
# variables: VCPUS MEM_MB TAP_NAME GUEST_MAC VM_IP GATEWAY_IP VM_NAME NO_AGENT.
vm_render_config() {
    local out="$1" kernel="$2" rootfs="$3" log="$4" metrics="$5"
    sed \
        -e "s|__KERNEL_PATH__|${kernel}|g" \
        -e "s|__ROOTFS_PATH__|${rootfs}|g" \
        -e "s|__VCPUS__|${VCPUS}|g" \
        -e "s|__MEM_MB__|${MEM_MB}|g" \
        -e "s|__TAP_NAME__|${TAP_NAME}|g" \
        -e "s|__GUEST_MAC__|${GUEST_MAC}|g" \
        -e "s|__VM_IP__|${VM_IP}|g" \
        -e "s|__GATEWAY_IP__|${GATEWAY_IP}|g" \
        -e "s|__DNS_IP__|${GATEWAY_IP}|g" \
        -e "s|__HOSTNAME__|${VM_NAME}|g" \
        -e "s|__NO_AGENT__|${NO_AGENT:-0}|g" \
        -e "s|__LOG_PATH__|${log}|g" \
        -e "s|__METRICS_PATH__|${metrics}|g" \
        "${SANDBOX_ROOT}/vm/config-template.json" > "${out}"
}

# vm_network_up <slot> <tap> <gateway-ip> <agent-type> [llm-ip-fallback]
# TAP device, per-VM firewall rules and the Squid egress ACL. Idempotent: any
# rules left over for this slot are removed first. The ACL always comes from
# the compiled build/<type>/allowlist.txt — the same file for launch and
# restore.
vm_network_up() {
    local slot="$1" tap="$2" gateway_ip="$3" agent_type="$4" llm_ip_fallback="${5:-}"

    create_tap "${tap}" "${gateway_ip}"
    remove_vm_nftables "${slot}"
    add_vm_nftables "${slot}" "${tap}" "${gateway_ip}" "${llm_ip_fallback}"

    ensure_compiled "${agent_type}"
    "${SANDBOX_ROOT}/network/squid/gen-acl.sh" "${slot}" "${agent_type}" \
        "${SANDBOX_ROOT}/build/${agent_type}/allowlist.txt"
}

# vm_start_firecracker — start the VMM for the caller's VM, jailed when
# possible. Reads INSTANCE_ID SLOT VM_STATE_DIR LOG_FILE plus the identity
# variables vm_render_config uses. Sets FC_PID and FC_SOCKET.
vm_start_firecracker() {
    local rootfs="${VM_STATE_DIR}/rootfs.ext4"
    mkdir -p "$(dirname "${LOG_FILE}")"
    touch "${LOG_FILE}"

    if vm_use_jailer; then
        # Jailer chroots Firecracker and runs it from the chroot root, so the
        # kernel, rootfs and config must live INSIDE the chroot and be
        # referenced by chroot-relative paths.
        local jail_root jail_uid jail_gid
        jail_root="$(vm_jail_dir "${INSTANCE_ID}")/root"
        jail_uid=$((10000 + SLOT))
        jail_gid=$((10000 + SLOT))

        # Dedicated unprivileged uid/gid per VM (jailer drops to it)
        id -u "fc-${SLOT}" &>/dev/null || \
            useradd -r -u "${jail_uid}" -s /usr/sbin/nologin "fc-${SLOT}" 2>/dev/null || true

        # Fresh chroot tree (jailer copies the firecracker binary in itself)
        vm_remove_jail "${INSTANCE_ID}"
        mkdir -p "${jail_root}"

        # Kernel: copy in. Rootfs: hard-link (shares the inode with the state
        # copy, so the VM's writes persist in state/ for a later restore),
        # falling back to a copy across filesystems.
        cp --reflink=auto "${KERNEL_PATH}" "${jail_root}/vmlinux"
        ln -f "${rootfs}" "${jail_root}/rootfs.ext4" 2>/dev/null \
            || cp --reflink=auto "${rootfs}" "${jail_root}/rootfs.ext4"

        # Firecracker's own log + metrics live in the chroot. The guest serial
        # console goes to the jailer process stdout (${LOG_FILE}.stdout).
        : > "${jail_root}/firecracker.log"
        : > "${jail_root}/metrics"

        vm_render_config "${jail_root}/vm-config.json" \
            /vmlinux /rootfs.ext4 /firecracker.log /metrics

        # Everything the dropped-privilege process touches must be owned by it.
        chown -R "${jail_uid}:${jail_gid}" "$(vm_jail_dir "${INSTANCE_ID}")"

        # Jailer creates the API socket inside the chroot.
        FC_SOCKET="${jail_root}/run/firecracker.socket"

        "${JAILER_BIN}" \
            --id "${INSTANCE_ID}" \
            --exec-file "${FIRECRACKER_BIN}" \
            --uid "${jail_uid}" \
            --gid "${jail_gid}" \
            --chroot-base-dir "${JAILER_BASE_DIR}" \
            --cgroup-version 2 \
            -- \
            --config-file vm-config.json \
            &>"${LOG_FILE}.stdout" &
        FC_PID=$!
    else
        # Development mode: raw firecracker (no jailer)
        echo "  (jailer not available or NO_JAILER=1 — using raw firecracker)"
        local config="${VM_STATE_DIR}/vm-config.json"
        local metrics="${VM_STATE_DIR}/metrics.fifo"
        FC_SOCKET="${VM_STATE_DIR}/firecracker.sock"

        # Firecracker requires the metrics FIFO to exist.
        rm -f "${FC_SOCKET}" "${metrics}"
        mkfifo "${metrics}"
        vm_render_config "${config}" "${KERNEL_PATH}" "${rootfs}" "${LOG_FILE}" "${metrics}"

        "${FIRECRACKER_BIN}" \
            --api-sock "${FC_SOCKET}" \
            --config-file "${config}" \
            &>"${LOG_FILE}.stdout" &
        FC_PID=$!
    fi

    echo "${FC_PID}" > "${VM_STATE_DIR}/firecracker.pid"
}

# vm_start_novnc <port> <vm-ip> — websockify proxy for the VM desktop.
# Sets WS_PID.
vm_start_novnc() {
    websockify --web /opt/novnc "0.0.0.0:$1" "$2:5900" &>/dev/null &
    WS_PID=$!
    echo "${WS_PID}" > "${VM_STATE_DIR}/websockify.pid"
}

# vm_restore <info.json> — bring a previously-running VM back after a reboot,
# through exactly the same network/jailer/VMM path as a fresh launch. The
# instance rootfs in state/ is reused as-is, so the VM keeps its disk.
# Returns non-zero (after logging why) if the VM could not be restored.
vm_restore() {
    local info_file="$1"

    INSTANCE_ID=$(jq -r '.instance_id' "${info_file}")
    VM_NAME=$(jq -r '.name // .instance_id' "${info_file}")
    SLOT=$(jq -r '.slot' "${info_file}")
    VM_IP=$(jq -r '.vm_ip' "${info_file}")
    GATEWAY_IP=$(jq -r '.gateway_ip' "${info_file}")
    TAP_NAME=$(jq -r '.tap_device' "${info_file}")
    VCPUS=$(jq -r '.vcpus' "${info_file}")
    MEM_MB=$(jq -r '.mem_mb' "${info_file}")
    GUEST_MAC=$(jq -r '.guest_mac' "${info_file}")
    NO_AGENT=0
    VM_STATE_DIR="$(dirname "${info_file}")"
    LOG_FILE="${LOG_DIR}/${INSTANCE_ID}.log"
    local agent_type novnc_port old_pid llm_ip
    llm_ip=$(jq -r '.llm_host_ip // ""' "${info_file}")
    agent_type=$(jq -r '.agent_type' "${info_file}")
    novnc_port=$(jq -r '.novnc_port' "${info_file}")
    old_pid=$(jq -r '.firecracker_pid' "${info_file}")

    if ! agent_type_exists "${agent_type}"; then
        log_warn "Skipping ${INSTANCE_ID}: agent type '${agent_type}' no longer exists"
        return 1
    fi
    if [[ ! -f "${VM_STATE_DIR}/rootfs.ext4" ]]; then
        log_warn "Skipping ${INSTANCE_ID}: rootfs missing"
        return 1
    fi
    if [[ "${old_pid}" =~ ^[0-9]+$ ]] && kill -0 "${old_pid}" 2>/dev/null \
        && grep -qa firecracker "/proc/${old_pid}/cmdline" 2>/dev/null; then
        # Re-running the boot service on a live host: leave the VM (and its
        # TAP device) alone, but re-add its firewall rules and ACL, which the
        # base ruleset reset has just dropped.
        log_info "${INSTANCE_ID} already running (PID ${old_pid}) — refreshing network rules only"
        remove_vm_nftables "${SLOT}"
        add_vm_nftables "${SLOT}" "${TAP_NAME}" "${GATEWAY_IP}" "${llm_ip}"
        "${SANDBOX_ROOT}/network/squid/gen-acl.sh" "${SLOT}" "${agent_type}" \
            "${SANDBOX_ROOT}/build/${agent_type}/allowlist.txt" || true
        return 0
    fi

    log_info "Restoring VM: ${INSTANCE_ID} (${agent_type}, slot ${SLOT})"
    vm_network_up "${SLOT}" "${TAP_NAME}" "${GATEWAY_IP}" "${agent_type}" "${llm_ip}" || return 1
    vm_start_firecracker || return 1

    sleep 1
    if ! kill -0 "${FC_PID}" 2>/dev/null; then
        log_error "${INSTANCE_ID}: Firecracker exited immediately — see ${LOG_FILE}.stdout"
        cleanup_vm_network "${SLOT}" "${TAP_NAME}"
        return 1
    fi

    vm_start_novnc "${novnc_port}" "${VM_IP}"

    jq --argjson fc "${FC_PID}" --argjson ws "${WS_PID}" --arg sock "${FC_SOCKET}" \
        --arg at "$(date -Iseconds)" \
        '.firecracker_pid = $fc | .websockify_pid = $ws | .fc_socket = $sock | .restored_at = $at' \
        "${info_file}" > "${info_file}.tmp" && mv -f "${info_file}.tmp" "${info_file}"

    log_info "  ${INSTANCE_ID} restored (FC PID ${FC_PID}, noVNC :${novnc_port})"
}
