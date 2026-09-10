# OpenAI API — Golden Fixtures

Wire shapes captured from the official OpenAI API documentation, per plan §2
facts O1–O12. Provenance in the `.meta.json` sidecars. The stream fixture is
raw SSE wire bytes so the contract test exercises the real parser.

Fixture set:

- `chat-request.*` — chat/completions request (O2, O3, O11): system role,
  image_url part, tools, tool_calls assembly input, tool result message.
- `chat-response.*` — non-streaming response (O3, O12): tool_calls in
  message, finish_reason, usage.
- `chat-stream.sse` — streaming chunks (O4): role delta, content fragments,
  tool_calls index-assembly deltas, terminating chunk with finish_reason,
  usage chunk per stream_options.include_usage.
- `models.*` — GET /v1/models response shape (O9).
- `error.*` — error body shape (O10).