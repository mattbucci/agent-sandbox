# Private configuration

You can clone this repository, define your own agents and settings, and never
change a tracked file. Everything that is yours lives in two places that git
ignores:

| Path | Holds | Version it? |
|---|---|---|
| `config/sandbox.yaml` | This host: LLM endpoint and key, network, gateway block, every token | No — it contains credentials |
| `config/local/` | Your agents, presets, install scripts and shared settings | Yes, in a repository of your own |

`git status` stays clean and `git pull` keeps working, because your changes are
not edits to the published files.

## Set it up

```bash
bin/sandbox-ctl config init                    # sandbox.yaml from the example + an empty config/local/
bin/sandbox-ctl config init <your-git-url>     # ...or config/local/ cloned from your own repo
```

`config init` never overwrites anything. With a URL, `config/local/` is a normal
clone of your repository: commit and push from inside it. It is a separate
repository nested in an ignored directory, so this repository never sees it.

To keep the overlay somewhere else, set `SANDBOX_LOCAL_CONFIG=/path/to/dir` in
the environment of every `sandbox-ctl` call (including the boot service).

## Layout

`config/local/` mirrors `config/`:

```
config/local/
├── agents/<type>.yaml                 your agents, and overrides of published ones
├── presets/egress/<name>.yaml         private presets
├── presets/capabilities/<name>.yaml
├── presets/prompts/<name>.yaml
├── install-scripts/<name>.sh          private install scripts
└── sandbox.yaml                       optional shared settings (no secrets)
```

### Agents

A file whose name is new adds an agent. It is an ordinary agent definition
([creating-agents.md](creating-agents.md)) and can be validated, built and
launched like any other:

```yaml
# config/local/agents/scout.yaml
agent: {type: scout, name: "Scout"}
egress: {presets: [github]}
capabilities: {presets: [python-dev]}
prompt:
  role: You survey a repository and report what you find.
  presets: [code-execution]
```

A file with the name of a published agent customises that agent. It is laid
over the published definition, so it only needs the parts you change:

```yaml
# config/local/agents/coder.yaml — keep everything, change the model
vm:
  model: my-code-model
  context_window: 131072
```

Mappings merge key by key. Lists and scalars are replaced whole: to drop one
egress preset, give the full list you want. Upstream changes to the rest of the
agent keep reaching you.

`sandbox-ctl list-agents` marks what comes from the overlay with `[local]` or
`[local override]`.

### Presets and install scripts

Agents refer to presets and install scripts by name. A name is looked up in
`config/local/` first, then in `config/`, so a local file adds a preset or
replaces a published one of the same name (whole file, no merging).
`sandbox-ctl config list-presets` marks local ones.

### Shared settings

`config/local/sandbox.yaml` is optional. It takes the same keys as
`config/sandbox.yaml` and is for the settings you want under version control:
the default model and its limits, the memory bank, its mission and mental
models, per-agent memory access. The host's `config/sandbox.yaml` is laid over
it with the same merge rule and wins where both set a key.

Keep credentials out of it: API keys, `gateway.tokens`, `memory.token` and the
`gateway.agents` keys belong in `config/sandbox.yaml`.

## Exposing a private agent through the gateway

Add it under `gateway.agents` in `config/sandbox.yaml`, then
`sandbox-ctl gateway compile` and restart the gateway
([hermes-gateway.md](hermes-gateway.md)). Its memory access comes from
`memory.access` in the agent file or the site config ([memory.md](memory.md)).

## What applies when

Compiled output in `build/` is refreshed when an agent file, a preset, an
install script or the site config is newer than it — published or local. An
image only changes when it is rebuilt (`build-agent`), and a running VM when it
is relaunched.
