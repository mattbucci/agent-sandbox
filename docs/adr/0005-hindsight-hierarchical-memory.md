# ADR 0005 — Hindsight as the agent memory backend: facts, observations, mental models

- **Status:** Accepted
- **Date:** 2026-11-15
- **Deciders:** repo owner (mattbucci)
- **Supersedes in part:** [ADR 0002](0002-agent-memory-hosted-surface.md) (memory stays a
  hosted surface; the backend and how agents use it change)

## Context

ADR 0002 made durable memory part of the hosting layer and backed it with
mnemosyne, a flat store exposed as MCP tools. Two things were wrong with where
that had ended up:

- **Agents rarely used it.** Memory was only a set of tools; whether anything
  was stored or recalled depended on the model choosing to call them. On the
  `deepagents` images the MCP client library was not even installed, so those
  agents had no memory tools at all.
- **It was flat.** A pile of stored snippets has no notion of what has been
  established versus mentioned once, and nothing an agent can load to start a
  session already knowing what matters.

We wanted a hierarchy — curated top-level understanding, settled beliefs under
it, raw facts at the bottom — and had to decide whether to build that on top of
mnemosyne or adopt a system that has it.

## Decision

**Adopt [Hindsight](https://github.com/vectorize-io/hindsight) as
`memory.backend: hindsight`, and move memory use from the model's discretion
into the harness.**

1. **Adopt rather than extend.** mnemosyne is a third-party package; extending
   it would mean writing fact extraction, consolidation and continuously
   refreshed summaries ourselves. Hindsight provides exactly that hierarchy
   (facts → observations → mental models), runs from a pip install with an
   embedded PostgreSQL, takes any OpenAI-compatible LLM (ours is the LAN
   router), embeds and reranks locally, and is MIT licensed. Verified on the
   sandbox host against a local model before committing to it.

2. **The harness recalls and retains; tools are the extra.** The in-VM server
   puts the agent's mental models and the memories relevant to the message in
   front of every turn, and queues the exchange for retention afterwards — for
   `deepagents` and `omp` alike, with a stdlib client and no new image
   dependencies. Explicit recall/retain/reflect tools remain for deliberate use.

3. **One bank, tagged by agent.** Agents share a bank so knowledge crosses
   between them; each write is tagged `agent:<type>` so models and recall can be
   scoped. Isolation is a per-agent option, not the default.

4. **Mental models are configuration.** Standing questions are declared in
   `sandbox.yaml` (shared) and the agent YAMLs (per agent) and synced with
   `sandbox-ctl memory apply`; ids are stable, applying is idempotent, nothing
   is deleted implicitly.

5. **Memory is never on the critical path.** Reads are time-bounded and fail
   open; writes are asynchronous on both sides, because extraction with a local
   model takes minutes.

6. **mnemosyne stays as a backend.** `backend` absent means mnemosyne, so
   existing configs are untouched. No data is migrated.

## Consequences

- Every turn costs one recall and (per session or per turn) the mental models
  in input tokens, and every retained turn costs background LLM work. With a
  small-context model the injected block competes with the task for the window;
  budgets are configurable (`MEMORY_RECALL_MAX_TOKENS`, `MEMORY_MODELS_MAX_CHARS`).
- The memory service needs a served LLM. If the router is down, turns still run
  but nothing new is learned until retention operations are retried.
- Recalled text reaches the model's context, so memory is a persistence path for
  injected instructions across agents and sessions. It is framed as background
  knowledge, the MCP tool list excludes destructive operations, and only turns
  and explicit retains are stored — but the shared token gives any agent full
  REST access to the bank. Per-agent credentials would need Hindsight's tenant
  extensions and are not done here.
- A new several-GB dependency on the host (PyTorch and local models), pinned by
  version with its resolution date frozen, and ~2 GB of RAM.
- The real `hermes` container is not wired to Hindsight and runs without shared
  memory under this backend.
- Agent images must be rebuilt to pick up the runtime that uses it.
