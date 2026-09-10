# Ollama Native /api/chat — Golden Fixtures

Wire shapes per plan §2 facts L1–L4 and the Phase 1 research pass facts
L7–L11 (docs.ollama.com/api/chat, accessed 2026-09-10). Residual risk
(exact `done_reason` value strings) is pinned by the local-instance contract
smoke from OP-4, not by these fixtures.

Fixture set:

- `chat-request.*` — native request: model, messages, stream:false
  explicit (L8 — stream defaults to true!), think parameter (L9), tools.
- `chat-response.*` — non-streaming response (L7): message{role, content,
  thinking, tool_calls with object arguments}, done, done_reason, usage
  counts.
- `chat-stream.ndjson` — newline-delimited JSON chunks (L10): partial
  message.content per chunk, final chunk with done:true, done_reason,
  full usage. NOT SSE — no data: framing.