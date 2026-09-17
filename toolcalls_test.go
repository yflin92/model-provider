package main

import (
	"io"
	"reflect"
	"testing"
)

func TestRequestToolsAllFormats(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []string
	}{
		{
			name: "openai chat",
			body: `{"tools":[{"type":"function","function":{"name":"get_weather"}},{"type":"function","function":{"name":"send_email"}}]}`,
			want: []string{"get_weather", "send_email"},
		},
		{
			name: "anthropic",
			body: `{"tools":[{"name":"get_weather","input_schema":{}},{"name":"search"}]}`,
			want: []string{"get_weather", "search"},
		},
		{
			name: "openai responses",
			body: `{"tools":[{"type":"function","name":"lookup"}]}`,
			want: []string{"lookup"},
		},
		{
			name: "dedupe",
			body: `{"tools":[{"function":{"name":"a"}},{"function":{"name":"a"}}]}`,
			want: []string{"a"},
		},
		{name: "none", body: `{"model":"m"}`, want: nil},
		{name: "empty", body: ``, want: nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := requestTools([]byte(c.body)); !reflect.DeepEqual(got, c.want) {
				t.Errorf("requestTools = %v, want %v", got, c.want)
			}
		})
	}
}

func TestResponseToolsNonStreaming(t *testing.T) {
	cases := []struct {
		name, ct, body string
		want           []string
	}{
		{
			name: "openai chat",
			ct:   "application/json",
			body: `{"choices":[{"message":{"tool_calls":[{"function":{"name":"get_weather"}},{"function":{"name":"send_email"}}]}}]}`,
			want: []string{"get_weather", "send_email"},
		},
		{
			name: "anthropic",
			ct:   "application/json",
			body: `{"content":[{"type":"text","text":"hi"},{"type":"tool_use","name":"get_weather"}]}`,
			want: []string{"get_weather"},
		},
		{
			name: "openai responses",
			ct:   "application/json",
			body: `{"output":[{"type":"function_call","name":"lookup"},{"type":"message"}]}`,
			want: []string{"lookup"},
		},
		{
			name: "no tools",
			ct:   "application/json",
			body: `{"choices":[{"message":{"content":"hi"}}]}`,
			want: nil,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := responseTools(c.ct, []byte(c.body)); !reflect.DeepEqual(got, c.want) {
				t.Errorf("responseTools = %v, want %v", got, c.want)
			}
		})
	}
}

func TestResponseToolsStreaming(t *testing.T) {
	cases := []struct {
		name, ct, body string
		want           []string
	}{
		{
			name: "openai chat chunks",
			ct:   "text/event-stream",
			body: "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"name\":\"get_weather\"}}]}}]}\n\n" +
				"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{}\"}}]}}]}\n\n" +
				"data: [DONE]\n\n",
			want: []string{"get_weather"},
		},
		{
			name: "anthropic content_block_start",
			ct:   "text/event-stream",
			body: "event: content_block_start\n" +
				"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"name\":\"search\"}}\n\n",
			want: []string{"search"},
		},
		{
			name: "responses output_item.added",
			ct:   "text/event-stream",
			body: "event: response.output_item.added\n" +
				"data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"function_call\",\"name\":\"lookup\"}}\n\n",
			want: []string{"lookup"},
		},
		{
			name: "sniff without content type",
			ct:   "",
			body: "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"function\":{\"name\":\"x\"}}]}}]}\n\n",
			want: []string{"x"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := responseTools(c.ct, []byte(c.body)); !reflect.DeepEqual(got, c.want) {
				t.Errorf("responseTools = %v, want %v", got, c.want)
			}
		})
	}
}

func TestTeeReaderCapsCapture(t *testing.T) {
	src := make([]byte, 10)
	for i := range src {
		src[i] = byte('a' + i)
	}
	tee := newTeeReader(&sliceReader{data: src}, 4)
	out := make([]byte, len(src))
	n, _ := tee.Read(out)
	if n != len(src) {
		t.Fatalf("read %d bytes, want %d", n, len(src))
	}
	if got := string(tee.captured()); got != "abcd" {
		t.Errorf("captured = %q, want first 4 bytes %q", got, "abcd")
	}
}

// sliceReader returns all its data in a single Read, then EOF.
type sliceReader struct {
	data []byte
	done bool
}

func (s *sliceReader) Read(p []byte) (int, error) {
	if s.done {
		return 0, io.EOF
	}
	n := copy(p, s.data)
	s.done = true
	return n, nil
}
