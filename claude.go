package main

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// --- Anthropic Messages API request shape (the subset we care about) ---

type claudeRequest struct {
	Model     string          `json:"model"`
	MaxTokens int             `json:"max_tokens"`
	Stream    bool            `json:"stream"`
	System    json.RawMessage `json:"system"`
	Messages  []claudeMessage `json:"messages"`
}

type claudeMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// lastUserText extracts the text of the final user message. Anthropic content
// can be either a plain string or an array of typed blocks.
func (req *claudeRequest) lastUserText() string {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role != "user" {
			continue
		}
		return extractText(req.Messages[i].Content)
	}
	return ""
}

// extractText handles both `"content": "..."` and
// `"content": [{"type":"text","text":"..."}]`.
func extractText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var out string
		for _, b := range blocks {
			if b.Type == "text" {
				if out != "" {
					out += "\n"
				}
				out += b.Text
			}
		}
		return out
	}
	return ""
}

func handleClaudeMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeClaudeError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}

	var req claudeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeClaudeError(w, http.StatusBadRequest, "invalid_request_error", "could not parse request body: "+err.Error())
		return
	}
	if req.Model == "" {
		writeClaudeError(w, http.StatusBadRequest, "invalid_request_error", "model is required")
		return
	}

	reply := cannedReply(req.lastUserText())
	msgID := "msg_" + randID()

	if req.Stream {
		streamClaude(w, req.Model, msgID, reply)
		return
	}

	inTokens := approxTokens(req.lastUserText())
	outTokens := approxTokens(reply)
	resp := map[string]any{
		"id":            msgID,
		"type":          "message",
		"role":          "assistant",
		"model":         req.Model,
		"content":       []map[string]any{{"type": "text", "text": reply}},
		"stop_reason":   "end_turn",
		"stop_sequence": nil,
		"usage": map[string]any{
			"input_tokens":  inTokens,
			"output_tokens": outTokens,
		},
	}
	writeJSON(w, http.StatusOK, resp)
}

// streamClaude emits the Anthropic SSE sequence for a single text block.
func streamClaude(w http.ResponseWriter, model, msgID, reply string) {
	sse, ok := newSSEWriter(w)
	if !ok {
		writeClaudeError(w, http.StatusInternalServerError, "api_error", "streaming unsupported")
		return
	}
	inTokens := 1
	outTokens := approxTokens(reply)

	messageStart := map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            msgID,
			"type":          "message",
			"role":          "assistant",
			"model":         model,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         map[string]any{"input_tokens": inTokens, "output_tokens": 0},
		},
	}
	sse.event("message_start", jsonString(messageStart))

	sse.event("content_block_start", jsonString(map[string]any{
		"type":          "content_block_start",
		"index":         0,
		"content_block": map[string]any{"type": "text", "text": ""},
	}))

	sse.event("ping", jsonString(map[string]any{"type": "ping"}))

	for _, tok := range tokens(reply) {
		sse.event("content_block_delta", jsonString(map[string]any{
			"type":  "content_block_delta",
			"index": 0,
			"delta": map[string]any{"type": "text_delta", "text": tok},
		}))
	}

	sse.event("content_block_stop", jsonString(map[string]any{
		"type":  "content_block_stop",
		"index": 0,
	}))

	sse.event("message_delta", jsonString(map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": outTokens},
	}))

	sse.event("message_stop", jsonString(map[string]any{"type": "message_stop"}))
}

func writeClaudeError(w http.ResponseWriter, status int, errType, msg string) {
	writeJSON(w, status, map[string]any{
		"type":  "error",
		"error": map[string]any{"type": errType, "message": msg},
	})
}

func jsonString(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("{\"error\":%q}", err.Error())
	}
	return string(b)
}
