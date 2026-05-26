#!/usr/bin/env bash
set -euo pipefail

# =============================================================================
# docker-compose.sh — Make Docker fully functional inside the Firecracker VM.
#
# Runs in the rootfs chroot at build time (has full host network), paired with
# the `docker` capability which installs docker.io via apt.
#
# Two fixes are required for the stock Firecracker CI guest kernel (6.1.128):
#   1. iptables backend — the kernel has CONFIG_IP_NF_IPTABLES=y but
#      CONFIG_NF_TABLES is NOT set, so Ubuntu's default iptables-nft backend
#      fails. Docker needs the legacy backend.
#   2. Compose v2 — docker.io does not bundle the compose plugin.
# =============================================================================

# 1. Use the legacy iptables backend (nft backend unavailable in guest kernel).
if command -v update-alternatives &>/dev/null; then
    update-alternatives --set iptables  /usr/sbin/iptables-legacy  2>/dev/null || true
    update-alternatives --set ip6tables /usr/sbin/ip6tables-legacy 2>/dev/null || true
fi

# 2. Install the Docker Compose v2 CLI plugin (works alongside docker.io).
COMPOSE_VERSION="v2.29.7"
case "$(uname -m)" in
    x86_64)  COMPOSE_ARCH="x86_64" ;;
    aarch64) COMPOSE_ARCH="aarch64" ;;
    *)       COMPOSE_ARCH="$(uname -m)" ;;
esac

mkdir -p /usr/local/lib/docker/cli-plugins
curl -fsSL \
    "https://github.com/docker/compose/releases/download/${COMPOSE_VERSION}/docker-compose-linux-${COMPOSE_ARCH}" \
    -o /usr/local/lib/docker/cli-plugins/docker-compose
chmod +x /usr/local/lib/docker/cli-plugins/docker-compose

# Back-compat: expose the classic `docker-compose` entrypoint too.
ln -sf /usr/local/lib/docker/cli-plugins/docker-compose /usr/local/bin/docker-compose

# Baseline dockerd config. userland-proxy=false avoids needing docker-proxy on
# PATH (agent-init starts dockerd as PID-1 child with a minimal PATH, so the
# proxy binary in /usr/bin would not be found otherwise).
#
# NOTE on the guest kernel: Docker >=28's default bridge driver needs the
# iptables `raw` table (CONFIG_IP_NF_RAW), which the stock Firecracker CI kernel
# (fetch-kernel.sh) lacks -> bridge fails with "can't initialize iptables table
# raw". Build the docker-capable guest kernel with `kernel/build-kernel.sh`
# (adds IP_NF_RAW + NF_TABLES); then the default bridge + port publishing work
# and no extra options are needed here. If you must run on the stock kernel, add
# "iptables": false (containers run, but no bridge NAT / port-publishing —
# use --network host/none, or a user-defined bridge for container-to-container).
mkdir -p /etc/docker
cat > /etc/docker/daemon.json <<'JSON'
{
  "userland-proxy": false,
  "storage-driver": "overlay2"
}
JSON

echo "Installed: $(/usr/local/lib/docker/cli-plugins/docker-compose version 2>/dev/null || echo 'docker compose plugin')"
echo "iptables backend: $(readlink -f "$(command -v iptables)" 2>/dev/null || echo unknown)"
