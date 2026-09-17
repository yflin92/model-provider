package main

import (
	"bytes"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newProxyTestServer wires the proxy handler through the logging middleware,
// pointed at the given upstream base URL.
func newProxyTestServer(baseURL, apiKey string, minimal bool) http.Handler {
	p := newProxyConfig(baseURL, apiKey)
	return logging(p.handler(minimal), minimal)
}

func TestProxyRelaysRequestAndResponse(t *testing.T) {
	var gotPath, gotAuth, gotBody, gotContentType string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotContentType = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"id":"chatcmpl-upstream","choices":[{"message":{"content":"hi from upstream"}}]}`)
	}))
	defer upstream.Close()

	srv := newProxyTestServer(upstream.URL+"/api/v1", "sk-test-key", false)

	reqBody := `{"model":"openai/gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if gotPath != "/api/v1/v1/chat/completions" {
		t.Errorf("upstream path = %q, want /api/v1/v1/chat/completions", gotPath)
	}
	if gotAuth != "Bearer sk-test-key" {
		t.Errorf("upstream Authorization = %q, want Bearer sk-test-key", gotAuth)
	}
	if gotContentType != "application/json" {
		t.Errorf("upstream Content-Type = %q, want application/json", gotContentType)
	}
	if gotBody != reqBody {
		t.Errorf("upstream body = %q, want %q", gotBody, reqBody)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("client status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "hi from upstream") {
		t.Errorf("client body = %q, want upstream content relayed", rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("client Content-Type = %q, want application/json (relayed)", ct)
	}
}

func TestProxyRelaysUpstreamErrorStatus(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"error":{"message":"rate limited"}}`)
	}))
	defer upstream.Close()

	srv := newProxyTestServer(upstream.URL, "", false)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"x"}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("client status = %d, want 429 relayed from upstream", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "rate limited") {
		t.Errorf("client body = %q, want upstream error relayed", rec.Body.String())
	}
}

func TestProxyMinimalLogsEndpointModelStatus(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{}`)
	}))
	defer upstream.Close()

	// Capture the log output for this test, restoring the discard sink after.
	var logBuf bytes.Buffer
	log.SetOutput(&logBuf)
	defer log.SetOutput(io.Discard)

	srv := newProxyTestServer(upstream.URL, "sk-test", true)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"openai/gpt-4o","messages":[]}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	out := logBuf.String()
	// Endpoint + model come from the inbound MODEL line; status from the PROXY line.
	if !strings.Contains(out, "MODEL POST /v1/chat/completions -> openai/gpt-4o") {
		t.Errorf("missing inbound MODEL log line; got:\n%s", out)
	}
	if !strings.Contains(out, "PROXY") || !strings.Contains(out, "-> 200 OK") {
		t.Errorf("missing PROXY status log line; got:\n%s", out)
	}
	if !strings.Contains(out, "model openai/gpt-4o") {
		t.Errorf("PROXY line missing model; got:\n%s", out)
	}
}

func TestNewProxyConfigNilWhenNoURL(t *testing.T) {
	if p := newProxyConfig("", "key"); p != nil {
		t.Error("newProxyConfig with empty URL should return nil (mock mode)")
	}
	if p := newProxyConfig("  ", "key"); p != nil {
		t.Error("newProxyConfig with blank URL should return nil (mock mode)")
	}
	if p := newProxyConfig("https://openrouter.ai/api/v1/", "key"); p == nil {
		t.Fatal("newProxyConfig with a URL should return a config")
	} else if p.baseURL != "https://openrouter.ai/api/v1" {
		t.Errorf("trailing slash not trimmed: %q", p.baseURL)
	}
}
