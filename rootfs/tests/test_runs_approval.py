#!/usr/bin/env python3
"""Host-side integration tests for the runs API in gateway_server.py.

Run from the repo root:
    PYTHONPATH=rootfs/overlay/opt/agent AGENT_LOG_FILE=/tmp/a.log \
        python3 -m unittest discover -v rootfs/tests

Drives the full run lifecycle (POST /v1/runs -> GET .../events -> approval ->
run.completed) over real HTTP against a ThreadingHTTPServer, with the DeepAgents
agent stubbed by a fake that simulates a dangerous shell command via the same
approval gate ApprovalShellBackend uses. This exercises the server plumbing
(202, event queue -> SSE, status store, approval routing, stop) without needing
the deepagents/LLM stack.
"""

import asyncio
import json
import threading
import time
import unittest
import urllib.error
import urllib.request
from http.server import ThreadingHTTPServer

import approvals
import gateway_server


class FakeAI:
    type = "ai"
    usage_metadata = None

    def __init__(self, content):
        self.content = content


class FakeAgent:
    """Stands in for the DeepAgents agent. When `dangerous`, it simulates a
    shell tool that requests approval (in an executor thread, exactly like
    ApprovalShellBackend.execute), then returns a final message reflecting the
    decision."""

    def __init__(self, dangerous=True):
        self.dangerous = dangerous

    async def ainvoke(self, agent_input):
        ctx = approvals.current_run.get()
        if self.dangerous and ctx is not None:
            loop = asyncio.get_event_loop()
            decision = await loop.run_in_executor(
                None, ctx.request_approval, "rm -rf /data", "wipe data")
            content = "denied by user" if decision == "deny" else "deleted /data"
        else:
            content = "hello there"
        return {"messages": [FakeAI(content)]}


class RunsAPITest(unittest.TestCase):
    def setUp(self):
        self.srv = ThreadingHTTPServer(("127.0.0.1", 0), gateway_server.GatewayHandler)
        self.srv.daemon_threads = True
        threading.Thread(target=self.srv.serve_forever, daemon=True).start()
        gateway_server.get_loop()  # ensure the persistent loop is up
        host, port = self.srv.server_address
        self.base = f"http://{host}:{port}"
        self.addCleanup(self.srv.shutdown)

    # ---- tiny HTTP helpers ----
    def _post(self, path, body=None):
        data = json.dumps(body).encode() if body is not None else b""
        req = urllib.request.Request(self.base + path, data=data, method="POST",
                                     headers={"Content-Type": "application/json"})
        try:
            r = urllib.request.urlopen(req, timeout=8)
            return r.status, json.loads(r.read() or b"{}")
        except urllib.error.HTTPError as e:
            return e.code, json.loads(e.read() or b"{}")

    def _get(self, path):
        try:
            r = urllib.request.urlopen(self.base + path, timeout=8)
            return r.status, json.loads(r.read() or b"{}")
        except urllib.error.HTTPError as e:
            return e.code, json.loads(e.read() or b"{}")

    def _collect_events(self, run_id, out):
        try:
            r = urllib.request.urlopen(self.base + f"/v1/runs/{run_id}/events", timeout=10)
            for raw in r:
                line = raw.decode("utf-8").strip()
                if line.startswith("data:"):
                    out.append(json.loads(line[len("data:"):].strip()))
        except Exception:
            pass

    def _wait_status(self, run_id, want, tries=150):
        for _ in range(tries):
            _, st = self._get(f"/v1/runs/{run_id}")
            if st.get("status") == want:
                return st
            time.sleep(0.02)
        self.fail(f"run {run_id} never reached {want} (last={st})")

    # ---- tests ----
    def test_dangerous_run_approve(self):
        gateway_server._AGENT = FakeAgent(dangerous=True)
        status, body = self._post("/v1/runs", {"input": "delete data", "model": "feature-dev"})
        self.assertEqual(status, 202, body)
        run_id = body["run_id"]

        events = []
        ev_thread = threading.Thread(target=self._collect_events, args=(run_id, events), daemon=True)
        ev_thread.start()

        self._wait_status(run_id, "waiting_for_approval")
        status, ack = self._post(f"/v1/runs/{run_id}/approval", {"choice": "once"})
        self.assertEqual(status, 200)
        self.assertEqual(ack["choice"], "once")
        self.assertEqual(ack["object"], "hermes.run.approval_response")

        ev_thread.join(timeout=6)
        kinds = [e.get("event") for e in events]
        self.assertIn("approval.request", kinds)
        self.assertIn("run.completed", kinds)
        completed = next(e for e in events if e.get("event") == "run.completed")
        self.assertEqual(completed["output"], "deleted /data")
        self.assertEqual(self._get(f"/v1/runs/{run_id}")[1]["status"], "completed")

    def test_dangerous_run_deny(self):
        gateway_server._AGENT = FakeAgent(dangerous=True)
        _, body = self._post("/v1/runs", {"input": "delete"})
        run_id = body["run_id"]
        self._wait_status(run_id, "waiting_for_approval")
        self.assertEqual(self._post(f"/v1/runs/{run_id}/approval", {"choice": "deny"})[0], 200)
        st = self._wait_status(run_id, "completed")
        self.assertEqual(st.get("output"), "denied by user")

    def test_safe_run_no_pause(self):
        gateway_server._AGENT = FakeAgent(dangerous=False)
        _, body = self._post("/v1/runs", {"input": "hi"})
        run_id = body["run_id"]
        st = self._wait_status(run_id, "completed")
        self.assertEqual(st.get("output"), "hello there")
        # never paused
        _, again = self._get(f"/v1/runs/{run_id}")
        self.assertEqual(again["status"], "completed")

    def test_approval_choice_aliases_and_validation(self):
        gateway_server._AGENT = FakeAgent(dangerous=True)
        _, body = self._post("/v1/runs", {"input": "x"})
        run_id = body["run_id"]
        self._wait_status(run_id, "waiting_for_approval")
        # invalid choice -> 400 (still pending)
        self.assertEqual(self._post(f"/v1/runs/{run_id}/approval", {"choice": "maybe"})[0], 400)
        # "approve" aliases to "once"
        s, ack = self._post(f"/v1/runs/{run_id}/approval", {"choice": "approve"})
        self.assertEqual(s, 200)
        self.assertEqual(ack["choice"], "once")

    def test_unknown_run_and_missing_input(self):
        self.assertEqual(self._get("/v1/runs/run_nope")[0], 404)
        self.assertEqual(self._post("/v1/runs/run_nope/approval", {"choice": "once"})[0], 404)
        self.assertEqual(self._post("/v1/runs/run_nope/stop")[0], 404)
        self.assertEqual(self._post("/v1/runs", {"model": "x"})[0], 400)

    def test_stop(self):
        gateway_server._AGENT = FakeAgent(dangerous=True)
        _, body = self._post("/v1/runs", {"input": "go"})
        run_id = body["run_id"]
        self._wait_status(run_id, "waiting_for_approval")
        s, resp = self._post(f"/v1/runs/{run_id}/stop")
        self.assertEqual(s, 200)
        self.assertEqual(resp["status"], "stopping")

    def test_capabilities_advertises_runs(self):
        s, body = self._get("/v1/capabilities")
        self.assertEqual(s, 200)
        self.assertEqual(body["object"], "hermes.api_server.capabilities")
        for k in ("approval_events", "run_approval_response", "run_events_sse", "run_submission"):
            self.assertTrue(body["features"][k], f"{k} should be advertised")


if __name__ == "__main__":
    unittest.main()
