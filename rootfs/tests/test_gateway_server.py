#!/usr/bin/env python3
"""
Host-side tests for rootfs/overlay/opt/agent/gateway_server.py.

Run from the repo root:
    PYTHONPATH=rootfs/overlay/opt/agent python3 -m unittest discover -v rootfs/tests

Covers:
  - traceparent carrier lowercasing + extraction (headers cased exactly as the
    Go router canonicalizes them: "Traceparent");
  - SESSIONS LRU eviction (cap 200) and per-session history cap (80 messages,
    whole user/assistant pairs).

Extraction tests skip cleanly when opentelemetry is not installed on the host.
"""

import sys
import types
import unittest

# gateway_server imports agent.py at module import; the real agent.py attaches
# a FileHandler on /var/log/agent.log and drags in the LLM stack, neither of
# which exists on the build host. Stub it BEFORE importing gateway_server.
_agent_stub = types.ModuleType("agent")
_agent_stub.create_agent = lambda callbacks=None: None
_agent_stub.init_tracing = lambda: []
sys.modules["agent"] = _agent_stub

import gateway_server  # noqa: E402

# A valid W3C traceparent, cased exactly as Go's net/http canonicalizes the
# header key when the router injects it. Synthetic ids, not real trace data.
GO_CANONICAL_HEADERS = {
    "Traceparent": "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01",
    "X-Hermes-Session-Id": "sess-1",
}


class CarrierTest(unittest.TestCase):
    def test_carrier_is_lowercased(self):
        carrier = gateway_server._otel_carrier(GO_CANONICAL_HEADERS)
        self.assertIn("traceparent", carrier)
        self.assertIn("x-hermes-session-id", carrier)
        self.assertNotIn("Traceparent", carrier)
        self.assertEqual(carrier["traceparent"], GO_CANONICAL_HEADERS["Traceparent"])

    def test_extract_without_otel_is_none(self):
        if gateway_server._OTEL_AVAILABLE:
            self.skipTest("opentelemetry installed; no-op path not reachable")
        self.assertIsNone(
            gateway_server._otel_extract(gateway_server._otel_carrier(GO_CANONICAL_HEADERS)))


@unittest.skipUnless(gateway_server._OTEL_AVAILABLE,
                     "opentelemetry not installed on host")
class ExtractionTest(unittest.TestCase):
    def _span_context(self, ctx):
        from opentelemetry import trace
        return trace.get_current_span(ctx).get_span_context()

    def test_extracts_parent_from_go_canonical_headers(self):
        ctx = gateway_server._otel_extract(
            gateway_server._otel_carrier(GO_CANONICAL_HEADERS))
        sc = self._span_context(ctx)
        self.assertTrue(sc.is_valid)
        self.assertEqual(format(sc.trace_id, "032x"),
                         "0af7651916cd43dd8448eb211c80319c")
        self.assertEqual(format(sc.span_id, "016x"), "b7ad6b7169203331")
        self.assertTrue(sc.is_remote)

    def test_lowercasing_is_load_bearing(self):
        # The OTel dict getter is an exact-key lookup for "traceparent":
        # feeding the Go-canonical casing WITHOUT lowercasing yields no parent.
        sc = self._span_context(gateway_server._otel_extract(GO_CANONICAL_HEADERS))
        self.assertFalse(sc.is_valid)


class SessionStoreTest(unittest.TestCase):
    def setUp(self):
        gateway_server.SESSIONS.clear()
        gateway_server.SESSIONS_LAST_ACCESS.clear()

    @staticmethod
    def _turn(i):
        return [{"role": "user", "content": f"u{i}"},
                {"role": "assistant", "content": f"a{i}"}]

    def test_history_capped_at_80_most_recent(self):
        sid = "long-session"
        for i in range(50):  # 100 messages appended
            history = gateway_server._session_history(sid)
            gateway_server._session_append(sid, history, self._turn(i))
        history = gateway_server.SESSIONS[sid]
        self.assertEqual(len(history), gateway_server.MAX_HISTORY_MESSAGES)
        # Oldest trimmed first: pairs 0..9 dropped, 10..49 kept, order intact.
        self.assertEqual(history[0], {"role": "user", "content": "u10"})
        self.assertEqual(history[1], {"role": "assistant", "content": "a10"})
        self.assertEqual(history[-1], {"role": "assistant", "content": "a49"})

    def test_trim_never_splits_a_pair(self):
        sid = "odd-session"
        # 83 alternating messages (trailing unpaired user turn): raw drop of 3
        # would split pair #1, so the trim must round up to 4.
        msgs = []
        for i in range(83):
            role = "user" if i % 2 == 0 else "assistant"
            msgs.append({"role": role, "content": f"m{i}"})
        with gateway_server.SESSIONS_LOCK:
            gateway_server.SESSIONS[sid] = msgs
            gateway_server._trim_history_locked(sid)
        history = gateway_server.SESSIONS[sid]
        self.assertEqual(len(history), 79)
        self.assertEqual(history[0], {"role": "user", "content": "m4"})

    def test_lru_eviction_beyond_200_sessions(self):
        # Fill to the cap with deterministic, increasing last-access times.
        for i in range(gateway_server.MAX_SESSIONS):
            sid = f"s{i}"
            gateway_server.SESSIONS[sid] = self._turn(i)
            gateway_server.SESSIONS_LAST_ACCESS[sid] = float(i)
        # Touch the otherwise-oldest session through the read path.
        gateway_server._session_history("s0")
        # A new session pushes us to 201 -> the LRU victim is now s1, not s0.
        gateway_server._session_append("s_new", [], self._turn(999))
        self.assertEqual(len(gateway_server.SESSIONS), gateway_server.MAX_SESSIONS)
        self.assertIn("s0", gateway_server.SESSIONS)
        self.assertIn("s_new", gateway_server.SESSIONS)
        self.assertNotIn("s1", gateway_server.SESSIONS)
        self.assertNotIn("s1", gateway_server.SESSIONS_LAST_ACCESS)

    def test_read_of_unknown_session_does_not_create_lru_entry(self):
        self.assertEqual(gateway_server._session_history("ghost"), [])
        self.assertNotIn("ghost", gateway_server.SESSIONS_LAST_ACCESS)


if __name__ == "__main__":
    unittest.main()
