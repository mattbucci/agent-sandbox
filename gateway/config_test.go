package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLegacyGatewayJSONLoadsWithDefaults asserts that a fixture mirroring the
// key shape of today's compiled gateway.json (7 top-level keys, no new
// blocks; every token value a placeholder) loads unchanged and that every
// new config knob gets its documented Go-side default (plan §d).
func TestLegacyGatewayJSONLoadsWithDefaults(t *testing.T) {
	cfg, err := LoadConfig(filepath.Join("testdata", "gateway_legacy.json"))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	// Legacy fields parse exactly as before.
	if cfg.Bind != "0.0.0.0" || cfg.Port != 8642 || cfg.VMGatewayPort != 8642 {
		t.Fatalf("legacy listen fields wrong: %+v", cfg)
	}
	if cfg.DefaultAgent != "feature-dev" {
		t.Fatalf("default_agent = %q", cfg.DefaultAgent)
	}
	if cfg.StateDir != "/home/placeholder/AI/agent-sandbox/state" {
		t.Fatalf("state_dir = %q", cfg.StateDir)
	}
	if len(cfg.Tokens) != 1 || cfg.Tokens[0].Name != "hermes-webui" || !agentAllowed(cfg.Tokens[0].Agents, "anything") {
		t.Fatalf("tokens wrong: %+v", cfg.Tokens)
	}
	if len(cfg.Agents) != 6 {
		t.Fatalf("agents = %d, want 6", len(cfg.Agents))
	}
	if ac := cfg.Agents["hermes"]; ac.Model != "gemma" || ac.APIServerKey == "" {
		t.Fatalf("hermes agent wrong: %+v", ac)
	}

	// Scheduler defaults.
	s := cfg.Scheduler
	if s.DefaultConcurrency != 1 || s.SyncQueueMax != 4 || s.SyncQueueWaitS != 120 ||
		s.AsyncStarvationAfterS != 300 || s.RetryAfterS != 15 {
		t.Fatalf("scheduler defaults wrong: %+v", s)
	}
	if got := cfg.AgentConcurrency("feature-dev"); got != 1 {
		t.Fatalf("AgentConcurrency = %d, want 1", got)
	}

	// Tasks defaults.
	tk := cfg.Tasks
	if !cfg.TasksEnabled() {
		t.Fatal("tasks should default enabled")
	}
	wantDir := filepath.Join(cfg.StateDir, "gateway", "tasks")
	if tk.Dir != wantDir {
		t.Fatalf("tasks.dir = %q, want %q", tk.Dir, wantDir)
	}
	if tk.DefaultTimeoutS != 3600 || tk.DefaultDeadlineS != 86400 || tk.DefaultMaxAttempts != 2 ||
		tk.RetryOnPartial || tk.RetryBackoffBaseS != 10 || tk.RetryBackoffCapS != 600 ||
		tk.IdleTimeoutS != 900 || tk.VMUnavailableRetryS != 30 || tk.RetentionH != 168 ||
		tk.MaxRecords != 2000 || tk.MaxPendingPerAgent != 200 {
		t.Fatalf("tasks defaults wrong: %+v", tk)
	}

	// Observability defaults.
	if got := cfg.OTLPEndpoint(); got != "http://127.0.0.1:4318" {
		t.Fatalf("otlp_endpoint = %q", got)
	}
	if got := cfg.SampleRatio(); got != 1.0 {
		t.Fatalf("sample_ratio = %v", got)
	}
	o := cfg.Observability
	if o.LogFormat != "json" || o.TracesFile != "/var/log/otel/traces.jsonl" ||
		o.SquidAccessLog != "/var/log/squid/access.log" {
		t.Fatalf("observability defaults wrong: %+v", o)
	}

	// Dashboard defaults.
	if !cfg.DashboardEnabled() {
		t.Fatal("dashboard should default enabled")
	}
	if len(cfg.Dashboard.Tokens) != 0 {
		t.Fatalf("dashboard.tokens should default empty: %+v", cfg.Dashboard.Tokens)
	}
}

// fullConfigMap returns a config document with every new block present and
// every value deliberately non-default.
func fullConfigMap() map[string]any {
	return map[string]any{
		"bind":            "127.0.0.1",
		"port":            9999,
		"default_agent":   "a",
		"state_dir":       "/tmp/hermes-test-state",
		"vm_gateway_port": 9999,
		"tokens": []any{
			map[string]any{"name": "test", "token": "hgw-PLACEHOLDER-TEST-TOKEN-1", "agents": []any{"*"}},
		},
		"agents": map[string]any{
			"a": map[string]any{"api_server_key": "", "concurrency": 3},
			"b": map[string]any{"api_server_key": ""},
		},
		"scheduler": map[string]any{
			"default_concurrency":      2,
			"sync_queue_max":           8,
			"sync_queue_wait_s":        60,
			"async_starvation_after_s": 100,
			"retry_after_s":            5,
		},
		"tasks": map[string]any{
			"enabled":                false,
			"dir":                    "/custom/tasks",
			"default_timeout_s":      100,
			"default_deadline_s":     200,
			"default_max_attempts":   5,
			"retry_on_partial":       true,
			"retry_backoff_base_s":   1,
			"retry_backoff_cap_s":    2,
			"idle_timeout_s":         3,
			"vm_unavailable_retry_s": 4,
			"retention_h":            5,
			"max_records":            6,
			"max_pending_per_agent":  7,
		},
		"observability": map[string]any{
			"otlp_endpoint":    "",
			"sample_ratio":     0.0,
			"log_format":       "text",
			"traces_file":      "/custom/traces.jsonl",
			"squid_access_log": "/custom/access.log",
		},
		"dashboard": map[string]any{
			"enabled": false,
			"tokens":  []any{"hgwd-PLACEHOLDER-TEST-TOKEN-1"},
		},
	}
}

func writeConfig(t *testing.T, doc map[string]any) string {
	t.Helper()
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	path := filepath.Join(t.TempDir(), "gateway.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

// TestEachBlockIndependentlyAbsent removes each new block in turn and asserts
// that only the removed block reverts to defaults.
func TestEachBlockIndependentlyAbsent(t *testing.T) {
	for _, missing := range []string{"scheduler", "tasks", "observability", "dashboard"} {
		missing := missing
		t.Run(missing, func(t *testing.T) {
			doc := fullConfigMap()
			delete(doc, missing)
			cfg, err := LoadConfig(writeConfig(t, doc))
			if err != nil {
				t.Fatalf("LoadConfig: %v", err)
			}

			// The removed block gets defaults; the others keep explicit values.
			if missing == "scheduler" {
				if cfg.Scheduler.DefaultConcurrency != 1 || cfg.Scheduler.SyncQueueMax != 4 {
					t.Fatalf("scheduler not defaulted: %+v", cfg.Scheduler)
				}
			} else if cfg.Scheduler.SyncQueueMax != 8 {
				t.Fatalf("scheduler override lost: %+v", cfg.Scheduler)
			}
			if missing == "tasks" {
				if !cfg.TasksEnabled() || cfg.Tasks.DefaultTimeoutS != 3600 {
					t.Fatalf("tasks not defaulted: %+v", cfg.Tasks)
				}
				want := filepath.Join("/tmp/hermes-test-state", "gateway", "tasks")
				if cfg.Tasks.Dir != want {
					t.Fatalf("tasks.dir = %q, want %q", cfg.Tasks.Dir, want)
				}
			} else if cfg.TasksEnabled() || cfg.Tasks.Dir != "/custom/tasks" {
				t.Fatalf("tasks override lost: %+v", cfg.Tasks)
			}
			if missing == "observability" {
				if cfg.OTLPEndpoint() != "http://127.0.0.1:4318" || cfg.SampleRatio() != 1.0 {
					t.Fatalf("observability not defaulted: %+v", cfg.Observability)
				}
			} else if cfg.OTLPEndpoint() != "" || cfg.SampleRatio() != 0 {
				t.Fatalf("observability override lost: %+v", cfg.Observability)
			}
			if missing == "dashboard" {
				if !cfg.DashboardEnabled() || len(cfg.Dashboard.Tokens) != 0 {
					t.Fatalf("dashboard not defaulted: %+v", cfg.Dashboard)
				}
			} else if cfg.DashboardEnabled() || len(cfg.Dashboard.Tokens) != 1 {
				t.Fatalf("dashboard override lost: %+v", cfg.Dashboard)
			}
		})
	}
}

// TestExplicitZeroValuesPreserved asserts the pointer-typed knobs distinguish
// "absent" from explicit disabling values.
func TestExplicitZeroValuesPreserved(t *testing.T) {
	cfg, err := LoadConfig(writeConfig(t, fullConfigMap()))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.OTLPEndpoint() != "" {
		t.Fatalf("explicit empty otlp_endpoint overwritten: %q", cfg.OTLPEndpoint())
	}
	if cfg.SampleRatio() != 0 {
		t.Fatalf("explicit sample_ratio 0 overwritten: %v", cfg.SampleRatio())
	}
	if cfg.TasksEnabled() {
		t.Fatal("explicit tasks.enabled=false overwritten")
	}
	if cfg.DashboardEnabled() {
		t.Fatal("explicit dashboard.enabled=false overwritten")
	}
	if !cfg.Tasks.RetryOnPartial {
		t.Fatal("explicit retry_on_partial=true lost")
	}
	if got := cfg.AgentConcurrency("a"); got != 3 {
		t.Fatalf("per-agent concurrency = %d, want 3", got)
	}
	if got := cfg.AgentConcurrency("b"); got != 2 {
		t.Fatalf("default_concurrency fallback = %d, want 2", got)
	}
	if got := cfg.AgentConcurrency("nope"); got != 2 {
		t.Fatalf("unknown-agent concurrency = %d, want 2", got)
	}
}

// TestGoModHasNoRequireLines is the executable form of the stdlib-only
// invariant: the gateway must never grow a dependency.
func TestGoModHasNoRequireLines(t *testing.T) {
	data, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "require") {
			t.Fatalf("go.mod contains a require line: %q", line)
		}
	}
}
