"""
Tests for the memory part of the config compiler (lib/agentconf.py): backend
selection and the declared mental-model plan.

Run:  python3 test/test_memory_config.py     (needs PyYAML)
"""
import os
import sys
import unittest

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "lib"))
import agentconf  # noqa: E402

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
        self.assertEqual(conf["HINDSIGHT_TOKEN"], '"hs_x"')
        self.assertEqual(conf["HINDSIGHT_BANK"], '"agent-sandbox"')

    def test_mnemosyne_backend_is_unchanged(self):
        conf = self.conf({"memory": {"enabled": True, "token": "m"}}, AGENTS["coder"])
        self.assertEqual(conf["MNEMOSYNE_ENABLED"], '"1"')
        self.assertEqual(conf["HINDSIGHT_ENABLED"], '"0"')
        self.assertEqual(conf["HINDSIGHT_TOKEN"], '""')   # not handed out when unused

    def test_agent_can_opt_out_and_override_scope(self):
        agent = {"agent": {"type": "x", "name": "X"}, "memory": {"enabled": False}}
        self.assertEqual(self.conf(GLOBAL, agent)["HINDSIGHT_ENABLED"], '"0"')
        agent = {"agent": {"type": "x", "name": "X"}, "memory": {"recall_scope": "agent", "bank": "private"}}
        conf = self.conf(GLOBAL, agent)
        self.assertEqual((conf["MEMORY_RECALL_SCOPE"], conf["HINDSIGHT_BANK"]), ('"agent"', '"private"'))

    def test_model_limits_only_when_configured(self):
        self.assertNotIn("OMP_MAX_TOKENS", self.conf(GLOBAL, AGENTS["coder"]))
        g = {**GLOBAL, "llm": {"model": "q", "context_window": 32768, "max_output_tokens": 8192}}
        agent = {"agent": {"type": "x", "name": "X"}, "vm": {"max_output_tokens": 4096}}
        conf = self.conf(g, agent)
        self.assertEqual((conf["OMP_CONTEXT_WINDOW"], conf["OMP_MAX_TOKENS"]), ('"32768"', '"4096"'))


if __name__ == "__main__":
    unittest.main()
