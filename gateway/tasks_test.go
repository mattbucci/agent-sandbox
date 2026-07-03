package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// taskTestConfig builds a defaulted config whose task dir lives in a temp dir.
func taskTestConfig(t *testing.T) *Config {
	t.Helper()
	cfg := &Config{
		StateDir: t.TempDir(),
		Agents: map[string]AgentConfig{
			"feature-dev": {},
			"debugger":    {},
		},
	}
	cfg.applyDefaults()
	return cfg
}

// newTestStore builds a store with an injected clock.
func newTestStore(t *testing.T, cfg *Config) (*TaskStore, *fakeClock) {
	t.Helper()
	st, err := NewTaskStore(cfg)
	if err != nil {
		t.Fatalf("NewTaskStore: %v", err)
	}
	clk := newFakeClock()
	st.now = clk.Now
	return st, clk
}

// reopenStore simulates a restart: a fresh store over the same dir + recovery.
func reopenStore(t *testing.T, cfg *Config, clk *fakeClock) *TaskStore {
	t.Helper()
	st, err := NewTaskStore(cfg)
	if err != nil {
		t.Fatalf("NewTaskStore (reopen): %v", err)
	}
	st.now = clk.Now
	if err := st.RecoverOnBoot(); err != nil {
		t.Fatalf("RecoverOnBoot: %v", err)
	}
	return st
}

func mustSubmit(t *testing.T, st *TaskStore, spec SubmitSpec) Task {
	t.Helper()
	task, err := st.Submit(spec)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	return task
}

func mustClaim(t *testing.T, st *TaskStore, agent string) Task {
	t.Helper()
	task, ok := st.Claim(agent)
	if !ok {
		t.Fatalf("Claim(%s): nothing runnable", agent)
	}
	return task
}

// TestSubmitDefaultsAndPersistRoundTrip: submitted fields plus config
// defaults survive an atomic-persist round trip into a fresh store.
func TestSubmitDefaultsAndPersistRoundTrip(t *testing.T) {
	cfg := taskTestConfig(t)
	st, clk := newTestStore(t, cfg)
	nb := clk.Now().Add(time.Hour)
	task := mustSubmit(t, st, SubmitSpec{
		Agent:       "feature-dev",
		Request:     json.RawMessage(`{"messages":[{"role":"user","content":"hi"}]}`),
		Priority:    10,
		NotBefore:   &nb,
		SubmittedBy: "hermes-webui",
		SubmitTrace: &TraceRef{TraceID: strings.Repeat("ab", 16), SpanID: strings.Repeat("cd", 8)},
	})

	if !strings.HasPrefix(task.ID, "t-20260702T120000Z-") || len(task.ID) != len("t-20260702T120000Z-")+8 {
		t.Fatalf("task id format wrong: %q", task.ID)
	}
	if task.Object != "task" || task.State != TaskPending || task.Priority != 10 {
		t.Fatalf("basic fields wrong: %+v", task)
	}
	if task.TimeoutS != 3600 || task.IdleTimeoutS != 900 || task.MaxAttempts != 2 || task.RetryOnPartial {
		t.Fatalf("config defaults not applied: %+v", task)
	}
	if want := clk.Now().Add(86400 * time.Second); !task.Deadline.Equal(want) {
		t.Fatalf("deadline = %v, want %v", task.Deadline, want)
	}
	if task.SessionID != "task:"+task.ID {
		t.Fatalf("session id = %q", task.SessionID)
	}
	if task.NotBefore == nil || !task.NotBefore.Equal(nb.UTC()) {
		t.Fatalf("not_before = %v", task.NotBefore)
	}

	// Disk record carries schema:1.
	data, err := os.ReadFile(filepath.Join(cfg.Tasks.Dir, task.ID+".json"))
	if err != nil {
		t.Fatalf("read record: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("parse record: %v", err)
	}
	if raw["schema"] != float64(1) {
		t.Fatalf("schema = %v, want 1", raw["schema"])
	}

	// Fresh store sees the identical record.
	st2 := reopenStore(t, cfg, clk)
	got, ok := st2.Get(task.ID)
	if !ok {
		t.Fatal("task missing after reopen")
	}
	if got.State != TaskPending || got.Priority != 10 || got.SessionID != task.SessionID ||
		got.SubmittedBy != "hermes-webui" || got.SubmitTrace == nil ||
		got.SubmitTrace.TraceID != task.SubmitTrace.TraceID ||
		!got.Deadline.Equal(task.Deadline) || !got.CreatedAt.Equal(task.CreatedAt) ||
		got.NotBefore == nil || !got.NotBefore.Equal(*task.NotBefore) ||
		string(got.Request) != string(task.Request) {
		t.Fatalf("round trip mismatch:\n got %+v\nwant %+v", got, task)
	}
}

// TestClaimPriorityThenFIFO: max priority wins, then created_at asc.
func TestClaimPriorityThenFIFO(t *testing.T) {
	cfg := taskTestConfig(t)
	st, clk := newTestStore(t, cfg)
	submit := func(prio int) string {
		id := mustSubmit(t, st, SubmitSpec{Agent: "feature-dev", Priority: prio}).ID
		clk.Advance(time.Second)
		return id
	}
	id0, id10, id5 := submit(0), submit(10), submit(5)
	fifoA, fifoB := submit(0), submit(0)

	wantOrder := []string{id10, id5, id0, fifoA, fifoB}
	for i, want := range wantOrder {
		got := mustClaim(t, st, "feature-dev")
		if got.ID != want {
			t.Fatalf("claim %d: got %s, want %s", i, got.ID, want)
		}
		if got.State != TaskRunning || got.Attempts != 1 || got.StartedAt == nil || len(got.AttemptHistory) != 1 {
			t.Fatalf("claimed task state wrong: %+v", got)
		}
	}
	if _, ok := st.Claim("feature-dev"); ok {
		t.Fatal("claim should be empty")
	}
}

// TestClaimHonorsNotBeforeAndNextWake.
func TestClaimHonorsNotBeforeAndNextWake(t *testing.T) {
	cfg := taskTestConfig(t)
	st, clk := newTestStore(t, cfg)
	nb := clk.Now().Add(10 * time.Minute)
	task := mustSubmit(t, st, SubmitSpec{Agent: "feature-dev", NotBefore: &nb})

	if _, ok := st.Claim("feature-dev"); ok {
		t.Fatal("claimed before not_before")
	}
	wake, ok := st.NextWake("feature-dev")
	if !ok || !wake.Equal(nb.UTC()) {
		t.Fatalf("NextWake = %v/%v, want %v", wake, ok, nb.UTC())
	}
	clk.Advance(10 * time.Minute)
	if got := mustClaim(t, st, "feature-dev"); got.ID != task.ID {
		t.Fatalf("claimed %s, want %s", got.ID, task.ID)
	}
	if _, ok := st.NextWake("feature-dev"); ok {
		t.Fatal("NextWake should report nothing pending")
	}
}

// TestDeadlineExpiry covers both claim-time and sweeper expiry.
func TestDeadlineExpiry(t *testing.T) {
	cfg := taskTestConfig(t)
	st, clk := newTestStore(t, cfg)
	a := mustSubmit(t, st, SubmitSpec{Agent: "feature-dev", DeadlineS: 60})
	b := mustSubmit(t, st, SubmitSpec{Agent: "debugger", DeadlineS: 60})
	clk.Advance(2 * time.Hour)

	// Claim-time opportunistic expiry.
	if _, ok := st.Claim("feature-dev"); ok {
		t.Fatal("expired task was claimed")
	}
	got, _ := st.Get(a.ID)
	if got.State != TaskExpired || got.Error == nil || got.Error.Kind != ErrKindExpired || got.FinishedAt == nil {
		t.Fatalf("claim-time expiry wrong: %+v", got)
	}

	// Sweeper expiry.
	st.sweepDeadlines(clk.Now())
	got, _ = st.Get(b.ID)
	if got.State != TaskExpired || got.Error == nil || got.Error.Kind != ErrKindExpired {
		t.Fatalf("sweeper expiry wrong: %+v", got)
	}
}

// TestBackoffSchedule: backoff(n) = min(base*2^(n-1), cap).
func TestBackoffSchedule(t *testing.T) {
	want := map[int]time.Duration{
		1: 10 * time.Second,
		2: 20 * time.Second,
		3: 40 * time.Second,
		4: 80 * time.Second,
		5: 160 * time.Second,
		6: 320 * time.Second,
		7: 600 * time.Second,
		8: 600 * time.Second,
	}
	for n, w := range want {
		if got := backoffDelay(10, 600, n); got != w {
			t.Fatalf("backoff(%d) = %v, want %v", n, got, w)
		}
	}
}

// TestFinishSuccess records the result and closes the attempt.
func TestFinishSuccess(t *testing.T) {
	cfg := taskTestConfig(t)
	st, clk := newTestStore(t, cfg)
	task := mustSubmit(t, st, SubmitSpec{Agent: "feature-dev"})
	mustClaim(t, st, "feature-dev")
	clk.Advance(time.Minute)

	got, err := st.Finish(task.ID, FinishSpec{
		VMIP:        "10.0.2.2",
		OutputBytes: 42,
		Result:      &TaskResult{Content: "hello", OutputBytes: 42, FinishReason: "stop"},
	})
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if got.State != TaskSucceeded || got.Result == nil || got.Result.Content != "hello" ||
		got.Error != nil || got.FinishedAt == nil {
		t.Fatalf("success state wrong: %+v", got)
	}
	rec := got.AttemptHistory[0]
	if rec.Outcome != "succeeded" || rec.VMIP != "10.0.2.2" || rec.EndedAt == nil || rec.OutputBytes != 42 {
		t.Fatalf("attempt record wrong: %+v", rec)
	}
}

// TestFinishRetryThenExhaust: zero-output errors retry with backoff while
// attempts < max_attempts, then fail.
func TestFinishRetryThenExhaust(t *testing.T) {
	cfg := taskTestConfig(t)
	st, clk := newTestStore(t, cfg)
	task := mustSubmit(t, st, SubmitSpec{Agent: "feature-dev", MaxAttempts: 2})
	mustClaim(t, st, "feature-dev")

	got, err := st.Finish(task.ID, FinishSpec{Err: &TaskError{Message: "boom", Kind: ErrKindDownstream}})
	if err != nil {
		t.Fatalf("Finish 1: %v", err)
	}
	if got.State != TaskPending || got.Attempts != 1 || got.NotBefore == nil {
		t.Fatalf("retry state wrong: %+v", got)
	}
	if want := clk.Now().Add(10 * time.Second); !got.NotBefore.Equal(want) {
		t.Fatalf("not_before = %v, want %v (backoff(1))", got.NotBefore, want)
	}
	if got.Error == nil || got.Error.Kind != ErrKindDownstream {
		t.Fatalf("last error not retained: %+v", got.Error)
	}

	clk.Advance(time.Minute)
	claimed := mustClaim(t, st, "feature-dev")
	if claimed.Attempts != 2 {
		t.Fatalf("attempts = %d, want 2", claimed.Attempts)
	}
	got, err = st.Finish(task.ID, FinishSpec{Err: &TaskError{Message: "boom again", Kind: ErrKindTimeout}})
	if err != nil {
		t.Fatalf("Finish 2: %v", err)
	}
	if got.State != TaskFailed || got.Error == nil || got.Error.Kind != ErrKindTimeout || got.FinishedAt == nil {
		t.Fatalf("exhausted state wrong: %+v", got)
	}
	if len(got.AttemptHistory) != 2 {
		t.Fatalf("attempt history = %d entries, want 2", len(got.AttemptHistory))
	}
}

// TestFinishPartialOutput: partial output is only retriable with
// retry_on_partial.
func TestFinishPartialOutput(t *testing.T) {
	cfg := taskTestConfig(t)
	st, _ := newTestStore(t, cfg)

	noRetry := mustSubmit(t, st, SubmitSpec{Agent: "feature-dev", MaxAttempts: 3})
	mustClaim(t, st, "feature-dev")
	got, err := st.Finish(noRetry.ID, FinishSpec{OutputBytes: 100, Err: &TaskError{Message: "died", Kind: ErrKindDownstream}})
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if got.State != TaskFailed {
		t.Fatalf("partial output without retry_on_partial: state = %s, want failed", got.State)
	}

	retry := true
	withRetry := mustSubmit(t, st, SubmitSpec{Agent: "feature-dev", MaxAttempts: 3, RetryOnPartial: &retry})
	mustClaim(t, st, "feature-dev")
	got, err = st.Finish(withRetry.ID, FinishSpec{OutputBytes: 100, Err: &TaskError{Message: "died", Kind: ErrKindDownstream}})
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if got.State != TaskPending {
		t.Fatalf("partial output with retry_on_partial: state = %s, want pending", got.State)
	}
}

// TestVMUnavailableRefund: no-VM outcomes refund the attempt and requeue
// after vm_unavailable_retry_s, bounded by the deadline not max_attempts.
func TestVMUnavailableRefund(t *testing.T) {
	cfg := taskTestConfig(t)
	st, clk := newTestStore(t, cfg)
	task := mustSubmit(t, st, SubmitSpec{Agent: "feature-dev", MaxAttempts: 2})
	mustClaim(t, st, "feature-dev")

	got, err := st.Finish(task.ID, FinishSpec{Err: &TaskError{Message: "no running VM", Kind: ErrKindVMUnreachable}})
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if got.State != TaskPending || got.Attempts != 0 || got.VMUnavailableCount != 1 {
		t.Fatalf("refund wrong: state=%s attempts=%d vm_unavailable_count=%d", got.State, got.Attempts, got.VMUnavailableCount)
	}
	if want := clk.Now().Add(30 * time.Second); got.NotBefore == nil || !got.NotBefore.Equal(want) {
		t.Fatalf("not_before = %v, want %v", got.NotBefore, want)
	}
	if got.AttemptHistory[0].Outcome != "vm_unreachable" {
		t.Fatalf("attempt outcome = %q", got.AttemptHistory[0].Outcome)
	}
	if _, ok := st.Claim("feature-dev"); ok {
		t.Fatal("claimed before vm retry delay")
	}
	clk.Advance(time.Minute)
	if claimed := mustClaim(t, st, "feature-dev"); claimed.Attempts != 1 {
		t.Fatalf("attempts after refund reclaim = %d, want 1", claimed.Attempts)
	}
}

// TestFinishPastDeadlineExpires: any error past the deadline is expiry.
func TestFinishPastDeadlineExpires(t *testing.T) {
	cfg := taskTestConfig(t)
	st, clk := newTestStore(t, cfg)
	task := mustSubmit(t, st, SubmitSpec{Agent: "feature-dev", DeadlineS: 60, MaxAttempts: 5})
	mustClaim(t, st, "feature-dev")
	clk.Advance(2 * time.Hour)
	got, err := st.Finish(task.ID, FinishSpec{Err: &TaskError{Message: "attempt timed out", Kind: ErrKindTimeout}})
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if got.State != TaskExpired || got.Error == nil || got.Error.Kind != ErrKindExpired {
		t.Fatalf("state = %+v, want expired", got)
	}
}

// TestCancelSemantics: pending cancels immediately; running sets the flag and
// fires the registered cancel func; terminal cancel is idempotent.
func TestCancelSemantics(t *testing.T) {
	cfg := taskTestConfig(t)
	st, _ := newTestStore(t, cfg)

	// Pending.
	pending := mustSubmit(t, st, SubmitSpec{Agent: "feature-dev", Priority: -1})
	got, alreadyTerminal, err := st.Cancel(pending.ID)
	if err != nil || alreadyTerminal {
		t.Fatalf("Cancel pending: %v/%v", err, alreadyTerminal)
	}
	if got.State != TaskCancelled || !got.CancelRequested || got.Error == nil || got.Error.Kind != ErrKindCancelled {
		t.Fatalf("cancelled pending wrong: %+v", got)
	}

	// Idempotent on terminal.
	got, alreadyTerminal, err = st.Cancel(pending.ID)
	if err != nil || !alreadyTerminal || got.State != TaskCancelled {
		t.Fatalf("second cancel: %+v/%v/%v", got, alreadyTerminal, err)
	}

	// Running.
	running := mustSubmit(t, st, SubmitSpec{Agent: "feature-dev"})
	mustClaim(t, st, "feature-dev")
	var causeMu sync.Mutex
	var cause error
	st.RegisterCancel(running.ID, func(c error) {
		causeMu.Lock()
		cause = c
		causeMu.Unlock()
	})
	got, alreadyTerminal, err = st.Cancel(running.ID)
	if err != nil || alreadyTerminal {
		t.Fatalf("Cancel running: %v/%v", err, alreadyTerminal)
	}
	if got.State != TaskRunning || !got.CancelRequested {
		t.Fatalf("running cancel should only set the flag: %+v", got)
	}
	causeMu.Lock()
	if !errors.Is(cause, ErrCancelRequested) {
		t.Fatalf("cancel cause = %v", cause)
	}
	causeMu.Unlock()

	// Runner finishes; even a non-cancel error kind maps to cancelled.
	got, err = st.Finish(running.ID, FinishSpec{Err: &TaskError{Message: "stream torn down", Kind: ErrKindDownstream}})
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if got.State != TaskCancelled || got.Error.Kind != ErrKindCancelled {
		t.Fatalf("finish after cancel wrong: %+v", got)
	}

	if _, _, err := st.Cancel("t-nope"); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("cancel missing: %v", err)
	}
}

// TestDeleteSemantics: 409-shaped error on non-terminal, removes sidecars on
// terminal.
func TestDeleteSemantics(t *testing.T) {
	cfg := taskTestConfig(t)
	st, _ := newTestStore(t, cfg)
	task := mustSubmit(t, st, SubmitSpec{Agent: "feature-dev"})
	mustClaim(t, st, "feature-dev")
	if err := st.Delete(task.ID); !errors.Is(err, ErrNotTerminal) {
		t.Fatalf("delete running: %v, want ErrNotTerminal", err)
	}
	if err := os.WriteFile(st.OutputPath(task.ID), []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Finish(task.ID, FinishSpec{Result: &TaskResult{Content: "done"}}); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if err := st.Delete(task.ID); err != nil {
		t.Fatalf("delete terminal: %v", err)
	}
	if _, ok := st.Get(task.ID); ok {
		t.Fatal("task still in store")
	}
	for _, p := range []string{filepath.Join(cfg.Tasks.Dir, task.ID+".json"), st.OutputPath(task.ID)} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s not deleted (err=%v)", p, err)
		}
	}
	if err := st.Delete(task.ID); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("delete missing: %v", err)
	}
}

// TestSubmitPendingCap enforces max_pending_per_agent per agent.
func TestSubmitPendingCap(t *testing.T) {
	cfg := taskTestConfig(t)
	cfg.Tasks.MaxPendingPerAgent = 2
	st, _ := newTestStore(t, cfg)
	mustSubmit(t, st, SubmitSpec{Agent: "feature-dev"})
	mustSubmit(t, st, SubmitSpec{Agent: "feature-dev"})
	if _, err := st.Submit(SubmitSpec{Agent: "feature-dev"}); !errors.Is(err, ErrTooManyPending) {
		t.Fatalf("err = %v, want ErrTooManyPending", err)
	}
	// Another agent is unaffected.
	mustSubmit(t, st, SubmitSpec{Agent: "debugger"})
}

// TestListFilterSortCursor.
func TestListFilterSortCursor(t *testing.T) {
	cfg := taskTestConfig(t)
	st, clk := newTestStore(t, cfg)
	var ids []string
	for i := 0; i < 5; i++ {
		agent := "feature-dev"
		if i%2 == 1 {
			agent = "debugger"
		}
		ids = append(ids, mustSubmit(t, st, SubmitSpec{Agent: agent}).ID)
		clk.Advance(time.Second)
	}
	// Newest first, all agents.
	all, hasMore := st.List(TaskFilter{})
	if len(all) != 5 || hasMore {
		t.Fatalf("List all = %d/%v", len(all), hasMore)
	}
	for i, task := range all {
		if want := ids[len(ids)-1-i]; task.ID != want {
			t.Fatalf("order[%d] = %s, want %s", i, task.ID, want)
		}
	}
	// Agent filter.
	fd, _ := st.List(TaskFilter{Agent: "feature-dev"})
	if len(fd) != 3 {
		t.Fatalf("agent filter = %d, want 3", len(fd))
	}
	// State filter.
	mustClaim(t, st, "feature-dev")
	running, _ := st.List(TaskFilter{States: []TaskState{TaskRunning}})
	if len(running) != 1 {
		t.Fatalf("state filter = %d, want 1", len(running))
	}
	// Scope filter.
	scoped, _ := st.List(TaskFilter{Scope: []string{"debugger"}})
	if len(scoped) != 2 {
		t.Fatalf("scope filter = %d, want 2", len(scoped))
	}
	// Limit + keyset cursor.
	page1, hasMore := st.List(TaskFilter{Limit: 2})
	if len(page1) != 2 || !hasMore {
		t.Fatalf("page1 = %d/%v", len(page1), hasMore)
	}
	page2, hasMore := st.List(TaskFilter{Limit: 2, After: page1[1].ID})
	if len(page2) != 2 || !hasMore || page2[0].ID == page1[1].ID {
		t.Fatalf("page2 = %d/%v (%v)", len(page2), hasMore, page2)
	}
	page3, hasMore := st.List(TaskFilter{Limit: 2, After: page2[1].ID})
	if len(page3) != 1 || hasMore {
		t.Fatalf("page3 = %d/%v", len(page3), hasMore)
	}
}

// TestRecoveryMatrix drives the full §c recovery matrix: {pending,
// running+empty-spool, running+spool-bytes, running+cancel_requested,
// terminal} x retry settings.
func TestRecoveryMatrix(t *testing.T) {
	retryTrue := true
	cases := []struct {
		name string
		// setup drives store1 into the pre-crash state and returns the task id.
		setup func(t *testing.T, st *TaskStore, clk *fakeClock) string
		// advance is applied to the clock before reopening.
		advance time.Duration
		want    TaskState
		// wantKind, when non-empty, is the expected Error.Kind.
		wantKind string
		// wantNotBeforeIn, when non-zero, is the expected not_before offset
		// from the recovery time.
		wantNotBeforeIn time.Duration
	}{
		{
			name: "pending stays pending",
			setup: func(t *testing.T, st *TaskStore, clk *fakeClock) string {
				return mustSubmit(t, st, SubmitSpec{Agent: "feature-dev"}).ID
			},
			want: TaskPending,
		},
		{
			name: "pending past deadline expires",
			setup: func(t *testing.T, st *TaskStore, clk *fakeClock) string {
				return mustSubmit(t, st, SubmitSpec{Agent: "feature-dev", DeadlineS: 60}).ID
			},
			advance:  2 * time.Hour,
			want:     TaskExpired,
			wantKind: ErrKindExpired,
		},
		{
			name: "running empty spool requeues with backoff",
			setup: func(t *testing.T, st *TaskStore, clk *fakeClock) string {
				id := mustSubmit(t, st, SubmitSpec{Agent: "feature-dev", MaxAttempts: 2}).ID
				mustClaim(t, st, "feature-dev")
				return id
			},
			want:            TaskPending,
			wantKind:        ErrKindInterrupted,
			wantNotBeforeIn: 10 * time.Second, // backoff(1)
		},
		{
			name: "running with spool and no retry_on_partial fails interrupted",
			setup: func(t *testing.T, st *TaskStore, clk *fakeClock) string {
				id := mustSubmit(t, st, SubmitSpec{Agent: "feature-dev", MaxAttempts: 2}).ID
				mustClaim(t, st, "feature-dev")
				if err := os.WriteFile(st.OutputPath(id), []byte("partial output"), 0o644); err != nil {
					t.Fatal(err)
				}
				return id
			},
			want:     TaskFailed,
			wantKind: ErrKindInterrupted,
		},
		{
			name: "running with spool and retry_on_partial requeues",
			setup: func(t *testing.T, st *TaskStore, clk *fakeClock) string {
				id := mustSubmit(t, st, SubmitSpec{Agent: "feature-dev", MaxAttempts: 2, RetryOnPartial: &retryTrue}).ID
				mustClaim(t, st, "feature-dev")
				if err := os.WriteFile(st.OutputPath(id), []byte("partial output"), 0o644); err != nil {
					t.Fatal(err)
				}
				return id
			},
			want:            TaskPending,
			wantKind:        ErrKindInterrupted,
			wantNotBeforeIn: 10 * time.Second,
		},
		{
			name: "running with cancel_requested cancels",
			setup: func(t *testing.T, st *TaskStore, clk *fakeClock) string {
				id := mustSubmit(t, st, SubmitSpec{Agent: "feature-dev"}).ID
				mustClaim(t, st, "feature-dev")
				if _, _, err := st.Cancel(id); err != nil {
					t.Fatal(err)
				}
				return id
			},
			want:     TaskCancelled,
			wantKind: ErrKindCancelled,
		},
		{
			name: "running cancel_requested wins over deadline",
			setup: func(t *testing.T, st *TaskStore, clk *fakeClock) string {
				id := mustSubmit(t, st, SubmitSpec{Agent: "feature-dev", DeadlineS: 60}).ID
				mustClaim(t, st, "feature-dev")
				if _, _, err := st.Cancel(id); err != nil {
					t.Fatal(err)
				}
				return id
			},
			advance:  2 * time.Hour,
			want:     TaskCancelled,
			wantKind: ErrKindCancelled,
		},
		{
			name: "running at max attempts fails interrupted",
			setup: func(t *testing.T, st *TaskStore, clk *fakeClock) string {
				id := mustSubmit(t, st, SubmitSpec{Agent: "feature-dev", MaxAttempts: 1}).ID
				mustClaim(t, st, "feature-dev")
				return id
			},
			want:     TaskFailed,
			wantKind: ErrKindInterrupted,
		},
		{
			name: "running past deadline expires",
			setup: func(t *testing.T, st *TaskStore, clk *fakeClock) string {
				id := mustSubmit(t, st, SubmitSpec{Agent: "feature-dev", DeadlineS: 60, MaxAttempts: 5}).ID
				mustClaim(t, st, "feature-dev")
				return id
			},
			advance:  2 * time.Hour,
			want:     TaskExpired,
			wantKind: ErrKindExpired,
		},
		{
			name: "terminal untouched",
			setup: func(t *testing.T, st *TaskStore, clk *fakeClock) string {
				id := mustSubmit(t, st, SubmitSpec{Agent: "feature-dev"}).ID
				mustClaim(t, st, "feature-dev")
				if _, err := st.Finish(id, FinishSpec{Result: &TaskResult{Content: "done"}}); err != nil {
					t.Fatal(err)
				}
				return id
			},
			want: TaskSucceeded,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			cfg := taskTestConfig(t)
			st1, clk := newTestStore(t, cfg)
			id := tc.setup(t, st1, clk)
			clk.Advance(tc.advance)
			// st1 is simply dropped: nothing is flushed or finalized (crash).
			st2 := reopenStore(t, cfg, clk)
			got, ok := st2.Get(id)
			if !ok {
				t.Fatalf("task %s missing after recovery", id)
			}
			if got.State != tc.want {
				t.Fatalf("state = %s, want %s", got.State, tc.want)
			}
			if tc.wantKind != "" {
				if got.Error == nil || got.Error.Kind != tc.wantKind {
					t.Fatalf("error = %+v, want kind %s", got.Error, tc.wantKind)
				}
			}
			if tc.wantNotBeforeIn != 0 {
				want := clk.Now().Add(tc.wantNotBeforeIn)
				if got.NotBefore == nil || !got.NotBefore.Equal(want) {
					t.Fatalf("not_before = %v, want %v", got.NotBefore, want)
				}
			}
			if tc.want.IsTerminal() && got.FinishedAt == nil {
				t.Fatal("terminal task missing finished_at")
			}
			// A dangling attempt record must have been closed.
			if n := len(got.AttemptHistory); n > 0 && got.AttemptHistory[n-1].EndedAt == nil {
				t.Fatalf("open attempt record after recovery: %+v", got.AttemptHistory)
			}
			// The recovered state must have been persisted (crash-again safe).
			st3 := reopenStore(t, cfg, clk)
			again, ok := st3.Get(id)
			if !ok || again.State != got.State {
				t.Fatalf("recovery not persisted: %v/%v", again.State, ok)
			}
		})
	}
}

// TestRecoverCorruptQuarantine renames unparseable records to .corrupt and
// keeps loading the rest.
func TestRecoverCorruptQuarantine(t *testing.T) {
	cfg := taskTestConfig(t)
	st1, clk := newTestStore(t, cfg)
	good := mustSubmit(t, st1, SubmitSpec{Agent: "feature-dev"})
	badPath := filepath.Join(cfg.Tasks.Dir, "t-20260702T000000Z-deadbeef.json")
	if err := os.WriteFile(badPath, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Stale tmp files are purged at boot.
	tmpPath := filepath.Join(cfg.Tasks.Dir, ".tmp", "t-x.json.abcd")
	if err := os.WriteFile(tmpPath, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	st2 := reopenStore(t, cfg, clk)
	if _, ok := st2.Get(good.ID); !ok {
		t.Fatal("good task lost")
	}
	if _, err := os.Stat(badPath); !os.IsNotExist(err) {
		t.Fatal("corrupt record still in place")
	}
	if _, err := os.Stat(badPath + ".corrupt"); err != nil {
		t.Fatalf("corrupt record not quarantined: %v", err)
	}
	if _, err := os.Stat(tmpPath); !os.IsNotExist(err) {
		t.Fatal("stale .tmp file not purged")
	}
}

// TestRecoverOrphanedTasks: tasks for unconfigured agents load, count as
// orphaned, are never claimed and expire at deadline.
func TestRecoverOrphanedTasks(t *testing.T) {
	cfg := taskTestConfig(t)
	st1, clk := newTestStore(t, cfg)
	orphan := mustSubmit(t, st1, SubmitSpec{Agent: "feature-dev", DeadlineS: 3600})

	cfg2 := &Config{StateDir: cfg.StateDir, Agents: map[string]AgentConfig{"debugger": {}}}
	cfg2.applyDefaults()
	st2, err := NewTaskStore(cfg2)
	if err != nil {
		t.Fatal(err)
	}
	st2.now = clk.Now
	if err := st2.RecoverOnBoot(); err != nil {
		t.Fatal(err)
	}
	snap := st2.Snapshot()
	if snap.Orphaned != 1 {
		t.Fatalf("orphaned = %d, want 1", snap.Orphaned)
	}
	got, _ := st2.Get(orphan.ID)
	if got.State != TaskPending {
		t.Fatalf("orphan state = %s, want pending", got.State)
	}
	clk.Advance(2 * time.Hour)
	st2.sweepDeadlines(clk.Now())
	got, _ = st2.Get(orphan.ID)
	if got.State != TaskExpired {
		t.Fatalf("orphan not expired at deadline: %s", got.State)
	}
}

// TestGCRetentionAndCap: terminal records older than retention_h are removed,
// then max_records is enforced oldest-terminal-first; non-terminal records
// are never collected.
func TestGCRetentionAndCap(t *testing.T) {
	cfg := taskTestConfig(t)
	cfg.Tasks.RetentionH = 1
	cfg.Tasks.MaxRecords = 3
	st, clk := newTestStore(t, cfg)

	finish := func() string {
		id := mustSubmit(t, st, SubmitSpec{Agent: "feature-dev"}).ID
		mustClaim(t, st, "feature-dev")
		if _, err := st.Finish(id, FinishSpec{Result: &TaskResult{}}); err != nil {
			t.Fatal(err)
		}
		clk.Advance(time.Second)
		return id
	}

	old := finish()
	clk.Advance(2 * time.Hour) // "old" ages past retention
	t1, t2, t3 := finish(), finish(), finish()
	pending := mustSubmit(t, st, SubmitSpec{Agent: "feature-dev"}).ID

	st.sweepGC(clk.Now())

	if _, ok := st.Get(old); ok {
		t.Fatal("retention-expired record survived gc")
	}
	if _, err := os.Stat(filepath.Join(cfg.Tasks.Dir, old+".json")); !os.IsNotExist(err) {
		t.Fatal("retention-expired record file survived gc")
	}
	// 3 terminal + 1 pending remain = 4 > max_records(3): oldest terminal goes.
	if _, ok := st.Get(t1); ok {
		t.Fatal("cap enforcement should drop the oldest terminal")
	}
	for _, id := range []string{t2, t3, pending} {
		if _, ok := st.Get(id); !ok {
			t.Fatalf("record %s should survive gc", id)
		}
	}
	// Non-terminal records are never collected, even over the cap.
	got, _ := st.Get(pending)
	if got.State != TaskPending {
		t.Fatalf("pending state = %s", got.State)
	}
}

// TestDegradedFlagAndRetry: a persist failure keeps the in-memory transition,
// sets degraded, and is retried (and cleared) on the next successful persist.
func TestDegradedFlagAndRetry(t *testing.T) {
	cfg := taskTestConfig(t)
	st, _ := newTestStore(t, cfg)
	a := mustSubmit(t, st, SubmitSpec{Agent: "feature-dev"})
	b := mustSubmit(t, st, SubmitSpec{Agent: "feature-dev"})

	tmpDir := filepath.Join(cfg.Tasks.Dir, ".tmp")
	if err := os.Chmod(tmpDir, 0o500); err != nil {
		t.Fatal(err)
	}
	restore := func() { os.Chmod(tmpDir, 0o755) }
	defer restore()

	got, _, err := st.Cancel(a.ID)
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if got.State != TaskCancelled {
		t.Fatal("in-memory transition must be kept on persist failure")
	}
	if !st.Degraded() {
		t.Fatal("store should be degraded after persist failure")
	}

	restore()
	if _, _, err := st.Cancel(b.ID); err != nil {
		t.Fatalf("Cancel b: %v", err)
	}
	if st.Degraded() {
		t.Fatal("degraded should clear after dirty records re-persist")
	}
	// The retried record really is on disk with its new state.
	data, err := os.ReadFile(filepath.Join(cfg.Tasks.Dir, a.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"state":"cancelled"`) {
		t.Fatal("retried record not updated on disk")
	}

	// Submit itself surfaces persist failures instead of going degraded.
	if err := os.Chmod(tmpDir, 0o500); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Submit(SubmitSpec{Agent: "feature-dev"}); err == nil {
		t.Fatal("Submit should fail when persist fails")
	}
	restore()
}

// TestTransitionHookAndPoke: onTransition sees the full lifecycle and the
// runnable channel is poked on submit and requeue.
func TestTransitionHookAndPoke(t *testing.T) {
	cfg := taskTestConfig(t)
	st, _ := newTestStore(t, cfg)
	var mu sync.Mutex
	var seen []TaskState
	st.onTransition = func(ev TaskEvent) {
		mu.Lock()
		seen = append(seen, ev.To)
		mu.Unlock()
	}
	poke := st.Runnable("feature-dev")

	task := mustSubmit(t, st, SubmitSpec{Agent: "feature-dev", MaxAttempts: 2})
	select {
	case <-poke:
	default:
		t.Fatal("submit did not poke the dispatcher")
	}
	mustClaim(t, st, "feature-dev")
	if _, err := st.Finish(task.ID, FinishSpec{Err: &TaskError{Message: "x", Kind: ErrKindDownstream}}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-poke:
	default:
		t.Fatal("requeue did not poke the dispatcher")
	}
	mu.Lock()
	defer mu.Unlock()
	want := []TaskState{TaskPending, TaskRunning, TaskPending}
	if len(seen) != len(want) {
		t.Fatalf("events = %v, want %v", seen, want)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("events = %v, want %v", seen, want)
		}
	}
}

// TestCancelAll invokes every registered running cancel func with the cause.
func TestCancelAll(t *testing.T) {
	cfg := taskTestConfig(t)
	st, _ := newTestStore(t, cfg)
	var mu sync.Mutex
	causes := map[string]error{}
	for i := 0; i < 2; i++ {
		task := mustSubmit(t, st, SubmitSpec{Agent: "feature-dev"})
		mustClaim(t, st, "feature-dev")
		id := task.ID
		st.RegisterCancel(id, func(c error) {
			mu.Lock()
			causes[id] = c
			mu.Unlock()
		})
	}
	shutdown := errors.New("shutdown")
	st.CancelAll(shutdown)
	mu.Lock()
	defer mu.Unlock()
	if len(causes) != 2 {
		t.Fatalf("cancelled %d, want 2", len(causes))
	}
	for id, c := range causes {
		if !errors.Is(c, shutdown) {
			t.Fatalf("cause for %s = %v", id, c)
		}
	}
}

// TestAppendTraceID persists per-attempt trace ids.
func TestAppendTraceID(t *testing.T) {
	cfg := taskTestConfig(t)
	st, clk := newTestStore(t, cfg)
	task := mustSubmit(t, st, SubmitSpec{Agent: "feature-dev"})
	trace := strings.Repeat("0f", 16)
	st.AppendTraceID(task.ID, trace)
	got, _ := st.Get(task.ID)
	if len(got.TraceIDs) != 1 || got.TraceIDs[0] != trace {
		t.Fatalf("trace_ids = %v", got.TraceIDs)
	}
	st2 := reopenStore(t, cfg, clk)
	got, _ = st2.Get(task.ID)
	if len(got.TraceIDs) != 1 || got.TraceIDs[0] != trace {
		t.Fatalf("trace_ids after reopen = %v", got.TraceIDs)
	}
}
