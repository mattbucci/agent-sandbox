# Agent Sandbox

Isolated AI agent sandboxes using Firecracker microVMs. Each agent gets a full Ubuntu XFCE desktop with Chromium, scoped network access, and its own tool suite — all inside a hardware-isolated VM.

## Why

AI agents execute arbitrary code. Containers share the host kernel. This project uses Firecracker (KVM) to give each agent its own kernel, its own filesystem, and network access restricted to only the domains it needs. If an agent gets prompt-injected or installs a compromised package, it can't phone home, can't reach other agents, and can't touch the host.

## How It Works

```
Host
├── Firecracker (one VM per agent, KVM isolation)
├── nftables + Squid (per-VM domain filtering via TLS SNI)
├── OTel collector (trace every LLM call)
├── noVNC (watch agent desktops in your browser)
│
├── VM: debugger       10.0.0.2  → sentry.io, github.com
├── VM: feature-dev    10.0.1.2  → github.com, npmjs.org
├── VM: devops         10.0.2.2  → github.com, terraform, k8s
├── VM: researcher     10.0.3.2  → hn, reddit, arxiv
└── VM: security       10.0.4.2  → nvd.nist.gov, github.com
```

Each agent is defined in a single YAML file with composable presets:

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

prompt:
  role: |
    You are a senior debugging specialist...
  presets: [explore-tools, debugging-workflow, git-workflow, code-execution,
            browser-instructions, report-output]
```

## Quick Start

```bash
# 1. Configure
cp config/sandbox.yaml.example config/sandbox.yaml
vim config/sandbox.yaml   # set llm.api_base, llm.api_key, network.host_iface

# 2. Setup (Firecracker, kernel, Squid, nftables, OTel, host hardening)
sudo bin/sandbox-ctl setup

# 3. Build
sudo bin/sandbox-ctl build-base    # Ubuntu + XFCE + Chrome + Python 3.12
sudo bin/sandbox-ctl build-all     # per-agent tool customization

# 4. Launch
sudo bin/sandbox-ctl launch debugger

# 5. Observe
bin/sandbox-ctl vnc debugger       # open desktop in browser
bin/sandbox-ctl status             # list all VMs
bin/sandbox-ctl ssh debugger       # SSH in (password: agent)
```

## Agents

Five built-in agents. Create your own by adding a YAML file — see [Creating Agents](docs/creating-agents.md).

| Agent | Role | Allowed Domains |
|-------|------|-----------------|
| **debugger** | Sentry traces → root cause analysis | sentry.io, github, stackoverflow |
| **feature-dev** | GitHub issues → pull requests | github, npm, pypi |
| **devops** | Deployments, feature flags, rollbacks | github, terraform, k8s, cloud |
| **researcher** | HN, Reddit, arxiv trend monitoring | news sites, arxiv, reddit |
| **security** | CVE scanning, dependency auditing | nvd.nist.gov, github, cve.org |

## Configuration

All config is YAML with composable presets:

```
config/
  sandbox.yaml                 # LLM endpoint, network, VM defaults
  agents/*.yaml                # one per agent type
  presets/
    egress/*.yaml              # domain groups (github, npm, pypi, ...)
    capabilities/*.yaml        # tool groups (python-dev, debugging, ...)
    prompts/*.yaml             # rulebook presets (git-workflow, ...)
  install-scripts/*.sh         # complex tool installers
  secrets/github-tokens/       # fine-grained PATs (gitignored)
```

```bash
bin/sandbox-ctl config list-presets   # browse available presets
bin/sandbox-ctl config validate X    # check an agent YAML
bin/sandbox-ctl config compile       # YAML → flat build files
```

## Security

Tested against real supply chain attacks (litellm .pth harvester, axios npm RAT) and 21 escape techniques across 7 categories. All exfiltration attempts blocked.

```bash
sudo bin/security-test.sh            # 34 tests, 7 attack categories
sudo bin/supply-chain-test.sh        # litellm + axios attack emulation
sudo bin/advanced-escape-test.sh     # domain fronting, DNS tunneling, ICMP, ...
sudo bin/novel-escape-test.sh        # IPv6 bypass, LLM exfil, GitHub C2
sudo bin/harden-host.sh audit        # Firecracker production compliance
```

See [Security](docs/security.md) for the full threat model and [docs/operations.md](docs/operations.md) for troubleshooting.

## GitHub Token Security

Agents use **fine-grained personal access tokens** scoped to specific repos and permissions. Classic tokens and SSH keys are rejected.

```bash
bin/setup-github-tokens.sh show       # see requirements per agent
bin/setup-github-tokens.sh            # interactive setup
bin/setup-github-tokens.sh validate   # check all tokens
```

## CLI Reference

```
Setup:      setup, build-base, build-agent, build-all
VMs:        launch, stop, stop-all, status, cleanup
Access:     vnc, logs, ssh
Config:     config compile, config validate, config list-presets, config docs
Info:       list-agents, network-status, help
Testing:    integration-test.sh, security-test.sh, supply-chain-test.sh,
            advanced-escape-test.sh, novel-escape-test.sh, harden-host.sh
```

## Documentation

| Doc | Contents |
|-----|----------|
| [Creating Agents](docs/creating-agents.md) | How to define custom agents with YAML + presets |
| [Architecture](docs/architecture.md) | System design, config pipeline, network model |
| [Operations](docs/operations.md) | Running, monitoring, troubleshooting, base tools |
| [Security](docs/security.md) | Threat model, defense layers, accepted risks |
| [Presets Reference](docs/presets-reference.md) | All egress, capability, and prompt presets |

## Requirements

- Linux x86_64 with KVM (`/dev/kvm`)
- ~60GB RAM for 5 VMs (configurable per-agent)
- Python 3 + PyYAML on host
