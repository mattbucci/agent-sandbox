package main

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// metricsTestConfig builds a one-agent config over a temp state dir.
func metricsTestConfig(t *testing.T) *Config {
	t.Helper()
	cfg := &Config{
		StateDir: t.TempDir(),
		Agents:   map[string]AgentConfig{"a1": {}},
		Tokens: []Token{
			{Name: "admin", Token: "hgw-PLACEHOLDER-TEST-TOKEN-metrics-000000000", Agents: []string{"*"}},
		},
	}
	cfg.applyDefaults()
	return cfg
}

// exposition renders the registry to a string.
func exposition(t *testing.T, m *Metrics) string {
	t.Helper()
	var b strings.Builder
	m.WriteExposition(&b)
	return b.String()
}

// family extracts one full family block (HELP line through the last sample)
// from the exposition text.
func family(t *testing.T, text, name string) string {
	t.Helper()
	start := strings.Index(text, "# HELP "+metricPrefix+name+" ")
	if start < 0 {
		t.Fatalf("family %s missing from exposition:\n%s", name, text)
	}
	rest := text[start:]
	if next := strings.Index(rest[1:], "# HELP "); next >= 0 {
		rest = rest[:next+1]
	}
	return rest
}

// TestMetricsExpositionGolden locks the text 0.0.4 shape: HELP/TYPE lines,
// pre-registered zero series, cumulative le buckets, _sum/_count.
func TestMetricsExpositionGolden(t *testing.T) {
	cfg := metricsTestConfig(t)
	m := newMetrics(cfg, "test", nil, nil)

	m.IncUpstreamError("a1", "no_vm")
	m.IncUpstreamError("a1", "no_vm")
	m.IncAuthFailure()
	m.HandleSchedEvent(SchedEvent{Type: SchedEventGranted, Agent: "a1", Class: "sync", Wait: 200 * time.Millisecond})
	m.HandleSchedEvent(SchedEvent{Type: SchedEventRejectedFull, Agent: "a1", Class: "sync"})
	m.ObserveHTTPRequest("/health", "GET", 200, 2*time.Millisecond)
	m.OnOTLPBatch("ok", 7)
	m.OnOTLPDrop(3)

	text := exposition(t, m)

	if got, want := family(t, text, "upstream_errors_total"),
		"# HELP hermes_gateway_upstream_errors_total Downstream VM request failures by agent and reason.\n"+
			"# TYPE hermes_gateway_upstream_errors_total counter\n"+
			"hermes_gateway_upstream_errors_total{agent=\"a1\",reason=\"connect\"} 0\n"+
			"hermes_gateway_upstream_errors_total{agent=\"a1\",reason=\"no_vm\"} 2\n"+
			"hermes_gateway_upstream_errors_total{agent=\"a1\",reason=\"status_5xx\"} 0\n"; got != want {
		t.Fatalf("upstream_errors_total:\n got: %q\nwant: %q", got, want)
	}

	if got, want := family(t, text, "auth_failures_total"),
		"# HELP hermes_gateway_auth_failures_total Requests rejected for a missing or invalid bearer token.\n"+
			"# TYPE hermes_gateway_auth_failures_total counter\n"+
			"hermes_gateway_auth_failures_total 1\n"; got != want {
		t.Fatalf("auth_failures_total:\n got: %q\nwant: %q", got, want)
	}

	if got, want := family(t, text, "sched_wait_seconds"),
		"# HELP hermes_gateway_sched_wait_seconds Scheduler queue wait before a grant, by agent.\n"+
			"# TYPE hermes_gateway_sched_wait_seconds histogram\n"+
			"hermes_gateway_sched_wait_seconds_bucket{agent=\"a1\",le=\"0.01\"} 0\n"+
			"hermes_gateway_sched_wait_seconds_bucket{agent=\"a1\",le=\"0.1\"} 0\n"+
			"hermes_gateway_sched_wait_seconds_bucket{agent=\"a1\",le=\"0.5\"} 1\n"+
			"hermes_gateway_sched_wait_seconds_bucket{agent=\"a1\",le=\"1\"} 1\n"+
			"hermes_gateway_sched_wait_seconds_bucket{agent=\"a1\",le=\"5\"} 1\n"+
			"hermes_gateway_sched_wait_seconds_bucket{agent=\"a1\",le=\"15\"} 1\n"+
			"hermes_gateway_sched_wait_seconds_bucket{agent=\"a1\",le=\"60\"} 1\n"+
			"hermes_gateway_sched_wait_seconds_bucket{agent=\"a1\",le=\"300\"} 1\n"+
			"hermes_gateway_sched_wait_seconds_bucket{agent=\"a1\",le=\"900\"} 1\n"+
			"hermes_gateway_sched_wait_seconds_bucket{agent=\"a1\",le=\"+Inf\"} 1\n"+
			"hermes_gateway_sched_wait_seconds_sum{agent=\"a1\"} 0.2\n"+
			"hermes_gateway_sched_wait_seconds_count{agent=\"a1\"} 1\n"; got != want {
		t.Fatalf("sched_wait_seconds:\n got: %q\nwant: %q", got, want)
	}

	if got, want := family(t, text, "sched_rejected_total"),
		"# HELP hermes_gateway_sched_rejected_total Scheduler slot rejections by agent and reason.\n"+
			"# TYPE hermes_gateway_sched_rejected_total counter\n"+
			"hermes_gateway_sched_rejected_total{agent=\"a1\",reason=\"client_gone\"} 0\n"+
			"hermes_gateway_sched_rejected_total{agent=\"a1\",reason=\"queue_full\"} 1\n"+
			"hermes_gateway_sched_rejected_total{agent=\"a1\",reason=\"wait_timeout\"} 0\n"; got != want {
		t.Fatalf("sched_rejected_total:\n got: %q\nwant: %q", got, want)
	}

	// http counters + duration histogram carry the observed request.
	for _, line := range []string{
		"hermes_gateway_http_requests_total{path=\"/health\",method=\"GET\",code=\"200\"} 1\n",
		"hermes_gateway_http_request_duration_seconds_bucket{path=\"/health\",le=\"0.01\"} 1\n",
		"hermes_gateway_http_request_duration_seconds_count{path=\"/health\"} 1\n",
		"hermes_gateway_otlp_export_batches_total{outcome=\"error\"} 0\n",
		"hermes_gateway_otlp_export_batches_total{outcome=\"ok\"} 1\n",
		"hermes_gateway_otlp_spans_exported_total 7\n",
		"hermes_gateway_otlp_spans_dropped_total 3\n",
		"hermes_gateway_dashboard_source_errors_total{source=\"squid\"} 0\n",
		"hermes_gateway_dashboard_source_errors_total{source=\"traces\"} 0\n",
		"hermes_gateway_build_info{version=\"test\"} 1\n",
	} {
		if !strings.Contains(text, line) {
			t.Fatalf("exposition missing %q:\n%s", line, text)
		}
	}
}

// TestMetricsLabelEscaping: backslash, quote and newline are escaped per the
// text format.
func TestMetricsLabelEscaping(t *testing.T) {
	if got, want := escapeLabelValue("a\"b\\c\nd"), `a\"b\\c\nd`; got != want {
		t.Fatalf("escapeLabelValue = %q, want %q", got, want)
	}
	cfg := metricsTestConfig(t)
	m := newMetrics(cfg, "test", nil, nil)
	m.IncUpstreamError("we\"ird\\agent\n", "no_vm")
	text := exposition(t, m)
	want := "hermes_gateway_upstream_errors_total{agent=\"we\\\"ird\\\\agent\\n\",reason=\"no_vm\"} 1\n"
	if !strings.Contains(text, want) {
		t.Fatalf("escaped series missing %q in:\n%s", want, family(t, text, "upstream_errors_total"))
	}
}

// TestMetricsScrapeTimeGauges: sched/tasks/vm_up/store_degraded are computed
// from live Snapshot()s at scrape time.
func TestMetricsScrapeTimeGauges(t *testing.T) {
	cfg := metricsTestConfig(t)
	sched := NewScheduler(cfg)
	store, err := NewTaskStore(cfg)
	if err != nil {
		t.Fatalf("NewTaskStore: %v", err)
	}
	m := newMetrics(cfg, "test", sched, store)

	// One running slot + one queued sync waiter (limit defaults to 1).
	release, err := sched.Acquire(context.Background(), "a1", classSync, SlotMeta{})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer release()
	waiterCtx, cancelWaiter := context.WithCancel(context.Background())
	defer cancelWaiter()
	waiterDone := make(chan struct{})
	go func() {
		defer close(waiterDone)
		rel, aerr := sched.Acquire(waiterCtx, "a1", classSync, SlotMeta{})
		if aerr == nil {
			rel()
		}
	}()
	waitForWaiting(t, sched, "a1", 1)

	// Two pending tasks and a live VM.
	for i := 0; i < 2; i++ {
		if _, err := store.Submit(SubmitSpec{Agent: "a1", Request: []byte(testMessages)}); err != nil {
			t.Fatalf("Submit: %v", err)
		}
	}
	writeVMInfo(t, cfg.StateDir, "a1")

	text := exposition(t, m)
	for _, line := range []string{
		"hermes_gateway_sched_queue_depth{agent=\"a1\",class=\"sync\"} 1\n",
		"hermes_gateway_sched_queue_depth{agent=\"a1\",class=\"task\"} 0\n",
		"hermes_gateway_sched_running{agent=\"a1\"} 1\n",
		"hermes_gateway_tasks{agent=\"a1\",state=\"pending\"} 2\n",
		"hermes_gateway_tasks{agent=\"a1\",state=\"running\"} 0\n",
		"hermes_gateway_task_oldest_pending_age_seconds{agent=\"a1\"} 0\n",
		"hermes_gateway_vm_up{agent=\"a1\"} 1\n",
		"hermes_gateway_store_degraded 0\n",
	} {
		if !strings.Contains(text, line) {
			t.Fatalf("exposition missing %q:\n%s", line, text)
		}
	}

	cancelWaiter()
	<-waiterDone
	release()
	text = exposition(t, m)
	for _, line := range []string{
		"hermes_gateway_sched_queue_depth{agent=\"a1\",class=\"sync\"} 0\n",
		"hermes_gateway_sched_running{agent=\"a1\"} 0\n",
	} {
		if !strings.Contains(text, line) {
			t.Fatalf("post-release exposition missing %q", line)
		}
	}
}

// TestMetricsEndpointAuth: loopback scrapes are unauthenticated; LAN callers
// need a gateway or dashboard bearer.
func TestMetricsEndpointAuth(t *testing.T) {
	cfg := metricsTestConfig(t)
	cfg.Dashboard.Tokens = []string{"hgwd_PLACEHOLDER-TEST-DASH-TOKEN-00000000"}
	sched := NewScheduler(cfg)
	m := newMetrics(cfg, "test", sched, nil)
	s := &server{cfg: cfg, sched: sched, mx: m}

	do := func(remoteAddr, bearer string) (int, string, string) {
		req := httptest.NewRequest("GET", "/metrics", nil)
		req.RemoteAddr = remoteAddr
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		rec := httptest.NewRecorder()
		s.handleMetrics(rec, req)
		return rec.Code, rec.Header().Get("Content-Type"), rec.Body.String()
	}

	// Loopback (v4 and v6): no auth needed.
	for _, addr := range []string{"127.0.0.1:52801", "[::1]:52801"} {
		code, ct, body := do(addr, "")
		if code != 200 {
			t.Fatalf("loopback %s = %d", addr, code)
		}
		if ct != "text/plain; version=0.0.4; charset=utf-8" {
			t.Fatalf("content-type = %q", ct)
		}
		if !strings.Contains(body, "hermes_gateway_build_info") {
			t.Fatalf("loopback scrape body missing metrics")
		}
	}

	// LAN without a token: 401.
	if code, _, _ := do("192.168.7.20:40000", ""); code != 401 {
		t.Fatalf("LAN unauth = %d, want 401", code)
	}
	// LAN with a wrong token: 401.
	if code, _, _ := do("192.168.7.20:40000", "hgw-PLACEHOLDER-TEST-TOKEN-wrong"); code != 401 {
		t.Fatalf("LAN wrong token = %d, want 401", code)
	}
	// LAN with the gateway bearer: 200.
	if code, _, _ := do("192.168.7.20:40000", "hgw-PLACEHOLDER-TEST-TOKEN-metrics-000000000"); code != 200 {
		t.Fatalf("LAN gateway token rejected")
	}
	// LAN with the dashboard bearer: 200.
	if code, _, _ := do("192.168.7.20:40000", "hgwd_PLACEHOLDER-TEST-DASH-TOKEN-00000000"); code != 200 {
		t.Fatalf("LAN dashboard token rejected")
	}
}

// TestPathLabel bounds the path label cardinality.
func TestPathLabel(t *testing.T) {
	cases := map[string]string{
		"/health":               "/health",
		"/v1/chat/completions":  "/v1/chat/completions",
		"/v1/tasks":             "/v1/tasks",
		"/v1/tasks/t-1":         "/v1/tasks/{id}",
		"/v1/tasks/t-1/output":  "/v1/tasks/{id}/output",
		"/v1/tasks/t-1/cancel":  "/v1/tasks/{id}/cancel",
		"/v1/tasks/t-1/bogus":   "/v1/tasks/{id}/other",
		"/metrics":              "/metrics",
		"/dashboard":            "/dashboard",
		"/dashboard/api/x":      "/dashboard",
		"/totally/unknown/path": "other",
	}
	for in, want := range cases {
		if got := pathLabel(in); got != want {
			t.Fatalf("pathLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestMetricsNilSafety: every instrumentation entry point must be a no-op on
// a nil registry (handlers run without metrics in older tests).
func TestMetricsNilSafety(t *testing.T) {
	var m *Metrics
	m.ObserveHTTPRequest("/health", "GET", 200, time.Millisecond)
	m.InflightAdd("/health", 1)
	m.IncAuthFailure()
	m.IncUpstreamError("a", "no_vm")
	m.AddStreamBytes("a", 1)
	m.ObserveProxyTTFB("a", time.Millisecond)
	m.HandleSchedEvent(SchedEvent{Type: SchedEventGranted})
	m.HandleTaskEvent(TaskEvent{})
	m.OnOTLPBatch("ok", 1)
	m.OnOTLPDrop(1)
	m.IncDashboardSourceError("traces")

	var h *History
	h.Observe("a", time.Millisecond, false)
	h.SampleGauges(nil)
	_ = h.Snapshot()
}
