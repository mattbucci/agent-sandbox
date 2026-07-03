package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// vmRequest is one request recorded by the fake VM.
type vmRequest struct {
	header http.Header
	body   []byte
}

// fakeVM is an httptest server standing in for the in-VM gateway server; it
// records every request before delegating to the test's handler.
type fakeVM struct {
	srv  *httptest.Server
	mu   sync.Mutex
	reqs []vmRequest
}

func newFakeVM(t *testing.T, handler http.HandlerFunc) *fakeVM {
	t.Helper()
	fv := &fakeVM{}
	fv.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		fv.mu.Lock()
		fv.reqs = append(fv.reqs, vmRequest{header: r.Header.Clone(), body: body})
		fv.mu.Unlock()
		handler(w, r)
	}))
	t.Cleanup(fv.srv.Close)
	return fv
}

// port returns the fake VM's listen port (used as vm_gateway_port).
func (fv *fakeVM) port(t *testing.T) int {
	t.Helper()
	u, err := url.Parse(fv.srv.URL)
	if err != nil {
		t.Fatalf("parse fake VM url: %v", err)
	}
	p, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("fake VM port: %v", err)
	}
	return p
}

func (fv *fakeVM) requests() []vmRequest {
	fv.mu.Lock()
	defer fv.mu.Unlock()
	out := make([]vmRequest, len(fv.reqs))
	copy(out, fv.reqs)
	return out
}

// dispatchTestConfig builds a defaulted config pointing at a temp state dir
// and the fake VM's port.
func dispatchTestConfig(t *testing.T, vmPort int) *Config {
	t.Helper()
	cfg := &Config{
		StateDir:      t.TempDir(),
		VMGatewayPort: vmPort,
		Agents:        map[string]AgentConfig{"feature-dev": {}},
	}
	cfg.applyDefaults()
	return cfg
}

// writeVMInfo makes findVMForAgent resolve 127.0.0.1 with a live pid.
func writeVMInfo(t *testing.T, stateDir, agent string) {
	t.Helper()
	dir := filepath.Join(stateDir, "vms", agent+"-test0")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir vm dir: %v", err)
	}
	info := VMInfo{
		InstanceID:     agent + "-test0",
		AgentType:      agent,
		VMIP:           "127.0.0.1",
		FirecrackerPID: os.Getpid(),
	}
	data, err := json.Marshal(info)
	if err != nil {
		t.Fatalf("marshal vm info: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "info.json"), data, 0o644); err != nil {
		t.Fatalf("write vm info: %v", err)
	}
}

// newDispatchStore builds a real-clock store (the dispatcher uses time.Now).
func newDispatchStore(t *testing.T, cfg *Config) *TaskStore {
	t.Helper()
	st, err := NewTaskStore(cfg)
	if err != nil {
		t.Fatalf("NewTaskStore: %v", err)
	}
	return st
}

// writeSSE writes SSE data lines per the documented in-VM contract.
func writeSSE(w http.ResponseWriter, payloads ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)
	for _, p := range payloads {
		fmt.Fprintf(w, "data: %s\n\n", p)
		if fl != nil {
			fl.Flush()
		}
	}
}

// happySSE is the standard successful stream: content, finish+usage, [DONE].
func happySSE(w http.ResponseWriter, _ *http.Request) {
	writeSSE(w,
		`{"id":"chatcmpl-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]}`,
		`{"id":"chatcmpl-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":" world"},"finish_reason":null}]}`,
		`{"id":"chatcmpl-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":34}}`,
		`[DONE]`,
	)
}

const testMessages = `{"messages":[{"role":"user","content":"hi"}]}`

// runOnce claims the runnable task and executes one attempt synchronously.
func runOnce(t *testing.T, cfg *Config, st *TaskStore, agent string) Task {
	t.Helper()
	claimed := mustClaim(t, st, agent)
	d := &dispatcher{cfg: cfg, store: st, agent: agent}
	d.runTask(claimed)
	got, ok := st.Get(claimed.ID)
	if !ok {
		t.Fatalf("task %s vanished", claimed.ID)
	}
	return got
}

// waitForTaskState polls until the task reaches want (dispatcher-loop tests).
func waitForTaskState(t *testing.T, st *TaskStore, id string, want TaskState) Task {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		task, ok := st.Get(id)
		if ok && task.State == want {
			return task
		}
		time.Sleep(10 * time.Millisecond)
	}
	task, _ := st.Get(id)
	t.Fatalf("task %s state=%s, want %s", id, task.State, want)
	return Task{}
}

// TestBuildVMRequest asserts the shared request builder produces the exact
// downstream wire shape: URL, model rewrite, bearer, session + traceparent.
func TestBuildVMRequest(t *testing.T) {
	cfg := &Config{
		VMGatewayPort: 8642,
		Agents: map[string]AgentConfig{
			"hermes": {APIServerKey: "hga-PLACEHOLDER-TEST-KEY-1", Model: "gemma"},
		},
	}
	vm := VMInfo{VMIP: "10.99.0.2"}
	body := []byte(`{"model":"hermes","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	tp := "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01"
	req, err := buildVMRequest(context.Background(), cfg, vm, "hermes", body, "sid-1", "skey-1", tp)
	if err != nil {
		t.Fatalf("buildVMRequest: %v", err)
	}
	if req.URL.String() != "http://10.99.0.2:8642/v1/chat/completions" {
		t.Fatalf("url = %s", req.URL)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer hga-PLACEHOLDER-TEST-KEY-1" {
		t.Fatalf("auth = %q", got)
	}
	if req.Header.Get("X-Hermes-Session-Id") != "sid-1" || req.Header.Get("X-Hermes-Session-Key") != "skey-1" {
		t.Fatalf("session headers = %v", req.Header)
	}
	if got := req.Header.Get("Traceparent"); got != tp {
		t.Fatalf("traceparent = %q", got)
	}
	if req.Header.Get("Content-Type") != "application/json" || req.Header.Get("Accept") != "text/event-stream" {
		t.Fatalf("content headers = %v", req.Header)
	}
	sent, _ := io.ReadAll(req.Body)
	var m map[string]any
	if err := json.Unmarshal(sent, &m); err != nil {
		t.Fatalf("body: %v", err)
	}
	if m["model"] != "gemma" || m["stream"] != true {
		t.Fatalf("body = %s", sent)
	}
}

// TestBuildVMRequestNoRewriteNoKey: without Model/APIServerKey the body bytes
// pass through verbatim and no Authorization header is attached.
func TestBuildVMRequestNoRewriteNoKey(t *testing.T) {
	cfg := &Config{VMGatewayPort: 8642, Agents: map[string]AgentConfig{"feature-dev": {}}}
	vm := VMInfo{VMIP: "10.99.0.3"}
	body := []byte(`{"model":"feature-dev","messages":[]}`)
	req, err := buildVMRequest(context.Background(), cfg, vm, "feature-dev", body, "", "", "")
	if err != nil {
		t.Fatalf("buildVMRequest: %v", err)
	}
	sent, _ := io.ReadAll(req.Body)
	if string(sent) != string(body) {
		t.Fatalf("body rewritten: %s", sent)
	}
	for _, h := range []string{"Authorization", "X-Hermes-Session-Id", "X-Hermes-Session-Key", "Traceparent"} {
		if req.Header.Get(h) != "" {
			t.Fatalf("unexpected header %s = %q", h, req.Header.Get(h))
		}
	}
}

// TestRunTaskHappyPath: SSE stream with usage; task succeeds, the spool and
// inline result carry the content, the attempt record closes, and the fake
// VM saw the rewritten model, forced stream flag and the task session id.
func TestRunTaskHappyPath(t *testing.T) {
	fv := newFakeVM(t, happySSE)
	cfg := dispatchTestConfig(t, fv.port(t))
	cfg.Agents["feature-dev"] = AgentConfig{Model: "gemma"}
	st := newDispatchStore(t, cfg)
	writeVMInfo(t, cfg.StateDir, "feature-dev")

	task := mustSubmit(t, st, SubmitSpec{Agent: "feature-dev", Request: json.RawMessage(testMessages)})
	got := runOnce(t, cfg, st, "feature-dev")

	if got.State != TaskSucceeded {
		t.Fatalf("state = %s (err %+v)", got.State, got.Error)
	}
	if got.Result == nil || got.Result.Content != "Hello world" || got.Result.ContentTruncated ||
		got.Result.FinishReason != "stop" || got.Result.OutputBytes != int64(len("Hello world")) {
		t.Fatalf("result = %+v", got.Result)
	}
	if string(got.Result.Usage) != `{"prompt_tokens":12,"completion_tokens":34}` {
		t.Fatalf("usage = %s", got.Result.Usage)
	}
	if got.FinishedAt == nil || got.Error != nil {
		t.Fatalf("finished_at/error wrong: %+v", got)
	}
	spool, err := os.ReadFile(st.OutputPath(task.ID))
	if err != nil || string(spool) != "Hello world" {
		t.Fatalf("spool = %q err=%v", spool, err)
	}
	if n := len(got.AttemptHistory); n != 1 {
		t.Fatalf("attempt_history len = %d", n)
	}
	ah := got.AttemptHistory[0]
	if ah.Outcome != attemptSucceeded || ah.EndedAt == nil || ah.VMIP != "127.0.0.1" || ah.OutputBytes != 11 {
		t.Fatalf("attempt record = %+v", ah)
	}

	reqs := fv.requests()
	if len(reqs) != 1 {
		t.Fatalf("fake VM saw %d requests", len(reqs))
	}
	if sid := reqs[0].header.Get("X-Hermes-Session-Id"); sid != task.SessionID || !strings.HasPrefix(sid, "task:") {
		t.Fatalf("session id = %q, want %q", sid, task.SessionID)
	}
	var sent map[string]any
	if err := json.Unmarshal(reqs[0].body, &sent); err != nil {
		t.Fatalf("sent body: %v", err)
	}
	if sent["model"] != "gemma" || sent["stream"] != true {
		t.Fatalf("sent body = %s", reqs[0].body)
	}
}

// TestRunTaskNonSSEFallback: a JSON (stream-ignoring) backend still succeeds.
func TestRunTaskNonSSEFallback(t *testing.T) {
	fv := newFakeVM(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"chatcmpl-2","object":"chat.completion",`+
			`"choices":[{"index":0,"message":{"role":"assistant","content":"full answer"},"finish_reason":"stop"}],`+
			`"usage":{"prompt_tokens":1,"completion_tokens":2}}`)
	})
	cfg := dispatchTestConfig(t, fv.port(t))
	st := newDispatchStore(t, cfg)
	writeVMInfo(t, cfg.StateDir, "feature-dev")

	mustSubmit(t, st, SubmitSpec{Agent: "feature-dev", Request: json.RawMessage(testMessages)})
	got := runOnce(t, cfg, st, "feature-dev")
	if got.State != TaskSucceeded || got.Result == nil || got.Result.Content != "full answer" {
		t.Fatalf("state=%s result=%+v err=%+v", got.State, got.Result, got.Error)
	}
}

// TestRunTaskMidStreamDeath: partial output then EOF without [DONE]; with
// retry_on_partial=false the task fails with downstream_error.
func TestRunTaskMidStreamDeath(t *testing.T) {
	fv := newFakeVM(t, func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, `{"choices":[{"delta":{"content":"partial"},"finish_reason":null}]}`)
		// handler returns: connection closes without [DONE]
	})
	cfg := dispatchTestConfig(t, fv.port(t))
	st := newDispatchStore(t, cfg)
	writeVMInfo(t, cfg.StateDir, "feature-dev")

	task := mustSubmit(t, st, SubmitSpec{Agent: "feature-dev", Request: json.RawMessage(testMessages)})
	got := runOnce(t, cfg, st, "feature-dev")
	if got.State != TaskFailed || got.Error == nil || got.Error.Kind != ErrKindDownstream {
		t.Fatalf("state=%s err=%+v", got.State, got.Error)
	}
	if got.AttemptHistory[0].Outcome != attemptError {
		t.Fatalf("attempt outcome = %q", got.AttemptHistory[0].Outcome)
	}
	spool, _ := os.ReadFile(st.OutputPath(task.ID))
	if string(spool) != "partial" {
		t.Fatalf("spool = %q", spool)
	}
}

// TestRunTaskZeroOutputRetries: an error with no output is retriable while
// attempts remain — the task requeues with backoff.
func TestRunTaskZeroOutputRetries(t *testing.T) {
	fv := newFakeVM(t, func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w) // headers only, then EOF
	})
	cfg := dispatchTestConfig(t, fv.port(t))
	st := newDispatchStore(t, cfg)
	writeVMInfo(t, cfg.StateDir, "feature-dev")

	mustSubmit(t, st, SubmitSpec{Agent: "feature-dev", Request: json.RawMessage(testMessages)})
	got := runOnce(t, cfg, st, "feature-dev")
	if got.State != TaskPending || got.Attempts != 1 || got.NotBefore == nil {
		t.Fatalf("state=%s attempts=%d not_before=%v", got.State, got.Attempts, got.NotBefore)
	}
	if got.Error == nil || got.Error.Kind != ErrKindDownstream {
		t.Fatalf("err = %+v", got.Error)
	}
}

// TestRunTaskNon2xxRefundsAttempt: a non-2xx before the first body byte is
// VM-unavailable — the attempt is refunded and the task requeues.
func TestRunTaskNon2xxRefundsAttempt(t *testing.T) {
	fv := newFakeVM(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	cfg := dispatchTestConfig(t, fv.port(t))
	st := newDispatchStore(t, cfg)
	writeVMInfo(t, cfg.StateDir, "feature-dev")

	mustSubmit(t, st, SubmitSpec{Agent: "feature-dev", Request: json.RawMessage(testMessages)})
	got := runOnce(t, cfg, st, "feature-dev")
	if got.State != TaskPending || got.Attempts != 0 || got.VMUnavailableCount != 1 || got.NotBefore == nil {
		t.Fatalf("state=%s attempts=%d vm_unavailable=%d not_before=%v",
			got.State, got.Attempts, got.VMUnavailableCount, got.NotBefore)
	}
	if got.Error == nil || got.Error.Kind != ErrKindVMUnreachable || !strings.Contains(got.Error.Message, "500") {
		t.Fatalf("err = %+v", got.Error)
	}
	if got.AttemptHistory[0].Outcome != attemptVMUnreachable {
		t.Fatalf("attempt outcome = %q", got.AttemptHistory[0].Outcome)
	}
}

// TestRunTaskNoVMRefundsAttempt: no live VM resolves — same refund path.
func TestRunTaskNoVMRefundsAttempt(t *testing.T) {
	cfg := dispatchTestConfig(t, 1) // port unused: no VM info written
	st := newDispatchStore(t, cfg)

	mustSubmit(t, st, SubmitSpec{Agent: "feature-dev", Request: json.RawMessage(testMessages)})
	got := runOnce(t, cfg, st, "feature-dev")
	if got.State != TaskPending || got.Attempts != 0 || got.VMUnavailableCount != 1 {
		t.Fatalf("state=%s attempts=%d vm_unavailable=%d", got.State, got.Attempts, got.VMUnavailableCount)
	}
	if got.Error == nil || got.Error.Kind != ErrKindVMUnreachable {
		t.Fatalf("err = %+v", got.Error)
	}
}

// TestRunTaskIdleWatchdog: a VM that never sends bytes trips the (shortened)
// idle watchdog; with max_attempts=1 the task fails with kind idle.
func TestRunTaskIdleWatchdog(t *testing.T) {
	fv := newFakeVM(t, func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w) // headers, then silence
		<-r.Context().Done()
	})
	cfg := dispatchTestConfig(t, fv.port(t))
	st := newDispatchStore(t, cfg)
	writeVMInfo(t, cfg.StateDir, "feature-dev")

	mustSubmit(t, st, SubmitSpec{
		Agent:        "feature-dev",
		Request:      json.RawMessage(testMessages),
		IdleTimeoutS: 1,
		MaxAttempts:  1,
	})
	got := runOnce(t, cfg, st, "feature-dev")
	if got.State != TaskFailed || got.Error == nil || got.Error.Kind != ErrKindIdle {
		t.Fatalf("state=%s err=%+v", got.State, got.Error)
	}
}

// TestRunTaskSlowDripSurvivesWatchdog: chunks arriving under the idle limit
// keep rearming the watchdog and the task succeeds.
func TestRunTaskSlowDripSurvivesWatchdog(t *testing.T) {
	fv := newFakeVM(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		for i := 0; i < 7; i++ {
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"x\"},\"finish_reason\":null}]}\n\n")
			fl.Flush()
			time.Sleep(200 * time.Millisecond)
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
		fl.Flush()
	})
	cfg := dispatchTestConfig(t, fv.port(t))
	st := newDispatchStore(t, cfg)
	writeVMInfo(t, cfg.StateDir, "feature-dev")

	mustSubmit(t, st, SubmitSpec{
		Agent:        "feature-dev",
		Request:      json.RawMessage(testMessages),
		IdleTimeoutS: 1,
	})
	got := runOnce(t, cfg, st, "feature-dev")
	if got.State != TaskSucceeded || got.Result == nil || got.Result.Content != "xxxxxxx" {
		t.Fatalf("state=%s result=%+v err=%+v", got.State, got.Result, got.Error)
	}
}

// TestRunTaskTimeoutRequeues: the per-attempt timeout fires with zero output
// and attempts remaining — kind timeout, requeued with backoff.
func TestRunTaskTimeoutRequeues(t *testing.T) {
	fv := newFakeVM(t, func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w) // headers only
		<-r.Context().Done()
	})
	cfg := dispatchTestConfig(t, fv.port(t))
	st := newDispatchStore(t, cfg)
	writeVMInfo(t, cfg.StateDir, "feature-dev")

	mustSubmit(t, st, SubmitSpec{
		Agent:    "feature-dev",
		Request:  json.RawMessage(testMessages),
		TimeoutS: 1,
	})
	got := runOnce(t, cfg, st, "feature-dev")
	if got.State != TaskPending || got.Attempts != 1 || got.NotBefore == nil {
		t.Fatalf("state=%s attempts=%d not_before=%v", got.State, got.Attempts, got.NotBefore)
	}
	if got.Error == nil || got.Error.Kind != ErrKindTimeout {
		t.Fatalf("err = %+v", got.Error)
	}
}

// TestRunTaskCancelMidStream: Cancel on a running task tears down the VM
// stream (runner ctx cancel) and the task lands in cancelled.
func TestRunTaskCancelMidStream(t *testing.T) {
	started := make(chan struct{})
	fv := newFakeVM(t, func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, `{"choices":[{"delta":{"content":"before cancel"},"finish_reason":null}]}`)
		close(started)
		<-r.Context().Done()
	})
	cfg := dispatchTestConfig(t, fv.port(t))
	st := newDispatchStore(t, cfg)
	writeVMInfo(t, cfg.StateDir, "feature-dev")

	task := mustSubmit(t, st, SubmitSpec{Agent: "feature-dev", Request: json.RawMessage(testMessages)})
	claimed := mustClaim(t, st, "feature-dev")
	d := &dispatcher{cfg: cfg, store: st, agent: "feature-dev"}
	done := make(chan struct{})
	go func() {
		d.runTask(claimed)
		close(done)
	}()
	<-started
	if _, _, err := st.Cancel(task.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("runner did not finish after cancel")
	}
	got, _ := st.Get(task.ID)
	if got.State != TaskCancelled || !got.CancelRequested || got.Error == nil || got.Error.Kind != ErrKindCancelled {
		t.Fatalf("state=%s cancel_requested=%v err=%+v", got.State, got.CancelRequested, got.Error)
	}
}

// TestDispatcherPriorityOrder: an end-to-end dispatcher loop at concurrency 1
// runs queued tasks in priority order 10, 5, 0.
func TestDispatcherPriorityOrder(t *testing.T) {
	fv := newFakeVM(t, happySSE)
	cfg := dispatchTestConfig(t, fv.port(t))
	st := newDispatchStore(t, cfg)
	writeVMInfo(t, cfg.StateDir, "feature-dev")
	sched := NewScheduler(cfg)

	var ids []string
	for _, p := range []int{0, 10, 5} {
		task := mustSubmit(t, st, SubmitSpec{
			Agent:     "feature-dev",
			Request:   json.RawMessage(testMessages),
			Priority:  p,
			SessionID: fmt.Sprintf("task-prio-%d", p),
		})
		ids = append(ids, task.ID)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	startDispatchers(ctx, cfg, sched, st, &wg)
	for _, id := range ids {
		waitForTaskState(t, st, id, TaskSucceeded)
	}
	cancel()
	wg.Wait()

	var order []string
	for _, r := range fv.requests() {
		order = append(order, r.header.Get("X-Hermes-Session-Id"))
	}
	want := []string{"task-prio-10", "task-prio-5", "task-prio-0"}
	if len(order) != 3 || order[0] != want[0] || order[1] != want[1] || order[2] != want[2] {
		t.Fatalf("run order = %v, want %v", order, want)
	}
}

// TestDispatcherShutdownInterrupts: shutting down mid-attempt (ctx cancel +
// CancelAll with cause shutdown) requeues a zero-output attempt as pending
// with kind interrupted — the same outcome class a kill -9 + recovery yields.
func TestDispatcherShutdownInterrupts(t *testing.T) {
	started := make(chan struct{})
	fv := newFakeVM(t, func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w) // headers only
		close(started)
		<-r.Context().Done()
	})
	cfg := dispatchTestConfig(t, fv.port(t))
	st := newDispatchStore(t, cfg)
	writeVMInfo(t, cfg.StateDir, "feature-dev")
	sched := NewScheduler(cfg)

	task := mustSubmit(t, st, SubmitSpec{Agent: "feature-dev", Request: json.RawMessage(testMessages)})

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	startDispatchers(ctx, cfg, sched, st, &wg)
	<-started

	// Shutdown sequence per main.go: scheduler, dispatcher ctx, runners.
	sched.Close()
	cancel()
	st.CancelAll(errShutdown)
	wg.Wait()

	got, _ := st.Get(task.ID)
	if got.State != TaskPending || got.Attempts != 1 || got.NotBefore == nil {
		t.Fatalf("state=%s attempts=%d not_before=%v", got.State, got.Attempts, got.NotBefore)
	}
	if got.Error == nil || got.Error.Kind != ErrKindInterrupted {
		t.Fatalf("err = %+v", got.Error)
	}
	if got.AttemptHistory[0].Outcome != attemptInterrupted {
		t.Fatalf("attempt outcome = %q", got.AttemptHistory[0].Outcome)
	}
}

// TestDispatcherRunsNotBeforeLater: the dispatcher's NextWake timer picks up
// a task whose not_before is slightly in the future without a fresh poke.
func TestDispatcherRunsNotBeforeLater(t *testing.T) {
	fv := newFakeVM(t, happySSE)
	cfg := dispatchTestConfig(t, fv.port(t))
	st := newDispatchStore(t, cfg)
	writeVMInfo(t, cfg.StateDir, "feature-dev")
	sched := NewScheduler(cfg)

	nb := time.Now().Add(300 * time.Millisecond)
	task := mustSubmit(t, st, SubmitSpec{
		Agent:     "feature-dev",
		Request:   json.RawMessage(testMessages),
		NotBefore: &nb,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	startDispatchers(ctx, cfg, sched, st, &wg)
	got := waitForTaskState(t, st, task.ID, TaskSucceeded)
	cancel()
	wg.Wait()
	if got.StartedAt == nil || got.StartedAt.Before(nb.Add(-50*time.Millisecond)) {
		t.Fatalf("started_at = %v, want >= %v", got.StartedAt, nb)
	}
}
