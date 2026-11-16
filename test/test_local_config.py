"""
Tests for the private configuration overlay (config/local/, see
docs/local-config.md): local agents, overrides of published agents, and local
presets / install scripts.

Run:  python3 test/test_local_config.py     (needs PyYAML)
"""
import os
import sys
import tempfile
import unittest
from pathlib import Path

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "lib"))
import agentconf  # noqa: E402


class LocalConfigTest(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.local = Path(tmp.name)
        saved = agentconf.LOCAL_DIR
        agentconf.LOCAL_DIR = self.local
        self.addCleanup(setattr, agentconf, "LOCAL_DIR", saved)

    def write(self, rel, text):
        path = self.local / rel
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(text)
        return path

    def test_without_an_overlay_nothing_changes(self):
        self.assertIn("coder", agentconf.agent_types())
        self.assertEqual(agentconf.agent_sources("coder"), [agentconf.AGENTS_DIR / "coder.yaml"])
        self.assertEqual(agentconf.load_agent("coder"),
                         agentconf.load_yaml(agentconf.AGENTS_DIR / "coder.yaml"))

    def test_local_agent_is_a_full_agent(self):
        self.write("agents/scout.yaml", """
agent: {type: scout, name: Scout}
egress: {presets: [github]}
capabilities: {presets: [python-dev]}
prompt: {role: "You scout.", presets: [code-execution]}
""")
        self.assertIn("scout", agentconf.agent_types())
        self.assertEqual(agentconf.load_agent("scout")["agent"]["name"], "Scout")
        self.assertIn("scout", agentconf._all_agents())
        self.assertTrue(agentconf.cmd_validate("scout"))
        self.assertIn("LLM_", agentconf.compile_agent_conf(agentconf.load_agent("scout"), {"llm": {"model": "m"}}))

    def test_local_file_overrides_a_published_agent(self):
        published = agentconf.load_yaml(agentconf.AGENTS_DIR / "coder.yaml")
        self.write("agents/coder.yaml", """
vm: {model: my-private-model}
egress: {presets: [github]}
""")
        agent = agentconf.load_agent("coder")
        # Mappings merge: the changed key changes, its siblings stay.
        self.assertEqual(agent["vm"]["model"], "my-private-model")
        self.assertEqual(agent["vm"]["vcpus"], published["vm"]["vcpus"])
        self.assertEqual(agent["agent"], published["agent"])
        # Lists are replaced whole, so an entry can be dropped.
        self.assertEqual(agent["egress"]["presets"], ["github"])
        self.assertEqual(agentconf.agent_types().count("coder"), 1)

    def test_site_config_is_laid_over_the_shared_one(self):
        with tempfile.TemporaryDirectory() as conf:
            saved = agentconf.CONFIG_DIR
            agentconf.CONFIG_DIR = Path(conf)
            self.addCleanup(setattr, agentconf, "CONFIG_DIR", saved)
            (Path(conf) / "sandbox.yaml").write_text(
                "llm: {api_key: sk-secret, model: host-model}\nmemory: {token: t}\n")
            self.assertEqual(agentconf.load_global_config()["llm"]["model"], "host-model")
            self.write("sandbox.yaml", "llm: {model: shared-model, context_window: 1000}\n"
                                       "memory: {backend: hindsight, mental_models: [{id: m, name: M, query: q}]}\n")
            conf_doc = agentconf.load_global_config()
        # The host file wins where both speak; the shared one fills the rest.
        self.assertEqual(conf_doc["llm"], {"api_key": "sk-secret", "model": "host-model", "context_window": 1000})
        self.assertEqual(conf_doc["memory"]["token"], "t")
        self.assertEqual(conf_doc["memory"]["mental_models"][0]["id"], "m")

    def test_merge_config(self):
        self.assertEqual(agentconf.merge_config({"a": {"b": 1, "c": [1, 2]}, "d": 1},
                                                {"a": {"c": [3]}, "e": 2}),
                         {"a": {"b": 1, "c": [3]}, "d": 1, "e": 2})
        self.assertEqual(agentconf.merge_config({"a": {"b": 1}}, {"a": None}), {"a": None})

    def test_local_presets_and_install_scripts(self):
        self.write("presets/egress/intranet.yaml", "name: intranet\ndomains: [wiki.corp.example]\n")
        shadow = self.write("presets/egress/github.yaml", "name: github\ndomains: [github.example]\n")
        script = self.write("install-scripts/private-tool.sh", "#!/bin/bash\necho private-tool\n")
        self.assertEqual(agentconf.load_preset("egress", "intranet")["domains"], ["wiki.corp.example"])
        self.assertEqual(agentconf.preset_path("egress", "github"), shadow)       # local wins
        self.assertEqual(agentconf.preset_path("egress", "npm"), agentconf.PRESETS_DIR / "egress" / "npm.yaml")
        names = [p.stem for p in agentconf.preset_files("egress")]
        self.assertEqual(names.count("github"), 1)
        self.assertIn("intranet", names)
        self.assertEqual(agentconf.install_script_path("private-tool.sh"), script)

        self.write("agents/insider.yaml", """
agent: {type: insider, name: Insider}
egress: {presets: [intranet]}
capabilities: {install_scripts: [private-tool.sh]}
prompt: {role: "Inside."}
""")
        self.assertTrue(agentconf.cmd_validate("insider"))
        self.assertIn("echo private-tool", agentconf.compile_customize_sh(agentconf.load_agent("insider")))


if __name__ == "__main__":
    unittest.main()
