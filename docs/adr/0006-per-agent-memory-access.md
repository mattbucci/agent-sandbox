# ADR 0006 — per-agent memory access, enforced in the memory service

- **Status:** Accepted
- **Date:** 2026-11-15
- **Deciders:** repo owner (mattbucci)
- **Builds on:** [ADR 0005](0005-hindsight-hierarchical-memory.md)

## Context

ADR 0005 put every agent on one Hindsight bank behind one bearer token. That
token was in every VM and granted the whole REST API: any agent could read
everything, write as anyone, and delete or reconfigure the bank. Which mental
models an agent saw was decided by the agent's own client. The only server-side
limit was the MCP tool list, which the REST API bypassed.

For memory that feeds every agent's prompt, "the agent decides what it may
read and write" is not a boundary.

## Decision

**Each agent gets its own credential, and the memory service enforces a
per-agent policy compiled from the YAML.**

1. **Identity.** `memory.token` becomes the admin credential and stays on the
   host. Each agent's token is derived from it — HMAC over the agent type — so
   nothing is stored or distributed, tokens are unrelated to each other, and
   rotating the master rotates all of them. The policy file holds only digests.

2. **Policy in Hindsight's own terms.** Inside a bank Hindsight scopes data by
   tag, so access is a read list and a write list of tags, with words for the
   common cases (`own`, `shared`, `all`, an agent's name). Per agent: which
   banks, and in each what it may read and write. One read list covers
   memories, observations and mental models.

3. **Enforced inside Hindsight, not in front of it.** Two extensions
   (`lib/hindsight_ext/`) plug into Hindsight's tenant and operation-validator
   hooks: authentication sets the caller's identity, which Hindsight carries
   through REST, MCP and the background tasks a request starts; the validator
   confines reads to a tag scope, tags and bounds writes, hides other banks, and
   reserves everything structural for the admin. A proxy in front was the
   alternative; it would have had to re-implement tag filtering by rewriting
   requests and responses for two protocols, and would not have covered
   Hindsight's own tools (reflect searches memory internally).

4. **Mental models separate "who reads" from "built from".** `visible_to` sets
   the model's tags; `sources` is stated explicitly as the model's trigger scope
   rather than left to follow from its tags.

5. **Defaults preserve ADR 0005's behaviour.** Read everything, write own tag.
   Restriction is opt-in, per agent or globally. What changes for everyone is
   that writes are attributable and nothing destructive is reachable.

6. **Fail closed.** A missing or corrupt policy admits nobody; an unknown
   caller is refused; a bank not in an agent's policy does not exist for it.
   The policy is re-read when it changes, so access changes apply immediately.

## Consequences

- An agent compromise is bounded by that agent's policy: it can read its scope
  and add memories under its own tag. It can still *poison* what it may write —
  facts it retains flow into observations and into any mental model sourced from
  its tag — so `sources` on a shared model is a trust decision.
- A model with `sources: all` shown to an agent with a narrow read scope
  discloses a summary of memories that agent cannot read directly. This is
  intended for shared models and is called out in the docs.
- The extensions depend on Hindsight's extension interfaces (1.1.0). The policy
  logic is separate from the adapter and tested without Hindsight; the adapter
  is small and was verified against the running service. An upgrade must
  re-verify it.
- Identity is the token alone; it is not bound to the VM's address.
- Agents launched before this change hold the master token and must be
  relaunched; the master should be rotated if it was ever exposed to an agent.
- The `hermes` container is still not wired to memory.
