#!/usr/bin/env bash
set -euo pipefail

# Install Terraform via HashiCorp apt repository

if command -v terraform &>/dev/null; then
    echo "terraform is already installed: $(terraform version | head -1)"
    exit 0
fi

apt-get update -qq
apt-get install -y -qq curl gnupg software-properties-common

curl -fsSL https://apt.releases.hashicorp.com/gpg | gpg --dearmor -o /usr/share/keyrings/hashicorp-archive-keyring.gpg

echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/hashicorp-archive-keyring.gpg] https://apt.releases.hashicorp.com $(lsb_release -cs) main" \
    > /etc/apt/sources.list.d/hashicorp.list

apt-get update -qq
apt-get install -y -qq terraform

echo "Installed: $(terraform version | head -1)"
