# Gemini golden fixtures

Shapes for the native Gemini v1beta REST wire
(`generativelanguage.googleapis.com`, `POST /v1beta/models/{model}:generateContent`,
`:streamGenerateContent?alt=sse`, `:countTokens`, `GET /v1beta/models`),
consumed by `internal/translate/gemini/` and `internal/provider/gemini/`.

Sources: the official API reference
(`https://aistudio.google.com/api-reference/generate-content`, the redirect
target of `ai.google.dev/api/generate-content`) and the machine-readable
v1beta discovery document
(`https://generativelanguage.googleapis.com/$discovery/rest?version=v1beta`)
— the reference Google's own SDKs are generated from, and the stable v1beta
truth while Google's guides migrate to the newer Interactions API. Access
date is on every `.meta.json` sidecar (2026-11-03).

What is pinned:

- `generate-request.json` — the full GenerateContentRequest: contents[]
  (`role` user/model/function), parts (text, inlineData, functionCall with
  id/name/args, functionResponse with id/name/response), systemInstruction
  (text only), tools (functionDeclarations), toolConfig (mode ANY +
  allowedFunctionNames), generationConfig (typed members,
  thinkingConfig{includeThoughts, thinkingBudget, thinkingLevel},
  responseFormat.text; `seed` as the nested passthrough sample) and `labels`
  as the top-level passthrough sample.
- `generate-response.json` + `-blocked` + `-malformed` — the response side:
  thought parts (thought:true + verbatim thoughtSignature), the STOP finish
  on a function-call turn (this wire has no tool-calls finish reason), the
  complete finishReason enum family, promptFeedback.blockReason with no
  candidates, usageMetadata incl. thoughtsTokenCount.
- `stream-generate.sse` (+ canonical `.expected.json`) — the alt=sse form:
  data:-framed chunks, each a complete GenerateContentResponse, implicit
  block framing, the final chunk carrying finishReason + usageMetadata.
- `stream-blocked.sse` — a blocked prompt stream.
- `models-list.json` — the paginated catalog page (pageSize ≤ 1000 +
  pageToken): `models/{model}` names, the `thinking` flag, inputTokenLimit,
  supportedGenerationMethods (embedding rows are not chat models),
  nextPageToken.
- `count-tokens-request.json` / `count-tokens-response.json` — the mutually
  exclusive contents[]-vs-generateContentRequest rule (the provider sends the
  full request).
- `error-body.json` — the Google error body: `{"error":{code,message,status}}`.