package main

import (
	"bufio"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
)

// sseBody assembles an SSE body from data payloads (adding the "data: "
// prefix and blank-line separators).
func sseBody(payloads ...string) string {
	var b strings.Builder
	for _, p := range payloads {
		b.WriteString("data: ")
		b.WriteString(p)
		b.WriteString("\n\n")
	}
	return b.String()
}

// TestDrainSSEHappyPath: content chunks, a finish chunk with usage, [DONE].
func TestDrainSSEHappyPath(t *testing.T) {
	body := sseBody(
		`{"id":"chatcmpl-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]}`,
		`{"id":"chatcmpl-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":" world"},"finish_reason":null}]}`,
		`{"id":"chatcmpl-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":34}}`,
		`[DONE]`,
	)
	var sink strings.Builder
	var activity atomic.Int64
	res, err := drainChatStream("text/event-stream; charset=utf-8", strings.NewReader(body), &sink,
		func() { activity.Add(1) })
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if !res.Done || res.FinishReason != "stop" {
		t.Fatalf("result = %+v", res)
	}
	if sink.String() != "Hello world" {
		t.Fatalf("sink = %q", sink.String())
	}
	if res.Bytes != int64(len("Hello world")) {
		t.Fatalf("bytes = %d", res.Bytes)
	}
	if string(res.Usage) != `{"prompt_tokens":12,"completion_tokens":34}` {
		t.Fatalf("usage = %s", res.Usage)
	}
	if activity.Load() == 0 {
		t.Fatal("onActivity never called")
	}
}

// TestDrainSSEStopsAtDone: bytes after [DONE] are not consumed as chunks.
func TestDrainSSEStopsAtDone(t *testing.T) {
	body := sseBody(
		`{"choices":[{"delta":{"content":"a"},"finish_reason":"stop"}]}`,
		`[DONE]`,
		`{"choices":[{"delta":{"content":"IGNORED"},"finish_reason":null}]}`,
	)
	var sink strings.Builder
	res, err := drainChatStream("text/event-stream", strings.NewReader(body), &sink, nil)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if !res.Done || sink.String() != "a" {
		t.Fatalf("done=%v sink=%q", res.Done, sink.String())
	}
}

// TestDrainSSETruncated: EOF before [DONE] is a distinct error, with the
// partial content already spooled.
func TestDrainSSETruncated(t *testing.T) {
	body := sseBody(`{"choices":[{"delta":{"content":"partial"},"finish_reason":null}]}`)
	var sink strings.Builder
	res, err := drainChatStream("text/event-stream", strings.NewReader(body), &sink, nil)
	if !errors.Is(err, errStreamTruncated) {
		t.Fatalf("err = %v, want errStreamTruncated", err)
	}
	if res.Done || sink.String() != "partial" || res.Bytes != int64(len("partial")) {
		t.Fatalf("res=%+v sink=%q", res, sink.String())
	}
}

// TestDrainSSEIgnoresNoise: comments, event:/id: fields, blank lines and
// malformed JSON chunks are skipped without failing the stream.
func TestDrainSSEIgnoresNoise(t *testing.T) {
	body := ": comment\n" +
		"event: message\n" +
		"id: 42\n" +
		"\n" +
		"data: {not json}\n\n" +
		sseBody(
			`{"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`,
			`[DONE]`,
		)
	var sink strings.Builder
	res, err := drainChatStream("text/event-stream", strings.NewReader(body), &sink, nil)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if !res.Done || sink.String() != "ok" || res.FinishReason != "stop" {
		t.Fatalf("res=%+v sink=%q", res, sink.String())
	}
}

// TestDrainSSELineTooLong: a data line beyond 1 MiB fails the stream.
func TestDrainSSELineTooLong(t *testing.T) {
	huge := "data: " + strings.Repeat("x", maxSSELineBytes+10) + "\n\n"
	var sink strings.Builder
	_, err := drainChatStream("text/event-stream", strings.NewReader(huge), &sink, nil)
	if !errors.Is(err, bufio.ErrTooLong) {
		t.Fatalf("err = %v, want bufio.ErrTooLong", err)
	}
}

// TestDrainJSONFallback: a non-SSE Content-Type is read as one completed
// JSON response (message content, finish_reason, usage).
func TestDrainJSONFallback(t *testing.T) {
	body := `{"id":"chatcmpl-2","object":"chat.completion",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"full answer"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":1,"completion_tokens":2}}`
	var sink strings.Builder
	res, err := drainChatStream("application/json", strings.NewReader(body), &sink, nil)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if !res.Done || res.FinishReason != "stop" || sink.String() != "full answer" {
		t.Fatalf("res=%+v sink=%q", res, sink.String())
	}
	if res.Bytes != int64(len("full answer")) {
		t.Fatalf("bytes = %d", res.Bytes)
	}
	if string(res.Usage) != `{"prompt_tokens":1,"completion_tokens":2}` {
		t.Fatalf("usage = %s", res.Usage)
	}
}

// TestDrainJSONFallbackBadJSON: an unparseable non-SSE body is an error.
func TestDrainJSONFallbackBadJSON(t *testing.T) {
	var sink strings.Builder
	_, err := drainChatStream("text/plain", strings.NewReader("oops"), &sink, nil)
	if err == nil {
		t.Fatal("want error for non-JSON fallback body")
	}
}

// TestIsSSEContentType covers the media-type split and case-insensitivity.
func TestIsSSEContentType(t *testing.T) {
	for ct, want := range map[string]bool{
		"text/event-stream":                true,
		"text/event-stream; charset=utf8":  true,
		"Text/Event-Stream":                true,
		"application/json":                 false,
		"application/json; charset=utf-8":  false,
		"":                                 false,
		"text/event-stream-extended-thing": false,
	} {
		if got := isSSEContentType(ct); got != want {
			t.Errorf("isSSEContentType(%q) = %v, want %v", ct, got, want)
		}
	}
}
