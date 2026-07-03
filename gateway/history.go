package main

// In-memory ring buffers for the dashboard traffic charts (plan item 12):
// per agent plus "_total", 360 buckets x 10s (one hour) of request
// count/errors/latency histogram, plus parallel gauge rings (queue depth,
// running) sampled by a 10s ticker goroutine. One mutex guards everything;
// rings are advanced lazily by (unixSec/10)%360 with skipped-bucket zeroing.

import (
	"context"
	"sync"
	"time"
)

const (
	// historyBuckets x historyStepS seconds = one hour of history.
	historyBuckets = 360
	historyStepS   = 10
)

// historyBoundsMs are the latency histogram upper bounds in milliseconds;
// values above the last bound are clamped into it.
var historyBoundsMs = [10]uint64{50, 100, 250, 500, 1000, 2500, 5000, 15000, 60000, 300000}

// historyCell is one 10s bucket of request traffic.
type historyCell struct {
	count  uint32
	errors uint32
	sumMs  uint64
	hist   [10]uint32
}

// historyRing is one series (an agent or "_total") of traffic cells.
type historyRing struct {
	lastSlot int64 // unixSec/historyStepS of the newest written slot
	cells    [historyBuckets]historyCell
}

// advance zeroes every bucket between lastSlot and slot so stale data from a
// previous lap never leaks into the current hour.
func (r *historyRing) advance(slot int64) {
	if slot <= r.lastSlot {
		return
	}
	gap := slot - r.lastSlot
	if gap > historyBuckets {
		gap = historyBuckets
	}
	for i := int64(1); i <= gap; i++ {
		r.cells[(r.lastSlot+i)%historyBuckets] = historyCell{}
	}
	r.lastSlot = slot
}

// gaugeHistoryRing holds the sampled queue-depth and running gauges.
type gaugeHistoryRing struct {
	lastSlot   int64
	queueDepth [historyBuckets]uint32
	running    [historyBuckets]uint32
}

func (r *gaugeHistoryRing) advance(slot int64) {
	if slot <= r.lastSlot {
		return
	}
	gap := slot - r.lastSlot
	if gap > historyBuckets {
		gap = historyBuckets
	}
	for i := int64(1); i <= gap; i++ {
		idx := (r.lastSlot + i) % historyBuckets
		r.queueDepth[idx] = 0
		r.running[idx] = 0
	}
	r.lastSlot = slot
}

// History owns all rings. All methods are nil-receiver-safe.
type History struct {
	mu     sync.Mutex
	series map[string]*historyRing
	gauges map[string]*gaugeHistoryRing

	// now is injectable for tests.
	now func() time.Time
}

// newHistory builds an empty History; series rings are created lazily on
// first observation, gauge rings on first sample.
func newHistory() *History {
	return &History{
		series: make(map[string]*historyRing),
		gauges: make(map[string]*gaugeHistoryRing),
		now:    time.Now,
	}
}

// Observe records one finished request into the "_total" ring and, when
// agent is non-empty, into that agent's ring.
func (h *History) Observe(agent string, dur time.Duration, isErr bool) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	slot := h.now().Unix() / historyStepS
	h.observeLocked("_total", slot, dur, isErr)
	if agent != "" {
		h.observeLocked(agent, slot, dur, isErr)
	}
}

func (h *History) observeLocked(key string, slot int64, dur time.Duration, isErr bool) {
	r := h.series[key]
	if r == nil {
		r = &historyRing{}
		h.series[key] = r
	}
	r.advance(slot)
	c := &r.cells[slot%historyBuckets]
	c.count++
	if isErr {
		c.errors++
	}
	ms := uint64(dur.Milliseconds())
	c.sumMs += ms
	c.hist[latencyBucketIdx(ms)]++
}

// latencyBucketIdx maps a millisecond latency to its histogram bucket.
func latencyBucketIdx(ms uint64) int {
	for i, b := range historyBoundsMs {
		if ms <= b {
			return i
		}
	}
	return len(historyBoundsMs) - 1
}

// SampleGauges records the current queue depth and running count per agent
// (called by the 10s ticker with a fresh scheduler snapshot).
func (h *History) SampleGauges(snaps []AgentSchedSnapshot) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	slot := h.now().Unix() / historyStepS
	for _, snap := range snaps {
		g := h.gauges[snap.Agent]
		if g == nil {
			g = &gaugeHistoryRing{}
			h.gauges[snap.Agent] = g
		}
		g.advance(slot)
		idx := slot % historyBuckets
		g.queueDepth[idx] = uint32(len(snap.Waiting))
		g.running[idx] = uint32(len(snap.Running))
	}
}

// Run is the gauge sampler goroutine: every 10s it snapshots the scheduler
// into the gauge rings, until ctx is cancelled.
func (h *History) Run(ctx context.Context, sched *Scheduler) {
	ticker := time.NewTicker(historyStepS * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			h.SampleGauges(sched.Snapshot())
		}
	}
}

// HistorySeries is one traffic series rendered oldest-first, fixed length
// historyBuckets, zero-filled.
type HistorySeries struct {
	Count    []uint32
	Errors   []uint32
	LatMsAvg []float64
	LatMsP95 []float64
}

// HistorySnapshot is the full chart dataset the dashboard consumes.
type HistorySnapshot struct {
	StartUnix int64 // unix seconds of the oldest bucket
	StepS     int
	Buckets   int
	Series    map[string]HistorySeries // agents + "_total"
	// Gauge rings per agent, oldest first.
	QueueDepth map[string][]uint32
	Running    map[string][]uint32
}

// Snapshot renders every ring as fixed-length oldest-first arrays.
func (h *History) Snapshot() HistorySnapshot {
	snap := HistorySnapshot{
		StepS:      historyStepS,
		Buckets:    historyBuckets,
		Series:     make(map[string]HistorySeries),
		QueueDepth: make(map[string][]uint32),
		Running:    make(map[string][]uint32),
	}
	if h == nil {
		return snap
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	cur := h.now().Unix() / historyStepS
	snap.StartUnix = (cur - historyBuckets + 1) * historyStepS
	for key, r := range h.series {
		r.advance(cur)
		s := HistorySeries{
			Count:    make([]uint32, historyBuckets),
			Errors:   make([]uint32, historyBuckets),
			LatMsAvg: make([]float64, historyBuckets),
			LatMsP95: make([]float64, historyBuckets),
		}
		for i := 0; i < historyBuckets; i++ {
			c := &r.cells[ringIdx(cur-int64(historyBuckets)+1+int64(i))]
			s.Count[i] = c.count
			s.Errors[i] = c.errors
			if c.count > 0 {
				s.LatMsAvg[i] = float64(c.sumMs) / float64(c.count)
				s.LatMsP95[i] = cellP95(c)
			}
		}
		snap.Series[key] = s
	}
	for agent, g := range h.gauges {
		g.advance(cur)
		qd := make([]uint32, historyBuckets)
		rn := make([]uint32, historyBuckets)
		for i := 0; i < historyBuckets; i++ {
			idx := ringIdx(cur - int64(historyBuckets) + 1 + int64(i))
			qd[i] = g.queueDepth[idx]
			rn[i] = g.running[idx]
		}
		snap.QueueDepth[agent] = qd
		snap.Running[agent] = rn
	}
	return snap
}

// ringIdx maps a (possibly negative) slot number onto a ring index.
func ringIdx(slot int64) int64 {
	return ((slot % historyBuckets) + historyBuckets) % historyBuckets
}

// cellP95 estimates the 95th-percentile latency of one cell as the upper
// bound of the bucket containing the 95th-percentile observation.
func cellP95(c *historyCell) float64 {
	if c.count == 0 {
		return 0
	}
	threshold := (uint64(c.count)*95 + 99) / 100 // ceil(0.95 * count)
	cum := uint64(0)
	for i, n := range c.hist {
		cum += uint64(n)
		if cum >= threshold {
			return float64(historyBoundsMs[i])
		}
	}
	return float64(historyBoundsMs[len(historyBoundsMs)-1])
}
