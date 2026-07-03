package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

const (
	// maxSSELineBytes caps a single SSE line; the in-VM server emits one JSON
	// chunk per data: line, so anything past 1 MiB is a broken stream.
	maxSSELineBytes = 1 << 20
	// maxJSONFallbackBytes caps the non-SSE single-JSON response body.
	maxJSONFallbackBytes = 16 << 20
)

// errStreamTruncated is returned when a text/event-stream response ends
// (EOF) before the literal "data: [DONE]" terminator.
var errStreamTruncated = errors.New("sse: stream ended before [DONE]")

// streamResult summarizes a fully drained downstream chat response.
type streamResult struct {
	// Done is true when the stream terminated cleanly: SSE saw [DONE], or the
	// non-SSE fallback parsed a complete JSON completion.
	Done bool
	// FinishReason is the last non-empty choices[].finish_reason observed.
	FinishReason string
	// Usage is the last non-null usage block observed, verbatim.
	Usage json.RawMessage
	// Bytes counts assistant content bytes written to the sink.
	Bytes int64
}

// sseChunk is a partial decode of one streamed chat.completion.chunk.
type sseChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage json.RawMessage `json:"usage"`
}

// chatCompletion is a partial decode of a non-streamed chat.completion.
type chatCompletion struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage json.RawMessage `json:"usage"`
}

// isSSEContentType reports whether ct denotes a text/event-stream body.
func isSSEContentType(ct string) bool {
	mediaType := strings.TrimSpace(strings.SplitN(ct, ";", 2)[0])
	return strings.EqualFold(mediaType, "text/event-stream")
}

// drainChatStream reads a downstream /v1/chat/completions response body to
// completion, appending assistant content fragments to sink (the task output
// spool). onActivity, when non-nil, is invoked whenever bytes arrive so the
// caller can rearm its idle watchdog.
//
// Content-Type text/event-stream is parsed as an SSE stream: one data: line
// per chunk (1 MiB max line), delta.content appended to sink, finish_reason
// and usage captured, literal [DONE] terminates. Any other Content-Type is
// the non-SSE fallback: the whole body is one JSON completion object.
func drainChatStream(contentType string, body io.Reader, sink io.Writer, onActivity func()) (streamResult, error) {
	if !isSSEContentType(contentType) {
		return drainJSONFallback(body, sink)
	}

	var res streamResult
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 64*1024), maxSSELineBytes)
	for sc.Scan() {
		if onActivity != nil {
			onActivity()
		}
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue // comments, event:/id: fields, blank separators
		}
		payload := strings.TrimSpace(line[len("data:"):])
		if payload == "[DONE]" {
			res.Done = true
			return res, nil
		}
		var chunk sseChunk
		if json.Unmarshal([]byte(payload), &chunk) != nil {
			continue // malformed chunk: skip, keep draining
		}
		if usagePresent(chunk.Usage) {
			res.Usage = chunk.Usage
		}
		for _, c := range chunk.Choices {
			if c.Delta.Content != "" {
				n, werr := io.WriteString(sink, c.Delta.Content)
				res.Bytes += int64(n)
				if werr != nil {
					return res, fmt.Errorf("sse: write output: %w", werr)
				}
			}
			if c.FinishReason != nil && *c.FinishReason != "" {
				res.FinishReason = *c.FinishReason
			}
		}
	}
	if err := sc.Err(); err != nil {
		return res, err
	}
	return res, errStreamTruncated
}

// drainJSONFallback handles a non-SSE downstream response: one complete JSON
// chat.completion object (the in-VM server's stream:false shape).
func drainJSONFallback(body io.Reader, sink io.Writer) (streamResult, error) {
	var res streamResult
	data, err := io.ReadAll(io.LimitReader(body, maxJSONFallbackBytes+1))
	if err != nil {
		return res, err
	}
	if len(data) > maxJSONFallbackBytes {
		return res, fmt.Errorf("sse: non-SSE response exceeds %d bytes", maxJSONFallbackBytes)
	}
	var cc chatCompletion
	if err := json.Unmarshal(data, &cc); err != nil {
		return res, fmt.Errorf("sse: parse non-SSE response: %w", err)
	}
	if usagePresent(cc.Usage) {
		res.Usage = cc.Usage
	}
	for _, c := range cc.Choices {
		if c.Message.Content != "" {
			n, werr := io.WriteString(sink, c.Message.Content)
			res.Bytes += int64(n)
			if werr != nil {
				return res, fmt.Errorf("sse: write output: %w", werr)
			}
		}
		if c.FinishReason != nil && *c.FinishReason != "" {
			res.FinishReason = *c.FinishReason
		}
	}
	res.Done = true
	return res, nil
}

// usagePresent reports whether a raw usage field carries a value.
func usagePresent(raw json.RawMessage) bool {
	return len(raw) > 0 && string(raw) != "null"
}
