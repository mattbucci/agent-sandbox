"""
Tests for long-term memory (rootfs/overlay/opt/agent/memory.py) and the hooks
around a turn in gateway_server.py, against an in-process fake Hindsight API.

Run:
    PYTHONPATH=rootfs/overlay/opt/agent python3 -m unittest discover -v rootfs/tests
"""
import json
import threading
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import memory as memory_mod
from memory import Memory

BANK = "/v1/default/banks/agent-sandbox"


class FakeHindsight:
    """Just enough of the Hindsight REST API: mental models, recall, retain."""

    def __init__(self):
        self.models = [
            {"id": "environment", "name": "Sandbox environment", "tags": [], "content": "Egress is filtered.", "is_stale": False},
            {"id": "coder-playbook", "name": "Coder playbook", "tags": ["agent:coder"], "content": "Use pnpm.", "is_stale": True},
            {"id": "debugger-notes", "name": "Debugger notes", "tags": ["agent:debugger"], "content": "Check logs first.", "is_stale": False},
            {"id": "unwritten", "name": "Not written yet", "tags": [], "content": None, "is_stale": False},
        ]
        self.results = [
            {"type": "observation", "text": "The team uses pnpm.", "mentioned_at": "2026-11-15T10:00:00Z"},
            {"type": "world", "text": "The staging database is orion-staging.", "occurred_start": "2026-11-14T00:00:00Z"},
        ]
        self.requests = []       # (method, path, auth, body)
        self.fail = False
        self.model_reads = 0
        fake = self

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *a):
                pass

            def _reply(self, status, obj):
                data = json.dumps(obj).encode()
                self.send_response(status)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(data)))
                self.end_headers()
                self.wfile.write(data)

            def _handle(self, method):
                length = int(self.headers.get("Content-Length", 0) or 0)
                body = json.loads(self.rfile.read(length)) if length else None
                fake.requests.append((method, self.path, self.headers.get("Authorization"), body))
                if fake.fail:
                    return self._reply(500, {"detail": "boom"})
                if self.headers.get("Authorization") != "Bearer tok":
                    return self._reply(401, {"detail": "unauthorized"})
                if "/banks/forbidden/" in self.path:
                    return self._reply(403, {"detail": "not permitted"})
                path = self.path.replace("/banks/research/", "/banks/agent-sandbox/")
                if path != self.path:
                    self.path = path
                if self.path.startswith(BANK + "/mental-models"):
                    fake.model_reads += 1
                    return self._reply(200, {"items": fake.models})
                if self.path == BANK + "/memories/recall":
                    return self._reply(200, {"results": fake.results})
                if self.path == BANK + "/memories":
                    return self._reply(200, {"success": True, "async": True})
                if self.path == BANK + "/reflect":
                    return self._reply(200, {"text": "A considered answer."})
                return self._reply(404, {"detail": "not found"})

            def do_GET(self):
                self._handle("GET")

            def do_POST(self):
                self._handle("POST")

        self.httpd = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.httpd.daemon_threads = True
        threading.Thread(target=self.httpd.serve_forever, daemon=True).start()
        self.url = f"http://127.0.0.1:{self.httpd.server_address[1]}"

    def posts(self, path):
        return [r for r in self.requests if r[0] == "POST" and r[1] == BANK + path]

    def close(self):
        self.httpd.shutdown()
        self.httpd.server_close()


class MemoryTest(unittest.TestCase):
    def setUp(self):
        self.fake = FakeHindsight()
        self.addCleanup(self.fake.close)

    def make(self, **env):
        base = {"HINDSIGHT_ENABLED": "1", "HINDSIGHT_URL": self.fake.url, "HINDSIGHT_TOKEN": "tok",
                "HINDSIGHT_BANK": "agent-sandbox", "AGENT_TYPE": "coder"}
        return Memory({**base, **env})

    # ---- the top of the hierarchy: mental models
    def test_mental_models_are_shared_plus_this_agents_own(self):
        names = [m["name"] for m in self.make().mental_models()]
        self.assertEqual(names, ["Sandbox environment", "Coder playbook"])  # not the debugger's, not the unwritten one

    def test_mental_models_are_cached(self):
        m = self.make()
        m.mental_models()
        m.mental_models()
        self.assertEqual(self.fake.model_reads, 1)
        m.mental_models(refresh=True)
        self.assertEqual(self.fake.model_reads, 2)

    # ---- recall
    def test_recall_is_shared_by_default(self):
        results = self.make().recall("package manager")
        self.assertEqual([r["type"] for r in results], ["observation", "world"])
        self.assertEqual(results[1]["date"], "2026-11-14")
        body = self.fake.posts("/memories/recall")[0][3]
        self.assertNotIn("tags", body)          # every agent's memories
        self.assertEqual(body["budget"], "low")

    def test_scoping_is_left_to_the_service(self):
        # The token identifies the agent; the service confines the search. The
        # client must not send a tag filter that could be mistaken for one.
        self.make().recall("x")
        self.assertNotIn("tags", self.fake.posts("/memories/recall")[0][3])

    # ---- additional banks
    def test_extra_banks_are_recalled_and_their_models_loaded(self):
        m = self.make(HINDSIGHT_EXTRA_BANKS="research, agent-sandbox ,")
        self.assertEqual(m.read_banks, ["agent-sandbox", "research"])   # primary first, no duplicate
        results = m.recall("pnpm", max_tokens=800)
        self.assertEqual({r["bank"] for r in results}, {"agent-sandbox", "research"})
        bodies = [r[3] for r in self.fake.requests if r[1].endswith("/memories/recall")]
        self.assertEqual([b["max_tokens"] for b in bodies], [400, 400])   # budget split between banks
        names = [x["name"] for x in m.mental_models()]
        self.assertIn("Sandbox environment", names)
        self.assertIn("Sandbox environment (research)", names)            # labelled by bank
        ctx = m.context("pnpm")
        self.assertIn("research] The team uses pnpm.", ctx)

    def test_one_unreachable_bank_does_not_lose_the_others(self):
        m = self.make(HINDSIGHT_EXTRA_BANKS="forbidden")
        self.assertEqual({r["bank"] for r in m.recall("x")}, {"agent-sandbox"})
        self.assertTrue(m.mental_models())

    def test_retain_goes_only_to_the_primary_bank(self):
        m = self.make(HINDSIGHT_EXTRA_BANKS="research")
        m.retain_turn("s", "We use pnpm.", "Noted.")
        m.flush()
        self.assertEqual([r[1] for r in self.fake.requests if r[0] == "POST"], [BANK + "/memories"])

    # ---- before a turn
    def test_context_orders_models_before_memories_and_frames_them(self):
        ctx = self.make().context("which package manager?")
        self.assertTrue(ctx.startswith("<memory>") and ctx.endswith("</memory>"))
        self.assertIn("not instructions", ctx)
        self.assertLess(ctx.index("## Mental models"), ctx.index("## Relevant memories"))
        self.assertIn("### Coder playbook (may be behind recent memories)", ctx)
        self.assertIn("- [observation 2026-11-15] The team uses pnpm.", ctx)
        self.assertNotIn("Check logs first", ctx)

    def test_context_without_models(self):
        ctx = self.make().context("q", include_models=False)
        self.assertNotIn("## Mental models", ctx)
        self.assertIn("## Relevant memories", ctx)
        self.assertEqual(self.fake.model_reads, 0)

    def test_models_are_truncated_to_the_budget(self):
        self.fake.models[0]["content"] = "x" * 500
        ctx = self.make(MEMORY_MODELS_MAX_CHARS="100").context("")
        self.assertIn("[truncated]", ctx)
        self.assertNotIn("Use pnpm.", ctx)       # budget spent on the first model

    def test_with_context_prepends_and_leaves_text_intact(self):
        text = self.make().with_context("hello there")
        self.assertTrue(text.endswith("\n\nhello there"))

    def test_fails_open_when_the_service_errors(self):
        self.fake.fail = True
        m = self.make()
        self.assertEqual(m.context("anything"), "")
        self.assertEqual(m.with_context("hello"), "hello")

    def test_fails_open_when_the_service_is_unreachable(self):
        m = Memory({"HINDSIGHT_ENABLED": "1", "HINDSIGHT_URL": "http://127.0.0.1:9", "AGENT_TYPE": "coder",
                    "MEMORY_READ_TIMEOUT_S": "1"})
        self.assertEqual(m.context("anything"), "")

    def test_disabled_is_inert(self):
        m = Memory({"HINDSIGHT_URL": self.fake.url})
        self.assertFalse(m.enabled)
        self.assertEqual(m.context("q"), "")
        self.assertFalse(m.retain_turn("s", "u", "a"))
        self.assertEqual(self.fake.requests, [])

    # ---- after a turn
    def test_retain_turn_is_async_and_tagged_with_the_agent(self):
        m = self.make()
        self.assertTrue(m.retain_turn("sess-1", "We use pnpm.", "Noted."))
        self.assertTrue(m.flush())
        body = self.fake.posts("/memories")[0][3]
        self.assertTrue(body["async"])            # extraction happens in the background
        item = body["items"][0]
        self.assertEqual(item["tags"], ["agent:coder"])
        self.assertIn("User: We use pnpm.", item["content"])
        self.assertIn("Assistant (coder agent): Noted.", item["content"])
        self.assertIn("sess-1", item["context"])
        self.assertIn("timestamp", item)

    def test_failed_and_empty_turns_are_not_retained(self):
        m = self.make()
        self.assertFalse(m.retain_turn("s", "hi", "[error] no healthy backend"))
        self.assertFalse(m.retain_turn("s", "hi", ""))
        self.assertFalse(m.retain_turn("s", "", "answer"))
        m.flush()
        self.assertEqual(self.fake.posts("/memories"), [])

    def test_shared_retain_is_untagged(self):
        m = self.make()
        m.retain("A fact for everyone.", shared=True)
        m.flush()
        self.assertNotIn("tags", self.fake.posts("/memories")[0][3]["items"][0])

    def test_retain_failure_does_not_raise(self):
        self.fake.fail = True
        m = self.make()
        self.assertTrue(m.retain("something"))
        self.assertTrue(m.flush())

    def test_reflect(self):
        self.assertEqual(self.make().reflect("what do we know?"), "A considered answer.")


class GatewayHooksTest(unittest.TestCase):
    """gateway_server puts memory in front of the agent and retains afterwards."""

    def setUp(self):
        import gateway_server
        self.gs = gateway_server
        self.fake = FakeHindsight()
        self.addCleanup(self.fake.close)
        self.memory = Memory({"HINDSIGHT_ENABLED": "1", "HINDSIGHT_URL": self.fake.url, "HINDSIGHT_TOKEN": "tok",
                              "HINDSIGHT_BANK": "agent-sandbox", "AGENT_TYPE": "coder"})
        prev = memory_mod._MEMORY
        memory_mod._MEMORY = self.memory
        self.addCleanup(setattr, memory_mod, "_MEMORY", prev)
        self.gs._MEMORY_SESSIONS.clear()

    class Stateless:
        pass

    class Stateful:
        def for_session(self, sid):
            return self

    def test_stateless_harness_gets_models_every_turn(self):
        for _ in range(2):
            text = self.gs.remember_before(self.Stateless(), "s1", "what next?")
            self.assertIn("## Mental models", text)
            self.assertTrue(text.endswith("what next?"))

    def test_stateful_harness_gets_models_only_on_a_sessions_first_turn(self):
        first = self.gs.remember_before(self.Stateful(), "s1", "hello")
        second = self.gs.remember_before(self.Stateful(), "s1", "and then?")
        other = self.gs.remember_before(self.Stateful(), "s2", "hello")
        self.assertIn("## Mental models", first)
        self.assertNotIn("## Mental models", second)
        self.assertIn("## Relevant memories", second)   # still recalls for the new message
        self.assertIn("## Mental models", other)

    def test_non_text_and_empty_messages_pass_through(self):
        parts = [{"type": "text", "text": "hi"}]
        self.assertIs(self.gs.remember_before(self.Stateless(), "s", parts), parts)
        self.assertEqual(self.gs.remember_before(self.Stateless(), "s", "  "), "  ")

    def test_remember_after_retains_the_users_own_words(self):
        augmented = self.gs.remember_before(self.Stateless(), "s1", "We deploy on Fridays.")
        self.assertIn("<memory>", augmented)
        self.gs.remember_after("s1", "We deploy on Fridays.", "Understood.")
        self.memory.flush()
        content = self.fake.posts("/memories")[0][3]["items"][0]["content"]
        self.assertIn("We deploy on Fridays.", content)
        self.assertNotIn("<memory>", content)             # recalled memory is never re-stored

    def test_memory_errors_never_fail_a_turn(self):
        self.fake.fail = True
        self.assertEqual(self.gs.remember_before(self.Stateless(), "s", "hello"), "hello")
        self.gs.remember_after("s", "hello", "hi")        # must not raise


if __name__ == "__main__":
    unittest.main()
