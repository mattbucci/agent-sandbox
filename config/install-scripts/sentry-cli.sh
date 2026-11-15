#!/usr/bin/env bash
set -euo pipefail

# Install the pinned sentry-cli release binary (sha256 verified).
# SENTRY_CLI_VERSION / SENTRY_CLI_SHA256 come from config/versions.env.

if command -v sentry-cli &>/dev/null; then
    echo "sentry-cli is already installed: $(sentry-cli --version)"
    exit 0
fi

apt-get update -qq
apt-get install -y -qq curl ca-certificates

fetch_verified \
    "https://github.com/getsentry/sentry-cli/releases/download/${SENTRY_CLI_VERSION}/sentry-cli-Linux-x86_64" \
    "$(pinned_sha "${SENTRY_CLI_SHA256}")" \
    /usr/local/bin/sentry-cli
chmod 0755 /usr/local/bin/sentry-cli

echo "Installed: $(sentry-cli --version)"
