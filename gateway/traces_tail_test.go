package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// tracesFixtureNow is the fixed "now" every traces fixture is anchored to.
var tracesFixtureNow = time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

// nanoStr renders a fixture timestamp offset from tracesFixtureNow.
func nanoStr(offset time.Duration) string {
	return fmt.Sprintf("%d", tracesFixtureNow.Add(offset).UnixNano())
}

// tracesFixture builds the primary golden fixture: two valid OTLP-JSON batch
// lines (string nanos + string status enum in one; numeric nanos + numeric
// status enum in the other), one malformed line and one blank line.
//
// Trace aaaa..01: gateway SERVER root -> gateway CLIENT child -> agent SERVER
// child (error, numeric enum 2, numeric nanos).
// Trace bbbb..02: gateway SERVER root only (string enum STATUS_CODE_ERROR).
func tracesFixture() string {
	line1 := `{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"hermes-gateway"}}]},"scopeSpans":[{"scope":{"name":"hermes-gateway"},"spans":[` +
		`{"traceId":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa01","spanId":"1111111111111111","name":"POST /v1/chat/completions","kind":2,"startTimeUnixNano":"` + nanoStr(-60*time.Second) + `","endTimeUnixNano":"` + nanoStr(-55*time.Second) + `"},` +
		`{"traceId":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa01","spanId":"2222222222222222","parentSpanId":"1111111111111111","name":"proxy /v1/chat/completions","kind":3,"startTimeUnixNano":"` + nanoStr(-59*time.Second) + `","endTimeUnixNano":"` + nanoStr(-56*time.Second) + `"},` +
		`{"traceId":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbb02","spanId":"3333333333333333","name":"POST /v1/tasks","kind":2,"startTimeUnixNano":"` + nanoStr(-30*time.Second) + `","endTimeUnixNano":"` + nanoStr(-29*time.Second) + `","status":{"code":"STATUS_CODE_ERROR","message":"boom"}}` +
		`]}]}]}`
	// Numeric nanos and numeric status enum (lenient encoder shape).
	line2 := fmt.Sprintf(`{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"agent"}}]},"scopeSpans":[{"scope":{"name":"agent"},"spans":[`+
		`{"traceId":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa01","spanId":"4444444444444444","parentSpanId":"2222222222222222","name":"agent.chat","kind":2,"startTimeUnixNano":%d,"endTimeUnixNano":%d,"status":{"code":2}}`+
		`]}]}]}`,
		tracesFixtureNow.Add(-58*time.Second).UnixNano(), tracesFixtureNow.Add(-57*time.Second).UnixNano())
	return line1 + "\n" + "{this is not json\n" + "\n" + line2 + "\n"
}

// rotatedTracesFixture is an older trace that only lives in <file>.1.
func rotatedTracesFixture() string {
	return `{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"hermes-gateway"}}]},"scopeSpans":[{"scope":{"name":"hermes-gateway"},"spans":[` +
		`{"traceId":"cccccccccccccccccccccccccccccc03","spanId":"5555555555555555","name":"POST /v1/chat/completions","kind":2,"startTimeUnixNano":"` + nanoStr(-5*time.Minute) + `","endTimeUnixNano":"` + nanoStr(-5*time.Minute+time.Second) + `"}` +
		`]}]}]}` + "\n"
}

func TestParseTracesDataEnumsAndMalformed(t *testing.T) {
	spans, parsed, skipped := parseTracesData([]byte(tracesFixture()))
	if parsed != 2 {
		t.Fatalf("parsed = %d, want 2", parsed)
	}
	if skipped != 1 {
		t.Fatalf("skipped = %d, want 1", skipped)
	}
	if len(spans) != 4 {
		t.Fatalf("spans = %d, want 4", len(spans))
	}
	byID := make(map[string]tailSpan)
	for _, sp := range spans {
		byID[sp.spanID] = sp
	}
	// String nanos decoded.
	if got := byID["1111111111111111"].start; !got.Equal(tracesFixtureNow.Add(-60 * time.Second)) {
		t.Fatalf("string nanos start = %v", got)
	}
	// Numeric nanos decoded.
	if got := byID["4444444444444444"].start; !got.Equal(tracesFixtureNow.Add(-58 * time.Second)) {
		t.Fatalf("numeric nanos start = %v", got)
	}
	// String status enum.
	if !byID["3333333333333333"].isError {
		t.Fatal("STATUS_CODE_ERROR string enum not detected")
	}
	// Numeric status enum.
	if !byID["4444444444444444"].isError {
		t.Fatal("numeric status code 2 not detected")
	}
	// Unset status.
	if byID["1111111111111111"].isError || byID["2222222222222222"].isError {
		t.Fatal("unset status must not be an error")
	}
	// Service names from resource attributes.
	if byID["1111111111111111"].service != "hermes-gateway" || byID["4444444444444444"].service != "agent" {
		t.Fatalf("service names wrong: %q %q", byID["1111111111111111"].service, byID["4444444444444444"].service)
	}
}

func TestSummarizeTracesGroupingRootsAndWindow(t *testing.T) {
	spans, _, _ := parseTracesData([]byte(tracesFixture()))
	sums := summarizeTraces(spans, 50, 15*time.Minute, tracesFixtureNow)
	if len(sums) != 2 {
		t.Fatalf("traces = %d, want 2", len(sums))
	}
	// Sorted newest first: bbbb..02 (-30s) before aaaa..01 (-60s).
	if sums[0].TraceID != "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbb02" || sums[1].TraceID != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa01" {
		t.Fatalf("order wrong: %s, %s", sums[0].TraceID, sums[1].TraceID)
	}
	multi := sums[1]
	if multi.SpanCount != 3 {
		t.Fatalf("span_count = %d, want 3", multi.SpanCount)
	}
	if multi.RootService != "hermes-gateway" || multi.RootName != "POST /v1/chat/completions" {
		t.Fatalf("root = %s %q", multi.RootService, multi.RootName)
	}
	if !multi.Error {
		t.Fatal("trace with an error child must be flagged")
	}
	if want := []string{"hermes-gateway", "agent"}; len(multi.Services) != 2 || multi.Services[0] != want[0] || multi.Services[1] != want[1] {
		t.Fatalf("services = %v", multi.Services)
	}
	// Root SERVER span spans -60s..-55s => 5000ms duration.
	if multi.DurationMs != 5000 {
		t.Fatalf("duration_ms = %v, want 5000", multi.DurationMs)
	}
	// A tight window excludes the older trace.
	sums = summarizeTraces(spans, 50, 45*time.Second, tracesFixtureNow)
	if len(sums) != 1 || sums[0].TraceID != "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbb02" {
		t.Fatalf("window filter wrong: %+v", sums)
	}
}

// A child span whose parent is missing from the tail (dropped by the 512KiB
// cut) must still become its trace's root.
func TestSummarizeTracesOrphanRoot(t *testing.T) {
	spans := []tailSpan{{
		service: "agent", traceID: "dddddddddddddddddddddddddddddd04",
		spanID: "6666666666666666", parentID: "9999999999999999", name: "agent.chat",
		start: tracesFixtureNow.Add(-10 * time.Second), end: tracesFixtureNow.Add(-9 * time.Second),
	}}
	sums := summarizeTraces(spans, 10, 15*time.Minute, tracesFixtureNow)
	if len(sums) != 1 || sums[0].RootName != "agent.chat" || sums[0].RootService != "agent" {
		t.Fatalf("orphan root not picked: %+v", sums)
	}
}

func TestReadTracesRotationFallback(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "traces.jsonl")
	if err := os.WriteFile(path, []byte(tracesFixture()), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".1", []byte(rotatedTracesFixture()), 0o644); err != nil {
		t.Fatal(err)
	}

	res := readTraces(path, 50, 15*time.Minute, tracesFixtureNow)
	if !res.Available {
		t.Fatalf("available = false: %s", res.Detail)
	}
	if len(res.Traces) != 3 {
		t.Fatalf("traces = %d, want 3 (primary 2 + rotated 1)", len(res.Traces))
	}
	if res.ParsedLines != 3 || res.SkippedLines != 1 {
		t.Fatalf("parsed/skipped = %d/%d, want 3/1", res.ParsedLines, res.SkippedLines)
	}
	if res.Traces[2].TraceID != "cccccccccccccccccccccccccccccc03" {
		t.Fatalf("rotated trace missing/misordered: %+v", res.Traces)
	}

	// When the primary already satisfies the limit, .1 is not consulted.
	res = readTraces(path, 2, 15*time.Minute, tracesFixtureNow)
	if len(res.Traces) != 2 || res.ParsedLines != 2 {
		t.Fatalf("limit-satisfied read used rotation: %d traces, %d parsed", len(res.Traces), res.ParsedLines)
	}
}

// TestReadTracesConcurrentRotationMerge: concurrent readTraces calls share
// the memoized .1 parse; merging it with the primary spans must never write
// into the cached backing array (regression for a shared-slice append race —
// meaningful under -race). The rotated file carries enough spans that the
// memoized slice has spare capacity from append growth, the precondition of
// the original race.
func TestReadTracesConcurrentRotationMerge(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "traces.jsonl")
	if err := os.WriteFile(path, []byte(tracesFixture()), 0o644); err != nil {
		t.Fatal(err)
	}
	var rotated strings.Builder
	for i := 0; i < 9; i++ {
		rotated.WriteString(fmt.Sprintf(`{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"hermes-gateway"}}]},"scopeSpans":[{"spans":[`+
			`{"traceId":"cccccccccccccccccccccccccccccc%02x","spanId":"55555555555555%02x","name":"POST /v1/chat/completions","kind":2,"startTimeUnixNano":"%s","endTimeUnixNano":"%s"}`+
			"]}]}]}\n",
			i, i, nanoStr(-5*time.Minute), nanoStr(-5*time.Minute+time.Second)))
	}
	if err := os.WriteFile(path+".1", []byte(rotated.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				res := readTraces(path, 50, 15*time.Minute, tracesFixtureNow)
				if !res.Available {
					t.Errorf("available = false: %s", res.Detail)
					return
				}
				if len(res.Traces) != 11 {
					t.Errorf("traces = %d, want 11 (primary 2 + rotated 9)", len(res.Traces))
					return
				}
			}
		}()
	}
	wg.Wait()
}

func TestReadTracesMissingFile(t *testing.T) {
	res := readTraces(filepath.Join(t.TempDir(), "nope.jsonl"), 50, 15*time.Minute, tracesFixtureNow)
	if res.Available {
		t.Fatal("missing file must be available:false")
	}
	if res.Detail == "" {
		t.Fatal("detail must explain the failure")
	}
	if res.Traces == nil || len(res.Traces) != 0 {
		t.Fatalf("traces must be an empty list: %+v", res.Traces)
	}
}

func TestTailFileBytesDropsPartialFirstLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tail.txt")
	content := "first-line-is-long-and-partial\nsecond\nthird\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	// A cut landing mid-first-line must drop through the first newline.
	data, _, _, err := tailFileBytes(path, int64(len(content)-5))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); got != "second\nthird\n" {
		t.Fatalf("tail = %q, want %q", got, "second\nthird\n")
	}
	// A cut larger than the file returns everything.
	data, _, _, err = tailFileBytes(path, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != content {
		t.Fatalf("full read = %q", data)
	}
}

func TestTracesMemoCache(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "traces.jsonl")
	if err := os.WriteFile(path, []byte(tracesFixture()), 0o644); err != nil {
		t.Fatal(err)
	}
	s1, p1, _, err := tracesTail.load(path)
	if err != nil {
		t.Fatal(err)
	}
	// Same size+mtime within the TTL: the memo entry must be reused.
	s2, p2, _, err := tracesTail.load(path)
	if err != nil {
		t.Fatal(err)
	}
	if p1 != p2 || len(s1) != len(s2) {
		t.Fatalf("memo mismatch: %d/%d spans, %d/%d parsed", len(s1), len(s2), p1, p2)
	}
	if len(s1) > 0 && &s1[0] != &s2[0] {
		t.Fatal("memoized load must return the cached slice")
	}
	// A grown file (different size) is reparsed immediately.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(strings.TrimRight(rotatedTracesFixture(), "\n") + "\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	s3, p3, _, err := tracesTail.load(path)
	if err != nil {
		t.Fatal(err)
	}
	if p3 != p1+1 || len(s3) != len(s1)+1 {
		t.Fatalf("changed file not reparsed: %d parsed, %d spans", p3, len(s3))
	}
}
