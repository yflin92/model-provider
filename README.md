# mock-model-provider

A single-binary mock LLM API server that speaks **both the Anthropic (Claude) and
OpenAI (Codex) wire formats**, returns deterministic canned responses, and logs
every request. No API keys, no network calls, no dependencies beyond the Go
standard library.

Use it to point Claude Code, the OpenAI/Codex CLI, or any SDK at a fake endpoint
for local development, integration tests, and CI — and to inspect exactly what
those clients send on the wire.

## Endpoints

| Path | Format | Streaming |
| --- | --- | --- |
| `POST /v1/messages` | Anthropic Messages | `event:`-named SSE (`message_start` … `message_stop`) |
| `POST /v1/chat/completions` | OpenAI Chat Completions | `choices[].delta` chunks ending in `data: [DONE]` |
| `POST /v1/responses` | OpenAI Responses (newer Codex) | typed SSE (`response.created` … `response.completed`) |
| `GET /v1/models`, `GET /v1/models/{id}` | Model listing (both ecosystems) | — |
| `GET /healthz` | Health check | — |

Each POST endpoint supports both non-streaming and streaming (set `"stream": true`).

## Run

```sh
go run .                 # listens on :8080
go run . -addr :9000     # custom port
ADDR=:9000 go run .      # or via env var
go run . -minimal        # log only endpoint + model per request
MINIMAL=1 go run .       # or via env var
```

Proxy mode — relay to a real upstream (OpenRouter) instead of returning canned
replies:

```sh
export OPENROUTER_API_KEY=sk-or-...
go run . -proxy https://openrouter.ai/api/v1
PROXY=https://openrouter.ai/api/v1 go run .   # or via env var

# combine with -minimal to log just endpoint + model + upstream status per call
go run . -proxy https://openrouter.ai/api/v1 -minimal
```

Build a standalone binary:

```sh
go build -o mock-model-provider .
./mock-model-provider
```

## Behavior

- **Deterministic canned replies.** Every response echoes the last user message
  back (`This is a mock response. You said: "…"`), so tests can assert the mock
  saw exactly what the client sent. Response IDs are random; timestamps are
  fixed for reproducibility.
- **Request logging.** Every request is logged with a monotonic id, method,
  path, headers, and a pretty-printed JSON body. Credentials
  (`Authorization`, `x-api-key`, `api-key`, `Cookie`) are redacted — a short
  prefix/suffix is kept so you can still tell keys apart. A paired response line
  records the status and byte count.
- **Minimal logging.** Pass `-minimal` (or `MINIMAL=1`) to log only a single
  `MODEL <method> <path> -> <model>` line per request, suppressing the header/
  body dump and the response line. The quiet way to watch which model a harness
  asks for on each call.
- **Proxy mode.** Pass `-proxy <base-url>` (or `PROXY=<base-url>`) to relay every
  request to a real upstream — e.g. OpenRouter at `https://openrouter.ai/api/v1`
  — and stream the response (status, headers, body) straight back to the client
  instead of returning a canned reply. The request path, body, and headers are
  forwarded as-is; when `OPENROUTER_API_KEY` is set it is sent as
  `Authorization: Bearer <key>`. With `-minimal`, each request logs the inbound
  `MODEL` line plus a `PROXY <method> <endpoint> (model …) -> <status>` line, so
  you see endpoint, model, and the status returned by OpenRouter per request.
  When a request offers tools or the model calls them, two more lines list the
  names — `PROXY tools offered: …` (from the request) and `PROXY tool calls: …`
  (from the response). Tool-call parsing tolerates all three formats (Anthropic
  `tool_use`, OpenAI Chat `tool_calls`, OpenAI Responses `function_call`), for
  both streaming and non-streaming responses.

## Point a client at it

Anthropic SDK / Claude Code:

```sh
export ANTHROPIC_BASE_URL=http://localhost:8080
export ANTHROPIC_API_KEY=sk-mock   # any non-empty value; it's ignored
```

OpenAI / Codex:

```sh
export OPENAI_BASE_URL=http://localhost:8080/v1
export OPENAI_API_KEY=sk-mock
```

## Quick check

```sh
# Claude
curl http://localhost:8080/v1/messages -H 'content-type: application/json' \
  -d '{"model":"claude-opus-4-8","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}'

# OpenAI chat
curl http://localhost:8080/v1/chat/completions -H 'content-type: application/json' \
  -d '{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}'

# OpenAI responses (streaming)
curl -N http://localhost:8080/v1/responses -H 'content-type: application/json' \
  -d '{"model":"gpt-5-codex","stream":true,"input":"hi"}'
```

## Test

```sh
go test ./...
```

## Layout

| File | Contents |
| --- | --- |
| `main.go` | Routing, server startup, `/v1/models`, shared JSON helpers |
| `logging.go` | Request/response logging middleware with credential redaction |
| `reply.go` | Canned-reply generation, tokenizer, SSE writer |
| `claude.go` | `/v1/messages` handler (Anthropic format) |
| `openai_chat.go` | `/v1/chat/completions` handler |
| `openai_responses.go` | `/v1/responses` handler |
| `proxy.go` | Proxy mode — relay requests to an upstream (OpenRouter) |
| `toolcalls.go` | Tool-call extraction from requests/responses (all formats) |
| `server_test.go` | End-to-end handler tests |
| `proxy_test.go` | Proxy-mode relay + logging tests |
| `toolcalls_test.go` | Tool-call extraction tests |
