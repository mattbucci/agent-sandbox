"""
Tests for the stdio<->SSE MCP bridge (rootfs/overlay/opt/agent/mcp_sse_bridge.py)
against a minimal in-process MCP-over-SSE server.

Run:
    PYTHONPATH=rootfs/overlay/opt/agent python3 -m unittest discover -v rootfs/tests
"""
import io
import json
import queue
import threading
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import mcp_sse_bridge


class FakeSseServer:
    """GET /sse announces an endpoint, then relays an echo of each POSTed message."""

    def __init__(self, token="Bearer t"):
        outbox = queue.Queue()
        seen = {"auth": [], "posts": []}
        self.seen = seen

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *a):
                pass

            def do_GET(self):
                seen["auth"].append(self.headers.get("Authorization"))
                if self.headers.get("Authorization") != token:
                    self.send_response(401)
                    self.end_headers()
                    return
                self.send_response(200)
                self.send_header("Content-Type", "text/event-stream")
                self.end_headers()
                self.wfile.write(b": keepalive\n\nevent: endpoint\ndata: /messages?session_id=abc\n\n")
                self.wfile.flush()
                while True:
                    item = outbox.get()
                    if item is None:
                        return
                    self.wfile.write(b"event: message\ndata: " + item.encode() + b"\n\n")
                    self.wfile.flush()

            def do_POST(self):
                body = self.rfile.read(int(self.headers.get("Content-Length", 0)))
                seen["posts"].append((self.path, self.headers.get("Authorization"), json.loads(body)))
                self.send_response(202)
                self.end_headers()
                msg = json.loads(body)
                if "id" in msg:  # requests get a response over the event stream
                    outbox.put(json.dumps({"jsonrpc": "2.0", "id": msg["id"], "result": {"echo": msg["method"]}}))

        self.httpd = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.httpd.daemon_threads = True
        self.outbox = outbox
        threading.Thread(target=self.httpd.serve_forever, daemon=True).start()
        self.url = f"http://127.0.0.1:{self.httpd.server_address[1]}/sse"

    def close(self):
        self.outbox.put(None)
        self.httpd.shutdown()
        self.httpd.server_close()


class _Collect(io.StringIO):
    """stdout stand-in that signals when N lines have been written."""

    def __init__(self, want):
        super().__init__()
        self.want, self.done = want, threading.Event()

    def flush(self):
        if self.getvalue().count("\n") >= self.want:
            self.done.set()


class BridgeTest(unittest.TestCase):
    def setUp(self):
        self.server = FakeSseServer()
        self.addCleanup(self.server.close)

    def test_relays_requests_and_responses(self):
        out = _Collect(want=2)
        bridge = mcp_sse_bridge.Bridge(self.server.url, "Bearer t")

        class Stdin:  # two requests and a notification, then stay open until answered
            def __iter__(self_inner):
                yield json.dumps({"jsonrpc": "2.0", "id": 1, "method": "initialize"}) + "\n"
                yield "\n"
                yield json.dumps({"jsonrpc": "2.0", "method": "notifications/initialized"}) + "\n"
                yield json.dumps({"jsonrpc": "2.0", "id": 2, "method": "tools/list"}) + "\n"
                out.done.wait(5)

        self.assertEqual(bridge.run(stdin=Stdin(), out=out), 0)
        lines = [json.loads(l) for l in out.getvalue().splitlines()]
        self.assertEqual([l["id"] for l in lines], [1, 2])
        self.assertEqual(lines[1]["result"], {"echo": "tools/list"})
        # POSTs went to the announced endpoint, resolved against the SSE URL, authenticated.
        self.assertEqual(len(self.server.seen["posts"]), 3)
        for path, auth, _body in self.server.seen["posts"]:
            self.assertEqual(path, "/messages?session_id=abc")
            self.assertEqual(auth, "Bearer t")

    def test_rejected_stream_fails_instead_of_hanging(self):
        bridge = mcp_sse_bridge.Bridge(self.server.url, "Bearer wrong")
        stdin = [json.dumps({"jsonrpc": "2.0", "id": 1, "method": "initialize"}) + "\n"]
        self.assertEqual(bridge.run(stdin=stdin, out=io.StringIO()), 1)
        self.assertEqual(self.server.seen["posts"], [])

    def test_non_json_input_is_dropped(self):
        out = _Collect(want=1)
        bridge = mcp_sse_bridge.Bridge(self.server.url, "Bearer t")

        class Stdin:
            def __iter__(self_inner):
                yield "not json\n"
                yield json.dumps({"jsonrpc": "2.0", "id": 7, "method": "ping"}) + "\n"
                out.done.wait(5)

        self.assertEqual(bridge.run(stdin=Stdin(), out=out), 0)
        self.assertEqual(len(self.server.seen["posts"]), 1)


class FakeStreamableServer:
    """Streamable-HTTP MCP: replies in the POST body, as JSON or an event stream."""

    def __init__(self):
        seen = {"sessions": [], "auth": [], "methods": []}
        self.seen = seen
        state = {"session": "sess-1"}
        self.state = state

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *a):
                pass

            def do_POST(self):
                msg = json.loads(self.rfile.read(int(self.headers.get("Content-Length", 0))))
                seen["sessions"].append(self.headers.get("Mcp-Session-Id"))
                seen["auth"].append(self.headers.get("Authorization"))
                seen["methods"].append(msg["method"])
                sent = self.headers.get("Mcp-Session-Id")
                if msg["method"] != "initialize" and sent != state["session"]:
                    self.send_response(404)              # unknown session
                    self.end_headers()
                    return
                if "id" not in msg:                      # notification
                    self.send_response(202)
                    self.end_headers()
                    return
                if msg["method"] == "boom":
                    self.send_response(500)
                    self.end_headers()
                    return
                reply = json.dumps({"jsonrpc": "2.0", "id": msg["id"], "result": {"echo": msg["method"]}})
                self.send_response(200)
                if msg["method"] == "initialize":
                    self.send_header("Mcp-Session-Id", state["session"])
                if msg["method"] == "tools/list":        # plain JSON body
                    body = reply.encode()
                    self.send_header("Content-Type", "application/json")
                else:                                    # event-stream body
                    body = b"event: message\ndata: " + reply.encode() + b"\n\n"
                    self.send_header("Content-Type", "text/event-stream")
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)

        self.httpd = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.httpd.daemon_threads = True
        threading.Thread(target=self.httpd.serve_forever, daemon=True).start()
        self.url = f"http://127.0.0.1:{self.httpd.server_address[1]}/mcp/bank/"

    def close(self):
        self.httpd.shutdown()
        self.httpd.server_close()


class HttpBridgeTest(unittest.TestCase):
    def setUp(self):
        self.server = FakeStreamableServer()
        self.addCleanup(self.server.close)

    def run_bridge(self, messages):
        out = io.StringIO()
        bridge = mcp_sse_bridge.HttpBridge(self.server.url, "Bearer t")
        self.assertEqual(bridge.run(stdin=[json.dumps(m) + "\n" for m in messages], out=out), 0)
        return bridge, {json.loads(l)["id"]: json.loads(l) for l in out.getvalue().splitlines()}

    def test_relays_json_and_event_stream_replies(self):
        bridge, replies = self.run_bridge([
            {"jsonrpc": "2.0", "id": 1, "method": "initialize"},
            {"jsonrpc": "2.0", "method": "notifications/initialized"},
            {"jsonrpc": "2.0", "id": 2, "method": "tools/list"},
            {"jsonrpc": "2.0", "id": 3, "method": "tools/call"},
        ])
        self.assertEqual(sorted(replies), [1, 2, 3])
        self.assertEqual(replies[2]["result"], {"echo": "tools/list"})
        self.assertEqual(replies[3]["result"], {"echo": "tools/call"})
        # The session id from initialize is carried on every later request.
        self.assertEqual(self.server.seen["sessions"][0], None)
        self.assertEqual(set(self.server.seen["sessions"][1:]), {"sess-1"})
        self.assertEqual(set(self.server.seen["auth"]), {"Bearer t"})

    def test_reconnects_when_the_server_forgot_the_session(self):
        out = io.StringIO()
        bridge = mcp_sse_bridge.HttpBridge(self.server.url, "Bearer t")
        for m in ({"jsonrpc": "2.0", "id": 1, "method": "initialize"},
                  {"jsonrpc": "2.0", "method": "notifications/initialized"}):
            bridge.handle(json.dumps(m), out)
        self.server.state["session"] = "sess-2"          # the server restarted
        bridge.handle(json.dumps({"jsonrpc": "2.0", "id": 2, "method": "tools/call"}), out)
        replies = {json.loads(l)["id"]: json.loads(l) for l in out.getvalue().splitlines()}
        # The call succeeded on a fresh session, and the replayed handshake's
        # reply was not relayed a second time.
        self.assertEqual(sorted(replies), [1, 2])
        self.assertEqual(replies[2]["result"], {"echo": "tools/call"})
        self.assertEqual(bridge.session_id, "sess-2")
        self.assertEqual(self.server.seen["methods"],
                         ["initialize", "notifications/initialized", "tools/call",
                          "initialize", "notifications/initialized", "tools/call"])

    def test_failed_request_is_answered_with_an_error(self):
        _bridge, replies = self.run_bridge([{"jsonrpc": "2.0", "id": 9, "method": "boom"}])
        self.assertIn("mcp bridge", replies[9]["error"]["message"])


if __name__ == "__main__":
    unittest.main()
