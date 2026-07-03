// Command hermes-gateway is the bare-metal ROUTER half of the Hermes Gateway.
//
// It exposes an OpenAI-compatible API on the host LAN (default 0.0.0.0:8642),
// authenticates incoming bearer tokens, maps the OpenAI "model" field to an
// agent type, resolves a live Firecracker VM running that agent, and
// reverse-proxies (streaming SSE) to the in-VM server at <vm_ip>:<vm_gateway_port>.
// It also runs the async task subsystem (/v1/tasks*) with per-agent admission
// control shared with the sync chat path, plus observability: OTLP spans,
// Prometheus /metrics, structured JSON logs and the dashboard hook.
//
// It uses only the Go standard library so it can be built on an offline host.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

// defaultConfigPath is used when neither -config nor GATEWAY_CONFIG is set.
const defaultConfigPath = "/home/letsrtfm/AI/agent-sandbox/state/gateway/gateway.json"

// version identifies the build (overridable: -ldflags "-X main.version=...").
var version = "dev"

// server bundles the loaded config and subsystems for the HTTP handlers.
// store is nil when the task subsystem is disabled (or failed to open, in
// which case routing keeps working and the task routes are not registered).
// mx/tracer/hist may be nil in tests; every use is nil-safe.
type server struct {
	cfg       *Config
	sched     *Scheduler
	store     *TaskStore
	mx        *Metrics
	tracer    *Tracer
	hist      *History
	runs      *runRegistry
	startedAt time.Time
}

func main() {
	configFlag := flag.String("config", "", "path to gateway.json (overrides GATEWAY_CONFIG env)")
	flag.Parse()

	configPath := *configFlag
	if configPath == "" {
		configPath = os.Getenv("GATEWAY_CONFIG")
	}
	if configPath == "" {
		configPath = defaultConfigPath
	}

	// Wiring order (plan §a item 9): config -> store -> RecoverOnBoot ->
	// scheduler -> observability hooks -> dispatchers -> mux (+ new routes)
	// -> serve.
	cfg, err := LoadConfig(configPath)
	if err != nil {
		logFatal("err", err.Error(), "config", configPath)
	}
	initLogging(cfg.Observability.LogFormat)

	var store *TaskStore
	if cfg.TasksEnabled() {
		st, serr := NewTaskStore(cfg)
		if serr == nil {
			serr = st.RecoverOnBoot()
		}
		if serr != nil {
			// The tasks dir is an external data source: degrade (no task
			// routes) without touching routing.
			logError("store_error", "err", serr.Error(), "detail", "task subsystem disabled")
		} else {
			store = st
		}
	}

	sched := NewScheduler(cfg)
	mx := newMetrics(cfg, version, sched, store)
	gwMetrics = mx
	tracer := newTracer(cfg.OTLPEndpoint(), cfg.SampleRatio(), version)
	tracer.onBatch = mx.OnOTLPBatch
	tracer.onDrop = mx.OnOTLPDrop
	tracer.StartExporter()
	hist := newHistory()

	// Observability hooks: set before any request or dispatcher runs.
	sched.onEvent = mx.HandleSchedEvent
	if store != nil {
		store.onTransition = func(ev TaskEvent) {
			mx.HandleTaskEvent(ev)
			logTaskEvent(ev)
		}
	}

	rootCtx, rootCancel := context.WithCancel(context.Background())
	defer rootCancel()
	var wg sync.WaitGroup
	go hist.Run(rootCtx, sched)
	if store != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			store.RunSweeper(rootCtx)
		}()
		startInstrumentedDispatchers(rootCtx, cfg, sched, store, &wg, tracer, mx)
	}

	s := &server{cfg: cfg, sched: sched, store: store, mx: mx, tracer: tracer, hist: hist, startedAt: time.Now()}
	mux := s.routes()

	addr := fmt.Sprintf("%s:%d", cfg.Bind, cfg.Port)
	logInfo("startup", "config", configPath, "addr", addr, "default_agent", cfg.DefaultAgent,
		"state_dir", cfg.StateDir, "tasks", store != nil, "version", version,
		"otlp_endpoint", cfg.OTLPEndpoint(), "sample_ratio", cfg.SampleRatio())

	srv := &http.Server{Addr: addr, Handler: s.instrument(mux)}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			logFatal("err", err.Error(), "detail", "server error")
		}
	case sig := <-sigCh:
		logInfo("shutdown", "signal", sig.String())
		// 1. Stop accepting; let in-flight sync streams finish (30s cap).
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		if err := srv.Shutdown(shutdownCtx); err != nil {
			logWarn("shutdown", "detail", "http shutdown", "err", err.Error())
		}
		cancel()
		// 2. Wake every queued waiter with ErrShuttingDown.
		sched.Close()
		// 3. Stop dispatchers/sweeper and cancel running task attempts with
		// cause shutdown; each finalizes via the same interruption rules boot
		// recovery uses.
		rootCancel()
		if store != nil {
			store.CancelAll(errShutdown)
		}
		// 4. Bounded wait for runners to finish (10s cap).
		done := make(chan struct{})
		go func() {
			wg.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			logWarn("shutdown", "detail", "timed out waiting for task runners")
		}
		// 5. Final span flush (2s cap inside Close).
		tracer.Close()
		logInfo("shutdown", "detail", "complete")
	}
}

// routes builds the full mux: the four legacy endpoints (byte-compatible),
// the task API when the subsystem is enabled, /metrics when a registry is
// wired, and the dashboard registration hook (no-op until the dashboard lane
// overrides it). Disabled task routes are simply not registered so they 404
// exactly like any unknown path.
func (s *server) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/v1/capabilities", s.handleCapabilities)
	mux.HandleFunc("/v1/models", s.handleModels)
	mux.HandleFunc("/v1/chat/completions", s.handleChat)
	registerRunsAPI(mux, s)
	if s.cfg.TasksEnabled() && s.store != nil {
		registerTasksAPI(mux, s)
	}
	if s.mx != nil {
		mux.HandleFunc("/metrics", s.handleMetrics)
	}
	registerDashboard(mux, &DashDeps{
		Cfg:       s.cfg,
		Sched:     s.sched,
		Store:     s.store,
		Hist:      s.hist,
		Tracer:    s.tracer,
		StartedAt: s.startedAt,
		Version:   version,
	})
	return mux
}

// reqInfo is per-request state shared between the middleware and handlers:
// the SERVER span plus labels only the handler can resolve. Handlers fetch
// it with reqInfoFrom; it is nil when the middleware is not installed.
type reqInfo struct {
	span      *Span
	agent     string
	tokenName string
}

// reqInfoKey is the context key for the per-request info.
type reqInfoKey struct{}

// reqInfoFrom returns the request's reqInfo, or nil without the middleware.
func reqInfoFrom(ctx context.Context) *reqInfo {
	ri, _ := ctx.Value(reqInfoKey{}).(*reqInfo)
	return ri
}

// statusRecorder captures the response status code while forwarding
// everything (including Flush, which the SSE stream path requires).
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (sr *statusRecorder) WriteHeader(code int) {
	if sr.status == 0 {
		sr.status = code
	}
	sr.ResponseWriter.WriteHeader(code)
}

func (sr *statusRecorder) Write(b []byte) (int, error) {
	if sr.status == 0 {
		sr.status = http.StatusOK
	}
	return sr.ResponseWriter.Write(b)
}

func (sr *statusRecorder) Flush() {
	if f, ok := sr.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// instrument is the middleware chain (plan item 15): metrics + inflight
// gauge + http_request log + SERVER span. Spans are opened for /v1/* paths
// only (the span model §g covers chat + task API; health/metrics/dashboard
// polling would be pure noise). A valid inbound traceparent makes the SERVER
// span a child of the caller's context; sampled=0 is overridden to sampled
// because we own the whole backend (ADR 0003). Invalid headers start a new
// root.
func (s *server) instrument(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		path := pathLabel(r.URL.Path)
		ri := &reqInfo{}
		if s.tracer != nil && strings.HasPrefix(r.URL.Path, "/v1/") {
			name := r.Method + " " + path
			if parent, ok := parseTraceparent(r.Header.Get("Traceparent")); ok {
				parent.Sampled = true
				ri.span = s.tracer.StartChild(parent, name, KindServer)
			} else {
				ri.span = s.tracer.StartRoot(name, KindServer)
			}
			ri.span.SetAttr("http.request.method", r.Method)
			ri.span.SetAttr("url.path", r.URL.Path)
			if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
				ri.span.SetAttr("client.address", host)
			}
		}

		s.mx.InflightAdd(path, 1)
		sr := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(sr, r.WithContext(context.WithValue(r.Context(), reqInfoKey{}, ri)))
		s.mx.InflightAdd(path, -1)

		status := sr.status
		if status == 0 {
			status = http.StatusOK
		}
		dur := time.Since(start)
		s.mx.ObserveHTTPRequest(path, r.Method, status, dur)
		// Sched rejections (429) and server-side failures count as errors;
		// 4xx client errors do not.
		isErr := status >= 500 || status == http.StatusTooManyRequests
		if path != "/metrics" && path != "/dashboard" {
			s.hist.Observe(ri.agent, dur, isErr)
		}

		if ri.span != nil {
			ri.span.SetAttr("http.response.status_code", status)
			if ri.tokenName != "" {
				ri.span.SetAttr("hermes.token_name", ri.tokenName)
			}
			if isErr {
				ri.span.SetError(http.StatusText(status))
			}
			ri.span.End()
		}

		args := []any{"method", r.Method, "path", r.URL.Path, "status", status,
			"duration_ms", dur.Milliseconds(), "client", r.RemoteAddr}
		if ri.agent != "" {
			args = append(args, "agent", ri.agent)
		}
		if ri.tokenName != "" {
			args = append(args, "token", ri.tokenName)
		}
		args = append(args, spanLogAttrs(ri.span)...)
		logInfo("http_request", args...)
	})
}

// handleHealth reports liveness without auth.
func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleCapabilities advertises the gateway feature surface for the agent named
// by the ?model= query (empty/"default" -> default_agent). The runs-API feature
// block (interactive dangerous-command approval) mirrors the resolved agent's
// static `approval` config flag: the webui probes this per chat to decide
// whether to use the runs path for the selected model. An unknown or absent
// model resolves the run features to false (safe default). No auth is required
// (the webui probes before presenting a key), matching the legacy contract and
// the real hermes-agent, whose capabilities object shape this mirrors.
func (s *server) handleCapabilities(w http.ResponseWriter, r *http.Request) {
	agent := r.URL.Query().Get("model")
	if agent == "" || agent == "default" {
		agent = s.cfg.DefaultAgent
	}
	ac, known := s.cfg.Agents[agent]
	approval := known && ac.Approval

	writeJSON(w, http.StatusOK, map[string]any{
		"object":   "hermes.api_server.capabilities",
		"platform": "hermes-gateway",
		"model":    agent,
		"features": map[string]bool{
			// Baseline chat surface — always available on the gateway.
			"chat_completions":           true,
			"chat_completions_streaming": true,
			// Runs API (proxied to the backend) — gated by the agent's flag.
			"run_submission":        approval,
			"run_status":            approval,
			"run_events_sse":        approval,
			"run_stop":              approval,
			"run_approval_response": approval,
			"tool_progress_events":  approval,
			"approval_events":       approval,
		},
	})
}

// handleModels returns the agents visible to the caller's token scope.
func (s *server) handleModels(w http.ResponseWriter, r *http.Request) {
	tok, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	names := s.cfg.scopedAgents(tok.Agents)
	data := make([]map[string]string, 0, len(names))
	for _, name := range names {
		data = append(data, map[string]string{
			"id":       name,
			"object":   "model",
			"owned_by": "hermes-gateway",
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// handleChat authenticates then delegates to the streaming proxy.
func (s *server) handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "Method not allowed", "invalid_request_error")
		return
	}
	tok, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	s.handleChatCompletions(w, r, tok.Agents)
}

// authenticate validates the bearer token and returns the matched token. On
// failure it writes the spec 401 body and returns ok=false. The matched token
// NAME (never the secret) is attached to the request's log/span context.
func (s *server) authenticate(w http.ResponseWriter, r *http.Request) (tok *Token, ok bool) {
	auth := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if strings.HasPrefix(auth, prefix) {
		presented := strings.TrimSpace(auth[len(prefix):])
		for i := range s.cfg.Tokens {
			t := &s.cfg.Tokens[i]
			if t.Token != "" && presented == t.Token {
				if ri := reqInfoFrom(r.Context()); ri != nil {
					ri.tokenName = t.Name
				}
				return t, true
			}
		}
	}
	s.mx.IncAuthFailure()
	writeError(w, http.StatusUnauthorized, "Invalid API key", "invalid_request_error")
	return nil, false
}

// writeJSON marshals v and writes it with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError emits an OpenAI-style error envelope.
func writeError(w http.ResponseWriter, status int, message, errType string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    errType,
		},
	})
}
