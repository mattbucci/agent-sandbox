#!/usr/bin/env bash
set -euo pipefail

# Install the pinned Helm release (sha256 verified).
# HELM_VERSION / HELM_SHA256 come from config/versions.env.

if command -v helm &>/dev/null; then
    echo "helm is already installed: $(helm version --short)"
    exit 0
fi

apt-get update -qq
apt-get install -y -qq curl ca-certificates

HELM_TMP="$(mktemp -d)"
fetch_verified \
    "https://get.helm.sh/helm-${HELM_VERSION}-linux-amd64.tar.gz" \
    "$(pinned_sha "${HELM_SHA256}")" \
    "${HELM_TMP}/helm.tar.gz"
tar -xzf "${HELM_TMP}/helm.tar.gz" -C "${HELM_TMP}"
install -m 0755 "${HELM_TMP}/linux-amd64/helm" /usr/local/bin/helm
rm -rf "${HELM_TMP}"

echo "Installed: $(helm version --short)"
