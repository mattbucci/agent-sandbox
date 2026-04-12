#!/usr/bin/env bash
set -euo pipefail

# Install Helm via the official get-helm-3 script

if command -v helm &>/dev/null; then
    echo "helm is already installed: $(helm version --short)"
    exit 0
fi

apt-get update -qq
apt-get install -y -qq curl

curl -fsSL https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 | bash

echo "Installed: $(helm version --short)"
