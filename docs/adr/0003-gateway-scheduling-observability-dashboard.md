# ADR 0003 — the gateway schedules, runs async tasks, observes itself, and ships an ops dashboard

- **Status:** Accepted
- **Date:** 2026-07-02
- **Deciders:** repo owner (mattbucci)

## Context

[ADR 0001](0001-scope-host-agents-dashboards-are-higher-level.md) scoped this repo
to *hosting the agents* and pushed dashboards and cross-agent orchestration to a
higher-level system. Living with that split exposed gaps that are properties of
the **hosting layer itself**, not of any higher-level product:

- **No backpressure.** The router proxied every chat concurrently into a VM whose
  agent is effectively single-threaded (one shell, one working tree). Concurrent
  turns against one agent interleave side effects; there was no queueing, no
  "busy" signal, and no way for a client to know it should retry.
- **No async work.** Everything was a synchronous SSE stream tied to a live client
  connection. An hour-long agent run required the caller to hold a socket open;
  a dropped connection killed the run with no record.
- **No operational visibility.** The router logged free-text lines; there were no
  metrics, no gateway-side traces (VM-internal OTel traces existed but started at
  the VM, unlinked to the request that caused them), and no single place to see
  queue depth, VM liveness, task states, or egress denials.

These are admission control, durable job execution, and self-observability for
the *gateway process* — they exist per-gateway, need the gateway's in-memory
state, and are useless anywhere else. Building them "higher-level" would mean a
second service polling this one for state it already holds.

Hard constraints carried into the design: the four legacy endpoints (`/health`,
`/v1/capabilities`, `/v1/models`, `/v1/chat/completions`) stay **byte-compatible**
for the pinned `hermes-webui`; the router stays **Go standard library only**
(`GOPROXY=off go build ./...`, zero `require` lines, go 1.21) so it builds on the
offline host; an old 7-key `gateway.json` keeps loading unchanged (defaults live
only in Go); every external data source (collector, `traces.jsonl`, squid log,
tasks dir) degrades without touching routing; and sizing is single-host homelab
(linear scans, one mutex per subsystem, no WAL, no preemption).

## Decision

**Partially reverse ADR 0001: per-agent scheduling/queueing/dispatch, an async
task subsystem, gateway observability (OTLP spans, Prometheus `/metrics`,
structured logs), and an embedded single-page ops dashboard now live inside this
gateway.** Higher-level *cross-agent orchestration* (workflow graphs, kanban,
multi-agent planning) remains out of scope, exactly as ADR 0001 drew it — the
dashboard added here is an **ops view of this process**, not a product surface.

The judgment calls, recorded so they are not relitigated:

- **Fairness and aging, no preemption.** One slot pool per agent
  (`default_concurrency: 1`, sync + async combined; slot held for the whole
  stream). Sync (interactive) requests join a bounded FIFO (`sync_queue_max: 4`,
  429 beyond, 503 after `sync_queue_wait_s: 120`); async (task) slot requests
  queue unbounded and never time out, but **age past the sync FIFO** after
  `async_starvation_after_s: 300` so background work cannot starve forever while
  interactive traffic keeps arriving. Running work is **never preempted** — an
  agent turn has shell side effects that cannot be safely suspended. 429/503 are
  additive, saturation-only responses; the success-path bytes are unchanged.
- **`retry_on_partial` defaults to `false`.** An attempt that already streamed
  output bytes has probably already *done* things (edited files, run commands).
  Re-running it by default risks duplicated side effects; the operator opts into
  retry-on-partial per task or globally when the workload is idempotent.
- **A crash burns an attempt.** `attempts` is incremented at claim time, so a
  task found `running` after a gateway crash/restart has already consumed that
  attempt. Recovery applies the same interruption matrix as graceful shutdown
  (cancel-requested ⇒ cancelled; past deadline ⇒ expired; partial output without
  retry-on-partial, or attempts exhausted ⇒ failed/interrupted; otherwise
  re-queued with backoff). The alternative — refunding attempts on crash — makes
  a crash-looping gateway retry a possibly destructive task forever. The one
  deliberate exception: **VM-unavailable refunds the attempt** (no VM, connect
  refused, non-2xx before the first byte), because the task never reached the
  agent; those retries are bounded by the deadline, not `max_attempts`.
- **Each task attempt is a new trace root.** The submit request's span context is
  persisted on the task (`submit_trace`) and every attempt root carries a **span
  link** back to it (and appends its trace id to `trace_ids` for the dashboard
  join). Rejected: making attempts children of the submit span — the submit span
  ends in milliseconds, and hour-long child spans of a long-dead parent render
  broken in every trace UI.
- **Inbound `sampled=0` is overridden to sampled.** We own the entire backend
  behind this gateway; an upstream's sampling decision would silently blind us to
  our own traffic. A valid inbound `traceparent` still parents the SERVER span
  (trace ids join up); only the sampled flag is forced on.
- **Dashboard token model.** `/dashboard/api/*` requires a bearer from a
  **dedicated** `dashboard.tokens` list — not the webui gateway tokens, so the
  ops credential can be rotated/revoked without touching chat clients, and a
  leaked chat token does not expose queue/task/egress state. Empty list ⇒ the
  APIs **fail closed** (403); the static shell is inert and unauthenticated;
  comparisons are constant-time; the token travels only in the `Authorization`
  header (localStorage on the client, no cookies). `/metrics` is unauthenticated
  from loopback only (the collector scrapes `127.0.0.1` secret-free); LAN callers
  need a gateway or dashboard bearer.
- **In-memory ring buffers over the collector's metrics file.** The dashboard's
  charts read the gateway's own 1-hour rings (360 × 10s buckets per agent) and
  live `Snapshot()`s — fresher, zero parsing, and working *while the collector is
  down*, which matters because this dashboard is what you look at when the
  observability stack is broken. Rejected: parsing the collector's
  `metrics.jsonl`, which adds a collector-liveness dependency to the ops view of
  the system that supervises the collector, plus a cumulative-counter parser for
  staler data. The collector's `file/metrics` export exists purely as an
  offline-grep archive.

## Consequences

- **In scope (here) now additionally:** per-agent admission control shared by
  sync chat and tasks; the durable task store under `state/gateway/tasks/`
  (atomic-rename JSON records + output spools, boot recovery, GC); the `/v1/tasks*`
  API; hand-rolled OTLP/HTTP span export, `hermes_gateway_*` Prometheus metrics,
  and slog JSON logs; and the `/dashboard/` ops page. All documented in
  [hermes-gateway.md](../hermes-gateway.md) and [operations.md](../operations.md).
- **Still out of scope (per ADR 0001):** cross-agent task distribution, workflow
  orchestration, and any kanban/product dashboard. A higher-level system now has
  much better raw material to build on (`/v1/tasks`, `/metrics`, traces).
- **Wire compatibility holds.** The four legacy endpoints are golden-tested
  byte-compatible; the only client-visible change on the chat path is the
  possibility of queue wait and the new 429/503 saturation responses (with
  `Retry-After`), plus a `traceparent` header sent downstream that both backends
  ignore.
- **The gateway is heavier.** It now owns goroutines (dispatchers, runners,
  sweeper, exporter, sampler), on-disk state, and an HTML surface. Mitigations:
  std-lib only, one mutex per subsystem, everything degrades — tasks disabled ⇒
  routes 404; store unopenable ⇒ routing continues without tasks; collector down
  ⇒ spans drop and a counter climbs; traces/squid files unreadable ⇒ the panel
  reports `available:false` with HTTP 200.
- **Old configs keep working.** Every new config block is optional; defaults are
  applied only in Go (`applyDefaults()`); `agentconf.py` copies the blocks
  through verbatim. A stale, hand-edited `gateway.json` behaves identically to a
  freshly compiled one.
