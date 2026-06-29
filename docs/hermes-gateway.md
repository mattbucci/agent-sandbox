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
  tokens:
    - name: "hermes-webui"
      token: "hgw_YOUR_GATEWAY_KEY"
      agents: ["*"]
  agents:                    # exposed agent types + downstream per-VM key (may be "")
    feature-dev: { api_server_key: "" }
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

## See also

- [`gateway/README.md`](../gateway/README.md) — the Go router internals (config
  schema, auth model, routing, endpoint table).
- [Architecture](architecture.md) — host components, config pipeline, network
  model.
- [Operations](operations.md) — launching, observing, and troubleshooting VMs.
