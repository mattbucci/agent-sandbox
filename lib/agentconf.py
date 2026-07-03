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
            lines.append(f"echo 'Running install script: {script_name}...'")
            lines.append(script_path.read_text())
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
    #   "hermes"               -> run-hermes.sh (pre-baked hermes container)
    harness = agent_info.get("harness", "deepagents")

    # Mnemosyne shared agent-memory (MCP over SSE). Inside every VM the host is
    # reachable as mnemosyne.host (published by agent-init -> the VM gateway IP),
    # so the URL is stable regardless of the per-VM gateway address.
    memory = global_config.get("memory", {}) or {}
    mnemosyne_enabled = 1 if memory.get("enabled", False) else 0
    mnemosyne_port = memory.get("port", 8077)
    mnemosyne_token = memory.get("token", "") or ""
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
    ]

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
    memory = config.get("memory", {}) or {}

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
        f'MEMORY_ENABLED={1 if memory.get("enabled", False) else 0}',
        f'MNEMOSYNE_PORT={memory.get("port", 8077)}',
        f'MNEMOSYNE_TOKEN="{memory.get("token", "") or ""}"',
        f'MNEMOSYNE_EMBEDDINGS="{memory.get("embeddings", "fastembed") or "fastembed"}"',
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
