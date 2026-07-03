package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// integrationCollector captures every OTLP POST as decoded spans.
type integrationCollector struct {
	mu    sync.Mutex
	spans []otlpSpan
}

func (c *integrationCollector) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req otlpExportRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		c.mu.Lock()
		for _, rs := range req.ResourceSpans {
			for _, ss := range rs.ScopeSpans {
				c.spans = append(c.spans, ss.Spans...)
			}
		}
		c.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}
}

func (c *integrationCollector) all() []otlpSpan {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]otlpSpan, len(c.spans))
	copy(out, c.spans)
	return out
}

// findSpan returns the first collected span matching name and pred.
func (c *integrationCollector) findSpan(name string, pred func(otlpSpan) bool) (otlpSpan, bool) {
	for _, sp := range c.all() {
		if sp.Name == name && (pred == nil || pred(sp)) {
			return sp, true
		}
	}
	return otlpSpan{}, false
}

// waitForSpan polls until a span with name (and pred) shows up.
func (c *integrationCollector) waitForSpan(t *testing.T, name string, pred func(otlpSpan) bool) otlpSpan {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if sp, ok := c.findSpan(name, pred); ok {
			return sp
		}
		time.Sleep(10 * time.Millisecond)
	}
	names := make([]string, 0)
	for _, sp := range c.all() {
		names = append(names, sp.Name)
	}
	t.Fatalf("span %q never exported (have %v)", name, names)
	return otlpSpan{}
}

// sseContent extracts the concatenated delta content from a raw SSE body.
func sseContent(t *testing.T, body string) string {
	t.Helper()
	var out strings.Builder
	sc := bufio.NewScanner(strings.NewReader(body))
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(line[len("data:"):])
		if payload == "[DONE]" {
			break
		}
		var chunk sseChunk
		if json.Unmarshal([]byte(payload), &chunk) != nil {
			continue
		}
		for _, c := range chunk.Choices {
			out.WriteString(c.Delta.Content)
		}
	}
	return out.String()
}

// TestIntegrationTraceMetricsAndTask is the single-process integration test
// (plan §h): a fake VM echoing the received traceparent + a fake collector
// capturing OTLP posts. It drives one chat and one task and asserts:
//   - one trace spans SERVER -> CLIENT -> VM (the VM saw the CLIENT span's
//     context; the SERVER span parents onto the inbound traceparent even when
//     the inbound sampled flag is 0)
//   - /metrics counted the chat
//   - the task attempt is a new root linked to the submit context
//   - the task record transitioned on disk (schema 1, succeeded)
func TestIntegrationTraceMetricsAndTask(t *testing.T) {
	collector := &integrationCollector{}
	collectorSrv := httptest.NewServer(collector.handler())
	defer collectorSrv.Close()

	// Fake VM: echoes the traceparent it received as the assistant content.
	fv := newFakeVM(t, func(w http.ResponseWriter, r *http.Request) {
		tp := r.Header.Get("Traceparent")
		writeSSE(w,
			fmt.Sprintf(`{"choices":[{"delta":{"content":"%s"},"finish_reason":null}]}`, tp),
			`{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":2}}`,
			`[DONE]`,
		)
	})

	cfg := apiTestConfig(t)
	cfg.VMGatewayPort = fv.port(t)
	otlp := collectorSrv.URL
	cfg.Observability.OTLPEndpoint = &otlp
	writeVMInfo(t, cfg.StateDir, "feature-dev")

	// Wire the runtime exactly as main() does.
	store, err := NewTaskStore(cfg)
	if err != nil {
		t.Fatalf("NewTaskStore: %v", err)
	}
	if err := store.RecoverOnBoot(); err != nil {
		t.Fatalf("RecoverOnBoot: %v", err)
	}
	sched := NewScheduler(cfg)
	mx := newMetrics(cfg, "integration", sched, store)
	tracer := newTracer(cfg.OTLPEndpoint(), cfg.SampleRatio(), "integration")
	tracer.flushEvery = 25 * time.Millisecond // fast flushes for the test
	tracer.onBatch = mx.OnOTLPBatch
	tracer.onDrop = mx.OnOTLPDrop
	tracer.StartExporter()
	defer tracer.Close()
	hist := newHistory()
	sched.onEvent = mx.HandleSchedEvent
	store.onTransition = func(ev TaskEvent) {
		mx.HandleTaskEvent(ev)
		logTaskEvent(ev)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	defer func() {
		cancel()
		wg.Wait()
	}()
	startInstrumentedDispatchers(ctx, cfg, sched, store, &wg, tracer, mx)

	s := &server{cfg: cfg, sched: sched, store: store, mx: mx, tracer: tracer, hist: hist, startedAt: time.Now()}
	ts := httptest.NewServer(s.instrument(s.routes()))
	defer ts.Close()

	// --- 1. Sync chat with an inbound traceparent (sampled=0: overridden) ---
	const (
		inboundTID = "4bf92f3577b34da6a3ce929d0e0e4736"
		inboundSID = "00f067aa0ba902b7"
	)
	resp, body := apiDo(t, http.MethodPost, ts.URL+"/v1/chat/completions", adminToken,
		[]byte(`{"model":"feature-dev","messages":[{"role":"user","content":"hi"}],"stream":true}`),
		map[string]string{"Traceparent": "00-" + inboundTID + "-" + inboundSID + "-00"})
	if resp.StatusCode != 200 {
		t.Fatalf("chat status = %d (%s)", resp.StatusCode, body)
	}
	vmSawChat, ok := parseTraceparent(sseContent(t, string(body)))
	if !ok {
		t.Fatalf("VM did not receive a valid traceparent; echoed %q", sseContent(t, string(body)))
	}
	if vmSawChat.TraceID != inboundTID {
		t.Fatalf("VM trace id = %s, want inbound %s (one trace end-to-end)", vmSawChat.TraceID, inboundTID)
	}
	if !vmSawChat.Sampled {
		t.Fatalf("inbound sampled=0 was not overridden to sampled")
	}

	// SERVER span: child of the inbound context, same trace.
	serverSpan := collector.waitForSpan(t, "POST /v1/chat/completions", func(sp otlpSpan) bool {
		return sp.TraceID == inboundTID
	})
	if serverSpan.ParentSpanID != inboundSID {
		t.Fatalf("server span parent = %q, want inbound %q", serverSpan.ParentSpanID, inboundSID)
	}
	if serverSpan.Kind != int(KindServer) {
		t.Fatalf("server span kind = %d", serverSpan.Kind)
	}
	// CLIENT span: child of the SERVER span; its span id is exactly what the
	// VM saw in the traceparent header.
	clientSpan := collector.waitForSpan(t, "proxy /v1/chat/completions", func(sp otlpSpan) bool {
		return sp.TraceID == inboundTID
	})
	if clientSpan.ParentSpanID != serverSpan.SpanID {
		t.Fatalf("client span parent = %q, want server span %q", clientSpan.ParentSpanID, serverSpan.SpanID)
	}
	if clientSpan.Kind != int(KindClient) {
		t.Fatalf("client span kind = %d", clientSpan.Kind)
	}
	if vmSawChat.SpanID != clientSpan.SpanID {
		t.Fatalf("VM saw span id %s, want CLIENT span id %s", vmSawChat.SpanID, clientSpan.SpanID)
	}

	// --- 2. Async task: submit over HTTP, dispatcher runs it ---
	resp, body = apiDo(t, http.MethodPost, ts.URL+"/v1/tasks", adminToken,
		[]byte(`{"agent":"feature-dev","input":"do the thing"}`), nil)
	if resp.StatusCode != 201 {
		t.Fatalf("submit status = %d (%s)", resp.StatusCode, body)
	}
	var task Task
	if err := json.Unmarshal(body, &task); err != nil {
		t.Fatalf("submit body: %v", err)
	}
	if task.SubmitTrace == nil || len(task.SubmitTrace.TraceID) != 32 || len(task.SubmitTrace.SpanID) != 16 {
		t.Fatalf("submit_trace not persisted: %+v", task.SubmitTrace)
	}

	done := waitForTaskState(t, store, task.ID, TaskSucceeded)
	if len(done.TraceIDs) != 1 {
		t.Fatalf("trace_ids = %v, want one attempt trace", done.TraceIDs)
	}
	attemptTID := done.TraceIDs[0]

	// The VM's echoed traceparent (= the task result content) belongs to the
	// attempt trace, carried by the attempt's CLIENT span.
	if done.Result == nil {
		t.Fatalf("no result on succeeded task")
	}
	vmSawTask, ok := parseTraceparent(done.Result.Content)
	if !ok {
		t.Fatalf("task attempt sent no valid traceparent; VM saw %q", done.Result.Content)
	}
	if vmSawTask.TraceID != attemptTID {
		t.Fatalf("VM attempt trace id = %s, want %s", vmSawTask.TraceID, attemptTID)
	}

	// task.attempt: a NEW ROOT (no parent) in its own trace, span-linked to
	// the persisted submit context.
	attemptSpan := collector.waitForSpan(t, "task.attempt", func(sp otlpSpan) bool {
		return sp.TraceID == attemptTID
	})
	if attemptSpan.ParentSpanID != "" {
		t.Fatalf("task.attempt has a parent (%q); attempts must be new roots", attemptSpan.ParentSpanID)
	}
	if len(attemptSpan.Links) != 1 ||
		attemptSpan.Links[0].TraceID != task.SubmitTrace.TraceID ||
		attemptSpan.Links[0].SpanID != task.SubmitTrace.SpanID {
		t.Fatalf("attempt links = %+v, want submit_trace %+v", attemptSpan.Links, task.SubmitTrace)
	}
	taskClient := collector.waitForSpan(t, "proxy /v1/chat/completions", func(sp otlpSpan) bool {
		return sp.TraceID == attemptTID
	})
	if taskClient.ParentSpanID != attemptSpan.SpanID {
		t.Fatalf("attempt client parent = %q, want %q", taskClient.ParentSpanID, attemptSpan.SpanID)
	}
	if vmSawTask.SpanID != taskClient.SpanID {
		t.Fatalf("VM saw attempt span id %s, want %s", vmSawTask.SpanID, taskClient.SpanID)
	}
	// The submit request produced its own SERVER span in the submit trace.
	collector.waitForSpan(t, "POST /v1/tasks", func(sp otlpSpan) bool {
		return sp.TraceID == task.SubmitTrace.TraceID && sp.SpanID == task.SubmitTrace.SpanID
	})

	// --- 3. /metrics counted all of it (loopback scrape, no auth) ---
	resp, body = apiDo(t, http.MethodGet, ts.URL+"/metrics", "", nil, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("/metrics = %d", resp.StatusCode)
	}
	metricsText := string(body)
	for _, line := range []string{
		`hermes_gateway_http_requests_total{path="/v1/chat/completions",method="POST",code="200"} 1`,
		`hermes_gateway_http_requests_total{path="/v1/tasks",method="POST",code="201"} 1`,
		`hermes_gateway_task_transitions_total{agent="feature-dev",to_state="succeeded"} 1`,
		`hermes_gateway_task_transitions_total{agent="feature-dev",to_state="running"} 1`,
		`hermes_gateway_sched_admitted_total{agent="feature-dev",class="sync"} 1`,
		`hermes_gateway_sched_admitted_total{agent="feature-dev",class="task"} 1`,
		`hermes_gateway_vm_up{agent="feature-dev"} 1`,
	} {
		if !strings.Contains(metricsText, line+"\n") {
			t.Fatalf("/metrics missing %q", line)
		}
	}
	if !strings.Contains(metricsText, `hermes_gateway_stream_bytes_total{agent="feature-dev"} `) ||
		strings.Contains(metricsText, `hermes_gateway_stream_bytes_total{agent="feature-dev"} 0`) {
		t.Fatalf("/metrics stream bytes not counted")
	}

	// --- 4. The task record transitioned on disk ---
	raw, err := os.ReadFile(filepath.Join(cfg.Tasks.Dir, task.ID+".json"))
	if err != nil {
		t.Fatalf("read task record: %v", err)
	}
	disk := string(raw)
	for _, frag := range []string{`"schema":1`, `"state":"succeeded"`, `"id":"` + task.ID + `"`,
		`"trace_ids":["` + attemptTID + `"]`} {
		if !strings.Contains(disk, frag) {
			t.Fatalf("disk record missing %s:\n%s", frag, disk)
		}
	}

	// History observed the traffic (agent ring exists with counts).
	hsnap := hist.Snapshot()
	if hs, ok := hsnap.Series["feature-dev"]; !ok {
		t.Fatalf("history has no feature-dev series")
	} else {
		total := uint32(0)
		for _, n := range hs.Count {
			total += n
		}
		if total == 0 {
			t.Fatalf("history counted nothing for feature-dev")
		}
	}
}
