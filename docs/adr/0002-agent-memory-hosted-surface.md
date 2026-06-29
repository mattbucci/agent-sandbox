# ADR 0002 — agent memory is part of the hosted agent surface (mnemosyne)

- **Status:** Accepted
- **Date:** 2026-06-28
- **Deciders:** repo owner (mattbucci)

## Context

[ADR 0001](0001-scope-host-agents-dashboards-are-higher-level.md) scoped this repo
to *hosting the agents* — the gateway, the per-agent Firecracker sandboxes, and the
agents' runtime/data layer — and explicitly deferred the **memory backend
(e.g. mnemosyne)** as a hosting concern to be decided separately. This ADR is that
decision.

Per the repo owner, **agent memory is part of the hosted agent surface**: an agent
without durable memory is an incomplete agent, and the higher-level system
(dashboards, orchestration) consumes the agents *with their memory included*. So
the memory backend belongs here, alongside the gateway and the sandboxes.

We evaluated [mnemosyne](https://pypi.org/project/mnemosyne-memory/) (pip package
`mnemosyne-memory`, latest 3.10.1, `requires_python >=3.10`) as that backend:

- It is a **Python library + CLI + MCP server**. There is **no REST / OpenAI-style
  memory API** — agents consume it over **MCP** (run as MCP-over-SSE with
  `mnemosyne mcp --transport sse --host 0.0.0.0 --port <PORT>`).
- Storage is a **SQLite tree** under `MNEMOSYNE_DATA_DIR`; the store is **shared**
  and partitioned by `author_id` / memory `bank`, so one service can serve every
  agent while keeping each agent's recall scoped.
- Auth is a bearer token (`MNEMOSYNE_MCP_TOKEN`); clients send
  `Authorization: Bearer <token>`.
- Embeddings are optional: local `fastembed` (default `BAAI/bge-small-en-v1.5`,
  the `[embeddings]` extra also pulls `sqlite-vec`) or a remote OpenAI-compatible
  endpoint. **Host Python is 3.14 (bleeding edge), where `fastembed` / `sqlite-vec`
  wheels may be missing.** The installer must therefore prefer a `python3.12` if
  present and **degrade gracefully** (install without `[embeddings]` ⇒ FTS5
  keyword-only recall) with a clear log, rather than hard-fail.

Topology options considered: (1) one shared LAN MCP service on the host wired to
all agent VMs; (2) a mnemosyne instance baked into each agent rootfs (per-VM
private store); (3) no shared service — leave memory to each harness.

## Decision

**We host mnemosyne as ONE shared LAN MCP service on the sandbox host and wire all
hosted agents to it.**

- The service runs on the **host** (not inside a VM), lifecycle via
  `bin/sandbox-ctl mnemosyne install|start|status|stop`, with its venv and data
  under `state/mnemosyne/` (`state/` is fully gitignored). It binds
  **`0.0.0.0:8077`**; its data lives in `state/mnemosyne/data`.
- Agent VMs reach the host at their **per-VM gateway IP** (`10.0.<slot>.1`), which
  is not a fixed address, so inside every VM we publish a stable hostname
  **`mnemosyne.host`** → the VM's gateway IP (`agent-init`, alongside the existing
  `/etc/agent-hosts` mechanism). All agent configs use the MCP SSE URL
  **`http://mnemosyne.host:8077/sse`**.
- Egress is a VM→host service call (the nftables `vm_filter` **input** chain, dest =
  gateway IP). `sandbox-ctl mnemosyne start` adds an idempotent input allow for
  `tcp dport 8077` from `tap-vm*` (removed on stop), mirroring how OTel
  (4317/4318) is allowed in `network/setup-host-network.sh`.
- The two hosted harnesses consume it:
  - **hermes** — the pre-baked container reads an `mcp` server block in its
    `config.yaml` (written by `vm/prepare-rootfs.sh`, only when memory is enabled),
    and `run-hermes.sh` passes `--add-host mnemosyne.host:<gw>` since the container
    gets its own `/etc/hosts` even under `--network host`.
  - **deepagents** — `agent.py` connects to the MCP server via
    `langchain-mcp-adapters` (`MultiServerMCPClient`) and adds its tools to the
    agent. Tool loading is async and must run on `gateway_server.py`'s persistent
    event loop.

Configuration follows the existing pipeline: a `memory` block in
`config/sandbox.yaml`; `agentconf compile-global` emits `MEMORY_ENABLED`,
`MNEMOSYNE_PORT`, `MNEMOSYNE_TOKEN`, `MNEMOSYNE_EMBEDDINGS` into
`build/sandbox.conf`; `compile_agent_conf` emits `MNEMOSYNE_ENABLED`,
`MNEMOSYNE_PORT`, `MNEMOSYNE_TOKEN`, `MNEMOSYNE_URL` into `build/<type>/agent.conf`
and thence `/etc/agent.conf`.

We did **not** bake a private mnemosyne into each rootfs: a single shared store
gives durable memory *across* agents (and across rootfs rebuilds), is one process
to operate, and matches mnemosyne's own `author_id`/`bank` partitioning.

## Consequences

- **Durable shared memory across agents.** All hosted agents read/write one
  persistent store (`state/mnemosyne/data`) partitioned per agent, surviving VM
  restarts and rootfs rebuilds (state/ is never baked into an image).
- **MCP is the only integration path.** Because mnemosyne exposes no REST API,
  every consumer speaks MCP-over-SSE; non-MCP harnesses get no memory until they
  add an MCP client.
- **Graceful degradation everywhere.** If `MNEMOSYNE_ENABLED` is unset/empty or the
  MCP connect fails, agents build with **no** memory tools and log a warning —
  never crash. If `fastembed`/`sqlite-vec` wheels are unavailable on host Python
  3.14, the service installs keyword-only (FTS5) and logs it.
- **One more host service to operate.** Add `mnemosyne` to the lifecycle/firewall
  surface (`sandbox-ctl mnemosyne …`, the `:8077` input allow). The shared service
  is a single store for all agents — isolation is logical (`author_id`/`bank`), not
  per-VM.
- **Consistent with ADR 0001.** Memory is hosted here as part of the agent surface;
  the higher-level system consumes the agents — memory included — rather than this
  repo growing a memory product/UI of its own.
