# Agent Sandbox

Firecracker microVM platform for running isolated AI agents with full Ubuntu XFCE desktops, Chromium, and per-VM domain-based egress filtering.

Each agent runs inside its own hardware-isolated VM (KVM) — not a container. The host controls what domains each agent can reach via Squid SNI-based HTTPS filtering + nftables.

## Architecture

```
Host (sandbox-ctl CLI)
├── Firecracker + Jailer (KVM-based VM isolation)
├── Squid (SNI peek-and-splice HTTPS domain filtering, no MITM)
├── nftables (force all VM traffic through Squid, block direct egress)
├── dnsmasq (DNS for VM subnets)
├── noVNC + websockify (observe agent desktops in browser)
├── systemd service (auto-restore VMs on reboot)
│
│  LiteLLM / sglang (remote, OpenAI-compatible API)
│       ↕
├── VM 0: debugger       10.0.0.2  noVNC :6080
├── VM 1: feature-dev    10.0.1.2  noVNC :6081
├── VM 2: devops         10.0.2.2  noVNC :6082
├── VM 3: researcher     10.0.3.2  noVNC :6083
└── VM 4: security       10.0.4.2  noVNC :6084
```

### Inside Each VM

```
Ubuntu 22.04 + XFCE + Chromium
├── Python 3.12 + uv (fast package manager)
├── DeepAgents runtime (LangGraph-based)
│   ├── Connects to LiteLLM via OpenAI-compatible API
│   ├── System prompt defines agent specialization
│   └── LocalShellBackend → real bash execution (VM is the sandbox)
├── Agent-specific CLI tools
└── SSH + VNC (for observation/debugging)
```

## 5 Agent Specializations

| Agent | Role | Key Tools | Allowed Domains |
|-------|------|-----------|-----------------|
| **debugger** | Investigates bugs from Sentry traces, reproduces and diagnoses | sentry-cli, gdb, strace | sentry.io, github.com, stackoverflow.com |
| **feature-dev** | Picks up GitHub issues, builds features, creates PRs | gh CLI, build-essential, docker | github.com, npmjs.org, pypi.org |
| **devops** | Ships code, manages feature flags, deploys and rolls back | terraform, kubectl, ansible, helm | github.com, registry.terraform.io, cloud APIs |
| **researcher** | Monitors HN, GitHub, Reddit, arxiv for trends | chromium, readability-cli, pandoc | news.ycombinator.com, github.com, arxiv.org |
| **security** | CVE monitoring, dependency auditing, pentesting | trivy, grype, semgrep, nmap | nvd.nist.gov, github.com, cve.org |

## Quick Start

```bash
# 1. Configure your LLM endpoint
cp config/sandbox.conf.example config/sandbox.conf
vim config/sandbox.conf   # Set LLM_API_BASE, LLM_API_KEY, LLM_MODEL, HOST_IFACE

# 2. Full setup (installs Firecracker, kernel, Squid, dnsmasq, noVNC, nftables)
sudo bin/sandbox-ctl setup

# 3. Build rootfs images
sudo bin/sandbox-ctl build-base      # ~15 min: Ubuntu + XFCE + Chrome + Python 3.12 + uv
sudo bin/sandbox-ctl build-all       # ~5 min each: per-agent tool customization

# 4. Launch agents
sudo bin/sandbox-ctl launch debugger
sudo bin/sandbox-ctl launch researcher

# 5. Observe agent desktops
bin/sandbox-ctl vnc debugger         # prints noVNC URL
bin/sandbox-ctl status               # list all VMs

# 6. Launch without an LLM (desktop-only, for testing)
sudo bin/sandbox-ctl launch debugger --no-agent
```

## Network Egress Filtering

Each VM's outbound traffic is filtered by domain:

1. **nftables** intercepts all TCP 80/443 from VM TAP devices
2. Traffic is redirected (DNAT) to **Squid** on the host
3. Squid uses **peek-and-splice** to read the TLS SNI field
4. If the domain is in the VM's allowlist → **splice** (pass through, no decryption)
5. If not → **terminate** (connection blocked)
6. The LiteLLM server is always allowed for all VMs

Per-VM domain allowlists live in `rootfs/agents/<type>/allowlist.txt`.

## Boot Persistence

A systemd service (`agent-sandbox.service`) auto-restores all VMs on host reboot:
- Recreates TAP devices and nftables rules
- Restarts Squid and dnsmasq
- Relaunches each VM from its saved state

```bash
# Installed automatically by sandbox-ctl setup
sudo systemctl enable agent-sandbox.service
```

## CLI Reference

```
sandbox-ctl setup              # One-time: install all deps, configure host
sandbox-ctl build-base         # Build base Ubuntu+XFCE rootfs
sandbox-ctl build-agent TYPE   # Build agent-specific rootfs
sandbox-ctl build-all          # Build all rootfs images

sandbox-ctl launch TYPE [--name N] [--vcpus N] [--mem MB] [--no-agent]
sandbox-ctl stop ID|NAME|TYPE
sandbox-ctl stop-all
sandbox-ctl status [--html]
sandbox-ctl cleanup            # Remove stopped VM state

sandbox-ctl vnc ID|NAME|TYPE   # Print noVNC URL
sandbox-ctl logs ID|NAME|TYPE  # Tail serial console
sandbox-ctl ssh ID|NAME|TYPE   # SSH in (password: agent)
sandbox-ctl list-agents        # Show available agent types
sandbox-ctl network-status     # Show TAP devices, nftables, Squid
```

## Resource Allocation

Default per VM: 4 vCPUs, 8GB RAM. Configurable per-agent in `config/agents/<type>.conf` or at launch with `--vcpus` and `--mem`.

| | Per VM | 5 VMs | Host Reserved |
|---|---|---|---|
| vCPUs | 4 | 20 | 4 |
| RAM | 8GB | 40GB | ~20GB |
| Disk | 8GB sparse | ~12GB actual | - |
| noVNC | 6080+N | 5 ports | - |

## Requirements

- Linux x86_64 with KVM (`/dev/kvm`)
- ~60GB RAM for 5 VMs (configurable)
- Root access for Firecracker, TAP devices, nftables

## Key Design Decisions

- **Firecracker, not Docker**: True hardware isolation via KVM. Each VM has its own kernel.
- **Custom init, not systemd**: Shell-script PID 1 — Firecracker doesn't support full systemd.
- **DeepAgents (LangGraph)**: Agent runtime with `LocalShellBackend`. Real bash execution since the VM is the sandbox.
- **uv + Python 3.12**: Fast, reproducible Python environment inside each VM.
- **LiteLLM (OpenAI-compatible)**: Configurable API base URL, key, model per agent. Works with sglang, vLLM, Ollama, etc.
- **Squid peek-and-splice**: HTTPS domain filtering by SNI without MITM decryption.
- **Per-VM /24 subnets**: No VM-to-VM communication possible.
- **COW rootfs clones**: Fast VM launch from pre-built agent images.
