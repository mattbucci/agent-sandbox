# Security Model

## Threat Model

The agent sandbox runs untrusted AI agents that execute arbitrary code. The security model assumes:

- **Agents are adversarial.** A prompt-injected or malfunctioning agent will attempt to escape, exfiltrate data, or access unauthorized resources.
- **The host is trusted.** Host-level tools (sandbox-ctl, nftables, Squid) are the enforcement boundary.
- **The LLM is untrusted.** The LLM's outputs drive agent behavior — it can instruct the agent to attempt anything.

## Defense Layers

### Layer 1: Hardware Isolation (Firecracker/KVM)

Each agent runs in a **Firecracker microVM** backed by KVM hardware virtualization. This is the primary isolation boundary.

| Property | Detail |
|----------|--------|
| Isolation level | Hardware (KVM), not container namespaces |
| Kernel | Each VM has its own Linux kernel (6.1.x) |
| Memory | Physically isolated via KVM — no shared pages between VMs |
| CPU | Firecracker enforces vCPU count limits |
| Disk | Each VM has its own ext4 rootfs — COW clone of base, no shared writes |
| Device model | Minimal: virtio-net, virtio-blk, serial. No GPU, USB, PCI passthrough |

**What this stops:**
- Container escapes (there are no containers — this is a real VM)
- Kernel exploits against the host (agent's kernel is separate)
- Memory snooping between VMs
- Disk access to host filesystem

**What this does NOT stop:**
- Network-based attacks (addressed by Layer 2)
- Resource exhaustion beyond allocated vCPU/RAM (addressed by Firecracker limits)

### Layer 2: Network Egress Filtering (nftables + Squid)

All VM network traffic is controlled by the host:

```
VM → TCP 80/443 → nftables DNAT → Squid → peek SNI → allow/block
VM → other ports → nftables DROP
```

| Rule | Detail |
|------|--------|
| Default policy | DROP all VM forward traffic |
| HTTP/HTTPS | Redirected to Squid via DNAT |
| Squid filtering | SNI peek-and-splice — reads domain from TLS handshake, no decryption |
| Allowed domains | Per-VM allowlist from agent YAML egress presets |
| DNS | VM queries host dnsmasq only (gateway IP) |
| LLM server | Always allowed (direct passthrough, bypasses Squid) |
| OTel collector | Always allowed (host gateway:4317/4318) |
| VM-to-VM | Impossible — separate /24 subnets, no cross-subnet forwarding |

**What this stops:**
- Data exfiltration to unauthorized domains
- C2 callbacks to attacker infrastructure
- Lateral movement between VMs
- Direct IP connections bypassing DNS

**Known limitations:**
- DNS tunneling through dnsmasq (mitigated: dnsmasq doesn't forward to arbitrary resolvers)
- TLS 1.3 Encrypted Client Hello (ECH) hides SNI — Squid can't filter. Mitigation: block ECH or use IP-based fallback.
- Squid must be running — if it crashes, VMs lose all HTTPS access (fail-closed)

### Layer 3: In-VM Constraints

| Constraint | Detail |
|------------|--------|
| User | Agent runs as `agent` (uid 1000), not root |
| Sudo | Agent has passwordless sudo (design choice: the VM is the boundary, not the user) |
| Agent process | Controlled by `/opt/agent/start.sh` — reads config from `/etc/agent.conf` |
| API keys | Injected at boot via rootfs mount, not baked into images |
| Rootfs | Instance rootfs is disposable — agent can trash it, doesn't affect base image |

### Layer 4: Observability

| Channel | Detail |
|---------|--------|
| OTel traces | Every LLM API call instrumented with agent type, timing, request/response |
| Serial console | Firecracker serial output to host log file |
| noVNC | Live desktop observation |
| Squid access log | All HTTP/HTTPS requests with domain, status, bytes |
| SSH | On-demand access for debugging |

## Attack Surface

| Vector | Risk | Mitigation |
|--------|------|------------|
| VM escape via Firecracker bug | Critical | Firecracker is hardened for this; use jailer for additional seccomp + chroot |
| VM escape via KVM bug | Critical | Kernel updates; KVM is heavily audited |
| Network exfiltration | High | Squid domain filtering + nftables |
| DNS tunneling | Medium | dnsmasq only resolves via configured upstream, rate limiting possible |
| Resource exhaustion (fork bomb, disk fill) | Medium | Firecracker vCPU/RAM limits; rootfs is fixed-size ext4 |
| API key theft | Medium | Keys in /etc/agent.conf readable by agent user; mitigated by VM isolation |
| Prompt injection causing agent misuse | Medium | Rulebook prompts with auditable rule IDs; OTel tracing |
| VM-to-VM lateral movement | Low | Separate /24 subnets, no forwarding rules |
| Host service exploitation (Squid, dnsmasq) | Low | Services run as non-root; standard packages with security updates |

## Security Testing

Run the security test suite to validate all defenses:

```bash
sudo bin/security-test.sh           # full suite against all VMs
sudo bin/security-test.sh --quick   # test against one VM only
```

The test suite attempts the following attacks from inside VMs:

1. **Egress to blocked domain** — curl/wget to a domain NOT in the allowlist
2. **Direct IP connection** — bypass DNS, connect to an IP directly
3. **VM-to-VM communication** — ping/connect to another VM's subnet
4. **DNS tunneling** — attempt to resolve and connect to arbitrary domains
5. **Host filesystem access** — try to read host files via /proc, /sys, device nodes
6. **Firecracker API socket access** — try to reach the host API socket
7. **Network scanning** — attempt to discover host services beyond allowed ports
8. **Resource exhaustion** — fork bomb, memory allocation, disk fill (limited scope)

## Hardening Checklist

- [ ] Switch from raw `firecracker` to `jailer` (adds seccomp + chroot + uid drop)
- [ ] Enable Squid access logging and ship to central log
- [ ] Add dnsmasq query rate limiting
- [ ] Block TLS ECH at Squid level
- [ ] Rotate API keys per VM boot (consider Vault integration)
- [ ] Add resource monitoring alerts (CPU/RAM/disk per VM)
- [ ] Regular security test runs via CI
