"""
Tests for the omp harness adapter (rootfs/overlay/opt/agent/omp_agent.py),
driven against fake_omp.py — a scripted `omp --mode rpc`.

Run:
    PYTHONPATH=rootfs/overlay/opt/agent python3 -m unittest discover -v rootfs/tests
"""
import asyncio
import json
import os
import stat
import tempfile
import unittest

import approvals
import omp_agent

FAKE_OMP = os.path.join(os.path.dirname(os.path.abspath(__file__)), "fake_omp.py")


def user(text):
    return {"messages": [{"role": "user", "content": "earlier turn"},
                         {"role": "assistant", "content": "earlier answer"},
                         {"role": "user", "content": text}]}


def answer(state):
    return state["messages"][-1].content


class OmpAgentTest(unittest.IsolatedAsyncioTestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        home = self.tmp.name
        self.prompt_file = os.path.join(home, "system-prompt.md")
        with open(self.prompt_file, "w") as f:
            f.write("be brief")
        self.env = {
            "HOME": home,
            "OMP_BIN": FAKE_OMP,
            "OMP_WORKSPACE": home,
            "OMP_SYSTEM_PROMPT_FILE": self.prompt_file,
            "LLM_API_BASE": "http://llm.internal:8080/v1",
            "LLM_API_KEY": "sk-test",
            "LLM_MODEL": "north",
            "OMP_MAX_SESSIONS": "2",
        }

    def make_agent(self, **env):
        agent = omp_agent.OmpAgent(omp_agent.OmpConfig({**self.env, **env}))
        self.addAsyncCleanup(agent.aclose)
        return agent

    # ---- config files ----
    def test_models_config_points_at_the_llm_router(self):
        agent = self.make_agent()
        path = os.path.join(self.tmp.name, ".omp", "agent", "models.yml")
        provider = json.load(open(path))["providers"]["sandbox"]
        self.assertEqual(provider["baseUrl"], "http://llm.internal:8080/v1")
        self.assertEqual(provider["api"], "openai-completions")
        self.assertEqual(provider["models"][0]["id"], "north")
        # carries the API key: owner-only
        self.assertEqual(stat.S_IMODE(os.stat(path).st_mode), 0o600)
        self.assertFalse(os.path.exists(os.path.join(agent.config.agent_dir, "mcp.json")))

    def test_mcp_config_written_only_with_memory_enabled_and_a_token(self):
        agent = self.make_agent(MNEMOSYNE_ENABLED="1", MNEMOSYNE_TOKEN="mnem_x",
                                MNEMOSYNE_URL="http://mnemosyne.host:8077/sse")
        server = json.load(open(os.path.join(agent.config.agent_dir, "mcp.json")))["mcpServers"]["mnemosyne"]
        # Served to omp over stdio by the SSE bridge; the token is in the
        # child's environment, never on its command line.
        self.assertEqual(server["type"], "stdio")
        self.assertTrue(server["args"][0].endswith("mcp_sse_bridge.py"))
        self.assertEqual(server["args"][1], "http://mnemosyne.host:8077/sse")
        self.assertEqual(server["env"]["MCP_BRIDGE_AUTHORIZATION"], "Bearer mnem_x")
        self.assertNotIn("mnem_x", " ".join(server["args"]))
        # omp must wait for the memory server's tools instead of its 250 ms default.
        self.assertEqual(agent.config.child_env()["OMP_MCP_STARTUP_TIMEOUT_MS"], "20000")
        self.assertNotIn("OMP_MCP_STARTUP_TIMEOUT_MS", self.make_agent().config.child_env())
        # Switched off (or token missing) on a later boot: the stale file goes.
        self.make_agent(MNEMOSYNE_ENABLED="1", MNEMOSYNE_URL="http://mnemosyne.host:8077/sse")
        self.assertFalse(os.path.exists(os.path.join(agent.config.agent_dir, "mcp.json")))

    def test_command_line(self):
        cmd = self.make_agent().config.command("abc")
        self.assertEqual(cmd[:3], [FAKE_OMP, "--mode", "rpc"])
        self.assertEqual(cmd[cmd.index("--model") + 1], "sandbox/north")
        self.assertEqual(cmd[cmd.index("--approval-mode") + 1], "write")
        self.assertIn("--continue", cmd)
        # Fail fast on a dead backend instead of omp's minutes-long default backoff.
        overlay = json.load(open(cmd[cmd.index("--config") + 1]))
        self.assertEqual(overlay["retry"]["maxRetries"], 2)
        self.assertEqual(cmd[cmd.index("--append-system-prompt") + 1], self.prompt_file)

    def test_session_dir_names_are_safe_and_distinct(self):
        names = {omp_agent._safe_session_dirname(s) for s in ("a/b", "a_b", "../../etc", "ok-1")}
        self.assertEqual(len(names), 4)
        for name in names:
            self.assertNotIn("/", name)
            self.assertFalse(name.startswith("."))

    # ---- turns ----
    async def test_ainvoke_returns_final_text_and_usage(self):
        state = await self.make_agent().for_session("s").ainvoke(user("echo:hello world"))
        msg = state["messages"][-1]
        self.assertEqual(msg.type, "ai")
        self.assertEqual(msg.content, "hello world")       # leading blank lines stripped
        self.assertEqual(msg.usage_metadata["input_tokens"], 10)
        self.assertEqual(msg.usage_metadata["output_tokens"], 3)

    async def test_only_the_newest_user_message_is_sent(self):
        # History is omp's job: resending earlier turns would duplicate them.
        state = await self.make_agent().for_session("s").ainvoke(user("echo:just this"))
        self.assertEqual(answer(state), "just this")

    async def test_astream_yields_text_deltas_then_final_state(self):
        from langchain_core.messages import AIMessage
        chunks, final = [], None
        async for mode, payload in self.make_agent().for_session("s").astream(
                user("echo:streamed text"), stream_mode=["messages", "values"]):
            if mode == "messages":
                self.assertIsInstance(payload[0], AIMessage)
                chunks.append(payload[0].content)
            else:
                final = payload
        self.assertGreater(len(chunks), 1)
        self.assertEqual("".join(chunks), "streamed text")  # no thinking, no leading blanks
        self.assertEqual(answer(final), "streamed text")

    async def test_sessions_are_separate_conversations(self):
        agent = self.make_agent()
        a, b = agent.for_session("a"), agent.for_session("b")
        self.assertEqual(answer(await a.ainvoke(user("count"))), "prompt 1")
        self.assertEqual(answer(await a.ainvoke(user("count"))), "prompt 2")
        self.assertEqual(answer(await b.ainvoke(user("count"))), "prompt 1")

    async def test_evicted_session_resumes_from_disk(self):
        agent = self.make_agent(OMP_MAX_SESSIONS="1")
        self.assertEqual(answer(await agent.for_session("a").ainvoke(user("count"))), "prompt 1")
        first = agent._sessions["a"].proc
        await agent.for_session("b").ainvoke(user("count"))          # evicts "a"
        self.assertEqual(list(agent._sessions), ["b"])
        self.assertIsNotNone(first.returncode)                        # process was closed
        self.assertEqual(answer(await agent.for_session("a").ainvoke(user("count"))), "prompt 2")

    async def test_provider_error_raises_and_session_recovers(self):
        s = self.make_agent().for_session("s")
        with self.assertRaisesRegex(omp_agent.OmpError, "no healthy backend"):
            await s.ainvoke(user("fail"))
        self.assertEqual(answer(await s.ainvoke(user("echo:back"))), "back")

    async def test_process_death_raises_and_next_turn_respawns(self):
        s = self.make_agent().for_session("s")
        with self.assertRaisesRegex(omp_agent.OmpError, "exited unexpectedly"):
            await s.ainvoke(user("die"))
        self.assertEqual(answer(await s.ainvoke(user("echo:alive"))), "alive")

    async def test_cancel_aborts_the_turn_and_keeps_the_process(self):
        agent = self.make_agent()
        task = asyncio.ensure_future(agent.for_session("s").ainvoke(user("hang")))
        await asyncio.sleep(0.5)
        proc = agent._sessions["s"].proc
        task.cancel()
        with self.assertRaises(asyncio.CancelledError):
            await task
        self.assertIsNone(proc.returncode)                            # aborted, not killed
        self.assertEqual(answer(await agent.for_session("s").ainvoke(user("echo:next"))), "next")
        self.assertIs(agent._sessions["s"].proc, proc)

    async def test_unsupported_dialog_is_declined_not_hung(self):
        state = await asyncio.wait_for(self.make_agent().for_session("s").ainvoke(user("dialog")), 10)
        self.assertEqual(answer(state), "cancelled")

    # ---- approvals ----
    async def test_outside_a_run_shell_commands_are_approved(self):
        state = await self.make_agent().for_session("s").ainvoke(user("bash:rm -rf /tmp/x"))
        self.assertEqual(answer(state), "ran")

    async def _in_run(self, prompt, choice):
        """Run one turn inside a run context, resolving approvals with `choice`."""
        loop = asyncio.get_running_loop()
        queue = asyncio.Queue()
        ctx = approvals.RunContext("run_t", loop, queue)
        events, requested = [], []

        async def resolver():
            while True:
                ev = await queue.get()
                events.append(ev["event"])
                if ev["event"] == "approval.request":
                    requested.append(ev["command"])
                    ctx.resolve(choice)

        watcher = asyncio.ensure_future(resolver())
        token = approvals.current_run.set(ctx)
        try:
            state = await self.make_agent().for_session("s").ainvoke(user(prompt))
        finally:
            approvals.current_run.reset(token)
        await asyncio.sleep(0.05)
        watcher.cancel()
        return answer(state), events, requested

    async def test_in_a_run_dangerous_command_denied(self):
        text, events, requested = await self._in_run("bash:rm -rf /tmp/x", "deny")
        self.assertEqual(text, "denied")
        self.assertEqual(requested, ["rm -rf /tmp/x"])
        self.assertEqual(events[0], "tool.started")
        self.assertIn("approval.request", events)
        self.assertEqual(events[-1], "tool.completed")

    async def test_in_a_run_dangerous_command_approved(self):
        text, _events, requested = await self._in_run("bash:rm -rf /tmp/x", "once")
        self.assertEqual(text, "ran")
        self.assertEqual(requested, ["rm -rf /tmp/x"])

    async def test_in_a_run_safe_command_is_not_put_to_the_client(self):
        text, events, requested = await self._in_run("bash:ls -la", "deny")
        self.assertEqual(text, "ran")
        self.assertEqual(requested, [])
        self.assertEqual(events, ["tool.started", "tool.completed"])


class GatewaySessionBindingTest(unittest.TestCase):
    """gateway_server binds a stateful harness to the request's session id."""

    def test_agent_for_session(self):
        import gateway_server

        class Stateless:  # DeepAgents: no for_session, history passed every call
            pass

        class Stateful:
            def for_session(self, sid):
                return ("bound", sid)

        plain = Stateless()
        self.assertIs(gateway_server.agent_for_session(plain, "s1"), plain)
        self.assertEqual(gateway_server.agent_for_session(Stateful(), "s1"), ("bound", "s1"))


if __name__ == "__main__":
    unittest.main()
