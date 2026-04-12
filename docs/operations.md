# Operations Guide

## Day-to-Day Operations

### Starting and Stopping

```bash
# Launch a single agent
sudo bin/sandbox-ctl launch debugger

# Launch without the LLM agent process (desktop-only for testing)
sudo bin/sandbox-ctl launch debugger --no-agent

# Launch with custom resources
sudo bin/sandbox-ctl launch researcher --vcpus 2 --mem 4096

# Stop one agent
sudo bin/sandbox-ctl stop debugger

# Stop everything
sudo bin/sandbox-ctl stop-all

# Clean up state from stopped VMs
sudo bin/sandbox-ctl cleanup
```

### Observing Agents

```bash
# Watch an agent's desktop in your browser
bin/sandbox-ctl vnc debugger
# → http://yourhost:6080/vnc.html?autoconnect=true

# Tail the serial console log
bin/sandbox-ctl logs debugger

# SSH into a VM (password: agent)
bin/sandbox-ctl ssh debugger

# Check all VM status
bin/sandbox-ctl status
```

### Sending Tasks to Agents

Agents watch `/home/agent/tasks/` inside their VM for `.md` files. To send a task:

```bash
# SSH in and create a task file
bin/sandbox-ctl ssh debugger
echo "Investigate Sentry issue PROJ-1234" > ~/tasks/001-investigate.md

# Or from the host via SSH
sshpass -p agent ssh agent@10.0.0.2 \
  'echo "Investigate Sentry issue PROJ-1234" > ~/tasks/001-investigate.md'
```

The agent picks up the file, processes it, and writes the result to `001-investigate.result.md`.

## Configuration Management

### Modifying Agent Configuration

```bash
# Edit the agent's YAML
vim config/agents/debugger.yaml

# Validate your changes
bin/sandbox-ctl config validate debugger

# See what changed in the compiled output
bin/sandbox-ctl config compile debugger
diff build/debugger/system-prompt.md /tmp/old-prompt.md

# Rebuild the rootfs to apply capability/package changes
sudo bin/sandbox-ctl build-agent debugger

# Relaunch
sudo bin/sandbox-ctl stop debugger
sudo bin/sandbox-ctl launch debugger
```

Note: prompt and egress changes take effect on next launch (recompiled automatically). Capability/package changes require a rootfs rebuild.

### Modifying Global Configuration

```bash
# Edit LLM endpoint, network settings, etc.
vim config/sandbox.yaml

# Recompile
bin/sandbox-ctl config compile

# Changes take effect on next VM launch
```

### Adding a New Tool to the Registry

```bash
# Create a tool doc
cat > rootfs/overlay/opt/tools/dev/my-tool.md << 'EOF'
# my-tool

> Description of what it does

**Category:** dev
**Binary:** /usr/local/bin/my-tool

## Quick Reference
```bash
my-tool --help
```
EOF

# Agents can now discover it via explore-tools
```

## Monitoring

### OTel Traces

Traces from all agents are collected at `/var/log/otel/traces.jsonl`:

```bash
# View recent traces
tail -f /var/log/otel/traces.jsonl | python3 -m json.tool

# Count traces per agent
cat /var/log/otel/traces.jsonl | \
  python3 -c "import sys,json; [print(json.loads(l).get('resourceSpans',[{}])[0].get('resource',{}).get('attributes',[{}])[0].get('value',{}).get('stringValue','?')) for l in sys.stdin]" | \
  sort | uniq -c | sort -rn
```

To forward traces to Jaeger or Grafana Tempo, edit `otel/otel-collector.yaml` and uncomment the appropriate exporter.

### Squid Access Logs

All HTTP/HTTPS requests through Squid are logged:

```bash
# View real-time access log
sudo tail -f /var/log/squid/access.log

# Find blocked requests
sudo grep "TCP_DENIED" /var/log/squid/access.log

# Requests per VM
sudo awk '{print $3}' /var/log/squid/access.log | sort | uniq -c | sort -rn
```

### Network Status

```bash
bin/sandbox-ctl network-status
# Shows: TAP devices, nftables rules, Squid status, dnsmasq status
```

## Troubleshooting

### VM fails to launch

```bash
# Check the Firecracker log
cat state/logs/<instance-id>.log.stdout

# Common issues:
# - "Logger error: No such file" → state/logs/ directory doesn't exist
#   Fix: mkdir -p state/logs
#
# - "Resource busy" → old rootfs still mounted
#   Fix: sudo umount /path/to/rootfs.ext4
#
# - "Cannot open /dev/kvm" → KVM not available or wrong permissions
#   Fix: sudo chmod 666 /dev/kvm
```

### Agent can't resolve DNS

The VM's `/etc/resolv.conf` must point to the gateway IP. Check:

```bash
bin/sandbox-ctl ssh debugger
cat /etc/resolv.conf
# Should show: nameserver 10.0.X.1
```

If empty, the init script may have failed to parse the `DNS_IP` boot parameter. Fix:

```bash
echo "nameserver $(ip route | grep default | awk '{print $3}')" | sudo tee /etc/resolv.conf
```

Ensure dnsmasq is running on the host and listening on TAP gateway IPs.

### Agent can't reach allowed domains

1. Check Squid is running: `sudo systemctl status squid`
2. Check ACLs exist: `ls /etc/squid/acls/vm-*.conf`
3. Regenerate ACLs: `bin/sandbox-ctl config compile && sudo squid -k reconfigure`
4. Check nftables DNAT rules: `sudo nft list table ip vm_filter | grep prerouting`

### Config compilation fails

```bash
# Validate the YAML
bin/sandbox-ctl config validate <agent>

# Common issues:
# - "Preset not found" → typo in preset name, check: bin/sandbox-ctl config list-presets
# - "Install script not found" → missing file in config/install-scripts/
# - YAML syntax error → check indentation (use spaces, not tabs)
```

### Squid won't start

```bash
# Check config syntax
sudo squid -k parse

# Common issues:
# - "subdomain of X" → duplicate domains in allowlist (e.g., .cloud.google.com + .google.com)
# - "empty ACL" → no vm-acls.conf file; create: echo "# empty" > /etc/squid/acls/vm-acls.conf
# - Rate limited after crashes → sudo systemctl reset-failed squid && sudo systemctl start squid
```

## Boot Persistence

VMs auto-restore on host reboot via the `agent-sandbox.service` systemd unit:

```bash
# Check service status
sudo systemctl status agent-sandbox

# View boot log
cat /var/log/agent-sandbox-boot.log

# Manually trigger a restore
sudo bin/agent-sandbox-boot.sh
```

The service restores: IP forwarding, nftables rules, Squid, dnsmasq, all saved VMs from `state/vms/*/info.json`.

## Base Image Tools

Every VM includes these tools regardless of agent type (from the base rootfs):

| Tool | Description |
|------|-------------|
| `chromium-browser` | Web browser (`--no-sandbox --disable-gpu`) |
| `agent-browser` | CLI browser automation for AI agents |
| `explore-tools` | Tool discovery CLI with LLM-powered search |
| `git` | Version control |
| `curl`, `wget` | HTTP clients |
| `jq` | JSON processor |
| `python3.12` | Python interpreter |
| `uv` | Fast Python package manager |
| `node` (v22) | Node.js runtime |
| `npm` | Node package manager |
| `build-essential` | gcc, make, etc. |
| `vim`, `nano` | Text editors |
| `ssh` | SSH client and server |
| `strace`, `gdb` | Debugging tools |
