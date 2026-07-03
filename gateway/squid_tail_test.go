package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// squidFixtureNow anchors the squid fixtures.
var squidFixtureNow = time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

// squidLine renders one native-format access-log line at now+offset.
func squidLine(offset time.Duration, client, code string, bytes int64, method, url string) string {
	ts := squidFixtureNow.Add(offset)
	return fmt.Sprintf("%d.%03d    145 %s %s %d %s %s - HIER_DIRECT/140.82.112.3 -",
		ts.Unix(), ts.Nanosecond()/1e6, client, code, bytes, method, url)
}

// squidFixture: a CONNECT tunnel, a plain-URL GET, a denied CONNECT, a
// too-short malformed line and a bad-timestamp line.
func squidFixture() string {
	return squidLine(-10*time.Minute, "10.0.2.2", "TCP_TUNNEL/200", 3421, "CONNECT", "github.com:443") + "\n" +
		squidLine(-8*time.Minute, "10.0.2.3", "TCP_MISS/200", 1200, "GET", "http://example.com/path?q=1") + "\n" +
		squidLine(-5*time.Minute, "10.0.2.2", "TCP_DENIED/403", 0, "CONNECT", "evil.example:443") + "\n" +
		"1234 too short\n" +
		"not-a-ts    145 10.0.2.2 TCP_MISS/200 10 GET http://x.example/ - HIER_DIRECT/1.2.3.4 -\n"
}

func TestParseSquidLineShapes(t *testing.T) {
	// CONNECT: URL is host:port.
	e, ok := parseSquidLine(squidLine(-time.Minute, "10.0.2.2", "TCP_TUNNEL/200", 3421, "CONNECT", "github.com:443"))
	if !ok {
		t.Fatal("CONNECT line must parse")
	}
	if e.Host != "github.com" || e.Denied || e.Status != 200 || e.Bytes != 3421 || e.ClientIP != "10.0.2.2" {
		t.Fatalf("CONNECT parse wrong: %+v", e)
	}
	if !e.TS.Equal(squidFixtureNow.Add(-time.Minute)) {
		t.Fatalf("timestamp = %v", e.TS)
	}

	// Plain URL: parsed hostname (port and path stripped).
	e, ok = parseSquidLine(squidLine(-time.Minute, "10.0.2.3", "TCP_MISS/200", 1200, "GET", "http://example.com:8080/path"))
	if !ok || e.Host != "example.com" {
		t.Fatalf("GET host = %q ok=%v", e.Host, ok)
	}

	// TCP_DENIED prefix marks denial.
	e, ok = parseSquidLine(squidLine(-time.Minute, "10.0.2.2", "TCP_DENIED/403", 0, "CONNECT", "evil.example:443"))
	if !ok || !e.Denied || e.Status != 403 || e.Host != "evil.example" {
		t.Fatalf("denied parse wrong: %+v ok=%v", e, ok)
	}

	// Malformed lines fail closed.
	for _, bad := range []string{
		"1234 too short",
		"not-a-ts    145 10.0.2.2 TCP_MISS/200 10 GET http://x/ - HIER_DIRECT/1.2.3.4 -",
		squidLine(-time.Minute, "10.0.2.2", "TCP_MISS_NO_SLASH", 10, "GET", "http://x/"),
	} {
		if _, ok := parseSquidLine(bad); ok {
			t.Fatalf("malformed line parsed: %q", bad)
		}
	}
}

func TestParseSquidDataCounts(t *testing.T) {
	entries, lines, skipped := parseSquidData([]byte(squidFixture()))
	if lines != 5 {
		t.Fatalf("lines = %d, want 5", lines)
	}
	if skipped != 2 {
		t.Fatalf("skipped = %d, want 2", skipped)
	}
	if len(entries) != 3 {
		t.Fatalf("entries = %d, want 3", len(entries))
	}
	denied := 0
	for _, e := range entries {
		if e.Denied {
			denied++
		}
	}
	if denied != 1 {
		t.Fatalf("denied = %d, want 1", denied)
	}
}

func TestReadSquidEntriesRotationFallback(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "access.log")
	if err := os.WriteFile(path, []byte(squidFixture()), 0o644); err != nil {
		t.Fatal(err)
	}
	rotated := squidLine(-40*time.Minute, "10.0.2.2", "TCP_TUNNEL/200", 999, "CONNECT", "old.example:443") + "\n"
	if err := os.WriteFile(path+".0", []byte(rotated), 0o644); err != nil {
		t.Fatal(err)
	}

	// The primary tail starts at -10m, after the -1h cutoff, so .0 is read too.
	entries, lines, skipped, err := readSquidEntries(path, time.Hour, squidFixtureNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 4 {
		t.Fatalf("entries = %d, want 4 (3 primary + 1 rotated)", len(entries))
	}
	if lines != 6 || skipped != 2 {
		t.Fatalf("lines/skipped = %d/%d, want 6/2", lines, skipped)
	}
	// Rotated entries come first (oldest-first ordering preserved).
	if entries[0].Host != "old.example" {
		t.Fatalf("rotated entry not prepended: %+v", entries[0])
	}
}

// TestReadSquidEntriesConcurrentRotationMerge: concurrent readSquidEntries
// calls share the memoized .0 parse; prepending it to the primary entries
// must never write into the cached backing array (regression for a
// shared-slice append race — meaningful under -race). The rotated file
// carries enough entries that the memoized slice has spare capacity from
// append growth, the precondition of the original race.
func TestReadSquidEntriesConcurrentRotationMerge(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "access.log")
	if err := os.WriteFile(path, []byte(squidFixture()), 0o644); err != nil {
		t.Fatal(err)
	}
	rotated := ""
	for i := 0; i < 5; i++ {
		rotated += squidLine(-50*time.Minute+time.Duration(i)*time.Minute,
			"10.0.2.2", "TCP_TUNNEL/200", 100, "CONNECT", fmt.Sprintf("old%d.example:443", i)) + "\n"
	}
	if err := os.WriteFile(path+".0", []byte(rotated), 0o644); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				entries, _, _, err := readSquidEntries(path, time.Hour, squidFixtureNow)
				if err != nil {
					t.Errorf("readSquidEntries: %v", err)
					return
				}
				if len(entries) != 8 {
					t.Errorf("entries = %d, want 8 (3 primary + 5 rotated)", len(entries))
					return
				}
				if entries[0].Host != "old0.example" {
					t.Errorf("rotated entries not prepended: %+v", entries[0])
					return
				}
			}
		}()
	}
	wg.Wait()
}

func TestReadSquidEntriesMissingFile(t *testing.T) {
	_, _, _, err := readSquidEntries(filepath.Join(t.TempDir(), "nope.log"), time.Hour, squidFixtureNow)
	if err == nil {
		t.Fatal("missing log must error (dashboard renders available:false)")
	}
}

func TestSquidMemoCache(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "access.log")
	if err := os.WriteFile(path, []byte(squidFixture()), 0o644); err != nil {
		t.Fatal(err)
	}
	e1, l1, _, err := squidTail.load(path)
	if err != nil {
		t.Fatal(err)
	}
	e2, l2, _, err := squidTail.load(path)
	if err != nil {
		t.Fatal(err)
	}
	if l1 != l2 || len(e1) != len(e2) {
		t.Fatalf("memo mismatch: %d/%d lines", l1, l2)
	}
	// A grown file is reparsed immediately (size key changes).
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(squidLine(-time.Minute, "10.0.2.4", "TCP_MISS/200", 5, "GET", "http://new.example/") + "\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	e3, l3, _, err := squidTail.load(path)
	if err != nil {
		t.Fatal(err)
	}
	if l3 != l1+1 || len(e3) != len(e1)+1 {
		t.Fatalf("changed file not reparsed: lines=%d entries=%d", l3, len(e3))
	}
}
