#!/usr/bin/env bash
set -euo pipefail

# Install the pinned Grype release (sha256 verified).
# GRYPE_VERSION / GRYPE_SHA256 come from config/versions.env.

if command -v grype &>/dev/null; then
    echo "grype is already installed: $(grype version 2>/dev/null | head -1)"
    exit 0
fi

apt-get update -qq
apt-get install -y -qq curl ca-certificates

GRYPE_TMP="$(mktemp -d)"
fetch_verified \
    "https://github.com/anchore/grype/releases/download/v${GRYPE_VERSION}/grype_${GRYPE_VERSION}_linux_amd64.tar.gz" \
    "$(pinned_sha "${GRYPE_SHA256}")" \
    "${GRYPE_TMP}/grype.tar.gz"
tar -xzf "${GRYPE_TMP}/grype.tar.gz" -C "${GRYPE_TMP}" grype
install -m 0755 "${GRYPE_TMP}/grype" /usr/local/bin/grype
rm -rf "${GRYPE_TMP}"

echo "Installed: $(grype version 2>/dev/null | head -1)"
