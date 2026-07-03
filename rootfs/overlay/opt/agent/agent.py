#!/usr/bin/env python3
"""
Agent Sandbox Runtime — DeepAgents + LangGraph

Minimal agent that connects to a LiteLLM proxy (OpenAI-compatible API)
and uses LocalShellBackend for real bash execution inside the Firecracker VM.

Configuration is read from environment variables (set via /etc/agent.conf):
  LLM_API_BASE  - LiteLLM proxy URL (e.g., http://192.168.1.100:4000/v1)
  LLM_API_KEY   - API key for LiteLLM
  LLM_MODEL     - Model name (e.g., deepseek-coder-v2)
  AGENT_TYPE    - Agent specialization (debugger, feature-dev, etc.)
  AGENT_NAME    - Human-readable agent name
  OTEL_EXPORTER_OTLP_ENDPOINT - OTel collector endpoint (e.g., http://10.0.X.1:4318)
"""

import os
import sys
import asyncio
import logging

# Log to the agent log file plus stdout. The file path is overridable via
# AGENT_LOG_FILE; if it can't be opened (e.g. host-side unit tests where
# /var/log isn't writable) fall back to stdout-only instead of crashing at
# import — importing this module must never hard-fail on the log sink.
_log_handlers = [logging.StreamHandler(sys.stdout)]
try:
    _log_handlers.insert(0, logging.FileHandler(os.environ.get("AGENT_LOG_FILE", "/var/log/agent.log")))
except OSError:
    pass
logging.basicConfig(
    level=logging.INFO,
    format="[%(asctime)s] %(name)s %(levelname)s: %(message)s",
    handlers=_log_handlers,
)
logger = logging.getLogger("agent")

def init_tracing():
    """Initialize OpenTelemetry tracing if an endpoint is configured.

    Returns a list of LangChain callback handlers (possibly empty) that the
    caller passes to create_agent(callbacks=...) so agent/LLM/tool runs are
    traced. Always returns a list; tracing failures never break the agent.
    """
    callbacks = []
    otel_endpoint = os.environ.get("OTEL_EXPORTER_OTLP_ENDPOINT")
    if not otel_endpoint:
        logger.info("No OTEL_EXPORTER_OTLP_ENDPOINT set, tracing disabled")
        return callbacks

    try:
        from opentelemetry import trace
        from opentelemetry.sdk.trace import TracerProvider
        from opentelemetry.sdk.trace.export import BatchSpanProcessor
        from opentelemetry.exporter.otlp.proto.http.trace_exporter import OTLPSpanExporter
        from opentelemetry.sdk.resources import Resource

        agent_type = os.environ.get("AGENT_TYPE", "generic")
        agent_name = os.environ.get("AGENT_NAME", "Agent")

        resource = Resource.create({
            "service.name": f"agent-sandbox-{agent_type}",
            "service.instance.id": os.environ.get("HOSTNAME", "unknown"),
            "agent.type": agent_type,
            "agent.name": agent_name,
        })

        provider = TracerProvider(resource=resource)
        exporter = OTLPSpanExporter(endpoint=f"{otel_endpoint}/v1/traces")
        provider.add_span_processor(BatchSpanProcessor(exporter))
        trace.set_tracer_provider(provider)

        # Auto-instrument HTTP clients so LLM calls are traced
        try:
            from opentelemetry.instrumentation.httpx import HTTPXClientInstrumentor
            HTTPXClientInstrumentor().instrument()
        except Exception:
            pass
        try:
            from opentelemetry.instrumentation.requests import RequestsInstrumentor
            RequestsInstrumentor().instrument()
        except Exception:
            pass

        logger.info(f"OTel tracing enabled → {otel_endpoint}")
    except Exception as e:
        logger.warning(f"Failed to initialize OTel tracing: {e}")
        return callbacks

    # Hand-rolled LangChain callback handler (agent.graph / chat / tool spans);
    # replaces opentelemetry-instrumentation-langchain — no extra pip package.
    try:
        from otel_callbacks import OTelCallbackHandler
        callbacks.append(OTelCallbackHandler())
    except Exception as e:
        logger.warning(f"OTel LangChain callbacks unavailable: {e}")

    return callbacks


class ApprovalShellBackend:
    """LocalShellBackend that gates dangerous commands behind the runs-API
    approval flow.

    Defined lazily as a subclass at first use (create_agent imports deepagents
    there, keeping module import cheap). When a shell command runs INSIDE a run
    (approvals.current_run is set) and is classified dangerous, execute() emits
    an approval.request event and blocks until the client decides; a "deny"
    returns a synthetic denied ExecuteResponse and never touches the shell. With
    no run context (plain chat / task path) it is exactly LocalShellBackend, so
    those paths are unchanged.

    This is a thin factory returning the real subclass so `isinstance`/backend
    protocol checks in deepagents still see a LocalShellBackend.
    """

    def __new__(cls, *args, **kwargs):
        from deepagents.backends import LocalShellBackend
        from deepagents.backends.protocol import ExecuteResponse
        import approvals

        class _ApprovalShellBackend(LocalShellBackend):
            def execute(self, command, *, timeout=None):
                run = approvals.current_run.get()
                if run is None:
                    return super().execute(command, timeout=timeout)
                preview = (command or "")[:200]
                run.emit({"event": "tool.started", "tool": "shell", "preview": preview})
                if approvals.is_dangerous(command):
                    choice = run.request_approval(command)
                    if choice == "deny":
                        run.emit({"event": "tool.completed", "tool": "shell",
                                  "error": True, "denied": True})
                        return ExecuteResponse(
                            output="Command denied by user via the approval flow.",
                            exit_code=126,
                            truncated=False,
                        )
                result = super().execute(command, timeout=timeout)
                run.emit({"event": "tool.completed", "tool": "shell",
                          "error": bool(getattr(result, "exit_code", 0))})
                return result

        return _ApprovalShellBackend(*args, **kwargs)


def load_system_prompt() -> str:
    """Load the agent's system prompt from /etc/agent/system-prompt.md"""
    prompt_path = "/etc/agent/system-prompt.md"
    if os.path.exists(prompt_path):
        with open(prompt_path) as f:
            return f.read().strip()
    return f"You are an AI agent of type: {os.environ.get('AGENT_TYPE', 'generic')}."


async def _load_mcp_tools():
    """Fetch tools from the mnemosyne MCP server (shared agent-memory).

    Returns a list of LangChain tools, or [] on any failure so the agent can
    always be built — even when mnemosyne is disabled or unreachable.
    """
    url = os.environ.get("MNEMOSYNE_URL")
    token = os.environ.get("MNEMOSYNE_TOKEN", "")
    if not url:
        logger.warning("MNEMOSYNE_ENABLED set but MNEMOSYNE_URL is empty; skipping MCP tools")
        return []

    try:
        from langchain_mcp_adapters.client import MultiServerMCPClient

        client = MultiServerMCPClient({
            "mnemosyne": {
                "url": url,
                "transport": "sse",
                "headers": {"Authorization": f"Bearer {token}"},
            }
        })
        tools = await client.get_tools()
        logger.info(f"Loaded {len(tools)} mnemosyne MCP tool(s) from {url}")
        return tools
    except Exception as e:
        logger.warning(f"Failed to load mnemosyne MCP tools from {url}: {e}")
        return []


def create_agent(callbacks=None):
    """Create and configure the DeepAgents agent.

    `callbacks` is the (possibly empty) list returned by init_tracing(); when
    present it is wired onto every run via .with_config so the whole
    LangGraph run tree is traced.
    """
    from deepagents import create_deep_agent
    from langchain_openai import ChatOpenAI

    api_base = os.environ.get("LLM_API_BASE", "http://localhost:4000/v1")
    api_key = os.environ.get("LLM_API_KEY", "sk-default")
    model_name = os.environ.get("LLM_MODEL", "default-model")
    agent_type = os.environ.get("AGENT_TYPE", "generic")

    logger.info(f"Creating {agent_type} agent")
    logger.info(f"LLM endpoint: {api_base}")
    logger.info(f"Model: {model_name}")

    # Set OpenAI-compatible env vars for LangChain
    os.environ["OPENAI_API_KEY"] = api_key
    os.environ["OPENAI_API_BASE"] = api_base

    system_prompt = load_system_prompt()
    logger.info(f"System prompt loaded ({len(system_prompt)} chars)")

    # Build the model explicitly with use_responses_api=False: the LLM router
    # implements /v1/chat/completions (+ /v1/messages), NOT the OpenAI Responses
    # API (/v1/responses). deepagents' default "openai:<model>" init uses the
    # Responses API, which 404s against the router — so pin chat-completions.
    model = ChatOpenAI(
        model=model_name,
        base_url=api_base,
        api_key=api_key,
        use_responses_api=False,
    )

    # Optionally load mnemosyne shared agent-memory tools over MCP (SSE).
    # create_agent() is always called OUTSIDE a running event loop (gateway_server
    # calls it under a thread lock, not inside a coroutine; main() calls it before
    # asyncio.run). So asyncio.run() here is safe and won't block a live loop.
    mcp_tools = []
    if os.environ.get("MNEMOSYNE_ENABLED", "").strip() not in ("", "0", "false", "False"):
        try:
            mcp_tools = asyncio.run(_load_mcp_tools())
        except Exception as e:
            logger.warning(f"mnemosyne MCP tool loading failed: {e}")
            mcp_tools = []
    else:
        logger.info("MNEMOSYNE_ENABLED not set; agent runs without MCP memory tools")

    # Create the agent with ApprovalShellBackend (a LocalShellBackend that gates
    # dangerous commands behind the runs-API approval flow when executing inside
    # a run; identical to LocalShellBackend on the chat/task paths). The
    # Firecracker VM IS the sandbox, so unrestricted shell is safe; the approval
    # gate is a UX affordance for the webui, not a security boundary.
    # deepagents >=0.6 renamed the working-directory arg from `cwd` to `root_dir`.
    deep_agent_kwargs = dict(
        model=model,
        backend=ApprovalShellBackend(
            root_dir="/home/agent/workspace",
        ),
        system_prompt=system_prompt,
    )
    if mcp_tools:
        deep_agent_kwargs["tools"] = mcp_tools

    agent = create_deep_agent(**deep_agent_kwargs)

    # Attach tracing callbacks to every run of this agent. Failure here costs
    # exactly one warning and yields an uninstrumented (but working) agent.
    if callbacks:
        try:
            agent = agent.with_config({"callbacks": list(callbacks)})
        except Exception as e:
            logger.warning(f"Failed to attach tracing callbacks: {e}")

    return agent


async def run_agent_loop(agent):
    """Run the agent in a loop, waiting for tasks via stdin or a task file."""
    agent_type = os.environ.get("AGENT_TYPE", "generic")
    agent_name = os.environ.get("AGENT_NAME", "Agent")

    logger.info(f"{agent_name} is ready and waiting for input...")

    # Check if there's an initial task file
    task_file = "/etc/agent/initial-task.md"
    if os.path.exists(task_file):
        with open(task_file) as f:
            initial_task = f.read().strip()
        if initial_task:
            logger.info(f"Found initial task ({len(initial_task)} chars)")
            result = await agent.ainvoke(
                {"messages": [{"role": "user", "content": initial_task}]}
            )
            logger.info("Initial task completed")
            if "messages" in result:
                for msg in result["messages"]:
                    if hasattr(msg, "content"):
                        logger.info(f"Agent response: {msg.content[:500]}")

    # Interactive loop: read tasks from a watch directory
    task_dir = "/home/agent/tasks"
    os.makedirs(task_dir, exist_ok=True)

    logger.info(f"Watching {task_dir} for new tasks...")
    processed = set()

    while True:
        try:
            for fname in sorted(os.listdir(task_dir)):
                fpath = os.path.join(task_dir, fname)
                if fpath in processed or not fname.endswith(".md"):
                    continue

                processed.add(fpath)
                logger.info(f"Processing task: {fname}")

                with open(fpath) as f:
                    task_content = f.read().strip()

                if not task_content:
                    continue

                result = await agent.ainvoke(
                    {"messages": [{"role": "user", "content": task_content}]}
                )

                # Write result
                result_path = fpath.replace(".md", ".result.md")
                if "messages" in result:
                    last_msg = result["messages"][-1]
                    if hasattr(last_msg, "content"):
                        with open(result_path, "w") as f:
                            f.write(last_msg.content)
                        logger.info(f"Result written to {result_path}")

        except Exception as e:
            logger.error(f"Error processing tasks: {e}")

        await asyncio.sleep(5)


def main():
    logger.info("Agent sandbox runtime starting...")
    callbacks = init_tracing()
    agent = create_agent(callbacks=callbacks)
    asyncio.run(run_agent_loop(agent))


if __name__ == "__main__":
    main()
