"""
Per-agent memory access policy — pure logic, no Hindsight imports.

The policy file (state/hindsight/policy.json, written by `agentconf.py
memory-policy`) says, for each principal, which banks it may use and — inside a
bank — which tags it may read and write:

    {
      "version": 1,
      "principals": {
        "admin": {"token_sha256": "…", "admin": true},
        "coder": {
          "token_sha256": "…",
          "banks": {
            "agent-sandbox": {
              "read": ["agent:coder"],     # tags it may read
              "read_untagged": true,       # …and the shared (untagged) scope
              "read_all": false,           # everything in the bank, unscoped
              "write": ["agent:coder"],    # tag patterns it may write
              "default_tags": ["agent:coder"]   # put on writes that carry none
            }
          },
          "mcp_tools": ["recall", "reflect", "retain"]
        }
      }
    }

Tags are how Hindsight scopes data inside a bank: a memory, an observation and a
mental model each carry tags, and "untagged" is the shared scope. So the same
read list decides which memories an agent can recall and which mental models it
can see.

This module answers questions about that file; sandbox_memory_policy.py plugs
the answers into Hindsight's extension hooks. Keeping the logic here lets it be
tested without Hindsight installed.
"""

import fnmatch
import hashlib
import hmac
import json
import os
import threading

ADMIN = "admin"


def token_digest(token: str) -> str:
    return hashlib.sha256((token or "").encode("utf-8")).hexdigest()


def agent_token(master_token: str, agent_type: str) -> str:
    """The bearer token for one agent, derived from the master token.

    Deterministic, so nothing has to be stored or distributed: the config
    compiler derives the same value when it writes the agent's /etc/agent.conf
    and when it writes the policy. Knowing one agent's token reveals nothing
    about another's or about the master.
    """
    mac = hmac.new((master_token or "").encode("utf-8"),
                   f"agent-sandbox-memory:{agent_type}".encode("utf-8"), hashlib.sha256)
    return f"hsa_{mac.hexdigest()[:48]}"


# Bookkeeping tags a client attaches beside the tag that says whose memory it
# is (hermes-agent's Hindsight provider stamps every retain with the session).
# They decide nothing about access: a label never makes an item writable, and
# no read scope names one.
LABEL_TAG_PATTERNS = ("session:*",)


def is_label_tag(tag):
    return any(fnmatch.fnmatchcase(str(tag), pat) for pat in LABEL_TAG_PATTERNS)


class BankAccess:
    """What one principal may do in one bank."""

    def __init__(self, spec):
        spec = spec or {}
        self.read = [str(t) for t in spec.get("read") or []]
        self.read_untagged = bool(spec.get("read_untagged", False))
        self.read_all = bool(spec.get("read_all", False))
        self.write = [str(t) for t in spec.get("write") or []]
        self.default_tags = [str(t) for t in spec.get("default_tags") or []]

    @property
    def can_read(self):
        return self.read_all or self.read_untagged or bool(self.read)

    @property
    def can_write(self):
        return bool(self.write)

    def read_scope(self):
        """The read confinement as a tag-group spec, or None for unrestricted.

        Returns {"tags": [...], "match": mode} — Hindsight's leaf tag group:
          any         one of the tags, or untagged (own + shared)
          any_strict  one of the tags, untagged excluded
          exact + []  only untagged
        """
        if self.read_all:
            return None
        if self.read:
            return {"tags": list(self.read), "match": "any" if self.read_untagged else "any_strict"}
        if self.read_untagged:
            return {"tags": [], "match": "exact"}
        # Nothing readable: a tag no memory carries.
        return {"tags": ["\x00none"], "match": "any_strict"}

    def may_write_tags(self, tags):
        """True when the tags name memory this principal may write.

        Every ownership tag must be covered by a write pattern, and there must
        be at least one: an item carrying only label tags would be owned by
        nobody, which is as good as shared.
        """
        tags = [str(t) for t in tags or []]
        owning = [t for t in tags if not is_label_tag(t)]
        if not self.write or not owning:
            return False  # untagged belongs to everyone: agents never write it
        return all(any(fnmatch.fnmatchcase(t, pat) for pat in self.write) for t in owning)

    def write_scope(self):
        """Patterns handed to Hindsight's own write check: the write patterns
        plus the label tags a client may attach beside them."""
        return list(self.write) + list(LABEL_TAG_PATTERNS) if self.write else []


class Principal:
    def __init__(self, name, spec):
        self.name = name
        self.admin = bool(spec.get("admin", False))
        self.token_sha256 = spec.get("token_sha256") or ""
        self.banks = {bank: BankAccess(access) for bank, access in (spec.get("banks") or {}).items()}
        tools = spec.get("mcp_tools")
        self.mcp_tools = None if tools is None else frozenset(tools)

    def bank(self, bank_id):
        """Access to `bank_id`, or None when the principal has none."""
        return self.banks.get(bank_id)


class Policy:
    """The parsed policy file. Immutable; reload by constructing a new one."""

    def __init__(self, data=None):
        data = data or {}
        self.principals = {name: Principal(name, spec or {})
                           for name, spec in (data.get("principals") or {}).items()}
        self._by_digest = {p.token_sha256: p for p in self.principals.values() if p.token_sha256}

    def authenticate(self, token):
        """The principal a bearer token belongs to, or None."""
        if not token:
            return None
        digest = token_digest(token)
        for known, principal in self._by_digest.items():
            if hmac.compare_digest(known, digest):
                return principal
        return None

    def principal(self, name):
        return self.principals.get(name) if name else None


class PolicyFile:
    """A Policy backed by a file, re-read when the file changes.

    `sandbox-ctl memory apply` rewrites the file; the running service picks the
    change up on the next request, so changing access needs no restart. A file
    that is missing or unreadable yields an EMPTY policy — nobody authenticates —
    rather than an open one.
    """

    def __init__(self, path):
        self.path = path
        self._lock = threading.Lock()
        self._stamp = None
        self._policy = Policy()

    def get(self):
        try:
            st = os.stat(self.path)
            stamp = (st.st_mtime_ns, st.st_size)
        except OSError:
            stamp = None
        with self._lock:
            if stamp != self._stamp:
                self._stamp = stamp
                self._policy = self._load() if stamp else Policy()
            return self._policy

    def _load(self):
        try:
            with open(self.path) as f:
                return Policy(json.load(f))
        except (OSError, ValueError):
            return Policy()  # fail closed
