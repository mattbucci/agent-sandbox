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

The tools act on the agent's primary bank, within its access.

`reflect` asks Hindsight to reason over the whole hierarchy and can take minutes.

## Who can reach what

Each agent has its own memory credential, and the memory service decides from
an access policy what that agent can reach. The policy comes from the YAML;
`sandbox-ctl memory access` prints it:

```
$ bin/sandbox-ctl memory access
coder
  bank agent-sandbox      read: everything                    write: agent:coder
    mental models: environment, user-preferences, lessons, coder-playbook
security
  bank agent-sandbox      read: agent:security, shared        write: agent:security
    mental models: environment, user-preferences, lessons
  bank advisories         read: everything                    write: nothing
    mental models: open-cves
```

### How data is scoped

Inside a bank, Hindsight scopes data by **tag**. Everything an agent writes is
tagged `agent:<type>` — the service adds the tag and refuses any other, so an
agent cannot write as another agent. Data with no tag is the **shared** scope;
only the admin writes there.

Access is a `read` list and a `write` list of:

| Word | Means |
|---|---|
| `own` | the agent's own tag, `agent:<type>` |
| `shared` | the untagged, shared scope (read only) |
| `all` | everything in the bank (read only) |
| an agent type, e.g. `debugger` | that agent's tag |
| any other string, e.g. `team:platform` | that tag verbatim |

The read list governs all three layers at once: the memories and observations
an agent can recall, and the mental models it can see.

### Per-agent access

```yaml
# config/sandbox.yaml — the default for every agent (this is also the built-in default)
memory:
  access:
    read: [all]
    write: [own]
```

```yaml
# config/agents/security.yaml — this agent only
memory:
  access:
    read: [own, shared]        # its own memories + shared; not other agents'
    write: [own]
```

`read: [own, shared, debugger]` adds one other agent's memories;
`write: []` makes an agent read-only (its turns are then not retained).

### Additional banks

A bank is a separate memory with its own facts, observations and mental models.
Declare it once, then grant agents access:

```yaml
# config/sandbox.yaml
memory:
  banks:
    advisories:
      mission: Security advisories and how they were handled.
      mental_models:
        - id: open-cves
          name: Open CVEs
          query: Which vulnerabilities are still open, and what is blocking each?
```

```yaml
# config/agents/security.yaml
memory:
  banks:
    advisories: {read: [all], write: [own]}
```

An agent's turns are retained in its primary bank (`memory.bank`, default the
shared `agent-sandbox`). Additional banks it can read are recalled from
alongside it, and their mental models are loaded too. An agent has no access to
a bank it was not granted. `memory: {enabled: false}` removes an agent from
memory entirely.

### Mental models: who sees them, what they are built from

Two things are set per model, independently:

| Key | Meaning | Default |
|---|---|---|
| `visible_to` | who may read it: `all`, or a list of agents / tags | `all` in `sandbox.yaml`; the agent itself in an agent YAML |
| `sources` | whose memories it is written from: `all`, or a list of agents / tags | same |

```yaml
memory:
  mental_models:
    - id: handoff
      name: Debugging handoff
      query: What has the debugger established that the coder should act on?
      visible_to: [coder, debugger]
      sources: [debugger]
```

An agent sees a model when its read access covers one of the model's tags (or
the model is visible to `all` and the agent may read `shared`). A model with
`sources: all` summarizes every agent's memories, so making it visible to an
agent that cannot read those memories directly is a deliberate disclosure — that
is what a shared model is for, but choose its question accordingly.

Of the models an agent may read, the ones put in front of each turn are those
visible to `all` and those naming the agent; others stay available through the
memory tools.

Per-agent models go in `config/agents/<type>.yaml`:

```yaml
memory:
  mental_models:
    - id: playbook
      name: Coder playbook
      query: >
        What coding conventions, tooling quirks and recurring mistakes has the
        coder agent learned across its tasks?
```

### Applying changes

```bash
bin/sandbox-ctl memory apply --dry-run   # banks, models and their scopes
bin/sandbox-ctl memory apply             # write the policy; create / update banks and models
bin/sandbox-ctl memory access [agent]    # the resulting access
```

Access changes take effect on the next request — the running service re-reads
the policy — with no restart and no agent relaunch. Granting an agent a *new
bank to recall from* needs that agent relaunched (the list of banks is in its
VM's config). Applying is idempotent: models have stable ids (`<id>` for shared,
`<agent>-<id>` for per-agent), only what changed is updated, and models removed
from the config are reported, not deleted.

### What is enforced, and where

The service runs two extensions from `lib/hindsight_ext/`:

- **Authentication.** Every request needs a bearer token. `memory.token` is the
  admin and stays on the host; each agent's token is derived from it
  (HMAC, per agent type) and is all that is written into its VM. The policy file
  holds only digests. A missing or unreadable policy admits nobody.
- **Authorization**, on every path — REST, the MCP endpoint and the tools:
  - a bank the agent was not granted: refused (403), and absent from its bank list;
  - reads confined to its read scope — recall, reflect, lists, and mental
    models (a model outside the scope is a 404, not a 403);
  - writes get the agent's tag; a write naming a tag outside its write scope is refused;
  - creating or deleting banks, changing a bank's configuration, creating /
    editing / deleting / refreshing mental models, editing memories, imports and
    on-demand consolidation: admin only;
  - MCP tools are limited to ones that read or add, minus `retain` for a
    read-only agent.

Background work the service starts itself — extraction, consolidation, mental
model refreshes — is not scoped; a mental model's `sources` bounds what it reads.

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
- Every request needs a bearer token, and each agent has its own; see
  [Who can reach what](#who-can-reach-what).
- The boot service restarts it.

Agents get the code that uses it from the runtime overlay, so after enabling the
backend rebuild and relaunch them: `sudo bin/sandbox-ctl build-agent <type>`.

## Things to know

- **Memory can carry instructions.** Anything an agent is told can come back
  later in another agent's `<memory>` block. The block is framed as background
  knowledge rather than instructions, but a model may still act on it. Only
  conversation turns and explicit `retain` calls are stored.
- **An agent's token is in its VM** (`/etc/agent.conf`). A compromised agent can
  do what its policy allows — read its scope, add memories under its own tag —
  and nothing else; it cannot delete or rewrite anything. Changing
  `memory.token` rotates every agent's token at once (relaunch the agents).
- **Shared recall is the default.** Unless you restrict `memory.access`, every
  agent reads every agent's memories.
- **Extraction quality follows the model.** Facts, observations and mental
  models are written by the configured LLM and can contain its mistakes.
- **Everything is remembered**, including trivial questions. There is no
  filtering of which turns are worth keeping.
- **The embedded database is single-host.** Back up `state/hindsight/home` to
  keep the memory; point `HINDSIGHT_API_DATABASE_URL` at an external PostgreSQL
  if it should outgrow that.
- `memory.backend: mnemosyne` keeps the earlier flat MCP store
  ([details](hermes-gateway.md#agent-memory-mnemosyne)); mental models need Hindsight.
