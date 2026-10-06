# Architecture Decision Records

One ADR per significant decision, mirroring the analyses (A–G) of the
project plan. Format: context → criteria → options considered → decision →
consequences.

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

ADR template: [TEMPLATE.md](TEMPLATE.md)