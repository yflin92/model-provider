package main

import (
	"fmt"
	"net/http"
	"strings"
)

// cannedReply builds a deterministic assistant reply from the last user message.
// Echoing the input back makes round-trips easy to verify in tests: the caller
// can assert the mock saw exactly what it sent.
func cannedReply(lastUserText string) string {
	lastUserText = strings.TrimSpace(lastUserText)
	if lastUserText == "" {
		return "This is a mock response. No user message was found in the request."
	}
	const max = 500
	shown := lastUserText
	if len(shown) > max {
		shown = shown[:max] + "…"
	}
	return fmt.Sprintf("This is a mock response. You said: %q", shown)
}

// tokens is a trivial whitespace tokenizer used to chunk streamed output so the
// SSE paths emit several deltas rather than one.
func tokens(s string) []string {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return []string{s}
	}
	out := make([]string, len(fields))
	for i, f := range fields {
		if i == 0 {
			out[i] = f
		} else {
			out[i] = " " + f
		}
	}
	return out
}

// approxTokens is a cheap stand-in for a token count in usage fields.
func approxTokens(s string) int {
	n := len(strings.Fields(s))
	if n == 0 {
		return 1
	}
	return n
}

// sseWriter writes Server-Sent Events and flushes after each one so clients
// receive chunks incrementally.
type sseWriter struct {
	w http.ResponseWriter
	f http.Flusher
}

func newSSEWriter(w http.ResponseWriter) (*sseWriter, bool) {
	f, ok := w.(http.Flusher)
	if !ok {
		return nil, false
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	return &sseWriter{w: w, f: f}, true
}

// event writes a named SSE event (Anthropic style: an `event:` line plus data).
func (s *sseWriter) event(name, data string) {
	if name != "" {
		fmt.Fprintf(s.w, "event: %s\n", name)
	}
	fmt.Fprintf(s.w, "data: %s\n\n", data)
	s.f.Flush()
}

// data writes a bare SSE data line (OpenAI style: no event name).
func (s *sseWriter) data(data string) {
	fmt.Fprintf(s.w, "data: %s\n\n", data)
	s.f.Flush()
}
