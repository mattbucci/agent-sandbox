package main

// Embedded ops dashboard registration (plan item 18). init() overrides the
// registerDashboard var from hooks.go — this lane never edits main.go or
// hooks.go. Static assets are embedded (zero CDN/fonts/build step) and served
// as an unauthenticated inert shell; every /dashboard/api/* call requires an
// Authorization: Bearer from dashboard.tokens (constant-time compare) and
// fails CLOSED with 403 when no token is configured. When dashboard.enabled
// is false nothing is registered, so every /dashboard* path 404s exactly like
// any unknown path.

import (
	"embed"
	"net/http"
	"strings"
)

//go:embed static
var dashboardStaticFS embed.FS

func init() {
	registerDashboard = func(mux *http.ServeMux, d *DashDeps) {
		if d == nil || d.Cfg == nil || !d.Cfg.DashboardEnabled() {
			return
		}
		dash := &dashboard{deps: d}
		mux.HandleFunc("/dashboard", dash.handleRedirect)
		mux.HandleFunc("/dashboard/", dash.handle)
	}
}

// dashboard bundles the frozen DashDeps for the handlers.
type dashboard struct {
	deps *DashDeps
}

// handleRedirect sends GET /dashboard to /dashboard/.
func (dash *dashboard) handleRedirect(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/dashboard/", http.StatusFound)
}

// handle routes everything under /dashboard/ (go1.21-safe manual routing):
// the shell, the static assets and the authenticated API.
func (dash *dashboard) handle(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-cache")
	p := r.URL.Path
	switch {
	case p == "/dashboard/":
		dash.serveStatic(w, r, "index.html")
	case strings.HasPrefix(p, "/dashboard/static/"):
		dash.serveStatic(w, r, strings.TrimPrefix(p, "/dashboard/static/"))
	case strings.HasPrefix(p, "/dashboard/api/"):
		if !dash.authorize(w, r) {
			return
		}
		dash.handleAPI(w, r, strings.TrimPrefix(p, "/dashboard/api/"))
	default:
		http.NotFound(w, r)
	}
}

// serveStatic serves one embedded asset by bare name. Names containing path
// separators or ".." are rejected outright (no traversal out of static/).
func (dash *dashboard) serveStatic(w http.ResponseWriter, r *http.Request, name string) {
	if name == "" || strings.ContainsAny(name, "/\\") || strings.Contains(name, "..") {
		http.NotFound(w, r)
		return
	}
	data, err := dashboardStaticFS.ReadFile("static/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	ct := "application/octet-stream"
	switch {
	case strings.HasSuffix(name, ".html"):
		ct = "text/html; charset=utf-8"
	case strings.HasSuffix(name, ".js"):
		ct = "text/javascript; charset=utf-8"
	case strings.HasSuffix(name, ".css"):
		ct = "text/css; charset=utf-8"
	}
	w.Header().Set("Content-Type", ct)
	_, _ = w.Write(data)
}

// authorize enforces the dashboard API auth rule (§b): the bearer must be one
// of dashboard.tokens (constant-time compare). An empty token list fails
// closed with 403; a missing/wrong bearer is 401.
func (dash *dashboard) authorize(w http.ResponseWriter, r *http.Request) bool {
	configured := false
	for _, t := range dash.deps.Cfg.Dashboard.Tokens {
		if t != "" {
			configured = true
			break
		}
	}
	if !configured {
		writeError(w, http.StatusForbidden, "dashboard token not configured", "invalid_request_error")
		return false
	}
	auth := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if strings.HasPrefix(auth, prefix) {
		presented := strings.TrimSpace(auth[len(prefix):])
		for _, t := range dash.deps.Cfg.Dashboard.Tokens {
			if t != "" && tokenEqual(presented, t) {
				return true
			}
		}
	}
	gwMetrics.IncAuthFailure()
	writeError(w, http.StatusUnauthorized, "Invalid dashboard token", "invalid_request_error")
	return false
}
