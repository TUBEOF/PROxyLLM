# Golden Fixtures

Every wire-protocol shape PROxyLLM handles is captured here as JSON,
**annotated with source URL and access date** — tests derive exclusively
from these specifications, never from generated code behavior (ADR-006).

Layout:

```
testdata/golden/
├── anthropic/       # Messages API request/response/streaming shapes
├── openai/          # chat/completions + responses shapes
├── ollama/          # native /api/chat shapes
└── firecrawl/       # v2 search/scrape request/response shapes
```

## Rules

1. **Provenance is mandatory**: each fixture file carries a header comment
   (or `.meta.json` sidecar) with `source` (URL) and `accessed` (date).
2. **No invention**: fixtures come from official documentation or captured
   live traffic — never hand-written "probably looks like this" samples.
3. **Re-validation**: when a reference changes, re-read it and update the
   fixture and its `.meta.json` in the same change. A drift means the spec
   moved, not that the code is wrong — and nothing automated can tell the
   difference, so the re-read is the review's job.
4. **RED-GREEN**: contract tests are written against the fixture before the
   codec exists; they start red and go green only via correct code.

## What the set covers

One directory per wire surface the proxy speaks or calls: `anthropic/` (the
Messages API and its event stream), `openai/` (chat completions and the Responses
API), `ollama/` (the native `/api/chat` NDJSON stream) and `firecrawl/` (the
web-tool backend). Each fixture pins a request, a response or a stream event, and
each `.meta.json` beside it records where the shape came from and when it was
read. (`_templates/` is not a surface: it holds the empty
`fixture.meta.json` every new sidecar starts from.)

The fixtures are the specification, not a record of what the code does. When a
reference changes, the fixture and its `.meta.json` change first — a fixture that
encodes a shape the reference no longer describes is a defect.
## Regenerating derived expectations (GOLDEN_UPDATE, ADR-028)

Only **derived** files end in `.expected.json`; their sources are the
transcribed request/response fixtures. To regenerate a derived expectation
after re-reading the vendor documentation:

```bash
GOLDEN_UPDATE=1 go test ./internal/translate/openai -run TestStream
git diff testdata/golden/     # this diff is the review artifact
```

The helper refuses to run with `GOLDEN_UPDATE=1` when `CI` is set, and it
refuses any path that is not a `.expected.json` file. Without
`GOLDEN_UPDATE=1` a mismatch fails with a diff — CI never regenerates what it
checks.
