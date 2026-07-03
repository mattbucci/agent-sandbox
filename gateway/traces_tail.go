package main

// Stateless tail of the collector's traces file (plan item 16): read the last
// 512 KiB of observability.traces_file (one OTLP-JSON batch per line), drop
// the first partial line, decode into minimal structs (numeric OR string
// enums; string OR numeric nanos), group spans by trace id and summarize for
// the dashboard. If the primary tail yields fewer traces than the requested
// limit and <file>.1 exists (rotation), that file is tailed too. Parsed spans
// are memoized for 5s keyed (path, size, mtime). Malformed lines are counted,
// never fatal; an absent/unreadable file degrades to available:false.

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// tracesTailBytes is how much of the end of the traces file is read.
	tracesTailBytes = 512 << 10
	// tailMemoTTL bounds how long a memoized tail parse is reused.
	tailMemoTTL = 5 * time.Second
)

// tailFileBytes reads up to maxBytes from the end of path. When the read
// started mid-file the first (partial) line is dropped.
func tailFileBytes(path string, maxBytes int64) (data []byte, size int64, mtime time.Time, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, time.Time{}, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, 0, time.Time{}, err
	}
	size, mtime = fi.Size(), fi.ModTime()
	off := int64(0)
	if size > maxBytes {
		off = size - maxBytes
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return nil, 0, time.Time{}, err
	}
	data, err = io.ReadAll(f)
	if err != nil {
		return nil, 0, time.Time{}, err
	}
	if off > 0 {
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			data = data[i+1:]
		} else {
			data = nil
		}
	}
	return data, size, mtime, nil
}

// flexNanos decodes an OTLP *TimeUnixNano field that may be a proto3-JSON
// decimal string or a bare JSON number (lenient encoders).
type flexNanos int64

func (n *flexNanos) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		*n = 0
		return nil
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		f, ferr := strconv.ParseFloat(s, 64)
		if ferr != nil {
			return err
		}
		v = int64(f)
	}
	*n = flexNanos(v)
	return nil
}

// flexStatusCode decodes an OTLP status code that may be numeric (2), a
// numeric string ("2") or an enum name ("STATUS_CODE_ERROR").
type flexStatusCode int

func (c *flexStatusCode) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	switch s {
	case "", "null", "STATUS_CODE_UNSET":
		*c = 0
		return nil
	case "STATUS_CODE_OK":
		*c = 1
		return nil
	case "STATUS_CODE_ERROR":
		*c = 2
		return nil
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return err
	}
	*c = flexStatusCode(v)
	return nil
}

// otlpFileLine is the minimal decode of one traces.jsonl line (one OTLP-JSON
// export batch as written by the collector's file exporter).
type otlpFileLine struct {
	ResourceSpans []struct {
		Resource struct {
			Attributes []struct {
				Key   string `json:"key"`
				Value struct {
					StringValue string `json:"stringValue"`
				} `json:"value"`
			} `json:"attributes"`
		} `json:"resource"`
		ScopeSpans []struct {
			Spans []struct {
				TraceID           string    `json:"traceId"`
				SpanID            string    `json:"spanId"`
				ParentSpanID      string    `json:"parentSpanId"`
				Name              string    `json:"name"`
				StartTimeUnixNano flexNanos `json:"startTimeUnixNano"`
				EndTimeUnixNano   flexNanos `json:"endTimeUnixNano"`
				Status            struct {
					Code flexStatusCode `json:"code"`
				} `json:"status"`
			} `json:"spans"`
		} `json:"scopeSpans"`
	} `json:"resourceSpans"`
}

// tailSpan is one decoded span, reduced to what trace summaries need.
type tailSpan struct {
	service  string
	traceID  string
	spanID   string
	parentID string
	name     string
	start    time.Time
	end      time.Time
	isError  bool
}

// parseTracesData decodes the tailed bytes line by line. Malformed lines are
// counted and skipped; blank lines are ignored entirely.
func parseTracesData(data []byte) (spans []tailSpan, parsed, skipped int) {
	for _, raw := range bytes.Split(data, []byte("\n")) {
		line := bytes.TrimSpace(raw)
		if len(line) == 0 {
			continue
		}
		var batch otlpFileLine
		if err := json.Unmarshal(line, &batch); err != nil {
			skipped++
			continue
		}
		parsed++
		for _, rs := range batch.ResourceSpans {
			service := "unknown"
			for _, kv := range rs.Resource.Attributes {
				if kv.Key == "service.name" && kv.Value.StringValue != "" {
					service = kv.Value.StringValue
				}
			}
			for _, ss := range rs.ScopeSpans {
				for _, sp := range ss.Spans {
					if sp.TraceID == "" || sp.SpanID == "" {
						continue
					}
					spans = append(spans, tailSpan{
						service:  service,
						traceID:  sp.TraceID,
						spanID:   sp.SpanID,
						parentID: sp.ParentSpanID,
						name:     sp.Name,
						start:    time.Unix(0, int64(sp.StartTimeUnixNano)).UTC(),
						end:      time.Unix(0, int64(sp.EndTimeUnixNano)).UTC(),
						isError:  sp.Status.Code == 2,
					})
				}
			}
		}
	}
	return spans, parsed, skipped
}

// tracesMemoEntry is one memoized tail parse.
type tracesMemoEntry struct {
	at      time.Time
	size    int64
	mtime   time.Time
	spans   []tailSpan
	parsed  int
	skipped int
}

// tracesTailCache memoizes tail parses per path for tailMemoTTL, keyed by
// (path, size, mtime) so a changed file is reparsed immediately.
type tracesTailCache struct {
	mu      sync.Mutex
	entries map[string]*tracesMemoEntry
	now     func() time.Time // injectable for tests
}

var tracesTail = &tracesTailCache{entries: make(map[string]*tracesMemoEntry), now: time.Now}

// load returns the parsed spans of the tail of path, memoized.
func (c *tracesTailCache) load(path string) (spans []tailSpan, parsed, skipped int, err error) {
	data, size, mtime, err := tailFileBytes(path, tracesTailBytes)
	if err != nil {
		return nil, 0, 0, err
	}
	c.mu.Lock()
	if e, ok := c.entries[path]; ok && e.size == size && e.mtime.Equal(mtime) && c.now().Sub(e.at) < tailMemoTTL {
		spans, parsed, skipped = e.spans, e.parsed, e.skipped
		c.mu.Unlock()
		return spans, parsed, skipped, nil
	}
	c.mu.Unlock()
	spans, parsed, skipped = parseTracesData(data)
	c.mu.Lock()
	c.entries[path] = &tracesMemoEntry{at: c.now(), size: size, mtime: mtime, spans: spans, parsed: parsed, skipped: skipped}
	c.mu.Unlock()
	return spans, parsed, skipped, nil
}

// TraceSummary is one grouped trace for GET /dashboard/api/traces (§b).
type TraceSummary struct {
	TraceID     string    `json:"trace_id"`
	RootService string    `json:"root_service"`
	RootName    string    `json:"root_name"`
	Start       time.Time `json:"start"`
	DurationMs  float64   `json:"duration_ms"`
	SpanCount   int       `json:"span_count"`
	Services    []string  `json:"services"`
	Error       bool      `json:"error"`
}

// tracesResult is the GET /dashboard/api/traces payload.
type tracesResult struct {
	Available    bool           `json:"available"`
	File         string         `json:"file"`
	Detail       string         `json:"detail,omitempty"`
	ParsedLines  int            `json:"parsed_lines"`
	SkippedLines int            `json:"skipped_lines"`
	Traces       []TraceSummary `json:"traces"`
}

// summarizeTraces groups spans by trace id, picks each trace's root (a span
// whose parent is absent from the tail; earliest-start tiebreak) and returns
// summaries within the window, newest first, capped at limit.
func summarizeTraces(spans []tailSpan, limit int, window time.Duration, now time.Time) []TraceSummary {
	byTrace := make(map[string][]tailSpan)
	seen := make(map[string]bool, len(spans))
	for _, sp := range spans {
		key := sp.traceID + "/" + sp.spanID
		if seen[key] {
			continue // rotation overlap
		}
		seen[key] = true
		byTrace[sp.traceID] = append(byTrace[sp.traceID], sp)
	}

	cutoff := now.Add(-window)
	out := make([]TraceSummary, 0, len(byTrace))
	for traceID, group := range byTrace {
		sort.Slice(group, func(i, j int) bool { return group[i].start.Before(group[j].start) })
		ids := make(map[string]bool, len(group))
		for _, sp := range group {
			ids[sp.spanID] = true
		}
		root := group[0]
		for _, sp := range group {
			if sp.parentID == "" || !ids[sp.parentID] {
				root = sp
				break
			}
		}
		start, end := group[0].start, group[0].end
		hasErr := false
		var services []string
		haveSvc := make(map[string]bool)
		for _, sp := range group {
			if sp.end.After(end) {
				end = sp.end
			}
			if sp.isError {
				hasErr = true
			}
			if !haveSvc[sp.service] {
				haveSvc[sp.service] = true
				services = append(services, sp.service)
			}
		}
		if window > 0 && start.Before(cutoff) {
			continue
		}
		out = append(out, TraceSummary{
			TraceID:     traceID,
			RootService: root.service,
			RootName:    root.name,
			Start:       start,
			DurationMs:  float64(end.Sub(start)) / float64(time.Millisecond),
			SpanCount:   len(group),
			Services:    services,
			Error:       hasErr,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Start.Equal(out[j].Start) {
			return out[i].Start.After(out[j].Start)
		}
		return out[i].TraceID < out[j].TraceID
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// readTraces tails the traces file (plus <file>.1 when the primary tail
// yields fewer traces than limit) and summarizes. Absent/unreadable primary
// file degrades to available:false; a missing rotation file is ignored.
func readTraces(path string, limit int, window time.Duration, now time.Time) tracesResult {
	spans, parsed, skipped, err := tracesTail.load(path)
	if err != nil {
		return tracesResult{Available: false, File: path, Detail: err.Error(), Traces: []TraceSummary{}}
	}
	sums := summarizeTraces(spans, limit, window, now)
	if len(sums) < limit {
		if s1, p1, k1, err1 := tracesTail.load(path + ".1"); err1 == nil {
			// Merge into a fresh slice: load returns the shared memoized slice,
			// and appending onto it would write into the cached backing array
			// raced by concurrent requests.
			merged := make([]tailSpan, 0, len(s1)+len(spans))
			merged = append(merged, s1...)
			merged = append(merged, spans...)
			spans = merged
			parsed += p1
			skipped += k1
			sums = summarizeTraces(spans, limit, window, now)
		}
	}
	if sums == nil {
		sums = []TraceSummary{}
	}
	return tracesResult{Available: true, File: path, ParsedLines: parsed, SkippedLines: skipped, Traces: sums}
}
