#!/usr/bin/env python3
"""
stdio <-> HTTP bridge for a remote MCP server (standard library only).

    mcp_sse_bridge.py <sse-url>            # legacy HTTP+SSE transport
    mcp_sse_bridge.py --http <mcp-url>     # streamable HTTP transport
                                           # bearer from MCP_BRIDGE_AUTHORIZATION

An MCP client that speaks stdio launches this as its "server"; the bridge
relays newline-delimited JSON-RPC between that client and a remote MCP server.

  HTTP+SSE         GET <sse-url> for the event stream, POST to the endpoint the
                   stream announces (mnemosyne).
  streamable HTTP  POST each message to <mcp-url>; the reply comes back in the
                   response body, as JSON or as an event stream (Hindsight).

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
import urllib.error
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


class HttpBridge:
    """stdio <-> streamable-HTTP MCP (POST per message, reply in the body)."""

    # A tool call can run for minutes (Hindsight's reflect reasons with an LLM).
    REQUEST_TIMEOUT_S = 900

    def __init__(self, url, authorization=""):
        self.url = url
        self.headers = {"Authorization": authorization} if authorization else {}
        self.session_id = None
        self._initialize = None    # the client's handshake, kept to replay
        self._initialized = None
        self._out_lock = threading.Lock()
        self._state_lock = threading.Lock()
        self._resume_lock = threading.Lock()

    def _emit(self, out, text):
        with self._out_lock:
            out.write(text.replace("\n", " ") + "\n")
            out.flush()

    def _error(self, out, message, reason):
        """Answer a request ourselves so the client does not wait forever."""
        if isinstance(message, dict) and "id" in message:
            self._emit(out, json.dumps({
                "jsonrpc": "2.0", "id": message["id"],
                "error": {"code": -32000, "message": f"mcp bridge: {reason}"}}))

    def _post(self, line, out, session_id):
        """POST one message and relay the reply. out=None discards it."""
        headers = {**self.headers, "Content-Type": "application/json",
                   "Accept": "application/json, text/event-stream"}
        if session_id:
            headers["Mcp-Session-Id"] = session_id
        req = urllib.request.Request(self.url, data=line.encode("utf-8"), method="POST", headers=headers)
        emit = (lambda text: self._emit(out, text)) if out is not None else (lambda text: None)
        with urllib.request.urlopen(req, timeout=self.REQUEST_TIMEOUT_S) as resp:
            sid = resp.headers.get("Mcp-Session-Id")
            if sid:
                with self._state_lock:
                    self.session_id = sid
            ctype = (resp.headers.get("Content-Type") or "").lower()
            if "text/event-stream" in ctype:
                data = []
                for raw in resp:
                    text = raw.decode("utf-8", "replace").rstrip("\r\n")
                    if text == "":
                        if data:
                            emit("\n".join(data))
                        data = []
                    elif text.startswith("data:"):
                        data.append(text[5:].lstrip(" "))
                if data:
                    emit("\n".join(data))
            else:
                body = resp.read().decode("utf-8", "replace").strip()
                if body:  # notifications are answered 202 with no body
                    emit(body)

    def _resume(self, stale):
        """Open a new session after the server forgot ours (it restarted).

        Replays the client's own handshake; the client already has its answer,
        so the replies are dropped. Several requests can fail at once: only the
        first one to get here reconnects.
        """
        with self._resume_lock:
            with self._state_lock:
                if self.session_id != stale:
                    return True  # another request already reconnected
                self.session_id = None
            if not self._initialize:
                return False
            log("session expired on the server; starting a new one")
            self._post(self._initialize, None, None)
            if self._initialized:
                with self._state_lock:
                    sid = self.session_id
                self._post(self._initialized, None, sid)
            return True

    def handle(self, line, out):
        message = json.loads(line)
        method = message.get("method") if isinstance(message, dict) else None
        if method == "initialize":
            self._initialize = line
        elif method == "notifications/initialized":
            self._initialized = line
        with self._state_lock:
            sid = self.session_id
        try:
            try:
                self._post(line, out, sid)
            except urllib.error.HTTPError as e:
                # 404 for a request that named a session means the session is
                # gone (MCP streamable HTTP): reconnect and send it again.
                if e.code != 404 or not sid or method == "initialize" or not self._resume(sid):
                    raise
                with self._state_lock:
                    sid = self.session_id
                self._post(line, out, sid)
        except Exception as e:  # noqa: BLE001
            log(f"request failed: {e}")
            self._error(out, message, str(e))

    def run(self, stdin=None, out=None):
        stdin = stdin or sys.stdin
        out = out or sys.stdout
        workers = []
        for line in stdin:
            line = line.strip()
            if not line:
                continue
            try:
                message = json.loads(line)
            except ValueError:
                log("dropping non-JSON input line")
                continue
            if isinstance(message, dict) and message.get("method") == "initialize":
                # The handshake establishes the session id every later request
                # carries, so it must finish before anything else is sent.
                self.handle(line, out)
                continue
            # Everything else runs concurrently: one slow tool call must not
            # hold up the others.
            t = threading.Thread(target=self.handle, args=(line, out), daemon=True)
            t.start()
            workers.append(t)
        for t in workers:
            t.join(timeout=self.REQUEST_TIMEOUT_S)
        return 0


def main(argv):
    auth = os.environ.get("MCP_BRIDGE_AUTHORIZATION", "")
    if len(argv) == 3 and argv[1] == "--http":
        return HttpBridge(argv[2], auth).run()
    if len(argv) == 2:
        return Bridge(argv[1], auth).run()
    log("usage: mcp_sse_bridge.py <sse-url> | --http <mcp-url>")
    return 2


if __name__ == "__main__":
    sys.exit(main(sys.argv))
