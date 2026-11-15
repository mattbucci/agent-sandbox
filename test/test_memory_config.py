"""
Tests for the memory part of the config compiler (lib/agentconf.py): backend
selection and the declared mental-model plan.

Run:  python3 test/test_memory_config.py     (needs PyYAML)
"""
import os
import sys
import unittest

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "lib"))
sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "lib", "hindsight_ext"))
import agentconf  # noqa: E402
import memory_policy  # noqa: E402

GLOBAL = {
    "llm": {"model": "gemma"},
    "memory": {
        "enabled": True, "backend": "hindsight", "token": "hs_x",
        "mental_models": [
            {"id": "environment", "name": "Sandbox environment", "query": "What is\n  the environment?"},
            {"name": "User Preferences", "query": "What do users prefer?"},
        ],
    },
}
AGENTS = {
    "coder": {"agent": {"type": "coder", "name": "Coder"},
              "memory": {"mental_models": [{"id": "playbook", "name": "Coder playbook", "query": "Conventions?"}]}},
    "debugger": {"agent": {"type": "debugger", "name": "Debugger"}},
}


class MemorySettingsTest(unittest.TestCase):
    def test_absent_backend_stays_mnemosyne(self):
        s = agentconf.memory_settings({"memory": {"enabled": True, "port": 8077}})
        self.assertEqual((s["backend"], s["port"]), ("mnemosyne", 8077))

    def test_hindsight_defaults(self):
        s = agentconf.memory_settings(GLOBAL)
        self.assertEqual(s["backend"], "hindsight")
        self.assertEqual(s["port"], 8890)          # 8888 belongs to the OTel collector
        self.assertEqual(s["bank"], "agent-sandbox")
        self.assertEqual(s["llm_model"], "gemma")  # falls back to llm.model
        self.assertEqual(s["recall_scope"], "shared")


class MemoryPlanTest(unittest.TestCase):
    def test_shared_models_are_untagged_and_agent_models_are_scoped(self):
        plan = agentconf.memory_plan(GLOBAL, AGENTS)
        self.assertEqual(plan["errors"], [])
        models = {m["id"]: m for m in plan["mental_models"]}
        self.assertEqual(sorted(models), ["coder-playbook", "environment", "user-preferences"])
        self.assertEqual(models["environment"]["tags"], [])
        self.assertEqual(models["coder-playbook"]["tags"], ["agent:coder"])
        # ids are stable slugs; multi-line YAML queries are folded to one line
        self.assertEqual(models["environment"]["source_query"], "What is the environment?")

    def test_incomplete_and_duplicate_models_are_errors(self):
        bad = {"memory": {"mental_models": [{"name": "No query"}, {"name": "A", "query": "q"}, {"id": "a", "name": "B", "query": "q"}]}}
        errors = agentconf.memory_plan(bad, {})["errors"]
        self.assertEqual(len(errors), 2)
        self.assertIn("'name' and 'query'", errors[0])
        self.assertIn("duplicate", errors[1])


class AgentConfTest(unittest.TestCase):
    def conf(self, global_config, agent):
        text = agentconf.compile_agent_conf(agent, global_config)
        return dict(line.split("=", 1) for line in text.splitlines() if "=" in line and not line.startswith("#"))

    def test_hindsight_backend_enables_hindsight_not_mnemosyne(self):
        conf = self.conf(GLOBAL, AGENTS["coder"])
        self.assertEqual(conf["HINDSIGHT_ENABLED"], '"1"')
        self.assertEqual(conf["MNEMOSYNE_ENABLED"], '"0"')
        self.assertEqual(conf["HINDSIGHT_BANK"], '"agent-sandbox"')

    def test_agent_gets_its_own_token_never_the_master(self):
        coder = self.conf(GLOBAL, AGENTS["coder"])["HINDSIGHT_TOKEN"].strip('"')
        debugger = self.conf(GLOBAL, AGENTS["debugger"])["HINDSIGHT_TOKEN"].strip('"')
        self.assertTrue(coder.startswith("hsa_"))
        self.assertNotEqual(coder, "hs_x")
        self.assertNotEqual(coder, debugger)
        self.assertEqual(coder, memory_policy.agent_token("hs_x", "coder"))

    def test_mnemosyne_backend_is_unchanged(self):
        conf = self.conf({"memory": {"enabled": True, "token": "m"}}, AGENTS["coder"])
        self.assertEqual(conf["MNEMOSYNE_ENABLED"], '"1"')
        self.assertEqual(conf["HINDSIGHT_ENABLED"], '"0"')
        self.assertEqual(conf["HINDSIGHT_TOKEN"], '""')   # not handed out when unused

    def test_agent_can_opt_out_and_use_another_primary_bank(self):
        agent = {"agent": {"type": "x", "name": "X"}, "memory": {"enabled": False}}
        conf = self.conf(GLOBAL, agent)
        self.assertEqual((conf["HINDSIGHT_ENABLED"], conf["HINDSIGHT_TOKEN"]), ('"0"', '""'))
        agent = {"agent": {"type": "x", "name": "X"}, "memory": {"bank": "private"}}
        self.assertEqual(self.conf(GLOBAL, agent)["HINDSIGHT_BANK"], '"private"')

    def test_extra_banks_lists_only_readable_ones(self):
        agent = {"agent": {"type": "x", "name": "X"},
                 "memory": {"banks": {"research": {"read": ["all"], "write": []},
                                      "dropbox": {"read": [], "write": ["own"]}}}}
        self.assertEqual(self.conf(GLOBAL, agent)["HINDSIGHT_EXTRA_BANKS"], '"research"')

    def test_model_limits_only_when_configured(self):
        self.assertNotIn("OMP_MAX_TOKENS", self.conf(GLOBAL, AGENTS["coder"]))
        g = {**GLOBAL, "llm": {"model": "q", "context_window": 32768, "max_output_tokens": 8192}}
        agent = {"agent": {"type": "x", "name": "X"}, "vm": {"max_output_tokens": 4096}}
        conf = self.conf(g, agent)
        self.assertEqual((conf["OMP_CONTEXT_WINDOW"], conf["OMP_MAX_TOKENS"]), ('"32768"', '"4096"'))


class AccessPolicyTest(unittest.TestCase):
    """memory.access / memory.banks -> the policy the service enforces."""

    def policy(self, agents, memory=None):
        g = {"memory": {"enabled": True, "backend": "hindsight", "token": "hs_x", **(memory or {})}}
        plan = agentconf.memory_plan(g, agents)
        return plan["policy"]["principals"], plan["errors"]

    def agent(self, **memory):
        return {"agent": {"type": "x", "name": "X"}, "memory": memory} if memory else {"agent": {"type": "x", "name": "X"}}

    def test_default_is_read_everything_write_own(self):
        principals, errors = self.policy({"coder": self.agent(), "debugger": self.agent()})
        self.assertEqual(errors, [])
        entry = principals["coder"]["banks"]["agent-sandbox"]
        self.assertTrue(entry["read_all"])
        self.assertEqual(entry["write"], ["agent:coder"])
        self.assertEqual(entry["default_tags"], ["agent:coder"])

    def test_admin_is_the_master_token_and_agents_are_derived(self):
        principals, _ = self.policy({"coder": self.agent()})
        self.assertTrue(principals["admin"]["admin"])
        self.assertEqual(principals["admin"]["token_sha256"], memory_policy.token_digest("hs_x"))
        self.assertEqual(principals["coder"]["token_sha256"],
                         memory_policy.token_digest(memory_policy.agent_token("hs_x", "coder")))
        self.assertNotIn("hs_x", str(principals))          # digests only

    def test_access_words_resolve_to_tags(self):
        agents = {"coder": self.agent(access={"read": ["own", "shared", "debugger", "team:platform"], "write": ["own"]}),
                  "debugger": self.agent()}
        entry = self.policy(agents)[0]["coder"]["banks"]["agent-sandbox"]
        self.assertEqual(entry["read"], ["agent:coder", "agent:debugger", "team:platform"])
        self.assertTrue(entry["read_untagged"])
        self.assertFalse(entry["read_all"])

    def test_global_default_and_per_agent_override(self):
        agents = {"coder": self.agent(), "security": self.agent(access={"read": ["own"], "write": ["own"]})}
        principals, _ = self.policy(agents, {"access": {"read": ["own", "shared"], "write": ["own"]}})
        self.assertTrue(principals["coder"]["banks"]["agent-sandbox"]["read_untagged"])
        sec = principals["security"]["banks"]["agent-sandbox"]
        self.assertEqual((sec["read"], sec["read_untagged"]), (["agent:security"], False))

    def test_legacy_recall_scope_agent_means_own_and_shared(self):
        entry = self.policy({"coder": self.agent(recall_scope="agent")})[0]["coder"]["banks"]["agent-sandbox"]
        self.assertEqual((entry["read"], entry["read_untagged"], entry["read_all"]), (["agent:coder"], True, False))

    def test_read_only_and_additional_banks(self):
        agents = {"researcher": self.agent(access={"read": ["all"], "write": []},
                                           banks={"research": {"read": ["all"], "write": ["own"]}})}
        principals, errors = self.policy(agents, {"banks": {"research": {"mission": "Papers."}}})
        self.assertEqual(errors, [])
        banks = principals["researcher"]["banks"]
        self.assertEqual(banks["agent-sandbox"]["write"], [])
        self.assertEqual(banks["agent-sandbox"]["default_tags"], [])
        self.assertEqual(banks["research"]["write"], ["agent:researcher"])

    def test_agents_cannot_be_granted_shared_or_whole_bank_writes(self):
        for word in ("shared", "all"):
            _, errors = self.policy({"coder": self.agent(access={"read": ["all"], "write": [word]})})
            self.assertEqual(len(errors), 1, word)
            self.assertIn("admin-only", errors[0])

    def test_undeclared_extra_bank_is_an_error(self):
        _, errors = self.policy({"coder": self.agent(banks={"nowhere": {"read": ["all"]}})})
        self.assertTrue(any("not declared under memory.banks" in e for e in errors))

    def test_agent_without_memory_has_no_principal(self):
        principals, _ = self.policy({"coder": self.agent(enabled=False)})
        self.assertNotIn("coder", principals)


class MentalModelScopeTest(unittest.TestCase):
    """visible_to (who may read a model) and sources (what it is built from)."""

    def models(self, shared=(), agents=None, banks=None):
        g = {"memory": {"enabled": True, "backend": "hindsight", "token": "t",
                        "mental_models": list(shared), "banks": banks or {}}}
        plan = agentconf.memory_plan(g, agents or {"coder": {"agent": {"type": "coder"}}, "debugger": {"agent": {"type": "debugger"}}})
        return {(m["bank"], m["id"]): m for m in plan["mental_models"]}, plan["errors"]

    EVERYTHING = [{"not": {"tags": ["__no_such_tag__"], "match": "any_strict"}}]

    def test_shared_model_defaults_to_everyone_and_the_whole_bank(self):
        models, errors = self.models([{"name": "Env", "query": "q"}])
        m = models[("agent-sandbox", "env")]
        self.assertEqual((errors, m["tags"], m["trigger"]["tag_groups"]), ([], [], self.EVERYTHING))
        self.assertTrue(m["trigger"]["refresh_after_consolidation"])

    def test_visibility_and_sources_are_independent(self):
        models, _ = self.models([{"id": "handoff", "name": "Handoff", "query": "q",
                                  "visible_to": ["coder", "debugger"], "sources": ["debugger"]},
                                 {"id": "digest", "name": "Digest", "query": "q", "visible_to": ["coder"], "sources": "all"}])
        handoff = models[("agent-sandbox", "handoff")]
        self.assertEqual(handoff["tags"], ["agent:coder", "agent:debugger"])
        self.assertEqual(handoff["trigger"]["tag_groups"], [{"tags": ["agent:debugger"], "match": "any_strict"}])
        digest = models[("agent-sandbox", "digest")]
        self.assertEqual((digest["tags"], digest["trigger"]["tag_groups"]), (["agent:coder"], self.EVERYTHING))

    def test_agent_model_defaults_to_that_agent_for_both(self):
        agents = {"coder": {"agent": {"type": "coder"}, "memory": {"mental_models": [{"id": "pb", "name": "PB", "query": "q"}]}}}
        m = self.models(agents=agents)[0][("agent-sandbox", "coder-pb")]
        self.assertEqual(m["tags"], ["agent:coder"])
        self.assertEqual(m["trigger"]["tag_groups"], [{"tags": ["agent:coder"], "match": "any_strict"}])

    def test_models_in_additional_banks(self):
        models, errors = self.models(banks={"research": {"mental_models": [{"name": "Reading list", "query": "q"}]}})
        self.assertEqual(errors, [])
        self.assertIn(("research", "reading-list"), models)

    def test_bad_scope_is_an_error(self):
        _, errors = self.models([{"name": "X", "query": "q", "visible_to": []}])
        self.assertTrue(any("visible_to" in e for e in errors))


class TagGroupNormalizationTest(unittest.TestCase):
    def test_server_defaults_do_not_count_as_a_change(self):
        ours = [{"not": {"tags": ["x"], "match": "any_strict"}}]
        theirs = [{"not": {"tags": ["x"], "match": "any_strict", "resolve": "exact"}}]
        self.assertEqual(agentconf._norm_tag_groups(theirs), ours)
        self.assertNotEqual(agentconf._norm_tag_groups([{"tags": ["x"], "match": "any_strict", "resolve": "fuzzy"}]),
                            [{"tags": ["x"], "match": "any_strict"}])


if __name__ == "__main__":
    unittest.main()
