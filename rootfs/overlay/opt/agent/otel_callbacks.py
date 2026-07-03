#!/usr/bin/env python3
"""
Hand-rolled OpenTelemetry callback handler for LangChain/LangGraph.

Replaces opentelemetry-instrumentation-langchain (no extra pip package): a
LangChain BaseCallbackHandler that emits
  - a top-level INTERNAL "agent.graph" span for the outermost chain run,
  - CLIENT "chat <model>" spans with gen_ai.* attrs incl. best-effort token
    usage for each LLM/chat-model call,
  - INTERNAL "tool <name>" spans carrying the tool name and input LENGTH only
    (never tool input/output content).

Parenting: a run's span is parented on its parent_run_id's span when we have
one, else on the current OTel context (so the whole callback tree hangs off
the SERVER "agent.chat" span opened by gateway_server._traced). Spans for
LLM/chat runs are also attach()ed as current so httpx auto-instrumented spans
nest beneath them; the attach token is kept in the run map and detached at end.

All imports are guarded: with opentelemetry (or langchain_core) absent this
module still imports and the handler degrades to a no-op. Tracing must never
break the agent.
"""

import logging
import threading
import time

logger = logging.getLogger("otel_callbacks")

try:
    from langchain_core.callbacks import BaseCallbackHandler
except Exception:  # pragma: no cover - langchain absent (e.g. host-side tests)
    BaseCallbackHandler = object

try:
    from opentelemetry import context as otel_context
    from opentelemetry import trace as otel_trace
    from opentelemetry.trace import SpanKind, Status, StatusCode, set_span_in_context
    _OTEL_AVAILABLE = True
except Exception:  # opentelemetry not installed -> no-op handler
    otel_context = otel_trace = SpanKind = Status = StatusCode = set_span_in_context = None
    _OTEL_AVAILABLE = False


def _model_name(serialized, kwargs):
    """Best-effort model name from LangChain start-callback arguments."""
    params = kwargs.get("invocation_params") or {}
    for key in ("model", "model_name", "model_id"):
        val = params.get(key)
        if val:
            return str(val)
    if isinstance(serialized, dict):
        for key in ("name",):
            val = serialized.get(key)
            if val:
                return str(val)
    return "unknown"


def _usage_from_response(response):
    """Best-effort {prompt, completion} token usage from an LLMResult."""
    try:
        llm_output = getattr(response, "llm_output", None) or {}
        usage = llm_output.get("token_usage") or llm_output.get("usage") or {}
        if usage:
            return usage.get("prompt_tokens"), usage.get("completion_tokens")
        for gens in getattr(response, "generations", None) or []:
            for gen in gens:
                um = getattr(getattr(gen, "message", None), "usage_metadata", None)
                if um:
                    return um.get("input_tokens"), um.get("output_tokens")
    except Exception:
        pass
    return None, None


class OTelCallbackHandler(BaseCallbackHandler):
    """LangChain callback handler emitting OTel spans (see module docstring)."""

    raise_error = False  # tracing bugs must never propagate into the agent
    _SWEEP_INTERVAL_S = 60.0
    _MAX_SPAN_AGE_S = 3600.0

    def __init__(self):
        self._tracer = otel_trace.get_tracer("hermes.otel_callbacks") if _OTEL_AVAILABLE else None
        # run_id -> (span, attach_token_or_None, started_monotonic)
        self._runs = {}
        self._lock = threading.Lock()
        self._last_sweep = time.monotonic()

    # ---- internal span bookkeeping -------------------------------------
    def _parent_ctx(self, parent_run_id):
        """Context of parent_run_id's span, falling back to the current ctx."""
        if parent_run_id is not None:
            with self._lock:
                entry = self._runs.get(parent_run_id)
            if entry is not None:
                return set_span_in_context(entry[0])
        return otel_context.get_current()

    def _start(self, run_id, parent_run_id, name, kind, attrs, make_current=False):
        if self._tracer is None:
            return
        span = self._tracer.start_span(name, context=self._parent_ctx(parent_run_id),
                                       kind=kind, attributes=attrs)
        token = otel_context.attach(set_span_in_context(span)) if make_current else None
        with self._lock:
            self._runs[run_id] = (span, token, time.monotonic())
        self._maybe_sweep()

    def _end(self, run_id, error=None, attrs=None):
        with self._lock:
            entry = self._runs.pop(run_id, None)
        if entry is None:
            return
        span, token, _ = entry
        try:
            if attrs:
                for k, v in attrs.items():
                    if v is not None:
                        span.set_attribute(k, v)
            if error is not None:
                if isinstance(error, BaseException):
                    span.record_exception(error)
                span.set_status(Status(StatusCode.ERROR, str(error)))
        finally:
            if token is not None:
                try:
                    otel_context.detach(token)
                except Exception:
                    pass  # token from another context (async hop) - span still ends
            span.end()

    def _maybe_sweep(self):
        """Leak sweep: end + drop spans whose end callback never arrived."""
        now = time.monotonic()
        if now - self._last_sweep < self._SWEEP_INTERVAL_S:
            return
        with self._lock:
            self._last_sweep = now
            stale = [rid for rid, (_, _, t0) in self._runs.items()
                     if now - t0 > self._MAX_SPAN_AGE_S]
            entries = [self._runs.pop(rid) for rid in stale]
        for span, _token, _t0 in entries:
            # Never detach a stale token: it belongs to some long-gone context.
            span.set_status(Status(StatusCode.ERROR, "span leaked (no end callback)"))
            span.end()
        if entries:
            logger.info("otel_callbacks: swept %d leaked span(s)", len(entries))

    # ---- LangChain callback surface -------------------------------------
    def on_chain_start(self, serialized, inputs, *, run_id, parent_run_id=None, **kwargs):
        # Only the OUTERMOST run gets a span; per-node chain spans are noise.
        if parent_run_id is None:
            self._start(run_id, None, "agent.graph", SpanKind.INTERNAL if _OTEL_AVAILABLE else None, {})

    def on_chain_end(self, outputs, *, run_id, **kwargs):
        self._end(run_id)

    def on_chain_error(self, error, *, run_id, **kwargs):
        self._end(run_id, error=error)

    def _on_model_start(self, serialized, *, run_id, parent_run_id, **kwargs):
        model = _model_name(serialized, kwargs)
        self._start(run_id, parent_run_id, f"chat {model}",
                    SpanKind.CLIENT if _OTEL_AVAILABLE else None,
                    {"gen_ai.operation.name": "chat", "gen_ai.request.model": model},
                    make_current=True)

    def on_chat_model_start(self, serialized, messages, *, run_id, parent_run_id=None, **kwargs):
        self._on_model_start(serialized, run_id=run_id, parent_run_id=parent_run_id, **kwargs)

    def on_llm_start(self, serialized, prompts, *, run_id, parent_run_id=None, **kwargs):
        self._on_model_start(serialized, run_id=run_id, parent_run_id=parent_run_id, **kwargs)

    def on_llm_end(self, response, *, run_id, **kwargs):
        in_tok, out_tok = _usage_from_response(response)
        self._end(run_id, attrs={"gen_ai.usage.input_tokens": in_tok,
                                 "gen_ai.usage.output_tokens": out_tok})

    def on_llm_error(self, error, *, run_id, **kwargs):
        self._end(run_id, error=error)

    def on_tool_start(self, serialized, input_str, *, run_id, parent_run_id=None, **kwargs):
        name = (serialized or {}).get("name") or "unknown"
        # Name + input LENGTH only - tool arguments are sensitive content.
        self._start(run_id, parent_run_id, f"tool {name}",
                    SpanKind.INTERNAL if _OTEL_AVAILABLE else None,
                    {"gen_ai.tool.name": str(name),
                     "hermes.tool.input_length": len(input_str or "")})

    def on_tool_end(self, output, *, run_id, **kwargs):
        self._end(run_id)

    def on_tool_error(self, error, *, run_id, **kwargs):
        self._end(run_id, error=error)
