#!/usr/bin/env python3
"""
omp harness adapter — drive oh-my-pi (`omp --mode rpc`) behind the in-VM gateway.

gateway_server.py serves the Hermes wire contract on :8642 and is written
against a LangGraph-style agent (`ainvoke` / `astream`). This module provides
an object with that same surface whose turns are executed by an `omp` process
instead of DeepAgents, so the chat, task and runs paths — sessions, SSE
streaming, the interactive approval flow — work unchanged for an agent whose
YAML says `harness: omp`.

Mapping:

  session      One `omp --mode rpc` process per X-Hermes-Session-Id. omp keeps
               the conversation itself, so only the newest user message is sent
               each turn. Every session has its own --session-dir and is started
               with --continue, so a process that was evicted (LRU, idle) or
               died resumes the same conversation from disk.
  streaming    `message_update` text deltas become AIMessageChunks on the
               "messages" stream; the final answer is the "values" state.
  approvals    omp runs with --approval-mode write, so every exec-tier tool call
               asks first. Outside a run the call is approved (the Firecracker
               VM is the sandbox). Inside a run, a shell command that
               approvals.is_dangerous() flags goes through the run's approval
               gate — the same policy ApprovalShellBackend applies for
               DeepAgents.
  model        A single custom provider ("sandbox") pointing at LLM_API_BASE,
               written to ~/.omp/agent/models.yml at startup.
  memory       When MNEMOSYNE_ENABLED, the shared mnemosyne MCP server is
               written to ~/.omp/agent/mcp.json — reached through the stdio
               bridge mcp_sse_bridge.py — and omp is told to wait for its tools
               at startup (OMP_MCP_STARTUP_TIMEOUT_MS).
  tracing      omp exports OTLP itself from OTEL_EXPORTER_OTLP_ENDPOINT, which
               start.sh already sets.

RPC protocol reference: https://github.com/can1357/oh-my-pi/blob/main/docs/rpc.md
"""

import asyncio
import json
import logging
import os
import re
import time
import uuid
from collections import OrderedDict

import approvals

logger = logging.getLogger("omp")

PROVIDER = "sandbox"
DEFAULT_SESSION = "default"

# One stdout frame is capped at 1 MiB by omp (protocol v1); leave headroom.
_FRAME_LIMIT = 4 * 1024 * 1024

# Dialog methods that block omp until answered. Everything else in the
# extension-UI channel (notify, setStatus, setWidget, ...) is fire-and-forget.
_DIALOG_METHODS = ("select", "confirm", "input", "editor", "ask")

_APPROVE = "Approve"
_DENY = "Deny"
_APPROVAL_TITLE = re.compile(r"^Allow tool: (\S+)")


class OmpError(RuntimeError):
    """A turn failed inside omp (provider error, process death, protocol)."""


def _env_int(env, name, default):
    try:
        return int(env.get(name, "") or default)
    except ValueError:
        return default


def _safe_session_dirname(session_id):
    """Map an arbitrary client session id to a safe, stable directory name."""
    clean = re.sub(r"[^A-Za-z0-9_.-]", "_", session_id or DEFAULT_SESSION)[:80]
    if clean != session_id:
        # Sanitising can collide ("a/b" vs "a_b"); keep them distinct.
        import hashlib
        clean += "-" + hashlib.sha256(session_id.encode("utf-8")).hexdigest()[:10]
    return clean.lstrip(".") or DEFAULT_SESSION


class OmpConfig:
    """Everything the adapter needs, read once from the environment."""

    def __init__(self, env=None):
        env = os.environ if env is None else env
        home = env.get("HOME") or os.path.expanduser("~")
        self.bin = env.get("OMP_BIN", "omp")
        self.agent_dir = env.get("PI_CODING_AGENT_DIR") or os.path.join(home, ".omp", "agent")
        self.sessions_root = env.get("OMP_SESSIONS_DIR") or os.path.join(home, ".omp", "gateway-sessions")
        self.workspace = env.get("OMP_WORKSPACE", "/home/agent/workspace")
        self.system_prompt_file = env.get("OMP_SYSTEM_PROMPT_FILE", "/etc/agent/system-prompt.md")
        self.api_base = env.get("LLM_API_BASE", "http://localhost:4000/v1")
        self.api_key = env.get("LLM_API_KEY", "sk-default")
        self.model = env.get("LLM_MODEL", "default-model")
        self.context_window = _env_int(env, "OMP_CONTEXT_WINDOW", 131072)
        # Input + this must fit the model's real window; 8192 leaves room for
        # omp's prompt, the tool list and recalled memory even on a 32k model.
        self.max_tokens = _env_int(env, "OMP_MAX_TOKENS", 8192)
        self.max_sessions = max(1, _env_int(env, "OMP_MAX_SESSIONS", 4))
        self.idle_timeout_s = _env_int(env, "OMP_IDLE_TIMEOUT_S", 1800)
        self.start_timeout_s = _env_int(env, "OMP_START_TIMEOUT_S", 120)
        self.extra_args = (env.get("OMP_EXTRA_ARGS", "") or "").split()
        self.mcp_startup_timeout_ms = _env_int(env, "OMP_MCP_STARTUP_TIMEOUT_MS", 20000)
        self.max_retries = _env_int(env, "OMP_MAX_RETRIES", 2)
        self.retry_max_delay_ms = _env_int(env, "OMP_RETRY_MAX_DELAY_MS", 10000)
        self._env = dict(env)
        self.bridge_python = env.get("OMP_MCP_BRIDGE_PYTHON", "python3")
        self.bridge_script = env.get("OMP_MCP_BRIDGE_SCRIPT") or os.path.join(
            os.path.dirname(os.path.abspath(__file__)), "mcp_sse_bridge.py")
        # Hindsight: the explicit memory tools, over MCP. (Recall before and
        # retain after every turn is done by gateway_server.py for all harnesses.)
        hs = (env.get("HINDSIGHT_ENABLED", "") or "").strip()
        self.hindsight_url = (env.get("HINDSIGHT_URL", "") or "").rstrip("/") if hs in ("1", "true", "True") else ""
        self.hindsight_token = env.get("HINDSIGHT_TOKEN", "")
        self.hindsight_bank = env.get("HINDSIGHT_BANK", "") or "agent-sandbox"
        mnem = (env.get("MNEMOSYNE_ENABLED", "") or "").strip()
        self.mnemosyne_url = env.get("MNEMOSYNE_URL", "") if mnem not in ("", "0", "false", "False") else ""
        self.mnemosyne_token = env.get("MNEMOSYNE_TOKEN", "")

    # ---- files omp reads at startup ----
    def models_config(self):
        """models.yml content: one OpenAI-compatible provider for the LLM router.

        Emitted as JSON, which is valid YAML, so no YAML library is needed.
        """
        return {
            "providers": {
                PROVIDER: {
                    "baseUrl": self.api_base,
                    "apiKey": self.api_key,
                    "api": "openai-completions",
                    "models": [{
                        "id": self.model,
                        "name": self.model,
                        "input": ["text"],
                        "contextWindow": self.context_window,
                        "maxTokens": self.max_tokens,
                        "cost": {"input": 0, "output": 0, "cacheRead": 0, "cacheWrite": 0},
                    }],
                }
            }
        }

    @property
    def overlay_path(self):
        return os.path.join(self.agent_dir, "gateway-overlay.yml")

    def settings_overlay(self):
        """Settings passed with --config (omp's own config.yml is left alone).

        omp retries a failing provider ten times with backoff up to five
        minutes — sensible at a terminal, but here it would hold the agent's
        single slot for minutes on a dead backend. Fail fast instead: the
        gateway owns retries (task API) and reports the error to the client.
        """
        return {"retry": {"maxRetries": self.max_retries, "maxDelayMs": self.retry_max_delay_ms}}

    def mcp_config(self):
        """mcp.json content, or None when shared memory is off / unusable."""
        if self.hindsight_url:
            # Hindsight's per-bank MCP endpoint (streamable HTTP), through the
            # stdio bridge like mnemosyne below.
            env = {}
            if self.hindsight_token:
                env["MCP_BRIDGE_AUTHORIZATION"] = f"Bearer {self.hindsight_token}"
            return {
                "mcpServers": {
                    "memory": {
                        "type": "stdio",
                        "command": self.bridge_python,
                        "args": [self.bridge_script, "--http",
                                 f"{self.hindsight_url}/mcp/{self.hindsight_bank}/"],
                        "env": env,
                    }
                }
            }
        if not self.mnemosyne_url:
            return None
        if not self.mnemosyne_token:
            logger.warning("MNEMOSYNE_ENABLED but MNEMOSYNE_TOKEN is empty; skipping MCP config (would 401)")
            return None
        # Through the stdio bridge rather than omp's own SSE client, which is
        # reset mid-handshake from inside the VM (see mcp_sse_bridge.py). The
        # token goes in the child's environment, not argv.
        return {
            "mcpServers": {
                "mnemosyne": {
                    "type": "stdio",
                    "command": self.bridge_python,
                    "args": [self.bridge_script, self.mnemosyne_url],
                    "env": {"MCP_BRIDGE_AUTHORIZATION": f"Bearer {self.mnemosyne_token}"},
                }
            }
        }

    def write_files(self):
        os.makedirs(self.agent_dir, exist_ok=True)
        os.makedirs(self.sessions_root, exist_ok=True)
        self._write_private(os.path.join(self.agent_dir, "models.yml"), self.models_config())
        self._write_private(self.overlay_path, self.settings_overlay())
        mcp_path = os.path.join(self.agent_dir, "mcp.json")
        mcp = self.mcp_config()
        if mcp is not None:
            self._write_private(mcp_path, mcp)
        elif os.path.exists(mcp_path):
            os.remove(mcp_path)  # memory was switched off since the last boot

    @staticmethod
    def _write_private(path, obj):
        # These carry the LLM key / memory token: owner-only.
        tmp = path + ".tmp"
        fd = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
        with os.fdopen(fd, "w") as f:
            json.dump(obj, f, indent=2)
            f.write("\n")
        os.replace(tmp, path)

    def child_env(self):
        """Environment for the omp process."""
        env = dict(self._env)
        if self.mcp_config() is not None:
            # Headless omp gives MCP servers only 250 ms to hand over their
            # tools before the session starts without them; the shared memory
            # server is a process spawn and a network hop away, so wait for it.
            env["OMP_MCP_STARTUP_TIMEOUT_MS"] = str(self.mcp_startup_timeout_ms)
        return env

    def command(self, session_id):
        args = [
            self.bin, "--mode", "rpc",
            "--cwd", self.workspace,
            "--model", f"{PROVIDER}/{self.model}",
            # Ask before every exec-tier tool; _decide_approval answers.
            "--approval-mode", "write",
            "--config", self.overlay_path,
            "--session-dir", os.path.join(self.sessions_root, _safe_session_dirname(session_id)),
            "--continue",
        ]
        if self.system_prompt_file and os.path.isfile(self.system_prompt_file):
            args += ["--append-system-prompt", self.system_prompt_file]
        return args + self.extra_args


class TurnResult:
    __slots__ = ("text", "usage")

    def __init__(self, text, usage):
        self.text = text
        self.usage = usage


class OmpSession:
    """One `omp --mode rpc` process and the conversation it holds."""

    def __init__(self, config, session_id):
        self.config = config
        self.session_id = session_id
        self.proc = None
        self.last_used = time.monotonic()
        self._lock = asyncio.Lock()   # one turn at a time per conversation
        self._stderr_task = None

    @property
    def busy(self):
        return self._lock.locked()

    def alive(self):
        return self.proc is not None and self.proc.returncode is None

    # ---- process lifecycle ----
    async def _start(self):
        cmd = self.config.command(self.session_id)
        os.makedirs(cmd[cmd.index("--session-dir") + 1], exist_ok=True)
        logger.info("starting omp for session %s", self.session_id)
        self.proc = await asyncio.create_subprocess_exec(
            *cmd,
            stdin=asyncio.subprocess.PIPE,
            stdout=asyncio.subprocess.PIPE,
            stderr=asyncio.subprocess.PIPE,
            limit=_FRAME_LIMIT,
            env=self.config.child_env(),
        )
        self._stderr_task = asyncio.ensure_future(self._drain_stderr(self.proc))
        try:
            await asyncio.wait_for(self._wait_ready(), self.config.start_timeout_s)
        except Exception:
            await self.close(kill=True)
            raise

    async def _wait_ready(self):
        while True:
            frame = await self._read_frame()
            if frame.get("type") == "ready":
                return

    async def _drain_stderr(self, proc):
        try:
            async for raw in proc.stderr:
                line = raw.decode("utf-8", "replace").rstrip()
                if line:
                    logger.info("omp[%s] %s", self.session_id, line[:500])
        except Exception:  # noqa: BLE001 — logging only
            pass

    async def close(self, kill=False):
        proc, self.proc = self.proc, None
        if proc is None:
            return
        if proc.returncode is None:
            try:
                if kill:
                    proc.kill()
                else:
                    # Closing stdin is omp's graceful shutdown: it drains,
                    # persists the session and exits 0.
                    proc.stdin.close()
                    try:
                        await asyncio.wait_for(proc.wait(), 15)
                    except asyncio.TimeoutError:
                        proc.kill()
                await proc.wait()
            except ProcessLookupError:
                pass
        if self._stderr_task is not None:
            self._stderr_task.cancel()
            self._stderr_task = None

    # ---- framing ----
    async def _read_frame(self):
        while True:
            try:
                raw = await self.proc.stdout.readline()
            except (ValueError, asyncio.LimitOverrunError) as e:
                raise OmpError(f"omp emitted an oversized frame: {e}") from e
            if not raw:
                code = self.proc.returncode
                raise OmpError(f"omp exited unexpectedly (code {code})")
            line = raw.strip()
            if not line:
                continue
            try:
                frame = json.loads(line)
            except ValueError:
                logger.debug("omp[%s] non-JSON stdout: %r", self.session_id, line[:200])
                continue
            if isinstance(frame, dict):
                return frame

    async def _send(self, obj):
        self.proc.stdin.write((json.dumps(obj) + "\n").encode("utf-8"))
        await self.proc.stdin.drain()

    # ---- one turn ----
    async def run_turn(self, message, on_delta=None):
        """Send one user message and wait for the agent to yield.

        on_delta(text) is called for each streamed chunk of assistant text.
        Returns a TurnResult. Raises OmpError on a failed turn; on cancellation
        the in-flight turn is aborted in omp before CancelledError propagates.
        """
        async with self._lock:
            self.last_used = time.monotonic()
            if not self.alive():
                await self._start()
            prompt_id = "p-" + uuid.uuid4().hex
            turn = _Turn(on_delta)
            try:
                await self._send({"id": prompt_id, "type": "prompt", "message": message})
                await self._pump(prompt_id, turn)
            except asyncio.CancelledError:
                await self._abort(prompt_id)
                raise
            except OmpError:
                await self.close(kill=True)  # next turn respawns and resumes
                raise
            finally:
                self.last_used = time.monotonic()
            return turn.result()

    async def _pump(self, prompt_id, turn):
        while True:
            frame = await self._read_frame()
            ftype = frame.get("type")
            if ftype == "message_update":
                turn.on_update(frame.get("assistantMessageEvent") or {})
            elif ftype == "message_end":
                turn.on_message_end(frame.get("message") or {})
            elif ftype == "tool_execution_start":
                turn.on_tool_start(frame)
            elif ftype == "tool_execution_end":
                turn.on_tool_end(frame)
            elif ftype == "extension_ui_request":
                await self._answer_ui(frame, turn)
            elif ftype == "response" and frame.get("id") == prompt_id:
                if not frame.get("success", False):
                    raise OmpError(frame.get("error") or "omp rejected the prompt")
                if (frame.get("data") or {}).get("agentInvoked") is False:
                    return  # handled locally (e.g. a slash command); no prompt_result follows
            elif ftype == "prompt_result" and frame.get("id") == prompt_id:
                status = frame.get("status")
                if status == "error":
                    err = frame.get("error") or {}
                    raise OmpError(err.get("message") or "omp turn failed")
                if status == "aborted":
                    raise OmpError("omp turn was aborted")
                return

    async def _abort(self, prompt_id):
        """Stop the in-flight turn so the process is reusable; else kill it."""
        if not self.alive():
            return
        try:
            await self._send({"type": "abort"})

            async def _drain():
                while True:
                    frame = await self._read_frame()
                    if frame.get("type") == "extension_ui_request" and frame.get("method") in _DIALOG_METHODS:
                        await self._send({"type": "extension_ui_response", "id": frame.get("id"), "cancelled": True})
                    if frame.get("type") == "prompt_result" and frame.get("id") == prompt_id:
                        return

            await asyncio.wait_for(_drain(), 20)
        except Exception:  # noqa: BLE001 — any failure here means: don't reuse it
            await self.close(kill=True)

    # ---- dialogs / approvals ----
    async def _answer_ui(self, frame, turn):
        method = frame.get("method")
        if method not in _DIALOG_METHODS:
            return  # presentation update: nothing to answer
        rid = frame.get("id")
        title = frame.get("title") or ""
        match = _APPROVAL_TITLE.match(title)
        options = frame.get("options") or []
        if method == "select" and match and _APPROVE in options:
            choice = await self._decide_approval(match.group(1), turn)
            await self._send({"type": "extension_ui_response", "id": rid, "value": choice})
            return
        # Any other dialog has no human behind it here; decline rather than hang.
        logger.info("omp[%s] declining unsupported %s dialog: %s", self.session_id, method, title[:120])
        await self._send({"type": "extension_ui_response", "id": rid, "cancelled": True})

    async def _decide_approval(self, tool, turn):
        """Approve / Deny one exec-tier tool call.

        Mirrors ApprovalShellBackend: only a dangerous shell command, inside a
        run, is put to the client; everything else runs (the VM is the sandbox,
        the gate is a UX affordance for the webui).
        """
        run = approvals.current_run.get()
        command = turn.pending_command(tool)
        if run is None or tool != "bash" or not command or not approvals.is_dangerous(command):
            return _APPROVE
        # request_approval blocks on a threading.Event until the client answers;
        # keep the loop (and the events SSE that carries the request) running.
        loop = asyncio.get_running_loop()
        choice = await loop.run_in_executor(None, run.request_approval, command)
        if choice == "deny":
            turn.denied.add(turn.pending_call_id(tool))
            return _DENY
        return _APPROVE


class _Turn:
    """Accumulates one turn's streamed text, usage and tool activity."""

    def __init__(self, on_delta):
        self._on_delta = on_delta
        self._streamed = []          # every text delta, in order
        self._started = False        # has non-whitespace text been emitted yet
        self._final_text = ""        # text of the last assistant message
        self._usage = {"input_tokens": 0, "output_tokens": 0}
        self._tools = OrderedDict()  # toolCallId -> {"name", "command"}; in flight
        self.denied = set()

    # -- assistant text --
    def on_update(self, ev):
        if ev.get("type") != "text_delta":
            return  # thinking / tool-call argument deltas are not answer text
        delta = ev.get("delta") or ""
        if not self._started:
            # Models often open with blank lines before the first word.
            delta = delta.lstrip()
            if not delta:
                return
            self._started = True
        self._streamed.append(delta)
        if self._on_delta is not None:
            self._on_delta(delta)

    def on_message_end(self, message):
        if message.get("role") != "assistant":
            return
        usage = message.get("usage") or {}
        self._usage["input_tokens"] += int(usage.get("input") or 0)
        self._usage["output_tokens"] += int(usage.get("output") or 0)
        text = "".join(
            part.get("text") or ""
            for part in (message.get("content") or [])
            if isinstance(part, dict) and part.get("type") == "text"
        ).strip()
        if text:
            self._final_text = text

    # -- tools (run events + approval context) --
    def on_tool_start(self, frame):
        call_id = frame.get("toolCallId")
        name = frame.get("toolName") or ""
        args = frame.get("args") or {}
        command = args.get("command") if isinstance(args, dict) else None
        self._tools[call_id] = {"name": name, "command": command if isinstance(command, str) else None}
        run = approvals.current_run.get()
        if run is not None:
            preview = (command if isinstance(command, str) else frame.get("intent") or "")[:200]
            run.emit({"event": "tool.started", "tool": _tool_label(name), "preview": preview})

    def on_tool_end(self, frame):
        call_id = frame.get("toolCallId")
        info = self._tools.pop(call_id, None) or {"name": frame.get("toolName") or ""}
        run = approvals.current_run.get()
        if run is not None:
            event = {"event": "tool.completed", "tool": _tool_label(info["name"]),
                     "error": bool(frame.get("isError"))}
            if call_id in self.denied:
                event["denied"] = True
            run.emit(event)

    def _pending(self, tool):
        # The approval prompt follows its tool_execution_start; with several
        # calls in flight, the most recent one for that tool is the one asking.
        for call_id in reversed(self._tools):
            if self._tools[call_id]["name"] == tool:
                return call_id, self._tools[call_id]
        return None, {}

    def pending_command(self, tool):
        return self._pending(tool)[1].get("command")

    def pending_call_id(self, tool):
        return self._pending(tool)[0]

    def result(self):
        # Prefer the last assistant message (the answer after any tool use);
        # fall back to everything streamed if it carried no text.
        text = self._final_text or "".join(self._streamed).strip()
        usage = dict(self._usage)
        usage["total_tokens"] = usage["input_tokens"] + usage["output_tokens"]
        return TurnResult(text, usage)


def _tool_label(name):
    # The runs-API events call the shell tool "shell" for every harness.
    return "shell" if name == "bash" else (name or "tool")


def _last_user_text(agent_input):
    """The newest user message from a LangGraph-style {"messages": [...]} input."""
    messages = agent_input.get("messages") if isinstance(agent_input, dict) else None
    for m in reversed(messages or []):
        role = m.get("role") if isinstance(m, dict) else getattr(m, "type", None)
        if role in ("user", "human"):
            content = m.get("content") if isinstance(m, dict) else getattr(m, "content", "")
            if isinstance(content, list):  # multimodal parts: keep the text
                content = "".join(
                    p.get("text", "") if isinstance(p, dict) else str(p) for p in content
                )
            return str(content or "")
    return ""


def _final_state(result):
    from langchain_core.messages import AIMessage
    return {"messages": [AIMessage(content=result.text, usage_metadata=result.usage)]}


class OmpAgent:
    """LangGraph-shaped facade over a pool of per-session omp processes."""

    def __init__(self, config=None):
        self.config = config or OmpConfig()
        self.config.write_files()
        self._sessions = OrderedDict()  # session_id -> OmpSession, LRU order
        self._pool_lock = None          # created lazily on the serving loop
        self._reaper = None

    # ---- gateway_server.py surface ----
    def for_session(self, session_id):
        """Bind the agent to one conversation (X-Hermes-Session-Id)."""
        return _BoundOmpAgent(self, session_id or DEFAULT_SESSION)

    async def ainvoke(self, agent_input, **_kwargs):
        return await self.for_session(DEFAULT_SESSION).ainvoke(agent_input)

    def astream(self, agent_input, **kwargs):
        return self.for_session(DEFAULT_SESSION).astream(agent_input, **kwargs)

    # ---- pool ----
    async def _session(self, session_id):
        if self._pool_lock is None:
            self._pool_lock = asyncio.Lock()
        async with self._pool_lock:
            if self._reaper is None:
                self._reaper = asyncio.ensure_future(self._reap_idle())
            session = self._sessions.get(session_id)
            if session is None:
                session = OmpSession(self.config, session_id)
                self._sessions[session_id] = session
            self._sessions.move_to_end(session_id)
            session.last_used = time.monotonic()
            await self._evict_locked(keep=session_id)
            return session

    async def _evict_locked(self, keep):
        """Close least-recently-used idle processes beyond the cap.

        The conversation is on disk, so an evicted session simply resumes in a
        fresh process on its next turn. Busy sessions are never evicted.
        """
        for sid in list(self._sessions):
            if len(self._sessions) <= self.config.max_sessions:
                break
            victim = self._sessions[sid]
            if sid == keep or victim.busy:
                continue
            logger.info("evicting idle omp session %s (cap %d)", sid, self.config.max_sessions)
            del self._sessions[sid]
            await victim.close()

    async def _reap_idle(self):
        while True:
            await asyncio.sleep(60)
            try:
                async with self._pool_lock:
                    cutoff = time.monotonic() - self.config.idle_timeout_s
                    for sid in list(self._sessions):
                        session = self._sessions[sid]
                        if not session.busy and session.last_used < cutoff:
                            logger.info("closing idle omp session %s", sid)
                            del self._sessions[sid]
                            await session.close()
            except Exception:  # noqa: BLE001 — never let the reaper die
                logger.debug("omp idle reaper error", exc_info=True)

    async def aclose(self):
        if self._reaper is not None:
            self._reaper.cancel()
        for session in list(self._sessions.values()):
            await session.close()
        self._sessions.clear()


class _BoundOmpAgent:
    """An OmpAgent pinned to one session id."""

    def __init__(self, agent, session_id):
        self._agent = agent
        self._session_id = session_id

    def for_session(self, session_id):
        return self._agent.for_session(session_id)

    async def ainvoke(self, agent_input, **_kwargs):
        session = await self._agent._session(self._session_id)
        result = await session.run_turn(_last_user_text(agent_input))
        return _final_state(result)

    async def astream(self, agent_input, stream_mode=None, **_kwargs):
        """Yield ("messages", (chunk, meta)) per text delta, then ("values", state)."""
        from langchain_core.messages import AIMessageChunk

        session = await self._agent._session(self._session_id)
        queue = asyncio.Queue()
        done = object()

        async def _run():
            try:
                return await session.run_turn(_last_user_text(agent_input), on_delta=queue.put_nowait)
            finally:
                queue.put_nowait(done)

        task = asyncio.ensure_future(_run())
        try:
            while True:
                item = await queue.get()
                if item is done:
                    break
                yield "messages", (AIMessageChunk(content=item), {})
            yield "values", _final_state(await task)
        finally:
            if not task.done():
                task.cancel()  # consumer went away: abort the turn in omp
                try:
                    await task
                except BaseException:  # noqa: BLE001 — cancellation or turn error
                    pass


def create_omp_agent(callbacks=None):
    """Factory matching agent.create_agent(); LangChain callbacks do not apply."""
    config = OmpConfig()
    logger.info("Creating omp agent (model %s/%s via %s)", PROVIDER, config.model, config.api_base)
    return OmpAgent(config)
