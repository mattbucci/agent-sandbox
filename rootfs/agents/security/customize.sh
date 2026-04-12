#!/usr/bin/env bash
# Customize rootfs for the Security Engineer agent
set -euo pipefail
export DEBIAN_FRONTEND=noninteractive

echo "Installing security agent tools..."

apt-get update -qq
apt-get install -y --no-install-recommends \
    nmap nikto \
    net-tools whois dnsutils \
    python3-pip

# Trivy (vulnerability scanner)
curl -sfL https://raw.githubusercontent.com/aquasecurity/trivy/main/contrib/install.sh | sh -s -- -b /usr/local/bin 2>/dev/null || \
    echo "WARNING: trivy install failed, may need manual setup"

# Grype (container vulnerability scanner)
curl -sSfL https://raw.githubusercontent.com/anchore/grype/main/install.sh | sh -s -- -b /usr/local/bin 2>/dev/null || \
    echo "WARNING: grype install failed, may need manual setup"

# Semgrep (static analysis)
pip3 install --break-system-packages semgrep 2>/dev/null || \
    echo "WARNING: semgrep install failed"

# GitHub CLI (for security advisories)
curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg | dd of=/usr/share/keyrings/githubcli-archive-keyring.gpg 2>/dev/null
echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" \
    > /etc/apt/sources.list.d/github-cli.list
apt-get update -qq
apt-get install -y gh

# Python security tools
pip3 install --break-system-packages \
    pip-audit safety bandit \
    requests httpx

# npm audit is built into npm
npm install -g snyk 2>/dev/null || true

echo "Security agent customization complete."
