package main

import (
	"encoding/json"
	"net/http"
)

// --- OpenAI Chat Completions request shape (subset) ---

type chatRequest struct {
	Model    string        `json:"model"`
	Stream   bool          `json:"stream"`
	Messages []chatMessage `json:"messages"`
}

type chatMessage struct {
	Role string `json:"role"`
	// Content is string in the common case but can be an array of parts.
	Content json.RawMessage `json:"content"`
}

func (req *chatRequest) lastUserText() string {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role != "user" {
			continue
		}
		return extractOpenAIContent(req.Messages[i].Content)
	}
	return ""
}

// extractOpenAIContent handles both a plain string and the multimodal parts
// array (`[{"type":"text","text":"..."}]`).
func extractOpenAIContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil {
		var out string
		for _, p := range parts {
			if p.Type == "text" {
				if out != "" {
					out += "\n"
				}
				out += p.Text
			}
		}
		return out
	}
	return ""
}

func handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}

	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "could not parse request body: "+err.Error())
		return
	}
	if req.Model == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "model is required")
		return
	}

	reply := cannedReply(req.lastUserText())
	id := "chatcmpl-" + randID()
	created := fixedCreated

	if req.Stream {
		streamChat(w, req.Model, id, created, reply)
		return
	}

	promptTokens := approxTokens(req.lastUserText())
	completionTokens := approxTokens(reply)
	resp := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": created,
		"model":   req.Model,
		"choices": []map[string]any{{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": reply},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{
			"prompt_tokens":     promptTokens,
			"completion_tokens": completionTokens,
			"total_tokens":      promptTokens + completionTokens,
		},
	}
	writeJSON(w, http.StatusOK, resp)
}

func streamChat(w http.ResponseWriter, model, id string, created int64, reply string) {
	sse, ok := newSSEWriter(w)
	if !ok {
		writeOpenAIError(w, http.StatusInternalServerError, "api_error", "streaming unsupported")
		return
	}

	chunk := func(delta map[string]any, finish any) string {
		return jsonString(map[string]any{
			"id":      id,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   model,
			"choices": []map[string]any{{
				"index":         0,
				"delta":         delta,
				"finish_reason": finish,
			}},
		})
	}

	// First chunk carries the role.
	sse.data(chunk(map[string]any{"role": "assistant"}, nil))

	for _, tok := range tokens(reply) {
		sse.data(chunk(map[string]any{"content": tok}, nil))
	}

	sse.data(chunk(map[string]any{}, "stop"))
	sse.data("[DONE]")
}

func writeOpenAIError(w http.ResponseWriter, status int, errType, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{"type": errType, "message": msg, "code": nil, "param": nil},
	})
}
