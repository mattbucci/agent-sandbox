#!/usr/bin/env bash
# =============================================================================
# build-base.sh — Build the base Ubuntu rootfs for Firecracker agent VMs
#
# Creates a bootable ext4 image with:
#   - Ubuntu 22.04 (Jammy) minimal + XFCE desktop
#   - Chromium browser
#   - x11vnc + Xvfb for remote desktop
#   - Python 3, Node.js 22, build tools
#   - DeepAgents runtime pre-installed
#   - Custom init system (agent-init, replaces systemd)
#
# Must be run as root (debootstrap requires it).
# =============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
SANDBOX_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

# Load config
source "${SANDBOX_ROOT}/config/sandbox.conf"

ROOTFS_SIZE_MB="${ROOTFS_SIZE_MB:-8192}"
ROOTFS_IMG="${SANDBOX_ROOT}/rootfs/base.ext4"
STAGING="${SANDBOX_ROOT}/rootfs/staging"
OVERLAY="${SANDBOX_ROOT}/rootfs/overlay"
UBUNTU_RELEASE="jammy"
UBUNTU_MIRROR="http://archive.ubuntu.com/ubuntu"

# --- Preflight checks ---
if [[ $EUID -ne 0 ]]; then
    echo "ERROR: Must run as root. Use: sudo $0"
    exit 1
fi

if ! command -v debootstrap &>/dev/null; then
    echo "Installing debootstrap..."
    if command -v dnf &>/dev/null; then
        dnf install -y debootstrap
    elif command -v apt-get &>/dev/null; then
        apt-get install -y debootstrap
    else
        echo "ERROR: Cannot install debootstrap. Install it manually."
        exit 1
    fi
fi

if [[ -f "${ROOTFS_IMG}" ]]; then
    echo "Base rootfs already exists at ${ROOTFS_IMG}"
    read -rp "Rebuild? This will delete the existing image. [y/N] " confirm
    if [[ "${confirm}" != "y" && "${confirm}" != "Y" ]]; then
        exit 0
    fi
    rm -f "${ROOTFS_IMG}"
fi

echo "=== Building Base Rootfs ==="
echo "Size: ${ROOTFS_SIZE_MB}MB (sparse)"
echo "Release: Ubuntu ${UBUNTU_RELEASE}"
echo ""

# --- Phase 1: debootstrap ---
echo "[1/6] Running debootstrap..."
rm -rf "${STAGING}"
mkdir -p "${STAGING}"
debootstrap --arch=amd64 --include=apt-utils,locales \
    "${UBUNTU_RELEASE}" "${STAGING}" "${UBUNTU_MIRROR}"

echo "[1/6] debootstrap complete."

# --- Phase 2: chroot setup ---
echo "[2/6] Configuring chroot environment..."

# Mount necessary filesystems for chroot
mount --bind /dev     "${STAGING}/dev"
mount --bind /dev/pts "${STAGING}/dev/pts"
mount -t proc  proc   "${STAGING}/proc"
mount -t sysfs sysfs  "${STAGING}/sys"
mount -t tmpfs tmpfs  "${STAGING}/tmp"

# Cleanup function
cleanup_mounts() {
    echo "Cleaning up mounts..."
    umount -lf "${STAGING}/tmp"   2>/dev/null || true
    umount -lf "${STAGING}/sys"   2>/dev/null || true
    umount -lf "${STAGING}/proc"  2>/dev/null || true
    umount -lf "${STAGING}/dev/pts" 2>/dev/null || true
    umount -lf "${STAGING}/dev"   2>/dev/null || true
}
trap cleanup_mounts EXIT

# Configure apt sources
cat > "${STAGING}/etc/apt/sources.list" <<EOF
deb ${UBUNTU_MIRROR} ${UBUNTU_RELEASE} main restricted universe multiverse
deb ${UBUNTU_MIRROR} ${UBUNTU_RELEASE}-updates main restricted universe multiverse
deb ${UBUNTU_MIRROR} ${UBUNTU_RELEASE}-security main restricted universe multiverse
EOF

# Configure locale
chroot "${STAGING}" locale-gen en_US.UTF-8
chroot "${STAGING}" update-locale LANG=en_US.UTF-8

# --- Phase 3: Install packages ---
echo "[3/6] Installing packages (this takes a while)..."

chroot "${STAGING}" bash -c '
export DEBIAN_FRONTEND=noninteractive

apt-get update -qq

# Core system
apt-get install -y --no-install-recommends \
    sudo iproute2 iputils-ping net-tools curl wget ca-certificates \
    openssh-server dbus dbus-x11 procps less vim nano \
    git jq unzip tar gzip bzip2

# XFCE desktop environment
apt-get install -y --no-install-recommends \
    xfce4 xfce4-terminal xfce4-taskmanager thunar \
    xvfb x11vnc xdg-utils desktop-file-utils

# Chromium browser
apt-get install -y --no-install-recommends \
    chromium-browser fonts-liberation fonts-dejavu-core \
    fonts-noto-core fonts-noto-cjk fonts-noto-color-emoji \
    libgbm1 libnss3 libatk-bridge2.0-0 libgtk-3-0

# Development tools
apt-get install -y --no-install-recommends \
    build-essential python3 python3-venv python3-dev \
    strace gdb

# Python 3.12 (Ubuntu 22.04 ships 3.10, we want 3.12)
add-apt-repository -y ppa:deadsnakes/ppa
apt-get update -qq
apt-get install -y --no-install-recommends python3.12 python3.12-venv python3.12-dev
update-alternatives --install /usr/bin/python3 python3 /usr/bin/python3.12 1

# uv — fast Python package manager
curl -LsSf https://astral.sh/uv/install.sh | sh
cp /root/.local/bin/uv /usr/local/bin/uv
cp /root/.local/bin/uvx /usr/local/bin/uvx

# Node.js 22 (for browser automation and other tools)
curl -fsSL https://deb.nodesource.com/setup_22.x | bash -
apt-get install -y nodejs

# Clean up apt cache
apt-get clean
rm -rf /var/lib/apt/lists/* /var/cache/apt/archives/*
rm -rf /usr/share/doc/* /usr/share/man/* /usr/share/info/*
'

echo "[3/6] Packages installed."

# --- Phase 4: Create agent user and configure system ---
echo "[4/6] Configuring system..."

chroot "${STAGING}" bash -c '
# Create agent user
useradd -m -s /bin/bash -G sudo,video,audio agent
echo "agent ALL=(ALL) NOPASSWD: ALL" > /etc/sudoers.d/agent
chmod 440 /etc/sudoers.d/agent

# Set passwords (agent user + root, both "agent" for emergency console access)
echo "agent:agent" | chpasswd
echo "root:root" | chpasswd

# Configure SSH
mkdir -p /home/agent/.ssh
chmod 700 /home/agent/.ssh
chown -R agent:agent /home/agent/.ssh
sed -i "s/#PermitRootLogin.*/PermitRootLogin yes/" /etc/ssh/sshd_config
sed -i "s/#PasswordAuthentication.*/PasswordAuthentication yes/" /etc/ssh/sshd_config
ssh-keygen -A

# Create workspace directories
mkdir -p /home/agent/workspace /home/agent/tasks /home/agent/Desktop
chown -R agent:agent /home/agent

# Create agent log file
touch /var/log/agent.log /var/log/x11vnc.log
chown agent:agent /var/log/agent.log
chmod 666 /var/log/x11vnc.log

# Minimal XFCE autostart config (no screensaver, no power manager)
mkdir -p /home/agent/.config/autostart
mkdir -p /home/agent/.config/xfce4/xfconf/xfce-perchannel-xml

# Disable screensaver and power management
cat > /home/agent/.config/xfce4/xfconf/xfce-perchannel-xml/xfce4-screensaver.xml <<XFCEEOF
<?xml version="1.0" encoding="UTF-8"?>
<channel name="xfce4-screensaver" version="1.0">
  <property name="saver" type="empty">
    <property name="enabled" type="bool" value="false"/>
  </property>
</channel>
XFCEEOF

cat > /home/agent/.config/xfce4/xfconf/xfce-perchannel-xml/xfce4-power-manager.xml <<XFCEEOF
<?xml version="1.0" encoding="UTF-8"?>
<channel name="xfce4-power-manager" version="1.0">
  <property name="xfce4-power-manager" type="empty">
    <property name="dpms-enabled" type="bool" value="false"/>
    <property name="blank-on-ac" type="int" value="0"/>
  </property>
</channel>
XFCEEOF

chown -R agent:agent /home/agent/.config

# Create Chromium desktop shortcut
cat > /home/agent/Desktop/chromium.desktop <<DESKEOF
[Desktop Entry]
Type=Application
Name=Chromium
Exec=chromium-browser --no-sandbox --disable-gpu --disable-dev-shm-usage
Icon=chromium-browser
Terminal=false
Categories=Network;WebBrowser;
DESKEOF
chmod +x /home/agent/Desktop/chromium.desktop
chown agent:agent /home/agent/Desktop/chromium.desktop
'

echo "[4/6] System configured."

# --- Phase 5: Install agent runtime (DeepAgents) ---
echo "[5/6] Installing agent runtime..."

# Copy overlay files into staging
cp -a "${OVERLAY}/sbin/agent-init" "${STAGING}/sbin/agent-init"
chmod 755 "${STAGING}/sbin/agent-init"

mkdir -p "${STAGING}/opt/agent"
cp -a "${OVERLAY}/opt/agent/"* "${STAGING}/opt/agent/"
chmod 755 "${STAGING}/opt/agent/start.sh"

# Install Python dependencies via uv
chroot "${STAGING}" bash -c '
cd /opt/agent
uv venv --python python3.12 .venv
uv pip install --python .venv/bin/python \
    langchain langchain-core langchain-openai \
    langgraph \
    requests beautifulsoup4

# Install deepagents from GitHub (not yet on PyPI)
uv pip install --python .venv/bin/python \
    "git+https://github.com/langchain-ai/deepagents.git#subdirectory=libs/deepagents" || \
    echo "WARNING: deepagents install failed (may need manual install later)"
'

echo "[5/6] Agent runtime installed."

# --- Phase 6: Create ext4 image ---
echo "[6/6] Creating ext4 image (${ROOTFS_SIZE_MB}MB sparse)..."

# Create sparse file
dd if=/dev/zero of="${ROOTFS_IMG}" bs=1M count=0 seek="${ROOTFS_SIZE_MB}" 2>/dev/null
mkfs.ext4 -F -L "agent-rootfs" "${ROOTFS_IMG}"

# Mount and copy
MOUNT_POINT=$(mktemp -d)
mount -o loop "${ROOTFS_IMG}" "${MOUNT_POINT}"

echo "Copying rootfs to image..."
rsync -aHAX --info=progress2 "${STAGING}/" "${MOUNT_POINT}/"

# Cleanup the staging mount artifacts
rm -rf "${MOUNT_POINT}/tmp/"*
rm -rf "${MOUNT_POINT}/var/tmp/"*

umount "${MOUNT_POINT}"
rmdir "${MOUNT_POINT}"

# Cleanup staging
cleanup_mounts
trap - EXIT
rm -rf "${STAGING}"

echo ""
echo "=== Base rootfs built successfully ==="
echo "Image: ${ROOTFS_IMG}"
echo "Size:  $(du -h "${ROOTFS_IMG}" | cut -f1) (apparent: $(du -h --apparent-size "${ROOTFS_IMG}" | cut -f1))"
echo ""
echo "Next: Run 'sandbox-ctl build-agent <type>' to create agent-specific images."
