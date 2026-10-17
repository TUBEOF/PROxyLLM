# Architecture decision records

One file per significant decision, in the format context → criteria → options
considered → decision → consequences. The `ADR-NNN` identifiers are stable:
the source code cites them in its comments (e.g. "the sync writes `synced`
rows — ADR-010"), so an identifier always resolves to the file of the same
name. A decision that later changes is amended in place with a dated note;
its identifier and its history stay.

| ADR | Decision | Status |
|---|---|---|
| [ADR-001](ADR-001-go-stdlib.md) | Backend: Go + stdlib net/http | Accepted |
| [ADR-002](ADR-002-react-vite-embed.md) | WebUI: React 18 + Vite + TS, embedded via go:embed | Accepted |
| [ADR-003](ADR-003-sqlite-modernc.md) | Persistence: SQLite via modernc.org/sqlite | Accepted |
| [ADR-004](ADR-004-ir-hub-and-spoke.md) | Translation: canonical IR hub-and-spoke | Accepted |
| [ADR-005](ADR-005-routing-rule-model.md) | Routing: deterministic rule model | Accepted |
| [ADR-006](ADR-006-test-strategy.md) | Testing: spec-derived golden fixtures, RED-GREEN | Accepted |
| [ADR-007](ADR-007-security-concept.md) | Security: auth default-on, envelope encryption, redaction-first | Accepted |
| [ADR-008](ADR-008-non-claude-routing.md) | Non-Claude routing via Anthropic surface: first-class feature | Accepted |
| [ADR-009](ADR-009-provider-accounts-and-strategies.md) | Provider accounts (rows per type) and routing strategies (round robin / fill-first) | Accepted |
| [ADR-010](ADR-010-model-sync-and-capability-fallbacks.md) | Model sync + capability fallback lists | Accepted |
| [ADR-011](ADR-011-implicit-model-routes.md) | Implicit model routes when no rule matches | Accepted |
| [ADR-012](ADR-012-account-usage-quota-page.md) | Per-account usage snapshot (Quota page): scrape-or-local KV blob, usage-cookie token kind | Accepted |
| [ADR-013](ADR-013-context-window-aware-fallbacks.md) | Context-window-aware fallbacks: `context_window_exceeded` class + pre-dispatch gate on the fallback walk | Accepted (gate superseded by ADR-014) |
| [ADR-014](ADR-014-image-to-text-vision-fallback.md) | Image-to-text vision fallback: vision models describe the image, the routed model answers | Accepted |
| [ADR-015](ADR-015-web-tool-emulation-resilience.md) | Web-tool emulation resilience: ordered account walk, bounded time, honest empty answers, per-account tracing | Accepted |
| [ADR-016](ADR-016-scalability-and-concurrency.md) | Scalability and concurrency: SQLite pool, key-hash caching, lock splitting, emulation budgets | Accepted |
| [ADR-017](ADR-017-decision-models.md) | Decision models: a second IR operation class (System One / Jev, the OpenAI Decisions API) | Accepted |

ADR template: [TEMPLATE.md](TEMPLATE.md)

## The system in one screen

The decisions below describe a single Go binary that serves the client API,
the operator console and the routing engine from one process.

A request enters through a surface handler, is decoded into the canonical
intermediate representation (IR), routed, dispatched to an upstream provider,
and encoded back into the client's protocol — with the trace recorded
alongside:

```
client ──▶ surface handler ──▶ router ──▶ executor ──▶ codec ──▶ provider ──▶ upstream
              (auth, decode)    (chain)   (attempts,    (encode)
                                           fallbacks)
```

| Package | Responsibility |
|---|---|
| `internal/translate/ir` | The canonical request/response/event types every protocol translates through; the chat and decision operation pairs |
| `internal/translate/{anthropic,openai,ollama,systemone}` | One codec pair per protocol plus one streaming state machine per direction; `sse` is the shared event-stream reader and writer |
| `internal/routing` | Chain resolution, strategies, health/cooldown, failure classification, the emulation loop, the trace |
| `internal/storage` | SQLite schema, migrations, typed accessors, the settings KV |
| `internal/provider`, `internal/tools` | The provider contract and registry, its implementations, and the web-tool backends |
| `internal/server`, `.../httpapi`, `.../webui` | Wiring, the client surfaces, the admin API and the embedded console |
| `internal/reqlog`, `internal/observability` | The in-flight/broadcast log hub and the redaction layer every log call goes through |

Where to read next: [routing and fallbacks](../routing-and-fallbacks.md) for
request behaviour, the [translation matrix](../reference/translation-matrix.md)
for field mapping, the [API reference](../api/README.md) for endpoints, and
[development](../development.md) for the build and test workflow.
