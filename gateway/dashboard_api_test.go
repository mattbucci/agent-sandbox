package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Placeholder dashboard bearer (never a real secret).
const dashTestToken = "hgwd_PLACEHOLDER-TEST-DASH-TOKEN-000000000000"

// dashTestConfig builds a two-agent config whose observability files point
// into an (empty) temp dir so both data sources are absent by default.
func dashTestConfig(t *testing.T) *Config {
	t.Helper()
	cfg := apiTestConfig(t)
	cfg.Dashboard.Tokens = []string{dashTestToken}
	cfg.Observability.TracesFile = filepath.Join(t.TempDir(), "traces.jsonl")
	cfg.Observability.SquidAccessLog = filepath.Join(t.TempDir(), "access.log")
	return cfg
}

// newDashServer registers the dashboard (via the overridden registerDashboard
// var) on a bare mux and serves it. store may be nil.
func newDashServer(t *testing.T, cfg *Config, store *TaskStore) (*DashDeps, *httptest.Server) {
	t.Helper()
	deps := &DashDeps{
		Cfg:       cfg,
		Sched:     NewScheduler(cfg),
		Store:     store,
		Hist:      newHistory(),
		Tracer:    newTracer("", 1.0, "test"),
		StartedAt: time.Now().Add(-90 * time.Second),
		Version:   "test",
	}
	mux := http.NewServeMux()
	registerDashboard(mux, deps)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return deps, ts
}

// dashStore builds a TaskStore over a temp dir for dashboard tests.
func dashStore(t *testing.T, cfg *Config) *TaskStore {
	t.Helper()
	st, err := NewTaskStore(cfg)
	if err != nil {
		t.Fatalf("NewTaskStore: %v", err)
	}
	return st
}

func TestDashboardAuthFailClosedOnEmptyTokens(t *testing.T) {
	cfg := dashTestConfig(t)
	cfg.Dashboard.Tokens = nil
	_, ts := newDashServer(t, cfg, nil)

	for _, token := range []string{"", adminToken, dashTestToken} {
		resp, body := apiDo(t, http.MethodGet, ts.URL+"/dashboard/api/overview", token, nil, nil)
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("token %q: status = %d, want 403 (body %s)", token, resp.StatusCode, body)
		}
		if !strings.Contains(string(body), "dashboard token not configured") {
			t.Fatalf("fail-closed message missing: %s", body)
		}
	}
}

func TestDashboardAuthBadToken(t *testing.T) {
	cfg := dashTestConfig(t)
	_, ts := newDashServer(t, cfg, nil)

	// Missing and wrong bearers: 401. Gateway tokens are NOT dashboard tokens.
	for _, token := range []string{"", "wrong-token", adminToken} {
		resp, body := apiDo(t, http.MethodGet, ts.URL+"/dashboard/api/overview", token, nil, nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("token %q: status = %d, want 401 (body %s)", token, resp.StatusCode, body)
		}
	}
	// The configured dashboard bearer: 200.
	resp, body := apiDo(t, http.MethodGet, ts.URL+"/dashboard/api/overview", dashTestToken, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("valid token rejected: %d (body %s)", resp.StatusCode, body)
	}
}

func TestDashboardDisabledIs404(t *testing.T) {
	cfg := dashTestConfig(t)
	cfg.Dashboard.Enabled = boolPtr(false)
	_, ts := newDashServer(t, cfg, nil)

	for _, path := range []string{"/dashboard", "/dashboard/", "/dashboard/static/app.js", "/dashboard/api/overview"} {
		resp, _ := apiDo(t, http.MethodGet, ts.URL+path, dashTestToken, nil, nil)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s: status = %d, want 404 when disabled", path, resp.StatusCode)
		}
	}
}

func TestDashboardSourcesUnavailable(t *testing.T) {
	cfg := dashTestConfig(t) // traces file + squid log point at absent files
	_, ts := newDashServer(t, cfg, nil)

	resp, body := apiDo(t, http.MethodGet, ts.URL+"/dashboard/api/traces", dashTestToken, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("traces status = %d, want 200", resp.StatusCode)
	}
	var tr tracesResult
	if err := json.Unmarshal(body, &tr); err != nil {
		t.Fatalf("traces body: %v (%s)", err, body)
	}
	if tr.Available || tr.Detail == "" {
		t.Fatalf("traces must degrade to available:false with detail: %+v", tr)
	}

	resp, body = apiDo(t, http.MethodGet, ts.URL+"/dashboard/api/egress", dashTestToken, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("egress status = %d, want 200", resp.StatusCode)
	}
	var eg dashEgress
	if err := json.Unmarshal(body, &eg); err != nil {
		t.Fatalf("egress body: %v (%s)", err, body)
	}
	if eg.Available || eg.Detail == "" {
		t.Fatalf("egress must degrade to available:false with detail: %+v", eg)
	}

	// Tasks endpoints with no store: still 200, available:false.
	resp, body = apiDo(t, http.MethodGet, ts.URL+"/dashboard/api/tasks", dashTestToken, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("tasks status = %d, want 200", resp.StatusCode)
	}
	var tl dashTaskList
	if err := json.Unmarshal(body, &tl); err != nil {
		t.Fatalf("tasks body: %v (%s)", err, body)
	}
	if tl.Available {
		t.Fatalf("tasks must be available:false without a store: %+v", tl)
	}

	// The overview keeps working and reports the deps as down.
	resp, body = apiDo(t, http.MethodGet, ts.URL+"/dashboard/api/overview", dashTestToken, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("overview status = %d", resp.StatusCode)
	}
	var ov dashOverview
	if err := json.Unmarshal(body, &ov); err != nil {
		t.Fatalf("overview body: %v", err)
	}
	for _, dep := range []string{"traces_file", "squid_log", "tasks_dir"} {
		if ov.Deps[dep].OK {
			t.Fatalf("dep %s should be down: %+v", dep, ov.Deps[dep])
		}
	}
}

func TestDashboardStaticServingNoTraversal(t *testing.T) {
	cfg := dashTestConfig(t)
	_, ts := newDashServer(t, cfg, nil)

	// The shell and assets are served unauthenticated.
	for path, ct := range map[string]string{
		"/dashboard/":                 "text/html",
		"/dashboard/static/app.js":    "text/javascript",
		"/dashboard/static/charts.js": "text/javascript",
		"/dashboard/static/style.css": "text/css",
	} {
		resp, body := apiDo(t, http.MethodGet, ts.URL+path, "", nil, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200", path, resp.StatusCode)
		}
		if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, ct) {
			t.Fatalf("%s: content-type = %q, want prefix %q", path, got, ct)
		}
		if resp.Header.Get("Cache-Control") != "no-cache" {
			t.Fatalf("%s: missing Cache-Control: no-cache", path)
		}
		if len(body) == 0 {
			t.Fatalf("%s: empty body", path)
		}
	}

	// /dashboard redirects to /dashboard/.
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Get(ts.URL + "/dashboard")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/dashboard/" {
		t.Fatalf("redirect = %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}

	// Path traversal attempts never yield 200 (mux cleaning or the handler's
	// name check stops them; embed.FS would refuse anyway). Raw paths are
	// exercised straight against the mux so no client normalization applies.
	mux := http.NewServeMux()
	registerDashboard(mux, &DashDeps{Cfg: cfg, Hist: newHistory(), StartedAt: time.Now(), Version: "test"})
	for _, raw := range []string{
		"/dashboard/static/../dashboard.go",
		"/dashboard/static/../../go.mod",
		"/dashboard/static/..%2f..%2fgo.mod",
		"/dashboard/static/%2e%2e/%2e%2e/go.mod",
		"/dashboard/static/sub/thing.js",
	} {
		req := httptest.NewRequest(http.MethodGet, raw, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code == http.StatusOK {
			t.Fatalf("%s: served with 200 (body starts %q)", raw, rec.Body.String()[:min(40, rec.Body.Len())])
		}
	}
}

func TestDashboardOverviewShape(t *testing.T) {
	cfg := dashTestConfig(t)
	store := dashStore(t, cfg)
	if _, err := store.Submit(SubmitSpec{
		Agent:   "feature-dev",
		Request: json.RawMessage(`{"messages":[{"role":"user","content":"hello"}]}`),
	}); err != nil {
		t.Fatal(err)
	}
	deps, ts := newDashServer(t, cfg, store)
	deps.Hist.Observe("feature-dev", 120*time.Millisecond, false)

	resp, body := apiDo(t, http.MethodGet, ts.URL+"/dashboard/api/overview", dashTestToken, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d (%s)", resp.StatusCode, body)
	}
	var ov dashOverview
	if err := json.Unmarshal(body, &ov); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	if ov.Gateway.Version != "test" || ov.Gateway.UptimeS < 60 || ov.Gateway.PID == 0 {
		t.Fatalf("gateway block wrong: %+v", ov.Gateway)
	}
	if len(ov.Agents) != 2 || ov.Agents[0].Agent != "debugger" || ov.Agents[1].Agent != "feature-dev" {
		t.Fatalf("agents wrong (want sorted): %+v", ov.Agents)
	}
	if ov.Agents[1].Limit != 1 || ov.Agents[1].QueueCap != 4 {
		t.Fatalf("limits wrong: %+v", ov.Agents[1])
	}
	if ov.Agents[0].Running == nil || ov.Agents[0].Waiting == nil {
		t.Fatal("running/waiting must be [] not null")
	}
	if ov.TasksByState["pending"] != 1 {
		t.Fatalf("tasks_by_state = %+v", ov.TasksByState)
	}
	if _, ok := ov.TasksByState["orphaned"]; !ok {
		t.Fatal("orphaned count missing")
	}
	if ov.Totals.Reqs1m != 1 {
		t.Fatalf("totals.reqs_1m = %d, want 1", ov.Totals.Reqs1m)
	}
	if !ov.Deps["tasks_dir"].OK {
		t.Fatalf("tasks_dir should be ok: %+v", ov.Deps["tasks_dir"])
	}
	// Export disabled ("" endpoint) is not an error condition.
	if coll := ov.Deps["collector"]; !coll.OK || coll.Detail != "span export disabled" {
		t.Fatalf("collector dep = %+v", coll)
	}
}

func TestDashboardTimeseriesShape(t *testing.T) {
	cfg := dashTestConfig(t)
	deps, ts := newDashServer(t, cfg, nil)
	deps.Hist.Observe("feature-dev", 80*time.Millisecond, true)

	resp, body := apiDo(t, http.MethodGet, ts.URL+"/dashboard/api/timeseries?window_s=600", dashTestToken, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var out dashTimeseries
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	if out.StepS != 10 || out.Buckets != 60 {
		t.Fatalf("step/buckets = %d/%d, want 10/60", out.StepS, out.Buckets)
	}
	// _total plus every configured agent, all zero-filled to fixed length.
	for _, key := range []string{"_total", "feature-dev", "debugger"} {
		s, ok := out.Series[key]
		if !ok {
			t.Fatalf("series %q missing", key)
		}
		if len(s.Count) != 60 || len(s.Errors) != 60 || len(s.LatMsAvg) != 60 || len(s.LatMsP95) != 60 {
			t.Fatalf("series %q not fixed-length: %d", key, len(s.Count))
		}
	}
	// The observation landed in the newest bucket.
	tot := out.Series["_total"]
	if tot.Count[59] != 1 || tot.Errors[59] != 1 {
		t.Fatalf("newest bucket = count %d errors %d, want 1/1", tot.Count[59], tot.Errors[59])
	}
	for _, agent := range []string{"feature-dev", "debugger"} {
		if len(out.Gauges.QueueDepth[agent]) != 60 || len(out.Gauges.Running[agent]) != 60 {
			t.Fatalf("gauges for %q not fixed-length", agent)
		}
	}
}

func TestDashboardTasksFlow(t *testing.T) {
	cfg := dashTestConfig(t)
	store := dashStore(t, cfg)
	task, err := store.Submit(SubmitSpec{
		Agent:   "feature-dev",
		Request: json.RawMessage(`{"messages":[{"role":"system","content":"be nice"},{"role":"user","content":"hello preview"}]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, ts := newDashServer(t, cfg, store)

	// List sees the task across all agents (ops-privileged token).
	resp, body := apiDo(t, http.MethodGet, ts.URL+"/dashboard/api/tasks", dashTestToken, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d", resp.StatusCode)
	}
	var list dashTaskList
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatal(err)
	}
	if !list.Available || len(list.Data) != 1 || list.Data[0].ID != task.ID {
		t.Fatalf("list = %+v", list)
	}

	// Detail carries request_preview (last user message).
	resp, body = apiDo(t, http.MethodGet, ts.URL+"/dashboard/api/tasks/"+task.ID, dashTestToken, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("detail status = %d", resp.StatusCode)
	}
	var detail struct {
		Available      bool   `json:"available"`
		ID             string `json:"id"`
		RequestPreview string `json:"request_preview"`
	}
	if err := json.Unmarshal(body, &detail); err != nil {
		t.Fatal(err)
	}
	if !detail.Available || detail.ID != task.ID || detail.RequestPreview != "hello preview" {
		t.Fatalf("detail = %+v", detail)
	}

	// Output spool (absent file => empty 200 text/plain).
	resp, body = apiDo(t, http.MethodGet, ts.URL+"/dashboard/api/tasks/"+task.ID+"/output", dashTestToken, nil, nil)
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain") {
		t.Fatalf("output = %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if len(body) != 0 {
		t.Fatalf("output body = %q, want empty", body)
	}

	// Cancel is POST-only and mirrors /v1 semantics.
	resp, _ = apiDo(t, http.MethodGet, ts.URL+"/dashboard/api/tasks/"+task.ID+"/cancel", dashTestToken, nil, nil)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET cancel = %d, want 405", resp.StatusCode)
	}
	resp, body = apiDo(t, http.MethodPost, ts.URL+"/dashboard/api/tasks/"+task.ID+"/cancel", dashTestToken, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cancel = %d (%s)", resp.StatusCode, body)
	}
	var cancelled Task
	if err := json.Unmarshal(body, &cancelled); err != nil {
		t.Fatal(err)
	}
	if cancelled.State != TaskCancelled {
		t.Fatalf("state = %s, want cancelled", cancelled.State)
	}
	// Idempotent second cancel.
	resp, body = apiDo(t, http.MethodPost, ts.URL+"/dashboard/api/tasks/"+task.ID+"/cancel", dashTestToken, nil, nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"already_terminal":true`) {
		t.Fatalf("second cancel = %d (%s)", resp.StatusCode, body)
	}

	// Unknown task id.
	resp, _ = apiDo(t, http.MethodGet, ts.URL+"/dashboard/api/tasks/t-nope", dashTestToken, nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("missing task = %d, want 404", resp.StatusCode)
	}
}

func TestDashboardEgressAggregation(t *testing.T) {
	cfg := dashTestConfig(t)
	// Point the squid log at a real fixture; anchor "now" is squidFixtureNow,
	// so use a wide window relative to wall clock: rewrite fixture times to be
	// recent instead.
	dir := t.TempDir()
	logPath := filepath.Join(dir, "access.log")
	now := time.Now().UTC()
	line := func(offset time.Duration, client, code string, nBytes int64, method, url string) string {
		ts := now.Add(offset)
		return fmt.Sprintf("%.3f 145 %s %s %d %s %s - HIER_DIRECT/1.2.3.4 -",
			float64(ts.UnixNano())/1e9, client, code, nBytes, method, url)
	}
	content := line(-2*time.Minute, "10.0.2.2", "TCP_TUNNEL/200", 1000, "CONNECT", "github.com:443") + "\n" +
		line(-1*time.Minute, "10.0.2.2", "TCP_DENIED/403", 0, "CONNECT", "evil.example:443") + "\n" +
		line(-30*time.Second, "10.0.2.9", "TCP_MISS/200", 500, "GET", "http://example.com/x") + "\n"
	if err := os.WriteFile(logPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg.Observability.SquidAccessLog = logPath
	_, ts := newDashServer(t, cfg, nil)

	resp, body := apiDo(t, http.MethodGet, ts.URL+"/dashboard/api/egress?window_s=900", dashTestToken, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var eg dashEgress
	if err := json.Unmarshal(body, &eg); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	if !eg.Available {
		t.Fatalf("egress unavailable: %s", eg.Detail)
	}
	if eg.Totals.Requests != 3 || eg.Totals.Denied != 1 || eg.Totals.Bytes != 1500 {
		t.Fatalf("totals = %+v", eg.Totals)
	}
	if len(eg.ByAgent) != 2 {
		t.Fatalf("by_agent = %+v", eg.ByAgent)
	}
	// No live VMs in the temp state dir: both client IPs map to "unknown".
	for _, a := range eg.ByAgent {
		if a.Agent != "unknown" {
			t.Fatalf("agent mapping without VMs = %+v", a)
		}
	}
	if len(eg.Denied) != 1 || eg.Denied[0].Host != "evil.example" || eg.Denied[0].Method != "CONNECT" {
		t.Fatalf("denied list = %+v", eg.Denied)
	}
	if len(eg.TopHosts) != 3 {
		t.Fatalf("top_hosts = %+v", eg.TopHosts)
	}
}
