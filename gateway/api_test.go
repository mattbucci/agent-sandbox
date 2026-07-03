package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// Placeholder bearer tokens (never real secrets).
const (
	adminToken  = "hgw-PLACEHOLDER-TEST-TOKEN-admin-000000000000"
	scopedToken = "hgw-PLACEHOLDER-TEST-TOKEN-scoped-00000000000"
)

// apiTestConfig builds a two-token (admin "*" + feature-dev-scoped), two-agent
// config over a temp state dir.
func apiTestConfig(t *testing.T) *Config {
	t.Helper()
	cfg := &Config{
		StateDir:     t.TempDir(),
		DefaultAgent: "feature-dev",
		Tokens: []Token{
			{Name: "hermes-webui", Token: adminToken, Agents: []string{"*"}},
			{Name: "scoped", Token: scopedToken, Agents: []string{"feature-dev"}},
		},
		Agents: map[string]AgentConfig{
			"feature-dev": {},
			"debugger":    {},
		},
	}
	cfg.applyDefaults()
	return cfg
}

// newAPIServer wires a full server (scheduler + optional store) and serves
// its routes() mux over httptest.
func newAPIServer(t *testing.T, cfg *Config) (*server, *httptest.Server) {
	t.Helper()
	var store *TaskStore
	if cfg.TasksEnabled() {
		st, err := NewTaskStore(cfg)
		if err != nil {
			t.Fatalf("NewTaskStore: %v", err)
		}
		store = st
	}
	s := &server{cfg: cfg, sched: NewScheduler(cfg), store: store}
	ts := httptest.NewServer(s.routes())
	t.Cleanup(ts.Close)
	return s, ts
}

// apiDo performs one request and returns the response plus the drained body.
func apiDo(t *testing.T, method, url, token string, body []byte, hdr map[string]string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	b, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp, b
}

// assertJSONGolden asserts an exact byte-level JSON response (writeJSON emits
// compact JSON with sorted map keys and a trailing newline).
func assertJSONGolden(t *testing.T, resp *http.Response, body []byte, wantStatus int, wantBody string) {
	t.Helper()
	if resp.StatusCode != wantStatus {
		t.Fatalf("status = %d, want %d (body %s)", resp.StatusCode, wantStatus, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type = %q", ct)
	}
	if string(body) != wantBody {
		t.Fatalf("body = %q, want %q", body, wantBody)
	}
}

// TestLegacyEndpointGoldens locks the byte-exact wire contract of the four
// legacy endpoints' JSON bodies and the 401/403/405/502 envelopes.
func TestLegacyEndpointGoldens(t *testing.T) {
	cfg := apiTestConfig(t)
	_, ts := newAPIServer(t, cfg)

	resp, body := apiDo(t, http.MethodGet, ts.URL+"/health", "", nil, nil)
	assertJSONGolden(t, resp, body, 200, "{\"status\":\"ok\"}\n")

	// Capabilities (no auth): rich object; default agent (feature-dev) has no
	// approval flag, so every run/approval feature is false. Parsed rather than
	// byte-golden because the object carries more than the two legacy flags.
	resp, body = apiDo(t, http.MethodGet, ts.URL+"/v1/capabilities", "", nil, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("capabilities status = %d", resp.StatusCode)
	}
	{
		var cap struct {
			Object   string          `json:"object"`
			Features map[string]bool `json:"features"`
		}
		if err := json.Unmarshal(body, &cap); err != nil {
			t.Fatalf("capabilities decode: %v (%s)", err, body)
		}
		if cap.Object != "hermes.api_server.capabilities" {
			t.Fatalf("capabilities object = %q", cap.Object)
		}
		if cap.Features["approval_events"] || cap.Features["run_approval_response"] {
			t.Fatalf("default-agent approval flags should be false: %v", cap.Features)
		}
	}

	// Models with the scoped token: deterministic single-agent list.
	resp, body = apiDo(t, http.MethodGet, ts.URL+"/v1/models", scopedToken, nil, nil)
	assertJSONGolden(t, resp, body, 200,
		"{\"data\":[{\"id\":\"feature-dev\",\"object\":\"model\",\"owned_by\":\"hermes-gateway\"}],\"object\":\"list\"}\n")

	// 401 envelope: missing and wrong bearer.
	want401 := "{\"error\":{\"message\":\"Invalid API key\",\"type\":\"invalid_request_error\"}}\n"
	resp, body = apiDo(t, http.MethodGet, ts.URL+"/v1/models", "", nil, nil)
	assertJSONGolden(t, resp, body, 401, want401)
	resp, body = apiDo(t, http.MethodPost, ts.URL+"/v1/chat/completions",
		"hgw-PLACEHOLDER-TEST-TOKEN-wrong", []byte(`{"model":"feature-dev"}`), nil)
	assertJSONGolden(t, resp, body, 401, want401)

	// 403 envelope: scoped token, out-of-scope agent.
	resp, body = apiDo(t, http.MethodPost, ts.URL+"/v1/chat/completions",
		scopedToken, []byte(`{"model":"debugger","messages":[]}`), nil)
	assertJSONGolden(t, resp, body, 403,
		"{\"error\":{\"message\":\"Token not authorized for agent debugger\",\"type\":\"invalid_request_error\"}}\n")

	// 405 envelope on non-POST chat.
	resp, body = apiDo(t, http.MethodGet, ts.URL+"/v1/chat/completions", adminToken, nil, nil)
	assertJSONGolden(t, resp, body, 405,
		"{\"error\":{\"message\":\"Method not allowed\",\"type\":\"invalid_request_error\"}}\n")

	// 502 envelope: configured agent, no running VM.
	resp, body = apiDo(t, http.MethodPost, ts.URL+"/v1/chat/completions",
		adminToken, []byte(`{"model":"feature-dev","messages":[]}`), nil)
	assertJSONGolden(t, resp, body, 502,
		"{\"error\":{\"message\":\"No running VM for agent feature-dev\",\"type\":\"invalid_request_error\"}}\n")

	// 502 envelope: unconfigured agent (admin "*" scope allows anything).
	resp, body = apiDo(t, http.MethodPost, ts.URL+"/v1/chat/completions",
		adminToken, []byte(`{"model":"nope","messages":[]}`), nil)
	assertJSONGolden(t, resp, body, 502,
		"{\"error\":{\"message\":\"No running VM for agent nope\",\"type\":\"invalid_request_error\"}}\n")
}

// TestChatPassthrough: request body bytes reach the VM verbatim (no model
// rewrite configured), X-Hermes-Session-* pass through, and the SSE response
// bytes plus Content-Type come back unmodified.
func TestChatPassthrough(t *testing.T) {
	const vmResponse = "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hello\"},\"finish_reason\":null}]}\n\n" +
		"data: [DONE]\n\n"
	fv := newFakeVM(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(vmResponse))
	})
	cfg := apiTestConfig(t)
	cfg.VMGatewayPort = fv.port(t)
	_, ts := newAPIServer(t, cfg)
	writeVMInfo(t, cfg.StateDir, "feature-dev")

	reqBody := []byte(`{"model":"feature-dev","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	resp, body := apiDo(t, http.MethodPost, ts.URL+"/v1/chat/completions", adminToken, reqBody, map[string]string{
		"X-Hermes-Session-Id":  "webui:sess-1",
		"X-Hermes-Session-Key": "hsk-PLACEHOLDER-TEST-KEY-1",
	})
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("status=%d ct=%q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if string(body) != vmResponse {
		t.Fatalf("response body = %q, want verbatim %q", body, vmResponse)
	}

	reqs := fv.requests()
	if len(reqs) != 1 {
		t.Fatalf("fake VM saw %d requests", len(reqs))
	}
	got := reqs[0]
	if !bytes.Equal(got.body, reqBody) {
		t.Fatalf("VM body = %q, want verbatim %q", got.body, reqBody)
	}
	if got.header.Get("X-Hermes-Session-Id") != "webui:sess-1" ||
		got.header.Get("X-Hermes-Session-Key") != "hsk-PLACEHOLDER-TEST-KEY-1" {
		t.Fatalf("session headers = %v", got.header)
	}
	if got.header.Get("Accept") != "text/event-stream" || got.header.Get("Content-Type") != "application/json" {
		t.Fatalf("content headers = %v", got.header)
	}
	if got.header.Get("Authorization") != "" {
		t.Fatalf("unexpected downstream auth %q", got.header.Get("Authorization"))
	}
}

// TestChatModelRewriteAndKey: an agent with model + api_server_key configured
// gets the rewritten model field and the downstream bearer.
func TestChatModelRewriteAndKey(t *testing.T) {
	fv := newFakeVM(t, happySSE)
	cfg := apiTestConfig(t)
	cfg.VMGatewayPort = fv.port(t)
	cfg.Agents["hermes"] = AgentConfig{Model: "gemma", APIServerKey: "hga-PLACEHOLDER-TEST-KEY-2"}
	_, ts := newAPIServer(t, cfg)
	writeVMInfo(t, cfg.StateDir, "hermes")

	resp, _ := apiDo(t, http.MethodPost, ts.URL+"/v1/chat/completions", adminToken,
		[]byte(`{"model":"hermes","messages":[{"role":"user","content":"hi"}]}`), nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	reqs := fv.requests()
	var sent map[string]any
	if err := json.Unmarshal(reqs[0].body, &sent); err != nil {
		t.Fatalf("VM body: %v", err)
	}
	if sent["model"] != "gemma" {
		t.Fatalf("model = %v, want gemma", sent["model"])
	}
	if got := reqs[0].header.Get("Authorization"); got != "Bearer hga-PLACEHOLDER-TEST-KEY-2" {
		t.Fatalf("downstream auth = %q", got)
	}
}

// waitForWaiting polls the scheduler snapshot until agent has n waiters.
func waitForWaiting(t *testing.T, sched *Scheduler, agent string, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, snap := range sched.Snapshot() {
			if snap.Agent == agent && len(snap.Waiting) == n {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("agent %s never reached %d waiters", agent, n)
}

// TestChatQueueFull429: at concurrency 1 with sync_queue_max 1, the third
// concurrent chat gets 429 + Retry-After with the documented envelope.
func TestChatQueueFull429(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 8)
	fv := newFakeVM(t, func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		<-release
		happySSE(w, r)
	})
	cfg := apiTestConfig(t)
	cfg.VMGatewayPort = fv.port(t)
	cfg.Scheduler.SyncQueueMax = 1
	s, ts := newAPIServer(t, cfg)
	writeVMInfo(t, cfg.StateDir, "feature-dev")

	chatBody := []byte(`{"model":"feature-dev","messages":[{"role":"user","content":"hi"}]}`)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, _ := apiDo(t, http.MethodPost, ts.URL+"/v1/chat/completions", adminToken, chatBody, nil)
			if resp.StatusCode != 200 {
				t.Errorf("held request status = %d", resp.StatusCode)
			}
		}()
		if i == 0 {
			<-started // first request holds the only slot
		} else {
			waitForWaiting(t, s.sched, "feature-dev", 1) // second is queued
		}
	}

	resp, body := apiDo(t, http.MethodPost, ts.URL+"/v1/chat/completions", adminToken, chatBody, nil)
	if resp.Header.Get("Retry-After") != "15" {
		t.Fatalf("Retry-After = %q", resp.Header.Get("Retry-After"))
	}
	assertJSONGolden(t, resp, body, 429,
		"{\"error\":{\"message\":\"Agent feature-dev is busy and its queue is full\",\"type\":\"rate_limit_error\"}}\n")

	close(release)
	wg.Wait()
}

// TestChatWaitTimeout503: a queued sync request that outlives
// sync_queue_wait_s gets 503 + Retry-After with the documented envelope.
func TestChatWaitTimeout503(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 8)
	fv := newFakeVM(t, func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		<-release
		happySSE(w, r)
	})
	cfg := apiTestConfig(t)
	cfg.VMGatewayPort = fv.port(t)
	cfg.Scheduler.SyncQueueWaitS = 1
	_, ts := newAPIServer(t, cfg)
	writeVMInfo(t, cfg.StateDir, "feature-dev")

	chatBody := []byte(`{"model":"feature-dev","messages":[{"role":"user","content":"hi"}]}`)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		resp, _ := apiDo(t, http.MethodPost, ts.URL+"/v1/chat/completions", adminToken, chatBody, nil)
		if resp.StatusCode != 200 {
			t.Errorf("held request status = %d", resp.StatusCode)
		}
	}()
	<-started

	resp, body := apiDo(t, http.MethodPost, ts.URL+"/v1/chat/completions", adminToken, chatBody, nil)
	if resp.Header.Get("Retry-After") != "15" {
		t.Fatalf("Retry-After = %q", resp.Header.Get("Retry-After"))
	}
	assertJSONGolden(t, resp, body, 503,
		"{\"error\":{\"message\":\"Timed out waiting for agent feature-dev\",\"type\":\"server_error\"}}\n")

	close(release)
	wg.Wait()
}

// TestTaskSubmitAndCRUD walks the whole task lifecycle over HTTP: submit,
// get, list, output, cancel (idempotent), delete.
func TestTaskSubmitAndCRUD(t *testing.T) {
	cfg := apiTestConfig(t)
	s, ts := newAPIServer(t, cfg)

	// Submit with input + clamped priority.
	resp, body := apiDo(t, http.MethodPost, ts.URL+"/v1/tasks", adminToken,
		[]byte(`{"agent":"feature-dev","input":"do the thing","priority":500}`), nil)
	if resp.StatusCode != 201 {
		t.Fatalf("submit status = %d (%s)", resp.StatusCode, body)
	}
	var task Task
	if err := json.Unmarshal(body, &task); err != nil {
		t.Fatalf("submit body: %v", err)
	}
	if task.Object != "task" || task.Agent != "feature-dev" || task.State != TaskPending ||
		task.Priority != 100 || task.SessionID != "task:"+task.ID {
		t.Fatalf("task = %+v", task)
	}
	if task.TimeoutS != 3600 || task.MaxAttempts != 2 || task.RetryOnPartial {
		t.Fatalf("defaults not applied: %+v", task)
	}
	var reqShape taskRequestShape
	if err := json.Unmarshal(task.Request, &reqShape); err != nil || len(reqShape.Messages) != 1 {
		t.Fatalf("request = %s", task.Request)
	}

	// Wire record marshals with the fields the plan requires present.
	for _, key := range []string{"\"not_before\":null", "\"started_at\":null", "\"finished_at\":null",
		"\"error\":null", "\"result\":null", "\"trace_ids\":[]", "\"attempt_history\":[]",
		"\"submitted_by\":\"hermes-webui\""} {
		if !strings.Contains(string(body), key) {
			t.Fatalf("submit body missing %s: %s", key, body)
		}
	}

	// Get by id.
	resp, body = apiDo(t, http.MethodGet, ts.URL+"/v1/tasks/"+task.ID, adminToken, nil, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("get status = %d", resp.StatusCode)
	}
	var got Task
	if err := json.Unmarshal(body, &got); err != nil || got.ID != task.ID {
		t.Fatalf("get body: %v %s", err, body)
	}

	// Unknown id: exact 404 envelope.
	resp, body = apiDo(t, http.MethodGet, ts.URL+"/v1/tasks/t-nope", adminToken, nil, nil)
	assertJSONGolden(t, resp, body, 404,
		"{\"error\":{\"message\":\"No such task\",\"type\":\"invalid_request_error\"}}\n")

	// List: one pending summary.
	resp, body = apiDo(t, http.MethodGet, ts.URL+"/v1/tasks?state=active", adminToken, nil, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("list status = %d", resp.StatusCode)
	}
	var list struct {
		Object  string        `json:"object"`
		Data    []taskSummary `json:"data"`
		HasMore bool          `json:"has_more"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("list body: %v", err)
	}
	if list.Object != "list" || len(list.Data) != 1 || list.HasMore {
		t.Fatalf("list = %+v", list)
	}
	sum := list.Data[0]
	if sum.ID != task.ID || sum.Object != "task.summary" || sum.State != TaskPending || sum.AgeS < 0 {
		t.Fatalf("summary = %+v", sum)
	}

	// Output: empty before any attempt, then the spool snapshot verbatim.
	resp, body = apiDo(t, http.MethodGet, ts.URL+"/v1/tasks/"+task.ID+"/output", adminToken, nil, nil)
	if resp.StatusCode != 200 || len(body) != 0 {
		t.Fatalf("empty output: status=%d body=%q", resp.StatusCode, body)
	}
	if err := os.WriteFile(s.store.OutputPath(task.ID), []byte("spooled output"), 0o644); err != nil {
		t.Fatalf("write spool: %v", err)
	}
	resp, body = apiDo(t, http.MethodGet, ts.URL+"/v1/tasks/"+task.ID+"/output", adminToken, nil, nil)
	if resp.StatusCode != 200 || string(body) != "spooled output" {
		t.Fatalf("output: status=%d body=%q", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Fatalf("output content-type = %q", ct)
	}

	// Delete non-terminal: 409.
	resp, body = apiDo(t, http.MethodDelete, ts.URL+"/v1/tasks/"+task.ID, adminToken, nil, nil)
	assertJSONGolden(t, resp, body, 409,
		"{\"error\":{\"message\":\"Task is not terminal\",\"type\":\"invalid_request_error\"}}\n")

	// Cancel pending: 200, cancelled, no already_terminal marker.
	resp, body = apiDo(t, http.MethodPost, ts.URL+"/v1/tasks/"+task.ID+"/cancel", adminToken, nil, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("cancel status = %d", resp.StatusCode)
	}
	var cancelled struct {
		Task
		AlreadyTerminal *bool `json:"already_terminal"`
	}
	if err := json.Unmarshal(body, &cancelled); err != nil {
		t.Fatalf("cancel body: %v", err)
	}
	if cancelled.State != TaskCancelled || cancelled.AlreadyTerminal != nil {
		t.Fatalf("cancel = state %s already_terminal %v", cancelled.State, cancelled.AlreadyTerminal)
	}

	// Cancel again: idempotent, already_terminal:true.
	resp, body = apiDo(t, http.MethodPost, ts.URL+"/v1/tasks/"+task.ID+"/cancel", adminToken, nil, nil)
	if resp.StatusCode != 200 || !strings.Contains(string(body), "\"already_terminal\":true") {
		t.Fatalf("re-cancel: status=%d body=%s", resp.StatusCode, body)
	}

	// Delete terminal: 200, then gone (and sidecar spool removed).
	resp, body = apiDo(t, http.MethodDelete, ts.URL+"/v1/tasks/"+task.ID, adminToken, nil, nil)
	assertJSONGolden(t, resp, body, 200,
		fmt.Sprintf("{\"id\":%q,\"object\":\"task.deleted\",\"deleted\":true}\n", task.ID))
	resp, _ = apiDo(t, http.MethodGet, ts.URL+"/v1/tasks/"+task.ID, adminToken, nil, nil)
	if resp.StatusCode != 404 {
		t.Fatalf("get after delete = %d", resp.StatusCode)
	}
	if _, err := os.Stat(s.store.OutputPath(task.ID)); !os.IsNotExist(err) {
		t.Fatalf("spool not deleted: %v", err)
	}
}

// TestTaskSubmitValidation covers every 400/413 validation branch.
func TestTaskSubmitValidation(t *testing.T) {
	cfg := apiTestConfig(t)
	_, ts := newAPIServer(t, cfg)

	cases := []struct {
		name string
		body string
		want string
	}{
		{"missing agent", `{"input":"x"}`, "Missing required field: agent"},
		{"unknown agent", `{"agent":"nope","input":"x"}`, "Unknown agent nope"},
		{"neither input nor request", `{"agent":"feature-dev"}`, "Exactly one of input or request"},
		{"both input and request", `{"agent":"feature-dev","input":"x","request":{"messages":[{"role":"user","content":"x"}]}}`, "Exactly one of input or request"},
		{"request without messages", `{"agent":"feature-dev","request":{"foo":1}}`, "non-empty messages array"},
		{"timeout too small", `{"agent":"feature-dev","input":"x","timeout_s":0}`, "timeout_s must be between"},
		{"timeout too large", `{"agent":"feature-dev","input":"x","timeout_s":90000}`, "timeout_s must be between"},
		{"deadline too small", `{"agent":"feature-dev","input":"x","deadline_s":30}`, "deadline_s must be between"},
		{"deadline too large", `{"agent":"feature-dev","input":"x","deadline_s":700000}`, "deadline_s must be between"},
		{"max_attempts too small", `{"agent":"feature-dev","input":"x","max_attempts":0}`, "max_attempts must be between"},
		{"max_attempts too large", `{"agent":"feature-dev","input":"x","max_attempts":6}`, "max_attempts must be between"},
		{"bad not_before", `{"agent":"feature-dev","input":"x","not_before":"tomorrow"}`, "not_before must be RFC3339"},
		{"invalid json", `{nope`, "Invalid JSON body"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := apiDo(t, http.MethodPost, ts.URL+"/v1/tasks", adminToken, []byte(tc.body), nil)
			if resp.StatusCode != 400 {
				t.Fatalf("status = %d (%s)", resp.StatusCode, body)
			}
			if !strings.Contains(string(body), tc.want) {
				t.Fatalf("body = %s, want substring %q", body, tc.want)
			}
		})
	}

	// model aliases agent.
	resp, body := apiDo(t, http.MethodPost, ts.URL+"/v1/tasks", adminToken,
		[]byte(`{"model":"feature-dev","input":"x"}`), nil)
	if resp.StatusCode != 201 {
		t.Fatalf("model-alias submit = %d (%s)", resp.StatusCode, body)
	}

	// Oversized body: 413.
	huge := fmt.Sprintf(`{"agent":"feature-dev","input":%q}`, strings.Repeat("x", maxTaskBodyBytes+16))
	resp, body = apiDo(t, http.MethodPost, ts.URL+"/v1/tasks", adminToken, []byte(huge), nil)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized submit = %d (%s)", resp.StatusCode, body)
	}

	// Bad method on the collection.
	resp, _ = apiDo(t, http.MethodPut, ts.URL+"/v1/tasks", adminToken, nil, nil)
	if resp.StatusCode != 405 {
		t.Fatalf("PUT /v1/tasks = %d", resp.StatusCode)
	}
}

// TestTaskScopeEnforcement: a feature-dev-scoped token cannot submit, read,
// cancel or even see debugger tasks.
func TestTaskScopeEnforcement(t *testing.T) {
	cfg := apiTestConfig(t)
	_, ts := newAPIServer(t, cfg)

	// Submit one task per agent with the admin token.
	resp, body := apiDo(t, http.MethodPost, ts.URL+"/v1/tasks", adminToken,
		[]byte(`{"agent":"debugger","input":"x"}`), nil)
	if resp.StatusCode != 201 {
		t.Fatalf("submit debugger = %d", resp.StatusCode)
	}
	var debugTask Task
	if err := json.Unmarshal(body, &debugTask); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	resp, _ = apiDo(t, http.MethodPost, ts.URL+"/v1/tasks", adminToken,
		[]byte(`{"agent":"feature-dev","input":"x"}`), nil)
	if resp.StatusCode != 201 {
		t.Fatalf("submit feature-dev = %d", resp.StatusCode)
	}

	// Scoped submit for debugger: 403 envelope.
	resp, body = apiDo(t, http.MethodPost, ts.URL+"/v1/tasks", scopedToken,
		[]byte(`{"agent":"debugger","input":"x"}`), nil)
	assertJSONGolden(t, resp, body, 403,
		"{\"error\":{\"message\":\"Token not authorized for agent debugger\",\"type\":\"invalid_request_error\"}}\n")

	// Scoped list: only feature-dev tasks are visible.
	resp, body = apiDo(t, http.MethodGet, ts.URL+"/v1/tasks", scopedToken, nil, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("scoped list = %d", resp.StatusCode)
	}
	var list taskListResponse
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list.Data) != 1 || list.Data[0].Agent != "feature-dev" {
		t.Fatalf("scoped list = %+v", list.Data)
	}

	// Scoped item access on a debugger task: 403 on every route.
	for _, probe := range []struct{ method, suffix string }{
		{http.MethodGet, ""},
		{http.MethodGet, "/output"},
		{http.MethodPost, "/cancel"},
		{http.MethodDelete, ""},
	} {
		resp, _ = apiDo(t, probe.method, ts.URL+"/v1/tasks/"+debugTask.ID+probe.suffix, scopedToken, nil, nil)
		if resp.StatusCode != 403 {
			t.Fatalf("%s %s = %d, want 403", probe.method, probe.suffix, resp.StatusCode)
		}
	}

	// No token at all: 401.
	resp, _ = apiDo(t, http.MethodGet, ts.URL+"/v1/tasks", "", nil, nil)
	if resp.StatusCode != 401 {
		t.Fatalf("unauthenticated list = %d", resp.StatusCode)
	}
}

// TestTaskListPaginationAndFilters: limit + keyset cursor + agent filter.
func TestTaskListPaginationAndFilters(t *testing.T) {
	cfg := apiTestConfig(t)
	_, ts := newAPIServer(t, cfg)

	var ids []string
	for i := 0; i < 3; i++ {
		resp, body := apiDo(t, http.MethodPost, ts.URL+"/v1/tasks", adminToken,
			[]byte(`{"agent":"feature-dev","input":"x"}`), nil)
		if resp.StatusCode != 201 {
			t.Fatalf("submit %d = %d", i, resp.StatusCode)
		}
		var task Task
		if err := json.Unmarshal(body, &task); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		ids = append(ids, task.ID)
	}

	resp, body := apiDo(t, http.MethodGet, ts.URL+"/v1/tasks?limit=2", adminToken, nil, nil)
	var page1 taskListResponse
	if err := json.Unmarshal(body, &page1); err != nil || resp.StatusCode != 200 {
		t.Fatalf("page1: %d %v", resp.StatusCode, err)
	}
	if len(page1.Data) != 2 || !page1.HasMore {
		t.Fatalf("page1 = %d items has_more=%v", len(page1.Data), page1.HasMore)
	}
	resp, body = apiDo(t, http.MethodGet,
		ts.URL+"/v1/tasks?limit=2&after="+page1.Data[1].ID, adminToken, nil, nil)
	var page2 taskListResponse
	if err := json.Unmarshal(body, &page2); err != nil || resp.StatusCode != 200 {
		t.Fatalf("page2: %d %v", resp.StatusCode, err)
	}
	if len(page2.Data) != 1 || page2.HasMore {
		t.Fatalf("page2 = %d items has_more=%v", len(page2.Data), page2.HasMore)
	}
	seen := map[string]bool{}
	for _, s := range append(page1.Data, page2.Data...) {
		seen[s.ID] = true
	}
	for _, id := range ids {
		if !seen[id] {
			t.Fatalf("id %s missing across pages", id)
		}
	}

	// Agent filter with no matches.
	resp, body = apiDo(t, http.MethodGet, ts.URL+"/v1/tasks?agent=debugger", adminToken, nil, nil)
	var empty taskListResponse
	if err := json.Unmarshal(body, &empty); err != nil || resp.StatusCode != 200 {
		t.Fatalf("filter: %d %v", resp.StatusCode, err)
	}
	if len(empty.Data) != 0 {
		t.Fatalf("agent filter leaked %d tasks", len(empty.Data))
	}

	// Bad query params.
	for _, q := range []string{"?limit=zero", "?limit=0", "?state=bogus"} {
		resp, _ = apiDo(t, http.MethodGet, ts.URL+"/v1/tasks"+q, adminToken, nil, nil)
		if resp.StatusCode != 400 {
			t.Fatalf("query %s = %d, want 400", q, resp.StatusCode)
		}
	}
}

// TestTaskPendingCap429: the per-agent non-terminal cap maps to 429 with
// Retry-After.
func TestTaskPendingCap429(t *testing.T) {
	cfg := apiTestConfig(t)
	cfg.Tasks.MaxPendingPerAgent = 1
	_, ts := newAPIServer(t, cfg)

	resp, _ := apiDo(t, http.MethodPost, ts.URL+"/v1/tasks", adminToken,
		[]byte(`{"agent":"feature-dev","input":"x"}`), nil)
	if resp.StatusCode != 201 {
		t.Fatalf("first submit = %d", resp.StatusCode)
	}
	resp, body := apiDo(t, http.MethodPost, ts.URL+"/v1/tasks", adminToken,
		[]byte(`{"agent":"feature-dev","input":"x"}`), nil)
	if resp.Header.Get("Retry-After") != "15" {
		t.Fatalf("Retry-After = %q", resp.Header.Get("Retry-After"))
	}
	assertJSONGolden(t, resp, body, 429,
		"{\"error\":{\"message\":\"Agent feature-dev has too many pending tasks\",\"type\":\"rate_limit_error\"}}\n")
}

// TestTasksDisabled404: with tasks.enabled=false the routes are not
// registered at all — byte-identical to any unknown path.
func TestTasksDisabled404(t *testing.T) {
	cfg := apiTestConfig(t)
	f := false
	cfg.Tasks.Enabled = &f
	_, ts := newAPIServer(t, cfg)

	for _, path := range []string{"/v1/tasks", "/v1/tasks/t-123", "/v1/tasks/t-123/output", "/v1/tasks/t-123/cancel"} {
		resp, body := apiDo(t, http.MethodGet, ts.URL+path, adminToken, nil, nil)
		if resp.StatusCode != 404 || string(body) != "404 page not found\n" {
			t.Fatalf("%s: status=%d body=%q", path, resp.StatusCode, body)
		}
	}

	// The legacy endpoints keep working.
	resp, body := apiDo(t, http.MethodGet, ts.URL+"/health", "", nil, nil)
	assertJSONGolden(t, resp, body, 200, "{\"status\":\"ok\"}\n")
}
