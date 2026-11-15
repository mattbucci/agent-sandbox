# Agent memory

Agents share a long-term memory that outlives sessions and VMs. With
`memory.backend: hindsight` it is a hierarchy, kept by
[Hindsight](https://github.com/vectorize-io/hindsight) running on the sandbox host:

| Layer | Produced by | What it is |
|---|---|---|
| **Mental models** | You, in YAML | A standing question with an answer Hindsight keeps written — a document per question |
| **Observations** | Hindsight, automatically | Beliefs consolidated from related facts, with their evidence |
| **Facts** | Hindsight, from what agents retain | World facts ("the staging DB is orion-staging") and the agent's own experiences ("I ran the migration and it failed") |

Each layer is a more settled version of the one below it. Agents write at the
bottom — they retain conversations — and read from the top down.

## What an agent does with it

The in-VM server (`rootfs/overlay/opt/agent/gateway_server.py`) does this for
every `deepagents` and `omp` agent, with no cooperation needed from the model:

- **Before a turn** the user message is given a `<memory>` block: the agent's
  mental models (a cached database read), then the observations and facts most
  relevant to the message (one recall, typically tens of milliseconds).
- **After a turn** the exchange is queued and sent in the background. Hindsight
  extracts facts from it with the LLM, consolidates them into observations, and
  refreshes any mental model whose scope changed. This takes minutes with a local
  model and never delays the agent.

Only the copy of the message handed to the agent is augmented; session history
keeps the user's own words, so recalled memory is never stored again as new.
A stateless harness (`deepagents`) gets the mental models every turn; one that
keeps its own conversation (`omp`) gets them on a session's first turn and only
the relevant memories after that.

Memory never fails a turn: every call is time-bounded and on any error the agent
runs without it.

Agents can also use memory deliberately:

| Harness | Tools |
|---|---|
| `deepagents` | `memory_recall`, `memory_retain`, `memory_reflect` |
| `omp` | `mcp__memory_recall`, `mcp__memory_retain`, `mcp__memory_reflect`, plus read-only mental-model and knowledge-page tools |
| `hermes` | not wired — the container runs without shared memory |

`reflect` asks Hindsight to reason over the whole hierarchy and can take minutes.

## One bank, tagged by agent

All agents use one bank (`memory.bank`, default `agent-sandbox`). Everything an
agent retains is tagged `agent:<type>`.

- `recall_scope: shared` (default) — an agent recalls from every agent's
  memories. What the debugger learns, the coder can use.
- `recall_scope: agent` — only its own and untagged memories. Set globally or
  per agent (`memory.recall_scope` in the agent YAML).
- An agent can use a separate bank (`memory.bank` in its YAML) or opt out
  (`memory: {enabled: false}`).

## Declaring mental models

Shared models go in `config/sandbox.yaml`; they are built from the whole bank
and shown to every agent:

```yaml
memory:
  mental_models:
    - id: user-preferences
      name: User preferences
      query: >
        What are the preferences, standing instructions and working style of
        the people the agents work for?
```

Per-agent models go in `config/agents/<type>.yaml`; they are built only from
that agent's memories and shown only to it:

```yaml
memory:
  mental_models:
    - id: playbook
      name: Coder playbook
      query: >
        What coding conventions, tooling quirks and recurring mistakes has the
        coder agent learned across its tasks?
```

```bash
bin/sandbox-ctl memory apply --dry-run   # show the plan
bin/sandbox-ctl memory apply             # create / update in Hindsight
```

Applying is idempotent: models have stable ids derived from the config (`<id>`
for shared, `<agent>-<id>` for per-agent), so only what changed is updated.
Models removed from the config are reported, not deleted. Each model refreshes
itself after consolidation produces something new in its scope.

## Running it

```yaml
# config/sandbox.yaml
memory:
  enabled: true
  backend: hindsight
  port: 8890
  token: "hs_..."
```

```bash
sudo bin/sandbox-ctl memory install    # pinned hindsight-api in state/hindsight/venv (several GB)
sudo bin/sandbox-ctl memory start      # start, open the VM firewall, apply mental models
bin/sandbox-ctl memory status          # service state + size of each layer
sudo bin/sandbox-ctl memory stop
```

- Runs as the checkout's owner, not root (its embedded PostgreSQL refuses root).
  Data and the model cache live in `state/hindsight/home`.
- Uses the sandbox's LLM router for extraction, consolidation and mental models
  (`memory.llm_model`, default `llm.model`). The model must be one the router
  serves; `memory.max_completion_tokens` (default 16000) must fit its output limit.
- Embeddings and reranking run locally on CPU inside the service (~2 GB RAM).
- Every request needs the bearer token. The MCP endpoint exposes only tools that
  read and add to memory — not the ones that delete banks, memories or models.
- The boot service restarts it.

Agents get the code that uses it from the runtime overlay, so after enabling the
backend rebuild and relaunch them: `sudo bin/sandbox-ctl build-agent <type>`.

## Things to know

- **Memory can carry instructions.** Anything an agent is told can come back
  later in another agent's `<memory>` block. The block is framed as background
  knowledge rather than instructions, but a model may still act on it. Only
  conversation turns and explicit `retain` calls are stored.
- **The token is shared.** Every agent has the same bearer token in
  `/etc/agent.conf`, and the REST API (unlike the MCP tool list) is not
  restricted — a compromised agent could alter or delete the bank.
- **Extraction quality follows the model.** Facts, observations and mental
  models are written by the configured LLM and can contain its mistakes.
- **Everything is remembered**, including trivial questions. There is no
  filtering of which turns are worth keeping.
- **The embedded database is single-host.** Back up `state/hindsight/home` to
  keep the memory; point `HINDSIGHT_API_DATABASE_URL` at an external PostgreSQL
  if it should outgrow that.
- `memory.backend: mnemosyne` keeps the earlier flat MCP store
  ([details](hermes-gateway.md#agent-memory-mnemosyne)); mental models need Hindsight.
