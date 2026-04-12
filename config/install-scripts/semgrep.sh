#!/usr/bin/env bash
set -euo pipefail

# Install Semgrep via pip

if command -v semgrep &>/dev/null; then
    echo "semgrep is already installed: $(semgrep --version)"
    exit 0
fi

apt-get update -qq
apt-get install -y -qq python3 python3-pip

pip3 install --break-system-packages semgrep

echo "Installed: $(semgrep --version)"
