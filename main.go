package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"os"
)

// fixedCreated is a stable timestamp used in `created`/`created_at` fields so
// responses are fully deterministic (useful for golden-file tests).
const fixedCreated int64 = 1735689600 // 2025-01-01T00:00:00Z

func main() {
	addr := flag.String("addr", envOr("ADDR", ":8080"), "listen address")
	flag.Parse()

	mux := http.NewServeMux()

	// Anthropic / Claude format.
	mux.HandleFunc("/v1/messages", handleClaudeMessages)

	// OpenAI / Codex formats.
	mux.HandleFunc("/v1/chat/completions", handleChatCompletions)
	mux.HandleFunc("/v1/responses", handleResponses)

	// Model listing endpoints both ecosystems probe.
	mux.HandleFunc("/v1/models", handleModels)
	mux.HandleFunc("/v1/models/", handleModel)

	// Health check.
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
	})

	handler := logging(mux)

	log.Printf("mock model provider listening on %s", *addr)
	log.Printf("  Claude:            POST %s/v1/messages", *addr)
	log.Printf("  OpenAI chat:       POST %s/v1/chat/completions", *addr)
	log.Printf("  OpenAI responses:  POST %s/v1/responses", *addr)
	if err := http.ListenAndServe(*addr, handler); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

// --- shared helpers ---

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

// randID returns a short random hex id for response identifiers.
func randID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "0000000000000000"
	}
	return hex.EncodeToString(b[:])
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// mockModels are advertised by both /v1/models formats.
var mockModels = []string{
	"claude-opus-4-8",
	"claude-sonnet-5",
	"gpt-5-codex",
	"gpt-4o",
}

func handleModels(w http.ResponseWriter, r *http.Request) {
	data := make([]map[string]any, 0, len(mockModels))
	for _, id := range mockModels {
		data = append(data, modelObject(id))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   data,
	})
}

func handleModel(w http.ResponseWriter, r *http.Request) {
	const prefix = "/v1/models/"
	id := r.URL.Path[len(prefix):]
	if id == "" {
		writeOpenAIError(w, http.StatusNotFound, "invalid_request_error", "model not found")
		return
	}
	writeJSON(w, http.StatusOK, modelObject(id))
}

func modelObject(id string) map[string]any {
	return map[string]any{
		"id":       id,
		"object":   "model",
		"created":  fixedCreated,
		"owned_by": "mock",
	}
}
