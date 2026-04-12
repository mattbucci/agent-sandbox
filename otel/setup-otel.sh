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

OTEL_VERSION="0.120.0"
OTEL_BIN="/usr/local/bin/otelcol-contrib"
OTEL_CONFIG="/etc/otel-collector.yaml"
OTEL_LOG_DIR="/var/log/otel"

echo "=== OpenTelemetry Collector Setup ==="

# Install otelcol-contrib
if [[ ! -f "${OTEL_BIN}" ]]; then
    echo "Downloading otelcol-contrib v${OTEL_VERSION}..."
    TMPDIR=$(mktemp -d)
    trap "rm -rf ${TMPDIR}" EXIT

    ARCH="linux_amd64"
    URL="https://github.com/open-telemetry/opentelemetry-collector-releases/releases/download/v${OTEL_VERSION}/otelcol-contrib_${OTEL_VERSION}_${ARCH}.tar.gz"

    curl -fsSL -o "${TMPDIR}/otelcol.tar.gz" "${URL}"
    tar -xzf "${TMPDIR}/otelcol.tar.gz" -C "${TMPDIR}"
    install -m 0755 "${TMPDIR}/otelcol-contrib" "${OTEL_BIN}"

    trap - EXIT
    rm -rf "${TMPDIR}"
    echo "Installed: ${OTEL_BIN}"
else
    echo "otelcol-contrib already installed at ${OTEL_BIN}"
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
systemctl start otel-collector.service

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
