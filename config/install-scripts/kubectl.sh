#!/usr/bin/env bash
set -euo pipefail

# Install kubectl via Kubernetes apt repository

if command -v kubectl &>/dev/null; then
    echo "kubectl is already installed: $(kubectl version --client --short 2>/dev/null || kubectl version --client)"
    exit 0
fi

apt-get update -qq
apt-get install -y -qq curl gnupg apt-transport-https ca-certificates

mkdir -p /etc/apt/keyrings
curl -fsSL https://pkgs.k8s.io/core:/stable:/v1.31/deb/Release.key \
    | gpg --dearmor -o /etc/apt/keyrings/kubernetes-apt-keyring.gpg

echo "deb [signed-by=/etc/apt/keyrings/kubernetes-apt-keyring.gpg] https://pkgs.k8s.io/core:/stable:/v1.31/deb/ /" \
    > /etc/apt/sources.list.d/kubernetes.list

apt-get update -qq
apt-get install -y -qq kubectl

echo "Installed: $(kubectl version --client 2>/dev/null | head -1)"
