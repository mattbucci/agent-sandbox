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

## Hermes Gateway

A Go router on the bare-metal host (`hermes-gateway.ph.ca` / `192.168.2.179:8642`,
binds `0.0.0.0`, no DNAT) presents the OpenAI-compatible `hermes-webui` gateway
contract and routes each request to a sandboxed agent VM. It is the abstraction
layer: the `model` field selects the agent, a per-token scope authorizes it, and
the request is stream-proxied to that VM's in-VM server on `:8642`, which runs
inference against the LAN LLM router (`simple-llm-router.ph.ca:8080`, default model
`gemma`). Configure it under the `gateway` block in `config/sandbox.yaml`, then
`sandbox-ctl gateway compile|build|start`. See [Hermes Gateway](docs/hermes-gateway.md).

A real pinned `NousResearch/hermes-agent:v2026.6.19` backend is also available — a
pre-baked Docker image in its own VM, selected from the webui as model `hermes`. See
[Real hermes-agent backend](docs/hermes-gateway.md#real-hermes-agent-backend-v2026619).

The router also schedules, runs async tasks, and observes itself
([ADR 0003](docs/adr/0003-gateway-scheduling-observability-dashboard.md)) — all
std-lib Go, all config optional, and the four legacy endpoints stay
byte-compatible (429/503 backpressure is additive, saturation-only):

- **Scheduling & backpressure** — per-agent concurrency (default 1, sync+async
  combined), bounded sync queue (`429` + `Retry-After` when full, `503` on wait
  timeout), async aging so tasks never starve. See
  [Scheduling & backpressure](docs/hermes-gateway.md#scheduling--backpressure).
- **Async task API** — `POST /v1/tasks` runs agent work without holding a
  connection: durable records under `state/gateway/tasks/`, priorities,
  retries with backoff, deadlines, idle watchdog, cancel, output spool, and
  crash-safe boot recovery. See [Task API](docs/hermes-gateway.md#task-api).
- **Observability** — OTLP spans end-to-end (gateway → VM → agent callbacks),
  Prometheus `/metrics` (`hermes_gateway_*`, loopback-unauth so the otel
  collector scrapes it secret-free), structured JSON logs. See
  [Observability](docs/hermes-gateway.md#observability).
- **Ops dashboard** — embedded single page at `/dashboard/` (agents, queues,
  tasks, traces, squid egress incl. denials), gated by dedicated
  `dashboard.tokens` bearers, zero external assets. See
  [Dashboard](docs/hermes-gateway.md#dashboard) and the
  [Gateway Runbook](docs/operations.md#gateway-runbook).

Durable agent memory is part of the hosted agent surface: a shared
[mnemosyne](https://pypi.org/project/mnemosyne-memory/) MCP service (`:8077`,
`sandbox-ctl mnemosyne start`) backs every agent — see
[Agent memory (mnemosyne)](docs/hermes-gateway.md#agent-memory-mnemosyne) and
[ADR 0002](docs/adr/0002-agent-memory-hosted-surface.md).

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
| [Operations](docs/operations.md) | Running, monitoring, troubleshooting, base tools, gateway runbook |
| [Hermes Gateway](docs/hermes-gateway.md) | OpenAI-compatible router fronting agent VMs: routing, scheduling, task API, observability, dashboard |
| [Security](docs/security.md) | Threat model, defense layers, accepted risks |
| [Presets Reference](docs/presets-reference.md) | All egress, capability, and prompt presets |

## Requirements

- Linux x86_64 with KVM (`/dev/kvm`)
- ~60GB RAM for 5 VMs (configurable per-agent)
- Python 3 + PyYAML on host

### Host packages

`bin/sandbox-ctl setup` installs most dependencies, but the base-image build and
host networking need these present first:

| Tool | Debian/Ubuntu | Fedora/RHEL | Arch |
|------|---------------|-------------|------|
| debootstrap (build rootfs) | `debootstrap` | `debootstrap` | `debootstrap` + `ubuntu-keyring` |
| squid / dnsmasq / nftables | `squid dnsmasq nftables` | same | `squid dnsmasq nftables` |
| websockify (noVNC) | `python3-websockify` | `python3-websockify` | AUR, or a venv: `python -m venv /opt/novnc-venv && /opt/novnc-venv/bin/pip install websockify` |
| ssh client w/ password (testing) | `sshpass` | `sshpass` | `sshpass` |

Also required on the host: `rsync`, `mkfs.ext4` (e2fsprogs), `curl`, `openssl`, `jq`.

### firewalld coexistence

If the host runs **firewalld** (default on Fedora/RHEL, common on Arch), its
input chain rejects VM→gateway traffic (DNS, Squid, OTel) before `vm_filter`'s
allow rules are evaluated — nftables enforces *all* tables. Stop firewalld from
rejecting the VM subnet so the sandbox's own `vm_filter` table is the authority:

```bash
sudo firewall-cmd --permanent --zone=trusted --add-source=10.0.0.0/16
sudo firewall-cmd --reload
```

This does **not** expose the host: `vm_filter`'s `input` chain (set up by
`setup-host-network.sh`) is the real VM→host control — it permits a VM to reach
only Squid (3128/3129), DNS (53) and OTel (4317/4318) on the gateway and
**drops everything else**, so an agent inside a VM cannot reach host SSH or any
other local service. Verify with, from inside a VM:
`echo > /dev/tcp/<gateway>/22` (should hang/fail) vs `…/3129` (should connect).

### jailer vs. raw firecracker

Launches use the Firecracker **jailer** by default (chroot + dropped privileges
+ cgroup v2). `launch.sh` stages the kernel, rootfs, and config into the
per-VM chroot (`/srv/jailer/firecracker/<id>/root/`) with chroot-relative paths,
so this works out of the box. For a quick dev launch without the jailer:

```bash
sudo env NO_JAILER=1 bin/sandbox-ctl launch <agent> --no-agent
```

### Reboot persistence & optional hardening

`bin/sandbox-ctl setup` applies host networking at runtime; it does **not**
survive a reboot on its own. Enable the bundled service so nftables/Squid/dnsmasq
(and any running VMs) are restored on boot:

```bash
sudo cp bin/agent-sandbox.service /etc/systemd/system/
sudo systemctl daemon-reload && sudo systemctl enable agent-sandbox.service
```

`harden-host.sh` also flags **SMT/hyperthreading** as a side-channel risk for
multi-tenant isolation. Disabling it persistently is a kernel-cmdline change
(reboot required). With systemd-boot + `kernel-install` (BLS entries), add the
options to `/etc/kernel/cmdline` so they survive kernel updates, then to the
active boot entry. Tip: leave the **`-fallback`** entry unmodified so it remains
a clean recovery path (SMT on, verbose):

```bash
# persist for future kernel-install regens
echo "$(cat /etc/kernel/cmdline) nosmt quiet loglevel=1" | sudo tee /etc/kernel/cmdline
# apply to the current main entry (not the fallback)
sudo sed -i '/^options/ s/$/ nosmt quiet loglevel=1/' \
  /efi/loader/entries/<machine-id>-<version>.conf
```

> `nosmt` halves available vCPUs. It is defense-in-depth, **not** required for
> the sandbox or Docker to function.

### Running Docker inside a sandbox

The `docker` capability installs Docker Engine + Compose v2. Docker + Compose run
inside the VM (overlay2, cgroup v2). Networking depends on the guest kernel:

- **Use `kernel/build-kernel.sh` for full Docker networking.** Docker ≥28's
  default bridge driver needs the iptables `raw` table (`CONFIG_IP_NF_RAW`),
  which the stock CI kernel (`fetch-kernel.sh`) omits. `build-kernel.sh` rebuilds
  the Firecracker guest kernel with `IP_NF_RAW` + `NF_TABLES` (+ the iptables-nft
  `NFT_COMPAT`) on top of the CI config, giving working `docker0` bridge, port
  publishing, and container egress NAT — verified end-to-end. It builds with
  clang/LLVM (Arch's bleeding-edge gcc miscompiles 6.1) and bakes `acpi=off` +
  `VIRTIO_MMIO_CMDLINE_DEVICES` into the kernel (a from-source vanilla kernel
  can't parse Firecracker's ACPI tables — the stock CI kernel carries FC patches
  — so it discovers devices from the `virtio_mmio.device=` boot args instead).
  This is transparent to `config-template.json`, which stays compatible with both
  kernels.
- **On the stock CI kernel**, the bridge fails ("can't initialize iptables table
  `raw`"); set `"iptables": false` in `/etc/docker/daemon.json` to run containers
  without bridge NAT/port-publishing (use `--network host`/`none`).
- **Per-VM HTTPS filtering is enforced in `ssl_bump`, not `http_access`.** Squid
  peeks the ClientHello at step1; the splice/terminate decision happens at step2
  where the SNI is reliably available. Putting the `ssl::server_name` allow rule
  in `http_access` (as earlier versions did) is racy — `http_access` runs before
  the peek completes on some connections, matches the destination *IP*, denies,
  and client-first **bumps** the connection, so allowlisted HTTPS hosts fail with
  TLS `unknown CA`. `gen-acl.sh` therefore emits `ssl_bump splice vmN_src
  vmN_domains` rules (included between `peek step1` and `terminate all`).
- **Allowlists are de-duplicated.** Squid rejects overlapping `ssl::server_name`
  entries (both `.docker.com` and `production.cloudflare.docker.com`): 6.x fails
  fatally, 7.x mis-matches the parent. `gen-acl.sh` drops covered entries.
