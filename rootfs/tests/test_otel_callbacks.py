#!/usr/bin/env python3
"""
Host-side tests for rootfs/overlay/opt/agent/otel_callbacks.py.

Run from the repo root:
    PYTHONPATH=rootfs/overlay/opt/agent python3 -m unittest discover -v rootfs/tests

Verifies span parenting (parent_run_id chain + fallback to the current context
under run_coroutine_threadsafe, mirroring gateway_server's loop model), tool
span content hygiene, gen_ai usage attrs, and the leak sweep — all against an
in-memory exporter. Skips cleanly when the opentelemetry SDK is absent.
"""

import asyncio
import threading
import time
import types
import unittest
import uuid

import otel_callbacks

try:
    from opentelemetry import trace
    from opentelemetry.sdk.trace import TracerProvider
    from opentelemetry.sdk.trace.export import SimpleSpanProcessor
    from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter
    from opentelemetry.trace import SpanKind, StatusCode
    _HAVE_SDK = True
except Exception:
    _HAVE_SDK = False


class ImportAlwaysWorksTest(unittest.TestCase):
    def test_handler_constructs_without_sdk_requirement(self):
        # Guarded imports: constructing and driving the handler must never
        # raise, whatever is (not) installed on this host.
        handler = otel_callbacks.OTelCallbackHandler()
        rid = uuid.uuid4()
        handler.on_chain_start({}, {}, run_id=rid)
        handler.on_chain_end({}, run_id=rid)


@unittest.skipUnless(_HAVE_SDK, "opentelemetry SDK not installed on host")
class OTelCallbackHandlerTest(unittest.TestCase):
    exporter = None

    @classmethod
    def setUpClass(cls):
        # The global tracer provider can only be set once per process.
        cls.exporter = InMemorySpanExporter()
        provider = TracerProvider()
        provider.add_span_processor(SimpleSpanProcessor(cls.exporter))
        trace.set_tracer_provider(provider)

    def setUp(self):
        self.exporter.clear()
        self.handler = otel_callbacks.OTelCallbackHandler()

    def _spans_by_name(self):
        return {s.name: s for s in self.exporter.get_finished_spans()}

    def test_parenting_via_parent_run_id(self):
        root, llm, tool = uuid.uuid4(), uuid.uuid4(), uuid.uuid4()
        self.handler.on_chain_start({}, {}, run_id=root)
        self.handler.on_chat_model_start(
            {"name": "ChatOpenAI"}, [], run_id=llm, parent_run_id=root,
            invocation_params={"model": "gemma"})
        response = types.SimpleNamespace(
            llm_output={"token_usage": {"prompt_tokens": 5, "completion_tokens": 7}},
            generations=[])
        self.handler.on_llm_end(response, run_id=llm)
        self.handler.on_tool_start({"name": "bash"}, "echo tool-input-content",
                                   run_id=tool, parent_run_id=root)
        self.handler.on_tool_end("tool output", run_id=tool)
        self.handler.on_chain_end({}, run_id=root)

        spans = self._spans_by_name()
        self.assertEqual(set(spans), {"agent.graph", "chat gemma", "tool bash"})
        graph = spans["agent.graph"]
        chat = spans["chat gemma"]
        tool_span = spans["tool bash"]

        # Both children parent on agent.graph, all in one trace.
        for child in (chat, tool_span):
            self.assertEqual(child.parent.span_id, graph.context.span_id)
            self.assertEqual(child.context.trace_id, graph.context.trace_id)

        self.assertEqual(chat.kind, SpanKind.CLIENT)
        self.assertEqual(chat.attributes["gen_ai.operation.name"], "chat")
        self.assertEqual(chat.attributes["gen_ai.request.model"], "gemma")
        self.assertEqual(chat.attributes["gen_ai.usage.input_tokens"], 5)
        self.assertEqual(chat.attributes["gen_ai.usage.output_tokens"], 7)

        # Tool spans carry name + input LENGTH only, never content.
        self.assertEqual(tool_span.kind, SpanKind.INTERNAL)
        self.assertEqual(tool_span.attributes["gen_ai.tool.name"], "bash")
        self.assertEqual(tool_span.attributes["hermes.tool.input_length"],
                         len("echo tool-input-content"))
        for value in tool_span.attributes.values():
            self.assertNotIn("tool-input-content", str(value))
        self.assertNotIn("tool output", str(dict(tool_span.attributes)))

        # No leaked run map entries once every end callback has fired.
        self.assertEqual(self.handler._runs, {})

    def test_fallback_to_current_ctx_under_run_coroutine_threadsafe(self):
        # Mirror gateway_server: a persistent loop in a background thread, one
        # coroutine per turn opening a SERVER span as current (like _traced).
        # A root chain run (parent_run_id=None) must parent on that span via
        # the current-context fallback.
        loop = asyncio.new_event_loop()

        def _run():
            asyncio.set_event_loop(loop)
            loop.run_forever()

        thread = threading.Thread(target=_run, daemon=True)
        thread.start()

        async def turn(handler, rid):
            tracer = trace.get_tracer("test")
            with tracer.start_as_current_span("agent.chat", kind=SpanKind.SERVER):
                handler.on_chain_start({}, {}, run_id=rid)
                handler.on_chain_end({}, run_id=rid)

        rid = uuid.uuid4()
        try:
            asyncio.run_coroutine_threadsafe(turn(self.handler, rid), loop).result(10)
        finally:
            loop.call_soon_threadsafe(loop.stop)
            thread.join(10)

        spans = self._spans_by_name()
        self.assertEqual(set(spans), {"agent.graph", "agent.chat"})
        self.assertEqual(spans["agent.graph"].parent.span_id,
                         spans["agent.chat"].context.span_id)
        self.assertEqual(spans["agent.graph"].context.trace_id,
                         spans["agent.chat"].context.trace_id)

    def test_nested_chain_runs_do_not_emit_spans(self):
        root, inner = uuid.uuid4(), uuid.uuid4()
        self.handler.on_chain_start({}, {}, run_id=root)
        self.handler.on_chain_start({}, {}, run_id=inner, parent_run_id=root)
        self.handler.on_chain_end({}, run_id=inner)
        self.handler.on_chain_end({}, run_id=root)
        self.assertEqual(set(self._spans_by_name()), {"agent.graph"})

    def test_error_callback_sets_error_status(self):
        rid = uuid.uuid4()
        self.handler.on_tool_start({"name": "bash"}, "x", run_id=rid)
        self.handler.on_tool_error(RuntimeError("boom"), run_id=rid)
        span = self._spans_by_name()["tool bash"]
        self.assertEqual(span.status.status_code, StatusCode.ERROR)

    def test_leak_sweep_ends_stale_spans(self):
        stale = uuid.uuid4()
        self.handler.on_chain_start({}, {}, run_id=stale)
        with self.handler._lock:
            span, token, t0 = self.handler._runs[stale]
            self.handler._runs[stale] = (span, token, t0 - 2 * self.handler._MAX_SPAN_AGE_S)
            self.handler._last_sweep -= 2 * self.handler._SWEEP_INTERVAL_S
        # Any subsequent start triggers the sweep.
        trigger = uuid.uuid4()
        self.handler.on_tool_start({"name": "bash"}, "y", run_id=trigger)
        self.assertNotIn(stale, self.handler._runs)
        swept = self._spans_by_name()["agent.graph"]
        self.assertEqual(swept.status.status_code, StatusCode.ERROR)
        self.handler.on_tool_end("done", run_id=trigger)


if __name__ == "__main__":
    unittest.main()
