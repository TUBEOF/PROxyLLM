# Ollama Native /api/chat — Golden Fixtures

Wire shapes from the Ollama API documentation (docs.ollama.com/api/chat, accessed
2026-09-10). Residual risk (the exact `done_reason` value strings) is pinned by the
live smoke test against a real endpoint, not by these fixtures.

Fixture set:

- `chat-request.*` — native request: model, messages, stream:false
  explicit (stream defaults to true!), think parameter, tools.
- `chat-response.*` — non-streaming response: message{role, content,
  thinking, tool_calls with object arguments}, done, done_reason, usage
  counts.
- `chat-stream.ndjson` — newline-delimited JSON chunks: partial
  message.content per chunk, final chunk with done:true, done_reason,
  full usage. NOT SSE — no data: framing.