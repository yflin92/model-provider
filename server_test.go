package main

import (
	"bufio"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestMain(m *testing.M) {
	// The logging middleware writes to the standard logger; discard it so test
	// output stays readable.
	log.SetOutput(io.Discard)
	os.Exit(m.Run())
}

// newTestServer returns the fully-wired handler (routes + logging middleware).
func newTestServer() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/messages", handleClaudeMessages)
	mux.HandleFunc("/v1/chat/completions", handleChatCompletions)
	mux.HandleFunc("/v1/responses", handleResponses)
	mux.HandleFunc("/v1/models", handleModels)
	mux.HandleFunc("/v1/models/", handleModel)
	return logging(mux)
}

func post(t *testing.T, srv http.Handler, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode response: %v\nbody: %s", err, rec.Body.String())
	}
	return m
}

func TestClaudeNonStream(t *testing.T) {
	srv := newTestServer()
	rec := post(t, srv, "/v1/messages",
		`{"model":"claude-opus-4-8","max_tokens":10,"messages":[{"role":"user","content":"ping"}]}`)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	m := decode(t, rec)
	if m["type"] != "message" || m["role"] != "assistant" {
		t.Errorf("unexpected envelope: %v", m)
	}
	content := m["content"].([]any)
	block := content[0].(map[string]any)
	if !strings.Contains(block["text"].(string), "ping") {
		t.Errorf("reply did not echo input: %v", block["text"])
	}
}

func TestClaudeBlockContent(t *testing.T) {
	// Content given as an array of typed blocks must still be extracted.
	srv := newTestServer()
	rec := post(t, srv, "/v1/messages",
		`{"model":"m","max_tokens":10,"messages":[{"role":"user","content":[{"type":"text","text":"blocktext"}]}]}`)
	block := decode(t, rec)["content"].([]any)[0].(map[string]any)
	if !strings.Contains(block["text"].(string), "blocktext") {
		t.Errorf("block content not extracted: %v", block["text"])
	}
}

func TestChatNonStream(t *testing.T) {
	srv := newTestServer()
	rec := post(t, srv, "/v1/chat/completions",
		`{"model":"gpt-4o","messages":[{"role":"user","content":"ping"}]}`)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	m := decode(t, rec)
	if m["object"] != "chat.completion" {
		t.Errorf("object = %v, want chat.completion", m["object"])
	}
	choice := m["choices"].([]any)[0].(map[string]any)
	msg := choice["message"].(map[string]any)
	if !strings.Contains(msg["content"].(string), "ping") {
		t.Errorf("reply did not echo input: %v", msg["content"])
	}
}

func TestResponsesStringInput(t *testing.T) {
	srv := newTestServer()
	rec := post(t, srv, "/v1/responses", `{"model":"gpt-5-codex","input":"ping"}`)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	m := decode(t, rec)
	if m["object"] != "response" || m["status"] != "completed" {
		t.Errorf("unexpected envelope: %v", m)
	}
	out := m["output"].([]any)[0].(map[string]any)
	part := out["content"].([]any)[0].(map[string]any)
	if !strings.Contains(part["text"].(string), "ping") {
		t.Errorf("reply did not echo input: %v", part["text"])
	}
}

func TestResponsesArrayInput(t *testing.T) {
	srv := newTestServer()
	rec := post(t, srv, "/v1/responses",
		`{"model":"m","input":[{"role":"user","content":[{"type":"input_text","text":"arraytext"}]}]}`)
	out := decode(t, rec)["output"].([]any)[0].(map[string]any)
	part := out["content"].([]any)[0].(map[string]any)
	if !strings.Contains(part["text"].(string), "arraytext") {
		t.Errorf("array input not extracted: %v", part["text"])
	}
}

func TestErrors(t *testing.T) {
	srv := newTestServer()
	// Malformed JSON.
	if rec := post(t, srv, "/v1/messages", `{bad`); rec.Code != 400 {
		t.Errorf("bad JSON status = %d, want 400", rec.Code)
	}
	// Missing model.
	if rec := post(t, srv, "/v1/chat/completions", `{"messages":[]}`); rec.Code != 400 {
		t.Errorf("missing model status = %d, want 400", rec.Code)
	}
}

// collectSSE reads an SSE stream and returns the ordered list of event names
// (for named events) and a flag for whether the OpenAI [DONE] sentinel appeared.
func collectSSE(t *testing.T, body io.Reader) (events []string, sawDone bool) {
	t.Helper()
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 1<<16), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if name, ok := strings.CutPrefix(line, "event: "); ok {
			events = append(events, name)
		}
		if line == "data: [DONE]" {
			sawDone = true
		}
	}
	return events, sawDone
}

func TestClaudeStream(t *testing.T) {
	srv := newTestServer()
	rec := post(t, srv, "/v1/messages",
		`{"model":"m","max_tokens":10,"stream":true,"messages":[{"role":"user","content":"hi there"}]}`)
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}
	events, _ := collectSSE(t, rec.Body)
	if len(events) == 0 || events[0] != "message_start" {
		t.Fatalf("first event = %v, want message_start", events)
	}
	if events[len(events)-1] != "message_stop" {
		t.Errorf("last event = %v, want message_stop", events[len(events)-1])
	}
	if !contains(events, "content_block_delta") {
		t.Errorf("no content_block_delta events: %v", events)
	}
}

func TestChatStream(t *testing.T) {
	srv := newTestServer()
	rec := post(t, srv, "/v1/chat/completions",
		`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	_, sawDone := collectSSE(t, rec.Body)
	if !sawDone {
		t.Errorf("chat stream missing [DONE] sentinel")
	}
}

func TestResponsesStream(t *testing.T) {
	srv := newTestServer()
	rec := post(t, srv, "/v1/responses",
		`{"model":"m","stream":true,"input":"hi"}`)
	events, _ := collectSSE(t, rec.Body)
	if len(events) == 0 || events[0] != "response.created" {
		t.Fatalf("first event = %v, want response.created", events)
	}
	if events[len(events)-1] != "response.completed" {
		t.Errorf("last event = %v, want response.completed", events[len(events)-1])
	}
	if !contains(events, "response.output_text.delta") {
		t.Errorf("no output_text.delta events: %v", events)
	}
}

func TestModels(t *testing.T) {
	srv := newTestServer()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if decode(t, rec)["object"] != "list" {
		t.Errorf("models list object mismatch")
	}
}

func TestRedact(t *testing.T) {
	if got := redact("sk-ant-secret-abcdef123456789"); strings.Contains(got, "secret") {
		t.Errorf("redact leaked secret: %q", got)
	}
	if got := redact("short"); got != "***" {
		t.Errorf("short secret = %q, want ***", got)
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
