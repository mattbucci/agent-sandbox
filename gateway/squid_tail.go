package main

// Stateless tail of the squid access log (plan item 17): read the last 1 MiB
// of observability.squid_access_log in squid NATIVE format —
//
//	ts.ms duration client code/status bytes method url user hierarchy type
//
// whitespace-split with >= 10 fields. A code starting with TCP_DENIED marks
// the request denied. For CONNECT the URL is host:port; anything else goes
// through url.Parse().Hostname(). When the primary tail does not cover the
// requested window and <file>.0 exists (rotation), that file is tailed too.
// Parses are memoized for 5s keyed (path, size, mtime). Malformed lines are
// counted, never fatal; an absent/unreadable log degrades to available:false.
// The client-ip -> agent mapping (via ListVMs) happens in dashboard_api.go.

import (
	"bytes"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// squidTailBytes is how much of the end of the access log is read.
const squidTailBytes = 1 << 20

// squidEntry is one parsed access-log line.
type squidEntry struct {
	TS       time.Time
	ClientIP string
	Code     string // e.g. "TCP_TUNNEL/200"
	Status   int
	Bytes    int64
	Method   string
	Host     string
	Denied   bool
}

// squidHost extracts the destination host: CONNECT URLs are host:port, all
// others are parsed as URLs. Falls back to the raw token (rendered with
// textContent only — squid URLs are attacker-influenced).
func squidHost(method, raw string) string {
	if method == "CONNECT" {
		if h, _, err := net.SplitHostPort(raw); err == nil && h != "" {
			return h
		}
		return raw
	}
	if u, err := url.Parse(raw); err == nil {
		if h := u.Hostname(); h != "" {
			return h
		}
	}
	return raw
}

// parseSquidLine parses one native-format line; ok is false on malformed input.
func parseSquidLine(line string) (squidEntry, bool) {
	fields := strings.Fields(line)
	if len(fields) < 10 {
		return squidEntry{}, false
	}
	ts, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return squidEntry{}, false
	}
	code := fields[3]
	slash := strings.IndexByte(code, '/')
	if slash < 0 {
		return squidEntry{}, false
	}
	status, err := strconv.Atoi(code[slash+1:])
	if err != nil {
		return squidEntry{}, false
	}
	nBytes, err := strconv.ParseInt(fields[4], 10, 64)
	if err != nil {
		return squidEntry{}, false
	}
	method := fields[5]
	sec := int64(ts)
	return squidEntry{
		TS:       time.Unix(sec, int64((ts-float64(sec))*1e9)).UTC(),
		ClientIP: fields[2],
		Code:     code,
		Status:   status,
		Bytes:    nBytes,
		Method:   method,
		Host:     squidHost(method, fields[6]),
		Denied:   strings.HasPrefix(code, "TCP_DENIED"),
	}, true
}

// parseSquidData parses the tailed bytes line by line; malformed lines are
// counted and skipped, blank lines ignored entirely.
func parseSquidData(data []byte) (entries []squidEntry, lines, skipped int) {
	for _, raw := range bytes.Split(data, []byte("\n")) {
		line := strings.TrimSpace(string(raw))
		if line == "" {
			continue
		}
		lines++
		e, ok := parseSquidLine(line)
		if !ok {
			skipped++
			continue
		}
		entries = append(entries, e)
	}
	return entries, lines, skipped
}

// squidMemoEntry is one memoized tail parse.
type squidMemoEntry struct {
	at      time.Time
	size    int64
	mtime   time.Time
	entries []squidEntry
	lines   int
	skipped int
}

// squidTailCache memoizes tail parses per path for tailMemoTTL, keyed by
// (path, size, mtime) so a changed file is reparsed immediately.
type squidTailCache struct {
	mu      sync.Mutex
	entries map[string]*squidMemoEntry
	now     func() time.Time // injectable for tests
}

var squidTail = &squidTailCache{entries: make(map[string]*squidMemoEntry), now: time.Now}

// load returns the parsed entries of the tail of path, memoized.
func (c *squidTailCache) load(path string) (entries []squidEntry, lines, skipped int, err error) {
	data, size, mtime, err := tailFileBytes(path, squidTailBytes)
	if err != nil {
		return nil, 0, 0, err
	}
	c.mu.Lock()
	if e, ok := c.entries[path]; ok && e.size == size && e.mtime.Equal(mtime) && c.now().Sub(e.at) < tailMemoTTL {
		entries, lines, skipped = e.entries, e.lines, e.skipped
		c.mu.Unlock()
		return entries, lines, skipped, nil
	}
	c.mu.Unlock()
	entries, lines, skipped = parseSquidData(data)
	c.mu.Lock()
	c.entries[path] = &squidMemoEntry{at: c.now(), size: size, mtime: mtime, entries: entries, lines: lines, skipped: skipped}
	c.mu.Unlock()
	return entries, lines, skipped, nil
}

// readSquidEntries tails the access log and, when the primary tail starts
// after the window cutoff (or is empty) and <file>.0 exists, prepends the
// rotated file's tail. Entries are returned oldest-first, unfiltered — the
// caller applies the window during aggregation.
func readSquidEntries(path string, window time.Duration, now time.Time) (entries []squidEntry, lines, skipped int, err error) {
	entries, lines, skipped, err = squidTail.load(path)
	if err != nil {
		return nil, 0, 0, err
	}
	cutoff := now.Add(-window)
	if len(entries) == 0 || entries[0].TS.After(cutoff) {
		if e0, l0, k0, err0 := squidTail.load(path + ".0"); err0 == nil {
			// Merge into a fresh slice: load returns the shared memoized slice,
			// and appending onto it would write into the cached backing array
			// raced by concurrent requests.
			merged := make([]squidEntry, 0, len(e0)+len(entries))
			merged = append(merged, e0...)
			merged = append(merged, entries...)
			entries = merged
			lines += l0
			skipped += k0
		}
	}
	return entries, lines, skipped, nil
}
