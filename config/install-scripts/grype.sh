#!/usr/bin/env bash
set -euo pipefail

# Install Grype via Anchore install script

if command -v grype &>/dev/null; then
    echo "grype is already installed: $(grype version 2>/dev/null | head -1)"
    exit 0
fi

apt-get update -qq
apt-get install -y -qq curl

curl -sSfL https://raw.githubusercontent.com/anchore/grype/main/install.sh | sh -s -- -b /usr/local/bin

echo "Installed: $(grype version 2>/dev/null | head -1)"
