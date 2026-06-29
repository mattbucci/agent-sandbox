# hermes-gateway (router)

The bare-metal **ROUTER** half of the Hermes Gateway. It runs on the Firecracker
sandbox host, binds the host LAN IP, and presents an OpenAI-compatible API to the
`hermes-webui` frontend (running in k3s). Each incoming request is authenticated,
mapped to an agent type via its `model` field, routed to a live Firecracker VM
running that agent, and reverse-proxied (streaming SSE) to the in-VM server.

```
hermes-webui (k3s)  --LAN host:8642  Bearer hgw_...-->  hermes-gateway ROUTER (this)
    -> auth (token scope) -> model field == agent -> resolve running VM IP
    -> stream-proxy to <vm_ip>:8642  (in-VM Python server) -> LLM router
```

Implemented with the Go **standard library only** (no external modules), so it
builds on an offline host.

## Build

```sh
cd gateway
go build -o hermes-gateway .
```

## Run

```sh
./hermes-gateway -config /home/letsrtfm/AI/agent-sandbox/state/gateway/gateway.json
```

Config path resolution order:

1. `-config <path>` flag
2. `GATEWAY_CONFIG` environment variable
3. default `/home/letsrtfm/AI/agent-sandbox/state/gateway/gateway.json`

Request activity (method, path, chosen agent, VM IP, status) is logged to stderr.

## Config file (`gateway.json`)

Produced by `lib/agentconf.py` (`compile-gateway`) from `config/sandbox.yaml`.
The schema is authoritative and shared with the rest of the toolchain:

```json
{
  "bind": "0.0.0.0",
  "port": 8642,
  "default_agent": "feature-dev",
  "state_dir": "/home/letsrtfm/AI/agent-sandbox/state",
  "vm_gateway_port": 8642,
  "tokens": [
    { "name": "hermes-webui", "token": "hgw_...", "agents": ["*"] }
  ],
  "agents": {
    "feature-dev": { "api_server_key": "" },
    "debugger":    { "api_server_key": "" }
  }
}
```

- `bind` / `port` — host LAN listen address (LAN-reachable).
- `default_agent` — used when the request `model` is empty or `"default"`.
- `state_dir` — root scanned for VM state at `state_dir/vms/*/info.json`.
- `vm_gateway_port` — port the in-VM server listens on (`8642`).
- `tokens[]` — bearer credentials; `agents` is the authorization scope
  (`["*"]` = every exposed agent).
- `agents{}` — exposed agent types and their downstream `api_server_key`
  (empty string = the in-VM server requires no auth, so no `Authorization`
  header is forwarded).

## Auth model

Every request (except `/health` and `/v1/capabilities`) requires
`Authorization: Bearer <token>`. The presented token is matched against
`tokens[].token`; a missing or unknown token returns:

```
HTTP 401
{"error":{"message":"Invalid API key","type":"invalid_request_error"}}
```

The matched token's `agents` scope authorizes which agents it may reach. A token
scoped to a set that does not include the resolved agent gets `403`.

## Routing model

The OpenAI `model` field **is** the agent name (e.g. `"feature-dev"`). Empty or
`"default"` resolves to `default_agent`.

VM resolution re-reads state on every request (cheap): it scans
`state_dir/vms/*/info.json` and picks the first VM whose `agent_type` matches the
requested agent and whose `firecracker_pid` is alive (`kill(pid, 0)`). If none is
running:

```
HTTP 502
{"error":{"message":"No running VM for agent <agent>", ...}}
```

The downstream request to `http://<vm_ip>:<vm_gateway_port>/v1/chat/completions`
reuses the original body bytes, sets `Content-Type: application/json` and
`Accept: text/event-stream`, passes `X-Hermes-Session-Id` and
`X-Hermes-Session-Key` through, and sets `Authorization: Bearer <api_server_key>`
only when that key is non-empty. The downstream status and `Content-Type` are
mirrored to the client and the body is streamed back **unbuffered** (flushed
after every chunk). Client disconnects cancel the upstream call via the request
context.

## Endpoints

| Method | Path                   | Auth | Description |
|--------|------------------------|------|-------------|
| GET    | `/health`              | no   | `{"status":"ok"}` |
| GET    | `/v1/capabilities`     | no   | feature flags (both `false`; legacy chat path) |
| GET    | `/v1/models`           | yes  | agents visible to the token scope |
| POST   | `/v1/chat/completions` | yes  | stream-proxy to the agent's VM |
