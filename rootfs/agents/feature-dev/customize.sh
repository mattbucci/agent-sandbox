#!/usr/bin/env bash
# Customize rootfs for the Feature Dev agent
set -euo pipefail
export DEBIAN_FRONTEND=noninteractive

echo "Installing feature-dev agent tools..."

apt-get update -qq
apt-get install -y --no-install-recommends \
    docker.io \
    shellcheck

# GitHub CLI
curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg | dd of=/usr/share/keyrings/githubcli-archive-keyring.gpg 2>/dev/null
echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" \
    > /etc/apt/sources.list.d/github-cli.list
apt-get update -qq
apt-get install -y gh

# Additional dev tools
npm install -g typescript eslint prettier

pip3 install --break-system-packages \
    pytest black ruff mypy

echo "Feature-dev agent customization complete."
