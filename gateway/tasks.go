package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// TaskState is the task lifecycle state (plan §c).
type TaskState string

const (
	TaskPending   TaskState = "pending"
	TaskRunning   TaskState = "running"
	TaskSucceeded TaskState = "succeeded"
	TaskFailed    TaskState = "failed"
	TaskCancelled TaskState = "cancelled"
	TaskExpired   TaskState = "expired"
)

// IsTerminal reports whether the state is final.
func (s TaskState) IsTerminal() bool {
	switch s {
	case TaskSucceeded, TaskFailed, TaskCancelled, TaskExpired:
		return true
	}
	return false
}

// taskEdges is the static transition table; every state change flows through
// transitionLocked, which validates against it.
var taskEdges = map[TaskState]map[TaskState]bool{
	TaskPending: {TaskRunning: true, TaskCancelled: true, TaskExpired: true},
	// running -> pending is a retry requeue (backoff / vm-unavailable refund /
	// interruption recovery).
	TaskRunning: {TaskPending: true, TaskSucceeded: true, TaskFailed: true, TaskCancelled: true, TaskExpired: true},
}

// Task error kinds (TaskError.Kind).
const (
	ErrKindTimeout       = "timeout"
	ErrKindIdle          = "idle"
	ErrKindVMUnreachable = "vm_unreachable"
	ErrKindDownstream    = "downstream_error"
	ErrKindCancelled     = "cancelled"
	ErrKindExpired       = "expired"
	ErrKindInterrupted   = "interrupted"
)

// Attempt outcomes (AttemptRecord.Outcome).
const (
	attemptSucceeded     = "succeeded"
	attemptError         = "error"
	attemptCancelled     = "cancelled"
	attemptInterrupted   = "interrupted"
	attemptVMUnreachable = "vm_unreachable"
)

const (
	// maxAttemptHistory caps attempt_history; oldest entries are dropped.
	maxAttemptHistory = 20
	// maxInlineResult is the largest result.content inlined into the record;
	// the full text is always available via the output spool.
	maxInlineResult = 65536
)

// Sentinel errors returned by TaskStore methods.
var (
	ErrTaskNotFound    = errors.New("tasks: no such task")
	ErrTooManyPending  = errors.New("tasks: too many pending tasks for agent")
	ErrNotTerminal     = errors.New("tasks: task is not terminal")
	ErrBadTransition   = errors.New("tasks: invalid state transition")
	ErrCancelRequested = errors.New("tasks: cancel requested")
)

// TaskError is the terminal (or last-attempt) error on a task.
type TaskError struct {
	Message string `json:"message"`
	Kind    string `json:"kind"` // timeout|idle|vm_unreachable|downstream_error|cancelled|expired|interrupted
}

// TaskResult is set on succeeded tasks only.
type TaskResult struct {
	Content          string          `json:"content"`
	ContentTruncated bool            `json:"content_truncated"`
	OutputBytes      int64           `json:"output_bytes"`
	FinishReason     string          `json:"finish_reason"`
	Usage            json.RawMessage `json:"usage,omitempty"`
}

// TraceRef is a persisted span-context reference (span-link target).
type TraceRef struct {
	TraceID string `json:"trace_id"`
	SpanID  string `json:"span_id"`
}

// AttemptRecord is one entry of attempt_history.
type AttemptRecord struct {
	Attempt     int        `json:"attempt"`
	StartedAt   time.Time  `json:"started_at"`
	EndedAt     *time.Time `json:"ended_at"`
	VMIP        string     `json:"vm_ip,omitempty"`
	Outcome     string     `json:"outcome,omitempty"` // error|succeeded|cancelled|interrupted|vm_unreachable
	Error       string     `json:"error,omitempty"`
	OutputBytes int64      `json:"output_bytes"`
}

// Task is the async task record. Wire format == disk format, except the disk
// record additionally carries "schema":1 (see diskTask). All timestamps are
// RFC3339Nano UTC.
type Task struct {
	Object             string          `json:"object"` // "task"
	ID                 string          `json:"id"`
	Agent              string          `json:"agent"`
	State              TaskState       `json:"state"`
	Priority           int             `json:"priority"`
	CreatedAt          time.Time       `json:"created_at"`
	UpdatedAt          time.Time       `json:"updated_at"`
	NotBefore          *time.Time      `json:"not_before"`
	Deadline           time.Time       `json:"deadline"`
	TimeoutS           int             `json:"timeout_s"`
	IdleTimeoutS       int             `json:"idle_timeout_s"`
	MaxAttempts        int             `json:"max_attempts"`
	RetryOnPartial     bool            `json:"retry_on_partial"`
	Attempts           int             `json:"attempts"`
	VMUnavailableCount int             `json:"vm_unavailable_count"`
	CancelRequested    bool            `json:"cancel_requested"`
	SessionID          string          `json:"session_id"`
	Request            json.RawMessage `json:"request"`
	SubmittedBy        string          `json:"submitted_by,omitempty"`
	SubmitTrace        *TraceRef       `json:"submit_trace,omitempty"`
	TraceIDs           []string        `json:"trace_ids"`
	StartedAt          *time.Time      `json:"started_at"`
	FinishedAt         *time.Time      `json:"finished_at"`
	Error              *TaskError      `json:"error"`
	Result             *TaskResult     `json:"result"`
	AttemptHistory     []AttemptRecord `json:"attempt_history"`
}

// diskTask is the on-disk envelope: the wire record plus a schema marker.
type diskTask struct {
	Schema int `json:"schema"`
	*Task
}

// clone returns a deep copy; *Task never escapes the store.
func (t *Task) clone() Task {
	c := *t
	c.NotBefore = copyTime(t.NotBefore)
	c.StartedAt = copyTime(t.StartedAt)
	c.FinishedAt = copyTime(t.FinishedAt)
	if t.Error != nil {
		e := *t.Error
		c.Error = &e
	}
	if t.Result != nil {
		r := *t.Result
		if t.Result.Usage != nil {
			r.Usage = append(json.RawMessage(nil), t.Result.Usage...)
		}
		c.Result = &r
	}
	if t.SubmitTrace != nil {
		s := *t.SubmitTrace
		c.SubmitTrace = &s
	}
	if t.TraceIDs != nil {
		c.TraceIDs = make([]string, len(t.TraceIDs))
		copy(c.TraceIDs, t.TraceIDs)
	}
	if t.Request != nil {
		c.Request = append(json.RawMessage(nil), t.Request...)
	}
	if t.AttemptHistory != nil {
		c.AttemptHistory = make([]AttemptRecord, len(t.AttemptHistory))
		copy(c.AttemptHistory, t.AttemptHistory)
		for i := range c.AttemptHistory {
			c.AttemptHistory[i].EndedAt = copyTime(t.AttemptHistory[i].EndedAt)
		}
	}
	return c
}

func copyTime(p *time.Time) *time.Time {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

// TaskEvent is delivered to the optional onTransition hook. From is "" for
// the submit event.
type TaskEvent struct {
	Task Task
	From TaskState
	To   TaskState
}

// SubmitSpec is a validated task submission (validation itself lives in the
// API layer). Zero fields take the config defaults.
type SubmitSpec struct {
	Agent          string
	Request        json.RawMessage
	Priority       int
	TimeoutS       int
	DeadlineS      int
	NotBefore      *time.Time
	MaxAttempts    int
	IdleTimeoutS   int
	RetryOnPartial *bool
	SessionID      string
	SubmittedBy    string
	SubmitTrace    *TraceRef
}

// FinishSpec reports the outcome of a running attempt. Err nil means success.
type FinishSpec struct {
	VMIP        string
	OutputBytes int64
	Err         *TaskError
	Result      *TaskResult
}

// TaskFilter selects tasks for List. Zero Limit means no limit. Scope, when
// non-nil, restricts to agents the token scope allows. After is a keyset
// cursor: results strictly after that id in the created_at-desc ordering.
type TaskFilter struct {
	Agent  string
	States []TaskState
	Scope  []string
	Limit  int
	After  string
}

// TaskStoreSnapshot is aggregate store state for the dashboard and metrics.
type TaskStoreSnapshot struct {
	Total            int
	ByState          map[TaskState]int
	ByAgent          map[string]map[TaskState]int
	Orphaned         int
	Degraded         bool
	OldestPendingAge map[string]time.Duration
}

// TaskStore owns every task record: one mutex, copy-in/copy-out, atomic
// rename persistence under <dir>, and a single transition() chokepoint with
// a static edge table. Persist failures keep the in-memory transition, mark
// the record dirty (store degraded) and are retried on the next transition;
// only Submit surfaces the persist error to the caller.
type TaskStore struct {
	mu            sync.Mutex
	dir           string
	cfg           *Config
	tasks         map[string]*Task
	runningCancel map[string]context.CancelCauseFunc
	dirty         map[string]bool
	pokes         map[string]chan struct{}
	// cancelAllCause, once set by CancelAll (shutdown), makes RegisterCancel
	// fire immediately so runners spawned in the Claim-to-register window are
	// still torn down.
	cancelAllCause error

	// now is injectable for tests.
	now func() time.Time
	// onTransition, when non-nil, receives every task state change. Set it
	// before serving; it is read without the lock and called after unlock.
	onTransition func(TaskEvent)
}

// NewTaskStore creates the store rooted at cfg.Tasks.Dir (which LoadConfig
// defaults to <state_dir>/gateway/tasks) and ensures dir and dir/.tmp exist.
func NewTaskStore(cfg *Config) (*TaskStore, error) {
	dir := cfg.Tasks.Dir
	if dir == "" {
		return nil, errors.New("tasks: dir not configured")
	}
	if err := os.MkdirAll(filepath.Join(dir, ".tmp"), 0o755); err != nil {
		return nil, fmt.Errorf("tasks: create dir: %w", err)
	}
	return &TaskStore{
		dir:           dir,
		cfg:           cfg,
		tasks:         make(map[string]*Task),
		runningCancel: make(map[string]context.CancelCauseFunc),
		dirty:         make(map[string]bool),
		pokes:         make(map[string]chan struct{}),
		now:           time.Now,
	}, nil
}

// newTaskID builds "t-<UTC compact ts>-<8 hex crypto/rand>".
func newTaskID(now time.Time) string {
	return "t-" + now.UTC().Format("20060102T150405Z") + "-" + randHex(4)
}

func (st *TaskStore) recordPath(id string) string { return filepath.Join(st.dir, id+".json") }

// OutputPath returns the append-only assistant-content spool for a task.
func (st *TaskStore) OutputPath(id string) string {
	return filepath.Join(st.dir, id+".output.txt")
}

// TruncateOutput empties (creating if needed) the output spool; called by the
// dispatcher at attempt start.
func (st *TaskStore) TruncateOutput(id string) error {
	return os.WriteFile(st.OutputPath(id), nil, 0o644)
}

// spoolSize returns the current spool size (0 when absent).
func (st *TaskStore) spoolSize(id string) int64 {
	fi, err := os.Stat(st.OutputPath(id))
	if err != nil {
		return 0
	}
	return fi.Size()
}

// Submit creates a pending task, persists it (failure => error, task not
// kept) and pokes the agent's dispatcher.
func (st *TaskStore) Submit(spec SubmitSpec) (Task, error) {
	st.mu.Lock()
	now := st.now().UTC()

	if max := st.cfg.Tasks.MaxPendingPerAgent; max > 0 {
		nonTerminal := 0
		for _, t := range st.tasks {
			if t.Agent == spec.Agent && !t.State.IsTerminal() {
				nonTerminal++
			}
		}
		if nonTerminal >= max {
			st.mu.Unlock()
			return Task{}, ErrTooManyPending
		}
	}

	tc := st.cfg.Tasks
	timeout := spec.TimeoutS
	if timeout == 0 {
		timeout = tc.DefaultTimeoutS
	}
	deadlineS := spec.DeadlineS
	if deadlineS == 0 {
		deadlineS = tc.DefaultDeadlineS
	}
	maxAttempts := spec.MaxAttempts
	if maxAttempts == 0 {
		maxAttempts = tc.DefaultMaxAttempts
	}
	idle := spec.IdleTimeoutS
	if idle == 0 {
		idle = tc.IdleTimeoutS
	}
	retryOnPartial := tc.RetryOnPartial
	if spec.RetryOnPartial != nil {
		retryOnPartial = *spec.RetryOnPartial
	}

	id := newTaskID(now)
	sessionID := spec.SessionID
	if sessionID == "" {
		sessionID = "task:" + id
	}
	t := &Task{
		Object:         "task",
		ID:             id,
		Agent:          spec.Agent,
		State:          TaskPending,
		Priority:       spec.Priority,
		CreatedAt:      now,
		UpdatedAt:      now,
		Deadline:       now.Add(time.Duration(deadlineS) * time.Second),
		TimeoutS:       timeout,
		IdleTimeoutS:   idle,
		MaxAttempts:    maxAttempts,
		RetryOnPartial: retryOnPartial,
		SessionID:      sessionID,
		SubmittedBy:    spec.SubmittedBy,
		TraceIDs:       []string{},
		AttemptHistory: []AttemptRecord{},
	}
	if spec.Request != nil {
		t.Request = append(json.RawMessage(nil), spec.Request...)
	}
	if spec.NotBefore != nil {
		nb := spec.NotBefore.UTC()
		t.NotBefore = &nb
	}
	if spec.SubmitTrace != nil {
		tr := *spec.SubmitTrace
		t.SubmitTrace = &tr
	}

	if err := st.writeTaskLocked(t); err != nil {
		st.mu.Unlock()
		return Task{}, fmt.Errorf("tasks: persist new task: %w", err)
	}
	st.tasks[id] = t
	st.pokeLocked(spec.Agent)
	res := t.clone()
	st.mu.Unlock()
	st.emit([]TaskEvent{{Task: res, From: "", To: TaskPending}})
	return res, nil
}

// Get returns a copy of the task.
func (st *TaskStore) Get(id string) (Task, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	t, ok := st.tasks[id]
	if !ok {
		return Task{}, false
	}
	return t.clone(), true
}

// List returns tasks matching f, sorted created_at desc (id desc tiebreak),
// with keyset pagination. hasMore reports whether results were truncated.
func (st *TaskStore) List(f TaskFilter) (items []Task, hasMore bool) {
	st.mu.Lock()
	var matched []*Task
	for _, t := range st.tasks {
		if f.Agent != "" && t.Agent != f.Agent {
			continue
		}
		if f.Scope != nil && !agentAllowed(f.Scope, t.Agent) {
			continue
		}
		if len(f.States) > 0 {
			found := false
			for _, s := range f.States {
				if t.State == s {
					found = true
					break
				}
			}
			if !found {
				continue
			}
		}
		matched = append(matched, t)
	}
	sort.Slice(matched, func(i, j int) bool {
		if !matched[i].CreatedAt.Equal(matched[j].CreatedAt) {
			return matched[i].CreatedAt.After(matched[j].CreatedAt)
		}
		return matched[i].ID > matched[j].ID
	})
	if f.After != "" {
		if cur, ok := st.cursorKeyLocked(f.After); ok {
			kept := matched[:0]
			for _, t := range matched {
				if keysetAfter(t, cur) {
					kept = append(kept, t)
				}
			}
			matched = kept
		}
	}
	if f.Limit > 0 && len(matched) > f.Limit {
		matched = matched[:f.Limit]
		hasMore = true
	}
	items = make([]Task, 0, len(matched))
	for _, t := range matched {
		items = append(items, t.clone())
	}
	st.mu.Unlock()
	return items, hasMore
}

// listCursor is the (created_at, id) sort key of a keyset cursor.
type listCursor struct {
	createdAt time.Time
	id        string
}

// cursorKeyLocked resolves an After cursor to its sort key. A cursor task
// still in the store supplies its exact key — even when it no longer matches
// the filter, so a state change between pages cannot restart pagination. A
// deleted/GC'd cursor falls back to the UTC second embedded in the id
// (newTaskID format), biased to the end of that second so nearby records are
// duplicated rather than skipped. ok is false only for an unparseable id.
func (st *TaskStore) cursorKeyLocked(after string) (listCursor, bool) {
	if t, ok := st.tasks[after]; ok {
		return listCursor{createdAt: t.CreatedAt, id: t.ID}, true
	}
	parts := strings.SplitN(after, "-", 3)
	if len(parts) == 3 {
		if ts, err := time.Parse("20060102T150405Z", parts[1]); err == nil {
			return listCursor{createdAt: ts.Add(time.Second - time.Nanosecond), id: after}, true
		}
	}
	return listCursor{}, false
}

// keysetAfter reports whether t sorts strictly after the cursor in the
// created_at-desc (id-desc tiebreak) ordering List uses.
func keysetAfter(t *Task, cur listCursor) bool {
	if !t.CreatedAt.Equal(cur.createdAt) {
		return t.CreatedAt.Before(cur.createdAt)
	}
	return t.ID < cur.id
}

// Claim picks the runnable pending task for agent with the highest priority
// (created_at asc, then id asc as tiebreaks), transitions it to running
// (attempts++, open attempt record) and returns a copy. Pending tasks past
// their deadline are opportunistically expired during the scan. ok is false
// when nothing is runnable.
func (st *TaskStore) Claim(agent string) (Task, bool) {
	var events []TaskEvent
	st.mu.Lock()
	now := st.now().UTC()
	var best *Task
	for _, t := range st.tasks {
		if t.Agent != agent || t.State != TaskPending {
			continue
		}
		if !now.Before(t.Deadline) {
			if ev, err := st.transitionLocked(t, TaskExpired, st.expireMutate(now, "deadline exceeded before start")); err == nil {
				events = append(events, ev)
			}
			continue
		}
		if t.NotBefore != nil && now.Before(*t.NotBefore) {
			continue
		}
		if best == nil || claimBefore(t, best) {
			best = t
		}
	}
	if best == nil {
		st.mu.Unlock()
		st.emit(events)
		return Task{}, false
	}
	ev, err := st.transitionLocked(best, TaskRunning, func(t *Task) {
		t.Attempts++
		sa := now
		t.StartedAt = &sa
		t.NotBefore = nil
		t.AttemptHistory = append(t.AttemptHistory, AttemptRecord{Attempt: t.Attempts, StartedAt: now})
		if n := len(t.AttemptHistory); n > maxAttemptHistory {
			t.AttemptHistory = append([]AttemptRecord(nil), t.AttemptHistory[n-maxAttemptHistory:]...)
		}
	})
	if err != nil {
		st.mu.Unlock()
		st.emit(events)
		return Task{}, false
	}
	events = append(events, ev)
	res := best.clone()
	st.mu.Unlock()
	st.emit(events)
	return res, true
}

// claimBefore reports whether a should be claimed before b.
func claimBefore(a, b *Task) bool {
	if a.Priority != b.Priority {
		return a.Priority > b.Priority
	}
	if !a.CreatedAt.Equal(b.CreatedAt) {
		return a.CreatedAt.Before(b.CreatedAt)
	}
	return a.ID < b.ID
}

// Finish reports the outcome of a running attempt and applies the
// retriability rules:
//   - cancel requested (or Err.Kind cancelled)      => cancelled
//   - Err.Kind expired or now past deadline         => expired
//   - vm_unreachable                                => attempt refunded
//     (attempts--, vm_unavailable_count++, requeue after vm_unavailable_retry_s;
//     bounded by the deadline, not max_attempts)
//   - interrupted (shutdown)                        => same matrix recovery uses
//   - other errors: retriable while attempts < max_attempts and
//     (no partial output || retry_on_partial)       => requeue with backoff
//   - otherwise                                     => failed
func (st *TaskStore) Finish(id string, spec FinishSpec) (Task, error) {
	var events []TaskEvent
	st.mu.Lock()
	t, ok := st.tasks[id]
	if !ok {
		st.mu.Unlock()
		return Task{}, ErrTaskNotFound
	}
	if t.State != TaskRunning {
		st.mu.Unlock()
		return Task{}, fmt.Errorf("%w: finish in state %s", ErrBadTransition, t.State)
	}
	delete(st.runningCancel, id)
	now := st.now().UTC()

	outcome, msg := attemptSucceeded, ""
	if spec.Err != nil {
		msg = spec.Err.Message
		switch spec.Err.Kind {
		case ErrKindCancelled:
			outcome = attemptCancelled
		case ErrKindVMUnreachable:
			outcome = attemptVMUnreachable
		case ErrKindInterrupted:
			outcome = attemptInterrupted
		default:
			outcome = attemptError
		}
	}
	st.closeAttemptLocked(t, now, spec.VMIP, outcome, msg, spec.OutputBytes)

	var to TaskState
	var mutate func(*Task)
	if spec.Err == nil {
		to = TaskSucceeded
		mutate = func(t *Task) {
			t.Result = spec.Result
			t.Error = nil
			f := now
			t.FinishedAt = &f
		}
	} else {
		taskErr := *spec.Err
		switch {
		case taskErr.Kind == ErrKindCancelled || t.CancelRequested:
			to = TaskCancelled
			mutate = func(t *Task) {
				t.Error = &TaskError{Message: taskErr.Message, Kind: ErrKindCancelled}
				f := now
				t.FinishedAt = &f
			}
		case taskErr.Kind == ErrKindExpired || !now.Before(t.Deadline):
			to = TaskExpired
			mutate = func(t *Task) {
				t.Error = &TaskError{Message: taskErr.Message, Kind: ErrKindExpired}
				f := now
				t.FinishedAt = &f
			}
		case taskErr.Kind == ErrKindVMUnreachable:
			to = TaskPending
			mutate = func(t *Task) {
				t.Attempts--
				t.VMUnavailableCount++
				nb := now.Add(time.Duration(st.cfg.Tasks.VMUnavailableRetryS) * time.Second)
				t.NotBefore = &nb
				t.Error = &taskErr
			}
		case taskErr.Kind == ErrKindInterrupted:
			if (spec.OutputBytes > 0 && !t.RetryOnPartial) || t.Attempts >= t.MaxAttempts {
				to = TaskFailed
				mutate = func(t *Task) {
					t.Error = &taskErr
					f := now
					t.FinishedAt = &f
				}
			} else {
				to = TaskPending
				mutate = st.requeueMutate(now, &taskErr)
			}
		default: // timeout | idle | downstream_error
			retriable := t.Attempts < t.MaxAttempts && (spec.OutputBytes == 0 || t.RetryOnPartial)
			if retriable {
				to = TaskPending
				mutate = st.requeueMutate(now, &taskErr)
			} else {
				to = TaskFailed
				mutate = func(t *Task) {
					t.Error = &taskErr
					f := now
					t.FinishedAt = &f
				}
			}
		}
	}
	ev, err := st.transitionLocked(t, to, mutate)
	if err != nil {
		st.mu.Unlock()
		return Task{}, err
	}
	events = append(events, ev)
	res := t.clone()
	st.mu.Unlock()
	st.emit(events)
	return res, nil
}

// requeueMutate builds the mutate for a retry requeue with backoff.
func (st *TaskStore) requeueMutate(now time.Time, taskErr *TaskError) func(*Task) {
	return func(t *Task) {
		nb := now.Add(st.backoff(t.Attempts))
		t.NotBefore = &nb
		t.Error = taskErr
	}
}

// closeAttemptLocked fills the still-open last attempt record, if any.
func (st *TaskStore) closeAttemptLocked(t *Task, now time.Time, vmIP, outcome, msg string, outputBytes int64) {
	n := len(t.AttemptHistory)
	if n == 0 || t.AttemptHistory[n-1].EndedAt != nil {
		return
	}
	rec := &t.AttemptHistory[n-1]
	e := now
	rec.EndedAt = &e
	rec.VMIP = vmIP
	rec.Outcome = outcome
	rec.Error = msg
	rec.OutputBytes = outputBytes
}

// backoff returns min(base*2^(n-1), cap) for attempt count n.
func (st *TaskStore) backoff(attempts int) time.Duration {
	return backoffDelay(st.cfg.Tasks.RetryBackoffBaseS, st.cfg.Tasks.RetryBackoffCapS, attempts)
}

// backoffDelay computes min(baseS*2^(attempts-1), capS) seconds.
func backoffDelay(baseS, capS, attempts int) time.Duration {
	capD := time.Duration(capS) * time.Second
	d := time.Duration(baseS) * time.Second
	for i := 1; i < attempts; i++ {
		d *= 2
		if d >= capD {
			break
		}
	}
	if d > capD {
		d = capD
	}
	return d
}

// Cancel requests cancellation. Terminal tasks are returned unchanged with
// alreadyTerminal=true (idempotent). Pending tasks transition to cancelled
// immediately; running tasks get cancel_requested set and their runner
// context cancelled — the runner's Finish completes the transition.
func (st *TaskStore) Cancel(id string) (task Task, alreadyTerminal bool, err error) {
	var events []TaskEvent
	var cancelFn context.CancelCauseFunc
	st.mu.Lock()
	t, ok := st.tasks[id]
	if !ok {
		st.mu.Unlock()
		return Task{}, false, ErrTaskNotFound
	}
	now := st.now().UTC()
	switch {
	case t.State.IsTerminal():
		res := t.clone()
		st.mu.Unlock()
		return res, true, nil
	case t.State == TaskPending:
		ev, terr := st.transitionLocked(t, TaskCancelled, func(t *Task) {
			t.CancelRequested = true
			t.Error = &TaskError{Message: "cancelled before start", Kind: ErrKindCancelled}
			f := now
			t.FinishedAt = &f
		})
		if terr != nil {
			st.mu.Unlock()
			return Task{}, false, terr
		}
		events = append(events, ev)
		// Poke so the dispatcher recomputes its wake timer.
		st.pokeLocked(t.Agent)
	default: // running
		t.CancelRequested = true
		t.UpdatedAt = now
		st.persistLocked(t)
		cancelFn = st.runningCancel[id]
	}
	res := t.clone()
	st.mu.Unlock()
	st.emit(events)
	if cancelFn != nil {
		cancelFn(ErrCancelRequested)
	}
	return res, false, nil
}

// Delete removes a terminal task and its sidecars. Non-terminal => ErrNotTerminal.
func (st *TaskStore) Delete(id string) error {
	st.mu.Lock()
	t, ok := st.tasks[id]
	if !ok {
		st.mu.Unlock()
		return ErrTaskNotFound
	}
	if !t.State.IsTerminal() {
		st.mu.Unlock()
		return ErrNotTerminal
	}
	delete(st.tasks, id)
	delete(st.dirty, id)
	st.mu.Unlock()
	os.Remove(st.recordPath(id))
	os.Remove(st.OutputPath(id))
	return nil
}

// NextWake returns the earliest time at which some pending task for agent is
// (or becomes) runnable; the time may be in the past, meaning "now". ok is
// false when the agent has no pending tasks.
func (st *TaskStore) NextWake(agent string) (time.Time, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	var best time.Time
	found := false
	for _, t := range st.tasks {
		if t.Agent != agent || t.State != TaskPending {
			continue
		}
		ready := t.CreatedAt
		if t.NotBefore != nil && t.NotBefore.After(ready) {
			ready = *t.NotBefore
		}
		if !found || ready.Before(best) {
			best = ready
			found = true
		}
	}
	return best, found
}

// Runnable returns the agent's poke channel (cap 1). The dispatcher selects
// on it; the store pokes on submit, retry requeue and cancel-of-pending.
func (st *TaskStore) Runnable(agent string) <-chan struct{} {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.pokeChLocked(agent)
}

func (st *TaskStore) pokeChLocked(agent string) chan struct{} {
	ch, ok := st.pokes[agent]
	if !ok {
		ch = make(chan struct{}, 1)
		st.pokes[agent] = ch
	}
	return ch
}

func (st *TaskStore) pokeLocked(agent string) {
	select {
	case st.pokeChLocked(agent) <- struct{}{}:
	default:
	}
}

// RegisterCancel records the cancel func for a running attempt (used by
// Cancel and by shutdown). Finish removes it. A cancel request (or shutdown
// CancelAll) that landed between Claim and this registration fires the func
// immediately, so the attempt is still torn down.
func (st *TaskStore) RegisterCancel(id string, cancel context.CancelCauseFunc) {
	st.mu.Lock()
	st.runningCancel[id] = cancel
	var cause error
	if t, ok := st.tasks[id]; ok && t.CancelRequested {
		cause = ErrCancelRequested
	} else if st.cancelAllCause != nil {
		cause = st.cancelAllCause
	}
	st.mu.Unlock()
	if cause != nil {
		cancel(cause)
	}
}

// CancelAll invokes every registered running-attempt cancel func with cause
// (shutdown path). The funcs are called after the lock is dropped. The cause
// is remembered so late RegisterCancel calls fire immediately.
func (st *TaskStore) CancelAll(cause error) {
	st.mu.Lock()
	st.cancelAllCause = cause
	fns := make([]context.CancelCauseFunc, 0, len(st.runningCancel))
	for _, fn := range st.runningCancel {
		fns = append(fns, fn)
	}
	st.mu.Unlock()
	for _, fn := range fns {
		fn(cause)
	}
}

// AppendTraceID records a per-attempt root trace id (dashboard join).
func (st *TaskStore) AppendTraceID(id, traceID string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	t, ok := st.tasks[id]
	if !ok {
		return
	}
	t.TraceIDs = append(t.TraceIDs, traceID)
	t.UpdatedAt = st.now().UTC()
	st.persistLocked(t)
}

// Degraded reports whether any record failed its last persist.
func (st *TaskStore) Degraded() bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	return len(st.dirty) > 0
}

// Snapshot returns aggregate counts for the dashboard and metric gauges.
func (st *TaskStore) Snapshot() TaskStoreSnapshot {
	st.mu.Lock()
	defer st.mu.Unlock()
	now := st.now().UTC()
	snap := TaskStoreSnapshot{
		Total:            len(st.tasks),
		ByState:          make(map[TaskState]int),
		ByAgent:          make(map[string]map[TaskState]int),
		Degraded:         len(st.dirty) > 0,
		OldestPendingAge: make(map[string]time.Duration),
	}
	for _, t := range st.tasks {
		snap.ByState[t.State]++
		byAgent := snap.ByAgent[t.Agent]
		if byAgent == nil {
			byAgent = make(map[TaskState]int)
			snap.ByAgent[t.Agent] = byAgent
		}
		byAgent[t.State]++
		if _, ok := st.cfg.Agents[t.Agent]; !ok {
			snap.Orphaned++
		}
		if t.State == TaskPending {
			age := now.Sub(t.CreatedAt)
			if cur, ok := snap.OldestPendingAge[t.Agent]; !ok || age > cur {
				snap.OldestPendingAge[t.Agent] = age
			}
		}
	}
	return snap
}

// transitionLocked is the single state-change chokepoint: it validates the
// edge, applies the mutation, bumps updated_at, persists atomically (failure
// => degraded, in-memory state kept) and pokes the dispatcher on requeue.
// The returned event must be emitted after the lock is dropped.
func (st *TaskStore) transitionLocked(t *Task, to TaskState, mutate func(*Task)) (TaskEvent, error) {
	from := t.State
	if !taskEdges[from][to] {
		return TaskEvent{}, fmt.Errorf("%w: %s -> %s", ErrBadTransition, from, to)
	}
	if mutate != nil {
		mutate(t)
	}
	t.State = to
	t.UpdatedAt = st.now().UTC()
	st.persistLocked(t)
	if to == TaskPending {
		st.pokeLocked(t.Agent)
	}
	return TaskEvent{Task: t.clone(), From: from, To: to}, nil
}

// expireMutate builds the mutate for a deadline expiry.
func (st *TaskStore) expireMutate(now time.Time, msg string) func(*Task) {
	return func(t *Task) {
		t.Error = &TaskError{Message: msg, Kind: ErrKindExpired}
		f := now
		t.FinishedAt = &f
	}
}

// persistLocked writes t and, on success, retries any dirty records. A
// failure marks t dirty (store degraded); in-memory state stays authoritative.
func (st *TaskStore) persistLocked(t *Task) {
	if err := st.writeTaskLocked(t); err != nil {
		if !st.dirty[t.ID] {
			logError("store_error", "task_id", t.ID, "detail", "persist failed; store degraded", "err", err.Error())
		}
		st.dirty[t.ID] = true
		return
	}
	delete(st.dirty, t.ID)
	for id := range st.dirty {
		other := st.tasks[id]
		if other == nil {
			delete(st.dirty, id)
			continue
		}
		if err := st.writeTaskLocked(other); err == nil {
			logInfo("store_error", "task_id", id, "detail", "persist recovered")
			delete(st.dirty, id)
		}
	}
}

// writeTaskLocked atomically persists one record: write <dir>/.tmp/<id>.json.<rand>,
// fsync, then rename over <dir>/<id>.json.
func (st *TaskStore) writeTaskLocked(t *Task) error {
	data, err := json.Marshal(diskTask{Schema: 1, Task: t})
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp := filepath.Join(st.dir, ".tmp", fmt.Sprintf("%s.json.%s", t.ID, randHex(4)))
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, st.recordPath(t.ID)); err != nil {
		os.Remove(tmp)
		return err
	}
	// fsync the directory so the rename itself survives power loss (the tmp
	// file was already synced; an acknowledged Submit must be durable).
	d, err := os.Open(st.dir)
	if err != nil {
		return err
	}
	err = d.Sync()
	d.Close()
	return err
}

// RecoverOnBoot loads every record from disk and applies the recovery matrix:
//   - .tmp is purged; unparseable records are renamed *.corrupt and skipped
//   - orphaned tasks (agent no longer configured) are counted and logged;
//     they are never claimed and expire at their deadline via the sweeper
//   - pending past deadline            => expired
//   - running: cancel_requested        => cancelled
//     past deadline                    => expired
//     spool > 0 && !retry_on_partial   => failed(interrupted)
//     attempts >= max_attempts         => failed(interrupted)
//     otherwise                        => pending with not_before = now+backoff
//     (claim-time attempts++ was already counted: a crash burns an attempt)
func (st *TaskStore) RecoverOnBoot() error {
	var events []TaskEvent
	st.mu.Lock()
	now := st.now().UTC()

	tmpDir := filepath.Join(st.dir, ".tmp")
	if entries, err := os.ReadDir(tmpDir); err == nil {
		for _, e := range entries {
			os.Remove(filepath.Join(tmpDir, e.Name()))
		}
	}

	entries, err := os.ReadDir(st.dir)
	if err != nil {
		st.mu.Unlock()
		return fmt.Errorf("tasks: read dir: %w", err)
	}
	loaded, corrupt, orphans := 0, 0, 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		path := filepath.Join(st.dir, name)
		var t Task
		ok := false
		if data, rerr := os.ReadFile(path); rerr == nil {
			dt := diskTask{Task: &t}
			ok = json.Unmarshal(data, &dt) == nil && t.ID != "" && t.ID+".json" == name
		}
		if !ok {
			corrupt++
			logWarn("store_error", "record", name, "detail", "quarantining corrupt record")
			os.Rename(path, path+".corrupt")
			continue
		}
		tt := t
		st.tasks[tt.ID] = &tt
		loaded++
	}

	agentsWithPending := make(map[string]bool)
	for _, t := range st.tasks {
		if _, ok := st.cfg.Agents[t.Agent]; !ok {
			orphans++
			logWarn("store_error", "task_id", t.ID, "agent", t.Agent,
				"detail", "task references unconfigured agent (orphaned; expires at deadline)")
		}
		switch t.State {
		case TaskPending:
			if !now.Before(t.Deadline) {
				if ev, terr := st.transitionLocked(t, TaskExpired, st.expireMutate(now, "deadline exceeded during downtime")); terr == nil {
					events = append(events, ev)
				}
			}
		case TaskRunning:
			if ev, ok := st.recoverRunningLocked(t, now); ok {
				events = append(events, ev)
			}
		}
		if t.State == TaskPending {
			agentsWithPending[t.Agent] = true
		}
	}
	for agent := range agentsWithPending {
		st.pokeLocked(agent)
	}
	if loaded > 0 || corrupt > 0 {
		logInfo("startup", "component", "tasks", "loaded", loaded, "corrupt", corrupt, "orphaned", orphans)
	}
	st.mu.Unlock()
	st.emit(events)
	return nil
}

// recoverRunningLocked applies the interruption rules to a task found in
// state running at boot (or finalized the same way at shutdown).
func (st *TaskStore) recoverRunningLocked(t *Task, now time.Time) (TaskEvent, bool) {
	spool := st.spoolSize(t.ID)
	var ev TaskEvent
	var terr error
	switch {
	case t.CancelRequested:
		st.closeAttemptLocked(t, now, "", attemptCancelled, "cancel requested before gateway restart", spool)
		ev, terr = st.transitionLocked(t, TaskCancelled, func(t *Task) {
			t.Error = &TaskError{Message: "cancel requested before gateway restart", Kind: ErrKindCancelled}
			f := now
			t.FinishedAt = &f
		})
	case !now.Before(t.Deadline):
		st.closeAttemptLocked(t, now, "", attemptInterrupted, "gateway restarted during attempt", spool)
		ev, terr = st.transitionLocked(t, TaskExpired, st.expireMutate(now, "deadline exceeded during downtime"))
	case spool > 0 && !t.RetryOnPartial:
		st.closeAttemptLocked(t, now, "", attemptInterrupted, "gateway restarted mid-attempt with partial output", spool)
		ev, terr = st.transitionLocked(t, TaskFailed, func(t *Task) {
			t.Error = &TaskError{Message: "gateway restarted mid-attempt with partial output", Kind: ErrKindInterrupted}
			f := now
			t.FinishedAt = &f
		})
	case t.Attempts >= t.MaxAttempts:
		st.closeAttemptLocked(t, now, "", attemptInterrupted, "gateway restarted during final attempt", spool)
		ev, terr = st.transitionLocked(t, TaskFailed, func(t *Task) {
			t.Error = &TaskError{Message: "gateway restarted during final attempt", Kind: ErrKindInterrupted}
			f := now
			t.FinishedAt = &f
		})
	default:
		st.closeAttemptLocked(t, now, "", attemptInterrupted, "gateway restarted during attempt", spool)
		ev, terr = st.transitionLocked(t, TaskPending, st.requeueMutate(now,
			&TaskError{Message: "gateway restarted during attempt", Kind: ErrKindInterrupted}))
	}
	return ev, terr == nil
}

// sweepDeadlines expires pending tasks past their deadline (30s tick).
func (st *TaskStore) sweepDeadlines(now time.Time) {
	var events []TaskEvent
	st.mu.Lock()
	for _, t := range st.tasks {
		if t.State == TaskPending && !now.Before(t.Deadline) {
			if ev, err := st.transitionLocked(t, TaskExpired, st.expireMutate(now, "deadline exceeded before start")); err == nil {
				events = append(events, ev)
			}
		}
	}
	st.mu.Unlock()
	st.emit(events)
}

// sweepGC deletes terminal records older than retention_h and enforces
// max_records (oldest terminal first). Sidecars are deleted with records.
// Non-terminal tasks are never garbage-collected.
func (st *TaskStore) sweepGC(now time.Time) {
	terminalRef := func(t *Task) time.Time {
		if t.FinishedAt != nil {
			return *t.FinishedAt
		}
		return t.UpdatedAt
	}
	st.mu.Lock()
	retention := time.Duration(st.cfg.Tasks.RetentionH) * time.Hour
	var victims []string
	var terminals []*Task
	for _, t := range st.tasks {
		if !t.State.IsTerminal() {
			continue
		}
		if now.Sub(terminalRef(t)) > retention {
			victims = append(victims, t.ID)
		} else {
			terminals = append(terminals, t)
		}
	}
	for _, id := range victims {
		delete(st.tasks, id)
		delete(st.dirty, id)
	}
	if max := st.cfg.Tasks.MaxRecords; max > 0 && len(st.tasks) > max {
		sort.Slice(terminals, func(i, j int) bool {
			return terminalRef(terminals[i]).Before(terminalRef(terminals[j]))
		})
		for _, t := range terminals {
			if len(st.tasks) <= max {
				break
			}
			delete(st.tasks, t.ID)
			delete(st.dirty, t.ID)
			victims = append(victims, t.ID)
		}
	}
	st.mu.Unlock()
	for _, id := range victims {
		os.Remove(st.recordPath(id))
		os.Remove(st.OutputPath(id))
	}
	if len(victims) > 0 {
		logInfo("task_gc", "removed", len(victims))
	}
}

// RunSweeper runs the deadline-expiry (30s) and GC (10m) ticks until ctx is
// cancelled. main.go launches it in its own goroutine when tasks are enabled.
func (st *TaskStore) RunSweeper(ctx context.Context) {
	deadlineTick := time.NewTicker(30 * time.Second)
	gcTick := time.NewTicker(10 * time.Minute)
	defer deadlineTick.Stop()
	defer gcTick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-deadlineTick.C:
			st.sweepDeadlines(st.now().UTC())
		case <-gcTick.C:
			st.sweepGC(st.now().UTC())
		}
	}
}

// emit delivers events to the optional hook (nil-safe, called unlocked).
func (st *TaskStore) emit(events []TaskEvent) {
	h := st.onTransition
	if h == nil {
		return
	}
	for _, ev := range events {
		h(ev)
	}
}
