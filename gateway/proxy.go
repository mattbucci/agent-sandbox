package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// chatRequest is a partial decode of the incoming chat-completions body; we
// only need the model field (== agent name) for routing and the stream flag
// for the span attribute. The full body bytes are forwarded downstream
// verbatim — stream stays a RawMessage so a non-boolean value (which the
// legacy endpoint forwarded untouched) never turns into a 400 here.
type chatRequest struct {
	Model  string          `json:"model"`
	Stream json.RawMessage `json:"stream"`
}

// streamFlag interprets the stream field for the span attribute only.
func (cr *chatRequest) streamFlag() bool {
	return string(bytes.TrimSpace(cr.Stream)) == "true"
}

// httpClient is used for all downstream calls. Timeout is 0 (no overall
// deadline) because SSE streams are long-lived; cancellation flows through the
// request context, which is tied to the client connection (sync) or the
// runner context (tasks).
var httpClient = &http.Client{Timeout: 0}

// buildVMRequest builds the downstream request to the in-VM server for agent,
// shared by the sync chat path and the task dispatcher so both produce
// identical wire bytes: target URL, per-agent model rewrite, api_server_key
// bearer, session headers, and (when non-empty) the traceparent header.
//
// The model rewrite: the client's `model` field is the agent id (routing
// key), not an LLM model. Backends like the deepagents adapter ignore it, but
// a black-box backend (the real hermes-agent) may reject or mis-forward it.
// When the agent declares a `model`, rewrite the field so the downstream
// receives a real model alias. Falls back to the original bytes if the body
// can't be round-tripped.
func buildVMRequest(ctx context.Context, cfg *Config, vm VMInfo, agent string, body []byte, sessionID, sessionKey, traceparent string) (*http.Request, error) {
	target := fmt.Sprintf("http://%s:%d/v1/chat/completions", vm.VMIP, cfg.VMGatewayPort)

	outBody := rewriteModelField(cfg, agent, body)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(outBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	// Session correlation headers pass through unchanged.
	if sessionID != "" {
		req.Header.Set("X-Hermes-Session-Id", sessionID)
	}
	if sessionKey != "" {
		req.Header.Set("X-Hermes-Session-Key", sessionKey)
	}
	if traceparent != "" {
		req.Header.Set("Traceparent", traceparent)
	}

	// Attach the downstream bearer only when a per-agent key is configured.
	if ac, ok := cfg.Agents[agent]; ok && ac.APIServerKey != "" {
		req.Header.Set("Authorization", "Bearer "+ac.APIServerKey)
	}
	return req, nil
}

// rewriteModelField applies the per-agent model rewrite to a JSON request body:
// the client's `model` field is the agent id (routing key), not an LLM model, so
// when the agent declares a `model` we swap the field to a real model alias
// before forwarding (black-box backends like the real hermes-agent read it).
// Falls back to the original bytes when the agent has no model override or the
// body can't be round-tripped as a JSON object (e.g. an empty/GET body) — so it
// never injects a spurious `model` into a body that had none.
func rewriteModelField(cfg *Config, agent string, body []byte) []byte {
	ac, ok := cfg.Agents[agent]
	if !ok || ac.Model == "" {
		return body
	}
	var m map[string]any
	if json.Unmarshal(body, &m) != nil || m == nil {
		return body
	}
	if _, hasModel := m["model"]; !hasModel {
		// The body carries no model field; don't add one (keeps approval/stop
		// bodies untouched). Only rewrite an existing routing-key model.
		return body
	}
	m["model"] = ac.Model
	if rb, mErr := json.Marshal(m); mErr == nil {
		return rb
	}
	return body
}

// emitSchedWaitSpan records the INTERNAL sched.wait span (§g), emitted only
// when the queue wait exceeded 5ms so uncontended requests stay two-span.
func (s *server) emitSchedWaitSpan(parent *Span, agent, class string, start time.Time, wait time.Duration, depthAtEnqueue int, outcome string) {
	if s.tracer == nil || parent == nil || wait <= 5*time.Millisecond {
		return
	}
	ws := s.tracer.StartChildAt(parent.Context(), "sched.wait", KindInternal, start)
	ws.SetAttr("hermes.agent", agent)
	ws.SetAttr("hermes.queue_class", class)
	ws.SetAttr("hermes.queue_depth_at_enqueue", depthAtEnqueue)
	ws.SetAttr("hermes.wait_outcome", outcome)
	ws.End()
}

// syncQueueDepth reports the current sync queue depth for agent (the
// sched.wait span's queue_depth_at_enqueue attribute; sampled just before
// Acquire, so approximate under races — fine for observability).
func (s *server) syncQueueDepth(agent string) int {
	depth := 0
	for _, snap := range s.sched.Snapshot() {
		if snap.Agent != agent {
			continue
		}
		for _, wtr := range snap.Waiting {
			if wtr.Kind == "sync" {
				depth++
			}
		}
	}
	return depth
}

// handleChatCompletions buffers the request body to extract the target agent,
// acquires a sync scheduler slot (queueing when the agent is busy), resolves a
// live VM, then reverse-proxies to that VM's in-VM server, streaming the
// response back to the client unbuffered. The slot is held for the whole
// stream — that is what concurrency=1 means.
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

	ri := reqInfoFrom(r.Context())
	var serverSpan *Span
	if ri != nil {
		ri.agent = agent
		serverSpan = ri.span
	}
	sessionID := r.Header.Get("X-Hermes-Session-Id")
	serverSpan.SetAttr("hermes.agent", agent)
	if sessionID != "" {
		serverSpan.SetAttr("hermes.session_id", sessionID)
	}
	serverSpan.SetAttr("hermes.stream", cr.streamFlag())
	serverSpan.SetAttr("hermes.mode", "sync")

	// Authorize the token's scope against the resolved agent.
	if !agentAllowed(scope, agent) {
		writeError(w, http.StatusForbidden,
			fmt.Sprintf("Token not authorized for agent %s", agent), "invalid_request_error")
		return
	}

	// Admission control BEFORE VM resolve: queue when the agent is at its
	// concurrency limit; 429 when the sync queue is full, 503 on wait timeout
	// (both additive, saturation-only responses).
	depthAtEnqueue := 0
	if serverSpan != nil {
		depthAtEnqueue = s.syncQueueDepth(agent)
	}
	waitStart := time.Now()
	release, err := s.sched.Acquire(r.Context(), agent, classSync, SlotMeta{
		SessionID: sessionID,
		TraceID:   serverSpan.Context().TraceID,
	})
	wait := time.Since(waitStart)
	switch {
	case err == nil:
		s.emitSchedWaitSpan(serverSpan, agent, "sync", waitStart, wait, depthAtEnqueue, "granted")
		defer release()
	case errors.Is(err, ErrQueueFull):
		s.emitSchedWaitSpan(serverSpan, agent, "sync", waitStart, wait, depthAtEnqueue, "queue_full")
		logWarn("sched_reject", append([]any{"agent", agent, "class", "sync", "reason", "queue_full"}, spanLogAttrs(serverSpan)...)...)
		w.Header().Set("Retry-After", strconv.Itoa(s.cfg.Scheduler.RetryAfterS))
		writeError(w, http.StatusTooManyRequests,
			fmt.Sprintf("Agent %s is busy and its queue is full", agent), "rate_limit_error")
		return
	case errors.Is(err, ErrWaitTimeout):
		s.emitSchedWaitSpan(serverSpan, agent, "sync", waitStart, wait, depthAtEnqueue, "wait_timeout")
		logWarn("sched_reject", append([]any{"agent", agent, "class", "sync", "reason", "wait_timeout"}, spanLogAttrs(serverSpan)...)...)
		w.Header().Set("Retry-After", strconv.Itoa(s.cfg.Scheduler.RetryAfterS))
		writeError(w, http.StatusServiceUnavailable,
			fmt.Sprintf("Timed out waiting for agent %s", agent), "server_error")
		return
	case errors.Is(err, ErrUnknownAgent):
		// Agent absent from config: preserve the legacy 502 envelope (there
		// can be no VM for an unconfigured agent).
		writeError(w, http.StatusBadGateway,
			fmt.Sprintf("No running VM for agent %s", agent), "invalid_request_error")
		return
	case errors.Is(err, ErrShuttingDown):
		writeError(w, http.StatusServiceUnavailable, "Gateway is shutting down", "server_error")
		return
	default:
		// Client went away while queued; nothing useful can be written.
		s.emitSchedWaitSpan(serverSpan, agent, "sync", waitStart, wait, depthAtEnqueue, "client_gone")
		logWarn("sched_reject", append([]any{"agent", agent, "class", "sync", "reason", "client_gone"}, spanLogAttrs(serverSpan)...)...)
		return
	}

	// Resolve a running VM for the agent (after the possible queue wait, so a
	// VM restarted meanwhile is picked up).
	vm, ok := findVMForAgent(s.cfg.StateDir, agent)
	if !ok {
		s.mx.IncUpstreamError(agent, "no_vm")
		logWarn("vm_resolve_fail", append([]any{"agent", agent}, spanLogAttrs(serverSpan)...)...)
		writeError(w, http.StatusBadGateway,
			fmt.Sprintf("No running VM for agent %s", agent), "invalid_request_error")
		return
	}

	// CLIENT proxy span (§g): its context is what traceparent carries into
	// the VM, so the in-VM SERVER span parents onto it.
	var clientSpan *Span
	traceparent := ""
	if s.tracer != nil && serverSpan != nil {
		clientSpan = s.tracer.StartChild(serverSpan.Context(), "proxy /v1/chat/completions", KindClient)
		clientSpan.SetAttr("server.address", vm.VMIP)
		clientSpan.SetAttr("server.port", s.cfg.VMGatewayPort)
		clientSpan.SetAttr("url.full", fmt.Sprintf("http://%s:%d/v1/chat/completions", vm.VMIP, s.cfg.VMGatewayPort))
		clientSpan.SetAttr("hermes.vm_instance", vm.InstanceID)
		clientSpan.SetAttr("hermes.model_rewritten", s.cfg.Agents[agent].Model != "")
		traceparent = formatTraceparent(clientSpan.Context())
	}

	// Build the downstream request, carrying the (possibly rewritten) body bytes
	// and context so client disconnects cancel the upstream call.
	req, err := buildVMRequest(r.Context(), s.cfg, vm, agent, body,
		sessionID, r.Header.Get("X-Hermes-Session-Key"), traceparent)
	if err != nil {
		clientSpan.SetError(err.Error())
		clientSpan.End()
		writeError(w, http.StatusInternalServerError, "Failed to build downstream request", "internal_error")
		return
	}

	doStart := time.Now()
	resp, err := httpClient.Do(req)
	if err != nil {
		s.mx.IncUpstreamError(agent, "connect")
		clientSpan.SetError(err.Error())
		clientSpan.End()
		writeError(w, http.StatusBadGateway,
			fmt.Sprintf("Failed to reach VM for agent %s: %v", agent, err), "internal_error")
		return
	}
	defer resp.Body.Close()

	clientSpan.SetAttr("http.response.status_code", resp.StatusCode)
	if resp.StatusCode >= 500 {
		s.mx.IncUpstreamError(agent, "status_5xx")
		clientSpan.SetError(fmt.Sprintf("VM returned status %d", resp.StatusCode))
	}

	// Mirror the downstream status and content type, then stream the body.
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)

	written, clientGone := streamBody(w, resp.Body, func() {
		s.mx.ObserveProxyTTFB(agent, time.Since(doStart))
		clientSpan.AddEvent("first_byte")
	})
	s.mx.AddStreamBytes(agent, written)
	clientSpan.SetAttr("hermes.bytes_streamed", written)
	if clientGone {
		clientSpan.AddEvent("client_disconnect")
	}
	clientSpan.End()
}

// streamBody copies src to w in small chunks, flushing after every write so
// SSE chunks reach the client immediately instead of being buffered.
// onFirstByte (nil-safe) fires once, before the first byte is forwarded;
// clientGone reports whether the copy stopped because the client went away.
func streamBody(w http.ResponseWriter, src io.Reader, onFirstByte func()) (written int64, clientGone bool) {
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 4096)
	first := true
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if first {
				first = false
				if onFirstByte != nil {
					onFirstByte()
				}
			}
			m, werr := w.Write(buf[:n])
			written += int64(m)
			if werr != nil {
				return written, true // client went away
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			return written, false // EOF or upstream/context cancellation
		}
	}
}
