"""
Tests for the per-agent memory access policy logic
(lib/hindsight_ext/memory_policy.py) — the decisions the Hindsight extension
makes, without Hindsight.

Run:  python3 test/test_memory_policy.py
"""
import json
import os
import sys
import tempfile
import time
import unittest

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "lib", "hindsight_ext"))
import memory_policy as mp  # noqa: E402

POLICY = {
    "version": 1,
    "principals": {
        "admin": {"admin": True, "token_sha256": mp.token_digest("master")},
        "coder": {
            "token_sha256": mp.token_digest(mp.agent_token("master", "coder")),
            "banks": {
                "main": {"read": ["agent:coder"], "read_untagged": True, "write": ["agent:coder"],
                         "default_tags": ["agent:coder"]},
                "library": {"read_all": True, "write": []},
            },
            "mcp_tools": ["recall", "retain"],
        },
    },
}


class TokenTest(unittest.TestCase):
    def test_agent_tokens_are_deterministic_and_distinct(self):
        self.assertEqual(mp.agent_token("m", "coder"), mp.agent_token("m", "coder"))
        self.assertNotEqual(mp.agent_token("m", "coder"), mp.agent_token("m", "debugger"))
        self.assertNotEqual(mp.agent_token("m", "coder"), mp.agent_token("other", "coder"))

    def test_authenticate(self):
        policy = mp.Policy(POLICY)
        self.assertEqual(policy.authenticate("master").name, "admin")
        self.assertEqual(policy.authenticate(mp.agent_token("master", "coder")).name, "coder")
        for bad in ("", None, "nope", mp.agent_token("master", "debugger"), mp.token_digest("master")):
            self.assertIsNone(policy.authenticate(bad), bad)


class BankAccessTest(unittest.TestCase):
    def access(self, **spec):
        return mp.BankAccess(spec)

    def test_read_scope(self):
        self.assertIsNone(self.access(read_all=True).read_scope())                      # unrestricted
        self.assertEqual(self.access(read=["a"], read_untagged=True).read_scope(),
                         {"tags": ["a"], "match": "any"})                              # own + shared
        self.assertEqual(self.access(read=["a"]).read_scope(),
                         {"tags": ["a"], "match": "any_strict"})                       # own only
        self.assertEqual(self.access(read_untagged=True).read_scope(),
                         {"tags": [], "match": "exact"})                               # shared only
        nothing = self.access().read_scope()
        self.assertEqual(nothing["match"], "any_strict")                                # matches no memory
        self.assertFalse(self.access().can_read)

    def test_write_scope(self):
        own = self.access(write=["agent:coder"])
        self.assertTrue(own.may_write_tags(["agent:coder"]))
        self.assertFalse(own.may_write_tags(["agent:debugger"]))            # another agent's tag
        self.assertFalse(own.may_write_tags(["agent:coder", "agent:debugger"]))
        self.assertFalse(own.may_write_tags([]))                             # untagged = shared: admin-only
        self.assertTrue(self.access(write=["project:*"]).may_write_tags(["project:apollo"]))
        self.assertFalse(self.access().may_write_tags(["agent:coder"]))      # read-only

    def test_label_tags_ride_along_but_grant_nothing(self):
        own = self.access(write=["agent:hermes"])
        # hermes-agent stamps each retain with its session.
        self.assertTrue(own.may_write_tags(["agent:hermes", "session:api-1"]))
        self.assertFalse(own.may_write_tags(["session:api-1"]))               # owned by nobody
        self.assertFalse(own.may_write_tags(["agent:coder", "session:api-1"]))
        self.assertFalse(self.access().may_write_tags(["agent:hermes", "session:api-1"]))
        self.assertEqual(own.write_scope(), ["agent:hermes", "session:*"])
        self.assertEqual(self.access().write_scope(), [])
        self.assertFalse(self.access().can_write)

    def test_principal_bank_lookup(self):
        coder = mp.Policy(POLICY).principal("coder")
        self.assertIsNotNone(coder.bank("main"))
        self.assertIsNone(coder.bank("elsewhere"))                           # no access at all
        self.assertTrue(coder.bank("library").read_all)
        self.assertEqual(coder.mcp_tools, frozenset({"recall", "retain"}))


class PolicyFileTest(unittest.TestCase):
    def setUp(self):
        self.dir = tempfile.TemporaryDirectory()
        self.addCleanup(self.dir.cleanup)
        self.path = os.path.join(self.dir.name, "policy.json")

    def write(self, data):
        with open(self.path, "w") as f:
            f.write(data if isinstance(data, str) else json.dumps(data))
        # make sure the stamp moves even on coarse filesystem clocks
        t = time.time() + self.write.__dict__.setdefault("n", 0)
        self.write.__dict__["n"] += 5
        os.utime(self.path, (t, t))

    def test_missing_file_is_closed_not_open(self):
        self.assertIsNone(mp.PolicyFile(self.path).get().authenticate("master"))

    def test_corrupt_file_is_closed_not_open(self):
        self.write("{not json")
        self.assertIsNone(mp.PolicyFile(self.path).get().authenticate("master"))

    def test_reloads_when_the_file_changes(self):
        pf = mp.PolicyFile(self.path)
        self.write(POLICY)
        self.assertIsNotNone(pf.get().authenticate(mp.agent_token("master", "coder")))
        revoked = {"version": 1, "principals": {"admin": POLICY["principals"]["admin"]}}
        self.write(revoked)
        self.assertIsNone(pf.get().authenticate(mp.agent_token("master", "coder")))   # access removed without restart
        self.assertEqual(pf.get().authenticate("master").name, "admin")


if __name__ == "__main__":
    unittest.main()
