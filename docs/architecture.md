# Architecture

## System Overview

```
┌─────────────────────────────────────────────────────┐
│ Host                                                │
│                                                     │
│  sandbox-ctl CLI                                    │
│  ├── agentconf.py (YAML → flat config compiler)     │
│  ├── Firecracker (VM hypervisor, KVM-based)         │
│  ├── nftables (firewall, per-VM egress rules)       │
│  ├── Squid (HTTPS domain filtering, SNI peek)       │
│  ├── dnsmasq (DNS for VM subnets)                   │
│  ├── otel-collector (trace aggregation)             │
│  └── websockify + noVNC (desktop observation)       │
│                                                     │
│  ┌──────────┐ ┌──────────┐ ┌──────────┐            │
│  │ VM 0     │ │ VM 1     │ │ VM N     │            │
│  │ 10.0.0.2 │ │ 10.0.1.2 │ │ 10.0.N.2 │           │
│  │ tap-vm0  │ │ tap-vm1  │ │ tap-vmN  │            │
│  └──────────┘ └──────────┘ └──────────┘            │
│       ↕              ↕             ↕                │
│  ┌─────────────────────────────────────────────┐    │
│  │ nftables: DNAT 80/443 → Squid              │    │
│  │           allow DNS, OTel, LiteLLM          │    │
│  │           deny everything else              │    │
│  └─────────────────────────────────────────────┘    │
└─────────────────────────────────────────────────────┘
         ↕
   LiteLLM / sglang (remote)
```

## Inside Each VM

```
Ubuntu 22.04 + custom init (PID 1)
├── Xvfb (virtual framebuffer, 1920x1080)
├── x11vnc (VNC server on :5900)
├── XFCE4 desktop environment
├── Chromium browser
├── SSH server (port 22)
├── agent-browser (CLI browser automation)
├── Python 3.12 + uv
├── /opt/agent/.venv (LangGraph + deps)
├── /opt/agent/agent.py (DeepAgents runtime)
└── /home/agent/workspace (working directory)
     └── /home/agent/tasks (task input directory)
```

## Configuration Pipeline

```
config/agents/debugger.yaml     ─┐
config/presets/egress/*.yaml     │  agentconf.py compile
config/presets/capabilities/*.yaml├─────────────────────→ build/debugger/
config/presets/prompts/*.yaml    │                        ├── agent.conf
config/install-scripts/*.sh      │                        ├── allowlist.txt
config/sandbox.yaml             ─┘                        ├── system-prompt.md
                                                          ├── customize.sh
                                                          └── tools.json
```

### Compile Step

`agentconf.py compile <type>` reads the agent YAML and all referenced presets, then outputs flat files:

1. **agent.conf** — bash-sourceable key=value pairs (AGENT_TYPE, AGENT_NAME, LLM_MODEL, VCPUS, MEM_MB)
2. **allowlist.txt** — union of all egress preset domains + inline domains
3. **system-prompt.md** — role paragraph + auto-generated tools list + rulebook sections from prompt presets + inline sections
4. **customize.sh** — batched apt/pip/npm installs from capability presets + install scripts
5. **tools.json** — all declared tools with descriptions and source attribution

### Build Step

`sandbox-ctl build-agent <type>`:
1. Calls `agentconf.py compile` to generate flat files
2. Copies base rootfs (COW clone)
3. Runs compiled `customize.sh` in chroot
4. Copies compiled `system-prompt.md` into rootfs

### Launch Step

`sandbox-ctl launch <type>`:
1. Clones agent rootfs → instance rootfs
2. Injects `/etc/agent.conf` (runtime LLM config from sandbox.yaml)
3. Creates TAP device, nftables rules, Squid ACLs from compiled allowlist
4. Generates Firecracker config, starts VM
5. Starts websockify for noVNC

## Network Isolation

Each VM gets a `/24` subnet with the host as gateway:

- **VM IP**: `10.0.{slot}.2`
- **Host/Gateway**: `10.0.{slot}.1`
- **TAP device**: `tap-vm{slot}`

### Egress Filtering

```
VM → TCP 80/443 → nftables DNAT → Squid (host)
                                    ├── peek at TLS SNI
                                    ├── domain in allowlist? → splice (pass through)
                                    └── domain NOT in allowlist? → terminate (block)
```

Squid uses **peek-and-splice** — it reads the SNI field from the TLS ClientHello without decrypting the traffic. No MITM, no CA trust issues inside VMs.

### Allowed Host Services

VMs can reach these ports on their gateway IP:
- UDP 53 (dnsmasq)
- TCP 3128/3129 (Squid)
- TCP 4317/4318 (OTel collector)
- TCP to LiteLLM server (direct passthrough, bypasses Squid)

### VM-to-VM Isolation

VMs cannot communicate with each other. Each is on a separate `/24` subnet with no cross-subnet forwarding rules.

## Boot Persistence

The `agent-sandbox.service` systemd unit runs on boot:
1. Enables IP forwarding
2. Restores nftables base rules
3. Starts Squid and dnsmasq
4. Iterates over `state/vms/*/info.json`
5. Recreates TAP devices and nftables rules for each saved VM
6. Relaunches Firecracker with the saved rootfs
7. Restarts websockify for noVNC

## Observability

### OpenTelemetry

Each agent VM auto-sends traces to the host's otel-collector:
- Agent runtime instruments HTTP clients (httpx, requests)
- Every LLM API call becomes a trace span
- Spans include: agent type, instance ID, model, timing
- Traces exported to `/var/log/otel/traces.jsonl`

### Serial Console

Firecracker serial output goes to `state/logs/{instance-id}.log`. View with:
```bash
sandbox-ctl logs <agent>
```

### noVNC

Watch agent desktops in your browser:
```bash
sandbox-ctl vnc <agent>  # prints URL
```
