# Agent Sandbox

Firecracker microVM platform for running isolated AI agents with full Ubuntu XFCE desktops, Chromium, and per-VM domain-based egress filtering.

Each agent runs inside its own hardware-isolated VM (KVM) — not a container. Agents are defined in a single YAML file with composable presets for egress domains, capabilities, and prompt rulebooks.

## Architecture

```
Host (sandbox-ctl CLI)
├── Firecracker (KVM-based VM isolation)
├── Squid (SNI peek-and-splice HTTPS domain filtering, no MITM)
├── nftables (force all VM traffic through Squid, block direct egress)
├── dnsmasq (DNS for VM subnets)
├── otel-collector (trace aggregation from all agents)
├── noVNC + websockify (observe agent desktops in browser)
│
│  LiteLLM / sglang (remote, OpenAI-compatible API)
│       ↕
├── VM 0: debugger       10.0.0.2  noVNC :6080
├── VM 1: feature-dev    10.0.1.2  noVNC :6081
├── VM 2: devops         10.0.2.2  noVNC :6082
├── VM 3: researcher     10.0.3.2  noVNC :6083
└── VM 4: security       10.0.4.2  noVNC :6084
```

Inside each VM: Ubuntu 22.04 + XFCE + Chromium + Python 3.12 + uv + agent-browser + DeepAgents (LangGraph) + SSH + VNC.

See [docs/architecture.md](docs/architecture.md) for the full system design.

## Agents

Agents are defined in `config/agents/*.yaml`. Each YAML is self-contained — it declares the agent's egress domains, installed tools, and system prompt using composable presets:

```yaml
# config/agents/debugger.yaml
agent:
  type: debugger
  name: "Sentry Bug Investigator"

egress:
  presets: [github, google, stackoverflow]
  domains: [.sentry.io]

capabilities:
  presets: [debugging, python-dev]
  install_scripts: [sentry-cli.sh]

prompt:
  role: |
    You are a senior debugging specialist...
  presets: [debugging-workflow, git-workflow, code-execution, browser-instructions, report-output]
```

**Built-in agents:** debugger, feature-dev, devops, researcher, security

```bash
sandbox-ctl list-agents              # show all agents
sandbox-ctl config list-presets      # show available presets
sandbox-ctl config validate <type>   # validate an agent YAML
```

See [docs/creating-agents.md](docs/creating-agents.md) for a full guide on creating custom agents.

## Quick Start

```bash
# 1. Configure your LLM endpoint
cp config/sandbox.yaml.example config/sandbox.yaml
vim config/sandbox.yaml   # set llm.api_base, llm.api_key, network.host_iface

# 2. Full setup (Firecracker, kernel, Squid, dnsmasq, noVNC, OTel, nftables)
sudo bin/sandbox-ctl setup

# 3. Build rootfs images
sudo bin/sandbox-ctl build-base      # ~15 min: Ubuntu + XFCE + Chrome + Python 3.12
sudo bin/sandbox-ctl build-all       # ~5 min each: per-agent customization

# 4. Launch agents
sudo bin/sandbox-ctl launch debugger
sudo bin/sandbox-ctl launch researcher

# 5. Observe
bin/sandbox-ctl vnc debugger         # prints noVNC URL
bin/sandbox-ctl status               # list all VMs

# 6. Test without LLM (desktop-only mode)
sudo bin/sandbox-ctl launch debugger --no-agent
```

## Configuration System

All configuration is YAML-based with composable presets:

```
config/
  sandbox.yaml                  # global: LLM endpoint, network, VM defaults
  agents/*.yaml                 # one per agent type
  presets/
    egress/*.yaml               # domain allowlist groups (github, npm, pypi, ...)
    capabilities/*.yaml         # tool/package groups (python-dev, debugging, ...)
    prompts/*.yaml              # rulebook presets (git-workflow, code-execution, ...)
  install-scripts/*.sh          # complex tool installers (terraform, trivy, ...)
```

The `agentconf.py` compiler resolves presets and outputs flat files for the bash scripts:

```bash
sandbox-ctl config compile debugger   # compile one agent
sandbox-ctl config compile            # compile all
cat build/debugger/system-prompt.md   # inspect compiled prompt
cat build/debugger/allowlist.txt      # inspect compiled domain list
```

### Prompt Rulebooks

Prompt presets use a rulebook format with IDs, rules, and examples:

```yaml
rules:
  - id: GIT-001
    rule: "Always create feature branches — never commit directly to main"
    example: |
      # GOOD [GIT-001]: git checkout -b fix/null-pointer
      # BAD [GIT-001]:  git commit -am "fix" on main
```

Agents reference rule IDs in their reasoning, making behavior auditable in OTel traces.

## Network Egress Filtering

1. **nftables** intercepts all TCP 80/443 from VM TAP devices
2. Traffic is redirected (DNAT) to **Squid** on the host
3. Squid uses **peek-and-splice** to read the TLS SNI field
4. If the domain is in the VM's allowlist → **splice** (pass through, no decryption)
5. If not → **terminate** (connection blocked)

Domain allowlists are composed from egress presets + per-agent inline domains.

## Observability

- **OTel traces** — every LLM API call is traced with agent type, model, and timing. Collector at `:4318`, traces to `/var/log/otel/traces.jsonl`.
- **noVNC** — watch agent desktops in your browser. `sandbox-ctl vnc <agent>`
- **Serial console** — `sandbox-ctl logs <agent>`
- **SSH** — `sandbox-ctl ssh <agent>` (password: `agent`)

## Boot Persistence

A systemd service auto-restores all VMs on host reboot:

```bash
sudo systemctl enable agent-sandbox.service  # installed by setup
```

## CLI Reference

```
Setup & Build:
  setup                          Install deps, kernel, network, OTel
  build-base                     Build base Ubuntu+XFCE rootfs
  build-agent <type>             Build agent-specific rootfs
  build-all                      Build all rootfs images

VM Management:
  launch <type> [--name N] [--vcpus N] [--mem MB] [--no-agent]
  stop <id|name|type>
  stop-all
  status [--html]
  cleanup                        Remove stopped VM state

Config:
  config compile [type]          Compile agent YAML → build/
  config validate <type>         Validate an agent YAML
  config list-presets [category] List available presets
  config docs                    Generate docs/presets-reference.md

Access:
  vnc <id|name|type>             Print noVNC URL
  logs <id|name|type>            Tail serial console
  ssh <id|name|type>             SSH into VM

Info:
  list-agents                    Show available agent types
  network-status                 Show TAP, nftables, Squid status
```

## Resources

Default per VM: 4 vCPUs, 8GB RAM. Override per-agent in YAML or at launch.

| | Per VM | 5 VMs | Host |
|---|---|---|---|
| vCPUs | 4 | 20 | 4 reserved |
| RAM | 8GB | 40GB | ~20GB free |
| Disk | 8GB sparse | ~12GB actual | - |

## Requirements

- Linux x86_64 with KVM (`/dev/kvm`)
- ~60GB RAM for 5 VMs (configurable)
- Root access for Firecracker, TAP devices, nftables
- Python 3 + PyYAML on host (for config compiler)

## Documentation

- [Creating Custom Agents](docs/creating-agents.md) — step-by-step guide
- [Presets Reference](docs/presets-reference.md) — all available presets
- [Architecture](docs/architecture.md) — system design and data flow
