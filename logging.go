package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// requestCounter gives every request a monotonically increasing id so paired
// request/response log lines are easy to correlate.
var requestCounter atomic.Uint64

// requestIDKey is the context key under which the logging middleware stores the
// id it assigns, so downstream handlers (e.g. the proxy) can log lines that
// correlate with the middleware's request line.
type requestIDKey struct{}

// requestIDFromContext returns the id the logging middleware assigned, or 0 if
// the request did not pass through the middleware.
func requestIDFromContext(ctx context.Context) uint64 {
	id, _ := ctx.Value(requestIDKey{}).(uint64)
	return id
}

// sensitiveHeaders are redacted in logs so real credentials never hit disk.
var sensitiveHeaders = map[string]bool{
	"authorization": true,
	"x-api-key":     true,
	"api-key":       true,
	"cookie":        true,
}

// captureWriter records the status code (and byte count) an http.Handler writes
// so the middleware can log the response side without buffering the body — SSE
// responses can be arbitrarily long and must stream straight through.
type captureWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *captureWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *captureWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += n
	return n, err
}

// Flush forwards to the underlying writer so streaming handlers can push SSE
// chunks immediately.
func (w *captureWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// logging wraps a handler, emitting a structured line for the request (with the
// body pretty-printed when it is JSON) and one for the response.
//
// When minimal is true it emits only the endpoint + model line per request
// (via logModel) and suppresses the header/body dump and the response line —
// the quiet way to watch which model a harness asks for on each call.
func logging(next http.Handler, minimal bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := requestCounter.Add(1)
		start := time.Now()

		// Make the id available to downstream handlers so their log lines
		// correlate with this request rather than re-reading the shared counter.
		r = r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id))

		// Read and restore the body so the downstream handler still sees it.
		var body []byte
		if r.Body != nil {
			body, _ = io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(body))
		}

		logModel(id, r, body)

		if minimal {
			next.ServeHTTP(w, r)
			return
		}

		logRequest(id, r, body)

		cw := &captureWriter{ResponseWriter: w}
		next.ServeHTTP(cw, r)

		log.Printf("[#%d] <- %d %s (%d bytes, %s)",
			id, cw.status, http.StatusText(cw.status), cw.bytes, time.Since(start).Round(time.Millisecond))
	})
}

// logModel emits a single, prominent line naming the model each request asks
// for — the fast way to observe what a harness (Claude Code, Codex, an SDK)
// actually sends on the wire, without scanning the full body dump below. Every
// POST format the server speaks (Anthropic Messages, OpenAI Chat, OpenAI
// Responses) carries `model` at the top level of the JSON body, so one field
// covers them all. Requests without a model (health checks, model listings,
// malformed bodies) are skipped.
func logModel(id uint64, r *http.Request, body []byte) {
	if len(body) == 0 {
		return
	}
	var payload struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &payload); err != nil || payload.Model == "" {
		return
	}
	log.Printf("[#%d] MODEL %s %s -> %s", id, r.Method, r.URL.Path, payload.Model)
}

func logRequest(id uint64, r *http.Request, body []byte) {
	var b strings.Builder
	b.WriteString("\n")
	b.WriteString("========================================\n")
	b.WriteString("[#")
	b.WriteString(itoa(id))
	b.WriteString("] -> ")
	b.WriteString(r.Method)
	b.WriteString(" ")
	b.WriteString(r.URL.String())
	b.WriteString("\n")

	b.WriteString("Headers:\n")
	for name, values := range r.Header {
		val := strings.Join(values, ", ")
		if sensitiveHeaders[strings.ToLower(name)] {
			val = redact(val)
		}
		b.WriteString("  ")
		b.WriteString(name)
		b.WriteString(": ")
		b.WriteString(val)
		b.WriteString("\n")
	}

	if len(body) > 0 {
		b.WriteString("Body:\n")
		b.WriteString(prettyJSON(body))
		b.WriteString("\n")
	}
	b.WriteString("========================================")
	log.Print(b.String())
}

// redact keeps a short prefix so you can tell keys apart in logs without
// exposing the secret.
func redact(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= 12 {
		return "***"
	}
	return s[:8] + "…" + s[len(s)-4:] + " (redacted)"
}

// prettyJSON re-indents a JSON body; if the body is not valid JSON it is
// returned verbatim.
func prettyJSON(b []byte) string {
	var out bytes.Buffer
	if err := json.Indent(&out, b, "  ", "  "); err != nil {
		return "  " + string(b)
	}
	return "  " + out.String()
}

func itoa(u uint64) string {
	if u == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for u > 0 {
		i--
		buf[i] = byte('0' + u%10)
		u /= 10
	}
	return string(buf[i:])
}
