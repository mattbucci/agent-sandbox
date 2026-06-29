package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
)

// chatRequest is a partial decode of the incoming chat-completions body; we only
// need the model field (== agent name) for routing. The full body bytes are
// forwarded downstream verbatim.
type chatRequest struct {
	Model string `json:"model"`
}

// httpClient is used for all downstream calls. Timeout is 0 (no overall
// deadline) because SSE streams are long-lived; cancellation flows through the
// request context, which is tied to the client connection.
var httpClient = &http.Client{Timeout: 0}

// handleChatCompletions buffers the request body to extract the target agent,
// resolves a live VM, then reverse-proxies to that VM's in-VM server, streaming
// the response back to the client unbuffered.
func (s *server) handleChatCompletions(w http.ResponseWriter, r *http.Request, scope []string) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "Failed to read request body", "invalid_request_error")
		return
	}

	var cr chatRequest
	if err := json.Unmarshal(body, &cr); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid JSON body", "invalid_request_error")
		return
	}

	// Resolve the agent: empty or "default" falls back to the configured default.
	agent := cr.Model
	if agent == "" || agent == "default" {
		agent = s.cfg.DefaultAgent
	}

	// Authorize the token's scope against the resolved agent.
	if !agentAllowed(scope, agent) {
		log.Printf("chat method=POST path=%s agent=%s status=403 (scope denied)", r.URL.Path, agent)
		writeError(w, http.StatusForbidden,
			fmt.Sprintf("Token not authorized for agent %s", agent), "invalid_request_error")
		return
	}

	// Resolve a running VM for the agent.
	vm, ok := findVMForAgent(s.cfg.StateDir, agent)
	if !ok {
		log.Printf("chat method=POST path=%s agent=%s status=502 (no running VM)", r.URL.Path, agent)
		writeError(w, http.StatusBadGateway,
			fmt.Sprintf("No running VM for agent %s", agent), "invalid_request_error")
		return
	}

	target := fmt.Sprintf("http://%s:%d/v1/chat/completions", vm.VMIP, s.cfg.VMGatewayPort)

	// Optional per-agent model rewrite: the client's `model` field is the agent
	// id (routing key), not an LLM model. Backends like the deepagents adapter
	// ignore it, but a black-box backend (the real hermes-agent) may reject or
	// mis-forward it. When the agent declares a `model`, rewrite the field so the
	// downstream receives a real model alias. Falls back to the original bytes if
	// the body can't be round-tripped.
	outBody := body
	if ac, ok := s.cfg.Agents[agent]; ok && ac.Model != "" {
		var m map[string]any
		if json.Unmarshal(body, &m) == nil {
			m["model"] = ac.Model
			if rb, mErr := json.Marshal(m); mErr == nil {
				outBody = rb
			}
		}
	}

	// Build the downstream request, carrying the (possibly rewritten) body bytes
	// and context so client disconnects cancel the upstream call.
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, target, bytes.NewReader(outBody))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to build downstream request", "internal_error")
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	// Pass session correlation headers through unchanged.
	if v := r.Header.Get("X-Hermes-Session-Id"); v != "" {
		req.Header.Set("X-Hermes-Session-Id", v)
	}
	if v := r.Header.Get("X-Hermes-Session-Key"); v != "" {
		req.Header.Set("X-Hermes-Session-Key", v)
	}

	// Attach the downstream bearer only when a per-agent key is configured.
	if ac, ok := s.cfg.Agents[agent]; ok && ac.APIServerKey != "" {
		req.Header.Set("Authorization", "Bearer "+ac.APIServerKey)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		log.Printf("chat method=POST path=%s agent=%s vm=%s status=502 (downstream error: %v)",
			r.URL.Path, agent, vm.VMIP, err)
		writeError(w, http.StatusBadGateway,
			fmt.Sprintf("Failed to reach VM for agent %s: %v", agent, err), "internal_error")
		return
	}
	defer resp.Body.Close()

	// Mirror the downstream status and content type, then stream the body.
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)

	log.Printf("chat method=POST path=%s agent=%s vm=%s status=%d (streaming)",
		r.URL.Path, agent, vm.VMIP, resp.StatusCode)

	streamBody(w, resp.Body)
}

// streamBody copies src to w in small chunks, flushing after every write so SSE
// chunks reach the client immediately instead of being buffered.
func streamBody(w http.ResponseWriter, src io.Reader) {
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 4096)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return // client went away
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			return // EOF or upstream/context cancellation
		}
	}
}
