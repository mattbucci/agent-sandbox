package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// Cancellation causes used by the runner to classify attempt outcomes.
var (
	// errShutdown is the cause used when the gateway is stopping; attempts
	// interrupted by it finalize via the same rules boot recovery uses.
	errShutdown = errors.New("gateway shutting down")
	// errIdleTimeout is the cause set by the idle watchdog when no bytes have
	// arrived from the VM for idle_timeout_s.
	errIdleTimeout = errors.New("idle timeout: no output from VM")
)

// dispatcher drives async task execution for one agent: it waits for runnable
// work, acquires a classAsync scheduler slot ("I have runnable work" — the
// concrete task is chosen by Claim only after the grant), then spawns a
// runner per attempt. tracer/mx are nil-safe observability hooks.
type dispatcher struct {
	cfg    *Config
	sched  *Scheduler
	store  *TaskStore
	agent  string
	wg     *sync.WaitGroup
	tracer *Tracer
	mx     *Metrics
}

// startDispatchers launches one dispatcher goroutine per configured agent
// without observability wiring (kept for tests and API stability).
func startDispatchers(ctx context.Context, cfg *Config, sched *Scheduler, store *TaskStore, wg *sync.WaitGroup) {
	startInstrumentedDispatchers(ctx, cfg, sched, store, wg, nil, nil)
}

// startInstrumentedDispatchers launches one dispatcher goroutine per
// configured agent, wiring the tracer and metrics registry (either may be
// nil). main.go calls it iff tasks are enabled. All goroutines (loops and
// runners) register on wg so shutdown can bound its wait.
func startInstrumentedDispatchers(ctx context.Context, cfg *Config, sched *Scheduler, store *TaskStore, wg *sync.WaitGroup, tracer *Tracer, mx *Metrics) {
	for name := range cfg.Agents {
		d := &dispatcher{cfg: cfg, sched: sched, store: store, agent: name, wg: wg, tracer: tracer, mx: mx}
		wg.Add(1)
		go d.loop(ctx)
	}
}

// loop waits on the store's poke channel / the NextWake timer / ctx, then
// drains runnable work. Tasks for agents without a dispatcher (orphans) are
// never claimed and expire at their deadline via the sweeper.
func (d *dispatcher) loop(ctx context.Context) {
	defer d.wg.Done()
	poke := d.store.Runnable(d.agent)
	for {
		var timer *time.Timer
		var timerC <-chan time.Time
		if wake, ok := d.store.NextWake(d.agent); ok {
			delay := time.Until(wake)
			if delay < 0 {
				delay = 0
			}
			timer = time.NewTimer(delay)
			timerC = timer.C
		}
		select {
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return
		case <-poke:
		case <-timerC:
		}
		if timer != nil {
			timer.Stop()
		}
		d.drainRunnable(ctx)
	}
}

// drainRunnable starts runners while there is immediately runnable work and
// capacity. It posts at most one classAsync slot request at a time: while it
// blocks in Acquire, no further requests pile up, and priority churn /
// cancels never touch scheduler state (Claim decides after the grant).
func (d *dispatcher) drainRunnable(ctx context.Context) {
	for {
		wake, ok := d.store.NextWake(d.agent)
		if !ok || wake.After(time.Now()) {
			return
		}
		acquireStart := time.Now()
		release, err := d.sched.Acquire(ctx, d.agent, classAsync, SlotMeta{})
		if err != nil {
			return // ctx cancelled or scheduler shutting down
		}
		queueWait := time.Since(acquireStart)
		task, claimed := d.store.Claim(d.agent)
		if !claimed {
			// The runnable task vanished while we waited (cancelled/expired).
			release()
			return
		}
		d.wg.Add(1)
		go func() {
			defer d.wg.Done()
			defer release()
			d.runAttempt(task, queueWait)
		}()
	}
}

// runTask executes one attempt with no recorded queue wait (test seam; the
// dispatcher loop always goes through runAttempt).
func (d *dispatcher) runTask(task Task) {
	d.runAttempt(task, 0)
}

// runAttempt executes one attempt of a claimed task: per-attempt context
// bounded by min(now+timeout_s, deadline), VM resolve, buildVMRequest, SSE
// drive with spool append and idle watchdog, then Finish with the classified
// outcome (Finish applies the retriability rules).
//
// Tracing (§g): every attempt is a NEW ROOT span "task.attempt" with a span
// link to the persisted submit_trace context; the CLIENT proxy span hangs
// under it and its context rides the traceparent header into the VM. The
// attempt trace id is appended to the task's trace_ids for the dashboard
// join.
func (d *dispatcher) runAttempt(task Task, queueWait time.Duration) {
	now := time.Now()
	attemptDeadline := now.Add(time.Duration(task.TimeoutS) * time.Second)
	if task.Deadline.Before(attemptDeadline) {
		attemptDeadline = task.Deadline
	}
	// The base context is Background, not the dispatcher ctx: shutdown stops
	// runners via CancelAll(errShutdown) so the cause is always explicit.
	base, cancelCause := context.WithCancelCause(context.Background())
	runCtx, cancelTimer := context.WithDeadline(base, attemptDeadline)
	defer cancelTimer()
	defer cancelCause(nil)
	d.store.RegisterCancel(task.ID, cancelCause)

	var attemptSpan *Span
	if d.tracer != nil {
		attemptSpan = d.tracer.StartRoot("task.attempt", KindInternal)
		attemptSpan.SetAttr("hermes.task_id", task.ID)
		attemptSpan.SetAttr("hermes.task_attempt", task.Attempts)
		attemptSpan.SetAttr("hermes.task_priority", task.Priority)
		attemptSpan.SetAttr("hermes.agent", d.agent)
		attemptSpan.SetAttr("hermes.queue_wait_ms", queueWait.Milliseconds())
		if task.SubmitTrace != nil {
			attemptSpan.AddLink(task.SubmitTrace.TraceID, task.SubmitTrace.SpanID)
		}
		d.store.AppendTraceID(task.ID, attemptSpan.Context().TraceID)
		defer attemptSpan.End()
	}

	finish := func(spec FinishSpec) {
		res, err := d.store.Finish(task.ID, spec)
		if err != nil {
			logError("store_error", append([]any{"task_id", task.ID, "agent", d.agent,
				"attempt", task.Attempts, "detail", "finish failed", "err", err.Error()},
				spanLogAttrs(attemptSpan)...)...)
			attemptSpan.SetAttr("hermes.outcome", "unknown")
			return
		}
		attemptSpan.SetAttr("hermes.outcome", string(res.State))
		if res.State == TaskFailed || res.State == TaskExpired {
			msg := string(res.State)
			if res.Error != nil {
				msg = res.Error.Message
			}
			attemptSpan.SetError(msg)
		}
	}

	vm, ok := findVMForAgent(d.cfg.StateDir, d.agent)
	if !ok {
		d.mx.IncUpstreamError(d.agent, "no_vm")
		logWarn("vm_resolve_fail", append([]any{"agent", d.agent, "task_id", task.ID},
			spanLogAttrs(attemptSpan)...)...)
		finish(FinishSpec{Err: &TaskError{
			Message: fmt.Sprintf("no running VM for agent %s", d.agent),
			Kind:    ErrKindVMUnreachable,
		}})
		return
	}

	if err := d.store.TruncateOutput(task.ID); err != nil {
		finish(FinishSpec{VMIP: vm.VMIP, Err: &TaskError{
			Message: fmt.Sprintf("truncate output spool: %v", err),
			Kind:    ErrKindDownstream,
		}})
		return
	}

	body, err := taskRequestBody(task)
	if err != nil {
		finish(FinishSpec{VMIP: vm.VMIP, Err: &TaskError{
			Message: fmt.Sprintf("build request body: %v", err),
			Kind:    ErrKindDownstream,
		}})
		return
	}

	// CLIENT proxy span; its context rides traceparent into the VM.
	var clientSpan *Span
	traceparent := ""
	if attemptSpan != nil {
		clientSpan = d.tracer.StartChild(attemptSpan.Context(), "proxy /v1/chat/completions", KindClient)
		clientSpan.SetAttr("server.address", vm.VMIP)
		clientSpan.SetAttr("server.port", d.cfg.VMGatewayPort)
		clientSpan.SetAttr("url.full", fmt.Sprintf("http://%s:%d/v1/chat/completions", vm.VMIP, d.cfg.VMGatewayPort))
		clientSpan.SetAttr("hermes.vm_instance", vm.InstanceID)
		clientSpan.SetAttr("hermes.model_rewritten", d.cfg.Agents[d.agent].Model != "")
		traceparent = formatTraceparent(clientSpan.Context())
		defer clientSpan.End()
	}

	req, err := buildVMRequest(runCtx, d.cfg, vm, d.agent, body, task.SessionID, "", traceparent)
	if err != nil {
		clientSpan.SetError(err.Error())
		finish(FinishSpec{VMIP: vm.VMIP, Err: &TaskError{
			Message: fmt.Sprintf("build downstream request: %v", err),
			Kind:    ErrKindDownstream,
		}})
		return
	}

	// Idle watchdog: armed across dial/TTFB, rearmed on every read while
	// streaming. It cancels the attempt with cause errIdleTimeout.
	idle := time.Duration(task.IdleTimeoutS) * time.Second
	watchdog := time.AfterFunc(idle, func() { cancelCause(errIdleTimeout) })
	defer watchdog.Stop()

	doStart := time.Now()
	resp, err := httpClient.Do(req)
	if err != nil {
		clientSpan.SetError(err.Error())
		// Connect refused / dial failure before the first byte is
		// VM-unavailable (refunded attempt) unless a cancel cause fired.
		if spec, ok := d.classifyCancel(runCtx, 0); ok {
			finish(spec.withVM(vm.VMIP))
			return
		}
		d.mx.IncUpstreamError(d.agent, "connect")
		finish(FinishSpec{VMIP: vm.VMIP, Err: &TaskError{
			Message: fmt.Sprintf("failed to reach VM: %v", err),
			Kind:    ErrKindVMUnreachable,
		}})
		return
	}
	defer resp.Body.Close()

	clientSpan.SetAttr("http.response.status_code", resp.StatusCode)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		if resp.StatusCode >= 500 {
			d.mx.IncUpstreamError(d.agent, "status_5xx")
		}
		// Non-2xx before the first body byte is VM-unavailable too.
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		clientSpan.SetError(fmt.Sprintf("VM returned status %d", resp.StatusCode))
		finish(FinishSpec{VMIP: vm.VMIP, Err: &TaskError{
			Message: fmt.Sprintf("VM returned status %d: %s", resp.StatusCode, string(snippet)),
			Kind:    ErrKindVMUnreachable,
		}})
		return
	}

	ct := resp.Header.Get("Content-Type")
	if !isSSEContentType(ct) {
		// Non-SSE fallback: the watchdog covered headers/TTFB only; the body
		// is one JSON object whose read is bounded by the attempt deadline.
		watchdog.Stop()
	}

	spool, err := os.OpenFile(d.store.OutputPath(task.ID), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		clientSpan.SetError(err.Error())
		finish(FinishSpec{VMIP: vm.VMIP, Err: &TaskError{
			Message: fmt.Sprintf("open output spool: %v", err),
			Kind:    ErrKindDownstream,
		}})
		return
	}
	firstByte := false
	result, drainErr := drainChatStream(ct, resp.Body, spool, func() {
		if !firstByte {
			firstByte = true
			d.mx.ObserveProxyTTFB(d.agent, time.Since(doStart))
			clientSpan.AddEvent("first_byte")
		}
		watchdog.Reset(idle)
	})
	watchdog.Stop()
	spool.Close()

	d.mx.AddStreamBytes(d.agent, result.Bytes)
	clientSpan.SetAttr("hermes.bytes_streamed", result.Bytes)
	if drainErr != nil {
		clientSpan.SetError(drainErr.Error())
	}

	finish(d.classifyOutcome(runCtx, task, vm.VMIP, result, drainErr))
}

// classifyOutcome maps the drain result + context cause to a FinishSpec.
func (d *dispatcher) classifyOutcome(runCtx context.Context, task Task, vmIP string, result streamResult, drainErr error) FinishSpec {
	if drainErr == nil && result.Done && result.FinishReason != "" {
		content, truncated := readSpoolPrefix(d.store.OutputPath(task.ID), maxInlineResult)
		return FinishSpec{
			VMIP:        vmIP,
			OutputBytes: result.Bytes,
			Result: &TaskResult{
				Content:          content,
				ContentTruncated: truncated,
				OutputBytes:      result.Bytes,
				FinishReason:     result.FinishReason,
				Usage:            result.Usage,
			},
		}
	}
	if spec, ok := d.classifyCancel(runCtx, result.Bytes); ok {
		return spec.withVM(vmIP)
	}
	msg := "stream completed without finish_reason"
	if drainErr != nil {
		msg = drainErr.Error()
	}
	return FinishSpec{VMIP: vmIP, OutputBytes: result.Bytes, Err: &TaskError{
		Message: msg,
		Kind:    ErrKindDownstream,
	}}
}

// classifyCancel inspects the runner context cause; ok is false when the
// context is still live (the failure came from the stream itself).
func (d *dispatcher) classifyCancel(runCtx context.Context, outputBytes int64) (FinishSpec, bool) {
	if runCtx.Err() == nil {
		return FinishSpec{}, false
	}
	cause := context.Cause(runCtx)
	spec := FinishSpec{OutputBytes: outputBytes}
	switch {
	case errors.Is(cause, ErrCancelRequested):
		spec.Err = &TaskError{Message: "cancelled by request", Kind: ErrKindCancelled}
	case errors.Is(cause, errShutdown):
		spec.Err = &TaskError{Message: "gateway shut down during attempt", Kind: ErrKindInterrupted}
	case errors.Is(cause, errIdleTimeout):
		spec.Err = &TaskError{Message: errIdleTimeout.Error(), Kind: ErrKindIdle}
	case errors.Is(cause, context.DeadlineExceeded):
		// Finish maps this to expired when the task deadline has passed.
		spec.Err = &TaskError{Message: "attempt timed out", Kind: ErrKindTimeout}
	default:
		spec.Err = &TaskError{Message: fmt.Sprintf("attempt cancelled: %v", cause), Kind: ErrKindCancelled}
	}
	return spec, true
}

// withVM returns a copy of the spec with the VM IP filled in.
func (s FinishSpec) withVM(vmIP string) FinishSpec {
	s.VMIP = vmIP
	return s
}

// taskRequestBody builds the downstream chat body for a task: the persisted
// request with model forced to the agent id (request.model is ignored at
// submit) and stream forced on so the idle watchdog sees bytes; the non-SSE
// fallback covers backends that ignore the stream flag.
func taskRequestBody(task Task) ([]byte, error) {
	var m map[string]any
	if err := json.Unmarshal(task.Request, &m); err != nil {
		return nil, err
	}
	m["model"] = task.Agent
	m["stream"] = true
	return json.Marshal(m)
}

// readSpoolPrefix reads up to max bytes of the output spool; truncated
// reports whether the file was larger than max.
func readSpoolPrefix(path string, max int) (content string, truncated bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, int64(max)+1))
	if err != nil {
		return "", false
	}
	if len(data) > max {
		return string(data[:max]), true
	}
	return string(data), false
}
