#!/usr/bin/env python3
"""
In-VM OpenAI-compatible gateway server — DeepAgents harness adapter.

This is the *deepagents* harness adapter: a plain stdlib HTTP server that exposes
the Hermes Gateway wire contract on 0.0.0.0:GATEWAY_PORT (default 8642) INSIDE the
Firecracker VM, and fulfils each request by driving the DeepAgents agent built by
agent.create_agent(). The bare-metal Go router reverse-proxies (streaming SSE) to
this server at <vm_ip>:8642.

Other harnesses may REPLACE this file as long as they honour the SAME :8642 wire
contract (OpenAI-compatible /v1/chat/completions SSE, /v1/models, /v1/capabilities,
/health and the per-session multi-turn continuity keyed by X-Hermes-Session-Id).

The request 'model' field is the AGENT ID (routing key on the host), NOT the LLM
model. Inference always uses this VM's configured LLM (LLM_MODEL env, e.g. gemma)
via the agent built in agent.py; we never forward the request 'model' to the LLM.

Configuration (from /etc/agent.conf via the environment):
  GATEWAY_PORT    - bind port (default 8642)
  API_SERVER_KEY  - downstream bearer; empty => no auth required on /v1/*
  AGENT_TYPE      - advertised model id on /v1/models
  LLM_*           - consumed by agent.create_agent()
"""

import asyncio
import json
import logging
import os
import threading
import time
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

# Importing agent.py configures root logging (file + stdout) and gives us the
# DeepAgents factory + tracing init. agent.py lives in the same directory and is
# on sys.path[0] because start.sh execs this file by absolute path.
from agent import create_agent, init_tracing

# Guarded OTel imports: with opentelemetry absent every tracing hook below
# degrades to a no-op and the wire behaviour is unchanged.
try:
    from opentelemetry import context as otel_context
    from opentelemetry import trace as otel_trace
    from opentelemetry.propagate import extract as _otel_propagate_extract
    from opentelemetry.trace import SpanKind as _OtelSpanKind
    _OTEL_AVAILABLE = True
except Exception:
    otel_context = otel_trace = _otel_propagate_extract = _OtelSpanKind = None
    _OTEL_AVAILABLE = False

logger = logging.getLogger("gateway")


def _otel_carrier(headers):
    """Build a LOWERCASED header carrier for traceparent extraction.

    Load-bearing: the Go router canonicalizes the header as "Traceparent",
    while the OTel dict getter does an exact-key lookup for "traceparent".
    Without lowercasing, extraction silently yields no parent and the in-VM
    spans detach from the gateway trace.
    """
    return {k.lower(): v for k, v in headers.items()}


def _otel_extract(carrier):
    """Extract the parent trace context from a (lowercased) carrier.

    Returns an OTel Context, or None when opentelemetry is unavailable or
    extraction fails.
    """
    if not _OTEL_AVAILABLE:
        return None
    try:
        return _otel_propagate_extract(carrier)
    except Exception:
        logger.warning("traceparent extraction failed", exc_info=True)
        return None

# ---------------------------------------------------------------------------
# Config (read once at startup)
# ---------------------------------------------------------------------------
GATEWAY_PORT = int(os.environ.get("GATEWAY_PORT", "8642") or "8642")
API_SERVER_KEY = os.environ.get("API_SERVER_KEY", "") or ""
AGENT_TYPE = os.environ.get("AGENT_TYPE", "generic")
DEFAULT_SESSION = "default"

# ---------------------------------------------------------------------------
# Per-session conversation history. The webui client does NOT resend prior turns;
# multi-turn continuity is THIS server's job, keyed by X-Hermes-Session-Id. We
# store only user/assistant turns (the agent supplies its own system prompt).
#
# Bounded: at most MAX_SESSIONS sessions (LRU-evicted by last access) and at
# most MAX_HISTORY_MESSAGES messages per session (oldest whole user/assistant
# pairs trimmed first), so long-lived VMs don't grow session memory unbounded.
# ---------------------------------------------------------------------------
MAX_SESSIONS = 200
MAX_HISTORY_MESSAGES = 80

SESSIONS = {}                      # sid -> [ {"role","content"}, ... ]
SESSIONS_LAST_ACCESS = {}          # sid -> time.monotonic() of last touch (LRU)
SESSIONS_LOCK = threading.Lock()


def _session_history(sid):
    """Snapshot a session's history and mark it recently used."""
    with SESSIONS_LOCK:
        if sid in SESSIONS:
            SESSIONS_LAST_ACCESS[sid] = time.monotonic()
        return list(SESSIONS.get(sid, []))


def _session_append(sid, history, new_msgs):
    """Append a completed turn to a session, then trim/evict past the caps.

    Re-reads the live history under the lock (rather than overwriting with the
    caller's stale snapshot) so a concurrent turn on the same session is not
    silently lost.
    """
    with SESSIONS_LOCK:
        SESSIONS[sid] = SESSIONS.get(sid, history) + new_msgs
        SESSIONS_LAST_ACCESS[sid] = time.monotonic()
        _trim_history_locked(sid)
        _evict_sessions_locked()


def _trim_history_locked(sid):
    """Cap a session at the most recent MAX_HISTORY_MESSAGES messages.

    Trims oldest first, always whole user/assistant pairs. Caller holds
    SESSIONS_LOCK.
    """
    history = SESSIONS.get(sid) or []
    if len(history) <= MAX_HISTORY_MESSAGES:
        return
    drop = len(history) - MAX_HISTORY_MESSAGES
    if drop % 2:
        drop += 1  # never split a user/assistant pair
    SESSIONS[sid] = history[drop:]
    logger.info("session %s history trimmed: dropped %d oldest message(s), %d kept",
                sid, drop, len(SESSIONS[sid]))


def _evict_sessions_locked():
    """LRU-evict sessions beyond MAX_SESSIONS. Caller holds SESSIONS_LOCK."""
    while len(SESSIONS) > MAX_SESSIONS:
        victim = min(SESSIONS, key=lambda s: SESSIONS_LAST_ACCESS.get(s, 0.0))
        SESSIONS.pop(victim, None)
        SESSIONS_LAST_ACCESS.pop(victim, None)
        logger.info("session %s evicted (LRU, over cap of %d sessions)",
                    victim, MAX_SESSIONS)

# Lazily-built, reused DeepAgents agent.
_AGENT = None
_AGENT_LOCK = threading.Lock()

# LangChain tracing callbacks from init_tracing(); set once in main().
_TRACING_CALLBACKS = []


def get_agent():
    """Build the DeepAgents agent once, then reuse it for every request."""
    global _AGENT
    with _AGENT_LOCK:
        if _AGENT is None:
            _AGENT = create_agent(callbacks=_TRACING_CALLBACKS)
        return _AGENT


# ---------------------------------------------------------------------------
# Persistent async event loop.
#
# The DeepAgents agent wraps a langchain-openai ChatOpenAI whose underlying
# openai.AsyncOpenAI / httpx.AsyncClient connection pool binds to the FIRST
# event loop it runs on. We must therefore drive every request from ONE
# long-lived loop: a per-request asyncio.run() closes its loop on return, so the
# next turn (a different loop, in a different ThreadingHTTPServer thread) would
# reuse a pool bound to a closed loop and raise "Event loop is closed". A single
# shared loop keeps the async client valid across multi-turn and concurrent
# requests. Each handler thread submits its coroutine with
# run_coroutine_threadsafe() and blocks on the resulting future.
# ---------------------------------------------------------------------------
_LOOP = None
_LOOP_LOCK = threading.Lock()


def get_loop():
    """Start (once) a dedicated background thread running a persistent event loop."""
    global _LOOP
    with _LOOP_LOCK:
        if _LOOP is None:
            loop = asyncio.new_event_loop()

            def _run():
                asyncio.set_event_loop(loop)
                loop.run_forever()

            threading.Thread(target=_run, name="agent-loop", daemon=True).start()
            _LOOP = loop
        return _LOOP


def run_coro(coro):
    """Run a coroutine on the persistent loop, blocking the caller for its result.

    Exceptions raised inside the coroutine propagate here (via Future.result()),
    so the existing per-handler try/except blocks behave exactly as before.
    """
    return asyncio.run_coroutine_threadsafe(coro, get_loop()).result()


async def _traced(coro, carrier, attrs):
    """Await `coro` inside a SERVER "agent.chat" span.

    The span parents on the router's inbound traceparent, extracted from the
    LOWERCASED header `carrier`. This wrapper runs INSIDE the loop coroutine:
    contextvars are captured per run_coroutine_threadsafe submission, so the
    attach()ed parent context and the current span cannot cross-contaminate
    concurrent sessions sharing the persistent loop. With opentelemetry absent
    it awaits `coro` untouched.
    """
    if not _OTEL_AVAILABLE:
        return await coro
    parent_ctx = _otel_extract(carrier)
    # Parent the SERVER span by passing the inbound context to
    # start_as_current_span(context=...) rather than attach()ing it ourselves.
    # The agent invocation inside `await coro` spawns its own async tasks and
    # mutates the OTel context, so a manual attach()/detach() pair straddling the
    # await raised "Token was created in a different Context" at detach time —
    # non-fatal, but it logged a traceback on every request. Scoping the parent
    # to the span's own context manager removes the stray token entirely.
    tracer = otel_trace.get_tracer("hermes.gateway_server")
    with tracer.start_as_current_span("agent.chat", context=parent_ctx,
                                      kind=_OtelSpanKind.SERVER, attributes=attrs):
        return await coro


# ---------------------------------------------------------------------------
# Content / result helpers
# ---------------------------------------------------------------------------
def _content_to_text(content) -> str:
    """Flatten LangChain message content (str or multimodal list) to plain text."""
    if content is None:
        return ""
    if isinstance(content, str):
        return content
    if isinstance(content, list):
        parts = []
        for c in content:
            if isinstance(c, str):
                parts.append(c)
            elif isinstance(c, dict):
                t = c.get("text")
                if isinstance(t, str):
                    parts.append(t)
        return "".join(parts)
    return str(content)


def _final_content(result) -> str:
    """Extract the assistant's final text from an agent.ainvoke() result."""
    try:
        from langchain_core.messages import AIMessage
    except Exception:
        AIMessage = None
    msgs = result.get("messages", []) if isinstance(result, dict) else []
    for m in reversed(msgs):
        is_ai = (AIMessage is not None and isinstance(m, AIMessage)) or getattr(m, "type", None) == "ai"
        if is_ai:
            text = _content_to_text(getattr(m, "content", ""))
            if text:
                return text
    if msgs:
        return _content_to_text(getattr(msgs[-1], "content", ""))
    return ""


def _extract_usage(result):
    """Best-effort token usage from the final AI message; None if unavailable."""
    try:
        msgs = result.get("messages", []) if isinstance(result, dict) else []
        for m in reversed(msgs):
            um = getattr(m, "usage_metadata", None)
            if um:
                return {
                    "prompt_tokens": um.get("input_tokens", 0) or 0,
                    "completion_tokens": um.get("output_tokens", 0) or 0,
                }
    except Exception:
        pass
    return None


def _chunk(cid, created, model, delta, finish):
    """Build one OpenAI chat.completion.chunk object."""
    return {
        "id": cid,
        "object": "chat.completion.chunk",
        "created": created,
        "model": model,
        "choices": [{"index": 0, "delta": delta, "finish_reason": finish}],
    }


# ---------------------------------------------------------------------------
# HTTP handler
# ---------------------------------------------------------------------------
class GatewayHandler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    server_version = "hermes-gateway-vm/1.0"

    # Route BaseHTTPRequestHandler's access log through our logger so it lands in
    # /var/log/agent.log + stdout instead of bare stderr.
    def log_message(self, fmt, *args):
        logger.info("%s %s", self.address_string(), fmt % args)

    # ---- low-level response helpers ----
    def _send_json(self, status, obj):
        data = json.dumps(obj).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        try:
            self.wfile.write(data)
        except (BrokenPipeError, ConnectionError):
            pass

    def _begin_sse(self):
        """Send SSE response headers; body is delimited by connection close."""
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-cache")
        self.send_header("Connection", "close")
        self.end_headers()
        self.close_connection = True
        self.wfile.flush()

    def _sse_send(self, obj):
        self.wfile.write(("data: " + json.dumps(obj) + "\n\n").encode("utf-8"))
        self.wfile.flush()

    def _sse_done(self):
        self.wfile.write(b"data: [DONE]\n\n")
        self.wfile.flush()

    # ---- auth ----
    def _authed(self):
        """Enforce the downstream bearer on /v1/* when API_SERVER_KEY is set."""
        if not API_SERVER_KEY:
            return True
        auth = self.headers.get("Authorization", "")
        if auth.startswith("Bearer ") and auth[len("Bearer "):].strip() == API_SERVER_KEY:
            return True
        self._send_json(401, {"error": {"message": "Invalid API key", "type": "invalid_request_error"}})
        return False

    # ---- routing ----
    def do_GET(self):
        path = self.path.split("?", 1)[0]
        if path == "/health":
            self._send_json(200, {"status": "ok"})
            return
        # /v1/capabilities is intentionally unauthenticated (matches the router and
        # the documented contract: the webui probes it before presenting a key).
        if path == "/v1/capabilities":
            self._send_json(200, {"features": {"approval_events": False, "run_approval_response": False}})
            return
        if path.startswith("/v1/") and not self._authed():
            return
        if path == "/v1/models":
            self._send_json(200, {"object": "list", "data": [
                {"id": AGENT_TYPE, "object": "model", "owned_by": "hermes-gateway"}]})
            return
        self._send_json(404, {"error": {"message": "Not found", "type": "invalid_request_error"}})

    def do_POST(self):
        path = self.path.split("?", 1)[0]
        if path.startswith("/v1/") and not self._authed():
            return
        if path != "/v1/chat/completions":
            self._send_json(404, {"error": {"message": "Not found", "type": "invalid_request_error"}})
            return
        self._handle_chat()

    # ---- chat completions ----
    def _handle_chat(self):
        try:
            length = int(self.headers.get("Content-Length", 0) or 0)
        except ValueError:
            length = 0
        raw = self.rfile.read(length) if length > 0 else b""
        try:
            body = json.loads(raw.decode("utf-8")) if raw else {}
        except (ValueError, UnicodeDecodeError):
            self._send_json(400, {"error": {"message": "Invalid JSON body", "type": "invalid_request_error"}})
            return

        messages = body.get("messages") or []
        stream = bool(body.get("stream", False))
        req_model = body.get("model") or ""
        # Echo a sensible model id; 'model' is the agent id, not the LLM model.
        resp_model = AGENT_TYPE if req_model in ("", "default") else req_model

        # Pull the latest user message. System message(s) are supplied by the
        # agent's own /etc/agent/system-prompt.md, so we do not re-inject them.
        user_msgs = [m for m in messages if isinstance(m, dict) and m.get("role") == "user"]
        if not user_msgs:
            msg = "No user message in request"
            if stream:
                self._begin_sse()
                created = int(time.time())
                cid = "chatcmpl-" + uuid.uuid4().hex
                try:
                    self._sse_send(_chunk(cid, created, resp_model, {"role": "assistant"}, None))
                    self._sse_send(_chunk(cid, created, resp_model, {"content": f"[error] {msg}"}, None))
                    self._sse_send(_chunk(cid, created, resp_model, {}, "stop"))
                    self._sse_done()
                except (BrokenPipeError, ConnectionError):
                    pass
            else:
                self._send_json(400, {"error": {"message": msg, "type": "invalid_request_error"}})
            return

        new_user = {"role": "user", "content": user_msgs[-1].get("content", "")}

        # Snapshot this session's history; build agent input = history + [new user].
        sid = self.headers.get("X-Hermes-Session-Id") or DEFAULT_SESSION
        history = _session_history(sid)
        agent_input = {"messages": history + [new_user]}

        logger.info("chat session=%s agent=%s stream=%s history_turns=%d",
                    sid, resp_model, stream, len(history))

        # Trace context: lowercased carrier (the Go router sends "Traceparent")
        # plus the SERVER-span attributes _traced() puts on "agent.chat".
        carrier = _otel_carrier(self.headers)
        span_attrs = {
            "hermes.session_id": sid,
            "hermes.agent": resp_model,
            "hermes.stream": stream,
            "hermes.history_turns": len(history),
        }

        try:
            ag = get_agent()
        except Exception as e:
            logger.exception("agent build failed")
            if stream:
                self._begin_sse()
                created = int(time.time())
                cid = "chatcmpl-" + uuid.uuid4().hex
                try:
                    self._sse_send(_chunk(cid, created, resp_model, {"role": "assistant"}, None))
                    self._sse_send(_chunk(cid, created, resp_model, {"content": f"[error] {e}"}, None))
                    self._sse_send(_chunk(cid, created, resp_model, {}, "stop"))
                    self._sse_done()
                except (BrokenPipeError, ConnectionError):
                    pass
            else:
                self._send_json(500, {"error": {"message": str(e), "type": "internal_error"}})
            return

        if stream:
            assistant_text = self._do_stream(ag, agent_input, resp_model, carrier, span_attrs)
        else:
            assistant_text = self._do_blocking(ag, agent_input, resp_model, carrier, span_attrs)

        if assistant_text is None:
            return  # non-stream error already written

        # Append this turn so the next request on the session continues it.
        _session_append(sid, history,
                        [new_user, {"role": "assistant", "content": assistant_text}])

    def _do_blocking(self, ag, agent_input, resp_model, carrier, span_attrs):
        created = int(time.time())
        cid = "chatcmpl-" + uuid.uuid4().hex
        try:
            result = run_coro(_traced(ag.ainvoke(agent_input), carrier, span_attrs))
        except Exception as e:
            logger.exception("agent invoke error")
            self._send_json(500, {"error": {"message": str(e), "type": "internal_error"}})
            return None
        content = _final_content(result)
        usage = _extract_usage(result)
        resp = {
            "id": cid,
            "object": "chat.completion",
            "created": created,
            "model": resp_model,
            "choices": [{
                "index": 0,
                "message": {"role": "assistant", "content": content},
                "finish_reason": "stop",
            }],
        }
        if usage:
            resp["usage"] = usage
        self._send_json(200, resp)
        return content

    def _do_stream(self, ag, agent_input, resp_model, carrier, span_attrs):
        created = int(time.time())
        cid = "chatcmpl-" + uuid.uuid4().hex
        try:
            self._begin_sse()
            self._sse_send(_chunk(cid, created, resp_model, {"role": "assistant"}, None))
        except (BrokenPipeError, ConnectionError):
            return ""

        acc = []
        try:
            run_coro(_traced(
                self._astream_agent(ag, agent_input, cid, created, resp_model, acc),
                carrier, span_attrs))
        except (BrokenPipeError, ConnectionError):
            return "".join(acc)  # client went away mid-stream
        except Exception as e:
            logger.exception("agent stream error")
            try:
                self._sse_send(_chunk(cid, created, resp_model, {"content": f"\n[error] {e}"}, None))
            except (BrokenPipeError, ConnectionError):
                return "".join(acc)

        try:
            self._sse_send(_chunk(cid, created, resp_model, {}, "stop"))
            self._sse_done()
        except (BrokenPipeError, ConnectionError):
            pass
        return "".join(acc)

    async def _astream_agent(self, ag, agent_input, cid, created, resp_model, acc):
        """Stream message-chunk content deltas.

        Uses stream_mode=["messages","values"] so we ALSO capture the final graph
        state in the same pass. If no AIMessage content streamed (rare — e.g. the
        model answered only via the final state), we recover the answer from that
        captured state instead of re-running the agent: a second ainvoke would
        re-execute real shell side-effects via LocalShellBackend and double cost.
        """
        from langchain_core.messages import AIMessage
        emitted = False
        last_values = None
        async for mode, payload in ag.astream(agent_input, stream_mode=["messages", "values"]):
            if mode == "messages":
                chunk = payload[0] if isinstance(payload, (tuple, list)) else payload
                if isinstance(chunk, AIMessage):
                    text = _content_to_text(getattr(chunk, "content", ""))
                    if text:
                        emitted = True
                        acc.append(text)
                        self._sse_send(_chunk(cid, created, resp_model, {"content": text}, None))
            elif mode == "values":
                last_values = payload
        if not emitted and last_values is not None:
            text = _final_content(last_values)
            if text:
                acc.append(text)
                self._sse_send(_chunk(cid, created, resp_model, {"content": text}, None))


def main():
    global _TRACING_CALLBACKS
    _TRACING_CALLBACKS = init_tracing() or []
    get_loop()  # start the persistent async loop thread before serving requests
    logger.info("hermes in-VM gateway (deepagents adapter) starting on 0.0.0.0:%d "
                "(agent=%s, auth=%s)", GATEWAY_PORT, AGENT_TYPE,
                "on" if API_SERVER_KEY else "off")
    httpd = ThreadingHTTPServer(("0.0.0.0", GATEWAY_PORT), GatewayHandler)
    httpd.daemon_threads = True
    try:
        httpd.serve_forever()
    except KeyboardInterrupt:
        logger.info("hermes in-VM gateway shutting down")
    finally:
        httpd.server_close()


if __name__ == "__main__":
    main()
