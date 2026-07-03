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
	"strings"
	"sync"
	"time"
)

// The Hermes runs API (interactive dangerous-command approval). Unlike sync
// chat — one long-lived SSE connection — a *run* is a background task on the
// backend addressed by five endpoints across SEPARATE connections (the wire
// contract verified against nousresearch/hermes-agent:v2026.6.19):
//
//	POST /v1/runs                  -> 202 {"run_id":"run_...","status":"started"}
//	GET  /v1/runs/{id}             -> pollable status object
//	GET  /v1/runs/{id}/events      -> SSE lifecycle stream (incl. approval.request)
//	POST /v1/runs/{id}/approval    -> {"choice":"once|session|always|deny"}
//	POST /v1/runs/{id}/stop        -> interrupt
//
// The router is backend-agnostic: it proxies every path to the VM that owns the
// run (the real hermes container implements this natively; the deepagents
// harness implements the same contract in-VM). Two things the router owns:
//
//  1. A run REGISTRY binding run_id -> {agent, vmIP}, populated from the 202
//     body, so the follow-up endpoints (which carry only the run id, no model)
//     route back to the SAME VM instance that holds the run's state.
//
//  2. A slot SUPERVISOR: POST /v1/runs acquires the agent's scheduler slot (the
//     single-shell admission invariant), then hands it to a background goroutine
//     that holds it for the whole run — polling the backend's status endpoint —
//     and releases it only when the run reaches a terminal state (or a TTL).
//     The follow-up endpoints acquire NO slot; the supervisor already holds it.

// Runs-API tuning. Package vars (not consts) so tests can shrink them.
var (
	// runPollInterval is how often the supervisor polls the backend for the
	// run's status while holding the agent's slot.
	runPollInterval = 2 * time.Second
	// runSupervisorTTL bounds how long the supervisor will hold a slot for a
	// single run before giving up (mirrors the backend's own run-status TTL).
	runSupervisorTTL = time.Hour
	// runVMGraceErrors is how many consecutive status-poll failures BEFORE the
	// run was ever observed alive are tolerated before the supervisor releases
	// the slot (a VM that never comes up must not pin a slot forever).
	runVMGraceErrors = 15
	// maxRunRespBytes caps the (small) JSON bodies read from run create/status/
	// approval/stop responses.
	maxRunRespBytes int64 = 1 << 20
	// maxRunReqBytes caps a run-create request body.
	maxRunReqBytes int64 = 1 << 20
)

// runBinding records the VM instance that owns a run, so the follow-up
// endpoints (events/status/approval/stop) reach the same backend.
type runBinding struct {
	agent string
	vmIP  string
}

// runRegistry is the in-memory run_id -> binding map. Entries are added on
// create and removed when the supervisor exits (terminal/TTL). Router-restart
// durability is out of scope: after a restart an unknown run id 404s.
type runRegistry struct {
	mu sync.Mutex
	m  map[string]runBinding
}

func newRunRegistry() *runRegistry { return &runRegistry{m: make(map[string]runBinding)} }

func (r *runRegistry) put(id string, b runBinding) {
	r.mu.Lock()
	r.m[id] = b
	r.mu.Unlock()
}

func (r *runRegistry) get(id string) (runBinding, bool) {
	r.mu.Lock()
	b, ok := r.m[id]
	r.mu.Unlock()
	return b, ok
}

func (r *runRegistry) delete(id string) {
	r.mu.Lock()
	delete(r.m, id)
	r.mu.Unlock()
}

func (r *runRegistry) len() int {
	r.mu.Lock()
	n := len(r.m)
	r.mu.Unlock()
	return n
}

// registerRunsAPI wires the /v1/runs* routes and initializes the registry.
// main.go calls it unconditionally (the router proxies runs regardless of
// which backend serves the agent; capabilities gate whether the webui uses it).
func registerRunsAPI(mux *http.ServeMux, s *server) {
	if s.runs == nil {
		s.runs = newRunRegistry()
	}
	mux.HandleFunc("/v1/runs", s.handleRunsCollection)
	mux.HandleFunc("/v1/runs/", s.handleRunItem)
}

// runCreateResponse is the minimal decode of the backend's 202 body: we only
// need the run id to bind the run to its VM.
type runCreateResponse struct {
	RunID string `json:"run_id"`
}

// runStatusShape is the minimal decode of GET /v1/runs/{id} used by the
// supervisor to detect terminal runs.
type runStatusShape struct {
	Status string `json:"status"`
}

// isTerminalRunStatus reports whether a polled run status means the backend has
// finished with the run (so the router can release its slot).
func isTerminalRunStatus(status string) bool {
	switch status {
	case "completed", "failed", "cancelled", "canceled", "expired":
		return true
	}
	return false
}

// handleRunsCollection serves POST /v1/runs: authenticate, resolve+authorize
// the agent, acquire a sync slot, resolve the VM, proxy the create, capture the
// run id, and hand the slot to a supervisor goroutine.
func (s *server) handleRunsCollection(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "Method not allowed", "invalid_request_error")
		return
	}
	tok, ok := s.authenticate(w, r)
	if !ok {
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRunReqBytes))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeError(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("Request body exceeds %d bytes", maxRunReqBytes), "invalid_request_error")
			return
		}
		writeError(w, http.StatusBadRequest, "Failed to read request body", "invalid_request_error")
		return
	}
	// A run body uses `input` (not `messages`), but the routing key is the same
	// `model` field the chat body carries, so the tolerant chatRequest decode
	// gives us the agent without a bespoke type.
	var cr chatRequest
	if err := json.Unmarshal(body, &cr); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid JSON body", "invalid_request_error")
		return
	}
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
	serverSpan.SetAttr("hermes.mode", "run")
	if sessionID != "" {
		serverSpan.SetAttr("hermes.session_id", sessionID)
	}

	if !agentAllowed(tok.Agents, agent) {
		writeError(w, http.StatusForbidden,
			fmt.Sprintf("Token not authorized for agent %s", agent), "invalid_request_error")
		return
	}

	// Admission control (same saturation semantics as sync chat): the slot is
	// held for the WHOLE run, so a run and a chat can't clobber the agent's
	// single shell/workspace.
	release, err := s.sched.Acquire(r.Context(), agent, classSync, SlotMeta{
		SessionID: sessionID,
		TraceID:   serverSpan.Context().TraceID,
	})
	switch {
	case err == nil:
		// keep the slot; the supervisor releases it (or we release on early exit)
	case errors.Is(err, ErrQueueFull):
		w.Header().Set("Retry-After", strconv.Itoa(s.cfg.Scheduler.RetryAfterS))
		writeError(w, http.StatusTooManyRequests,
			fmt.Sprintf("Agent %s is busy and its queue is full", agent), "rate_limit_error")
		return
	case errors.Is(err, ErrWaitTimeout):
		w.Header().Set("Retry-After", strconv.Itoa(s.cfg.Scheduler.RetryAfterS))
		writeError(w, http.StatusServiceUnavailable,
			fmt.Sprintf("Timed out waiting for agent %s", agent), "server_error")
		return
	case errors.Is(err, ErrUnknownAgent):
		writeError(w, http.StatusBadGateway,
			fmt.Sprintf("No running VM for agent %s", agent), "invalid_request_error")
		return
	case errors.Is(err, ErrShuttingDown):
		writeError(w, http.StatusServiceUnavailable, "Gateway is shutting down", "server_error")
		return
	default:
		// Client went away while queued.
		return
	}

	// From here the slot is HELD. Every early-exit path below must release it.
	vm, ok := findVMForAgent(s.cfg.StateDir, agent)
	if !ok {
		release()
		s.mx.IncUpstreamError(agent, "no_vm")
		logWarn("vm_resolve_fail", append([]any{"agent", agent, "mode", "run"}, spanLogAttrs(serverSpan)...)...)
		writeError(w, http.StatusBadGateway,
			fmt.Sprintf("No running VM for agent %s", agent), "invalid_request_error")
		return
	}

	var clientSpan *Span
	traceparent := ""
	if s.tracer != nil && serverSpan != nil {
		clientSpan = s.tracer.StartChild(serverSpan.Context(), "proxy POST /v1/runs", KindClient)
		clientSpan.SetAttr("server.address", vm.VMIP)
		clientSpan.SetAttr("hermes.vm_instance", vm.InstanceID)
		traceparent = formatTraceparent(clientSpan.Context())
	}

	req, err := buildVMRunRequest(r.Context(), s.cfg, vm, agent, http.MethodPost, "/v1/runs",
		body, sessionID, r.Header.Get("X-Hermes-Session-Key"), traceparent, "application/json", true)
	if err != nil {
		release()
		clientSpan.SetError(err.Error())
		clientSpan.End()
		writeError(w, http.StatusInternalServerError, "Failed to build downstream request", "internal_error")
		return
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		release()
		s.mx.IncUpstreamError(agent, "connect")
		clientSpan.SetError(err.Error())
		clientSpan.End()
		writeError(w, http.StatusBadGateway,
			fmt.Sprintf("Failed to reach VM for agent %s: %v", agent, err), "internal_error")
		return
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxRunRespBytes))
	clientSpan.SetAttr("http.response.status_code", resp.StatusCode)

	// Non-2xx: the run was not created; release the slot and mirror the error.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		release()
		if resp.StatusCode >= 500 {
			s.mx.IncUpstreamError(agent, "status_5xx")
			clientSpan.SetError(fmt.Sprintf("VM returned status %d", resp.StatusCode))
		}
		clientSpan.End()
		mirrorResponse(w, resp.StatusCode, resp.Header.Get("Content-Type"), respBody)
		return
	}

	var created runCreateResponse
	_ = json.Unmarshal(respBody, &created)
	if created.RunID == "" {
		// The backend accepted the run but we can't address it for follow-ups;
		// don't leak the slot waiting on a run we can never observe.
		release()
		logWarn("run_no_id", append([]any{"agent", agent, "status", resp.StatusCode}, spanLogAttrs(serverSpan)...)...)
		clientSpan.End()
		mirrorResponse(w, resp.StatusCode, resp.Header.Get("Content-Type"), respBody)
		return
	}

	s.runs.put(created.RunID, runBinding{agent: agent, vmIP: vm.VMIP})
	serverSpan.SetAttr("hermes.run_id", created.RunID)
	clientSpan.SetAttr("hermes.run_id", created.RunID)
	clientSpan.End()
	go s.superviseRun(created.RunID, agent, vm.VMIP, release)

	logInfo("run_start", append([]any{"agent", agent, "run_id", created.RunID}, spanLogAttrs(serverSpan)...)...)
	mirrorResponse(w, resp.StatusCode, resp.Header.Get("Content-Type"), respBody)
}

// handleRunItem serves the suffix-routed run endpoints: GET /v1/runs/{id},
// GET /v1/runs/{id}/events, POST /v1/runs/{id}/approval, POST /v1/runs/{id}/stop.
// All route to the VM the run is bound to; none acquires a slot.
func (s *server) handleRunItem(w http.ResponseWriter, r *http.Request) {
	tok, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/v1/runs/")
	id, action := rest, ""
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		id, action = rest[:i], rest[i+1:]
	}
	if id == "" {
		writeError(w, http.StatusNotFound, "Run not found", "invalid_request_error")
		return
	}

	binding, found := s.runs.get(id)
	if !found {
		writeError(w, http.StatusNotFound, fmt.Sprintf("Run not found: %s", id), "invalid_request_error")
		return
	}
	if ri := reqInfoFrom(r.Context()); ri != nil {
		ri.agent = binding.agent
		ri.span.SetAttr("hermes.agent", binding.agent)
		ri.span.SetAttr("hermes.mode", "run")
		ri.span.SetAttr("hermes.run_id", id)
	}
	if !agentAllowed(tok.Agents, binding.agent) {
		writeError(w, http.StatusForbidden,
			fmt.Sprintf("Token not authorized for agent %s", binding.agent), "invalid_request_error")
		return
	}

	switch action {
	case "":
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "Method not allowed", "invalid_request_error")
			return
		}
		s.proxyRunJSON(w, r, binding, http.MethodGet, "/v1/runs/"+id, nil)
	case "events":
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "Method not allowed", "invalid_request_error")
			return
		}
		s.proxyRunEvents(w, r, binding, id)
	case "approval":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "Method not allowed", "invalid_request_error")
			return
		}
		s.proxyRunItemPost(w, r, binding, "/v1/runs/"+id+"/approval")
	case "stop":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "Method not allowed", "invalid_request_error")
			return
		}
		s.proxyRunItemPost(w, r, binding, "/v1/runs/"+id+"/stop")
	default:
		writeError(w, http.StatusNotFound, "Run not found", "invalid_request_error")
	}
}

// proxyRunItemPost reads the (small) request body and proxies a POST to the
// run's VM (approval/stop), mirroring the response.
func (s *server) proxyRunItemPost(w http.ResponseWriter, r *http.Request, b runBinding, path string) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRunReqBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, "Failed to read request body", "invalid_request_error")
		return
	}
	s.proxyRunJSON(w, r, b, http.MethodPost, path, body)
}

// proxyRunJSON proxies a non-streaming JSON round-trip to the run's VM and
// mirrors status/content-type/body. Approval and stop bodies are forwarded
// verbatim (no model rewrite — they carry no routing model).
func (s *server) proxyRunJSON(w http.ResponseWriter, r *http.Request, b runBinding, method, path string, body []byte) {
	vm := VMInfo{VMIP: b.vmIP}
	req, err := buildVMRunRequest(r.Context(), s.cfg, vm, b.agent, method, path,
		body, r.Header.Get("X-Hermes-Session-Id"), r.Header.Get("X-Hermes-Session-Key"),
		reqTraceparent(reqInfoFrom(r.Context())), "application/json", false)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to build downstream request", "internal_error")
		return
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		s.mx.IncUpstreamError(b.agent, "connect")
		writeError(w, http.StatusBadGateway,
			fmt.Sprintf("Failed to reach VM for agent %s: %v", b.agent, err), "internal_error")
		return
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxRunRespBytes))
	if resp.StatusCode >= 500 {
		s.mx.IncUpstreamError(b.agent, "status_5xx")
	}
	mirrorResponse(w, resp.StatusCode, resp.Header.Get("Content-Type"), respBody)
}

// proxyRunEvents stream-proxies the run's SSE event stream to the client
// unbuffered (the same flush-per-chunk copy the chat path uses). It holds no
// slot — the supervisor already owns it.
func (s *server) proxyRunEvents(w http.ResponseWriter, r *http.Request, b runBinding, id string) {
	vm := VMInfo{VMIP: b.vmIP}
	req, err := buildVMRunRequest(r.Context(), s.cfg, vm, b.agent, http.MethodGet, "/v1/runs/"+id+"/events",
		nil, r.Header.Get("X-Hermes-Session-Id"), r.Header.Get("X-Hermes-Session-Key"),
		reqTraceparent(reqInfoFrom(r.Context())), "text/event-stream", false)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to build downstream request", "internal_error")
		return
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		s.mx.IncUpstreamError(b.agent, "connect")
		writeError(w, http.StatusBadGateway,
			fmt.Sprintf("Failed to reach VM for agent %s: %v", b.agent, err), "internal_error")
		return
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)
	written, _ := streamBody(w, resp.Body, nil)
	s.mx.AddStreamBytes(b.agent, written)
}

// superviseRun holds the agent's slot for the lifetime of a run, polling the
// backend's status endpoint, and releases it (and drops the registry entry) on
// a terminal status, on TTL, or when the VM has clearly gone away. It runs on
// its own background context — the create request that spawned it returned the
// 202 long ago.
func (s *server) superviseRun(runID, agent, vmIP string, release func()) {
	defer release()
	defer s.runs.delete(runID)

	deadline := time.Now().Add(runSupervisorTTL)
	seen := false
	errs := 0
	for {
		if time.Now().After(deadline) {
			logWarn("run_supervisor_ttl", "agent", agent, "run_id", runID)
			return
		}
		status, ok := s.pollRunStatus(vmIP, agent, runID)
		switch {
		case ok:
			seen = true
			errs = 0
			if isTerminalRunStatus(status) {
				logInfo("run_finish", "agent", agent, "run_id", runID, "status", status)
				return
			}
		case seen:
			// The run existed and now the status is unreachable/404 — the
			// backend swept it or the VM died; treat as terminal.
			logInfo("run_finish", "agent", agent, "run_id", runID, "status", "gone")
			return
		default:
			// Never observed alive yet — tolerate a short grace so a slow boot
			// or create/status race doesn't drop the run.
			errs++
			if errs >= runVMGraceErrors {
				logWarn("run_supervisor_giveup", "agent", agent, "run_id", runID)
				return
			}
		}
		time.Sleep(runPollInterval)
	}
}

// pollRunStatus does one GET /v1/runs/{id} against the backend, returning the
// status string. ok is false on a transport error or a non-2xx (incl. 404).
func (s *server) pollRunStatus(vmIP, agent, runID string) (status string, ok bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := buildVMRunRequest(ctx, s.cfg, VMInfo{VMIP: vmIP}, agent, http.MethodGet,
		"/v1/runs/"+runID, nil, "", "", "", "application/json", false)
	if err != nil {
		return "", false
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", false
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxRunRespBytes))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", false
	}
	var st runStatusShape
	if json.Unmarshal(respBody, &st) != nil {
		return "", false
	}
	return st.Status, true
}

// buildVMRunRequest builds a downstream request to a run endpoint on the VM,
// reusing the session/traceparent headers and per-agent bearer from the chat
// path. rewriteModel applies the agent's model rewrite to the body (create
// only); accept sets the Accept header (application/json or text/event-stream).
func buildVMRunRequest(ctx context.Context, cfg *Config, vm VMInfo, agent, method, path string,
	body []byte, sessionID, sessionKey, traceparent, accept string, rewriteModel bool) (*http.Request, error) {
	target := fmt.Sprintf("http://%s:%d%s", vm.VMIP, cfg.VMGatewayPort, path)

	var reader io.Reader
	if body != nil {
		out := body
		if rewriteModel {
			out = rewriteModelField(cfg, agent, body)
		}
		reader = bytes.NewReader(out)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	if sessionID != "" {
		req.Header.Set("X-Hermes-Session-Id", sessionID)
	}
	if sessionKey != "" {
		req.Header.Set("X-Hermes-Session-Key", sessionKey)
	}
	if traceparent != "" {
		req.Header.Set("Traceparent", traceparent)
	}
	if ac, ok := cfg.Agents[agent]; ok && ac.APIServerKey != "" {
		req.Header.Set("Authorization", "Bearer "+ac.APIServerKey)
	}
	return req, nil
}

// mirrorResponse copies a downstream status/content-type/body to the client.
func mirrorResponse(w http.ResponseWriter, status int, contentType string, body []byte) {
	if contentType != "" {
		w.Header().Set("Content-Type", contentType)
	} else {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// reqTraceparent returns the traceparent header value for the request's server
// span (empty when tracing is off), so follow-up run proxies stay linked to the
// same trace as their client call.
func reqTraceparent(ri *reqInfo) string {
	if ri == nil || ri.span == nil {
		return ""
	}
	return formatTraceparent(ri.span.Context())
}
