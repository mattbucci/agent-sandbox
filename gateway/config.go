package main

import (
	"encoding/json"
	"fmt"
	"os"
)

// Config is the router configuration loaded from gateway.json.
// The schema is authoritative and shared with lib/agentconf.py (compile-gateway).
type Config struct {
	Bind          string                 `json:"bind"`
	Port          int                    `json:"port"`
	DefaultAgent  string                 `json:"default_agent"`
	StateDir      string                 `json:"state_dir"`
	VMGatewayPort int                    `json:"vm_gateway_port"`
	Tokens        []Token                `json:"tokens"`
	Agents        map[string]AgentConfig `json:"agents"`
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
type AgentConfig struct {
	APIServerKey string `json:"api_server_key"`
	Model        string `json:"model,omitempty"`
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
	if cfg.Port == 0 {
		cfg.Port = 8642
	}
	if cfg.VMGatewayPort == 0 {
		cfg.VMGatewayPort = 8642
	}
	if cfg.Bind == "" {
		cfg.Bind = "0.0.0.0"
	}
	return &cfg, nil
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
