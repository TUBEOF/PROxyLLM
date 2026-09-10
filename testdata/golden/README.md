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
3. **Re-validation**: `make fixtures-check` diffs fixtures against current
   live docs; a drift means the spec moved, not that our code is wrong.
4. **RED-GREEN**: contract tests are written against the fixture before the
   codec exists; they start red and go green only via correct code.

## Phase 0 status

Directory scaffolded; fixture capture begins with Phase 1 (translation
engine contract tests). The fixture set is listed in the plan's §2 Research
Digest (A1–A14, O1–O12, L1–L6, F1–F5).