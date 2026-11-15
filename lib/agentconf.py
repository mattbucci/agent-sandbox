#!/usr/bin/env python3
"""
agentconf.py — YAML config compiler for agent-sandbox

Reads agent YAML definitions + preset modules, compiles them into flat
files that the bash scripts consume:

  agentconf.py compile <type>      → build/<type>/{agent.conf, allowlist.txt,
                                      system-prompt.md, customize.sh, tools.json}
  agentconf.py compile-global      → build/sandbox.conf
  agentconf.py compile-gateway     → state/gateway/gateway.json
  agentconf.py compile-all         → compile global + gateway + all agents
  agentconf.py validate <type>     → validate agent YAML + preset references
  agentconf.py memory-apply [url]  → write the per-agent access policy and sync
                                      banks + mental models to Hindsight
                                      (--dry-run prints the plan)
  agentconf.py memory-access [type]→ who may read/write what; models each sees
  agentconf.py list                → list agent types
  agentconf.py list-presets [cat]  → list presets (egress|capabilities|prompts)
  agentconf.py docs                → generate docs/presets-reference.md
"""

import json
import os
import sys
from pathlib import Path

try:
    import yaml
except ImportError:
    print("ERROR: PyYAML required. Install with: pip3 install pyyaml", file=sys.stderr)
    sys.exit(1)

SANDBOX_ROOT = Path(__file__).resolve().parent.parent
CONFIG_DIR = SANDBOX_ROOT / "config"
PRESETS_DIR = CONFIG_DIR / "presets"
AGENTS_DIR = CONFIG_DIR / "agents"
INSTALL_SCRIPTS_DIR = CONFIG_DIR / "install-scripts"
BUILD_DIR = SANDBOX_ROOT / "build"
# In-VM backends (dispatched by rootfs/overlay/opt/agent/start.sh) and the
# capability preset each one needs baked into the image.
HARNESSES = ("deepagents", "omp", "hermes")
HARNESS_CAPABILITY = {"omp": "omp", "hermes": "docker"}

VERSIONS_ENV = CONFIG_DIR / "versions.env"
VERSIONS_LIB = SANDBOX_ROOT / "lib" / "versions.sh"


def customize_prelude() -> list:
    """Pinned versions + the verified-download helper for the chroot.

    customize.sh runs inside the rootfs chroot, where the checkout is not
    visible, so the KEY="value" pins from config/versions.env and
    fetch_verified/pinned_sha from lib/versions.sh are inlined verbatim.
    """
    lines = ["# --- Pinned versions (config/versions.env) ---"]
    for raw in VERSIONS_ENV.read_text().splitlines():
        line = raw.strip()
        if line and not line.startswith("#"):
            lines.append(line)
    lines.append("")
    lines.append("# --- Verified download helpers (lib/versions.sh) ---")
    helpers = VERSIONS_LIB.read_text()
    lines.append(helpers[helpers.index("# fetch_verified <url>"):].rstrip())
    lines.append("")
    return lines


def load_yaml(path: Path) -> dict:
    with open(path) as f:
        return yaml.safe_load(f) or {}


def load_preset(category: str, name: str) -> dict:
    path = PRESETS_DIR / category / f"{name}.yaml"
    if not path.exists():
        print(f"ERROR: Preset not found: {category}/{name} ({path})", file=sys.stderr)
        sys.exit(1)
    return load_yaml(path)


def load_agent(agent_type: str) -> dict:
    path = AGENTS_DIR / f"{agent_type}.yaml"
    if not path.exists():
        print(f"ERROR: Agent not found: {agent_type} ({path})", file=sys.stderr)
        sys.exit(1)
    return load_yaml(path)


# ---------------------------------------------------------------------------
# Memory (config `memory:` block)
# ---------------------------------------------------------------------------
MEMORY_BACKENDS = ("hindsight", "mnemosyne")
# Hindsight's own default (8888) is taken on the host by the OTel collector's
# telemetry endpoint, so the sandbox uses 8890.
MEMORY_DEFAULT_PORT = {"hindsight": 8890, "mnemosyne": 8077}


def memory_settings(global_config: dict) -> dict:
    """Normalize the `memory:` block.

    `backend` selects the service: "hindsight" (hierarchical memory: facts ->
    observations -> mental models) or "mnemosyne" (the earlier flat MCP store,
    and the default when `backend` is absent so existing configs keep working).
    """
    memory = global_config.get("memory", {}) or {}
    backend = (memory.get("backend") or "mnemosyne").strip()
    return {
        "enabled": bool(memory.get("enabled", False)),
        "backend": backend,
        "port": memory.get("port") or MEMORY_DEFAULT_PORT.get(backend, 8890),
        "token": memory.get("token", "") or "",
        "bank": memory.get("bank") or "agent-sandbox",
        "mission": (memory.get("mission") or "").strip(),
        "recall_scope": memory.get("recall_scope") or "shared",
        "llm_model": memory.get("llm_model") or (global_config.get("llm", {}) or {}).get("model", ""),
        "max_completion_tokens": memory.get("max_completion_tokens") or 16000,
        "embeddings": memory.get("embeddings", "fastembed") or "fastembed",
        "mental_models": memory.get("mental_models") or [],
    }


def _slug(text: str) -> str:
    out = "".join(c.lower() if c.isalnum() else "-" for c in str(text))
    return "-".join(part for part in out.split("-") if part)


# ---------------------------------------------------------------------------
# Memory access: who may read and write what
#
# Inside a bank, Hindsight scopes data by tag. Everything an agent writes is
# tagged agent:<type>; untagged data is the shared scope. Access is expressed in
# those terms, with a few words for the common cases:
#
#     own            the agent's own tag (agent:<type>)
#     shared         the untagged, shared scope            (read only)
#     all            everything in the bank                (read only)
#     <agent type>   that agent's tag, e.g. `debugger`
#     <tag>          any other tag verbatim, e.g. `team:platform`
#
# The same read list governs memories, observations and mental models: an agent
# sees a mental model exactly when it may read one of the model's tags (or the
# model is shared and it may read `shared`).
# ---------------------------------------------------------------------------
DEFAULT_ACCESS = {"read": ["all"], "write": ["own"]}
# Tools an agent may be offered over MCP; none of them deletes or reconfigures.
AGENT_MCP_TOOLS = [
    "retain", "recall", "reflect", "list_mental_models", "get_mental_model",
    "list_memories", "get_memory", "list_tags",
    "search_knowledge_base", "get_knowledge_page", "get_knowledge_base_tree",
]


def _agent_tag(agent_type: str) -> str:
    return f"agent:{agent_type}"


def _resolve_tag(word, agent_type, agent_types):
    """One access word -> a concrete tag ('own'/agent names/verbatim tags)."""
    word = str(word).strip()
    if word == "own":
        return _agent_tag(agent_type)
    if word in agent_types:
        return _agent_tag(word)
    return word


def resolve_access(spec, agent_type, agent_types, errors=None, where=""):
    """An access spec ({read: [...], write: [...]}) -> a policy bank entry."""
    errors = errors if errors is not None else []
    spec = spec or {}
    read_words = spec.get("read", DEFAULT_ACCESS["read"])
    write_words = spec.get("write", DEFAULT_ACCESS["write"])
    for name, words in (("read", read_words), ("write", write_words)):
        if not isinstance(words, list):
            errors.append(f"{where}: memory access '{name}' must be a list")
            return None
    read_all = "all" in read_words
    read_untagged = read_all or "shared" in read_words
    read = sorted({_resolve_tag(w, agent_type, agent_types)
                   for w in read_words if w not in ("all", "shared")})
    for w in write_words:
        if w in ("all", "shared"):
            errors.append(f"{where}: agents cannot be given write access to '{w}' "
                          "(the shared scope and whole-bank writes are admin-only)")
            return None
    write = sorted({_resolve_tag(w, agent_type, agent_types) for w in write_words})
    own = _agent_tag(agent_type)
    return {
        "read": [] if read_all else read,
        "read_untagged": read_untagged,
        "read_all": read_all,
        "write": write,
        # Untagged writes get the agent's own tag when it may write it, so an
        # agent's memories are always attributable and never land in `shared`.
        "default_tags": [own] if own in write else ([write[0]] if write and "*" not in write[0] else []),
    }


def _agent_memory_access(global_config, agent_type, agent, agent_types, errors):
    """{bank_id: policy entry} for one agent, or {} when it has no memory."""
    settings = memory_settings(global_config)
    agent_memory = agent.get("memory", {}) or {}
    if not settings["enabled"] or not agent_memory.get("enabled", True):
        return {}
    where = f"agent '{agent_type}'"
    banks = {}

    # Primary bank: the agent's own `access`, else the global default, else
    # the built-in default. `recall_scope: agent` is the older spelling of
    # read: [own, shared].
    spec = agent_memory.get("access")
    if spec is None:
        spec = (global_config.get("memory", {}) or {}).get("access")
    if spec is None:
        scope = agent_memory.get("recall_scope") or settings["recall_scope"]
        spec = {"read": ["own", "shared"], "write": ["own"]} if scope == "agent" else DEFAULT_ACCESS
    primary = agent_memory.get("bank") or settings["bank"]
    entry = resolve_access(spec, agent_type, agent_types, errors, where)
    if entry:
        banks[primary] = entry

    # Additional banks the agent is granted.
    for bank_id, bank_spec in (agent_memory.get("banks") or {}).items():
        if bank_id == primary:
            errors.append(f"{where}: bank '{bank_id}' is its primary bank; use memory.access")
            continue
        entry = resolve_access(bank_spec if bank_spec is not None else {"read": ["all"], "write": []},
                               agent_type, agent_types, errors, f"{where} bank '{bank_id}'")
        if entry:
            banks[bank_id] = entry
    return banks


def _policy_module():
    sys.path.insert(0, str(SANDBOX_ROOT / "lib" / "hindsight_ext"))
    import memory_policy
    return memory_policy


def memory_policy_doc(global_config: dict, agents: dict) -> dict:
    """The access policy the Hindsight extension enforces (see memory_policy.py).

    Tokens are stored as SHA-256 digests only. The admin principal is the master
    `memory.token`; each agent's token is derived from it (agent_token()).
    """
    mp = _policy_module()
    settings = memory_settings(global_config)
    errors = []
    agent_types = set(agents)
    principals = {mp.ADMIN: {"admin": True, "token_sha256": mp.token_digest(settings["token"])}}
    for agent_type in sorted(agents):
        banks = _agent_memory_access(global_config, agent_type, agents[agent_type], agent_types, errors)
        if not banks:
            continue
        principals[agent_type] = {
            "token_sha256": mp.token_digest(mp.agent_token(settings["token"], agent_type)),
            "banks": banks,
            "mcp_tools": AGENT_MCP_TOOLS,
        }
    return {"version": 1, "principals": principals, "errors": errors}


def _model_scope(words, default_agent, agent_types, errors, where, field):
    """`visible_to` / `sources` -> None for everyone, else a list of tags."""
    if words is None:
        words = [default_agent] if default_agent else "all"
    if words == "all" or words == ["all"]:
        return None
    if not isinstance(words, list) or not words:
        errors.append(f"{where}: '{field}' must be 'all' or a non-empty list of agents/tags")
        return None
    return sorted({_resolve_tag(w, default_agent or "", agent_types) for w in words})


def memory_plan(global_config: dict, agents: dict) -> dict:
    """The declared top of the memory hierarchy: banks and their mental models.

    A mental model is a standing question Hindsight keeps answered. Two things
    are set per model:

      visible_to  who may read it — 'all' (untagged: the shared scope) or a list
                  of agents/tags (the model carries those tags, so an agent sees
                  it when its read access covers one of them)
      sources     whose memories it is written from — 'all' (the whole bank) or
                  a list of agents/tags

    Models under `memory.mental_models` in sandbox.yaml default to all/all;
    ones in an agent's YAML default to that agent for both. Additional banks and
    their models are declared under `memory.banks`.
    """
    settings = memory_settings(global_config)
    memory_cfg = global_config.get("memory", {}) or {}
    agent_types = set(agents)
    models, errors, seen = [], [], set()
    banks = {settings["bank"]: {"id": settings["bank"], "mission": settings["mission"]}}
    for bank_id, bank_cfg in (memory_cfg.get("banks") or {}).items():
        banks[bank_id] = {"id": bank_id, "mission": " ".join(str((bank_cfg or {}).get("mission") or "").split())}

    def add(spec, bank_id, agent_type=None):
        where = f"agent '{agent_type}'" if agent_type else f"bank '{bank_id}'"
        if not isinstance(spec, dict) or not spec.get("name") or not spec.get("query"):
            errors.append(f"{where}: each memory.mental_models entry needs 'name' and 'query'")
            return
        base = _slug(spec.get("id") or spec["name"])
        model_id = f"{agent_type}-{base}" if agent_type else base
        if (bank_id, model_id) in seen:
            errors.append(f"{where}: duplicate mental model id '{model_id}'")
            return
        seen.add((bank_id, model_id))
        where = f"{where} mental model '{model_id}'"
        visible = _model_scope(spec.get("visible_to"), agent_type, agent_types, errors, where, "visible_to")
        sources = _model_scope(spec.get("sources"), agent_type, agent_types, errors, where, "sources")
        # What it is written from, stated explicitly: left to itself Hindsight
        # derives this from the model's own tags, which would tie who may read a
        # model to what it is built from. `any_strict` = carries one of the
        # tags; the NOT of an impossible tag = every memory in the bank.
        if sources is None:
            tag_groups = [{"not": {"tags": ["__no_such_tag__"], "match": "any_strict"}}]
        else:
            tag_groups = [{"tags": sources, "match": "any_strict"}]
        models.append({
            "bank": bank_id,
            "id": model_id,
            "name": spec["name"],
            "source_query": " ".join(str(spec["query"]).split()),
            "tags": visible or [],
            "max_tokens": int(spec.get("max_tokens") or 2048),
            "trigger": {"refresh_after_consolidation": True, "tag_groups": tag_groups},
        })

    for spec in settings["mental_models"]:
        add(spec, settings["bank"])
    for bank_id, bank_cfg in (memory_cfg.get("banks") or {}).items():
        for spec in (bank_cfg or {}).get("mental_models") or []:
            add(spec, bank_id)
    for agent_type in sorted(agents):
        agent_memory = agents[agent_type].get("memory", {}) or {}
        bank_id = agent_memory.get("bank") or settings["bank"]
        banks.setdefault(bank_id, {"id": bank_id, "mission": ""})
        for spec in agent_memory.get("mental_models") or []:
            add(spec, bank_id, agent_type)
        for extra in agent_memory.get("banks") or {}:
            if extra not in banks:
                errors.append(f"agent '{agent_type}': bank '{extra}' is not declared under memory.banks in sandbox.yaml")

    policy = memory_policy_doc(global_config, agents)
    errors.extend(policy.pop("errors"))
    return {
        "backend": settings["backend"],
        "bank": banks[settings["bank"]],
        "banks": list(banks.values()),
        "mental_models": models,
        "policy": policy,
        "errors": errors,
    }


def _all_agents() -> dict:
    return {p.stem: load_yaml(p) for p in sorted(AGENTS_DIR.glob("*.yaml"))}


def _hindsight_call(base_url: str, token: str, method: str, path: str, body=None):
    """One Hindsight REST call. Returns (status, json-or-None)."""
    import urllib.error
    import urllib.request

    data = None if body is None else json.dumps(body).encode()
    headers = {"Accept": "application/json"}
    if data is not None:
        headers["Content-Type"] = "application/json"
    if token:
        headers["Authorization"] = f"Bearer {token}"
    req = urllib.request.Request(base_url + path, data=data, method=method, headers=headers)
    try:
        with urllib.request.urlopen(req, timeout=30) as resp:
            raw = resp.read()
            return resp.status, (json.loads(raw) if raw else None)
    except urllib.error.HTTPError as e:
        raw = e.read()
        try:
            return e.code, json.loads(raw)
        except ValueError:
            return e.code, {"detail": raw.decode("utf-8", "replace")[:300]}


def memory_policy_path() -> Path:
    return SANDBOX_ROOT / "state" / "hindsight" / "policy.json"


def write_memory_policy(plan: dict) -> Path:
    """Write the access policy the running service enforces (owner-only).

    The service re-reads it when it changes, so this takes effect on the next
    request without a restart.
    """
    path = memory_policy_path()
    path.parent.mkdir(parents=True, exist_ok=True)
    tmp = path.with_suffix(".json.tmp")
    fd = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(fd, "w") as f:
        json.dump(plan["policy"], f, indent=2, sort_keys=True)
        f.write("\n")
    os.replace(tmp, path)
    return path


def _describe_access(entry: dict) -> str:
    if entry["read_all"]:
        read = "everything"
    else:
        parts = list(entry["read"]) + (["shared"] if entry["read_untagged"] else [])
        read = ", ".join(parts) or "nothing"
    return f"read: {read:<44} write: {', '.join(entry['write']) or 'nothing'}"


def cmd_memory_access(agent_type: str = ""):
    """Print who may read and write what, and which mental models each agent sees."""
    config = load_global_config()
    plan = memory_plan(config, _all_agents())
    for e in plan["errors"]:
        print(f"ERROR: {e}", file=sys.stderr)
    if plan["errors"]:
        return False
    principals = plan["policy"]["principals"]
    for name in sorted(principals):
        if name == "admin" or (agent_type and name != agent_type):
            continue
        print(f"{name}")
        for bank_id, entry in sorted(principals[name]["banks"].items()):
            print(f"  bank {bank_id:<18} {_describe_access(entry)}")
            visible = []
            for m in plan["mental_models"]:
                if m["bank"] != bank_id:
                    continue
                if entry["read_all"] or (not m["tags"] and entry["read_untagged"]) \
                        or any(t in entry["read"] for t in m["tags"]):
                    visible.append(m["id"])
            print(f"    mental models: {', '.join(visible) or '(none)'}")
    if agent_type and agent_type not in principals:
        print(f"{agent_type}: no memory access (memory disabled for this agent, or unknown agent)")
    return True


def _norm_tag_groups(value):
    """Tag groups without the defaults Hindsight fills in, for comparison."""
    if isinstance(value, list):
        return [_norm_tag_groups(v) for v in value]
    if isinstance(value, dict):
        return {k: _norm_tag_groups(v) for k, v in value.items()
                if not (k == "resolve" and v == "exact")}
    return value


def cmd_memory_apply(base_url: str = "", dry_run: bool = False):
    """Write the access policy, then create/update banks and mental models.

    Idempotent: models are addressed by a stable id derived from the config, so
    re-applying only changes what the config changed. Models that exist in a
    bank but are no longer declared are reported, never deleted.
    """
    config = load_global_config()
    settings = memory_settings(config)
    plan = memory_plan(config, _all_agents())
    if plan["errors"]:
        for e in plan["errors"]:
            print(f"ERROR: {e}", file=sys.stderr)
        return False
    if settings["backend"] != "hindsight":
        print(f"memory.backend is '{settings['backend']}': nothing to apply (mental models need hindsight).")
        return True

    for bank in plan["banks"]:
        print(f"Bank: {bank['id']}")
        for m in plan["mental_models"]:
            if m["bank"] != bank["id"]:
                continue
            visible = ", ".join(m["tags"]) or "all"
            groups = m["trigger"]["tag_groups"][0]
            sources = ", ".join(groups["tags"]) if "tags" in groups else "all"
            print(f"  mental model {m['id']:<28} visible to: {visible:<28} built from: {sources}")
    agents_with_access = sorted(n for n in plan["policy"]["principals"] if n != "admin")
    print(f"Access policy: {len(agents_with_access)} agent(s) — see 'sandbox-ctl memory access'")
    if dry_run:
        return True

    try:
        policy_path = write_memory_policy(plan)
    except OSError as e:
        print(f"ERROR: could not write the access policy: {e}", file=sys.stderr)
        return False
    print(f"Policy written: {policy_path}")

    base_url = (base_url or f"http://127.0.0.1:{settings['port']}").rstrip("/")
    token = settings["token"]  # the admin principal

    def call(method, path, body=None):
        status, payload = _hindsight_call(base_url, token, method, path, body)
        if status >= 400:
            raise RuntimeError(f"{method} {path}: HTTP {status} {payload}")
        return payload

    created = updated = unchanged = 0
    extra = []
    try:
        for bank in plan["banks"]:
            bank_path = f"/v1/default/banks/{bank['id']}"
            bank_body = {"name": bank["id"]}
            if bank["mission"]:
                bank_body["mission"] = bank["mission"]
            call("PUT", bank_path, bank_body)

            existing = call("GET", f"{bank_path}/mental-models?detail=content&limit=100") or {}
            items = existing.get("items") if isinstance(existing, dict) else existing
            by_id = {m["id"]: m for m in items or []}

            declared = set()
            for m in plan["mental_models"]:
                if m["bank"] != bank["id"]:
                    continue
                declared.add(m["id"])
                body = {k: m[k] for k in ("name", "source_query", "tags", "max_tokens", "trigger")}
                current = by_id.get(m["id"])
                if current is None:
                    call("POST", f"{bank_path}/mental-models", {"id": m["id"], **body})
                    created += 1
                    continue
                cur_trigger = current.get("trigger") or {}
                same = (
                    current.get("name") == m["name"]
                    and current.get("source_query") == m["source_query"]
                    and sorted(current.get("tags") or []) == sorted(m["tags"])
                    and current.get("max_tokens") == m["max_tokens"]
                    and _norm_tag_groups(cur_trigger.get("tag_groups")) == m["trigger"]["tag_groups"]
                    and bool(cur_trigger.get("refresh_after_consolidation")) is True
                )
                if same:
                    unchanged += 1
                else:
                    call("PATCH", f"{bank_path}/mental-models/{m['id']}", body)
                    updated += 1
            extra.extend(f"{bank['id']}/{i}" for i in sorted(by_id) if i not in declared)
    except (RuntimeError, OSError) as e:
        print(f"ERROR: could not apply memory plan to {base_url}: {e}", file=sys.stderr)
        return False

    print(f"Applied: {created} created, {updated} updated, {unchanged} unchanged.")
    if extra:
        print("Not declared in config (left in place): " + ", ".join(extra))
    return True


def load_global_config() -> dict:
    path = CONFIG_DIR / "sandbox.yaml"
    if not path.exists():
        print(f"ERROR: Global config not found: {path}", file=sys.stderr)
        sys.exit(1)
    return load_yaml(path)


# ---------------------------------------------------------------------------
# Compile: Agent
# ---------------------------------------------------------------------------

def compile_allowlist(agent: dict) -> str:
    """Merge egress presets + inline domains into a flat domain list."""
    domains = set()
    egress = agent.get("egress", {})

    for preset_name in egress.get("presets", []):
        preset = load_preset("egress", preset_name)
        for d in preset.get("domains", []):
            domains.add(d)

    for d in egress.get("domains", []):
        domains.add(d)

    lines = [f"# Allowed domains for {agent['agent']['type']}"]
    lines += sorted(domains)
    return "\n".join(lines) + "\n"


def compile_customize_sh(agent: dict) -> str:
    """Generate a customize.sh from capability presets + inline packages."""
    apt_packages = set()
    pip_packages = set()
    npm_packages = set()
    install_scripts = []

    caps = agent.get("capabilities", {})

    # Collect from presets
    for preset_name in caps.get("presets", []):
        preset = load_preset("capabilities", preset_name)
        pkgs = preset.get("packages", {})
        apt_packages.update(pkgs.get("apt", []))
        pip_packages.update(pkgs.get("pip", []))
        npm_packages.update(pkgs.get("npm", []))
        is_field = preset.get("install_script", [])
        if isinstance(is_field, str):
            install_scripts.append(is_field)
        elif isinstance(is_field, list):
            install_scripts.extend(is_field)
        for s in preset.get("install_scripts", []):
            install_scripts.append(s)

    # Collect inline
    inline_pkgs = caps.get("packages", {})
    apt_packages.update(inline_pkgs.get("apt", []))
    pip_packages.update(inline_pkgs.get("pip", []))
    npm_packages.update(inline_pkgs.get("npm", []))
    for s in caps.get("install_scripts", []):
        install_scripts.append(s)

    # Build script
    lines = [
        "#!/usr/bin/env bash",
        f"# Auto-generated customize.sh for {agent['agent']['type']}",
        "# Do not edit — regenerate with: agentconf.py compile",
        "set -euo pipefail",
        "export DEBIAN_FRONTEND=noninteractive",
        "",
    ]
    lines += customize_prelude()

    if apt_packages:
        lines.append("echo 'Installing apt packages...'")
        lines.append("apt-get update -qq")
        lines.append("apt-get install -y --no-install-recommends \\")
        lines.append("    " + " \\\n    ".join(sorted(apt_packages)))
        lines.append("")

    if pip_packages:
        # The base image ships uv (no system pip/pip3), so install capability
        # pip packages into the system python via uv. --system targets the
        # system environment; --break-system-packages tolerates Ubuntu's
        # PEP668 marker in the chroot.
        lines.append("echo 'Installing pip packages...'")
        lines.append("uv pip install --system --break-system-packages \\")
        lines.append("    " + " \\\n    ".join(sorted(pip_packages)))
        lines.append("")

    if npm_packages:
        lines.append("echo 'Installing npm packages...'")
        lines.append("npm install -g " + " ".join(sorted(npm_packages)))
        lines.append("")

    for script_name in install_scripts:
        script_path = INSTALL_SCRIPTS_DIR / script_name
        if script_path.exists():
            # Each script runs in its own subshell: they are written as
            # standalone programs (own `set -e`, `exit 0` when the tool is
            # already present), and an inlined `exit` must not end the whole
            # customize.sh and silently skip the scripts after it.
            lines.append(f"echo 'Running install script: {script_name}...'")
            lines.append("(")
            lines.append(script_path.read_text().rstrip())
            lines.append(")")
            lines.append("")
        else:
            lines.append(f"echo 'WARNING: Install script not found: {script_name}'")

    lines.append("echo 'Customization complete.'")
    return "\n".join(lines) + "\n"


def compile_tools_json(agent: dict) -> str:
    """Collect all declared tools from agent + capability presets."""
    tools = {}

    # Tools from capability presets
    caps = agent.get("capabilities", {})
    for preset_name in caps.get("presets", []):
        preset = load_preset("capabilities", preset_name)
        for tool in preset.get("provides_tools", []):
            tools[tool["name"]] = {
                "name": tool["name"],
                "description": tool["description"],
                "from": f"preset:{preset_name}",
            }

    # Tools declared on agent (override preset descriptions)
    for tool in agent.get("tools", []):
        tools[tool["name"]] = {
            "name": tool["name"],
            "description": tool.get("description", ""),
            "from": tool.get("from", "agent"),
        }

    return json.dumps(sorted(tools.values(), key=lambda t: t["name"]), indent=2) + "\n"


def compile_system_prompt(agent: dict) -> str:
    """Assemble system prompt from role + preset rules + inline sections."""
    parts = []
    prompt = agent.get("prompt", {})
    agent_type = agent["agent"]["type"]

    # Role section
    role = prompt.get("role", f"You are an AI agent of type: {agent_type}.")
    parts.append(role.strip())

    # Tools section (auto-generated from declared tools)
    tools = {}
    caps = agent.get("capabilities", {})
    for preset_name in caps.get("presets", []):
        preset = load_preset("capabilities", preset_name)
        for tool in preset.get("provides_tools", []):
            tools[tool["name"]] = tool["description"]
    for tool in agent.get("tools", []):
        tools[tool["name"]] = tool.get("description", "")

    if tools:
        parts.append("\n## Tools Available")
        for name in sorted(tools):
            parts.append(f"- `{name}` — {tools[name]}")

    # Prompt presets (rulebook format)
    for preset_name in prompt.get("presets", []):
        preset = load_preset("prompts", preset_name)
        rules = preset.get("rules", [])
        if rules:
            section_title = preset.get("name", preset_name).replace("-", " ").title()
            parts.append(f"\n## {section_title} Rules")
            for rule in rules:
                rid = rule.get("id", "")
                parts.append(f"\n**[{rid}]** {rule['rule']}")
                if "example" in rule:
                    parts.append(f"```\n{rule['example'].rstrip()}\n```")
        elif "content" in preset:
            # Plain content preset (no rules)
            parts.append(f"\n{preset['content'].strip()}")

    # Inline sections
    sections = prompt.get("sections", {})
    for key, value in sections.items():
        if key == "output_dir":
            parts.append(f"\n## Output\nWrite results to `/home/agent/workspace/{value}/` as markdown files.")
        else:
            parts.append(f"\n## {key.replace('_', ' ').title()}\n{value.strip()}")

    return "\n".join(parts) + "\n"


def compile_agent_conf(agent: dict, global_config: dict) -> str:
    """Generate a bash-sourceable agent.conf."""
    agent_info = agent.get("agent", {})
    vm = agent.get("vm", {})
    llm = global_config.get("llm", {})
    defaults = global_config.get("vm_defaults", {})
    github = agent.get("github", {})

    # Gateway (in-VM OpenAI server) config
    gateway = global_config.get("gateway", {}) or {}
    gw_agents = gateway.get("agents", {}) or {}
    agent_gw = gw_agents.get(agent_info.get("type", "generic"), {}) or {}
    gateway_enabled = 1 if gateway.get("enabled", True) else 0
    gateway_port = gateway.get("vm_gateway_port", 8642)
    api_server_key = agent_gw.get("api_server_key", "") or ""

    # Harness selects the in-VM backend runner (see start.sh):
    #   "deepagents" (default) -> gateway_server.py / agent.py
    #   "omp"                  -> gateway_server.py / omp_agent.py (oh-my-pi)
    #   "hermes"               -> run-hermes.sh (pre-baked hermes container)
    harness = agent_info.get("harness", "deepagents")

    # Mnemosyne shared agent-memory (MCP over SSE). Inside every VM the host is
    # reachable as mnemosyne.host (published by agent-init -> the VM gateway IP),
    # so the URL is stable regardless of the per-VM gateway address.
    #
    # With memory.backend "hindsight" the agent instead gets HINDSIGHT_* (the
    # URL itself is written at launch by vm/prepare-rootfs.sh, which knows the
    # VM's gateway IP). An agent opts out with `memory: {enabled: false}`.
    mem = memory_settings(global_config)
    agent_memory = agent.get("memory", {}) or {}
    mem_on = mem["enabled"] and agent_memory.get("enabled", True)
    mnemosyne_enabled = 1 if mem_on and mem["backend"] == "mnemosyne" else 0
    hindsight_enabled = 1 if mem_on and mem["backend"] == "hindsight" else 0
    mnemosyne_port = mem["port"] if mem["backend"] == "mnemosyne" else 8077
    # Only when mnemosyne is the backend: under hindsight memory.token is the
    # ADMIN credential and must never be written into a VM.
    mnemosyne_token = mem["token"] if mnemosyne_enabled else ""
    agent_hs_token, agent_extra_banks = "", []
    if hindsight_enabled:
        agent_type_ = agent_info.get("type", "generic")
        agent_hs_token = _policy_module().agent_token(mem["token"], agent_type_)
        primary_bank = agent_memory.get("bank") or mem["bank"]
        # Additional banks it may read; recalled from alongside the primary.
        for extra_bank, spec in (agent_memory.get("banks") or {}).items():
            entry = resolve_access(spec if spec is not None else {"read": ["all"], "write": []},
                                   agent_type_, set(), [], "")
            if extra_bank != primary_bank and entry and (entry["read_all"] or entry["read"] or entry["read_untagged"]):
                agent_extra_banks.append(extra_bank)
    mnemosyne_url = f"http://mnemosyne.host:{mnemosyne_port}/sse"

    lines = [
        f"# Auto-generated agent.conf for {agent_info.get('type', 'unknown')}",
        f'AGENT_TYPE="{agent_info.get("type", "generic")}"',
        f'AGENT_NAME="{agent_info.get("name", "Agent")}"',
        f'LLM_MODEL="{vm.get("model", llm.get("model", "default-model"))}"',
        f'VCPUS="{vm.get("vcpus", defaults.get("vcpus", 4))}"',
        f'MEM_MB="{vm.get("mem_mb", defaults.get("mem_mb", 8192))}"',
        f'GATEWAY_ENABLED="{gateway_enabled}"',
        f'GATEWAY_PORT="{gateway_port}"',
        f'API_SERVER_KEY="{api_server_key}"',
        f'HARNESS="{harness}"',
        f'MNEMOSYNE_ENABLED="{mnemosyne_enabled}"',
        f'MNEMOSYNE_PORT="{mnemosyne_port}"',
        f'MNEMOSYNE_TOKEN="{mnemosyne_token}"',
        f'MNEMOSYNE_URL="{mnemosyne_url}"',
        f'HINDSIGHT_ENABLED="{hindsight_enabled}"',
        f'HINDSIGHT_PORT="{mem["port"] if mem["backend"] == "hindsight" else 8890}"',
        # This agent's own token (derived from the master, which never leaves
        # the host) and the banks the access policy grants it.
        f'HINDSIGHT_TOKEN="{agent_hs_token}"',
        f'HINDSIGHT_BANK="{agent_memory.get("bank") or mem["bank"]}"',
        f'HINDSIGHT_EXTRA_BANKS="{",".join(agent_extra_banks)}"',
    ]

    # Model limits, for harnesses that have to be told them: omp advertises both
    # to its own context management, and hermes requests max_output_tokens as
    # its completion budget (unset, it asks for 65536, which a local model
    # server rejects). llm.context_window / llm.max_output_tokens in
    # sandbox.yaml, overridable per agent under vm:. Unset -> harness defaults.
    context_window = vm.get("context_window") or llm.get("context_window")
    max_output = vm.get("max_output_tokens") or llm.get("max_output_tokens")
    if context_window:
        lines.append(f'LLM_CONTEXT_WINDOW="{int(context_window)}"')
    if max_output:
        lines.append(f'LLM_MAX_OUTPUT_TOKENS="{int(max_output)}"')

    # GitHub token injection — read from secrets file if it exists
    agent_type = agent_info.get("type", "generic")
    token_file = SANDBOX_ROOT / "config" / "secrets" / "github-tokens" / f"{agent_type}.token"
    if token_file.exists():
        token = token_file.read_text().strip()
        if token:
            lines.append(f'GITHUB_TOKEN="{token}"')

    # GitHub config from YAML
    if github:
        repos = github.get("repos", [])
        if repos:
            lines.append(f'GITHUB_REPOS="{" ".join(repos)}"')
        perms = github.get("permissions", [])
        if perms:
            lines.append(f'GITHUB_PERMISSIONS="{" ".join(perms)}"')

    return "\n".join(lines) + "\n"


def cmd_compile(agent_type: str):
    """Compile an agent YAML into flat build artifacts."""
    agent = load_agent(agent_type)
    global_config = load_global_config()
    out_dir = BUILD_DIR / agent_type
    out_dir.mkdir(parents=True, exist_ok=True)

    (out_dir / "agent.conf").write_text(compile_agent_conf(agent, global_config))
    (out_dir / "allowlist.txt").write_text(compile_allowlist(agent))
    (out_dir / "system-prompt.md").write_text(compile_system_prompt(agent))

    customize = compile_customize_sh(agent)
    customize_path = out_dir / "customize.sh"
    customize_path.write_text(customize)
    customize_path.chmod(0o755)

    (out_dir / "tools.json").write_text(compile_tools_json(agent))

    print(f"Compiled {agent_type} → {out_dir}/")
    for f in sorted(out_dir.iterdir()):
        print(f"  {f.name} ({f.stat().st_size} bytes)")


# ---------------------------------------------------------------------------
# Compile: Global Config
# ---------------------------------------------------------------------------

def cmd_compile_global():
    """Compile sandbox.yaml into a bash-sourceable sandbox.conf."""
    config = load_global_config()
    BUILD_DIR.mkdir(parents=True, exist_ok=True)

    llm = config.get("llm", {})
    defaults = config.get("vm_defaults", {})
    network = config.get("network", {})
    fc = config.get("firecracker", {})
    rootfs = config.get("rootfs", {})
    squid = config.get("squid", {})
    mem = memory_settings(config)

    lines = [
        "# Auto-generated from config/sandbox.yaml",
        "# Do not edit — regenerate with: agentconf.py compile-global",
        "",
        f'LLM_API_BASE="{llm.get("api_base", "http://localhost:4000/v1")}"',
        f'LLM_API_KEY="{llm.get("api_key", "sk-default")}"',
        f'LLM_MODEL="{llm.get("model", "default-model")}"',
        "",
        f'DEFAULT_VCPUS={defaults.get("vcpus", 4)}',
        f'DEFAULT_MEM_MB={defaults.get("mem_mb", 8192)}',
        "",
        f'HOST_IFACE="{network.get("host_iface", "enp12s0")}"',
        f'VM_SUBNET_PREFIX="{network.get("subnet_prefix", "10.0")}"',
        f'NOVNC_BASE_PORT={network.get("novnc_base_port", 6080)}',
        f'SSH_BASE_PORT={network.get("ssh_base_port", 2200)}',
        "",
        f'FIRECRACKER_BIN="{fc.get("bin", "/usr/local/bin/firecracker")}"',
        f'JAILER_BIN="{fc.get("jailer", "/usr/local/bin/jailer")}"',
        "",
        f'KERNEL_PATH="${{SANDBOX_ROOT}}/kernel/vmlinux"',
        f'BASE_ROOTFS="${{SANDBOX_ROOT}}/rootfs/base.ext4"',
        f'ROOTFS_SIZE_MB={rootfs.get("size_mb", 8192)}',
        "",
        f'LOG_DIR="${{SANDBOX_ROOT}}/state/logs"',
        f'STATE_DIR="${{SANDBOX_ROOT}}/state"',
        "",
        f'SQUID_HTTP_PORT={squid.get("http_port", 3128)}',
        f'SQUID_HTTPS_PORT={squid.get("https_port", 3129)}',
        "",
        f'MEMORY_ENABLED={1 if mem["enabled"] else 0}',
        f'MEMORY_BACKEND="{mem["backend"]}"',
        f'MNEMOSYNE_PORT={mem["port"] if mem["backend"] == "mnemosyne" else 8077}',
        f'MNEMOSYNE_TOKEN="{mem["token"] if mem["backend"] == "mnemosyne" else ""}"',
        f'MNEMOSYNE_EMBEDDINGS="{mem["embeddings"]}"',
        f'HINDSIGHT_PORT={mem["port"] if mem["backend"] == "hindsight" else 8890}',
        # The admin credential, under a name no per-VM file uses: the launch
        # scripts source this file, and a VM must only ever receive the agent's
        # own HINDSIGHT_TOKEN from its agent.conf.
        f'HINDSIGHT_ADMIN_TOKEN="{mem["token"] if mem["backend"] == "hindsight" else ""}"',
        f'HINDSIGHT_BANK="{mem["bank"]}"',
        f'HINDSIGHT_LLM_MODEL="{mem["llm_model"]}"',
        f'HINDSIGHT_MAX_COMPLETION_TOKENS={mem["max_completion_tokens"]}',
    ]

    # Per-agent rootfs size overrides -> ROOTFS_SIZE_MB_<type> (hyphens -> _).
    # cmd_build_agent reads these to grow a cloned rootfs (e.g. hermes, which
    # needs headroom to bake + docker-load the pre-baked image on one fs).
    rootfs_per_agent = rootfs.get("per_agent", {}) or {}
    if rootfs_per_agent:
        lines.append("")
        for atype, size in rootfs_per_agent.items():
            var = "ROOTFS_SIZE_MB_" + str(atype).replace("-", "_")
            lines.append(f"{var}={size}")

    out_path = BUILD_DIR / "sandbox.conf"
    out_path.write_text("\n".join(lines) + "\n")
    print(f"Compiled global config → {out_path}")


# ---------------------------------------------------------------------------
# Compile: Gateway Router Config
# ---------------------------------------------------------------------------

def cmd_compile_gateway():
    """Compile the gateway: block from sandbox.yaml into state/gateway/gateway.json."""
    config = load_global_config()
    gateway = config.get("gateway", {}) or {}

    out_dir = SANDBOX_ROOT / "state" / "gateway"
    out_dir.mkdir(parents=True, exist_ok=True)

    # Normalize tokens -> [{name, token, agents}]
    tokens = []
    for t in (gateway.get("tokens", []) or []):
        t = t or {}
        tokens.append({
            "name": t.get("name", ""),
            "token": t.get("token", ""),
            "agents": t.get("agents", []),
        })

    # Normalize agents -> {"<type>": {"api_server_key": "<key>"[, "model": "<m>"]
    #                                 [, "concurrency": <n>]}}
    agents = {}
    for name, spec in (gateway.get("agents", {}) or {}).items():
        spec = spec or {}
        entry = {"api_server_key": spec.get("api_server_key", "") or ""}
        # Optional per-agent model rewrite: the router rewrites the outgoing
        # OpenAI `model` (== agent id) to this value before forwarding downstream.
        model = spec.get("model")
        if model:
            entry["model"] = model
        # Optional per-agent concurrency override (0/absent -> the router's
        # scheduler.default_concurrency; the Go side is authoritative).
        if "concurrency" in spec:
            entry["concurrency"] = int(spec["concurrency"])
        # Optional per-agent runs-API capability flag: advertises interactive
        # dangerous-command approval on /v1/capabilities for this agent. Omit
        # when false to match the Go router's `omitempty` (false is the default).
        if spec.get("approval"):
            entry["approval"] = True
        agents[name] = entry

    gateway_json = {
        "bind": gateway.get("bind", "0.0.0.0"),
        "port": gateway.get("port", 8642),
        "default_agent": gateway.get("default_agent", "feature-dev"),
        "state_dir": str(SANDBOX_ROOT / "state"),
        "vm_gateway_port": gateway.get("vm_gateway_port", 8642),
        "tokens": tokens,
        "agents": agents,
    }

    # Scheduler / tasks / observability / dashboard blocks pass through
    # VERBATIM when present. No Python-side defaulting — the Go router's
    # applyDefaults() is authoritative, so a hand-edited or stale gateway.json
    # (missing these blocks entirely) behaves identically to one compiled here.
    for key in ("scheduler", "tasks", "observability", "dashboard"):
        if key in gateway:
            gateway_json[key] = gateway[key]

    out_path = out_dir / "gateway.json"
    with open(out_path, "w") as f:
        json.dump(gateway_json, f, indent=2)
        f.write("\n")
    print(f"Compiled gateway config → {out_path}")


# ---------------------------------------------------------------------------
# Validate
# ---------------------------------------------------------------------------

def cmd_validate(agent_type: str):
    """Validate an agent YAML and its preset references."""
    agent = load_agent(agent_type)
    errors = []
    warnings = []

    # Check required fields
    if "agent" not in agent or "type" not in agent.get("agent", {}):
        errors.append("Missing agent.type")
    if "agent" not in agent or "name" not in agent.get("agent", {}):
        errors.append("Missing agent.name")

    # Validate the harness and what it needs in the image
    harness = agent.get("agent", {}).get("harness", "deepagents")
    if harness not in HARNESSES:
        errors.append(f"Unknown agent.harness '{harness}' (expected one of: {', '.join(HARNESSES)})")
    needs = HARNESS_CAPABILITY.get(harness)
    if needs and needs not in agent.get("capabilities", {}).get("presets", []):
        errors.append(f"agent.harness '{harness}' requires the '{needs}' capability preset")

    # Validate memory: the backend, and this agent's mental models
    global_config = load_global_config() if (CONFIG_DIR / "sandbox.yaml").exists() else {}
    backend = memory_settings(global_config)["backend"]
    if backend not in MEMORY_BACKENDS:
        errors.append(f"Unknown memory.backend '{backend}' (expected one of: {', '.join(MEMORY_BACKENDS)})")
    errors.extend(memory_plan(global_config, {**_all_agents(), agent_type: agent})["errors"])
    if (agent.get("memory", {}) or {}).get("mental_models") and backend != "hindsight" and global_config:
        warnings.append("memory.mental_models are only used with memory.backend: hindsight")

    # Validate egress presets
    for p in agent.get("egress", {}).get("presets", []):
        path = PRESETS_DIR / "egress" / f"{p}.yaml"
        if not path.exists():
            errors.append(f"Egress preset not found: {p}")

    # Validate capability presets
    for p in agent.get("capabilities", {}).get("presets", []):
        path = PRESETS_DIR / "capabilities" / f"{p}.yaml"
        if not path.exists():
            errors.append(f"Capability preset not found: {p}")

    # Validate prompt presets
    for p in agent.get("prompt", {}).get("presets", []):
        path = PRESETS_DIR / "prompts" / f"{p}.yaml"
        if not path.exists():
            errors.append(f"Prompt preset not found: {p}")

    # Validate install scripts
    for s in agent.get("capabilities", {}).get("install_scripts", []):
        path = INSTALL_SCRIPTS_DIR / s
        if not path.exists():
            errors.append(f"Install script not found: {s}")

    # Check capability preset install scripts
    for p in agent.get("capabilities", {}).get("presets", []):
        preset_path = PRESETS_DIR / "capabilities" / f"{p}.yaml"
        if preset_path.exists():
            preset = load_yaml(preset_path)
            # Normalize install_script (str or list) + install_scripts (list)
            scripts = []
            is_field = preset.get("install_script", [])
            if isinstance(is_field, str):
                scripts.append(is_field)
            elif isinstance(is_field, list):
                scripts.extend(is_field)
            scripts.extend(preset.get("install_scripts", []))
            for s in scripts:
                script = INSTALL_SCRIPTS_DIR / s
                if not script.exists():
                    warnings.append(f"Install script from preset {p}: {s} not found")

    if errors:
        print(f"FAIL: {agent_type} has {len(errors)} error(s):")
        for e in errors:
            print(f"  ERROR: {e}")
    if warnings:
        for w in warnings:
            print(f"  WARN: {w}")
    if not errors:
        print(f"OK: {agent_type} is valid")

    return len(errors) == 0


# ---------------------------------------------------------------------------
# List
# ---------------------------------------------------------------------------

def cmd_list():
    """List all available agent types."""
    agents = sorted(AGENTS_DIR.glob("*.yaml"))
    if not agents:
        print("No agents defined in config/agents/")
        return
    print("Available agents:")
    for path in agents:
        agent = load_yaml(path)
        name = agent.get("agent", {}).get("name", "")
        atype = path.stem
        print(f"  {atype:<20} {name}")


def cmd_list_presets(category: str = None):
    """List available presets, optionally filtered by category."""
    categories = [category] if category else ["egress", "capabilities", "prompts"]
    for cat in categories:
        cat_dir = PRESETS_DIR / cat
        if not cat_dir.exists():
            continue
        print(f"\n{cat.upper()} presets:")
        for path in sorted(cat_dir.glob("*.yaml")):
            preset = load_yaml(path)
            name = preset.get("name", path.stem)
            desc = preset.get("description", "")
            print(f"  {name:<25} {desc}")


# ---------------------------------------------------------------------------
# Docs Generation
# ---------------------------------------------------------------------------

def cmd_docs():
    """Generate docs/presets-reference.md from all preset files."""
    docs_dir = SANDBOX_ROOT / "docs"
    docs_dir.mkdir(exist_ok=True)
    out = docs_dir / "presets-reference.md"

    lines = ["# Presets Reference", "", "Auto-generated by `agentconf.py docs`.", ""]

    for cat in ["egress", "capabilities", "prompts"]:
        cat_dir = PRESETS_DIR / cat
        if not cat_dir.exists():
            continue
        lines.append(f"## {cat.title()} Presets")
        lines.append("")

        for path in sorted(cat_dir.glob("*.yaml")):
            preset = load_yaml(path)
            name = preset.get("name", path.stem)
            desc = preset.get("description", "")
            lines.append(f"### `{name}`")
            lines.append(f"{desc}")
            lines.append("")

            if cat == "egress":
                domains = preset.get("domains", [])
                if domains:
                    lines.append("Domains:")
                    for d in domains:
                        lines.append(f"- `{d}`")
                    lines.append("")

            elif cat == "capabilities":
                pkgs = preset.get("packages", {})
                for pkg_type in ["apt", "pip", "npm"]:
                    pkg_list = pkgs.get(pkg_type, [])
                    if pkg_list:
                        lines.append(f"{pkg_type}: `{', '.join(pkg_list)}`")
                if "install_script" in preset:
                    lines.append(f"Install script: `{preset['install_script']}`")
                tools = preset.get("provides_tools", [])
                if tools:
                    lines.append("")
                    lines.append("Tools provided:")
                    for t in tools:
                        lines.append(f"- **{t['name']}** — {t['description']}")
                lines.append("")

            elif cat == "prompts":
                rules = preset.get("rules", [])
                if rules:
                    lines.append(f"Rules ({len(rules)}):")
                    for r in rules:
                        lines.append(f"- **[{r['id']}]** {r['rule']}")
                    lines.append("")

    out.write_text("\n".join(lines) + "\n")
    print(f"Generated {out}")


# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

def main():
    if len(sys.argv) < 2:
        print(__doc__)
        sys.exit(1)

    cmd = sys.argv[1]

    if cmd == "compile" and len(sys.argv) >= 3:
        cmd_compile(sys.argv[2])
    elif cmd == "compile-global":
        cmd_compile_global()
    elif cmd == "compile-gateway":
        cmd_compile_gateway()
    elif cmd == "compile-all":
        cmd_compile_global()
        cmd_compile_gateway()
        for path in sorted(AGENTS_DIR.glob("*.yaml")):
            cmd_compile(path.stem)
    elif cmd == "validate" and len(sys.argv) >= 3:
        ok = cmd_validate(sys.argv[2])
        sys.exit(0 if ok else 1)
    elif cmd == "memory-policy":
        # Write the access policy only (the service must have it before it starts).
        plan = memory_plan(load_global_config(), _all_agents())
        for e in plan["errors"]:
            print(f"ERROR: {e}", file=sys.stderr)
        if plan["errors"]:
            sys.exit(1)
        print(f"Policy written: {write_memory_policy(plan)}")
    elif cmd == "memory-access":
        sys.exit(0 if cmd_memory_access(sys.argv[2] if len(sys.argv) > 2 else "") else 1)
    elif cmd == "memory-apply":
        args = sys.argv[2:]
        dry = "--dry-run" in args
        urls = [a for a in args if not a.startswith("--")]
        sys.exit(0 if cmd_memory_apply(urls[0] if urls else "", dry_run=dry) else 1)
    elif cmd == "list":
        cmd_list()
    elif cmd == "list-presets":
        category = sys.argv[2] if len(sys.argv) >= 3 else None
        cmd_list_presets(category)
    elif cmd == "docs":
        cmd_docs()
    else:
        print(__doc__)
        sys.exit(1)


if __name__ == "__main__":
    main()
