#!/usr/bin/env bash
set -euo pipefail

# Install sentry-cli via the official install script

if command -v sentry-cli &>/dev/null; then
    echo "sentry-cli is already installed: $(sentry-cli --version)"
    exit 0
fi

apt-get update -qq
apt-get install -y -qq curl

curl -sL https://sentry.io/get-cli/ | bash

echo "Installed: $(sentry-cli --version)"
