#!/usr/bin/env bash
# =============================================================================
# setup-otel.sh — Install and configure OpenTelemetry Collector on the host
#
# The collector listens on 0.0.0.0:4317 (gRPC) and 0.0.0.0:4318 (HTTP)
# Agent VMs send traces to their gateway IP on these ports.
# =============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
SANDBOX_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

source "${SANDBOX_ROOT}/lib/versions.sh"

OTEL_VERSION="${OTELCOL_VERSION}"
OTEL_BIN="/usr/local/bin/otelcol-contrib"
OTEL_CONFIG="/etc/otel-collector.yaml"
OTEL_LOG_DIR="/var/log/otel"

echo "=== OpenTelemetry Collector Setup ==="

# Install (or upgrade to) the pinned otelcol-contrib
INSTALLED_VERSION="$("${OTEL_BIN}" --version 2>/dev/null | grep -oE '[0-9]+\.[0-9]+\.[0-9]+' | head -1 || true)"
UPGRADED=0
if [[ "${INSTALLED_VERSION}" != "${OTEL_VERSION}" ]]; then
    echo "Downloading otelcol-contrib v${OTEL_VERSION} (sha256 verified)${INSTALLED_VERSION:+, replacing v${INSTALLED_VERSION}}..."
    WORK_DIR=$(mktemp -d)
    trap 'rm -rf "${WORK_DIR}"' EXIT

    case "$(uname -m)" in
        aarch64|arm64) ARCH="linux_arm64" ;;
        *)             ARCH="linux_amd64" ;;
    esac
    URL="https://github.com/open-telemetry/opentelemetry-collector-releases/releases/download/v${OTEL_VERSION}/otelcol-contrib_${OTEL_VERSION}_${ARCH}.tar.gz"

    fetch_verified "${URL}" "$(pinned_sha "${OTELCOL_SHA256}")" "${WORK_DIR}/otelcol.tar.gz"
    tar -xzf "${WORK_DIR}/otelcol.tar.gz" -C "${WORK_DIR}"

    # Refuse to install a collector that rejects our config: a failed upgrade
    # must not take tracing down.
    "${WORK_DIR}/otelcol-contrib" validate --config "${SCRIPT_DIR}/otel-collector.yaml"

    install -m 0755 "${WORK_DIR}/otelcol-contrib" "${OTEL_BIN}"

    trap - EXIT
    rm -rf "${WORK_DIR}"
    UPGRADED=1
    echo "Installed: ${OTEL_BIN} (v${OTEL_VERSION})"
else
    echo "otelcol-contrib v${OTEL_VERSION} already installed at ${OTEL_BIN}"
fi

# Install config
cp "${SCRIPT_DIR}/otel-collector.yaml" "${OTEL_CONFIG}"
echo "Config installed at ${OTEL_CONFIG}"

# Create log directory
mkdir -p "${OTEL_LOG_DIR}"

# Create systemd service
cat > /etc/systemd/system/otel-collector.service <<'EOF'
[Unit]
Description=OpenTelemetry Collector (Agent Sandbox)
After=network.target

[Service]
Type=simple
ExecStart=/usr/local/bin/otelcol-contrib --config /etc/otel-collector.yaml
Restart=on-failure
RestartSec=5
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable otel-collector.service
if [[ ${UPGRADED} -eq 1 ]]; then
    systemctl restart otel-collector.service   # pick up the new binary
else
    systemctl start otel-collector.service
fi

sleep 2
if systemctl is-active --quiet otel-collector.service; then
    echo ""
    echo "=== OTel Collector running ==="
    echo "  gRPC: 0.0.0.0:4317"
    echo "  HTTP: 0.0.0.0:4318"
    echo "  Traces log: ${OTEL_LOG_DIR}/traces.jsonl"
    echo ""
    echo "Agent VMs auto-detect the collector at http://<gateway-ip>:4318"
else
    echo "WARNING: otel-collector failed to start"
    journalctl -eu otel-collector.service --no-pager | tail -10
fi
