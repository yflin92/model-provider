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
		id := requestCounter.Load()

		// Buffer the body so we can both forward it and read the model for the
		// log line. Request bodies here are small (chat payloads), not streams.
		var body []byte
		if r.Body != nil {
			body, _ = io.ReadAll(r.Body)
		}

		target := p.baseURL + r.URL.Path
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

		logProxy(id, r.Method, target, modelFromBody(body), resp.StatusCode, time.Since(start))

		// Relay status + headers + body back to the client. Copy headers before
		// WriteHeader; stream the body with periodic flushes so SSE responses
		// reach the client incrementally.
		copyHeaders(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		flushingCopy(w, resp.Body)
	}
}

// logProxy emits the minimal-mode proxy line: upstream endpoint, model, and the
// status returned by the upstream.
func logProxy(id uint64, method, target, model string, status int, dur time.Duration) {
	if model == "" {
		model = "-"
	}
	log.Printf("[#%d] PROXY %s %s (model %s) -> %d %s (%s)",
		id, method, target, model, status, http.StatusText(status), dur.Round(time.Millisecond))
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
