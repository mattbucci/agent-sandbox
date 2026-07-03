package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestParseTraceparentTable is the W3C parse table: version rules, lengths,
// case, zero ids, future versions, absent header.
func TestParseTraceparentTable(t *testing.T) {
	const (
		tid = "4bf92f3577b34da6a3ce929d0e0e4736"
		sid = "00f067aa0ba902b7"
	)
	cases := []struct {
		name        string
		header      string
		ok          bool
		wantSampled bool
	}{
		{"valid sampled", "00-" + tid + "-" + sid + "-01", true, true},
		{"valid unsampled", "00-" + tid + "-" + sid + "-00", true, false},
		{"future version extra fields", "cc-" + tid + "-" + sid + "-01-extra-stuff", true, true},
		{"version ff", "ff-" + tid + "-" + sid + "-01", false, false},
		{"uppercase version", "0A-" + tid + "-" + sid + "-01", false, false},
		{"uppercase trace id", "00-4BF92F3577B34DA6A3CE929D0E0E4736-" + sid + "-01", false, false},
		{"uppercase span id", "00-" + tid + "-00F067AA0BA902B7-01", false, false},
		{"trace id too short", "00-4bf92f3577b34da6-" + sid + "-01", false, false},
		{"trace id too long", "00-" + tid + "ab-" + sid + "-01", false, false},
		{"span id too short", "00-" + tid + "-00f067aa-01", false, false},
		{"zero trace id", "00-00000000000000000000000000000000-" + sid + "-01", false, false},
		{"zero span id", "00-" + tid + "-0000000000000000-01", false, false},
		{"version 00 with extra fields", "00-" + tid + "-" + sid + "-01-extra", false, false},
		{"non-hex flags", "00-" + tid + "-" + sid + "-zz", false, false},
		{"three parts", "00-" + tid + "-" + sid, false, false},
		{"empty", "", false, false},
		{"garbage", "not-a-traceparent", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sc, ok := parseTraceparent(tc.header)
			if ok != tc.ok {
				t.Fatalf("parse(%q) ok = %v, want %v", tc.header, ok, tc.ok)
			}
			if !ok {
				return
			}
			if sc.TraceID != tid || sc.SpanID != sid || sc.Sampled != tc.wantSampled {
				t.Fatalf("parse(%q) = %+v", tc.header, sc)
			}
		})
	}
}

// TestFormatTraceparent covers both flag values and the parse round trip.
func TestFormatTraceparent(t *testing.T) {
	sc := SpanContext{TraceID: "4bf92f3577b34da6a3ce929d0e0e4736", SpanID: "00f067aa0ba902b7", Sampled: true}
	if got := formatTraceparent(sc); got != "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01" {
		t.Fatalf("formatTraceparent = %q", got)
	}
	sc.Sampled = false
	if got := formatTraceparent(sc); got != "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-00" {
		t.Fatalf("formatTraceparent unsampled = %q", got)
	}
	back, ok := parseTraceparent(formatTraceparent(sc))
	if !ok || back != sc {
		t.Fatalf("round trip = %+v ok=%v", back, ok)
	}
}

// TestSampleTraceID: 1.0 always, 0 never, and ratio 0.5 is deterministic on
// the leading 8 bytes of the trace id.
func TestSampleTraceID(t *testing.T) {
	low := "00000000000000010000000000000000"  // leading uint64 == 1
	high := "ffffffffffffffff0000000000000000" // leading uint64 == max
	for _, id := range []string{low, high} {
		if !sampleTraceID(id, 1.0) {
			t.Fatalf("ratio 1.0 must sample %s", id)
		}
		if sampleTraceID(id, 0) {
			t.Fatalf("ratio 0 must not sample %s", id)
		}
	}
	if !sampleTraceID(low, 0.5) {
		t.Fatalf("low id must be sampled at 0.5")
	}
	if sampleTraceID(high, 0.5) {
		t.Fatalf("high id must not be sampled at 0.5")
	}
	// Deterministic: same input, same answer.
	for i := 0; i < 3; i++ {
		if sampleTraceID(low, 0.5) != true {
			t.Fatalf("sampling is not deterministic")
		}
	}
}

// TestOTLPGoldenJSON locks the exact proto3-JSON encoding: hex id strings,
// decimal-string nanos, intValue as string, parentSpanId omitted for roots,
// status omitted when unset.
func TestOTLPGoldenJSON(t *testing.T) {
	resource := resourceAttrs("host1", "v1", 42)

	full := &Span{
		sc:     SpanContext{TraceID: "0af7651916cd43dd8448eb211c80319c", SpanID: "b7ad6b7169203331", Sampled: true},
		parent: "00f067aa0ba902b7",
		name:   "proxy /v1/chat/completions",
		kind:   KindClient,
		start:  time.Unix(0, 1700000000000000000).UTC(),
		end:    time.Unix(0, 1700000001500000000).UTC(),
	}
	full.SetAttr("hermes.agent", "feature-dev")
	full.SetAttr("hermes.bytes_streamed", int64(42))
	full.SetAttr("hermes.model_rewritten", true)
	full.SetAttr("hermes.ratio", 0.5)
	full.events = append(full.events, spanEvent{time: time.Unix(0, 1700000000600000000).UTC(), name: "first_byte"})
	full.AddLink("11111111111111111111111111111111", "2222222222222222")
	full.SetError("boom")

	root := &Span{
		sc:    SpanContext{TraceID: "0af7651916cd43dd8448eb211c80319c", SpanID: "00f067aa0ba902b7", Sampled: true},
		name:  "task.attempt",
		kind:  KindInternal,
		start: time.Unix(0, 1700000000000000000).UTC(),
		end:   time.Unix(0, 1700000002000000000).UTC(),
	}

	got := string(encodeOTLP(resource, []*Span{full, root}))
	want := `{"resourceSpans":[{"resource":{"attributes":[` +
		`{"key":"service.name","value":{"stringValue":"hermes-gateway"}},` +
		`{"key":"service.version","value":{"stringValue":"v1"}},` +
		`{"key":"service.instance.id","value":{"stringValue":"host1-42"}},` +
		`{"key":"host.name","value":{"stringValue":"host1"}}]},` +
		`"scopeSpans":[{"scope":{"name":"hermes-gateway"},"spans":[` +
		`{"traceId":"0af7651916cd43dd8448eb211c80319c","spanId":"b7ad6b7169203331",` +
		`"parentSpanId":"00f067aa0ba902b7","name":"proxy /v1/chat/completions","kind":3,` +
		`"startTimeUnixNano":"1700000000000000000","endTimeUnixNano":"1700000001500000000",` +
		`"attributes":[` +
		`{"key":"hermes.agent","value":{"stringValue":"feature-dev"}},` +
		`{"key":"hermes.bytes_streamed","value":{"intValue":"42"}},` +
		`{"key":"hermes.model_rewritten","value":{"boolValue":true}},` +
		`{"key":"hermes.ratio","value":{"doubleValue":0.5}}],` +
		`"events":[{"timeUnixNano":"1700000000600000000","name":"first_byte"}],` +
		`"links":[{"traceId":"11111111111111111111111111111111","spanId":"2222222222222222"}],` +
		`"status":{"code":2,"message":"boom"}},` +
		`{"traceId":"0af7651916cd43dd8448eb211c80319c","spanId":"00f067aa0ba902b7",` +
		`"name":"task.attempt","kind":1,` +
		`"startTimeUnixNano":"1700000000000000000","endTimeUnixNano":"1700000002000000000"}]}]}]}`
	if got != want {
		t.Fatalf("golden mismatch:\n got: %s\nwant: %s", got, want)
	}
}

// spanCollector is a fake OTLP collector counting spans per POST.
type spanCollector struct {
	mu      sync.Mutex
	batches [][]otlpSpan
	status  atomic.Int64 // response status; 0 => 200
	delay   time.Duration
}

func (c *spanCollector) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if c.delay > 0 {
			time.Sleep(c.delay)
		}
		var req otlpExportRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		var spans []otlpSpan
		for _, rs := range req.ResourceSpans {
			for _, ss := range rs.ScopeSpans {
				spans = append(spans, ss.Spans...)
			}
		}
		c.mu.Lock()
		c.batches = append(c.batches, spans)
		c.mu.Unlock()
		if st := c.status.Load(); st != 0 {
			w.WriteHeader(int(st))
			return
		}
		w.WriteHeader(http.StatusOK)
	}
}

func (c *spanCollector) snapshot() [][]otlpSpan {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([][]otlpSpan, len(c.batches))
	copy(out, c.batches)
	return out
}

// testTracer builds a tracer against endpoint with test-friendly internals.
func testTracer(endpoint string, batchMax int) *Tracer {
	return &Tracer{
		endpoint:   endpoint,
		ratio:      1,
		client:     &http.Client{Timeout: 500 * time.Millisecond},
		resource:   resourceAttrs("testhost", "test", 1),
		ch:         make(chan *Span, 1024),
		quit:       make(chan struct{}),
		done:       make(chan struct{}),
		batchMax:   batchMax,
		flushEvery: time.Hour, // ticks are injected
		now:        time.Now,
	}
}

// endedSpan builds a minimal finished sampled span.
func endedSpan(tr *Tracer, name string) *Span {
	return &Span{
		t:     tr,
		sc:    SpanContext{TraceID: randHex(16), SpanID: randHex(8), Sampled: true},
		name:  name,
		kind:  KindInternal,
		start: time.Now(),
		end:   time.Now(),
	}
}

// waitForBatches polls the collector until it has n batches.
func waitForBatches(t *testing.T, c *spanCollector, n int) [][]otlpSpan {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if got := c.snapshot(); len(got) >= n {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("collector never received %d batches (have %d)", n, len(c.snapshot()))
	return nil
}

// TestTracerBatching: flush at batchMax spans, on tick, and on shutdown.
func TestTracerBatching(t *testing.T) {
	c := &spanCollector{}
	srv := httptest.NewServer(c.handler())
	defer srv.Close()

	tr := testTracer(srv.URL, 3)
	tick := make(chan time.Time)
	go tr.run(tick)

	// Size-triggered flush at exactly batchMax.
	for i := 0; i < 3; i++ {
		tr.enqueue(endedSpan(tr, "size"))
	}
	got := waitForBatches(t, c, 1)
	if len(got[0]) != 3 {
		t.Fatalf("size batch = %d spans, want 3", len(got[0]))
	}

	// Tick-triggered flush of a partial batch.
	tr.enqueue(endedSpan(tr, "tick"))
	// The span may still be in flight to the exporter goroutine; keep ticking
	// until the batch lands (empty batches are not exported).
	deadline := time.Now().Add(5 * time.Second)
	for len(c.snapshot()) < 2 && time.Now().Before(deadline) {
		select {
		case tick <- time.Now():
		default:
		}
		time.Sleep(2 * time.Millisecond)
	}
	got = waitForBatches(t, c, 2)
	if len(got[1]) != 1 || got[1][0].Name != "tick" {
		t.Fatalf("tick batch = %+v", got[1])
	}

	// Shutdown drains what is already queued.
	tr.enqueue(endedSpan(tr, "final"))
	close(tr.quit)
	select {
	case <-tr.done:
	case <-time.After(5 * time.Second):
		t.Fatalf("exporter never stopped")
	}
	got = c.snapshot()
	last := got[len(got)-1]
	if len(last) != 1 || last[0].Name != "final" {
		t.Fatalf("final batch = %+v", last)
	}
	if tr.lastOK.Load() == 0 {
		t.Fatalf("lastOK never set on successful export")
	}
}

// TestTracerChannelFullDrop: a full span channel drops (and counts) instead
// of blocking; disabled export and unsampled spans never enqueue.
func TestTracerChannelFullDrop(t *testing.T) {
	tr := testTracer("http://127.0.0.1:1", 512)
	tr.ch = make(chan *Span, 2)
	var droppedHook atomic.Int64
	tr.onDrop = func(n int) { droppedHook.Add(int64(n)) }

	for i := 0; i < 5; i++ {
		tr.enqueue(endedSpan(tr, "x"))
	}
	if got := tr.DroppedSpans(); got != 3 {
		t.Fatalf("dropped = %d, want 3", got)
	}
	if droppedHook.Load() != 3 {
		t.Fatalf("onDrop total = %d, want 3", droppedHook.Load())
	}

	// Unsampled span: never enqueued, never dropped.
	sp := endedSpan(tr, "unsampled")
	sp.sc.Sampled = false
	tr.enqueue(sp)
	if got := tr.DroppedSpans(); got != 3 {
		t.Fatalf("unsampled span counted as dropped: %d", got)
	}

	// Disabled export: End() is a no-op enqueue.
	disabled := testTracer("", 512)
	disabled.ch = make(chan *Span, 1)
	disabled.enqueue(endedSpan(disabled, "a"))
	disabled.enqueue(endedSpan(disabled, "b"))
	if len(disabled.ch) != 0 || disabled.DroppedSpans() != 0 {
		t.Fatalf("disabled tracer enqueued spans: len=%d dropped=%d", len(disabled.ch), disabled.DroppedSpans())
	}
}

// TestTracerExportOutcomes: 200 => ok + lastOK; 500 => error, batch dropped;
// hang => client timeout => error. No retries ever happen (one batch, one
// POST).
func TestTracerExportOutcomes(t *testing.T) {
	c := &spanCollector{}
	srv := httptest.NewServer(c.handler())
	defer srv.Close()

	tr := testTracer(srv.URL, 512)
	var outcomes []string
	var spanTotals []int
	tr.onBatch = func(outcome string, spans int) {
		outcomes = append(outcomes, outcome)
		spanTotals = append(spanTotals, spans)
	}

	tr.export([]*Span{endedSpan(tr, "ok")})
	if tr.lastOK.Load() == 0 {
		t.Fatalf("lastOK not set after 200")
	}
	if tr.LastExportOK().IsZero() {
		t.Fatalf("LastExportOK zero after success")
	}

	c.status.Store(http.StatusInternalServerError)
	before := tr.LastExportOK()
	tr.export([]*Span{endedSpan(tr, "fail1"), endedSpan(tr, "fail2")})
	if !tr.LastExportOK().Equal(before) {
		t.Fatalf("lastOK advanced on a failed export")
	}

	// Hanging collector: the 500ms test client timeout fires (production
	// uses 3s per the plan).
	c.status.Store(0)
	c.delay = time.Second
	tr.export([]*Span{endedSpan(tr, "hang")})

	want := []string{"ok", "error", "error"}
	if len(outcomes) != 3 || outcomes[0] != want[0] || outcomes[1] != want[1] || outcomes[2] != want[2] {
		t.Fatalf("outcomes = %v, want %v", outcomes, want)
	}
	if spanTotals[0] != 1 || spanTotals[1] != 2 || spanTotals[2] != 1 {
		t.Fatalf("span totals = %v", spanTotals)
	}
	// The hanging handler finishes recording after the client timeout; wait
	// for it, then confirm exactly one POST per batch (no retries).
	if got := waitForBatches(t, c, 3); len(got) != 3 {
		t.Fatalf("collector saw %d POSTs, want 3 (no retries)", len(got))
	}
}

// TestTracerWarnRateLimit: export-failure warnings fire at most once/minute.
func TestTracerWarnRateLimit(t *testing.T) {
	tr := testTracer("http://127.0.0.1:1", 512)
	now := time.Unix(1_700_000_000, 0)
	tr.now = func() time.Time { return now }

	if !tr.shouldWarnExport() {
		t.Fatalf("first warn suppressed")
	}
	now = now.Add(30 * time.Second)
	if tr.shouldWarnExport() {
		t.Fatalf("warn within the same minute not suppressed")
	}
	now = now.Add(31 * time.Second)
	if !tr.shouldWarnExport() {
		t.Fatalf("warn after a minute suppressed")
	}
}

// TestTracerStartAPIs: root vs child ids, sampling inheritance, disabled
// tracer still generates propagatable contexts, nil safety.
func TestTracerStartAPIs(t *testing.T) {
	tr := newTracer("", 1.0, "test")
	root := tr.StartRoot("op", KindServer)
	sc := root.Context()
	if len(sc.TraceID) != 32 || len(sc.SpanID) != 16 || !sc.Sampled {
		t.Fatalf("root context = %+v", sc)
	}
	if tp := formatTraceparent(sc); tp == "" {
		t.Fatalf("no traceparent from disabled tracer")
	}
	child := tr.StartChild(sc, "child", KindClient)
	cc := child.Context()
	if cc.TraceID != sc.TraceID || cc.SpanID == sc.SpanID || child.parent != sc.SpanID {
		t.Fatalf("child context = %+v parent=%q", cc, child.parent)
	}
	// End on a disabled tracer must not block or panic.
	root.End()
	child.End()

	// Nil tracer / nil span safety.
	var nilTracer *Tracer
	sp := nilTracer.StartRoot("x", KindInternal)
	if sp != nil {
		t.Fatalf("nil tracer returned a span")
	}
	sp.SetAttr("k", "v")
	sp.AddEvent("e")
	sp.AddLink("t", "s")
	sp.SetError("m")
	sp.End()
	if got := sp.Context(); got != (SpanContext{}) {
		t.Fatalf("nil span context = %+v", got)
	}
	nilTracer.Close()

	// Ratio 0: root spans are unsampled but ids still exist.
	tr0 := newTracer("", 0, "test")
	r0 := tr0.StartRoot("op", KindServer)
	if r0.Context().Sampled {
		t.Fatalf("ratio 0 sampled a root")
	}
	if r0.Context().TraceID == "" {
		t.Fatalf("ratio 0 root has no trace id")
	}
}
