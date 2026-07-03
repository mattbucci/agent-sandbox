package main

// Hand-rolled Prometheus metrics (plan item 11, metric set §f): a fixed
// registry of counters (atomic.Uint64), gauges and histograms (per-series
// mutex, fixed buckets), exposed in text format 0.0.4 at /metrics.
// Scheduler/task/VM gauges are computed at scrape time from Snapshot()s so
// they cannot drift. Agent-labeled series are pre-registered from config.
//
// /metrics auth (§b): unauthenticated iff RemoteAddr is loopback (the
// collector scrapes 127.0.0.1 secret-free); otherwise any gateway or
// dashboard bearer — the LAN cannot enumerate agents/load anonymously.

import (
	"crypto/subtle"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// metricPrefix namespaces every exported series.
const metricPrefix = "hermes_gateway_"

// taskStateNames is the fixed to_state / task-state label set.
var taskStateNames = []TaskState{TaskPending, TaskRunning, TaskSucceeded, TaskFailed, TaskCancelled, TaskExpired}

// Histogram bucket bounds (§f), seconds.
var (
	httpDurationBuckets = []float64{.01, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60, 120, 300, 600, 1800, 3600}
	proxyTTFBBuckets    = []float64{.1, .25, .5, 1, 2.5, 5, 10, 30, 60, 120}
	schedWaitBuckets    = []float64{.01, .1, .5, 1, 5, 15, 60, 300, 900}
	taskDurationBuckets = []float64{10, 60, 300, 900, 1800, 3600, 7200, 14400, 28800}
)

// vecSeries is one label combination of a counter or gauge vector.
type vecSeries struct {
	labelVals []string
	c         atomic.Uint64 // counters
	g         atomic.Int64  // gauges
}

// metricVec is a counter or gauge vector: fixed label names, series created
// on first use (or pre-registered so they expose as 0). One mutex guards the
// series map only; increments on a fetched series are atomic.
type metricVec struct {
	name       string // without prefix
	help       string
	typ        string // "counter" | "gauge"
	labelNames []string

	mu     sync.Mutex
	series map[string]*vecSeries
}

func newMetricVec(name, help, typ string, labelNames ...string) *metricVec {
	return &metricVec{name: name, help: help, typ: typ, labelNames: labelNames, series: make(map[string]*vecSeries)}
}

// with returns (creating if needed) the series for the given label values.
func (v *metricVec) with(labelVals ...string) *vecSeries {
	key := strings.Join(labelVals, "\x00")
	v.mu.Lock()
	defer v.mu.Unlock()
	s, ok := v.series[key]
	if !ok {
		vals := make([]string, len(labelVals))
		copy(vals, labelVals)
		s = &vecSeries{labelVals: vals}
		v.series[key] = s
	}
	return s
}

func (v *metricVec) add(n uint64, labelVals ...string) { v.with(labelVals...).c.Add(n) }
func (v *metricVec) inc(labelVals ...string)           { v.add(1, labelVals...) }
func (v *metricVec) gaugeAdd(d int64, labelVals ...string) {
	v.with(labelVals...).g.Add(d)
}

// snapshot returns the series sorted by label values for stable exposition.
func (v *metricVec) snapshot() []*vecSeries {
	v.mu.Lock()
	out := make([]*vecSeries, 0, len(v.series))
	for _, s := range v.series {
		out = append(out, s)
	}
	v.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		return strings.Join(out[i].labelVals, "\x00") < strings.Join(out[j].labelVals, "\x00")
	})
	return out
}

// write emits HELP/TYPE and every series (counters as c, gauges as g).
func (v *metricVec) write(w io.Writer) {
	fmt.Fprintf(w, "# HELP %s%s %s\n# TYPE %s%s %s\n", metricPrefix, v.name, v.help, metricPrefix, v.name, v.typ)
	for _, s := range v.snapshot() {
		if v.typ == "gauge" {
			fmt.Fprintf(w, "%s%s%s %d\n", metricPrefix, v.name, labelString(v.labelNames, s.labelVals), s.g.Load())
		} else {
			fmt.Fprintf(w, "%s%s%s %d\n", metricPrefix, v.name, labelString(v.labelNames, s.labelVals), s.c.Load())
		}
	}
}

// histSeries is one label combination of a histogram vector.
type histSeries struct {
	labelVals []string
	mu        sync.Mutex
	counts    []uint64 // per-bucket (non-cumulative); overflow goes to +Inf only
	sum       float64
	count     uint64
}

// histogramVec is a histogram vector with fixed buckets.
type histogramVec struct {
	name       string
	help       string
	labelNames []string
	buckets    []float64

	mu     sync.Mutex
	series map[string]*histSeries
}

func newHistogramVec(name, help string, buckets []float64, labelNames ...string) *histogramVec {
	return &histogramVec{name: name, help: help, labelNames: labelNames, buckets: buckets, series: make(map[string]*histSeries)}
}

func (h *histogramVec) with(labelVals ...string) *histSeries {
	key := strings.Join(labelVals, "\x00")
	h.mu.Lock()
	defer h.mu.Unlock()
	s, ok := h.series[key]
	if !ok {
		vals := make([]string, len(labelVals))
		copy(vals, labelVals)
		s = &histSeries{labelVals: vals, counts: make([]uint64, len(h.buckets))}
		h.series[key] = s
	}
	return s
}

func (h *histogramVec) observe(v float64, labelVals ...string) {
	s := h.with(labelVals...)
	s.mu.Lock()
	for i, b := range h.buckets {
		if v <= b {
			s.counts[i]++
			break
		}
	}
	s.sum += v
	s.count++
	s.mu.Unlock()
}

func (h *histogramVec) write(w io.Writer) {
	fmt.Fprintf(w, "# HELP %s%s %s\n# TYPE %s%s histogram\n", metricPrefix, h.name, h.help, metricPrefix, h.name)
	h.mu.Lock()
	all := make([]*histSeries, 0, len(h.series))
	for _, s := range h.series {
		all = append(all, s)
	}
	h.mu.Unlock()
	sort.Slice(all, func(i, j int) bool {
		return strings.Join(all[i].labelVals, "\x00") < strings.Join(all[j].labelVals, "\x00")
	})
	for _, s := range all {
		s.mu.Lock()
		counts := make([]uint64, len(s.counts))
		copy(counts, s.counts)
		sum, count := s.sum, s.count
		s.mu.Unlock()

		cum := uint64(0)
		for i, b := range h.buckets {
			cum += counts[i]
			fmt.Fprintf(w, "%s%s_bucket%s %d\n", metricPrefix, h.name,
				labelStringExtra(h.labelNames, s.labelVals, "le", fmtFloat(b)), cum)
		}
		fmt.Fprintf(w, "%s%s_bucket%s %d\n", metricPrefix, h.name,
			labelStringExtra(h.labelNames, s.labelVals, "le", "+Inf"), count)
		fmt.Fprintf(w, "%s%s_sum%s %s\n", metricPrefix, h.name, labelString(h.labelNames, s.labelVals), fmtFloat(sum))
		fmt.Fprintf(w, "%s%s_count%s %d\n", metricPrefix, h.name, labelString(h.labelNames, s.labelVals), count)
	}
}

// labelString renders {a="x",b="y"} ("" when there are no labels).
func labelString(names, vals []string) string {
	if len(names) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteByte('{')
	for i, n := range names {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(n)
		b.WriteString(`="`)
		b.WriteString(escapeLabelValue(vals[i]))
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}

// labelStringExtra is labelString plus one appended label (the le bound).
func labelStringExtra(names, vals []string, extraName, extraVal string) string {
	return labelString(append(append([]string{}, names...), extraName),
		append(append([]string{}, vals...), extraVal))
}

// escapeLabelValue escapes a label value per the text exposition format.
func escapeLabelValue(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	return s
}

// fmtFloat renders a float the way Prometheus expects (shortest form).
func fmtFloat(v float64) string {
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// Metrics is the process-wide registry. All methods are nil-receiver-safe so
// instrumentation call sites work in tests that wire no metrics.
type Metrics struct {
	cfg     *Config
	version string
	sched   *Scheduler
	store   *TaskStore

	// counters
	httpRequests     *metricVec
	authFailures     atomic.Uint64
	upstreamErrors   *metricVec
	streamBytes      *metricVec
	schedAdmitted    *metricVec
	schedRejected    *metricVec
	taskTransitions  *metricVec
	taskRetries      *metricVec
	otlpBatches      *metricVec
	otlpSpansOut     atomic.Uint64
	otlpSpansDropped atomic.Uint64
	dashSourceErrors *metricVec

	// set-style gauges
	inflight *metricVec

	// histograms
	httpDuration *histogramVec
	proxyTTFB    *histogramVec
	schedWait    *histogramVec
	taskDuration *histogramVec
}

// gwMetrics is the process registry set by main(); the dashboard lane uses it
// for dashboard_source_errors_total (DashDeps is frozen without a Metrics
// field). Nil in tests that do not wire metrics.
var gwMetrics *Metrics

// newMetrics builds the registry and pre-registers every fixed-label series
// derivable from config, so known series expose as 0 instead of appearing.
func newMetrics(cfg *Config, version string, sched *Scheduler, store *TaskStore) *Metrics {
	m := &Metrics{
		cfg:     cfg,
		version: version,
		sched:   sched,
		store:   store,

		httpRequests: newMetricVec("http_requests_total",
			"Total HTTP requests handled, by normalized path, method and status code.", "counter",
			"path", "method", "code"),
		upstreamErrors: newMetricVec("upstream_errors_total",
			"Downstream VM request failures by agent and reason.", "counter", "agent", "reason"),
		streamBytes: newMetricVec("stream_bytes_total",
			"Response bytes streamed from VMs, by agent.", "counter", "agent"),
		schedAdmitted: newMetricVec("sched_admitted_total",
			"Scheduler slot grants by agent and class.", "counter", "agent", "class"),
		schedRejected: newMetricVec("sched_rejected_total",
			"Scheduler slot rejections by agent and reason.", "counter", "agent", "reason"),
		taskTransitions: newMetricVec("task_transitions_total",
			"Task state transitions by agent and destination state.", "counter", "agent", "to_state"),
		taskRetries: newMetricVec("task_retries_total",
			"Task retry requeues by agent.", "counter", "agent"),
		otlpBatches: newMetricVec("otlp_export_batches_total",
			"OTLP span export batches by outcome.", "counter", "outcome"),
		dashSourceErrors: newMetricVec("dashboard_source_errors_total",
			"Dashboard data-source read/parse errors by source.", "counter", "source"),
		inflight: newMetricVec("http_inflight_requests",
			"In-flight HTTP requests by normalized path.", "gauge", "path"),

		httpDuration: newHistogramVec("http_request_duration_seconds",
			"HTTP request duration by normalized path.", httpDurationBuckets, "path"),
		proxyTTFB: newHistogramVec("proxy_ttfb_seconds",
			"Time to first downstream body byte, by agent.", proxyTTFBBuckets, "agent"),
		schedWait: newHistogramVec("sched_wait_seconds",
			"Scheduler queue wait before a grant, by agent.", schedWaitBuckets, "agent"),
		taskDuration: newHistogramVec("task_duration_seconds",
			"Task duration from submit to terminal state, by agent and outcome.", taskDurationBuckets, "agent", "outcome"),
	}

	agents := make([]string, 0, len(cfg.Agents))
	for name := range cfg.Agents {
		agents = append(agents, name)
	}
	sort.Strings(agents)
	for _, a := range agents {
		for _, reason := range []string{"no_vm", "connect", "status_5xx"} {
			m.upstreamErrors.with(a, reason)
		}
		m.streamBytes.with(a)
		for _, class := range []string{"sync", "task"} {
			m.schedAdmitted.with(a, class)
		}
		for _, reason := range []string{"queue_full", "wait_timeout", "client_gone"} {
			m.schedRejected.with(a, reason)
		}
		for _, st := range taskStateNames {
			m.taskTransitions.with(a, string(st))
		}
		m.taskRetries.with(a)
		m.proxyTTFB.with(a)
		m.schedWait.with(a)
		for _, outcome := range []TaskState{TaskSucceeded, TaskFailed, TaskCancelled, TaskExpired} {
			m.taskDuration.with(a, string(outcome))
		}
	}
	for _, outcome := range []string{"ok", "error"} {
		m.otlpBatches.with(outcome)
	}
	for _, source := range []string{"traces", "squid"} {
		m.dashSourceErrors.with(source)
	}
	return m
}

// ObserveHTTPRequest records one finished request (middleware).
func (m *Metrics) ObserveHTTPRequest(path, method string, code int, dur time.Duration) {
	if m == nil {
		return
	}
	m.httpRequests.inc(path, method, strconv.Itoa(code))
	m.httpDuration.observe(dur.Seconds(), path)
}

// InflightAdd adjusts the in-flight gauge for a path (middleware).
func (m *Metrics) InflightAdd(path string, delta int64) {
	if m == nil {
		return
	}
	m.inflight.gaugeAdd(delta, path)
}

// IncAuthFailure counts one rejected bearer.
func (m *Metrics) IncAuthFailure() {
	if m == nil {
		return
	}
	m.authFailures.Add(1)
}

// IncUpstreamError counts a downstream VM failure (no_vm|connect|status_5xx).
func (m *Metrics) IncUpstreamError(agent, reason string) {
	if m == nil {
		return
	}
	m.upstreamErrors.inc(agent, reason)
}

// AddStreamBytes counts bytes streamed from a VM.
func (m *Metrics) AddStreamBytes(agent string, n int64) {
	if m == nil || n <= 0 {
		return
	}
	m.streamBytes.add(uint64(n), agent)
}

// ObserveProxyTTFB records time-to-first-byte for a downstream call.
func (m *Metrics) ObserveProxyTTFB(agent string, d time.Duration) {
	if m == nil {
		return
	}
	m.proxyTTFB.observe(d.Seconds(), agent)
}

// HandleSchedEvent is the Scheduler onEvent hook.
func (m *Metrics) HandleSchedEvent(ev SchedEvent) {
	if m == nil {
		return
	}
	switch ev.Type {
	case SchedEventGranted:
		m.schedAdmitted.inc(ev.Agent, ev.Class)
		m.schedWait.observe(ev.Wait.Seconds(), ev.Agent)
	case SchedEventRejectedFull:
		m.schedRejected.inc(ev.Agent, "queue_full")
	case SchedEventWaitTimeout:
		m.schedRejected.inc(ev.Agent, "wait_timeout")
	case SchedEventWaitCancelled:
		m.schedRejected.inc(ev.Agent, "client_gone")
	}
}

// HandleTaskEvent is (part of) the TaskStore onTransition hook.
func (m *Metrics) HandleTaskEvent(ev TaskEvent) {
	if m == nil {
		return
	}
	m.taskTransitions.inc(ev.Task.Agent, string(ev.To))
	if ev.From == TaskRunning && ev.To == TaskPending {
		m.taskRetries.inc(ev.Task.Agent)
	}
	if ev.To.IsTerminal() {
		end := ev.Task.UpdatedAt
		if ev.Task.FinishedAt != nil {
			end = *ev.Task.FinishedAt
		}
		m.taskDuration.observe(end.Sub(ev.Task.CreatedAt).Seconds(), ev.Task.Agent, string(ev.To))
	}
}

// OnOTLPBatch is the Tracer onBatch hook.
func (m *Metrics) OnOTLPBatch(outcome string, spans int) {
	if m == nil {
		return
	}
	m.otlpBatches.inc(outcome)
	if outcome == "ok" {
		m.otlpSpansOut.Add(uint64(spans))
	}
}

// OnOTLPDrop is the Tracer onDrop hook.
func (m *Metrics) OnOTLPDrop(n int) {
	if m == nil {
		return
	}
	m.otlpSpansDropped.Add(uint64(n))
}

// IncDashboardSourceError counts a dashboard data-source failure
// (source: traces|squid). Used by the dashboard lane via gwMetrics.
func (m *Metrics) IncDashboardSourceError(source string) {
	if m == nil {
		return
	}
	m.dashSourceErrors.inc(source)
}

// writeScalarCounter emits a single label-less counter.
func writeScalarCounter(w io.Writer, name, help string, v uint64) {
	fmt.Fprintf(w, "# HELP %s%s %s\n# TYPE %s%s counter\n%s%s %d\n",
		metricPrefix, name, help, metricPrefix, name, metricPrefix, name, v)
}

// gaugeLine emits one gauge sample with an integer value.
func gaugeLine(w io.Writer, name string, names, vals []string, v int64) {
	fmt.Fprintf(w, "%s%s%s %d\n", metricPrefix, name, labelString(names, vals), v)
}

// gaugeHeader emits HELP/TYPE for a gauge family.
func gaugeHeader(w io.Writer, name, help string) {
	fmt.Fprintf(w, "# HELP %s%s %s\n# TYPE %s%s gauge\n", metricPrefix, name, help, metricPrefix, name)
}

// WriteExposition renders the whole registry in text format 0.0.4, families
// in the §f order. Scheduler/task/VM gauges are computed here, at scrape
// time, from the live Snapshot()s.
func (m *Metrics) WriteExposition(w io.Writer) {
	// Counters.
	m.httpRequests.write(w)
	writeScalarCounter(w, "auth_failures_total", "Requests rejected for a missing or invalid bearer token.", m.authFailures.Load())
	m.upstreamErrors.write(w)
	m.streamBytes.write(w)
	m.schedAdmitted.write(w)
	m.schedRejected.write(w)
	m.taskTransitions.write(w)
	m.taskRetries.write(w)
	m.otlpBatches.write(w)
	writeScalarCounter(w, "otlp_spans_exported_total", "Spans successfully exported to the OTLP collector.", m.otlpSpansOut.Load())
	writeScalarCounter(w, "otlp_spans_dropped_total", "Spans dropped because the export queue was full.", m.otlpSpansDropped.Load())
	m.dashSourceErrors.write(w)

	// Gauges.
	gaugeHeader(w, "build_info", "Build metadata; value is always 1.")
	gaugeLine(w, "build_info", []string{"version"}, []string{m.version}, 1)
	m.inflight.write(w)

	var schedSnaps []AgentSchedSnapshot
	if m.sched != nil {
		schedSnaps = m.sched.Snapshot()
	}
	gaugeHeader(w, "sched_queue_depth", "Queued slot requests by agent and class (scrape-time).")
	for _, snap := range schedSnaps {
		nSync, nTask := 0, 0
		for _, wtr := range snap.Waiting {
			if wtr.Kind == "sync" {
				nSync++
			} else {
				nTask++
			}
		}
		gaugeLine(w, "sched_queue_depth", []string{"agent", "class"}, []string{snap.Agent, "sync"}, int64(nSync))
		gaugeLine(w, "sched_queue_depth", []string{"agent", "class"}, []string{snap.Agent, "task"}, int64(nTask))
	}
	gaugeHeader(w, "sched_running", "Held scheduler slots by agent (scrape-time).")
	for _, snap := range schedSnaps {
		gaugeLine(w, "sched_running", []string{"agent"}, []string{snap.Agent}, int64(len(snap.Running)))
	}

	var tsnap TaskStoreSnapshot
	if m.store != nil {
		tsnap = m.store.Snapshot()
	}
	agents := make([]string, 0, len(m.cfg.Agents))
	for name := range m.cfg.Agents {
		agents = append(agents, name)
	}
	for name := range tsnap.ByAgent {
		if _, ok := m.cfg.Agents[name]; !ok {
			agents = append(agents, name) // orphaned agents still expose counts
		}
	}
	sort.Strings(agents)
	gaugeHeader(w, "tasks", "Task records by agent and state (scrape-time).")
	for _, a := range agents {
		for _, st := range taskStateNames {
			gaugeLine(w, "tasks", []string{"agent", "state"}, []string{a, string(st)}, int64(tsnap.ByAgent[a][st]))
		}
	}
	gaugeHeader(w, "task_oldest_pending_age_seconds", "Age of the oldest pending task by agent (scrape-time).")
	for _, a := range agents {
		gaugeLine(w, "task_oldest_pending_age_seconds", []string{"agent"}, []string{a},
			int64(tsnap.OldestPendingAge[a]/time.Second))
	}

	vmUp := make(map[string]bool)
	for _, vm := range ListVMs(m.cfg.StateDir) {
		vmUp[vm.AgentType] = true
	}
	configured := make([]string, 0, len(m.cfg.Agents))
	for name := range m.cfg.Agents {
		configured = append(configured, name)
	}
	sort.Strings(configured)
	gaugeHeader(w, "vm_up", "Whether a live VM exists for the agent (scrape-time).")
	for _, a := range configured {
		v := int64(0)
		if vmUp[a] {
			v = 1
		}
		gaugeLine(w, "vm_up", []string{"agent"}, []string{a}, v)
	}

	gaugeHeader(w, "store_degraded", "1 when some task record failed its last persist.")
	degraded := int64(0)
	if tsnap.Degraded {
		degraded = 1
	}
	gaugeLine(w, "store_degraded", nil, nil, degraded)

	// Histograms.
	m.httpDuration.write(w)
	m.proxyTTFB.write(w)
	m.schedWait.write(w)
	m.taskDuration.write(w)
}

// metricsAuthorized implements the /metrics auth rule: loopback callers are
// unauthenticated; everyone else needs a gateway or dashboard bearer
// (constant-time compare).
func metricsAuthorized(r *http.Request, cfg *Config) bool {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
			return true
		}
	}
	auth := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(auth, prefix) {
		return false
	}
	presented := strings.TrimSpace(auth[len(prefix):])
	for i := range cfg.Tokens {
		if t := cfg.Tokens[i].Token; t != "" && tokenEqual(presented, t) {
			return true
		}
	}
	for _, t := range cfg.Dashboard.Tokens {
		if t != "" && tokenEqual(presented, t) {
			return true
		}
	}
	return false
}

// tokenEqual is a constant-time string comparison.
func tokenEqual(a, b string) bool {
	return len(a) == len(b) && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// handleMetrics serves GET /metrics with the loopback-unauth rule.
func (s *server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if !metricsAuthorized(r, s.cfg) {
		s.mx.IncAuthFailure()
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	s.mx.WriteExposition(w)
}

// pathLabel normalizes a request path to a bounded metric label set.
func pathLabel(p string) string {
	switch p {
	case "/health", "/v1/capabilities", "/v1/models", "/v1/chat/completions", "/v1/tasks", "/v1/runs", "/metrics":
		return p
	}
	if strings.HasPrefix(p, "/v1/runs/") {
		rest := p[len("/v1/runs/"):]
		if i := strings.IndexByte(rest, '/'); i >= 0 {
			switch rest[i+1:] {
			case "events":
				return "/v1/runs/{id}/events"
			case "approval":
				return "/v1/runs/{id}/approval"
			case "stop":
				return "/v1/runs/{id}/stop"
			}
			return "/v1/runs/{id}/other"
		}
		return "/v1/runs/{id}"
	}
	if strings.HasPrefix(p, "/v1/tasks/") {
		rest := p[len("/v1/tasks/"):]
		if i := strings.IndexByte(rest, '/'); i >= 0 {
			switch rest[i+1:] {
			case "output":
				return "/v1/tasks/{id}/output"
			case "cancel":
				return "/v1/tasks/{id}/cancel"
			}
			return "/v1/tasks/{id}/other"
		}
		return "/v1/tasks/{id}"
	}
	if p == "/dashboard" || strings.HasPrefix(p, "/dashboard/") {
		return "/dashboard"
	}
	return "other"
}
