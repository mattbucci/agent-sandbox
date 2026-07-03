package main

// The dashboard registration seam (plan item 14). main() calls
// registerDashboard(mux, deps) unconditionally; the default is a no-op. The
// dashboard lane overrides the var from its own file via init() and never
// edits main.go or this file again.

import (
	"net/http"
	"time"
)

// DashDeps bundles everything the embedded ops dashboard needs. Store may be
// nil (tasks disabled or the store failed to open); Hist/Tracer are always
// non-nil in a real process but may be nil in tests.
type DashDeps struct {
	Cfg       *Config
	Sched     *Scheduler
	Store     *TaskStore
	Hist      *History
	Tracer    *Tracer
	StartedAt time.Time
	Version   string
}

// registerDashboard mounts the dashboard routes onto mux. No-op by default.
var registerDashboard = func(mux *http.ServeMux, d *DashDeps) {}
