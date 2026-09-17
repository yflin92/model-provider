package main

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
)

// This file extracts tool-call names from requests and responses so proxy mode
// can log them. It tolerates all three wire formats the server speaks —
// Anthropic Messages, OpenAI Chat Completions, and OpenAI Responses — because a
// proxied client could be any of them, and it never assumes which one is in use:
// each extractor probes the fields for every format and returns whatever it
// finds.

// requestTools returns the names of tools the client *offered* upstream, pulled
// from the request body. Order is preserved; duplicates are removed.
//
//	OpenAI Chat:      tools[].function.name
//	Anthropic:        tools[].name
//	OpenAI Responses: tools[].name  (also custom tools with a top-level name)
func requestTools(body []byte) []string {
	if len(body) == 0 {
		return nil
	}
	var payload struct {
		Tools []struct {
			Name     string `json:"name"`
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil
	}
	var names []string
	for _, t := range payload.Tools {
		if t.Function.Name != "" {
			names = append(names, t.Function.Name)
		} else if t.Name != "" {
			names = append(names, t.Name)
		}
	}
	return dedupe(names)
}

// responseTools returns the names of tool calls the model *made* in a response.
// It handles both a single non-streaming JSON body and a streamed SSE body,
// across all three formats. Order is preserved; duplicates are removed.
func responseTools(contentType string, body []byte) []string {
	if len(body) == 0 {
		return nil
	}
	if isSSE(contentType, body) {
		return dedupe(toolsFromSSE(body))
	}
	return dedupe(toolsFromJSON(body))
}

// isSSE reports whether a response body should be parsed as Server-Sent Events
// rather than a single JSON object, keying off the content type with a body
// sniff as a fallback.
func isSSE(contentType string, body []byte) bool {
	if strings.Contains(strings.ToLower(contentType), "text/event-stream") {
		return true
	}
	return bytes.HasPrefix(bytes.TrimSpace(body), []byte("event:")) ||
		bytes.HasPrefix(bytes.TrimSpace(body), []byte("data:"))
}

// toolsFromJSON extracts tool-call names from a single non-streaming response
// object in any of the three formats.
//
//	OpenAI Chat:      choices[].message.tool_calls[].function.name
//	Anthropic:        content[].type=="tool_use" -> name
//	OpenAI Responses: output[].type=="function_call" -> name
func toolsFromJSON(body []byte) []string {
	var obj struct {
		// OpenAI Chat
		Choices []struct {
			Message struct {
				ToolCalls []toolCall `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		// Anthropic
		Content []contentBlock `json:"content"`
		// OpenAI Responses
		Output []outputItem `json:"output"`
	}
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil
	}
	var names []string
	for _, c := range obj.Choices {
		for _, tc := range c.Message.ToolCalls {
			if n := tc.name(); n != "" {
				names = append(names, n)
			}
		}
	}
	for _, b := range obj.Content {
		if b.Type == "tool_use" && b.Name != "" {
			names = append(names, b.Name)
		}
	}
	for _, o := range obj.Output {
		if o.Type == "function_call" && o.Name != "" {
			names = append(names, o.Name)
		}
	}
	return names
}

// toolsFromSSE walks the `data:` payloads of a streamed response and collects
// tool-call names. Each format announces a tool call up front (Chat streams the
// name in the first tool_calls delta; Anthropic in content_block_start;
// Responses in response.output_item.added), so we only need the naming events,
// not the argument deltas.
func toolsFromSSE(body []byte) []string {
	var names []string
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		names = append(names, toolsFromSSEEvent([]byte(data))...)
	}
	return names
}

// toolsFromSSEEvent pulls tool-call names out of one decoded SSE data payload,
// probing the naming event of each format.
func toolsFromSSEEvent(data []byte) []string {
	var ev struct {
		// OpenAI Chat chunk
		Choices []struct {
			Delta struct {
				ToolCalls []toolCall `json:"tool_calls"`
			} `json:"delta"`
		} `json:"choices"`
		// Anthropic content_block_start
		Type         string       `json:"type"`
		ContentBlock contentBlock `json:"content_block"`
		// OpenAI Responses output_item.added
		Item outputItem `json:"item"`
	}
	if err := json.Unmarshal(data, &ev); err != nil {
		return nil
	}
	var names []string
	for _, c := range ev.Choices {
		for _, tc := range c.Delta.ToolCalls {
			if n := tc.name(); n != "" {
				names = append(names, n)
			}
		}
	}
	if ev.Type == "content_block_start" && ev.ContentBlock.Type == "tool_use" && ev.ContentBlock.Name != "" {
		names = append(names, ev.ContentBlock.Name)
	}
	if ev.Type == "response.output_item.added" && ev.Item.Type == "function_call" && ev.Item.Name != "" {
		names = append(names, ev.Item.Name)
	}
	return names
}

// toolCall is the OpenAI tool-call shape shared by non-streaming messages and
// streaming deltas.
type toolCall struct {
	Function struct {
		Name string `json:"name"`
	} `json:"function"`
}

func (tc toolCall) name() string { return tc.Function.Name }

// contentBlock is the Anthropic content block shape (only the fields we read).
type contentBlock struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

// outputItem is the OpenAI Responses output item shape (only the fields we read).
type outputItem struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

// dedupe returns names with duplicates removed, preserving first-seen order.
func dedupe(names []string) []string {
	if len(names) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(names))
	out := names[:0:0]
	for _, n := range names {
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
}

// teeReader wraps an io.Reader, capturing up to a byte limit of what passes
// through so the proxy can parse tool calls out of a response it is streaming
// straight to the client. Reads past the cap still stream to the client; they
// just stop being buffered, so tool-call logging is best-effort on very large
// responses.
type teeReader struct {
	r   io.Reader
	buf bytes.Buffer
	cap int
}

func newTeeReader(r io.Reader, capBytes int) *teeReader {
	return &teeReader{r: r, cap: capBytes}
}

func (t *teeReader) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if n > 0 && t.buf.Len() < t.cap {
		room := t.cap - t.buf.Len()
		if room > n {
			room = n
		}
		t.buf.Write(p[:room])
	}
	return n, err
}

// captured returns the bytes buffered so far.
func (t *teeReader) captured() []byte { return t.buf.Bytes() }
