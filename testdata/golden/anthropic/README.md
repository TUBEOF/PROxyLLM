# Anthropic Messages API — Golden Fixtures

Wire shapes captured from the official Anthropic documentation, per plan §2
facts A1–A14. Provenance per fixture in the `.meta.json` sidecars. Shapes are
field-complete per the documented schema; placeholder values (model IDs,
token ids, base64 payloads) use the documentation's own example conventions.

Fixture set:

- `message-request.*` — Messages create request (A1, A3, A11, A12): text and
  image content blocks, block-form system, tools, thinking, cache_control.
- `message-response.*` — non-streaming response (A3, A4, A11): text +
  tool_use blocks, stop_reason, usage with cache fields.
- `message-request-thinking.*` — extended-thinking request (A12).
- `stream-events.*` — full ordered SSE event sequence (A5–A7): message_start
  → block_start → deltas (text, input_json_delta, thinking_delta,
  signature_delta) → block_stop → message_delta → message_stop, ping, error.
- `stream-error.*` — mid-stream error event shape (A13).
- `count-tokens.*` — count_tokens request/response (A2).