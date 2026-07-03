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

## Gateway Runbook

Operating the hermes-gateway router's scheduling / task / observability /
dashboard features (see [Hermes Gateway](hermes-gateway.md) for the contracts
and [ADR 0003](adr/0003-gateway-scheduling-observability-dashboard.md) for the
rationale). A manual end-to-end exercise of everything below lives in
`test/gateway-smoke.sh` (run by hand on the host, never CI).

### Reading /metrics

`GET /metrics` is Prometheus text format, prefix `hermes_gateway_`. From the
host it needs **no auth** (loopback rule — this is how the otel collector
scrapes it); from the LAN present any gateway or dashboard bearer.

```bash
# Everything, from the host
curl -s http://127.0.0.1:8642/metrics

# Is anything saturated? (queue depth, running slots, rejections)
curl -s http://127.0.0.1:8642/metrics | grep -E 'sched_(queue_depth|running|rejected)'

# Task health: states, retries, oldest pending age, store persistence
curl -s http://127.0.0.1:8642/metrics | grep -E '(^| )hermes_gateway_(tasks\{|task_|store_degraded)'

# VM liveness as the router sees it
curl -s http://127.0.0.1:8642/metrics | grep vm_up

# From a LAN machine (gateway or dashboard bearer required, else 401)
curl -s -H "Authorization: Bearer $KEY" http://hermes-gateway.ph.ca:8642/metrics
```

The useful alarms: `sched_rejected_total{reason=queue_full|wait_timeout}`
climbing (clients are getting 429/503), `task_oldest_pending_age_seconds`
growing (dispatcher starved or VM down — check `vm_up`), `store_degraded 1`
(task records failing to persist — check disk/permissions on
`state/gateway/tasks/`), and `otlp_export_batches_total{outcome="error"}`
climbing (collector down; see below). The collector also scrapes these every
30s into `/var/log/otel/metrics.jsonl` (archival/offline grep only — the
dashboard reads live in-memory state instead).

### Dashboard & token issuance

The ops dashboard is at `http://hermes-gateway.ph.ca:8642/dashboard/`. The page
itself is an inert shell; every data call needs a bearer from
`gateway.dashboard.tokens` — a **dedicated ops credential**, deliberately not
the webui gateway token. With no token configured the APIs fail closed (403).

```bash
# 1. Generate a token
echo "hgwd_$(openssl rand -hex 24)"

# 2. Add it to config/sandbox.yaml
#    gateway:
#      dashboard:
#        tokens: ["hgwd_<the value>"]

# 3. Recompile and restart the router
bin/sandbox-ctl gateway compile
sudo bin/sandbox-ctl gateway stop && sudo bin/sandbox-ctl gateway start
```

Open `/dashboard/`, paste the token at the prompt (kept in browser
localStorage; sent only as an `Authorization` header). Rotate by replacing the
list entry and recompiling/restarting; revoke the old browser by clearing its
localStorage. Multiple tokens are allowed, so each operator can have their own.

### Task recovery behavior

Task records persist under `state/gateway/tasks/` (`<id>.json` +
`<id>.output.txt` spool). On every boot the router replays the recovery matrix
— a `kill -9`, a crash, and a clean `gateway stop` all land the same outcomes:

| Found at boot | Becomes |
|---|---|
| `pending`, deadline passed | `expired` |
| `running`, cancel was requested | `cancelled` |
| `running`, deadline passed | `expired` |
| `running`, spool has bytes, `retry_on_partial:false` | `failed` (interrupted) — partial output usually means side effects already happened |
| `running`, attempts exhausted | `failed` (interrupted) |
| `running`, otherwise | re-queued `pending` with backoff |
| unparseable record | renamed `<name>.corrupt`, skipped (inspect/delete by hand) |
| record for an agent no longer in config | orphaned: logged, never claimed, expires at its deadline |

Note the interrupted attempt still counts (`attempts` is incremented at claim
time — a crash burns an attempt). Terminal tasks are garbage-collected after
`tasks.retention_h` (default 7 days) and capped at `tasks.max_records`.

### Degraded-mode banners

Every external dependency degrades without touching chat routing. Where you see
it and what it means:

| Signal | Meaning | Fix |
|---|---|---|
| dashboard **collector** dot red / "no successful export yet" | no successful OTLP export in >5 min | `systemctl status otel-collector` (see below) |
| dashboard **traces_file** / **squid_log** dot red; panel shows `available:false` | `/var/log/otel/traces.jsonl` or `/var/log/squid/access.log` missing/unreadable | check the collector/squid and file permissions; `dashboard_source_errors_total` counts these |
| dashboard **tasks_dir** dot red; task panels `available:false`; `/v1/tasks*` returns 404 | task store failed to open at boot — routing continues without tasks | fix `state/gateway/tasks/` (exists? writable?), restart the gateway |
| `store_degraded` gauge = 1 / overview `store_degraded:true` | a task record failed its last persist; in-memory state is authoritative and the write retries on the next transition | check disk space/permissions; watch for `store_error` in `state/logs/gateway.log` |
| `429` / `503` on chat | not degradation — saturation backpressure by design | raise per-agent `concurrency` only if the agent can truly handle parallel turns |

### Collector-down symptoms

When the otel collector is stopped (or `:4318` is unreachable):

- Routing, chat, and tasks are **completely unaffected** — span export is
  fire-and-forget.
- `otlp_export_batches_total{outcome="error"}` climbs; batches are dropped, not
  retried (the collector is the durability layer). If spans are produced faster
  than the failing exports drain, `otlp_spans_dropped_total` climbs too.
- `state/logs/gateway.log` shows an `otlp_export_fail` warning at most once per
  minute.
- The dashboard's collector dot goes red ("last export Ns ago"); the recent-
  traces panel goes stale and, once the file rotates away or was never written,
  reports `available:false`.

Recover with `sudo systemctl restart otel-collector` — the dot goes green on
the next successful export; nothing in the gateway needs a restart. To run
permanently without a collector, set `gateway.observability.otlp_endpoint: ""`
(span export off; `traceparent` is still propagated so in-VM traces stay
linkable).

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
