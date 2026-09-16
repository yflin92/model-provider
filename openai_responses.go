package main

import (
	"encoding/json"
	"net/http"
)

// --- OpenAI Responses API request shape (subset) ---
//
// `input` may be a plain string or an array of message items, each with
// `content` that is itself a string or an array of typed parts. This handler
// tolerates all of those shapes.

type responsesRequest struct {
	Model  string          `json:"model"`
	Stream bool            `json:"stream"`
	Input  json.RawMessage `json:"input"`
}

func (req *responsesRequest) lastUserText() string {
	if len(req.Input) == 0 {
		return ""
	}
	// Case 1: input is a plain string.
	var s string
	if err := json.Unmarshal(req.Input, &s); err == nil {
		return s
	}
	// Case 2: input is an array of message items.
	var items []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(req.Input, &items); err == nil {
		for i := len(items) - 1; i >= 0; i-- {
			if items[i].Role != "" && items[i].Role != "user" {
				continue
			}
			if txt := extractResponsesContent(items[i].Content); txt != "" {
				return txt
			}
		}
	}
	return ""
}

// extractResponsesContent handles a string or an array of parts. Responses
// parts use `input_text` / `output_text` type tags.
func extractResponsesContent(raw json.RawMessage) string {
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
			if p.Text == "" {
				continue
			}
			if out != "" {
				out += "\n"
			}
			out += p.Text
		}
		return out
	}
	return ""
}

func handleResponses(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}

	var req responsesRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "could not parse request body: "+err.Error())
		return
	}
	if req.Model == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "model is required")
		return
	}

	reply := cannedReply(req.lastUserText())
	id := "resp_" + randID()
	msgID := "msg_" + randID()
	created := fixedCreated

	if req.Stream {
		streamResponses(w, req.Model, id, msgID, created, reply)
		return
	}

	writeJSON(w, http.StatusOK, responsesObject(req.Model, id, msgID, created, reply, "completed"))
}

// responsesObject builds the top-level `response` object returned both as the
// non-streaming body and inside the streaming lifecycle events.
func responsesObject(model, id, msgID string, created int64, reply, status string) map[string]any {
	inTokens := approxTokens(reply)
	outTokens := approxTokens(reply)
	var output []any
	if status == "completed" {
		output = []any{map[string]any{
			"type":   "message",
			"id":     msgID,
			"role":   "assistant",
			"status": "completed",
			"content": []any{map[string]any{
				"type":        "output_text",
				"text":        reply,
				"annotations": []any{},
			}},
		}}
	} else {
		output = []any{}
	}
	return map[string]any{
		"id":         id,
		"object":     "response",
		"created_at": created,
		"status":     status,
		"model":      model,
		"output":     output,
		"usage": map[string]any{
			"input_tokens":  inTokens,
			"output_tokens": outTokens,
			"total_tokens":  inTokens + outTokens,
		},
	}
}

func streamResponses(w http.ResponseWriter, model, id, msgID string, created int64, reply string) {
	sse, ok := newSSEWriter(w)
	if !ok {
		writeOpenAIError(w, http.StatusInternalServerError, "api_error", "streaming unsupported")
		return
	}

	emit := func(eventType string, payload map[string]any) {
		payload["type"] = eventType
		sse.event(eventType, jsonString(payload))
	}

	emit("response.created", map[string]any{
		"response": responsesObject(model, id, msgID, created, reply, "in_progress"),
	})
	emit("response.in_progress", map[string]any{
		"response": responsesObject(model, id, msgID, created, reply, "in_progress"),
	})

	emit("response.output_item.added", map[string]any{
		"output_index": 0,
		"item": map[string]any{
			"type": "message", "id": msgID, "role": "assistant", "status": "in_progress",
			"content": []any{},
		},
	})
	emit("response.content_part.added", map[string]any{
		"item_id": msgID, "output_index": 0, "content_index": 0,
		"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
	})

	for _, tok := range tokens(reply) {
		emit("response.output_text.delta", map[string]any{
			"item_id": msgID, "output_index": 0, "content_index": 0, "delta": tok,
		})
	}

	emit("response.output_text.done", map[string]any{
		"item_id": msgID, "output_index": 0, "content_index": 0, "text": reply,
	})
	emit("response.content_part.done", map[string]any{
		"item_id": msgID, "output_index": 0, "content_index": 0,
		"part": map[string]any{"type": "output_text", "text": reply, "annotations": []any{}},
	})
	emit("response.output_item.done", map[string]any{
		"output_index": 0,
		"item": map[string]any{
			"type": "message", "id": msgID, "role": "assistant", "status": "completed",
			"content": []any{map[string]any{"type": "output_text", "text": reply, "annotations": []any{}}},
		},
	})
	emit("response.completed", map[string]any{
		"response": responsesObject(model, id, msgID, created, reply, "completed"),
	})
}
