#!/usr/bin/env bash
# =============================================================================
# build-kernel.sh — Build a Firecracker guest kernel with the netfilter tables
# Docker's bridge driver needs.
#
# The stock Firecracker CI kernel (fetch-kernel.sh) is built WITHOUT
# CONFIG_IP_NF_RAW and CONFIG_NF_TABLES, so `dockerd` cannot set up the default
# bridge ("can't initialize iptables table `raw`") and you must fall back to
# `"iptables": false`. This rebuilds the same kernel config with those tables
# enabled, producing kernel/vmlinux.
#
# Base config: the embedded IKCONFIG of the current kernel/vmlinux (so all the
# working microVM/overlay/veth/virtio options are preserved verbatim).
# =============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
KVER="${KVER:-6.1.128}"
BUILD_DIR="${BUILD_DIR:-/tmp/kbuild}"
SRC="${BUILD_DIR}/linux-${KVER}"
TARBALL="${BUILD_DIR}/linux-${KVER}.tar.xz"
URL="https://cdn.kernel.org/pub/linux/kernel/v6.x/linux-${KVER}.tar.xz"
JOBS="$(nproc)"
OUT="${SCRIPT_DIR}/vmlinux"

mkdir -p "${BUILD_DIR}"

# 1. Source
[[ -f "${TARBALL}" ]] || curl -fSL -o "${TARBALL}" "${URL}"
[[ -d "${SRC}" ]]     || tar -C "${BUILD_DIR}" -xf "${TARBALL}"

# 2. Base config from the current kernel's embedded IKCONFIG
BASE_CFG="${BUILD_DIR}/base.config"
if [[ ! -f "${BASE_CFG}" ]]; then
    python3 - "${OUT}" "${BASE_CFG}" <<'PY'
import sys, zlib
d = open(sys.argv[1], "rb").read()
m = d.find(b"IKCFG_ST"); g = d.find(b"\x1f\x8b\x08", m)
cfg = zlib.decompressobj(16 + zlib.MAX_WBITS).decompress(d[g:]).decode("utf-8", "replace")
open(sys.argv[2], "w").write(cfg)
PY
fi
cp "${BASE_CFG}" "${SRC}/.config"

# 3. Enable netfilter tables Docker needs (raw table + nftables family)
cd "${SRC}"

# GCC >=15 defaults to C23, where bool/true/false are keywords; 6.1's main build
# pins -std=gnu11 but REALMODE_CFLAGS does not, so realmode fails to compile.
# Pin it there too (no-op on older toolchains).
grep -qE 'REALMODE_CFLAGS \+= -std=gnu11' arch/x86/Makefile || \
    sed -i '/^export REALMODE_CFLAGS/i REALMODE_CFLAGS += -std=gnu11' arch/x86/Makefile

ENABLE=(
    IP_NF_RAW                                   # the `raw` table (legacy iptables)
    NF_TABLES NF_TABLES_INET NF_TABLES_IPV4 NF_TABLES_IPV6 NF_TABLES_NETDEV
    NFT_COMPAT NFT_NAT NFT_CHAIN_NAT NFT_MASQ NFT_REDIR NFT_CT
    VIRTIO_MMIO_CMDLINE_DEVICES                 # discover virtio-mmio from boot args
)
for c in "${ENABLE[@]}"; do scripts/config --enable "CONFIG_${c}"; done

# A from-source vanilla kernel can't parse Firecracker's custom ACPI tables
# (the stock CI kernel carries FC-specific patches), so ACPI device discovery
# fails and /dev/vda never appears. Bake `acpi=off` into the kernel's built-in
# command line (appended to FC's args) so it discovers devices from the
# `virtio_mmio.device=` boot args instead — no change needed to config-template.
scripts/config --enable  CONFIG_CMDLINE_BOOL
scripts/config --set-str CONFIG_CMDLINE "acpi=off"
scripts/config --enable  CONFIG_CMDLINE_EXTEND

scripts/config --disable CONFIG_WERROR          # tolerate new-compiler warnings
make LLVM="${LLVM:-1}" olddefconfig

# 4. Verify the critical options actually stuck
for c in IP_NF_RAW NF_TABLES NFT_COMPAT VIRTIO_MMIO_CMDLINE_DEVICES; do
    grep -q "^CONFIG_${c}=y" .config || { echo "ERROR: CONFIG_${c} did not enable"; exit 1; }
done

# 5. Build the uncompressed ELF vmlinux Firecracker boots.
# Built with clang/LLVM by default (LLVM=1): Arch's bleeding-edge gcc miscompiles
# this older kernel. Set LLVM=0 to use gcc (the realmode -std=gnu11 patch above
# is needed there for gcc >=15).
make LLVM="${LLVM:-1}" -j"${JOBS}" vmlinux

cp -f "${OUT}" "${OUT}.fcci.bak" 2>/dev/null || true   # keep the stock CI kernel
cp -f vmlinux "${OUT}"
echo "=== Built guest kernel with IP_NF_RAW + NF_TABLES -> ${OUT} ==="
ls -lh "${OUT}"
