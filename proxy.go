package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// proxyConfig holds the settings for relaying requests to a real upstream
// (OpenRouter). When enabled, the server forwards each request to the upstream
// and streams the response straight back to the client instead of returning a
// canned reply.
type proxyConfig struct {
	// baseURL is the upstream root, e.g. "https://openrouter.ai/api/v1".
	// Trailing slashes are trimmed so it can be joined with the request path.
	baseURL string
	// apiKey is sent as `Authorization: Bearer <key>` when non-empty.
	apiKey string
	// client is reused across requests; nil timeout so streaming responses are
	// not cut short.
	client *http.Client
}

// newProxyConfig builds a proxyConfig from the base URL and API key, or returns
// nil when no upstream is configured (mock mode).
func newProxyConfig(baseURL, apiKey string) *proxyConfig {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return nil
	}
	return &proxyConfig{
		baseURL: baseURL,
		apiKey:  apiKey,
		client:  &http.Client{},
	}
}

// proxyHandler relays the incoming request to the configured upstream and copies
// the response — status, headers, and body (streamed) — back to the client.
//
// In minimal mode the caller's logging middleware already emits the inbound
// `MODEL <method> <path> -> <model>` line; this handler adds a single line with
// the upstream endpoint, model, and status once the upstream responds, so a
// minimal run shows endpoint + model + status per request.
func (p *proxyConfig) handler(minimal bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := requestIDFromContext(r.Context())

		// Buffer the body so we can both forward it and read the model for the
		// log line. Request bodies here are small (chat payloads), not streams.
		var body []byte
		if r.Body != nil {
			body, _ = io.ReadAll(r.Body)
		}

		target := joinURL(p.baseURL, r.URL.Path)
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}

		outReq, err := http.NewRequestWithContext(r.Context(), r.Method, target, bytes.NewReader(body))
		if err != nil {
			writeOpenAIError(w, http.StatusBadGateway, "api_error", "could not build upstream request: "+err.Error())
			return
		}

		// Copy client headers, then override auth/host so the upstream accepts
		// the request regardless of what the client sent.
		copyHeaders(outReq.Header, r.Header)
		outReq.Header.Del("Host")
		if p.apiKey != "" {
			outReq.Header.Set("Authorization", "Bearer "+p.apiKey)
			// OpenRouter ignores x-api-key; drop it to avoid confusion.
			outReq.Header.Del("x-api-key")
		}

		start := time.Now()
		resp, err := p.client.Do(outReq)
		if err != nil {
			log.Printf("[#%d] PROXY %s %s -> ERROR %v", id, r.Method, target, err)
			writeOpenAIError(w, http.StatusBadGateway, "api_error", "upstream request failed: "+err.Error())
			return
		}
		defer resp.Body.Close()

		logProxy(proxyLog{
			id:     id,
			method: r.Method,
			target: target,
			model:  modelFromBody(body),
			status: resp.StatusCode,
			dur:    time.Since(start),
		})

		// Relay status + headers + body back to the client. Copy headers before
		// WriteHeader; stream the body with periodic flushes so SSE responses
		// reach the client incrementally. Tee the body as it streams so tool
		// calls can be logged once the response completes without buffering the
		// whole (possibly large) stream.
		copyHeaders(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		tee := newTeeReader(resp.Body, maxToolCallCapture)
		flushingCopy(w, tee)

		logToolCalls(id, body, resp.Header.Get("Content-Type"), tee.captured())
	}
}

// maxToolCallCapture bounds how much of a streamed response is buffered for
// tool-call extraction; tool calls are announced early in a response, so this is
// ample without holding large streams in memory.
const maxToolCallCapture = 256 * 1024

// logToolCalls emits one line listing the tools the client offered upstream and
// one listing the tool calls the model made, when either is present.
func logToolCalls(id uint64, reqBody []byte, respContentType string, respBody []byte) {
	if offered := requestTools(reqBody); len(offered) > 0 {
		log.Printf("[#%d] PROXY tools offered: %s", id, strings.Join(offered, ", "))
	}
	if called := responseTools(respContentType, respBody); len(called) > 0 {
		log.Printf("[#%d] PROXY tool calls: %s", id, strings.Join(called, ", "))
	}
}

// proxyLog carries the fields for a single upstream-response log line.
type proxyLog struct {
	id     uint64
	method string
	target string
	model  string
	status int
	dur    time.Duration
}

// logProxy emits the minimal-mode proxy line: upstream endpoint, model, and the
// status returned by the upstream.
func logProxy(l proxyLog) {
	model := l.model
	if model == "" {
		model = "-"
	}
	log.Printf("[#%d] PROXY %s %s (model %s) -> %d %s (%s)",
		l.id, l.method, l.target, model, l.status, http.StatusText(l.status), l.dur.Round(time.Millisecond))
}

// modelFromBody pulls the top-level `model` field out of a JSON request body,
// returning "" when absent or unparseable.
func modelFromBody(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var payload struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return ""
	}
	return payload.Model
}

// joinURL joins the upstream base with the client's request path, collapsing a
// version segment that appears at the end of the base and the start of the path.
//
// Clients send fully-qualified paths like "/v1/chat/completions", and upstreams
// are conventionally documented with the version included
// ("https://openrouter.ai/api/v1"). Naive concatenation would produce
// ".../api/v1/v1/chat/completions", which OpenRouter serves as 404. When the
// base's last path segment equals the request path's first segment (e.g. both
// "v1"), the duplicate is dropped.
func joinURL(base, path string) string {
	baseSeg := lastPathSegment(base)
	if baseSeg != "" && baseSeg == firstPathSegment(path) {
		path = strings.TrimPrefix(path, "/"+baseSeg)
	}
	return base + path
}

// firstPathSegment returns the first "/"-delimited segment of a path, or "".
func firstPathSegment(path string) string {
	return strings.SplitN(strings.TrimPrefix(path, "/"), "/", 2)[0]
}

// lastPathSegment returns the final "/"-delimited segment of a URL or path, or "".
func lastPathSegment(s string) string {
	s = strings.TrimRight(s, "/")
	if i := strings.LastIndex(s, "/"); i >= 0 {
		return s[i+1:]
	}
	return s
}

// copyHeaders copies all header values from src to dst without clobbering keys
// that dst already has beyond what src supplies.
func copyHeaders(dst, src http.Header) {
	for name, values := range src {
		for _, v := range values {
			dst.Add(name, v)
		}
	}
}

// flushingCopy streams src to dst, flushing after each chunk so streamed (SSE)
// responses are delivered to the client without buffering.
func flushingCopy(dst http.ResponseWriter, src io.Reader) {
	flusher, _ := dst.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}
