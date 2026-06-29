// Command hermes-gateway is the bare-metal ROUTER half of the Hermes Gateway.
//
// It exposes an OpenAI-compatible API on the host LAN (default 0.0.0.0:8642),
// authenticates incoming bearer tokens, maps the OpenAI "model" field to an
// agent type, resolves a live Firecracker VM running that agent, and
// reverse-proxies (streaming SSE) to the in-VM server at <vm_ip>:<vm_gateway_port>.
//
// It uses only the Go standard library so it can be built on an offline host.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
)

// defaultConfigPath is used when neither -config nor GATEWAY_CONFIG is set.
const defaultConfigPath = "/home/letsrtfm/AI/agent-sandbox/state/gateway/gateway.json"

// server bundles the loaded config for the HTTP handlers.
type server struct {
	cfg *Config
}

func main() {
	configFlag := flag.String("config", "", "path to gateway.json (overrides GATEWAY_CONFIG env)")
	flag.Parse()

	configPath := *configFlag
	if configPath == "" {
		configPath = os.Getenv("GATEWAY_CONFIG")
	}
	if configPath == "" {
		configPath = defaultConfigPath
	}

	cfg, err := LoadConfig(configPath)
	if err != nil {
		log.Fatalf("hermes-gateway: %v", err)
	}

	s := &server{cfg: cfg}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/v1/capabilities", s.handleCapabilities)
	mux.HandleFunc("/v1/models", s.handleModels)
	mux.HandleFunc("/v1/chat/completions", s.handleChat)

	addr := fmt.Sprintf("%s:%d", cfg.Bind, cfg.Port)
	log.Printf("hermes-gateway: config=%s listening on %s default_agent=%s state_dir=%s",
		configPath, addr, cfg.DefaultAgent, cfg.StateDir)

	srv := &http.Server{Addr: addr, Handler: mux}
	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("hermes-gateway: server error: %v", err)
	}
}

// handleHealth reports liveness without auth.
func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleCapabilities advertises gateway features. We run the legacy chat path,
// so both flags are false. No auth is required, but a token is accepted.
func (s *server) handleCapabilities(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"features": map[string]bool{
			"approval_events":       false,
			"run_approval_response": false,
		},
	})
}

// handleModels returns the agents visible to the caller's token scope.
func (s *server) handleModels(w http.ResponseWriter, r *http.Request) {
	scope, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	names := s.cfg.scopedAgents(scope)
	data := make([]map[string]string, 0, len(names))
	for _, name := range names {
		data = append(data, map[string]string{
			"id":       name,
			"object":   "model",
			"owned_by": "hermes-gateway",
		})
	}
	log.Printf("models method=GET path=%s status=200 count=%d", r.URL.Path, len(data))
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// handleChat authenticates then delegates to the streaming proxy.
func (s *server) handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "Method not allowed", "invalid_request_error")
		return
	}
	scope, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	s.handleChatCompletions(w, r, scope)
}

// authenticate validates the bearer token and returns the matched token's agents
// scope. On failure it writes the spec 401 body and returns ok=false.
func (s *server) authenticate(w http.ResponseWriter, r *http.Request) (scope []string, ok bool) {
	auth := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if strings.HasPrefix(auth, prefix) {
		presented := strings.TrimSpace(auth[len(prefix):])
		for _, t := range s.cfg.Tokens {
			if t.Token != "" && presented == t.Token {
				return t.Agents, true
			}
		}
	}
	log.Printf("auth method=%s path=%s status=401 (invalid api key)", r.Method, r.URL.Path)
	writeError(w, http.StatusUnauthorized, "Invalid API key", "invalid_request_error")
	return nil, false
}

// writeJSON marshals v and writes it with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError emits an OpenAI-style error envelope.
func writeError(w http.ResponseWriter, status int, message, errType string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    errType,
		},
	})
}
