#!/usr/bin/env bash
# Customize rootfs for the Debugger agent
# Runs inside chroot during build-agent
set -euo pipefail
export DEBIAN_FRONTEND=noninteractive

echo "Installing debugger agent tools..."

apt-get update -qq
apt-get install -y --no-install-recommends \
    gdb strace ltrace valgrind \
    python3-pip \
    tcpdump

# Install sentry-cli
curl -sL https://sentry.io/get-cli/ | bash 2>/dev/null || \
    npm install -g @sentry/cli

# Python debugging tools
pip3 install --break-system-packages \
    ipdb pdbpp rich httpx

echo "Debugger agent customization complete."
