#!/usr/bin/env bash
set -euo pipefail

# Install Trivy via Aqua Security install script

if command -v trivy &>/dev/null; then
    echo "trivy is already installed: $(trivy --version | head -1)"
    exit 0
fi

apt-get update -qq
apt-get install -y -qq curl

curl -fsSL https://raw.githubusercontent.com/aquasecurity/trivy/main/contrib/install.sh | sh -s -- -b /usr/local/bin

echo "Installed: $(trivy --version | head -1)"
