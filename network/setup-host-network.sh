#!/usr/bin/env bash
# =============================================================================
# setup-host-network.sh — One-time host network configuration
#
# Sets up:
#   - IP forwarding
#   - Base nftables rules (default deny for VM egress)
#   - NAT masquerade for Squid outbound
#   - dnsmasq for VM DNS
#   - Squid proxy for domain-based HTTPS filtering
#
# Must be run as root.
# =============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
SANDBOX_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

source "${SANDBOX_ROOT}/lib/common.sh"
ensure_global_compiled

HOST_IFACE="${HOST_IFACE:-enp12s0}"

if [[ $EUID -ne 0 ]]; then
    echo "ERROR: Must run as root."
    exit 1
fi

echo "=== Host Network Setup ==="

# --- 1. IP Forwarding ---
echo "[1/5] Enabling IP forwarding..."
sysctl -w net.ipv4.ip_forward=1
if ! grep -q "net.ipv4.ip_forward=1" /etc/sysctl.conf 2>/dev/null; then
    echo "net.ipv4.ip_forward=1" >> /etc/sysctl.conf
fi

# --- 2. Install dependencies ---
echo "[2/5] Checking dependencies..."

install_pkg() {
    local pkg="$1"
    if ! command -v "${pkg}" &>/dev/null && ! rpm -q "${pkg}" &>/dev/null 2>&1; then
        echo "  Installing ${pkg}..."
        if command -v dnf &>/dev/null; then
            dnf install -y "${pkg}" >/dev/null 2>&1
        elif command -v apt-get &>/dev/null; then
            apt-get install -y "${pkg}" >/dev/null 2>&1
        elif command -v pacman &>/dev/null; then
            pacman -S --noconfirm --needed "${pkg}" >/dev/null 2>&1
        fi
    fi
}

# pkg name may differ from the binary name (e.g. on Arch the binary is `nft`).
ensure_cmd() {
    local cmd="$1" pkg="$2"
    if ! command -v "${cmd}" &>/dev/null; then
        echo "  Installing ${pkg} (provides ${cmd})..."
        if command -v pacman &>/dev/null; then
            pacman -S --noconfirm --needed "${pkg}" >/dev/null 2>&1
        elif command -v dnf &>/dev/null; then
            dnf install -y "${pkg}" >/dev/null 2>&1
        elif command -v apt-get &>/dev/null; then
            apt-get install -y "${pkg}" >/dev/null 2>&1
        fi
    fi
}

ensure_cmd nft   nftables
ensure_cmd squid squid
ensure_cmd dnsmasq dnsmasq
ensure_cmd jq    jq        # used host-side by sandbox-ctl + the boot service

# --- 3. nftables base rules ---
echo "[3/5] Configuring nftables..."

# Flush any existing vm_filter table
nft delete table ip vm_filter 2>/dev/null || true

nft -f - <<'NFT'
table ip vm_filter {
    chain input {
        type filter hook input priority 0; policy accept;

        # Block VMs from reaching host SSH
        iifname "tap-vm*" tcp dport 22 drop comment "block-vm-to-host-ssh"
    }

    chain forward {
        type filter hook forward priority 0; policy drop;

        # Allow established/related connections back to VMs
        ct state established,related accept
    }

    chain prerouting {
        type nat hook prerouting priority -100;
        # Per-VM DNAT rules added dynamically by vm/launch.sh
    }

    chain postrouting {
        type nat hook postrouting priority 100;
    }
}
NFT

# Add NAT masquerade for outbound traffic from Squid
nft add rule ip vm_filter postrouting oifname "${HOST_IFACE}" masquerade

echo "  nftables base rules applied."
echo "  Default policy: DROP all VM forward traffic."
echo "  Per-VM rules will be added dynamically at launch."

# --- 4. Squid configuration ---
echo "[4/5] Configuring Squid proxy..."

SQUID_CONF="/etc/squid/squid.conf"
SQUID_SSL_DIR="/etc/squid/ssl"
SQUID_ACL_DIR="/etc/squid/acls"

mkdir -p "${SQUID_SSL_DIR}" "${SQUID_ACL_DIR}"

# Generate self-signed CA for Squid SSL bump (peek-and-splice only, no MITM)
if [[ ! -f "${SQUID_SSL_DIR}/squid-ca.pem" ]]; then
    echo "  Generating Squid SSL CA certificate..."
    openssl req -new -newkey rsa:2048 -days 3650 -nodes -x509 \
        -subj "/CN=Agent Sandbox Squid CA" \
        -keyout "${SQUID_SSL_DIR}/squid-ca-key.pem" \
        -out "${SQUID_SSL_DIR}/squid-ca.pem" 2>/dev/null
    cat "${SQUID_SSL_DIR}/squid-ca-key.pem" "${SQUID_SSL_DIR}/squid-ca.pem" \
        > "${SQUID_SSL_DIR}/squid-ca-combined.pem"
    chmod 600 "${SQUID_SSL_DIR}"/*.pem
fi

# Locate security_file_certgen (path differs: /usr/lib64 on RH, /usr/lib on Arch/Debian)
CERTGEN=""
for c in /usr/lib64/squid/security_file_certgen /usr/lib/squid/security_file_certgen; do
    [[ -x "$c" ]] && CERTGEN="$c" && break
done

# Determine the user Squid runs as (Arch: proxy, Debian/RH: proxy/squid)
SQUID_USER="proxy"
id squid &>/dev/null && SQUID_USER="squid"

# Initialize Squid SSL database
mkdir -p /var/lib/squid
if [[ ! -d /var/lib/squid/ssl_db && -n "$CERTGEN" ]]; then
    "$CERTGEN" -c -s /var/lib/squid/ssl_db -M 64MB 2>/dev/null || true
fi
chown -R "${SQUID_USER}:${SQUID_USER}" /var/lib/squid 2>/dev/null || true

# Install Squid config, rewriting the certgen path to the one we found on this host
cp "${SCRIPT_DIR}/squid/squid-base.conf" "${SQUID_CONF}"
if [[ -n "$CERTGEN" ]]; then
    sed -i "s|^sslcrtd_program .*security_file_certgen|sslcrtd_program ${CERTGEN}|" "${SQUID_CONF}"
fi

# Create placeholder ACL files so Squid can start before any VM is launched.
# (squid-base.conf references all-allowed-domains.txt and includes vm-acls.conf)
[[ -f "${SQUID_ACL_DIR}/all-allowed-domains.txt" ]] || \
    echo "# No VMs running yet" > "${SQUID_ACL_DIR}/all-allowed-domains.txt"
[[ -f "${SQUID_ACL_DIR}/vm-acls.conf" ]] || \
    echo "# No VMs running yet" > "${SQUID_ACL_DIR}/vm-acls.conf"

# Restart Squid
systemctl enable squid 2>/dev/null || true
systemctl restart squid 2>/dev/null || squid -k reconfigure 2>/dev/null || squid &
echo "  Squid configured and started."

# --- 5. dnsmasq configuration ---
echo "[5/5] Configuring dnsmasq..."

DNSMASQ_CONF="/etc/dnsmasq.d/agent-vms.conf"
mkdir -p /etc/dnsmasq.d

cp "${SCRIPT_DIR}/dnsmasq/dnsmasq-vms.conf" "${DNSMASQ_CONF}"

# Start dnsmasq
systemctl enable dnsmasq 2>/dev/null || true
systemctl restart dnsmasq 2>/dev/null || dnsmasq &
echo "  dnsmasq configured and started."

echo ""
echo "=== Host network setup complete ==="
echo ""
echo "Summary:"
echo "  IP forwarding: enabled"
echo "  nftables:      vm_filter table active (default deny)"
echo "  Squid:         running on ports ${SQUID_HTTP_PORT}/${SQUID_HTTPS_PORT}"
echo "  dnsmasq:       running (will listen on 10.0.X.1 per VM)"
echo "  NAT:           masquerade on ${HOST_IFACE}"
echo ""
echo "Ready to launch VMs with 'sandbox-ctl launch <type>'"
