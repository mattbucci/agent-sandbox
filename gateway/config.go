package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Config is the router configuration loaded from gateway.json.
// The schema is authoritative and shared with lib/agentconf.py (compile-gateway).
// All new blocks (scheduler, tasks, observability, dashboard) are optional:
// a legacy 7-key gateway.json loads unchanged and applyDefaults fills every
// zero value. Defaults live ONLY here in Go — the Python compiler copies the
// blocks through verbatim and never defaults anything.
type Config struct {
	Bind          string                 `json:"bind"`
	Port          int                    `json:"port"`
	DefaultAgent  string                 `json:"default_agent"`
	StateDir      string                 `json:"state_dir"`
	VMGatewayPort int                    `json:"vm_gateway_port"`
	Tokens        []Token                `json:"tokens"`
	Agents        map[string]AgentConfig `json:"agents"`
	Scheduler     SchedulerConfig        `json:"scheduler"`
	Tasks         TasksConfig            `json:"tasks"`
	Observability ObservabilityConfig    `json:"observability"`
	Dashboard     DashboardConfig        `json:"dashboard"`
}

// SchedulerConfig tunes the shared admission control (scheduler.go).
type SchedulerConfig struct {
	// DefaultConcurrency is the per-agent number of simultaneous runs
	// (sync + async combined) unless the agent overrides it.
	DefaultConcurrency int `json:"default_concurrency"`
	// SyncQueueMax is the number of waiting sync requests per agent before 429.
	SyncQueueMax int `json:"sync_queue_max"`
	// SyncQueueWaitS is the max sync queue wait in seconds before 503.
	SyncQueueWaitS int `json:"sync_queue_wait_s"`
	// AsyncStarvationAfterS is the aging threshold: an async slot request
	// jumps the sync FIFO after waiting this many seconds.
	AsyncStarvationAfterS int `json:"async_starvation_after_s"`
	// RetryAfterS is the Retry-After header value sent on 429/503.
	RetryAfterS int `json:"retry_after_s"`
}

// TasksConfig tunes the async task subsystem (tasks.go, dispatcher).
// Enabled is a *bool so an explicit false survives applyDefaults (default true).
type TasksConfig struct {
	Enabled             *bool  `json:"enabled"`
	Dir                 string `json:"dir"` // "" -> <state_dir>/gateway/tasks
	DefaultTimeoutS     int    `json:"default_timeout_s"`
	DefaultDeadlineS    int    `json:"default_deadline_s"`
	DefaultMaxAttempts  int    `json:"default_max_attempts"`
	RetryOnPartial      bool   `json:"retry_on_partial"`
	RetryBackoffBaseS   int    `json:"retry_backoff_base_s"`
	RetryBackoffCapS    int    `json:"retry_backoff_cap_s"`
	IdleTimeoutS        int    `json:"idle_timeout_s"`
	VMUnavailableRetryS int    `json:"vm_unavailable_retry_s"`
	RetentionH          int    `json:"retention_h"`
	MaxRecords          int    `json:"max_records"`
	MaxPendingPerAgent  int    `json:"max_pending_per_agent"`
}

// ObservabilityConfig tunes tracing/logging and the dashboard's read-only
// data sources. OTLPEndpoint and SampleRatio are pointers so an explicit
// "" (disable export) / 0 (sample nothing) survives applyDefaults.
type ObservabilityConfig struct {
	OTLPEndpoint   *string  `json:"otlp_endpoint"`
	SampleRatio    *float64 `json:"sample_ratio"`
	LogFormat      string   `json:"log_format"` // json|text
	TracesFile     string   `json:"traces_file"`
	SquidAccessLog string   `json:"squid_access_log"`
}

// DashboardConfig gates the embedded ops dashboard. Enabled is a *bool so an
// explicit false survives applyDefaults (default true). An empty Tokens list
// means the dashboard APIs fail closed (403).
type DashboardConfig struct {
	Enabled *bool    `json:"enabled"`
	Tokens  []string `json:"tokens"`
}

// Token is a single bearer credential and the agents it is scoped to.
// agents may be ["*"] to authorize every exposed agent.
type Token struct {
	Name   string   `json:"name"`
	Token  string   `json:"token"`
	Agents []string `json:"agents"`
}

// AgentConfig holds per-agent settings. api_server_key is the downstream
// bearer the in-VM server requires (empty string => send no Authorization).
// Model, when non-empty, rewrites the outgoing OpenAI `model` field (which from
// the client is the agent id) to a real upstream model alias before forwarding —
// used for black-box backends (e.g. the real hermes-agent) that would otherwise
// reject or mis-forward the agent id as a model name.
// Concurrency, when > 0, overrides scheduler.default_concurrency for this agent.
type AgentConfig struct {
	APIServerKey string `json:"api_server_key"`
	Model        string `json:"model,omitempty"`
	Concurrency  int    `json:"concurrency,omitempty"`
}

// LoadConfig reads and parses the gateway.json file at path.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	cfg.applyDefaults()
	return &cfg, nil
}

// applyDefaults fills every zero value with its documented default (plan §d).
// It is the ONLY place defaults live; a hand-edited or stale gateway.json
// behaves identically to a freshly compiled one. After it returns, all
// pointer-typed config fields are non-nil.
func (c *Config) applyDefaults() {
	if c.Port == 0 {
		c.Port = 8642
	}
	if c.VMGatewayPort == 0 {
		c.VMGatewayPort = 8642
	}
	if c.Bind == "" {
		c.Bind = "0.0.0.0"
	}

	// scheduler
	if c.Scheduler.DefaultConcurrency == 0 {
		c.Scheduler.DefaultConcurrency = 1
	}
	if c.Scheduler.SyncQueueMax == 0 {
		c.Scheduler.SyncQueueMax = 4
	}
	if c.Scheduler.SyncQueueWaitS == 0 {
		c.Scheduler.SyncQueueWaitS = 120
	}
	if c.Scheduler.AsyncStarvationAfterS == 0 {
		c.Scheduler.AsyncStarvationAfterS = 300
	}
	if c.Scheduler.RetryAfterS == 0 {
		c.Scheduler.RetryAfterS = 15
	}

	// tasks
	if c.Tasks.Enabled == nil {
		c.Tasks.Enabled = boolPtr(true)
	}
	if c.Tasks.Dir == "" {
		c.Tasks.Dir = filepath.Join(c.StateDir, "gateway", "tasks")
	}
	if c.Tasks.DefaultTimeoutS == 0 {
		c.Tasks.DefaultTimeoutS = 3600
	}
	if c.Tasks.DefaultDeadlineS == 0 {
		c.Tasks.DefaultDeadlineS = 86400
	}
	if c.Tasks.DefaultMaxAttempts == 0 {
		c.Tasks.DefaultMaxAttempts = 2
	}
	if c.Tasks.RetryBackoffBaseS == 0 {
		c.Tasks.RetryBackoffBaseS = 10
	}
	if c.Tasks.RetryBackoffCapS == 0 {
		c.Tasks.RetryBackoffCapS = 600
	}
	if c.Tasks.IdleTimeoutS == 0 {
		c.Tasks.IdleTimeoutS = 900
	}
	if c.Tasks.VMUnavailableRetryS == 0 {
		c.Tasks.VMUnavailableRetryS = 30
	}
	if c.Tasks.RetentionH == 0 {
		c.Tasks.RetentionH = 168
	}
	if c.Tasks.MaxRecords == 0 {
		c.Tasks.MaxRecords = 2000
	}
	if c.Tasks.MaxPendingPerAgent == 0 {
		c.Tasks.MaxPendingPerAgent = 200
	}

	// observability
	if c.Observability.OTLPEndpoint == nil {
		v := "http://127.0.0.1:4318"
		c.Observability.OTLPEndpoint = &v
	}
	if c.Observability.SampleRatio == nil {
		v := 1.0
		c.Observability.SampleRatio = &v
	}
	if c.Observability.LogFormat == "" {
		c.Observability.LogFormat = "json"
	}
	if c.Observability.TracesFile == "" {
		c.Observability.TracesFile = "/var/log/otel/traces.jsonl"
	}
	if c.Observability.SquidAccessLog == "" {
		c.Observability.SquidAccessLog = "/var/log/squid/access.log"
	}

	// dashboard
	if c.Dashboard.Enabled == nil {
		c.Dashboard.Enabled = boolPtr(true)
	}
}

func boolPtr(v bool) *bool { return &v }

// TasksEnabled reports whether the async task subsystem is on (default true).
func (c *Config) TasksEnabled() bool {
	return c.Tasks.Enabled == nil || *c.Tasks.Enabled
}

// DashboardEnabled reports whether the ops dashboard is on (default true).
func (c *Config) DashboardEnabled() bool {
	return c.Dashboard.Enabled == nil || *c.Dashboard.Enabled
}

// OTLPEndpoint returns the span export endpoint; "" means export is disabled
// (traceparent is still generated and propagated).
func (c *Config) OTLPEndpoint() string {
	if c.Observability.OTLPEndpoint == nil {
		return "http://127.0.0.1:4318"
	}
	return *c.Observability.OTLPEndpoint
}

// SampleRatio returns the trace sampling ratio (nil/absent means 1.0; an
// explicit 0 is honored).
func (c *Config) SampleRatio() float64 {
	if c.Observability.SampleRatio == nil {
		return 1.0
	}
	return *c.Observability.SampleRatio
}

// AgentConcurrency returns the effective concurrency limit for agent:
// the per-agent override when > 0, else scheduler.default_concurrency.
func (c *Config) AgentConcurrency(agent string) int {
	if ac, ok := c.Agents[agent]; ok && ac.Concurrency > 0 {
		return ac.Concurrency
	}
	if c.Scheduler.DefaultConcurrency > 0 {
		return c.Scheduler.DefaultConcurrency
	}
	return 1
}

// agentAllowed reports whether the given agents scope authorizes target.
// A scope containing "*" authorizes everything.
func agentAllowed(scope []string, target string) bool {
	for _, a := range scope {
		if a == "*" || a == target {
			return true
		}
	}
	return false
}

// scopedAgents returns the list of agent names visible to the given scope.
// If the scope contains "*", every configured agent is returned.
func (c *Config) scopedAgents(scope []string) []string {
	for _, a := range scope {
		if a == "*" {
			names := make([]string, 0, len(c.Agents))
			for name := range c.Agents {
				names = append(names, name)
			}
			return names
		}
	}
	// Only the explicitly listed agents that are actually configured.
	names := make([]string, 0, len(scope))
	for _, a := range scope {
		if _, ok := c.Agents[a]; ok {
			names = append(names, a)
		}
	}
	return names
}
