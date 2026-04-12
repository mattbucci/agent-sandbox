# Creating Custom Agents

This guide walks through defining a new agent from scratch using the YAML configuration system.

## Quick Start: Minimal Agent

The simplest possible agent definition:

```yaml
# config/agents/my-agent.yaml
agent:
  type: my-agent
  name: "My Custom Agent"

egress:
  presets: [google]

capabilities:
  presets: [python-dev]

prompt:
  role: |
    You are a helpful assistant that can write and run Python code.
  presets: [code-execution]
```

Build and launch:

```bash
sandbox-ctl config validate my-agent    # check YAML is valid
sudo sandbox-ctl build-agent my-agent   # build rootfs
sudo sandbox-ctl launch my-agent        # launch VM
```

## Full Agent Example

```yaml
# config/agents/qa-tester.yaml
agent:
  type: qa-tester
  name: "QA Test Engineer"

vm:
  vcpus: 2
  mem_mb: 4096
  model: deepseek-coder-v2       # override default LLM model

# Which domains can this agent reach?
egress:
  presets:
    - github                     # .github.com, .githubusercontent.com
    - npm                        # .npmjs.org, registry.npmjs.org
    - pypi                       # .pypi.org, files.pythonhosted.org
    - google                     # .google.com, .googleapis.com
  domains:                       # additional domains not in any preset
    - .cypress.io
    - .playwright.dev

# What tools/packages does this agent need?
capabilities:
  presets:
    - github-cli                 # installs gh CLI
    - python-dev                 # pytest, black, ruff, mypy
    - node-dev                   # typescript, eslint, prettier
  packages:                      # additional packages
    apt:
      - chromium-driver
    pip:
      - playwright
      - selenium
    npm:
      - cypress
  install_scripts: []            # any custom install scripts from config/install-scripts/

# Declare tools for documentation + validation
tools:
  - name: pytest
    description: "Python test framework"
  - name: cypress
    description: "End-to-end testing framework"
  - name: playwright
    description: "Browser automation for testing"
  - name: gh
    description: "GitHub CLI"
  - name: git
    from: base
  - name: chromium
    from: base
  - name: agent-browser
    from: base

# System prompt assembled from role + presets + inline sections
prompt:
  role: |
    You are a QA Test Engineer agent. Your job is to write and run
    automated tests for web applications. You clone repositories,
    understand the existing test suite, identify gaps in coverage,
    and write new tests using pytest, Cypress, or Playwright
    depending on what the project uses.
  presets:
    - github-pr-workflow         # how to work with GitHub PRs
    - git-workflow               # git branching and commit rules
    - code-quality               # linting and style rules
    - code-execution             # bash vs python execution rules
    - browser-instructions       # how to use agent-browser
    - report-output              # how to write reports
  sections:
    testing-strategy: |
      ## Testing Strategy
      1. Read the project's existing test setup (package.json, pytest.ini, etc.)
      2. Identify the test framework in use
      3. Run the existing test suite to establish a baseline
      4. Analyze code coverage to find untested paths
      5. Write tests for uncovered code, focusing on:
         - Happy path behavior
         - Edge cases and error handling
         - Integration points between modules
      6. Run the full suite to verify no regressions
    output_dir: test-reports
```

## Available Presets

List all presets:

```bash
sandbox-ctl config list-presets              # all categories
sandbox-ctl config list-presets egress       # just egress
sandbox-ctl config list-presets capabilities # just capabilities
sandbox-ctl config list-presets prompts      # just prompts
```

See [presets-reference.md](presets-reference.md) for the full reference.

## Egress Presets

Control which domains the agent can reach. Each preset defines a group of related domains:

| Preset | Domains |
|--------|---------|
| `github` | .github.com, .githubusercontent.com |
| `google` | .google.com, .googleapis.com |
| `npm` | .npmjs.org, registry.npmjs.org |
| `pypi` | .pypi.org, files.pythonhosted.org |
| `stackoverflow` | .stackoverflow.com, .stackexchange.com |
| `docker-hub` | .docker.io, registry-1.docker.io |
| `hashicorp` | registry.terraform.io, releases.hashicorp.com |
| `kubernetes` | .kubernetes.io, pkgs.k8s.io |
| `cloud-providers` | .amazonaws.com, .cloud.google.com, .azure.com |
| `security-databases` | nvd.nist.gov, .cve.org, osv.dev |
| `research-sites` | news.ycombinator.com, .arxiv.org, .reddit.com |

Add domains not covered by presets with `egress.domains`.

The LiteLLM server is always allowed (configured in `sandbox.yaml`).

## Capability Presets

Install tools and packages into the agent's VM:

| Preset | Installs |
|--------|----------|
| `python-dev` | pytest, black, ruff, mypy, rich |
| `node-dev` | typescript, eslint, prettier |
| `github-cli` | gh (GitHub CLI) |
| `docker` | docker.io |
| `debugging` | gdb, strace, ltrace, valgrind, ipdb |
| `kubernetes` | kubectl, helm |
| `security-scanning` | trivy, grype, semgrep, nmap, nikto, pip-audit |
| `research-tools` | readability-cli, pandoc, lynx, feedparser |

Add extra packages with `capabilities.packages.{apt,pip,npm}`.

## Prompt Presets (Rulebooks)

Prompt presets use a **rulebook format** — each rule has an ID, a clear statement, and examples showing correct and incorrect behavior:

| Preset | Rules |
|--------|-------|
| `git-workflow` | GIT-001 to GIT-004: branching, commits, blame |
| `github-pr-workflow` | PR-001 to PR-005: issue-driven PRs |
| `debugging-workflow` | DBG-001 to DBG-004: reproduce → diagnose → report |
| `code-quality` | CQ-001 to CQ-004: style, linting, tests |
| `code-execution` | EXEC-001 to EXEC-004: bash vs python, uv |
| `browser-instructions` | BROWSER-001 to BROWSER-004: agent-browser CLI |
| `report-output` | RPT-001 to RPT-003: structured markdown reports |
| `security-scan-workflow` | SEC-001 to SEC-005: scan → audit → report |
| `research-sweep` | RES-001 to RES-004: multi-source research |

Agents reference rule IDs in their reasoning (e.g., "Per [GIT-002], here's why I made this change..."), making behavior auditable in traces.

## Creating Custom Presets

Create a new file in the appropriate `config/presets/` subdirectory:

### Custom Egress Preset

```yaml
# config/presets/egress/my-service.yaml
name: my-service
description: "Internal services"
domains:
  - api.internal.example.com
  - dashboard.example.com
```

### Custom Capability Preset

```yaml
# config/presets/capabilities/ml-tools.yaml
name: ml-tools
description: "Machine learning development tools"
packages:
  pip:
    - torch
    - transformers
    - scikit-learn
    - pandas
    - matplotlib
provides_tools:
  - name: python3
    description: "Python with ML libraries (torch, transformers, sklearn)"
```

### Custom Prompt Preset

```yaml
# config/presets/prompts/incident-response.yaml
name: incident-response
description: "Incident response workflow"
rules:
  - id: IR-001
    rule: "Assess severity before taking any action"
    example: |
      # GOOD [IR-001]:
      # 1. Check error rate: curl metrics endpoint
      # 2. Check affected users: query logs
      # 3. Classify: P1 (>10% users), P2 (>1%), P3 (<1%)
      # Then proceed based on severity.

      # BAD [IR-001]:
      # Immediately rolling back without understanding impact
  - id: IR-002
    rule: "Document timeline as you go, not after the fact"
    example: |
      # GOOD [IR-002]:
      echo "$(date -Iseconds) Identified root cause: OOM in worker pods" >> /home/agent/workspace/incident.md

      # BAD [IR-002]:
      # Fixing everything first, then trying to reconstruct what happened
```

## How It Works

When you run `sandbox-ctl build-agent <type>`, the system:

1. **Compiles** the YAML → flat files via `agentconf.py`:
   - `agent.conf` — bash key=value for scripts
   - `allowlist.txt` — merged domain list from all egress presets
   - `system-prompt.md` — assembled role + tools + rulebook sections
   - `customize.sh` — package installs from capability presets
   - `tools.json` — declared tools for validation

2. **Builds** the rootfs by cloning the base image and running `customize.sh` in chroot

3. At **launch**, the compiled prompt and config are injected into the VM instance

You can inspect compiled output anytime:

```bash
sandbox-ctl config compile debugger
cat build/debugger/system-prompt.md
cat build/debugger/allowlist.txt
cat build/debugger/tools.json
```
