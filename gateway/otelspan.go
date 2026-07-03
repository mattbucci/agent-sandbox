package main

// Hand-rolled OpenTelemetry tracing (plan item 10, span model §g): W3C
// traceparent parse/format, deterministic trace-id ratio sampling, and an
// OTLP/HTTP proto3-JSON exporter POSTing to <otlp_endpoint>/v1/traces.
// Standard library only (offline-host constraint).
//
// Disabled mode (otlp_endpoint ""): spans are still created with real ids so
// traceparent keeps being generated/propagated (in-VM traces stay linkable),
// but End() drops them instead of exporting.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// SpanKind values follow the OTLP span kind enum.
type SpanKind int

const (
	KindInternal SpanKind = 1
	KindServer   SpanKind = 2
	KindClient   SpanKind = 3
)

// otlpStatusError is the OTLP status code for STATUS_CODE_ERROR.
const otlpStatusError = 2

// SpanContext identifies a span: lowercase-hex trace/span ids plus the
// sampled flag, exactly what a traceparent header carries.
type SpanContext struct {
	TraceID string // 32 lowercase hex chars
	SpanID  string // 16 lowercase hex chars
	Sampled bool
}

// parseTraceparent parses a W3C traceparent header per the spec table:
// >= 4 dash-separated parts, 2-char lowercase-hex version != "ff" (version 00
// must have exactly 4 parts; future versions may append fields), lowercase
// hex only, non-zero trace and span ids. Invalid headers yield ok=false and
// the caller starts a new root.
func parseTraceparent(h string) (SpanContext, bool) {
	parts := strings.Split(h, "-")
	if len(parts) < 4 {
		return SpanContext{}, false
	}
	version, traceID, spanID, flags := parts[0], parts[1], parts[2], parts[3]
	if len(version) != 2 || !isLowerHex(version) || version == "ff" {
		return SpanContext{}, false
	}
	if version == "00" && len(parts) != 4 {
		return SpanContext{}, false
	}
	if len(traceID) != 32 || !isLowerHex(traceID) || isZeroHex(traceID) {
		return SpanContext{}, false
	}
	if len(spanID) != 16 || !isLowerHex(spanID) || isZeroHex(spanID) {
		return SpanContext{}, false
	}
	if len(flags) != 2 || !isLowerHex(flags) {
		return SpanContext{}, false
	}
	fv, _ := strconv.ParseUint(flags, 16, 8)
	return SpanContext{TraceID: traceID, SpanID: spanID, Sampled: fv&1 == 1}, true
}

// formatTraceparent renders sc as a version-00 traceparent header.
func formatTraceparent(sc SpanContext) string {
	flags := "00"
	if sc.Sampled {
		flags = "01"
	}
	return "00-" + sc.TraceID + "-" + sc.SpanID + "-" + flags
}

// isLowerHex reports whether s is entirely [0-9a-f].
func isLowerHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// isZeroHex reports whether s is all '0' characters.
func isZeroHex(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] != '0' {
			return false
		}
	}
	return true
}

// sampleTraceID is the deterministic ratio sampler: the first 8 bytes of the
// trace id, read as a uint64, are compared against ratio*MaxUint64 so every
// participant that sees the same trace id makes the same decision.
func sampleTraceID(traceID string, ratio float64) bool {
	if ratio >= 1 {
		return true
	}
	if ratio <= 0 {
		return false
	}
	if len(traceID) < 16 {
		return false
	}
	v, err := strconv.ParseUint(traceID[:16], 16, 64)
	if err != nil {
		return false
	}
	return float64(v) < ratio*float64(math.MaxUint64)
}

// spanEvent is a point-in-time annotation on a span.
type spanEvent struct {
	time time.Time
	name string
}

// spanLink references another span context (task attempt -> submit trace).
type spanLink struct {
	traceID string
	spanID  string
}

// spanAttr is one key/value attribute, already in OTLP value form.
type spanAttr struct {
	key string
	val otlpValue
}

// Span is a single in-flight or finished span. A span is owned by one
// goroutine until End(); no internal locking. All methods are nil-safe so
// call sites need no tracer-enabled checks.
type Span struct {
	t      *Tracer
	sc     SpanContext
	parent string // parent span id; "" for roots (parentSpanId omitted)
	name   string
	kind   SpanKind
	start  time.Time
	end    time.Time
	attrs  []spanAttr
	events []spanEvent
	links  []spanLink
	errSet bool
	errMsg string
	ended  bool
}

// Context returns the span's context (zero value on nil).
func (s *Span) Context() SpanContext {
	if s == nil {
		return SpanContext{}
	}
	return s.sc
}

// SetAttr records an attribute. Supported value types: string, bool, int,
// int64, float64 (anything else is stored via fmt.Sprint as a string).
func (s *Span) SetAttr(key string, value any) {
	if s == nil {
		return
	}
	s.attrs = append(s.attrs, spanAttr{key: key, val: otlpValueOf(value)})
}

// AddEvent records a point-in-time event stamped with the tracer clock.
func (s *Span) AddEvent(name string) {
	if s == nil {
		return
	}
	s.events = append(s.events, spanEvent{time: s.t.now(), name: name})
}

// AddLink records a link to another span context.
func (s *Span) AddLink(traceID, spanID string) {
	if s == nil {
		return
	}
	s.links = append(s.links, spanLink{traceID: traceID, spanID: spanID})
}

// SetError marks the span status as ERROR with a message. Unset status is
// omitted from the OTLP encoding entirely.
func (s *Span) SetError(msg string) {
	if s == nil {
		return
	}
	s.errSet = true
	s.errMsg = msg
}

// End stamps the end time and hands the span to the exporter (dropped when
// export is disabled, the span is unsampled, or the queue is full).
func (s *Span) End() {
	if s == nil || s.ended {
		return
	}
	s.ended = true
	s.end = s.t.now()
	s.t.enqueue(s)
}

// spanLogAttrs returns trace_id/span_id log attrs for sp (nil => none), so
// in-request canonical events can be joined with their trace.
func spanLogAttrs(sp *Span) []any {
	if sp == nil {
		return nil
	}
	return []any{"trace_id", sp.sc.TraceID, "span_id", sp.sc.SpanID}
}

// Tracer creates spans and exports them: one exporter goroutine drains a
// cap-1024 channel into 512-span/5s batches POSTed with a 3s timeout.
// Failures drop the batch (the collector is the durability layer) with a
// warn rate-limited to one per minute.
type Tracer struct {
	endpoint string // "" => export disabled
	ratio    float64
	client   *http.Client
	resource []otlpKeyValue

	ch   chan *Span
	quit chan struct{}
	done chan struct{}

	batchMax   int
	flushEvery time.Duration

	// now is injectable for tests.
	now func() time.Time
	// onBatch/onDrop are the metrics hooks (nil-safe), wired by main().
	onBatch func(outcome string, spans int)
	onDrop  func(n int)

	closed  atomic.Bool
	started atomic.Bool
	lastOK  atomic.Int64 // UnixNano of the last successful export
	dropped atomic.Uint64

	warnMu   sync.Mutex
	lastWarn time.Time
}

// newTracer builds a Tracer for the given OTLP endpoint ("" disables export)
// and sampling ratio. version feeds the service.version resource attribute.
func newTracer(endpoint string, ratio float64, version string) *Tracer {
	host, _ := os.Hostname()
	if host == "" {
		host = "unknown"
	}
	return &Tracer{
		endpoint:   strings.TrimRight(endpoint, "/"),
		ratio:      ratio,
		client:     &http.Client{Timeout: 3 * time.Second},
		resource:   resourceAttrs(host, version, os.Getpid()),
		ch:         make(chan *Span, 1024),
		quit:       make(chan struct{}),
		done:       make(chan struct{}),
		batchMax:   512,
		flushEvery: 5 * time.Second,
		now:        time.Now,
	}
}

// resourceAttrs builds the fixed OTLP resource attribute list (§g).
func resourceAttrs(host, version string, pid int) []otlpKeyValue {
	return []otlpKeyValue{
		otlpStringKV("service.name", "hermes-gateway"),
		otlpStringKV("service.version", version),
		otlpStringKV("service.instance.id", fmt.Sprintf("%s-%d", host, pid)),
		otlpStringKV("host.name", host),
	}
}

// StartRoot opens a new root span in a fresh trace; the sampling decision is
// made here from the new trace id.
func (t *Tracer) StartRoot(name string, kind SpanKind) *Span {
	if t == nil {
		return nil
	}
	traceID := randHex(16)
	sc := SpanContext{TraceID: traceID, SpanID: randHex(8), Sampled: sampleTraceID(traceID, t.ratio)}
	return &Span{t: t, sc: sc, name: name, kind: kind, start: t.now()}
}

// StartChild opens a child span of parent, inheriting trace id and sampling.
func (t *Tracer) StartChild(parent SpanContext, name string, kind SpanKind) *Span {
	if t == nil {
		return nil
	}
	return t.StartChildAt(parent, name, kind, t.now())
}

// StartChildAt is StartChild with an explicit start time (used for spans
// reconstructed after the fact, e.g. sched.wait).
func (t *Tracer) StartChildAt(parent SpanContext, name string, kind SpanKind, start time.Time) *Span {
	if t == nil {
		return nil
	}
	sc := SpanContext{TraceID: parent.TraceID, SpanID: randHex(8), Sampled: parent.Sampled}
	return &Span{t: t, sc: sc, parent: parent.SpanID, name: name, kind: kind, start: start}
}

// enqueue hands a finished span to the exporter, dropping (and counting) when
// the channel is full, export is disabled, or the span is unsampled.
func (t *Tracer) enqueue(s *Span) {
	if t.endpoint == "" || !s.sc.Sampled || t.closed.Load() {
		return
	}
	select {
	case t.ch <- s:
	default:
		t.dropped.Add(1)
		if h := t.onDrop; h != nil {
			h(1)
		}
	}
}

// StartExporter launches the single exporter goroutine (no-op when export is
// disabled). Call once, after the onBatch/onDrop hooks are set.
func (t *Tracer) StartExporter() {
	if t == nil || t.endpoint == "" || t.started.Swap(true) {
		return
	}
	ticker := time.NewTicker(t.flushEvery)
	go func() {
		defer ticker.Stop()
		t.run(ticker.C)
	}()
}

// run drains the span channel into batches: flush at batchMax spans, on every
// tick, and once more (draining what is already queued) on shutdown.
func (t *Tracer) run(tick <-chan time.Time) {
	defer close(t.done)
	batch := make([]*Span, 0, t.batchMax)
	flush := func() {
		if len(batch) > 0 {
			t.export(batch)
			batch = batch[:0]
		}
	}
	for {
		select {
		case sp := <-t.ch:
			batch = append(batch, sp)
			if len(batch) >= t.batchMax {
				flush()
			}
		case <-tick:
			flush()
		case <-t.quit:
			for {
				select {
				case sp := <-t.ch:
					batch = append(batch, sp)
					if len(batch) >= t.batchMax {
						flush()
					}
				default:
					flush()
					return
				}
			}
		}
	}
}

// Close stops the exporter after a final flush (2s cap). Safe to call on a
// tracer that never exported.
func (t *Tracer) Close() {
	if t == nil || t.closed.Swap(true) {
		return
	}
	if !t.started.Load() {
		return
	}
	close(t.quit)
	select {
	case <-t.done:
	case <-time.After(2 * time.Second):
	}
}

// LastExportOK returns the time of the last successful export (zero when
// none yet) — the dashboard's collector-liveness dot.
func (t *Tracer) LastExportOK() time.Time {
	if t == nil {
		return time.Time{}
	}
	n := t.lastOK.Load()
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}

// DroppedSpans returns the cumulative count of spans dropped at enqueue.
func (t *Tracer) DroppedSpans() uint64 {
	if t == nil {
		return 0
	}
	return t.dropped.Load()
}

// export POSTs one batch; failures drop it (no retry) and warn at most once
// per minute. The onBatch hook feeds the otlp_export_batches_total metric.
func (t *Tracer) export(batch []*Span) {
	body := encodeOTLP(t.resource, batch)
	outcome := "ok"
	req, err := http.NewRequest(http.MethodPost, t.endpoint+"/v1/traces", bytes.NewReader(body))
	if err != nil {
		outcome = "error"
		t.warnExportFail(err.Error())
	} else {
		req.Header.Set("Content-Type", "application/json")
		resp, derr := t.client.Do(req)
		if derr != nil {
			outcome = "error"
			t.warnExportFail(derr.Error())
		} else {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode < 200 || resp.StatusCode > 299 {
				outcome = "error"
				t.warnExportFail(fmt.Sprintf("collector returned status %d", resp.StatusCode))
			} else {
				t.lastOK.Store(t.now().UnixNano())
			}
		}
	}
	if h := t.onBatch; h != nil {
		h(outcome, len(batch))
	}
}

// warnExportFail emits the otlp_export_fail event, rate-limited to 1/min.
func (t *Tracer) warnExportFail(msg string) {
	if t.shouldWarnExport() {
		logWarn("otlp_export_fail", "endpoint", t.endpoint, "err", msg)
	}
}

// shouldWarnExport implements the 1/min warn rate limit.
func (t *Tracer) shouldWarnExport() bool {
	t.warnMu.Lock()
	defer t.warnMu.Unlock()
	now := t.now()
	if !t.lastWarn.IsZero() && now.Sub(t.lastWarn) < time.Minute {
		return false
	}
	t.lastWarn = now
	return true
}

// --- OTLP proto3-JSON encoding (§g quirks are golden-tested) ---
//
// Trace/span ids are lowercase hex strings; *TimeUnixNano are decimal
// strings; intValue is a string; parentSpanId is omitted for roots; status
// is omitted when unset.

type otlpExportRequest struct {
	ResourceSpans []otlpResourceSpans `json:"resourceSpans"`
}

type otlpResourceSpans struct {
	Resource   otlpResource     `json:"resource"`
	ScopeSpans []otlpScopeSpans `json:"scopeSpans"`
}

type otlpResource struct {
	Attributes []otlpKeyValue `json:"attributes"`
}

type otlpScopeSpans struct {
	Scope otlpScope  `json:"scope"`
	Spans []otlpSpan `json:"spans"`
}

type otlpScope struct {
	Name string `json:"name"`
}

type otlpKeyValue struct {
	Key   string    `json:"key"`
	Value otlpValue `json:"value"`
}

type otlpValue struct {
	StringValue *string  `json:"stringValue,omitempty"`
	BoolValue   *bool    `json:"boolValue,omitempty"`
	IntValue    *string  `json:"intValue,omitempty"` // proto3-JSON: int64 as decimal string
	DoubleValue *float64 `json:"doubleValue,omitempty"`
}

type otlpSpan struct {
	TraceID           string          `json:"traceId"`
	SpanID            string          `json:"spanId"`
	ParentSpanID      string          `json:"parentSpanId,omitempty"`
	Name              string          `json:"name"`
	Kind              int             `json:"kind"`
	StartTimeUnixNano string          `json:"startTimeUnixNano"`
	EndTimeUnixNano   string          `json:"endTimeUnixNano"`
	Attributes        []otlpKeyValue  `json:"attributes,omitempty"`
	Events            []otlpSpanEvent `json:"events,omitempty"`
	Links             []otlpSpanLink  `json:"links,omitempty"`
	Status            *otlpStatus     `json:"status,omitempty"`
}

type otlpSpanEvent struct {
	TimeUnixNano string `json:"timeUnixNano"`
	Name         string `json:"name"`
}

type otlpSpanLink struct {
	TraceID string `json:"traceId"`
	SpanID  string `json:"spanId"`
}

type otlpStatus struct {
	Code    int    `json:"code"`
	Message string `json:"message,omitempty"`
}

// otlpStringKV builds a string-valued OTLP attribute.
func otlpStringKV(key, val string) otlpKeyValue {
	return otlpKeyValue{Key: key, Value: otlpValue{StringValue: &val}}
}

// otlpValueOf converts a Go value to its OTLP AnyValue encoding.
func otlpValueOf(value any) otlpValue {
	switch v := value.(type) {
	case string:
		return otlpValue{StringValue: &v}
	case bool:
		return otlpValue{BoolValue: &v}
	case int:
		s := strconv.FormatInt(int64(v), 10)
		return otlpValue{IntValue: &s}
	case int64:
		s := strconv.FormatInt(v, 10)
		return otlpValue{IntValue: &s}
	case float64:
		return otlpValue{DoubleValue: &v}
	default:
		s := fmt.Sprint(v)
		return otlpValue{StringValue: &s}
	}
}

// encodeOTLP renders one export request for a batch of finished spans.
func encodeOTLP(resource []otlpKeyValue, batch []*Span) []byte {
	spans := make([]otlpSpan, 0, len(batch))
	for _, s := range batch {
		es := otlpSpan{
			TraceID:           s.sc.TraceID,
			SpanID:            s.sc.SpanID,
			ParentSpanID:      s.parent,
			Name:              s.name,
			Kind:              int(s.kind),
			StartTimeUnixNano: strconv.FormatInt(s.start.UnixNano(), 10),
			EndTimeUnixNano:   strconv.FormatInt(s.end.UnixNano(), 10),
		}
		for _, a := range s.attrs {
			es.Attributes = append(es.Attributes, otlpKeyValue{Key: a.key, Value: a.val})
		}
		for _, e := range s.events {
			es.Events = append(es.Events, otlpSpanEvent{
				TimeUnixNano: strconv.FormatInt(e.time.UnixNano(), 10),
				Name:         e.name,
			})
		}
		for _, l := range s.links {
			es.Links = append(es.Links, otlpSpanLink{TraceID: l.traceID, SpanID: l.spanID})
		}
		if s.errSet {
			es.Status = &otlpStatus{Code: otlpStatusError, Message: s.errMsg}
		}
		spans = append(spans, es)
	}
	req := otlpExportRequest{
		ResourceSpans: []otlpResourceSpans{{
			Resource:   otlpResource{Attributes: resource},
			ScopeSpans: []otlpScopeSpans{{Scope: otlpScope{Name: "hermes-gateway"}, Spans: spans}},
		}},
	}
	data, _ := json.Marshal(req)
	return data
}
