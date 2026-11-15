#!/usr/bin/env bash
set -euo pipefail

# Install the pinned Trivy release (sha256 verified).
# TRIVY_VERSION / TRIVY_SHA256 come from config/versions.env.

if command -v trivy &>/dev/null; then
    echo "trivy is already installed: $(trivy --version | head -1)"
    exit 0
fi

apt-get update -qq
apt-get install -y -qq curl ca-certificates

TRIVY_TMP="$(mktemp -d)"
fetch_verified \
    "https://github.com/aquasecurity/trivy/releases/download/v${TRIVY_VERSION}/trivy_${TRIVY_VERSION}_Linux-64bit.tar.gz" \
    "$(pinned_sha "${TRIVY_SHA256}")" \
    "${TRIVY_TMP}/trivy.tar.gz"
tar -xzf "${TRIVY_TMP}/trivy.tar.gz" -C "${TRIVY_TMP}" trivy
install -m 0755 "${TRIVY_TMP}/trivy" /usr/local/bin/trivy
rm -rf "${TRIVY_TMP}"

echo "Installed: $(trivy --version | head -1)"
