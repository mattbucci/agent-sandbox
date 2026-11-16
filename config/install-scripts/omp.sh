#!/usr/bin/env bash
set -euo pipefail

# Install the pinned oh-my-pi (omp) release binary (sha256 verified).
# OMP_VERSION / OMP_SHA256 come from config/versions.env.
#
# omp is a single self-contained executable (it embeds its Bun runtime and
# native addons), so there is nothing else to install. Agents select it with
# `agent.harness: omp`; the in-VM gateway then drives `omp --mode rpc`
# (rootfs/overlay/opt/agent/omp_agent.py).

if command -v omp &>/dev/null && [[ "$(omp --version 2>/dev/null)" == "omp/${OMP_VERSION#v}" ]]; then
    echo "omp is already installed: $(omp --version)"
    exit 0
fi

apt-get update -qq
apt-get install -y -qq curl ca-certificates

fetch_verified \
    "https://github.com/can1357/oh-my-pi/releases/download/${OMP_VERSION}/omp-linux-x64" \
    "$(pinned_sha "${OMP_SHA256}")" \
    /usr/local/bin/omp
chmod 0755 /usr/local/bin/omp

# Run once as a smoke test: a binary that cannot start on this libc must fail
# the image build, not the first request.
echo "Installed: $(HOME=/tmp omp --version)"
