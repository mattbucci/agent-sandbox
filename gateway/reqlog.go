package main

// Structured logging (plan item 13): a process-wide log/slog logger emitting
// one JSON object per line on stdout (the systemd unit redirects stdout to
// logs/gateway.log). Canonical events: http_request, sched_reject,
// task_submit/start/finish/retry/cancel, vm_resolve_fail, otlp_export_fail,
// store_error, startup, shutdown, fatal. Every in-request event carries
// trace_id/span_id when a server span exists. Log token NAMES only — never
// token values, session keys or message content.

import (
	"log/slog"
	"os"
)

// slogger is the process-wide structured logger. It defaults to JSON;
// initLogging switches the format per observability.log_format. main()
// reconfigures it once at startup before any goroutines are spawned.
var slogger = slog.New(slog.NewJSONHandler(os.Stdout, nil))

// initLogging configures the global logger format ("json" | "text").
func initLogging(format string) {
	if format == "text" {
		slogger = slog.New(slog.NewTextHandler(os.Stdout, nil))
		return
	}
	slogger = slog.New(slog.NewJSONHandler(os.Stdout, nil))
}

// logInfo emits a canonical event at INFO with key/value attrs.
func logInfo(event string, args ...any) { slogger.Info(event, args...) }

// logWarn emits a canonical event at WARN with key/value attrs.
func logWarn(event string, args ...any) { slogger.Warn(event, args...) }

// logError emits a canonical event at ERROR with key/value attrs.
func logError(event string, args ...any) { slogger.Error(event, args...) }

// logFatal emits the canonical "fatal" event and exits non-zero.
func logFatal(args ...any) {
	slogger.Error("fatal", args...)
	os.Exit(1)
}

// logTaskEvent maps a task state transition to its canonical log event.
// Wired as (part of) the TaskStore onTransition hook by main().
func logTaskEvent(ev TaskEvent) {
	t := ev.Task
	args := []any{"task_id", t.ID, "agent", t.Agent, "state", string(ev.To), "attempts", t.Attempts}
	if t.SubmitTrace != nil {
		args = append(args, "trace_id", t.SubmitTrace.TraceID, "span_id", t.SubmitTrace.SpanID)
	}
	switch {
	case ev.From == "":
		logInfo("task_submit", append(args, "priority", t.Priority, "submitted_by", t.SubmittedBy)...)
	case ev.To == TaskRunning:
		logInfo("task_start", args...)
	case ev.To == TaskPending:
		if t.Error != nil {
			args = append(args, "error_kind", t.Error.Kind)
		}
		logInfo("task_retry", args...)
	case ev.To == TaskCancelled:
		logInfo("task_cancel", args...)
	default: // succeeded | failed | expired
		if t.Error != nil {
			args = append(args, "error_kind", t.Error.Kind, "error", t.Error.Message)
		}
		logInfo("task_finish", args...)
	}
}
