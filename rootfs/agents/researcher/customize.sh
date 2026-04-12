#!/usr/bin/env bash
# Customize rootfs for the Researcher agent
set -euo pipefail
export DEBIAN_FRONTEND=noninteractive

echo "Installing researcher agent tools..."

apt-get update -qq
apt-get install -y --no-install-recommends \
    pandoc \
    lynx \
    w3m

# Readability CLI (extract article content from web pages)
npm install -g @phulin/readability-cli 2>/dev/null || \
    npm install -g readability-cli 2>/dev/null || true

# Python research tools
pip3 install --break-system-packages \
    requests beautifulsoup4 feedparser \
    arxiv scholarly \
    markdownify readability-lxml \
    rich httpx

echo "Researcher agent customization complete."
