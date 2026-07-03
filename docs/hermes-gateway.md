# Hermes Gateway

The **Hermes Gateway** presents the `hermes-webui` gateway contract to a frontend
and routes each chat request to a sandboxed agent backend. It is the *abstraction*
layer: the frontend speaks one stable OpenAI-compatible API, and the gateway hides
which Firecracker VM (and which agent harness) actually serves the request.

## Architecture

```
hermes-webui (k3s, gateway mode)
      │  POST /v1/chat/completions   Bearer hgw_...   (over the LAN)
      ▼
hermes-gateway ROUTER  (Go, bare-metal host)
   hermes-gateway.ph.ca / 192.168.2.179:8642   binds 0.0.0.0, no DNAT
      │  auth (token scope) → model == agent → resolve live VM
      ▼
per-agent Firecracker VM — in-VM OpenAI server  :8642  (0.0.0.0 inside the VM)
      │  inference call
      ▼
LLM router  simple-llm-router.ph.ca:8080/v1   (default model: gemma)
```

Request flow, end to end:

1. `hermes-webui` (running in k3s, configured for *gateway* chat backend) sends a
   streaming `POST /v1/chat/completions` to the router over the LAN, presenting the
   `hgw_` gateway key as a bearer token.
2. The **router** runs on the bare-metal sandbox host, binds the host LAN IP
   directly (`0.0.0.0:8642`, DNS `hermes-gateway.ph.ca` / `192.168.2.179`). There
   is **no DNAT** — k3s pods reach it as a plain LAN service. It authenticates the
   token, maps the OpenAI `model` field to an agent type, finds a live VM running
   that agent, and reverse-proxies (unbuffered SSE) to that VM.
3. The **agent VM** runs a plain OpenAI-compatible SSE server on `0.0.0.0:8642`
   *inside* the VM. The router dials it over the host bridge at `<vm_ip>:8642`
   (e.g. `10.0.0.2:8642`).
4. The in-VM agent performs inference against the **LLM router** at
   `http://simple-llm-router.ph.ca:8080/v1`, using `LLM_MODEL` (default `gemma`).

## The abstraction principle

The gateway *is* the abstraction. It presents the Hermes contract and is
protocol-compatible with `NousResearch/hermes-agent@v2026.6.19` — i.e. the API
contract and capabilities the pinned `hermes-webui` image expects. We keep our own
Go router plus hand-rolled agents; we are **not** running the literal
`hermes-agent`.

Because the contract lives at the `:8642` boundary, backends are swappable:

- The shipped in-VM adapter is a hand-rolled Python harness (`deepagents`).
- Any other harness may replace it, as long as it honors the **same wire contract**
  on `:8642` (the endpoints, SSE chunk shape, and per-session history described
  below).

Multi-turn continuity is the backend's responsibility: `hermes-webui` keys
conversations by `X-Hermes-Session-Id` and does **not** resend prior turns, so the
in-VM server must keep per-session history.

## Real hermes-agent backend (v2026.6.19)

The `deepagents` harness above is hand-rolled, but the gateway can also front the
**literal** upstream agent. One backend — exposed to the router as the agent/model
id **`hermes`** — runs the real `NousResearch/hermes-agent` (Docker image
`nousresearch/hermes-agent:v2026.6.19`, the pinned date tag; `latest` is *not* this
build) **pre-baked** into its own Firecracker VM. It honors the same `:8642` wire
contract, so to `hermes-webui` it is just another selectable model: set the OpenAI
`model` field to `"hermes"` and the router resolves it to this VM.

The image is **pre-baked** rather than pulled at boot: the running VM has no
registry egress (it can only reach the LLM endpoint), so the pull happens on the
**host** at build time, the image is `docker save`d to a tar baked into the agent
rootfs, and at VM boot the runner `docker load`s the tar, deletes it, and
`docker run`s the container. Because the loaded image lives in the rootfs, the
agent's ~8 GB rootfs must be large enough to hold it (the tar is deleted right
after `docker load`, so only the loaded image — not image + tar — persists).

### Workflow

```bash
# 1. Pre-bake on the HOST (needs Docker + internet): pulls
#    nousresearch/hermes-agent:v2026.6.19 and docker save's it into the rootfs tree.
sudo bin/sandbox-ctl gateway prebake-hermes

# 2. Build the hermes agent rootfs (bakes in the saved image tar + runner).
sudo bin/sandbox-ctl build-agent hermes

# 3. Launch the hermes VM the router will route to.
sudo bin/sandbox-ctl launch hermes
```

Pre-bake is the only step that touches Docker Hub, and it runs on the host (which
has internet) — `build-agent`'s `customize.sh` runs in a chroot with no dockerd, so
the image cannot be pulled there.

### Harness selection

The mechanism that lets one `start.sh` run different backends is the per-agent
`HARNESS` var (default `"deepagents"`). `config/agents/hermes.yaml` declares
`agent.harness: hermes`; `lib/agentconf.py` threads it into `build/<type>/agent.conf`,
`vm/prepare-rootfs.sh` writes it into `/etc/agent.conf`, and
`rootfs/overlay/opt/agent/start.sh` branches on it:

| `HARNESS` | Behavior |
|-----------|----------|
| `deepagents` (default) | existing flow — `GATEWAY_ENABLED` runs `/opt/agent/gateway_server.py`, else `agent.py` |
| `hermes` | runs the pre-baked container via `/opt/agent/run-hermes.sh` |

All other agent types keep working unchanged because `HARNESS` defaults to
`deepagents`.

### Configuration

The container is the OpenAI server itself; that server is **off by default** in the
image, so `run-hermes.sh` starts the container with these env vars (the
`API_SERVER_KEY` is the same gateway downstream key from
`gateway.agents.hermes.api_server_key`, injected into `/etc/agent.conf` by
`agentconf` and presented by the Go router as `Authorization: Bearer <key>`):

| Env | Value |
|-----|-------|
| `API_SERVER_ENABLED` | `true` |
| `API_SERVER_HOST` | `0.0.0.0` |
| `API_SERVER_PORT` | `8642` |
| `API_SERVER_KEY` | `hgw_YOUR_GATEWAY_KEY` (≥8 chars) |

The container command is `gateway run` (the image entrypoint is `hermes`, so this
runs `hermes gateway run`).

The LLM provider is configured in the data dir's `config.yaml`. The host-side file
baked into the rootfs at `/opt/hermes/data/config.yaml` is mounted into the
container's data dir (`/opt/data` in the official image) and points the agent at the
LAN LLM router:

```yaml
model:
  default: gemma                                   # default model alias
  provider: custom
  base_url: http://simple-llm-router.ph.ca:8080/v1
  api_key: sk-lan                                  # placeholder; LAN router is keyless
```

### Runtime networking & egress

The container runs with **`--network host`** so it binds the VM's `0.0.0.0:8642`
directly — the router then reaches it at `<vm_ip>:8642` exactly like any other
agent VM. Host networking avoids the `docker0` bridge / iptables `raw`-table
problem on the stock guest kernel; for the same reason `/etc/docker/daemon.json`
sets `{"iptables": false}` so dockerd comes up cleanly. `dockerd` is started by
`rootfs/overlay/sbin/agent-init`, and since `start.sh` is launched *before* dockerd
is ready, `run-hermes.sh` waits for `/var/run/docker.sock` before it `docker load`s
and `docker run`s.

Egress is minimal: the **image pull happens on the host at pre-bake time**, so the
running VM needs only the LLM endpoint (`simple-llm-router.ph.ca:8080`) — no
registry, no telemetry. That single destination is allowed by the same per-VM
nftables passthrough described under [Network](#network).

### Using it from the webui

No webui change beyond model selection: with the router running and the hermes VM
launched, set the chat **model** to `hermes` (the gateway key is already scoped
`["*"]`). The router maps `model: "hermes"` to this VM and stream-proxies to the
real hermes-agent inside it.

Because the client's `model` field is the *agent id* (`hermes`), not an LLM model,
the router **rewrites** it to the agent's configured upstream model before
forwarding when `gateway.agents.hermes.model` is set (`gemma` here). The container
is started with `API_SERVER_MODEL_NAME=<that model>` so its API server accepts the
value, and actual inference uses `model.default` from `config.yaml`. (The
hand-rolled deepagents adapter needs no rewrite — it ignores the field and always
infers with `LLM_MODEL`.)

## Routing and auth

**Routing.** The OpenAI `model` field **is** the agent name (e.g. `feature-dev`).
An empty model or the literal `"default"` resolves to `default_agent`. VM
resolution re-reads state on every request: the router scans
`state/vms/*/info.json` and picks the first VM whose `agent_type` matches and whose
`firecracker_pid` is alive (`kill(pid, 0)`). If none is running it returns `502`.

**Auth — two independent hops:**

| Hop | Credential | Where it lives |
|-----|-----------|----------------|
| webui → router | `hgw_` gateway key as a bearer token | `tokens[]` in `gateway.json`; webui sends it as `HERMES_WEBUI_GATEWAY_API_KEY` |
| router → VM | per-agent `API_SERVER_KEY` (downstream bearer) | `agents{}.api_server_key` in `gateway.json` / `/etc/agent.conf` |

Each token carries an `agents` scope. The shipped `hgw_` token is scoped
`["*"]` (every exposed agent); a token scoped to a narrower set that excludes the
resolved agent gets `403`. The downstream `API_SERVER_KEY` is forwarded as
`Authorization: Bearer <key>` **only when non-empty** — an empty key means the
in-VM server requires no auth and no header is sent.

## Wire contract

The router exposes exactly the legacy default `/v1/chat/completions` path that
`hermes-webui` expects (runs-API off; richer tool/approval events are not
implemented).

**These four paths are unchanged and byte-compatible** — same bodies, SSE
framing, 401/403/502 envelopes, and `X-Hermes-Session-Id`/`-Key` passthrough —
across the scheduling/tasks/observability upgrade. The only additions on the
chat path are **saturation-only**: a request may wait in the agent's queue, and
a saturated agent returns `429` (queue full) or `503` (wait timeout), both with
`Retry-After` (see [Scheduling & backpressure](#scheduling--backpressure)). The
router also sends a `traceparent` header downstream, which both backends ignore.

| Method | Path                   | Auth | Returns |
|--------|------------------------|------|---------|
| GET    | `/health`              | no   | `{"status":"ok"}` |
| GET    | `/v1/capabilities`     | no   | `{"features":{"approval_events":false,"run_approval_response":false}}` |
| GET    | `/v1/models`           | yes  | `{"object":"list","data":[{"id":"<agent>","object":"model","owned_by":"hermes-gateway"}]}` |
| POST   | `/v1/chat/completions` | yes  | streaming `text/event-stream` (or a single JSON object when `stream:false`) |

**Request** (`POST /v1/chat/completions`):

```
Content-Type: application/json
Accept: text/event-stream
Authorization: Bearer hgw_...
X-Hermes-Session-Id: <sid>
X-Hermes-Session-Key: webui:<sid>
```
```json
{
  "model": "feature-dev",
  "stream": true,
  "messages": [
    {"role": "system", "content": "..."},
    {"role": "user", "content": "..."}
  ]
}
```

`content` may be a string or a multimodal array; `provider`,
`reasoning_effort`, and `service_tier` are optional. The router passes the body
bytes through verbatim and forwards `X-Hermes-Session-Id` / `X-Hermes-Session-Key`
unchanged.

**Response, `stream:true`** — `text/event-stream`, one `data:` line per chunk:

```
data: {"id":"chatcmpl-...","object":"chat.completion.chunk","created":1750000000,"model":"gemma","choices":[{"index":0,"delta":{"content":"piece"},"finish_reason":null}]}
data: {"id":"chatcmpl-...","object":"chat.completion.chunk","created":1750000000,"model":"gemma","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":34,"estimated_cost":0}}
data: [DONE]
```

`delta.reasoning_content` may carry thinking; the final chunk has `delta:{}`,
`finish_reason:"stop"`, an optional `usage` block, and the stream terminates with
the literal `data: [DONE]`.

**Response, `stream:false`** — a single `chat.completion` object with
`choices[0].message` and a `usage` block.

**Auth failure** — `401` with:

```json
{"error":{"message":"Invalid API key","type":"invalid_request_error"}}
```

## Scheduling & backpressure

The router runs shared **admission control** per agent (an in-VM agent is
effectively single-threaded — one shell, one working tree — so concurrent turns
would interleave side effects). Sync chat and async tasks draw from the same
per-agent slot pool ([ADR 0003](adr/0003-gateway-scheduling-observability-dashboard.md)):

- **Concurrency limit** — `scheduler.default_concurrency` (default `1`)
  simultaneous runs per agent, sync + async combined; a per-agent
  `concurrency` override in `gateway.agents.<name>` wins. The slot is held for
  the **whole stream** — that is what concurrency=1 means.
- **Sync queue** — when the agent is at its limit, an interactive chat waits in
  a bounded FIFO: up to `sync_queue_max` (default `4`) waiters, for at most
  `sync_queue_wait_s` (default `120`) seconds.
- **Async aging** — task-dispatch slot requests queue unbounded and never time
  out, but jump ahead of the sync FIFO once they have waited
  `async_starvation_after_s` (default `300`) seconds, so background tasks are
  not starved forever by interactive traffic.
- **No preemption** — running work is never interrupted to make room (shell
  side effects cannot be safely suspended).

The two saturation responses are **additive** to the chat contract (nothing else
changed); both carry `Retry-After: <scheduler.retry_after_s>` (default `15`):

| Condition | Status | Body |
|-----------|--------|------|
| sync queue full | `429` | `{"error":{"message":"Agent <a> is busy and its queue is full","type":"rate_limit_error"}}` |
| queued longer than `sync_queue_wait_s` | `503` | `{"error":{"message":"Timed out waiting for agent <a>","type":"server_error"}}` |

Admission happens **before** VM resolve, so a VM restarted while the request was
queued is picked up. A request whose client disconnects while queued abandons
its place; no slot leaks.

## Task API

The task subsystem (`tasks.enabled`, default on) runs agent work
**asynchronously**: submit a prompt, poll the record, fetch the growing output —
no client connection has to stay open. Tasks are durable JSON records under
`state/gateway/tasks/` (atomic-rename persistence, output spool sidecars, boot
recovery, GC after `retention_h`). One dispatcher goroutine per agent claims the
highest-priority runnable task (priority desc, then created_at asc) whenever the
scheduler grants it a slot.

Auth is the **existing gateway bearer**; the token's scope must allow the
task's agent. When `tasks.enabled: false`, every `/v1/tasks*` path is simply not
registered and returns `404` like any unknown path.

| Method + path | Request | Response |
|---|---|---|
| `POST /v1/tasks` | `{"agent":"feature-dev", "input":"..."` **XOR** `"request":{"messages":[...]}, "priority":0, "timeout_s":3600, "deadline_s":86400, "not_before":"RFC3339", "max_attempts":2, "retry_on_partial":false, "session_id":"task:custom"}` — `agent` may alias as `model`; `request.model` is ignored; body ≤ 1 MiB (413); priority clamped to [-100,100]; `1≤timeout_s≤86400`, `60≤deadline_s≤604800`, `1≤max_attempts≤5` | `201` full task object; `400` validation; `403` scope; `429`+`Retry-After` when the agent already has ≥ `max_pending_per_agent` non-terminal tasks; `500` if the initial persist fails |
| `GET /v1/tasks?agent=&state=&limit=50&after=<id>` | `state` repeatable, or `active` (= pending+running); scope-filtered | `{"object":"list","data":[{"id","object":"task.summary","agent","state","priority","created_at","updated_at","attempts","cancel_requested","age_s","deadline","error"}],"has_more":false}` sorted created_at desc, keyset cursor |
| `GET /v1/tasks/{id}` | — | full record; `404` envelope `{"error":{"message":"No such task","type":"invalid_request_error"}}` |
| `GET /v1/tasks/{id}/output` | — | `text/plain; charset=utf-8` — the whole output-spool snapshot (poll it to watch a run grow) |
| `POST /v1/tasks/{id}/cancel` | — | `200` updated object; idempotent — a terminal task returns unchanged with `"already_terminal":true` |
| `DELETE /v1/tasks/{id}` | — | `200` on a terminal task; `409` otherwise |

**Lifecycle.** `pending → running → succeeded | failed | cancelled | expired`
(and `running → pending` on a retry re-queue). Per attempt: context deadline of
`min(timeout_s, deadline)`; an **idle watchdog** fails the attempt if no bytes
arrive from the VM for `idle_timeout_s`. Retries back off
`min(retry_backoff_base_s·2ⁿ⁻¹, retry_backoff_cap_s)`. VM-unavailable (no VM,
connect refused, non-2xx before the first byte) **refunds** the attempt and
retries after `vm_unavailable_retry_s`, bounded by the deadline. An attempt that
already produced output is only retried when `retry_on_partial` is set (default
`false` — partial output usually means side effects already happened).

**Record shape** (wire == disk; the disk copy adds `"schema":1`): id
`t-<UTC ts>-<8 hex>`; `session_id` defaults to `task:<id>` (each task gets its
own in-VM history — note the in-VM server never evicts sessions, so unique
session ids grow VM memory by roughly one history per task; acceptable at
homelab volume); `result` (succeeded only) inlines `content` up to 64 KiB
(`content_truncated`, `output_bytes`, `finish_reason`, `usage`), with full text
always at `/output`; `attempt_history` (capped 20) records per-attempt
`started_at/ended_at/vm_ip/outcome/error/output_bytes`; `submit_trace` and
`trace_ids` join the task to its traces.

**Recovery on boot** (same matrix as SIGTERM — `kill -9` and a clean stop yield
the same task outcomes): `.tmp` purged; unparseable records renamed
`*.corrupt`; pending past deadline ⇒ `expired`; `running` ⇒ cancel-requested ⇒
`cancelled`, past deadline ⇒ `expired`, partial output without
`retry_on_partial` ⇒ `failed (interrupted)`, attempts exhausted ⇒
`failed (interrupted)`, otherwise re-queued `pending` with backoff. Claim-time
`attempts++` was already counted — **a crash burns an attempt** (ADR 0003).
Tasks referencing an agent no longer in config are orphaned: counted, logged,
never claimed, expired at their deadline.

## Observability

All observability degrades without touching routing: collector down ⇒ spans
drop (counted); files unreadable ⇒ dashboard panels report `available:false`.

**Traces (OTLP).** The router exports hand-encoded OTLP/HTTP JSON spans to
`observability.otlp_endpoint` (default `http://127.0.0.1:4318`, the local otel
collector; `""` disables export while **still generating and propagating
`traceparent`**, so in-VM traces stay linkable). `sample_ratio` (default `1.0`)
is a deterministic trace-id sampler. A sync chat is **one trace end-to-end**:

```
SERVER "POST /v1/chat/completions"        (root, or child of a valid inbound traceparent;
  │                                        inbound sampled=0 is overridden — ADR 0003)
  ├─ INTERNAL "sched.wait"                (only when the queue wait exceeded 5ms)
  └─ CLIENT "proxy /v1/chat/completions"  (its context rides the traceparent header into the VM)
       └─ SERVER "agent.chat"             (in-VM gateway_server.py)
            └─ agent.graph / tool <name> / chat <model>  (in-VM callback spans)
```

Task attempts are **new roots** (`INTERNAL "task.attempt"`, attrs
`hermes.task_id/_attempt/_priority`, `hermes.agent`, `hermes.outcome`,
`hermes.queue_wait_ms`) with a **span link** to the persisted submit-request
context; each attempt's trace id is appended to the task's `trace_ids`. Export:
one goroutine, cap-1024 queue (non-blocking; drops counted), 512-span/5s
batches, 3s POST timeout, drop-on-failure (the collector is the durability
layer), warnings rate-limited to 1/min.

**Metrics (Prometheus).** `GET /metrics`, text format 0.0.4, prefix
`hermes_gateway_`. Unauthenticated **only from loopback** (the collector scrapes
`127.0.0.1:8642/metrics` secret-free — see `otel/otel-collector.yaml`); any
other caller must present a gateway or dashboard bearer, else `401`.

| Kind | Series |
|------|--------|
| counters | `http_requests_total{path,method,code}` · `auth_failures_total` · `upstream_errors_total{agent,reason=no_vm\|connect\|status_5xx}` · `stream_bytes_total{agent}` · `sched_admitted_total{agent,class=sync\|task}` · `sched_rejected_total{agent,reason=queue_full\|wait_timeout\|client_gone}` · `task_transitions_total{agent,to_state}` · `task_retries_total{agent}` · `otlp_export_batches_total{outcome=ok\|error}` · `otlp_spans_exported_total` · `otlp_spans_dropped_total` · `dashboard_source_errors_total{source=traces\|squid}` |
| gauges | `build_info{version}` · `http_inflight_requests{path}` · `sched_queue_depth{agent,class}` · `sched_running{agent}` · `tasks{agent,state}` · `task_oldest_pending_age_seconds{agent}` · `vm_up{agent}` · `store_degraded` — scheduler/task/VM gauges are computed at scrape time from live snapshots, so they cannot drift |
| histograms | `http_request_duration_seconds{path}` · `proxy_ttfb_seconds{agent}` · `sched_wait_seconds{agent}` · `task_duration_seconds{agent,outcome}` |

**Logs.** `log/slog` structured logging (JSON by default,
`observability.log_format: text` for plain) on stdout — `sandbox-ctl gateway
start` redirects it to `state/logs/gateway.log`. Canonical events:
`http_request`, `sched_reject`, `task_submit/start/finish/retry/cancel`,
`vm_resolve_fail`, `store_error`, `startup`, `shutdown`, `fatal`. In-request
events carry `trace_id`/`span_id` for joining with traces; token **names** only
— never secrets or message content.

## Dashboard

An embedded single-page ops dashboard (dark, zero CDN/fonts/build step — all
assets compiled into the binary) at **`/dashboard/`** (`/dashboard` redirects
`302`). Panels: status strip (gateway + dependency dots), per-agent cards (VM
liveness, running/waiting with run ids and trace ids, admission counters),
traffic charts (1h of in-memory 10s rings — fresh even when the collector is
down, see ADR 0003), tasks table with detail drawer/output/cancel, recent trace
summaries (tail of the collector's `traces.jsonl`), and an egress panel (tail of
the squid access log) with a dedicated denied list.

**Auth.** The shell and static assets are an unauthenticated inert page. Every
`/dashboard/api/*` call requires `Authorization: Bearer` from the dedicated
`dashboard.tokens` list (constant-time compare; the UI prompts once and keeps it
in localStorage — no cookies). An empty token list **fails closed**: `403
"dashboard token not configured"`. `dashboard.enabled: false` unregisters
everything (plain `404`). Data-source problems are `200` + `available:false`,
never 5xx; auth failures are `401`/`403`.

| Endpoint (poll cadence) | Returns |
|---|---|
| `GET /dashboard/api/overview` (2s) | gateway info (`started_at`, `uptime_s`, `pid`, `version`), dependency dots (`collector` via last successful OTLP export, `traces_file`, `squid_log`, `tasks_dir`), per-agent `{vm, limit, queue_cap, running[], waiting[], counters, last_error}`, `tasks_by_state` (+`orphaned`), `store_degraded`, last-minute totals (`reqs_1m`, `errors_1m`, `p95_ms_1m`) |
| `GET /dashboard/api/timeseries?window_s=3600` (10s) | `{"start_unix","step_s":10,"buckets",series:{"_total":{count,errors,lat_ms_avg,lat_ms_p95},"<agent>":…},"gauges":{queue_depth,running}}` — fixed-length zero-filled arrays, oldest first |
| `GET /dashboard/api/tasks?state=&agent=&limit=100&after=` (5s) | task summaries across **all** agents (the dashboard token is ops-privileged); `tasks/{id}` adds `request_preview` (first 2 KiB of the last user message); `tasks/{id}/output` returns the spool; `POST tasks/{id}/cancel` mirrors `/v1/tasks/{id}/cancel` |
| `GET /dashboard/api/traces?limit=50&window_s=900` (10s) | `{"available","file","parsed_lines","skipped_lines","traces":[{"trace_id","root_service","root_name","start","duration_ms","span_count","services","error"}]}` from the tail of `observability.traces_file` (rotation-aware, malformed lines counted) |
| `GET /dashboard/api/egress?window_s=900` (15s) | `{"available","window_s","log","lines","skipped_lines","totals":{requests,denied,bytes},"by_agent","top_hosts","denied":[…]}` from the tail of `observability.squid_access_log`; client IPs are mapped to agents via live VM state |

## Configuration

### `config/sandbox.yaml`

Two blocks drive the gateway. The `llm` block points the in-VM agents at the LAN
LLM router; the `gateway` block defines the router itself.

```yaml
llm:
  api_base: "http://simple-llm-router.ph.ca:8080/v1"
  api_key: "sk-lan"          # placeholder; LAN-trusted
  model: "gemma"             # aliases: gemma (default), north, smart, council

gateway:
  enabled: true
  bind: "0.0.0.0"            # host LAN bind for the router
  port: 8642
  default_agent: "feature-dev"
  vm_gateway_port: 8642      # in-VM OpenAI server port (inside each VM)

  # Scheduling, async tasks, observability, dashboard. EVERY key below is
  # optional: defaults live ONLY in the Go router (applyDefaults()), and
  # compile-gateway copies these blocks into gateway.json verbatim when
  # present — omit a block entirely and the router behaves the same. The
  # values shown are the built-in defaults.
  scheduler:
    default_concurrency: 1          # per-agent simultaneous runs, sync+async combined
    sync_queue_max: 4               # waiting sync requests per agent before 429
    sync_queue_wait_s: 120          # max sync queue wait before 503
    async_starvation_after_s: 300   # aging: async slot request jumps sync FIFO after this
    retry_after_s: 15               # Retry-After on 429/503
  tasks:
    enabled: true
    dir: ""                         # "" -> <state_dir>/gateway/tasks
    default_timeout_s: 3600         # per attempt
    default_deadline_s: 86400
    default_max_attempts: 2
    retry_on_partial: false
    retry_backoff_base_s: 10        # backoff(n)=min(base*2^(n-1), cap)
    retry_backoff_cap_s: 600
    idle_timeout_s: 900             # no SSE bytes this long -> attempt fails
    vm_unavailable_retry_s: 30
    retention_h: 168
    max_records: 2000
    max_pending_per_agent: 200
  observability:
    otlp_endpoint: "http://127.0.0.1:4318"   # "" disables span export (traceparent still propagated)
    sample_ratio: 1.0
    log_format: "json"              # json|text
    traces_file: "/var/log/otel/traces.jsonl"
    squid_access_log: "/var/log/squid/access.log"
  dashboard:
    enabled: true
    tokens: []                      # dedicated ops bearers, e.g. ["hgwd_<48hex>"];
                                    # empty => /dashboard/api/* fail closed (403)

  tokens:
    - name: "hermes-webui"
      token: "hgw_YOUR_GATEWAY_KEY"
      agents: ["*"]
  agents:                    # exposed agent types + downstream per-VM key (may be "")
    # optional per-agent `concurrency` overrides scheduler.default_concurrency
    feature-dev: { api_server_key: "", concurrency: 1 }
    debugger:    { api_server_key: "" }
    devops:      { api_server_key: "" }
    researcher:  { api_server_key: "" }
    security:    { api_server_key: "" }
```

`sandbox-ctl gateway compile` renders this into `state/gateway/gateway.json`:

```json
{
  "bind": "0.0.0.0",
  "port": 8642,
  "default_agent": "feature-dev",
  "state_dir": "/home/letsrtfm/AI/agent-sandbox/state",
  "vm_gateway_port": 8642,
  "tokens": [{ "name": "hermes-webui", "token": "hgw_...", "agents": ["*"] }],
  "agents": { "feature-dev": { "api_server_key": "" }, "...": {} }
}
```

The router resolves its config path from `-config <path>`, then the
`GATEWAY_CONFIG` env var, then the default
`/home/letsrtfm/AI/agent-sandbox/state/gateway/gateway.json`.

A **legacy 7-key `gateway.json`** (as shown above, without the new blocks)
loads unchanged: defaults for `scheduler`/`tasks`/`observability`/`dashboard`
are applied only inside the Go router, never by the Python compiler, so a
stale or hand-edited config behaves identically to a freshly compiled one.

### Per-agent `/etc/agent.conf`

`compile-gateway`'s sibling agent compile injects three gateway vars into each
agent's `/etc/agent.conf` (written by `vm/prepare-rootfs.sh`):

| Var | Default | Meaning |
|-----|---------|---------|
| `GATEWAY_ENABLED` | `1` | `1` runs the in-VM OpenAI server instead of the file-watch loop |
| `GATEWAY_PORT` | `8642` | port the in-VM server listens on |
| `API_SERVER_KEY` | *(empty)* | downstream bearer the in-VM server requires; empty = no auth |

The in-VM agent also reads `LLM_API_BASE`, `LLM_API_KEY`, and `LLM_MODEL`
(`gemma`) for inference.

## Pointing `hermes-webui` at the gateway

Set these on the `hermes-webui` deployment (k3s) so it uses the gateway chat
backend at the default legacy `/v1/chat/completions` path (runs-API off):

```
HERMES_WEBUI_CHAT_BACKEND=gateway
HERMES_WEBUI_GATEWAY_BASE_URL=http://192.168.2.179:8642
# or, by DNS:
HERMES_WEBUI_GATEWAY_BASE_URL=http://hermes-gateway.ph.ca:8642
HERMES_WEBUI_GATEWAY_API_KEY=hgw_YOUR_GATEWAY_KEY
```

## CLI workflow

```bash
# 1. Render config from sandbox.yaml → state/gateway/gateway.json
bin/sandbox-ctl gateway compile

# 2. Build the router binary (needs Go; std-lib only, builds offline)
bin/sandbox-ctl gateway build

# 3. Start the router on the host LAN (compiles/builds first if needed)
sudo bin/sandbox-ctl gateway start

# 4. Check router status + which exposed agents have a live VM
bin/sandbox-ctl gateway status

# 5. Launch the agent VM(s) the gateway will route to
sudo bin/sandbox-ctl launch feature-dev

# Stop the router
sudo bin/sandbox-ctl gateway stop
```

`gateway start` writes `state/gateway/gateway.pid`, logs to
`state/logs/gateway.log`, and opens the host firewall for the router port via
`allow_gateway_ingress` (see Network below).

## Network

**Inbound (k3s → router).** The router binds the host LAN IP directly and there is
no DNAT, so plain `LAN → host:8642` is already accepted by the kernel —
`vm_filter`'s input chain only drops `tap-vm*` traffic. The one thing that can
block it is a separate host firewall: if **firewalld** is installed *and* active,
`sandbox-ctl gateway start` calls `allow_gateway_ingress 8642` to open the port at
runtime. (It is a no-op when firewalld is absent or stopped. Make it permanent by
adding `--permanent` and reloading if it should survive a firewalld reload.)

**Outbound (VM → LLM router).** Each agent VM reaches
`simple-llm-router.ph.ca:8080` through the existing per-VM nftables passthrough:
`lib/network.sh` resolves the LLM hostname to an IP (`resolve_ipv4`) and installs
an allow rule for that destination, so the VM can call the LAN LLM router while the
rest of its egress stays filtered by Squid/nftables.

**VM-side name resolution.** The VM's dnsmasq forwards only to *public* upstreams,
so an internal `.ph.ca` name would `NXDOMAIN` inside the VM. `vm/prepare-rootfs.sh`
therefore resolves the LLM host **host-side at launch** (same `resolve_ipv4`) and
pins `<ip> <host>` into the VM's `/etc/hosts` (staged in `/etc/agent-hosts`, which
`agent-init` appends at boot). For the hermes container — which gets its own
`/etc/hosts` even under `--network host` — `run-hermes.sh` passes the mapping via
`--add-host`. So the friendly DNS name keeps working inside the sandbox without
exposing internal DNS to the VM.

## Smoke test

With the router running and at least one agent VM launched
(`sudo bin/sandbox-ctl launch feature-dev`):

```bash
GW=http://192.168.2.179:8642
KEY=hgw_YOUR_GATEWAY_KEY

# Liveness (no auth)
curl -s $GW/health
# {"status":"ok"}

# Capabilities (no auth; legacy chat path, both flags false)
curl -s $GW/v1/capabilities
# {"features":{"approval_events":false,"run_approval_response":false}}

# Models visible to this token's scope (auth required)
curl -s -H "Authorization: Bearer $KEY" $GW/v1/models

# Streaming chat against the feature-dev agent
curl -N -s $GW/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Accept: text/event-stream" \
  -H "Authorization: Bearer $KEY" \
  -H "X-Hermes-Session-Id: smoke-1" \
  -H "X-Hermes-Session-Key: webui:smoke-1" \
  -d '{"model":"feature-dev","stream":true,
       "messages":[{"role":"user","content":"Say hello in one short sentence."}]}'
# data: {"id":"chatcmpl-...","object":"chat.completion.chunk",...,"choices":[{"index":0,"delta":{"content":"Hello"},...}]}
# ...
# data: [DONE]
```

A missing or unknown bearer returns `401
{"error":{"message":"Invalid API key","type":"invalid_request_error"}}`; a token
not scoped for the agent returns `403`; and a request for an agent with no running
VM returns `502`.

## Agent memory (mnemosyne)

Durable agent memory is part of the hosted agent surface (see
[ADR 0002](adr/0002-agent-memory-hosted-surface.md)). We host
[mnemosyne](https://pypi.org/project/mnemosyne-memory/) as **one shared LAN MCP
service** on the sandbox host and wire every agent VM to it. mnemosyne is
MCP-only (no REST API): agents consume it over MCP-over-SSE. The store is a single
shared SQLite tree partitioned per agent (`author_id` / `bank`).

### Contract

| Thing | Value |
|-------|-------|
| Host bind | `0.0.0.0:8077` |
| Hostname inside VMs | `mnemosyne.host` (→ the VM's gateway IP `10.0.<slot>.1`) |
| MCP SSE URL | `http://mnemosyne.host:8077/sse` |
| Auth | bearer token — `Authorization: Bearer <token>` (`memory.token`) |
| Data | `state/mnemosyne/data` (persistent; `state/` is gitignored) |
| Venv | `state/mnemosyne/venv` |

### Configuring it

Add a `memory` block to `config/sandbox.yaml`:

```yaml
memory:
  enabled: true
  port: 8077
  token: "mnem_YOUR_LONG_RANDOM_TOKEN"   # gitignored real value
  embeddings: "fastembed"                # fastembed | none | <remote /v1 url>
```

`sandbox-ctl config compile` threads this through the existing pipeline:
`compile-global` writes `MEMORY_ENABLED`, `MNEMOSYNE_PORT`, `MNEMOSYNE_TOKEN`,
`MNEMOSYNE_EMBEDDINGS` into `build/sandbox.conf`; the per-agent compile writes
`MNEMOSYNE_ENABLED`, `MNEMOSYNE_PORT`, `MNEMOSYNE_TOKEN`, and
`MNEMOSYNE_URL=http://mnemosyne.host:<port>/sse` into `build/<type>/agent.conf` and
thence each VM's `/etc/agent.conf` (auto-exported by `start.sh`'s `set -a`).

### CLI workflow

```bash
# 1. Install the service venv under state/mnemosyne/ (one-time; re-runnable).
sudo bin/sandbox-ctl mnemosyne install

# 2. Start it on the host LAN (binds 0.0.0.0:8077, opens the VM→host firewall).
sudo bin/sandbox-ctl mnemosyne start

# 3. Check it (process + port + data dir).
bin/sandbox-ctl mnemosyne status

# 4. Stop it (also removes the nftables input allow).
sudo bin/sandbox-ctl mnemosyne stop
```

`mnemosyne start` runs `mnemosyne mcp --transport sse --host 0.0.0.0 --port 8077`
with `MNEMOSYNE_DATA_DIR=state/mnemosyne/data` and `MNEMOSYNE_MCP_TOKEN` from the
config. `start`/`stop` require root because they edit the firewall: like the OTel
allow in `network/setup-host-network.sh`, they add (and on stop remove) an
idempotent `vm_filter` **input** rule
`iifname "tap-vm*" tcp dport 8077 accept`, so VMs can reach the service on their
gateway IP while the rest of VM→host stays dropped.

### How the agents consume it

**Name resolution.** Agent VMs reach the host at their per-VM gateway IP, which is
not fixed, so `agent-init` publishes `mnemosyne.host` → the gateway IP (parsed from
`/proc/cmdline`) right after it writes `/etc/hosts` / appends `/etc/agent-hosts` —
the same pattern used to pin the LLM router name.

**hermes.** `vm/prepare-rootfs.sh` adds an `mcp` server block to the container's
`/opt/hermes/data/config.yaml` (only when `MNEMOSYNE_ENABLED=1`):

```yaml
mcp:
  servers:
    mnemosyne:
      url: http://mnemosyne.host:8077/sse
      transport: sse
      headers: { Authorization: "Bearer <token>" }
```

Because the container gets its own `/etc/hosts` even under `--network host`,
`run-hermes.sh` passes `--add-host mnemosyne.host:<gw>` (with
`gw=$(ip route | awk '/default/{print $3}')`).

**deepagents.** `agent.py` connects with
[`langchain-mcp-adapters`](https://pypi.org/project/langchain-mcp-adapters/) and
adds the memory tools to the agent:

```python
client = MultiServerMCPClient({"mnemosyne": {
    "url": MNEMOSYNE_URL, "transport": "sse",
    "headers": {"Authorization": f"Bearer {MNEMOSYNE_TOKEN}"}}})
tools = await client.get_tools()
agent = create_deep_agent(model=..., backend=..., system_prompt=..., tools=tools)
```

Tool loading is async and runs on `gateway_server.py`'s persistent event loop. It
**degrades gracefully**: if `MNEMOSYNE_ENABLED` is unset/empty or the MCP connect
fails, the agent is built with no memory tools and a warning is logged — it never
crashes.

### Persistence & the host-Python caveat

The store persists at `state/mnemosyne/data` (never baked into a rootfs), so memory
survives VM restarts and image rebuilds and is shared across all agents.

Embeddings are optional. The host Python is **3.14** (bleeding edge), where
`fastembed` / `sqlite-vec` wheels may not yet exist. `mnemosyne install` therefore
prefers a **`python3.12`** if one is on the host, and **degrades gracefully**: if
the `[embeddings]` extra can't be installed it falls back to a keyword-only (FTS5)
install and logs it clearly, rather than hard-failing. Set `embeddings: "none"` to
force keyword-only, or point `embeddings` at a remote OpenAI-compatible `/v1` URL
to use a remote embedder instead of local `fastembed`.

## See also

- [`gateway/README.md`](../gateway/README.md) — the Go router internals (config
  schema, auth model, routing, endpoint table).
- [ADR 0003](adr/0003-gateway-scheduling-observability-dashboard.md) — why
  scheduling, tasks, observability and the ops dashboard live in this gateway,
  and the judgment calls behind them.
- [Architecture](architecture.md) — host components, config pipeline, network
  model.
- [Operations](operations.md) — launching, observing, and troubleshooting VMs;
  includes the gateway ops runbook (metrics, dashboard tokens, task recovery,
  degraded modes).
