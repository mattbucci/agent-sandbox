package main

// Dashboard JSON API (plan item 19, contracts §b): overview, timeseries,
// tasks (+detail/output/cancel), traces, egress. Data-source failures are
// HTTP 200 + available:false — never 5xx and never a routing impact; auth
// failures are 401/403 (dashboard.go). Source read errors also feed the
// hermes_gateway_dashboard_source_errors_total counter via gwMetrics
// (DashDeps is frozen without a Metrics field).

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Dashboard list/window bounds.
const (
	dashTasksDefaultLimit  = 100
	dashTracesDefaultLimit = 50
	dashTracesMaxLimit     = 500
	dashTracesWindowS      = 900
	dashEgressWindowS      = 900
	dashMaxWindowS         = 86400
	// requestPreviewBytes caps the task detail's request_preview.
	requestPreviewBytes = 2048
	// collectorStaleAfter is how old the last successful OTLP export may be
	// before the collector dep is reported not-ok. Spans only flow with /v1
	// traffic, so this is generous (judgment call; plan is silent).
	collectorStaleAfter = 5 * time.Minute
	// deniedListCap bounds the egress panel's denied list.
	deniedListCap = 50
	// topHostsCap bounds the egress panel's host table.
	topHostsCap = 20
)

// handleAPI dispatches /dashboard/api/<rest> (manual go1.21-safe routing).
func (dash *dashboard) handleAPI(w http.ResponseWriter, r *http.Request, rest string) {
	switch {
	case rest == "overview":
		dash.getOnly(w, r, dash.handleOverview)
	case rest == "timeseries":
		dash.getOnly(w, r, dash.handleTimeseries)
	case rest == "tasks":
		dash.getOnly(w, r, dash.handleTasksList)
	case strings.HasPrefix(rest, "tasks/"):
		dash.handleTaskItem(w, r, strings.TrimPrefix(rest, "tasks/"))
	case rest == "traces":
		dash.getOnly(w, r, dash.handleTraces)
	case rest == "egress":
		dash.getOnly(w, r, dash.handleEgress)
	default:
		writeError(w, http.StatusNotFound, "No such dashboard endpoint", "invalid_request_error")
	}
}

// getOnly rejects non-GET methods with the standard 405 envelope.
func (dash *dashboard) getOnly(w http.ResponseWriter, r *http.Request, h func(http.ResponseWriter, *http.Request)) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "Method not allowed", "invalid_request_error")
		return
	}
	h(w, r)
}

// intQuery parses an integer query param with a default and clamped range.
func intQuery(r *http.Request, name string, def, min, max int) int {
	v := r.URL.Query().Get(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	if n < min {
		n = min
	}
	if n > max {
		n = max
	}
	return n
}

// --- overview -------------------------------------------------------------

// dashDepStatus reports one external dependency in the overview.
type dashDepStatus struct {
	OK        bool       `json:"ok"`
	Detail    string     `json:"detail,omitempty"`
	Path      string     `json:"path,omitempty"`
	SizeBytes int64      `json:"size_bytes,omitempty"`
	Mtime     *time.Time `json:"mtime,omitempty"`
}

// dashGatewayInfo is the overview's gateway block.
type dashGatewayInfo struct {
	StartedAt time.Time `json:"started_at"`
	UptimeS   int64     `json:"uptime_s"`
	PID       int       `json:"pid"`
	Version   string    `json:"version"`
}

// dashVMInfo is the per-agent VM block (null when no live VM exists).
type dashVMInfo struct {
	InstanceID string `json:"instance_id"`
	VMIP       string `json:"vm_ip"`
	Alive      bool   `json:"alive"`
	StartedAt  string `json:"started_at,omitempty"`
}

// dashRunning is one held slot in the overview.
type dashRunning struct {
	RunID     string    `json:"run_id"`
	Kind      string    `json:"kind"`
	TaskID    string    `json:"task_id,omitempty"`
	SessionID string    `json:"session_id,omitempty"`
	TraceID   string    `json:"trace_id,omitempty"`
	StartedAt time.Time `json:"started_at"`
	AgeS      int64     `json:"age_s"`
}

// dashWaiting is one queued slot request in the overview.
type dashWaiting struct {
	ID         string    `json:"id"`
	Kind       string    `json:"kind"`
	TaskID     string    `json:"task_id,omitempty"`
	Priority   int       `json:"priority"`
	EnqueuedAt time.Time `json:"enqueued_at"`
	WaitS      int64     `json:"wait_s"`
}

// dashSchedCounters mirrors SchedCounters with wire names.
type dashSchedCounters struct {
	Granted       uint64 `json:"granted"`
	RejectedFull  uint64 `json:"rejected_full"`
	WaitTimeouts  uint64 `json:"wait_timeouts"`
	WaitCancelled uint64 `json:"wait_cancelled"`
}

// dashLastError is the most recent task error observed for an agent
// (judgment call: the frozen DashDeps expose no sync-path error feed, so the
// task store is the source; status carries the error kind).
type dashLastError struct {
	At      time.Time `json:"at"`
	Status  string    `json:"status"`
	Message string    `json:"message"`
}

// dashAgent is one agent's admission + VM state in the overview.
type dashAgent struct {
	Agent     string            `json:"agent"`
	VM        *dashVMInfo       `json:"vm"`
	Limit     int               `json:"limit"`
	QueueCap  int               `json:"queue_cap"`
	Running   []dashRunning     `json:"running"`
	Waiting   []dashWaiting     `json:"waiting"`
	Counters  dashSchedCounters `json:"counters"`
	LastError *dashLastError    `json:"last_error"`
}

// dashTotals is the overview's last-minute traffic block.
type dashTotals struct {
	Reqs1m   uint64  `json:"reqs_1m"`
	Errors1m uint64  `json:"errors_1m"`
	P95Ms1m  float64 `json:"p95_ms_1m"`
}

// dashOverview is the GET /dashboard/api/overview payload (§b, 2s poll).
type dashOverview struct {
	Now           time.Time                `json:"now"`
	Gateway       dashGatewayInfo          `json:"gateway"`
	Deps          map[string]dashDepStatus `json:"deps"`
	Agents        []dashAgent              `json:"agents"`
	TasksByState  map[string]int           `json:"tasks_by_state"`
	StoreDegraded bool                     `json:"store_degraded"`
	Totals        dashTotals               `json:"totals"`
}

// vmInfoExtra decodes the started_at field the host tooling writes into
// info.json (VMInfo is frozen without it).
type vmInfoExtra struct {
	StartedAt string `json:"started_at"`
}

// vmStartedAt reads state/vms/<id>/info.json for its started_at (best-effort).
func vmStartedAt(stateDir, instanceID string) string {
	data, err := os.ReadFile(filepath.Join(stateDir, "vms", instanceID, "info.json"))
	if err != nil {
		return ""
	}
	var extra vmInfoExtra
	if json.Unmarshal(data, &extra) != nil {
		return ""
	}
	return extra.StartedAt
}

// handleOverview assembles the whole ops snapshot from in-memory state plus
// cheap stats of the external files.
func (dash *dashboard) handleOverview(w http.ResponseWriter, r *http.Request) {
	d := dash.deps
	now := time.Now().UTC()

	out := dashOverview{
		Now: now,
		Gateway: dashGatewayInfo{
			StartedAt: d.StartedAt.UTC(),
			UptimeS:   int64(now.Sub(d.StartedAt) / time.Second),
			PID:       os.Getpid(),
			Version:   d.Version,
		},
		Deps:         make(map[string]dashDepStatus, 4),
		Agents:       []dashAgent{},
		TasksByState: make(map[string]int, len(taskStateNames)+1),
	}

	// Dependency dots.
	out.Deps["collector"] = dash.collectorStatus(now)
	out.Deps["traces_file"] = fileDepStatus(d.Cfg.Observability.TracesFile)
	out.Deps["squid_log"] = fileDepStatus(d.Cfg.Observability.SquidAccessLog)
	out.Deps["tasks_dir"] = dash.tasksDirStatus()

	// Task aggregates.
	for _, st := range taskStateNames {
		out.TasksByState[string(st)] = 0
	}
	out.TasksByState["orphaned"] = 0
	if d.Store != nil {
		tsnap := d.Store.Snapshot()
		for st, n := range tsnap.ByState {
			out.TasksByState[string(st)] = n
		}
		out.TasksByState["orphaned"] = tsnap.Orphaned
		out.StoreDegraded = tsnap.Degraded
	}

	// Per-agent admission state + VM liveness.
	vmByAgent := make(map[string]VMInfo)
	for _, vm := range ListVMs(d.Cfg.StateDir) {
		if _, ok := vmByAgent[vm.AgentType]; !ok {
			vmByAgent[vm.AgentType] = vm
		}
	}
	var snaps []AgentSchedSnapshot
	if d.Sched != nil {
		snaps = d.Sched.Snapshot()
	}
	for _, snap := range snaps {
		a := dashAgent{
			Agent:    snap.Agent,
			Limit:    snap.Limit,
			QueueCap: snap.QueueCap,
			Running:  make([]dashRunning, 0, len(snap.Running)),
			Waiting:  make([]dashWaiting, 0, len(snap.Waiting)),
			Counters: dashSchedCounters{
				Granted:       snap.Counters.Granted,
				RejectedFull:  snap.Counters.RejectedFull,
				WaitTimeouts:  snap.Counters.WaitTimeouts,
				WaitCancelled: snap.Counters.WaitCancelled,
			},
			LastError: dash.lastTaskError(snap.Agent),
		}
		if vm, ok := vmByAgent[snap.Agent]; ok {
			a.VM = &dashVMInfo{
				InstanceID: vm.InstanceID,
				VMIP:       vm.VMIP,
				Alive:      true,
				StartedAt:  vmStartedAt(d.Cfg.StateDir, vm.InstanceID),
			}
		}
		for _, run := range snap.Running {
			a.Running = append(a.Running, dashRunning{
				RunID:     run.RunID,
				Kind:      run.Kind,
				TaskID:    run.TaskID,
				SessionID: run.SessionID,
				TraceID:   run.TraceID,
				StartedAt: run.StartedAt.UTC(),
				AgeS:      int64(now.Sub(run.StartedAt) / time.Second),
			})
		}
		for _, wtr := range snap.Waiting {
			a.Waiting = append(a.Waiting, dashWaiting{
				ID:         wtr.RunID,
				Kind:       wtr.Kind,
				TaskID:     wtr.TaskID,
				Priority:   wtr.Priority,
				EnqueuedAt: wtr.EnqueuedAt.UTC(),
				WaitS:      int64(now.Sub(wtr.EnqueuedAt) / time.Second),
			})
		}
		out.Agents = append(out.Agents, a)
	}

	// Last-minute totals from the "_total" history ring (p95 is the max
	// bucket p95 — bucket-upper-bound estimate, same basis as the charts).
	hsnap := d.Hist.Snapshot()
	if total, ok := hsnap.Series["_total"]; ok {
		lastN := 60 / historyStepS
		for i := len(total.Count) - lastN; i < len(total.Count); i++ {
			if i < 0 {
				continue
			}
			out.Totals.Reqs1m += uint64(total.Count[i])
			out.Totals.Errors1m += uint64(total.Errors[i])
			if total.LatMsP95[i] > out.Totals.P95Ms1m {
				out.Totals.P95Ms1m = total.LatMsP95[i]
			}
		}
	}

	writeJSON(w, http.StatusOK, out)
}

// collectorStatus derives the collector dot from the tracer's lastOK.
func (dash *dashboard) collectorStatus(now time.Time) dashDepStatus {
	t := dash.deps.Tracer
	if t == nil {
		return dashDepStatus{OK: false, Detail: "tracer unavailable"}
	}
	if t.endpoint == "" {
		return dashDepStatus{OK: true, Detail: "span export disabled"}
	}
	last := t.LastExportOK()
	if last.IsZero() {
		return dashDepStatus{OK: false, Detail: "no successful export yet"}
	}
	age := now.Sub(last)
	return dashDepStatus{
		OK:     age < collectorStaleAfter,
		Detail: fmt.Sprintf("last export %ds ago", int64(age/time.Second)),
	}
}

// fileDepStatus stats one read-only input file.
func fileDepStatus(path string) dashDepStatus {
	fi, err := os.Stat(path)
	if err != nil {
		return dashDepStatus{OK: false, Path: path, Detail: err.Error()}
	}
	mt := fi.ModTime().UTC()
	return dashDepStatus{OK: true, Path: path, SizeBytes: fi.Size(), Mtime: &mt}
}

// tasksDirStatus reports the task store dependency.
func (dash *dashboard) tasksDirStatus() dashDepStatus {
	dir := dash.deps.Cfg.Tasks.Dir
	if dash.deps.Store == nil {
		return dashDepStatus{OK: false, Path: dir, Detail: "task store unavailable"}
	}
	if _, err := os.Stat(dir); err != nil {
		return dashDepStatus{OK: false, Path: dir, Detail: err.Error()}
	}
	return dashDepStatus{OK: true, Path: dir}
}

// lastTaskError scans the agent's most recent tasks for the newest error.
func (dash *dashboard) lastTaskError(agent string) *dashLastError {
	if dash.deps.Store == nil {
		return nil
	}
	items, _ := dash.deps.Store.List(TaskFilter{Agent: agent, Limit: 25})
	for i := range items {
		if items[i].Error != nil {
			return &dashLastError{
				At:      items[i].UpdatedAt,
				Status:  items[i].Error.Kind,
				Message: items[i].Error.Message,
			}
		}
	}
	return nil
}

// --- timeseries -------------------------------------------------------------

// dashSeries is one traffic series in the timeseries payload.
type dashSeries struct {
	Count    []uint32  `json:"count"`
	Errors   []uint32  `json:"errors"`
	LatMsAvg []float64 `json:"lat_ms_avg"`
	LatMsP95 []float64 `json:"lat_ms_p95"`
}

// dashGaugeSeries are the sampled per-agent gauge rings.
type dashGaugeSeries struct {
	QueueDepth map[string][]uint32 `json:"queue_depth"`
	Running    map[string][]uint32 `json:"running"`
}

// dashTimeseries is the GET /dashboard/api/timeseries payload (§b): fixed-
// length zero-filled arrays, oldest first.
type dashTimeseries struct {
	StartUnix int64                 `json:"start_unix"`
	StepS     int                   `json:"step_s"`
	Buckets   int                   `json:"buckets"`
	Series    map[string]dashSeries `json:"series"`
	Gauges    dashGaugeSeries       `json:"gauges"`
}

// handleTimeseries renders the history rings, truncated to window_s.
func (dash *dashboard) handleTimeseries(w http.ResponseWriter, r *http.Request) {
	windowS := intQuery(r, "window_s", historyBuckets*historyStepS, historyStepS, historyBuckets*historyStepS)
	n := windowS / historyStepS
	if n < 1 {
		n = 1
	}

	snap := dash.deps.Hist.Snapshot()
	out := dashTimeseries{
		StartUnix: snap.StartUnix + int64((historyBuckets-n)*historyStepS),
		StepS:     historyStepS,
		Buckets:   n,
		Series:    make(map[string]dashSeries),
		Gauges: dashGaugeSeries{
			QueueDepth: make(map[string][]uint32),
			Running:    make(map[string][]uint32),
		},
	}

	// Every configured agent plus "_total" is always present (zero-filled),
	// so the UI's slot mapping is stable; extra observed keys pass through.
	keys := map[string]bool{"_total": true}
	for name := range dash.deps.Cfg.Agents {
		keys[name] = true
	}
	for name := range snap.Series {
		keys[name] = true
	}
	for key := range keys {
		s, ok := snap.Series[key]
		if !ok {
			out.Series[key] = dashSeries{
				Count:    make([]uint32, n),
				Errors:   make([]uint32, n),
				LatMsAvg: make([]float64, n),
				LatMsP95: make([]float64, n),
			}
			continue
		}
		out.Series[key] = dashSeries{
			Count:    s.Count[historyBuckets-n:],
			Errors:   s.Errors[historyBuckets-n:],
			LatMsAvg: s.LatMsAvg[historyBuckets-n:],
			LatMsP95: s.LatMsP95[historyBuckets-n:],
		}
	}
	for name := range dash.deps.Cfg.Agents {
		qd, rn := snap.QueueDepth[name], snap.Running[name]
		if qd == nil {
			qd = make([]uint32, historyBuckets)
		}
		if rn == nil {
			rn = make([]uint32, historyBuckets)
		}
		out.Gauges.QueueDepth[name] = qd[historyBuckets-n:]
		out.Gauges.Running[name] = rn[historyBuckets-n:]
	}

	writeJSON(w, http.StatusOK, out)
}

// --- tasks ------------------------------------------------------------------

// dashTaskList is the GET /dashboard/api/tasks payload: the /v1/tasks list
// shape plus the availability wrapper (store failures are 200+available:false).
type dashTaskList struct {
	Available bool          `json:"available"`
	Detail    string        `json:"detail,omitempty"`
	Object    string        `json:"object"`
	Data      []taskSummary `json:"data"`
	HasMore   bool          `json:"has_more"`
}

// dashTaskDetail is the full record plus the request preview.
type dashTaskDetail struct {
	Available      bool   `json:"available"`
	RequestPreview string `json:"request_preview"`
	Task
}

// storeUnavailable writes the degraded (but 200) task-store response.
func storeUnavailable(w http.ResponseWriter) {
	writeJSON(w, http.StatusOK, map[string]any{"available": false, "detail": "task store unavailable"})
}

// handleTasksList lists task summaries across ALL agents (the dashboard token
// is ops-privileged; no scope filter).
func (dash *dashboard) handleTasksList(w http.ResponseWriter, r *http.Request) {
	if dash.deps.Store == nil {
		writeJSON(w, http.StatusOK, dashTaskList{Available: false, Detail: "task store unavailable", Object: "list", Data: []taskSummary{}})
		return
	}
	q := r.URL.Query()
	filter := TaskFilter{
		Agent: q.Get("agent"),
		Limit: intQuery(r, "limit", dashTasksDefaultLimit, 1, maxListLimit),
		After: q.Get("after"),
	}
	for _, v := range q["state"] {
		switch TaskState(v) {
		case TaskPending, TaskRunning, TaskSucceeded, TaskFailed, TaskCancelled, TaskExpired:
			filter.States = append(filter.States, TaskState(v))
		default:
			if v == "active" {
				filter.States = append(filter.States, TaskPending, TaskRunning)
			} else if v != "" {
				writeError(w, http.StatusBadRequest, fmt.Sprintf("Unknown state %s", v), "invalid_request_error")
				return
			}
		}
	}
	items, hasMore := dash.deps.Store.List(filter)
	now := time.Now().UTC()
	data := make([]taskSummary, 0, len(items))
	for i := range items {
		t := &items[i]
		data = append(data, taskSummary{
			ID:              t.ID,
			Object:          "task.summary",
			Agent:           t.Agent,
			State:           t.State,
			Priority:        t.Priority,
			CreatedAt:       t.CreatedAt,
			UpdatedAt:       t.UpdatedAt,
			Attempts:        t.Attempts,
			CancelRequested: t.CancelRequested,
			AgeS:            int64(now.Sub(t.CreatedAt) / time.Second),
			Deadline:        t.Deadline,
			Error:           t.Error,
		})
	}
	writeJSON(w, http.StatusOK, dashTaskList{Available: true, Object: "list", Data: data, HasMore: hasMore})
}

// handleTaskItem routes tasks/{id}[/output|/cancel].
func (dash *dashboard) handleTaskItem(w http.ResponseWriter, r *http.Request, rest string) {
	id, action := rest, ""
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		id, action = rest[:i], rest[i+1:]
	}
	if id == "" {
		writeError(w, http.StatusNotFound, "No such task", "invalid_request_error")
		return
	}
	if dash.deps.Store == nil {
		storeUnavailable(w)
		return
	}
	switch action {
	case "":
		dash.getOnly(w, r, func(w http.ResponseWriter, r *http.Request) { dash.handleTaskDetail(w, id) })
	case "output":
		dash.getOnly(w, r, func(w http.ResponseWriter, r *http.Request) { dash.handleTaskOutputSpool(w, id) })
	case "cancel":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "Method not allowed", "invalid_request_error")
			return
		}
		dash.handleTaskCancelAction(w, id)
	default:
		writeError(w, http.StatusNotFound, "No such task", "invalid_request_error")
	}
}

// handleTaskDetail returns the full record plus request_preview (first 2 KiB
// of the last user message).
func (dash *dashboard) handleTaskDetail(w http.ResponseWriter, id string) {
	task, ok := dash.deps.Store.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "No such task", "invalid_request_error")
		return
	}
	writeJSON(w, http.StatusOK, dashTaskDetail{
		Available:      true,
		RequestPreview: requestPreview(task.Request),
		Task:           task,
	})
}

// requestPreview extracts the last user message's content from a task
// request, truncated to requestPreviewBytes on a rune boundary.
func requestPreview(request json.RawMessage) string {
	var shape struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(request, &shape) != nil {
		return ""
	}
	var content json.RawMessage
	for _, m := range shape.Messages {
		if m.Role == "user" {
			content = m.Content
		}
	}
	if content == nil {
		return ""
	}
	var s string
	if json.Unmarshal(content, &s) != nil {
		s = string(content) // structured content: show the raw JSON
	}
	if len(s) > requestPreviewBytes {
		cut := requestPreviewBytes
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut]
	}
	return s
}

// handleTaskOutputSpool streams the whole spool snapshot as text/plain.
func (dash *dashboard) handleTaskOutputSpool(w http.ResponseWriter, id string) {
	if _, ok := dash.deps.Store.Get(id); !ok {
		writeError(w, http.StatusNotFound, "No such task", "invalid_request_error")
		return
	}
	data, err := os.ReadFile(dash.deps.Store.OutputPath(id))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusInternalServerError, "Failed to read task output", "server_error")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// handleTaskCancelAction mirrors POST /v1/tasks/{id}/cancel semantics.
func (dash *dashboard) handleTaskCancelAction(w http.ResponseWriter, id string) {
	task, alreadyTerminal, err := dash.deps.Store.Cancel(id)
	if err != nil {
		if errors.Is(err, ErrTaskNotFound) {
			writeError(w, http.StatusNotFound, "No such task", "invalid_request_error")
			return
		}
		writeError(w, http.StatusInternalServerError, "Failed to cancel task", "server_error")
		return
	}
	writeJSON(w, http.StatusOK, taskCancelResponse{Task: task, AlreadyTerminal: alreadyTerminal})
}

// --- traces -----------------------------------------------------------------

// handleTraces summarizes the tail of the collector's traces file.
func (dash *dashboard) handleTraces(w http.ResponseWriter, r *http.Request) {
	limit := intQuery(r, "limit", dashTracesDefaultLimit, 1, dashTracesMaxLimit)
	windowS := intQuery(r, "window_s", dashTracesWindowS, historyStepS, dashMaxWindowS)
	res := readTraces(dash.deps.Cfg.Observability.TracesFile, limit,
		time.Duration(windowS)*time.Second, time.Now().UTC())
	if !res.Available {
		gwMetrics.IncDashboardSourceError("traces")
	}
	writeJSON(w, http.StatusOK, res)
}

// --- egress -----------------------------------------------------------------

// egressTotals aggregates the whole window.
type egressTotals struct {
	Requests int   `json:"requests"`
	Denied   int   `json:"denied"`
	Bytes    int64 `json:"bytes"`
}

// egressAgent is one client VM's traffic.
type egressAgent struct {
	Agent    string `json:"agent"`
	VMIP     string `json:"vm_ip"`
	Requests int    `json:"requests"`
	Denied   int    `json:"denied"`
	Bytes    int64  `json:"bytes"`
}

// egressHost is one destination host's traffic.
type egressHost struct {
	Host     string `json:"host"`
	Requests int    `json:"requests"`
	Bytes    int64  `json:"bytes"`
	Denied   int    `json:"denied"`
}

// egressDenied is one denied request for the dedicated denied list.
type egressDenied struct {
	TS     time.Time `json:"ts"`
	Agent  string    `json:"agent"`
	Host   string    `json:"host"`
	Method string    `json:"method"`
	Result string    `json:"result"`
}

// dashEgress is the GET /dashboard/api/egress payload (§b).
type dashEgress struct {
	Available    bool           `json:"available"`
	Detail       string         `json:"detail,omitempty"`
	WindowS      int            `json:"window_s"`
	Log          string         `json:"log"`
	Lines        int            `json:"lines"`
	SkippedLines int            `json:"skipped_lines"`
	Totals       egressTotals   `json:"totals"`
	ByAgent      []egressAgent  `json:"by_agent"`
	TopHosts     []egressHost   `json:"top_hosts"`
	Denied       []egressDenied `json:"denied"`
}

// handleEgress aggregates the squid access-log tail over the window.
func (dash *dashboard) handleEgress(w http.ResponseWriter, r *http.Request) {
	windowS := intQuery(r, "window_s", dashEgressWindowS, historyStepS, dashMaxWindowS)
	window := time.Duration(windowS) * time.Second
	now := time.Now().UTC()
	logPath := dash.deps.Cfg.Observability.SquidAccessLog

	entries, lines, skipped, err := readSquidEntries(logPath, window, now)
	if err != nil {
		gwMetrics.IncDashboardSourceError("squid")
		writeJSON(w, http.StatusOK, dashEgress{
			Available: false, Detail: err.Error(), WindowS: windowS, Log: logPath,
			ByAgent: []egressAgent{}, TopHosts: []egressHost{}, Denied: []egressDenied{},
		})
		return
	}

	ipAgent := make(map[string]string)
	for _, vm := range ListVMs(dash.deps.Cfg.StateDir) {
		ipAgent[vm.VMIP] = vm.AgentType
	}
	agentOf := func(ip string) string {
		if a, ok := ipAgent[ip]; ok {
			return a
		}
		return "unknown"
	}

	out := dashEgress{
		Available: true, WindowS: windowS, Log: logPath,
		Lines: lines, SkippedLines: skipped,
		ByAgent: []egressAgent{}, TopHosts: []egressHost{}, Denied: []egressDenied{},
	}
	cutoff := now.Add(-window)
	byIP := make(map[string]*egressAgent)
	byHost := make(map[string]*egressHost)
	for _, e := range entries {
		if e.TS.Before(cutoff) {
			continue
		}
		out.Totals.Requests++
		out.Totals.Bytes += e.Bytes
		a := byIP[e.ClientIP]
		if a == nil {
			a = &egressAgent{Agent: agentOf(e.ClientIP), VMIP: e.ClientIP}
			byIP[e.ClientIP] = a
		}
		a.Requests++
		a.Bytes += e.Bytes
		h := byHost[e.Host]
		if h == nil {
			h = &egressHost{Host: e.Host}
			byHost[e.Host] = h
		}
		h.Requests++
		h.Bytes += e.Bytes
		if e.Denied {
			out.Totals.Denied++
			a.Denied++
			h.Denied++
			out.Denied = append(out.Denied, egressDenied{
				TS: e.TS, Agent: a.Agent, Host: e.Host, Method: e.Method, Result: e.Code,
			})
		}
	}
	for _, a := range byIP {
		out.ByAgent = append(out.ByAgent, *a)
	}
	sort.Slice(out.ByAgent, func(i, j int) bool {
		if out.ByAgent[i].Agent != out.ByAgent[j].Agent {
			return out.ByAgent[i].Agent < out.ByAgent[j].Agent
		}
		return out.ByAgent[i].VMIP < out.ByAgent[j].VMIP
	})
	for _, h := range byHost {
		out.TopHosts = append(out.TopHosts, *h)
	}
	sort.Slice(out.TopHosts, func(i, j int) bool {
		if out.TopHosts[i].Requests != out.TopHosts[j].Requests {
			return out.TopHosts[i].Requests > out.TopHosts[j].Requests
		}
		return out.TopHosts[i].Host < out.TopHosts[j].Host
	})
	if len(out.TopHosts) > topHostsCap {
		out.TopHosts = out.TopHosts[:topHostsCap]
	}
	// Newest denied first, capped.
	sort.Slice(out.Denied, func(i, j int) bool { return out.Denied[i].TS.After(out.Denied[j].TS) })
	if len(out.Denied) > deniedListCap {
		out.Denied = out.Denied[:deniedListCap]
	}

	writeJSON(w, http.StatusOK, out)
}
