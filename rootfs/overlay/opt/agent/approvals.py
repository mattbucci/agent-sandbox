#!/usr/bin/env python3
"""Run approval gate + danger policy for the deepagents in-VM harness.

This implements the server-side half of the Hermes *runs API*'s interactive
dangerous-command approval flow. When the agent's shell backend
(ApprovalShellBackend in agent.py) is about to run a command classified
dangerous, it blocks on this gate, which:

  1. emits an ``approval.request`` lifecycle event onto the run's event queue
     (drained by GET /v1/runs/{id}/events), and
  2. waits for the client's decision (posted to POST /v1/runs/{id}/approval),
     then returns "once" | "session" | "always" | "deny".

Threading model (mirrors the real hermes-agent api_server): the run is driven on
gateway_server's persistent asyncio loop; the shell tool — and therefore this
gate's blocking wait — runs in a langchain executor THREAD (langchain copies the
context into that thread, so ``current_run`` is visible); the approval HTTP
request is handled on its own ThreadingHTTPServer thread and unblocks the wait
via a plain threading.Event. The event queue is an asyncio.Queue, so cross-thread
pushes go through ``loop.call_soon_threadsafe``.

On the plain chat/task paths ``current_run`` is unset, so the shell backend never
gates — those paths are byte-for-byte unchanged.
"""

import contextvars
import logging
import re
import threading
import time
import uuid

logger = logging.getLogger("gateway.approvals")

# The active run for the current agent invocation. Set on the run's driving
# coroutine (gateway_server._drive_run); None everywhere else.
current_run = contextvars.ContextVar("current_run", default=None)

# How long the shell backend blocks on an approval before defaulting to deny
# (a safe, non-destructive default so a walked-away client can't wedge a slot).
APPROVAL_TIMEOUT_S = 600

# Retain a terminal run's status/record this long so late status polls and
# /events reconnects still resolve before the sweeper drops it.
RUN_RETENTION_S = 600

_WORKSPACE = "/home/agent/workspace"

# Conservative danger policy: a command matching any pattern requires approval.
# Tunable — err toward prompting; a false positive costs one click, a false
# negative runs a destructive command unattended.
_DANGER_PATTERNS = [
    r"\brm\s+(-[a-zA-Z]*\s+)*(-[a-zA-Z]*r[a-zA-Z]*|--recursive)\b",  # rm -r / -rf
    r"\brm\s+[^|&;]*\*",                                             # rm with a glob
    r"\bdd\b",
    r"\bmkfs\b", r"\bmke2fs\b", r"\bwipefs\b",
    r"\bfdisk\b", r"\bparted\b", r"\bsgdisk\b",
    r":\s*\(\)\s*\{.*\|.*&\s*\}\s*;",                                # fork bomb
    r"\bshutdown\b", r"\breboot\b", r"\bhalt\b", r"\bpoweroff\b", r"\binit\s+0\b",
    r"\bsudo\b", r"\bsu\s+-?\b",
    r"\bchmod\s+-[a-zA-Z]*R", r"\bchown\s+-[a-zA-Z]*R",
    r"\b(curl|wget)\b[^|&;]*\|\s*(sudo\s+)?(bash|sh|zsh|python[0-9.]*)\b",  # curl | sh
    r">\s*/dev/(sd|nvme|vd|mmcblk)",                                 # write to a raw disk
    r"\bgit\s+push\b[^|&;]*(--force\b|-f\b)",
    r"\bgit\b[^|&;]*\bpush\b[^|&;]*(--force\b|-f\b)",
    r"\bkill\s+-9\s+-1\b", r"\bkillall\b", r"\bpkill\b",
    r"\btruncate\b",
    r"\buserdel\b", r"\bgroupdel\b", r"\busermod\b",
    r"\biptables\b", r"\bnft\b", r"\bmount\b", r"\bumount\b",
]
_DANGER_RE = [re.compile(p) for p in _DANGER_PATTERNS]


def is_dangerous(command: str) -> bool:
    """Report whether a shell command should require interactive approval."""
    if not command or not isinstance(command, str):
        return False
    cmd = command.strip()
    for rx in _DANGER_RE:
        if rx.search(cmd):
            return True
    # An output redirect to an absolute path outside the workspace (and not a
    # harmless /tmp or /dev/null target) writes to the wider filesystem.
    for m in re.finditer(r">>?\s*(/[^\s;|&]+)", cmd):
        target = m.group(1)
        if (not target.startswith(_WORKSPACE)
                and not target.startswith("/tmp")
                and target != "/dev/null"):
            return True
    return False


class RunContext:
    """Server-side state for one active run: event queue, status, and the
    pending-approval gate. Thread-safe."""

    def __init__(self, run_id, loop, queue, session_id=None, model=None):
        self.run_id = run_id
        self.loop = loop
        self.queue = queue  # asyncio.Queue drained by the events SSE
        self.task_future = None  # concurrent.futures.Future of the driving coro
        self._lock = threading.Lock()
        self._pending = {}       # approval_id -> (threading.Event, holder{"choice"})
        self._blanket_allow = False  # a session/always decision allows the rest
        now = time.time()
        self._status = {
            "object": "hermes.run",
            "run_id": run_id,
            "status": "queued",
            "created_at": now,
            "updated_at": now,
            "session_id": session_id or run_id,
            "model": model,
            "last_event": None,
        }

    # ---- event + status plumbing ----
    def emit(self, event: dict):
        """Push a lifecycle event to the run's SSE queue (from any thread)."""
        event.setdefault("run_id", self.run_id)
        event.setdefault("timestamp", time.time())
        with self._lock:
            self._status["last_event"] = event.get("event")
            self._status["updated_at"] = time.time()
        try:
            self.loop.call_soon_threadsafe(self.queue.put_nowait, event)
        except Exception:
            logger.debug("emit dropped for run %s", self.run_id, exc_info=True)

    def close(self):
        """Signal the events SSE to end (sentinel)."""
        try:
            self.loop.call_soon_threadsafe(self.queue.put_nowait, None)
        except Exception:
            pass

    def set_status(self, status, **fields):
        with self._lock:
            self._status["status"] = status
            self._status["updated_at"] = time.time()
            for k, v in fields.items():
                if v is not None:
                    self._status[k] = v

    def snapshot_status(self):
        with self._lock:
            return dict(self._status)

    def is_terminal(self):
        with self._lock:
            return self._status["status"] in ("completed", "failed", "cancelled")

    # ---- approval gate ----
    def request_approval(self, command, description=""):
        """Block (in the shell tool's executor thread) until the client resolves
        an approval for `command`; return the decision. A prior session/always
        decision short-circuits to allow."""
        with self._lock:
            if self._blanket_allow:
                return "session"
        approval_id = "appr_" + uuid.uuid4().hex
        ev = threading.Event()
        holder = {"choice": None}
        with self._lock:
            self._pending[approval_id] = (ev, holder)
        self.set_status("waiting_for_approval")
        self.emit({
            "event": "approval.request",
            "approval_id": approval_id,
            "command": command,
            "description": description or f"Run shell command: {command}",
            "choices": ["once", "session", "always", "deny"],
        })
        if not ev.wait(timeout=APPROVAL_TIMEOUT_S):
            logger.warning("approval %s for run %s timed out; defaulting to deny",
                           approval_id, self.run_id)
        with self._lock:
            self._pending.pop(approval_id, None)
            choice = holder["choice"] or "deny"
            if choice in ("session", "always"):
                self._blanket_allow = True
        self.set_status("running")
        return choice

    def resolve(self, choice) -> int:
        """Resolve every pending approval for this run with `choice`; return the
        count resolved (0 => nothing was pending)."""
        with self._lock:
            pending = list(self._pending.items())
        n = 0
        for _aid, (ev, holder) in pending:
            holder["choice"] = choice
            ev.set()
            n += 1
        if n:
            self.emit({"event": "approval.responded", "choice": choice, "resolved": n})
        return n


# ---------------------------------------------------------------------------
# Process-wide run registry.
# ---------------------------------------------------------------------------
_RUNS_LOCK = threading.Lock()
_RUNS = {}  # run_id -> RunContext


def register_run(ctx: RunContext):
    with _RUNS_LOCK:
        _RUNS[ctx.run_id] = ctx


def get_run(run_id) -> "RunContext | None":
    with _RUNS_LOCK:
        return _RUNS.get(run_id)


def drop_run(run_id):
    with _RUNS_LOCK:
        _RUNS.pop(run_id, None)


def sweep_terminal(now=None):
    """Drop terminal runs whose retention window has elapsed. Returns the count
    dropped (for logging/tests)."""
    now = now if now is not None else time.time()
    dropped = 0
    with _RUNS_LOCK:
        for run_id in list(_RUNS):
            ctx = _RUNS[run_id]
            snap = ctx.snapshot_status()
            if snap["status"] in ("completed", "failed", "cancelled") and \
                    now - snap["updated_at"] > RUN_RETENTION_S:
                del _RUNS[run_id]
                dropped += 1
    return dropped
