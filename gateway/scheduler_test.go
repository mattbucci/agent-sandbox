package main

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// testSchedParams are fast defaults for tests; individual tests override.
func testSchedParams() schedParams {
	return schedParams{
		syncQueueMax:         4,
		syncWait:             10 * time.Second,
		asyncStarvationAfter: 300 * time.Second,
	}
}

// fakeClock is an injectable clock for aging tests.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)}
}

func (f *fakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}

func (f *fakeClock) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.t = f.t.Add(d)
}

// waitUntil polls cond for up to 5s.
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// agentSnap fetches the snapshot entry for one agent.
func agentSnap(t *testing.T, s *Scheduler, agent string) AgentSchedSnapshot {
	t.Helper()
	for _, snap := range s.Snapshot() {
		if snap.Agent == agent {
			return snap
		}
	}
	t.Fatalf("agent %s not in snapshot", agent)
	return AgentSchedSnapshot{}
}

// TestLimit1Serialization proves limit=1 never runs two acquirers at once,
// via an atomic high-water mark.
func TestLimit1Serialization(t *testing.T) {
	s := newScheduler(testSchedParams(), map[string]int{"a": 1})
	var cur, high atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, err := s.Acquire(context.Background(), "a", classAsync, SlotMeta{})
			if err != nil {
				t.Errorf("Acquire: %v", err)
				return
			}
			n := cur.Add(1)
			for {
				h := high.Load()
				if n <= h || high.CompareAndSwap(h, n) {
					break
				}
			}
			time.Sleep(100 * time.Microsecond)
			cur.Add(-1)
			release()
		}()
	}
	wg.Wait()
	if got := high.Load(); got != 1 {
		t.Fatalf("high-water mark = %d, want 1", got)
	}
	if snap := agentSnap(t, s, "a"); len(snap.Running) != 0 || len(snap.Waiting) != 0 {
		t.Fatalf("leaked state: %+v", snap)
	}
}

// TestLimit3Pipelining proves limit=3 admits exactly three at once.
func TestLimit3Pipelining(t *testing.T) {
	s := newScheduler(testSchedParams(), map[string]int{"a": 3})
	var releases []func()
	for i := 0; i < 3; i++ {
		done := make(chan func(), 1)
		go func() {
			rel, err := s.Acquire(context.Background(), "a", classSync, SlotMeta{})
			if err != nil {
				t.Errorf("Acquire: %v", err)
			}
			done <- rel
		}()
		select {
		case rel := <-done:
			releases = append(releases, rel)
		case <-time.After(2 * time.Second):
			t.Fatalf("acquire %d blocked under limit", i)
		}
	}
	// Fourth must wait.
	blocked := make(chan struct{})
	go func() {
		rel, err := s.Acquire(context.Background(), "a", classSync, SlotMeta{})
		if err == nil {
			rel()
		}
		close(blocked)
	}()
	waitUntil(t, "fourth to queue", func() bool {
		snap := agentSnap(t, s, "a")
		return len(snap.Waiting) == 1 && len(snap.Running) == 3
	})
	releases[0]()
	<-blocked
	for _, rel := range releases[1:] {
		rel()
	}
	waitUntil(t, "all released", func() bool {
		snap := agentSnap(t, s, "a")
		return len(snap.Running) == 0 && len(snap.Waiting) == 0
	})
}

// TestSyncQueueFull rejects the request that would exceed sync_queue_max,
// while async requests keep queueing.
func TestSyncQueueFull(t *testing.T) {
	p := testSchedParams()
	p.syncQueueMax = 2
	s := newScheduler(p, map[string]int{"a": 1})
	release, err := s.Acquire(context.Background(), "a", classSync, SlotMeta{})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for i := 0; i < 2; i++ {
		go func() {
			rel, err := s.Acquire(ctx, "a", classSync, SlotMeta{})
			if err == nil {
				rel()
			}
		}()
	}
	waitUntil(t, "two sync waiters", func() bool {
		return len(agentSnap(t, s, "a").Waiting) == 2
	})
	if _, err := s.Acquire(context.Background(), "a", classSync, SlotMeta{}); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("third sync waiter: err = %v, want ErrQueueFull", err)
	}
	if got := agentSnap(t, s, "a").Counters.RejectedFull; got != 1 {
		t.Fatalf("RejectedFull = %d, want 1", got)
	}
	// Async is never rejected for queue depth.
	asyncDone := make(chan error, 1)
	go func() {
		rel, err := s.Acquire(ctx, "a", classAsync, SlotMeta{})
		if err == nil {
			rel()
		}
		asyncDone <- err
	}()
	waitUntil(t, "async waiter queued", func() bool {
		return len(agentSnap(t, s, "a").Waiting) == 3
	})
	cancel()
	release()
	<-asyncDone
}

// TestSyncWaitTimeout maps a saturated agent to ErrWaitTimeout after
// sync_queue_wait_s.
func TestSyncWaitTimeout(t *testing.T) {
	p := testSchedParams()
	p.syncWait = 50 * time.Millisecond
	s := newScheduler(p, map[string]int{"a": 1})
	release, err := s.Acquire(context.Background(), "a", classSync, SlotMeta{})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer release()
	start := time.Now()
	if _, err := s.Acquire(context.Background(), "a", classSync, SlotMeta{}); !errors.Is(err, ErrWaitTimeout) {
		t.Fatalf("err = %v, want ErrWaitTimeout", err)
	}
	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Fatalf("timed out too early: %v", elapsed)
	}
	snap := agentSnap(t, s, "a")
	if snap.Counters.WaitTimeouts != 1 || len(snap.Waiting) != 0 {
		t.Fatalf("post-timeout state wrong: %+v", snap)
	}
}

// TestSyncFIFOOrder grants queued sync waiters strictly in enqueue order.
func TestSyncFIFOOrder(t *testing.T) {
	s := newScheduler(testSchedParams(), map[string]int{"a": 1})
	release, err := s.Acquire(context.Background(), "a", classSync, SlotMeta{})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	order := make(chan int, 3)
	for i := 1; i <= 3; i++ {
		i := i
		go func() {
			rel, err := s.Acquire(context.Background(), "a", classSync, SlotMeta{})
			if err != nil {
				t.Errorf("waiter %d: %v", i, err)
				return
			}
			order <- i
			rel()
		}()
		waitUntil(t, "waiter to queue", func() bool {
			return len(agentSnap(t, s, "a").Waiting) == i
		})
	}
	release()
	for want := 1; want <= 3; want++ {
		select {
		case got := <-order:
			if got != want {
				t.Fatalf("grant order: got %d, want %d", got, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("waiter %d never granted", want)
		}
	}
}

// TestAsyncNotStarvedAfterAging: an async waiter that has aged past
// async_starvation_after_s jumps the sync FIFO; a fresh one does not.
func TestAsyncNotStarvedAfterAging(t *testing.T) {
	clk := newFakeClock()
	s := newScheduler(schedParams{
		syncQueueMax:         4,
		syncWait:             time.Hour,
		asyncStarvationAfter: 300 * time.Second,
	}, map[string]int{"a": 1})
	s.now = clk.Now

	release, err := s.Acquire(context.Background(), "a", classSync, SlotMeta{})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	grants := make(chan string, 2)
	spawn := func(name string, class slotClass, queued int) func() {
		relCh := make(chan func(), 1)
		go func() {
			rel, err := s.Acquire(context.Background(), "a", class, SlotMeta{})
			if err != nil {
				t.Errorf("%s: %v", name, err)
				return
			}
			grants <- name
			relCh <- rel
		}()
		waitUntil(t, name+" queued", func() bool {
			return len(agentSnap(t, s, "a").Waiting) == queued
		})
		return func() { (<-relCh)() }
	}
	relAsync := spawn("async", classAsync, 1)
	relSync := spawn("sync", classSync, 2)

	clk.Advance(301 * time.Second)
	release()
	if got := <-grants; got != "async" {
		t.Fatalf("aged async should be granted first, got %q", got)
	}
	relAsync()
	if got := <-grants; got != "sync" {
		t.Fatalf("sync should be granted second, got %q", got)
	}
	relSync()
}

// TestSyncPreferredOverFreshAsync: without aging, the sync head wins.
func TestSyncPreferredOverFreshAsync(t *testing.T) {
	s := newScheduler(testSchedParams(), map[string]int{"a": 1})
	release, err := s.Acquire(context.Background(), "a", classSync, SlotMeta{})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	grants := make(chan string, 2)
	go func() {
		rel, err := s.Acquire(context.Background(), "a", classAsync, SlotMeta{})
		if err == nil {
			grants <- "async"
			rel()
		}
	}()
	waitUntil(t, "async queued", func() bool { return len(agentSnap(t, s, "a").Waiting) == 1 })
	go func() {
		rel, err := s.Acquire(context.Background(), "a", classSync, SlotMeta{})
		if err == nil {
			grants <- "sync"
			rel()
		}
	}()
	waitUntil(t, "sync queued", func() bool { return len(agentSnap(t, s, "a").Waiting) == 2 })
	release()
	if got := <-grants; got != "sync" {
		t.Fatalf("fresh sync should beat fresh async, got %q", got)
	}
	if got := <-grants; got != "async" {
		t.Fatalf("async should be granted second, got %q", got)
	}
}

// TestGrantCancelRace races a grant (via release) against a ctx cancel 10k
// times and asserts no slot is ever leaked (run with -race).
func TestGrantCancelRace(t *testing.T) {
	p := testSchedParams()
	p.syncWait = 30 * time.Second
	s := newScheduler(p, map[string]int{"a": 1})
	type result struct {
		rel func()
		err error
	}
	for i := 0; i < 10000; i++ {
		holder, err := s.Acquire(context.Background(), "a", classSync, SlotMeta{})
		if err != nil {
			t.Fatalf("iter %d holder: %v", i, err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan result, 1)
		go func() {
			rel, err := s.Acquire(ctx, "a", classSync, SlotMeta{})
			done <- result{rel, err}
		}()
		go cancel()
		holder()
		r := <-done
		if r.err == nil {
			r.rel()
		} else if !errors.Is(r.err, context.Canceled) {
			t.Fatalf("iter %d waiter: unexpected error %v", i, r.err)
		}
		cancel()
	}
	snap := agentSnap(t, s, "a")
	if len(snap.Running) != 0 || len(snap.Waiting) != 0 {
		t.Fatalf("leaked slots after race: %+v", snap)
	}
	// A final acquire must succeed instantly if nothing leaked.
	instant, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	rel, err := s.Acquire(instant, "a", classSync, SlotMeta{})
	if err != nil {
		t.Fatalf("final acquire blocked: %v", err)
	}
	rel()
}

// TestCloseWakesAllWaiters: Close wakes every queued waiter with
// ErrShuttingDown and refuses new acquires.
func TestCloseWakesAllWaiters(t *testing.T) {
	s := newScheduler(testSchedParams(), map[string]int{"a": 1})
	release, err := s.Acquire(context.Background(), "a", classSync, SlotMeta{})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	errs := make(chan error, 2)
	go func() {
		_, err := s.Acquire(context.Background(), "a", classSync, SlotMeta{})
		errs <- err
	}()
	go func() {
		_, err := s.Acquire(context.Background(), "a", classAsync, SlotMeta{})
		errs <- err
	}()
	waitUntil(t, "both waiters queued", func() bool {
		return len(agentSnap(t, s, "a").Waiting) == 2
	})
	s.Close()
	for i := 0; i < 2; i++ {
		if err := <-errs; !errors.Is(err, ErrShuttingDown) {
			t.Fatalf("waiter err = %v, want ErrShuttingDown", err)
		}
	}
	if _, err := s.Acquire(context.Background(), "a", classSync, SlotMeta{}); !errors.Is(err, ErrShuttingDown) {
		t.Fatalf("post-close acquire err = %v, want ErrShuttingDown", err)
	}
	release() // must not panic or grant
	if snap := agentSnap(t, s, "a"); len(snap.Running) != 0 || len(snap.Waiting) != 0 {
		t.Fatalf("post-close state wrong: %+v", snap)
	}
}

// TestUnknownAgent rejects agents absent from config.
func TestUnknownAgent(t *testing.T) {
	s := newScheduler(testSchedParams(), map[string]int{"a": 1})
	if _, err := s.Acquire(context.Background(), "nope", classSync, SlotMeta{}); !errors.Is(err, ErrUnknownAgent) {
		t.Fatalf("err = %v, want ErrUnknownAgent", err)
	}
}

// TestSnapshotMetadata surfaces SlotMeta and counters for the dashboard.
func TestSnapshotMetadata(t *testing.T) {
	s := newScheduler(testSchedParams(), map[string]int{"a": 1})
	meta := SlotMeta{TaskID: "t-1", SessionID: "task:t-1", TraceID: "abc123", Priority: 7}
	release, err := s.Acquire(context.Background(), "a", classAsync, meta)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		rel, err := s.Acquire(ctx, "a", classSync, SlotMeta{RunID: "run-2"})
		if err == nil {
			rel()
		}
	}()
	waitUntil(t, "waiter queued", func() bool { return len(agentSnap(t, s, "a").Waiting) == 1 })
	snap := agentSnap(t, s, "a")
	if snap.Limit != 1 || snap.QueueCap != 4 {
		t.Fatalf("limit/queue_cap wrong: %+v", snap)
	}
	r := snap.Running[0]
	if r.Kind != "task" || r.TaskID != "t-1" || r.SessionID != "task:t-1" || r.TraceID != "abc123" || r.RunID == "" {
		t.Fatalf("running metadata wrong: %+v", r)
	}
	w := snap.Waiting[0]
	if w.Kind != "sync" || w.RunID != "run-2" || w.EnqueuedAt.IsZero() {
		t.Fatalf("waiting metadata wrong: %+v", w)
	}
	if snap.Counters.Granted != 1 {
		t.Fatalf("Granted = %d, want 1", snap.Counters.Granted)
	}
	cancel()
	release()
}

// TestOnEventHook receives granted/rejected events (nil hook is the default
// and exercised implicitly by every other test).
func TestOnEventHook(t *testing.T) {
	p := testSchedParams()
	p.syncQueueMax = 1
	s := newScheduler(p, map[string]int{"a": 1})
	var mu sync.Mutex
	var events []SchedEvent
	s.onEvent = func(ev SchedEvent) {
		mu.Lock()
		events = append(events, ev)
		mu.Unlock()
	}
	release, err := s.Acquire(context.Background(), "a", classSync, SlotMeta{})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		rel, err := s.Acquire(ctx, "a", classSync, SlotMeta{})
		if err == nil {
			rel()
		}
	}()
	waitUntil(t, "waiter queued", func() bool { return len(agentSnap(t, s, "a").Waiting) == 1 })
	if _, err := s.Acquire(context.Background(), "a", classSync, SlotMeta{}); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("err = %v, want ErrQueueFull", err)
	}
	release()
	waitUntil(t, "granted event for waiter", func() bool {
		mu.Lock()
		defer mu.Unlock()
		granted, rejected := 0, 0
		for _, ev := range events {
			switch ev.Type {
			case SchedEventGranted:
				granted++
			case SchedEventRejectedFull:
				rejected++
			}
		}
		return granted == 2 && rejected == 1
	})
}
