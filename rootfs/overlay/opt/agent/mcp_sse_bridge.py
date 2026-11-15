#!/usr/bin/env python3
"""
stdio <-> SSE bridge for an MCP server (standard library only).

    mcp_sse_bridge.py <sse-url>          # bearer from MCP_BRIDGE_AUTHORIZATION

An MCP client that speaks stdio launches this as its "server"; the bridge
relays newline-delimited JSON-RPC between that client and a remote MCP server
using the HTTP+SSE transport (GET <sse-url> for the event stream, POST to the
endpoint the stream announces).

Why it exists: the omp harness reaches the shared mnemosyne memory server over
SSE, and omp's own SSE client is reset mid-handshake from inside the VM while
plain HTTP clients on the same path work. Bridging keeps omp on stdio — the
transport every MCP client implements best — and the network hop on urllib.

The token is read from the environment, never argv, so it does not show in ps.
"""
import json
import os
import sys
import threading
import urllib.parse
import urllib.request

CONNECT_TIMEOUT_S = 20


def log(msg):
    sys.stderr.write(f"mcp-sse-bridge: {msg}\n")
    sys.stderr.flush()


class Bridge:
    def __init__(self, sse_url, authorization=""):
        self.sse_url = sse_url
        self.headers = {"Authorization": authorization} if authorization else {}
        self.post_url = None
        self.ready = threading.Event()   # set once the endpoint is known
        self.closed = threading.Event()  # set when the event stream ends
        self._out_lock = threading.Lock()

    # ---- server -> client ----
    def pump_events(self, out=None):
        """Read the SSE stream; forward each `message` event's data to stdout."""
        out = out or sys.stdout
        try:
            req = urllib.request.Request(
                self.sse_url, headers={**self.headers, "Accept": "text/event-stream"})
            with urllib.request.urlopen(req, timeout=CONNECT_TIMEOUT_S) as resp:
                # The connect timeout must not apply to the idle event stream.
                try:
                    resp.fp.raw._sock.settimeout(None)
                except AttributeError:
                    pass
                event, data = "message", []
                for raw in resp:
                    line = raw.decode("utf-8", "replace").rstrip("\r\n")
                    if line == "":                      # blank line: dispatch
                        if data:
                            self._dispatch(event, "\n".join(data), out)
                        event, data = "message", []
                    elif line.startswith(":"):          # comment / keepalive
                        continue
                    elif line.startswith("event:"):
                        event = line[6:].strip()
                    elif line.startswith("data:"):
                        data.append(line[5:].lstrip(" "))
        except Exception as e:  # noqa: BLE001 — any failure ends the bridge
            log(f"event stream failed: {e}")
        finally:
            self.closed.set()
            self.ready.set()  # unblock a sender waiting for the endpoint

    def _dispatch(self, event, data, out):
        if event == "endpoint":
            # Relative to the SSE URL, e.g. "/messages?session_id=...".
            self.post_url = urllib.parse.urljoin(self.sse_url, data.strip())
            self.ready.set()
        elif event == "message":
            with self._out_lock:
                out.write(data.replace("\n", " ") + "\n")
                out.flush()

    # ---- client -> server ----
    def send(self, line):
        self.ready.wait(CONNECT_TIMEOUT_S)
        if self.closed.is_set() or not self.post_url:
            raise ConnectionError("no MCP endpoint (event stream not established)")
        req = urllib.request.Request(
            self.post_url, data=line.encode("utf-8"), method="POST",
            headers={**self.headers, "Content-Type": "application/json"})
        with urllib.request.urlopen(req, timeout=CONNECT_TIMEOUT_S) as resp:
            resp.read()

    def run(self, stdin=None, out=None):
        stdin = stdin or sys.stdin
        threading.Thread(target=self.pump_events, args=(out,), daemon=True).start()
        for line in stdin:
            line = line.strip()
            if not line:
                continue
            try:
                json.loads(line)
            except ValueError:
                log("dropping non-JSON input line")
                continue
            try:
                self.send(line)
            except Exception as e:  # noqa: BLE001
                log(f"send failed: {e}")
                return 1
            if self.closed.is_set():
                return 1
        return 0  # stdin closed: the client is done with us


def main(argv):
    if len(argv) != 2:
        log("usage: mcp_sse_bridge.py <sse-url>")
        return 2
    return Bridge(argv[1], os.environ.get("MCP_BRIDGE_AUTHORIZATION", "")).run()


if __name__ == "__main__":
    sys.exit(main(sys.argv))
