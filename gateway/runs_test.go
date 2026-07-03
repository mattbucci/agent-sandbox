package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// startMockVM serves the runs mock over httptest and returns its listen port.
// Unlike newFakeVM it does NOT pre-drain r.Body, so handlers that decode the
// request body (e.g. the approval choice) see it intact.
func startMockVM(t *testing.T, m *runsMock) int {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(m.handler))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse mock url: %v", err)
	}
	p, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("mock port: %v", err)
	}
	return p
}

// runsMock is a stateful fake in-VM backend implementing the five run
// endpoints (plus chat, so the slot test can drive a competing chat). Run
// status is test-controllable so a run can be pinned non-terminal until the
// test flips it.
type runsMock struct {
	mu            sync.Mutex
	status        map[string]string // run_id -> status
	approvals     []string          // choices received
	initialStatus string
	nextID        string
}

func newRunsMock() *runsMock {
	return &runsMock{status: map[string]string{}, initialStatus: "running", nextID: "run_test1"}
}

func (m *runsMock) set(id, status string) {
	m.mu.Lock()
	m.status[id] = status
	m.mu.Unlock()
}

func (m *runsMock) get(id string) (string, bool) {
	m.mu.Lock()
	s, ok := m.status[id]
	m.mu.Unlock()
	return s, ok
}

func (m *runsMock) choices() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.approvals...)
}

func (m *runsMock) handler(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	switch {
	case path == "/v1/chat/completions":
		happySSE(w, r)
		return
	case r.Method == http.MethodPost && path == "/v1/runs":
		id := m.nextID
		m.set(id, m.initialStatus)
		writeJSON(w, http.StatusAccepted, map[string]any{"run_id": id, "status": "started"})
		return
	case strings.HasPrefix(path, "/v1/runs/"):
		rest := strings.TrimPrefix(path, "/v1/runs/")
		id, action := rest, ""
		if i := strings.IndexByte(rest, '/'); i >= 0 {
			id, action = rest[:i], rest[i+1:]
		}
		st, known := m.get(id)
		if !known {
			writeJSON(w, http.StatusNotFound, map[string]any{
				"error": map[string]any{"message": "Run not found: " + id, "type": "invalid_request_error"}})
			return
		}
		switch action {
		case "":
			writeJSON(w, http.StatusOK, map[string]any{
				"object": "hermes.run", "run_id": id, "status": st})
		case "events":
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			fl, _ := w.(http.Flusher)
			fmt.Fprintf(w, "data: %s\n\n", `{"event":"message.delta","run_id":"`+id+`","delta":"hi"}`)
			fmt.Fprintf(w, "data: %s\n\n", `{"event":"approval.request","run_id":"`+id+`","choices":["once","session","always","deny"]}`)
			if fl != nil {
				fl.Flush()
			}
		case "approval":
			var body struct {
				Choice string `json:"choice"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			m.mu.Lock()
			m.approvals = append(m.approvals, body.Choice)
			m.mu.Unlock()
			m.set(id, "running")
			writeJSON(w, http.StatusOK, map[string]any{
				"object": "hermes.run.approval_response", "run_id": id, "choice": body.Choice, "resolved": 1})
		case "stop":
			m.set(id, "cancelled")
			writeJSON(w, http.StatusOK, map[string]any{"run_id": id, "status": "stopping"})
		default:
			writeJSON(w, http.StatusNotFound, map[string]any{
				"error": map[string]any{"message": "Not found", "type": "invalid_request_error"}})
		}
		return
	}
	w.WriteHeader(http.StatusNotFound)
}

// newRunsServer wires a server whose feature-dev/hermes agents resolve to the
// runs mock VM. hermes advertises approval; feature-dev does not.
func newRunsServer(t *testing.T, m *runsMock) (*server, *httptest.Server) {
	t.Helper()
	cfg := &Config{
		StateDir:      t.TempDir(),
		DefaultAgent:  "feature-dev",
		VMGatewayPort: startMockVM(t, m),
		Tokens: []Token{
			{Name: "admin", Token: adminToken, Agents: []string{"*"}},
			{Name: "scoped", Token: scopedToken, Agents: []string{"feature-dev"}},
		},
		Agents: map[string]AgentConfig{
			"feature-dev": {},
			"hermes":      {Approval: true},
		},
	}
	cfg.applyDefaults()
	writeVMInfo(t, cfg.StateDir, "feature-dev")
	writeVMInfo(t, cfg.StateDir, "hermes")
	s := &server{cfg: cfg, sched: NewScheduler(cfg)}
	ts := httptest.NewServer(s.routes())
	t.Cleanup(ts.Close)
	return s, ts
}

func decodeFeatures(t *testing.T, body []byte) map[string]bool {
	t.Helper()
	var cap struct {
		Features map[string]bool `json:"features"`
	}
	if err := json.Unmarshal(body, &cap); err != nil {
		t.Fatalf("capabilities decode: %v (%s)", err, body)
	}
	return cap.Features
}

// TestCapabilitiesPerAgent checks the runs feature flags follow the resolved
// agent's static approval config.
func TestCapabilitiesPerAgent(t *testing.T) {
	m := newRunsMock()
	_, ts := newRunsServer(t, m)

	// hermes: approval on -> run flags true.
	_, body := apiDo(t, http.MethodGet, ts.URL+"/v1/capabilities?model=hermes", "", nil, nil)
	f := decodeFeatures(t, body)
	for _, k := range []string{"approval_events", "run_approval_response", "run_events_sse", "run_submission", "run_stop", "run_status"} {
		if !f[k] {
			t.Fatalf("hermes capabilities: %s should be true (%v)", k, f)
		}
	}
	if !f["chat_completions"] {
		t.Fatalf("chat_completions should always be true")
	}

	// feature-dev: approval off -> run flags false.
	_, body = apiDo(t, http.MethodGet, ts.URL+"/v1/capabilities?model=feature-dev", "", nil, nil)
	f = decodeFeatures(t, body)
	if f["approval_events"] || f["run_approval_response"] {
		t.Fatalf("feature-dev approval flags should be false: %v", f)
	}

	// unknown model -> false; no model -> default agent (feature-dev) -> false.
	_, body = apiDo(t, http.MethodGet, ts.URL+"/v1/capabilities?model=nope", "", nil, nil)
	if decodeFeatures(t, body)["approval_events"] {
		t.Fatalf("unknown model should not advertise approval")
	}
	_, body = apiDo(t, http.MethodGet, ts.URL+"/v1/capabilities", "", nil, nil)
	if decodeFeatures(t, body)["approval_events"] {
		t.Fatalf("default agent (feature-dev) should not advertise approval")
	}
}

// TestRunCreateRegistryAndRouting drives create -> status -> events -> approval
// -> stop and asserts each follow-up routes to the bound VM.
func TestRunCreateRegistryAndRouting(t *testing.T) {
	m := newRunsMock()
	s, ts := newRunsServer(t, m)
	// Keep the run non-terminal so the supervisor holds the registry entry.
	m.initialStatus = "running"

	resp, body := apiDo(t, http.MethodPost, ts.URL+"/v1/runs", adminToken,
		[]byte(`{"model":"hermes","input":"do something"}`), nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("create status = %d (%s)", resp.StatusCode, body)
	}
	var created struct {
		RunID  string `json:"run_id"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(body, &created); err != nil || created.RunID == "" {
		t.Fatalf("create body = %s (err %v)", body, err)
	}
	if _, ok := s.runs.get(created.RunID); !ok {
		t.Fatalf("registry missing run %s", created.RunID)
	}

	// status
	resp, body = apiDo(t, http.MethodGet, ts.URL+"/v1/runs/"+created.RunID, adminToken, nil, nil)
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"status":"running"`) {
		t.Fatalf("status = %d %s", resp.StatusCode, body)
	}

	// events (SSE) — should carry the approval.request frame from the backend.
	resp, body = apiDo(t, http.MethodGet, ts.URL+"/v1/runs/"+created.RunID+"/events", adminToken, nil, nil)
	if resp.StatusCode != 200 || !strings.Contains(string(body), "approval.request") {
		t.Fatalf("events = %d %s", resp.StatusCode, body)
	}

	// approval
	resp, _ = apiDo(t, http.MethodPost, ts.URL+"/v1/runs/"+created.RunID+"/approval", adminToken,
		[]byte(`{"choice":"once"}`), nil)
	if resp.StatusCode != 200 {
		t.Fatalf("approval status = %d", resp.StatusCode)
	}
	if got := m.choices(); len(got) != 1 || got[0] != "once" {
		t.Fatalf("backend approvals = %v", got)
	}

	// stop
	resp, _ = apiDo(t, http.MethodPost, ts.URL+"/v1/runs/"+created.RunID+"/stop", adminToken, nil, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("stop status = %d", resp.StatusCode)
	}

	// unknown run id -> 404.
	resp, _ = apiDo(t, http.MethodGet, ts.URL+"/v1/runs/run_nope", adminToken, nil, nil)
	if resp.StatusCode != 404 {
		t.Fatalf("unknown run status = %d, want 404", resp.StatusCode)
	}
}

// TestRunAuthAndScope checks the runs endpoints enforce auth + agent scope.
func TestRunAuthAndScope(t *testing.T) {
	m := newRunsMock()
	_, ts := newRunsServer(t, m)

	// no token -> 401
	resp, _ := apiDo(t, http.MethodPost, ts.URL+"/v1/runs", "", []byte(`{"model":"hermes","input":"x"}`), nil)
	if resp.StatusCode != 401 {
		t.Fatalf("no-token create = %d, want 401", resp.StatusCode)
	}
	// scoped token, out-of-scope agent (hermes) -> 403
	resp, _ = apiDo(t, http.MethodPost, ts.URL+"/v1/runs", scopedToken, []byte(`{"model":"hermes","input":"x"}`), nil)
	if resp.StatusCode != 403 {
		t.Fatalf("scoped create hermes = %d, want 403", resp.StatusCode)
	}
	// wrong method on collection -> 405
	resp, _ = apiDo(t, http.MethodDelete, ts.URL+"/v1/runs", adminToken, nil, nil)
	if resp.StatusCode != 405 {
		t.Fatalf("DELETE /v1/runs = %d, want 405", resp.StatusCode)
	}
}

// TestRunSlotHeldUntilTerminal is the core admission-control assertion: a run
// holds the agent's single slot for its whole lifetime, so a competing chat is
// rejected until the run reaches a terminal status and the supervisor releases.
func TestRunSlotHeldUntilTerminal(t *testing.T) {
	// Fast supervisor polling; restore after.
	oldInterval := runPollInterval
	runPollInterval = 5 * time.Millisecond
	t.Cleanup(func() { runPollInterval = oldInterval })

	m := newRunsMock()
	cfg := &Config{
		StateDir:      t.TempDir(),
		DefaultAgent:  "hermes",
		VMGatewayPort: startMockVM(t, m),
		Tokens:        []Token{{Name: "admin", Token: adminToken, Agents: []string{"*"}}},
		Agents:        map[string]AgentConfig{"hermes": {Approval: true, Concurrency: 1}},
	}
	cfg.applyDefaults()
	cfg.Scheduler.SyncQueueMax = 0 // a busy agent rejects immediately (no queue)
	writeVMInfo(t, cfg.StateDir, "hermes")
	s := &server{cfg: cfg, sched: NewScheduler(cfg)}
	ts := httptest.NewServer(s.routes())
	t.Cleanup(ts.Close)

	// Create a run; it acquires + holds the only slot (status stays "running").
	m.initialStatus = "running"
	resp, body := apiDo(t, http.MethodPost, ts.URL+"/v1/runs", adminToken,
		[]byte(`{"model":"hermes","input":"x"}`), nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("create = %d %s", resp.StatusCode, body)
	}
	var created struct {
		RunID string `json:"run_id"`
	}
	_ = json.Unmarshal(body, &created)

	// A competing chat must be rejected while the run holds the slot.
	resp, _ = apiDo(t, http.MethodPost, ts.URL+"/v1/chat/completions", adminToken,
		[]byte(`{"model":"hermes","messages":[{"role":"user","content":"hi"}]}`), nil)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("competing chat while run active = %d, want 429", resp.StatusCode)
	}

	// Finish the run; the supervisor should release the slot shortly after.
	m.set(created.RunID, "completed")

	deadline := time.Now().Add(3 * time.Second)
	var last int
	for time.Now().Before(deadline) {
		resp, _ = apiDo(t, http.MethodPost, ts.URL+"/v1/chat/completions", adminToken,
			[]byte(`{"model":"hermes","messages":[{"role":"user","content":"hi"}]}`), nil)
		last = resp.StatusCode
		if last == http.StatusOK {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if last != http.StatusOK {
		t.Fatalf("chat after run terminal = %d, want 200 (slot never released)", last)
	}
	// Registry entry is dropped once the supervisor exits.
	deadline = time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := s.runs.get(created.RunID); !ok {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("registry entry for %s not cleaned up", created.RunID)
}
