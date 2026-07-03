package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sort"
	"sync"
	"time"
)

// Sentinel errors returned by Scheduler.Acquire. The sync chat path maps
// ErrQueueFull to 429 and ErrWaitTimeout to 503 (both with Retry-After).
var (
	ErrQueueFull    = errors.New("scheduler: sync queue full")
	ErrWaitTimeout  = errors.New("scheduler: timed out waiting for a slot")
	ErrShuttingDown = errors.New("scheduler: shutting down")
	ErrUnknownAgent = errors.New("scheduler: unknown agent")
)

// slotClass types a slot request. Sync requests (interactive chat) join a
// bounded FIFO with a wait timeout; async requests (task dispatchers) queue
// unbounded, never time out, and age past the sync FIFO after
// scheduler.async_starvation_after_s.
type slotClass int

const (
	classSync slotClass = iota
	classAsync
)

// String returns the metric/dashboard label for the class ("sync" | "task").
func (c slotClass) String() string {
	if c == classSync {
		return "sync"
	}
	return "task"
}

// SlotMeta is caller-supplied metadata attached to a slot request, surfaced
// verbatim in Snapshot() for the dashboard. RunID is generated when empty.
type SlotMeta struct {
	RunID     string
	TaskID    string
	SessionID string
	TraceID   string
	Priority  int
}

// SchedEventType enumerates scheduler lifecycle events for the onEvent hook.
type SchedEventType string

const (
	SchedEventGranted       SchedEventType = "granted"
	SchedEventRejectedFull  SchedEventType = "rejected_full"
	SchedEventWaitTimeout   SchedEventType = "wait_timeout"
	SchedEventWaitCancelled SchedEventType = "wait_cancelled"
)

// SchedEvent is delivered to the optional onEvent hook (metrics/spans, wired
// by the observability lane). Wait and QueueDepthAtEnqueue are zero for
// grants that never queued.
type SchedEvent struct {
	Type                SchedEventType
	Agent               string
	Class               string // "sync" | "task"
	Wait                time.Duration
	QueueDepthAtEnqueue int
}

// SchedCounters are cumulative per-agent admission counters.
type SchedCounters struct {
	Granted       uint64
	RejectedFull  uint64
	WaitTimeouts  uint64
	WaitCancelled uint64
}

// SchedRunningInfo describes one held slot for the dashboard.
type SchedRunningInfo struct {
	RunID     string
	Kind      string // "sync" | "task"
	TaskID    string
	SessionID string
	TraceID   string
	StartedAt time.Time
}

// SchedWaitingInfo describes one queued slot request for the dashboard.
type SchedWaitingInfo struct {
	RunID      string
	Kind       string // "sync" | "task"
	TaskID     string
	Priority   int
	EnqueuedAt time.Time
}

// AgentSchedSnapshot is the per-agent admission state exposed by Snapshot().
type AgentSchedSnapshot struct {
	Agent    string
	Limit    int
	QueueCap int
	Running  []SchedRunningInfo
	Waiting  []SchedWaitingInfo
	Counters SchedCounters
}

// schedParams are the scheduler tunables, pre-converted to durations.
type schedParams struct {
	syncQueueMax         int
	syncWait             time.Duration
	asyncStarvationAfter time.Duration
}

// slotEntry is one held slot. Identity (pointer) links release() to it.
type slotEntry struct {
	class     slotClass
	meta      SlotMeta
	startedAt time.Time
	released  bool
}

// waiter is one queued slot request. ready is closed exactly once, under
// Scheduler.mu, by either a grant (granted=true, entry set) or Close()
// (err set). An abandoning waiter (ctx cancel / timeout) re-checks granted
// under mu and releases the slot if the grant won the race, so exactly one
// of {consume grant, abandon-release} happens and no slot leaks.
type waiter struct {
	class          slotClass
	meta           SlotMeta
	enqueuedAt     time.Time
	depthAtEnqueue int
	ready          chan struct{}
	granted        bool
	entry          *slotEntry
	err            error
}

// agentSched is the per-agent admission state, guarded by Scheduler.mu.
type agentSched struct {
	name     string
	limit    int
	running  []*slotEntry
	syncQ    []*waiter
	asyncQ   []*waiter
	counters SchedCounters
}

// Scheduler is the shared admission control for sync chat and async task
// dispatch. One mutex guards everything; the lock is never held across I/O
// or while calling another subsystem (onEvent is invoked after unlock).
// Invariant: waiters exist only while running == limit — grants are eager on
// Acquire and inside every release. There is no preemption of running work.
type Scheduler struct {
	mu     sync.Mutex
	agents map[string]*agentSched
	params schedParams
	closed bool

	// now is injectable for tests (aging/starvation).
	now func() time.Time
	// onEvent, when non-nil, receives scheduler lifecycle events. Set it
	// before serving; it is read without the lock.
	onEvent func(SchedEvent)
}

// NewScheduler builds a Scheduler from the (defaulted) config: one entry per
// configured agent with its effective concurrency limit.
func NewScheduler(cfg *Config) *Scheduler {
	limits := make(map[string]int, len(cfg.Agents))
	for name := range cfg.Agents {
		limits[name] = cfg.AgentConcurrency(name)
	}
	return newScheduler(schedParams{
		syncQueueMax:         cfg.Scheduler.SyncQueueMax,
		syncWait:             time.Duration(cfg.Scheduler.SyncQueueWaitS) * time.Second,
		asyncStarvationAfter: time.Duration(cfg.Scheduler.AsyncStarvationAfterS) * time.Second,
	}, limits)
}

// newScheduler is the test-friendly constructor (durations, explicit limits).
func newScheduler(p schedParams, limits map[string]int) *Scheduler {
	s := &Scheduler{
		agents: make(map[string]*agentSched, len(limits)),
		params: p,
		now:    time.Now,
	}
	for name, limit := range limits {
		if limit < 1 {
			limit = 1
		}
		s.agents[name] = &agentSched{name: name, limit: limit}
	}
	return s
}

// Acquire blocks until a slot for agent is granted, then returns a release
// func that must be called exactly once when the run finishes (idempotent).
// Sync requests are rejected with ErrQueueFull when the sync FIFO is at
// sync_queue_max and fail with ErrWaitTimeout after sync_queue_wait_s.
// Async requests wait indefinitely. ctx cancellation abandons the wait.
func (s *Scheduler) Acquire(ctx context.Context, agent string, class slotClass, meta SlotMeta) (release func(), err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if meta.RunID == "" {
		meta.RunID = randHex(4)
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, ErrShuttingDown
	}
	a := s.agents[agent]
	if a == nil {
		s.mu.Unlock()
		return nil, ErrUnknownAgent
	}
	if len(a.running) < a.limit {
		// Eager grant: by invariant the queues are empty here.
		e := &slotEntry{class: class, meta: meta, startedAt: s.now()}
		a.running = append(a.running, e)
		a.counters.Granted++
		s.mu.Unlock()
		s.emit(SchedEvent{Type: SchedEventGranted, Agent: agent, Class: class.String()})
		return s.releaseFunc(a, e), nil
	}
	if class == classSync && len(a.syncQ) >= s.params.syncQueueMax {
		a.counters.RejectedFull++
		s.mu.Unlock()
		s.emit(SchedEvent{Type: SchedEventRejectedFull, Agent: agent, Class: class.String()})
		return nil, ErrQueueFull
	}
	w := &waiter{class: class, meta: meta, enqueuedAt: s.now(), ready: make(chan struct{})}
	if class == classSync {
		a.syncQ = append(a.syncQ, w)
		w.depthAtEnqueue = len(a.syncQ)
	} else {
		a.asyncQ = append(a.asyncQ, w)
		w.depthAtEnqueue = len(a.asyncQ)
	}
	s.mu.Unlock()

	var timeoutC <-chan time.Time
	if class == classSync {
		timer := time.NewTimer(s.params.syncWait)
		defer timer.Stop()
		timeoutC = timer.C
	}

	select {
	case <-w.ready:
		s.mu.Lock()
		if !w.granted {
			// Woken by Close().
			gErr := w.err
			s.mu.Unlock()
			if gErr == nil {
				gErr = ErrShuttingDown
			}
			return nil, gErr
		}
		a.counters.Granted++
		wait := s.now().Sub(w.enqueuedAt)
		e := w.entry
		s.mu.Unlock()
		s.emit(SchedEvent{Type: SchedEventGranted, Agent: agent, Class: class.String(), Wait: wait, QueueDepthAtEnqueue: w.depthAtEnqueue})
		return s.releaseFunc(a, e), nil

	case <-ctx.Done():
		s.mu.Lock()
		if w.granted {
			// Lost the race with a grant; the client is gone, hand the slot on.
			a.counters.WaitCancelled++
			s.releaseEntryLocked(a, w.entry)
			s.mu.Unlock()
			s.emit(SchedEvent{Type: SchedEventWaitCancelled, Agent: agent, Class: class.String(), Wait: 0, QueueDepthAtEnqueue: w.depthAtEnqueue})
			return nil, ctx.Err()
		}
		a.syncQ = removeWaiter(a.syncQ, w)
		a.asyncQ = removeWaiter(a.asyncQ, w)
		a.counters.WaitCancelled++
		s.mu.Unlock()
		s.emit(SchedEvent{Type: SchedEventWaitCancelled, Agent: agent, Class: class.String(), QueueDepthAtEnqueue: w.depthAtEnqueue})
		return nil, ctx.Err()

	case <-timeoutC:
		s.mu.Lock()
		if w.granted {
			// The grant won a race with the timer; the client is still here, use it.
			a.counters.Granted++
			wait := s.now().Sub(w.enqueuedAt)
			e := w.entry
			s.mu.Unlock()
			s.emit(SchedEvent{Type: SchedEventGranted, Agent: agent, Class: class.String(), Wait: wait, QueueDepthAtEnqueue: w.depthAtEnqueue})
			return s.releaseFunc(a, e), nil
		}
		a.syncQ = removeWaiter(a.syncQ, w)
		a.counters.WaitTimeouts++
		s.mu.Unlock()
		s.emit(SchedEvent{Type: SchedEventWaitTimeout, Agent: agent, Class: class.String(), Wait: s.params.syncWait, QueueDepthAtEnqueue: w.depthAtEnqueue})
		return nil, ErrWaitTimeout
	}
}

// releaseFunc builds the idempotent release closure for a held slot.
func (s *Scheduler) releaseFunc(a *agentSched, e *slotEntry) func() {
	return func() {
		s.mu.Lock()
		s.releaseEntryLocked(a, e)
		s.mu.Unlock()
	}
}

// releaseEntryLocked frees a held slot and eagerly grants queued waiters
// until the limit is reached again. Idempotent per entry.
func (s *Scheduler) releaseEntryLocked(a *agentSched, e *slotEntry) {
	if e == nil || e.released {
		return
	}
	e.released = true
	for i, x := range a.running {
		if x == e {
			a.running = append(a.running[:i], a.running[i+1:]...)
			break
		}
	}
	s.grantLoopLocked(a)
}

// grantLoopLocked grants queued waiters while capacity remains.
func (s *Scheduler) grantLoopLocked(a *agentSched) {
	for len(a.running) < a.limit && !s.closed {
		w := s.pickNextLocked(a)
		if w == nil {
			return
		}
		e := &slotEntry{class: w.class, meta: w.meta, startedAt: s.now()}
		a.running = append(a.running, e)
		w.granted = true
		w.entry = e
		close(w.ready)
	}
}

// pickNextLocked implements the fairness rule:
//  1. async head that has waited >= async_starvation_after_s (aging)
//  2. sync FIFO head
//  3. async FIFO head
//  4. nil
func (s *Scheduler) pickNextLocked(a *agentSched) *waiter {
	if len(a.asyncQ) > 0 && s.now().Sub(a.asyncQ[0].enqueuedAt) >= s.params.asyncStarvationAfter {
		w := a.asyncQ[0]
		a.asyncQ = a.asyncQ[1:]
		return w
	}
	if len(a.syncQ) > 0 {
		w := a.syncQ[0]
		a.syncQ = a.syncQ[1:]
		return w
	}
	if len(a.asyncQ) > 0 {
		w := a.asyncQ[0]
		a.asyncQ = a.asyncQ[1:]
		return w
	}
	return nil
}

// Close wakes every waiter with ErrShuttingDown and refuses new Acquires.
// Running slots are unaffected; their releases still work but grant nothing.
func (s *Scheduler) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	for _, a := range s.agents {
		for _, q := range [][]*waiter{a.syncQ, a.asyncQ} {
			for _, w := range q {
				w.err = ErrShuttingDown
				close(w.ready)
			}
		}
		a.syncQ, a.asyncQ = nil, nil
	}
}

// Snapshot returns the full admission state, sorted by agent name, for the
// dashboard and scrape-time metric gauges.
func (s *Scheduler) Snapshot() []AgentSchedSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]AgentSchedSnapshot, 0, len(s.agents))
	for name, a := range s.agents {
		snap := AgentSchedSnapshot{
			Agent:    name,
			Limit:    a.limit,
			QueueCap: s.params.syncQueueMax,
			Counters: a.counters,
		}
		for _, e := range a.running {
			snap.Running = append(snap.Running, SchedRunningInfo{
				RunID:     e.meta.RunID,
				Kind:      e.class.String(),
				TaskID:    e.meta.TaskID,
				SessionID: e.meta.SessionID,
				TraceID:   e.meta.TraceID,
				StartedAt: e.startedAt,
			})
		}
		for _, q := range [][]*waiter{a.syncQ, a.asyncQ} {
			for _, w := range q {
				snap.Waiting = append(snap.Waiting, SchedWaitingInfo{
					RunID:      w.meta.RunID,
					Kind:       w.class.String(),
					TaskID:     w.meta.TaskID,
					Priority:   w.meta.Priority,
					EnqueuedAt: w.enqueuedAt,
				})
			}
		}
		out = append(out, snap)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Agent < out[j].Agent })
	return out
}

// emit delivers an event to the optional hook (nil-safe, called unlocked).
func (s *Scheduler) emit(ev SchedEvent) {
	if h := s.onEvent; h != nil {
		h(ev)
	}
}

// removeWaiter deletes w from q by identity; no-op when absent.
func removeWaiter(q []*waiter, w *waiter) []*waiter {
	for i, x := range q {
		if x == w {
			return append(q[:i], q[i+1:]...)
		}
	}
	return q
}

// randHex returns 2*nBytes lowercase hex chars from crypto/rand. On the
// (practically impossible) failure of the system entropy source it falls
// back to time-derived bytes rather than aborting the request.
func randHex(nBytes int) string {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		now := time.Now().UnixNano()
		for i := range b {
			b[i] = byte(now >> (8 * (i % 8)))
		}
	}
	return hex.EncodeToString(b)
}
