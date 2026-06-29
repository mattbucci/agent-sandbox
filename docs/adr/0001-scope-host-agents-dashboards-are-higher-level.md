# ADR 0001 — agent-sandbox hosts agents; dashboards & orchestration are higher-level

- **Status:** Accepted
- **Date:** 2026-06-28
- **Deciders:** repo owner (mattbucci)

## Context

`agent-sandbox` (deployed as **hermes-gateway.ph.ca**) runs sandboxed AI agents in
Firecracker microVMs behind a host-side Go gateway that presents an
OpenAI-compatible API on `:8642`. Backends today: the real
`NousResearch/hermes-agent` (v2026.6.19) in one VM, and a hand-rolled deepagents
adapter in the others. The chat frontend (`hermes-webui`) runs separately in
**k3s** (`hermes.hellauseful.com`) and talks to the gateway over the LAN.

We evaluated surfacing a **multi-agent Kanban dashboard**. Findings:

- Hermes **Kanban** is a gateway-side feature: one durable SQLite board
  (`kanban.db`) owned by one gateway process, with an in-process dispatcher;
  "different agents" are **Hermes profiles**, not separate VMs. It is bundled
  (auto-enabled) and already running in the hermes VM.
- The **hermes-webui kanban panel is file-based**: `api/kanban_bridge.py` reads
  `kanban.db` directly via `hermes_cli.kanban_db` ("the only source of truth").
  It assumes the webui and the gateway **share one `HERMES_HOME` filesystem**
  (the single-host two-container model). There is **no remote HTTP kanban API**
  for the webui to call.
- Our topology is **split**: webui in k3s, gateway in a Firecracker VM on the
  sandbox host — **no shared filesystem**. So the webui's kanban tab cannot see
  the VM's board without a shared/network volume (SQLite-over-network locking
  caveats) or co-locating the board with the webui.

Options considered for the dashboard surface:

1. Hermes' own dashboard (`:9119`) in the hermes VM (co-located with `kanban.db`),
   exposed on the LAN like `:8642`.
2. The existing hermes-webui kanban panel (requires a shared `HERMES_HOME` across
   k3s↔VM — network volume or co-location).
3. Co-locate `hermes-agent` + a `hermes-webui` in one VM sharing `HERMES_HOME`.

## Decision

**This repo's scope is to *host the agents* — the gateway, the per-agent
Firecracker sandboxes, and the agents' runtime/data layer. It does not build or
maintain a dashboard or higher-level orchestration.**

The multi-agent **Kanban dashboard (and any cross-agent orchestration/visualization)
will be built custom, at a higher level**, by the operator — consuming the agents
this repo hosts. We do not adopt the hermes-webui kanban panel or the Hermes
`:9119` dashboard as a product surface here.

## Consequences

- **In scope (here):** the OpenAI-compatible gateway (`:8642`), per-agent
  sandbox VMs and their lifecycle, and the agents' own durable state (e.g. the
  hermes kanban board, agent memory) as a *hosting* concern. These remain
  available for a higher-level system to read/drive.
- **Out of scope (higher-level/custom):** the kanban dashboard UI, cross-agent
  task distribution, and multi-agent workflow orchestration.
- The hermes-webui's built-in kanban tab is **not used** in this split topology
  (file-based; would need a shared `HERMES_HOME`). Hermes' `:9119` dashboard may
  still be enabled per-VM for ops/debugging but is not the product surface.
- The higher-level system is responsible for how it reads/aggregates per-agent
  boards. If it needs programmatic access to a hosted agent's kanban, that should
  be exposed deliberately (e.g. a kanban HTTP/export endpoint proxied by the
  gateway) — tracked separately, not assumed by the webui's file-based bridge.
- Agent **persistence** and the **memory backend** (e.g. mnemosyne) are hosting
  concerns and are tracked/decided separately from this dashboard decision.
