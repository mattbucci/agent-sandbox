#!/usr/bin/env bash
set -euo pipefail

# Install GitHub CLI (gh) via apt keyring

if command -v gh &>/dev/null; then
    echo "gh is already installed: $(gh --version | head -1)"
    exit 0
fi

apt-get update -qq
apt-get install -y -qq curl gnupg

mkdir -p /etc/apt/keyrings
curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg \
    -o /etc/apt/keyrings/githubcli-archive-keyring.gpg
chmod go+r /etc/apt/keyrings/githubcli-archive-keyring.gpg

echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" \
    > /etc/apt/sources.list.d/github-cli.list

apt-get update -qq
apt-get install -y -qq gh

echo "Installed: $(gh --version | head -1)"
