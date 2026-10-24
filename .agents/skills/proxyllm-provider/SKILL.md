---
name: proxyllm-provider
description: Use when adding or changing an upstream provider type in PROxyLLM — provider registration, account auth (API key or OAuth), model sync, capability flags, usage/quota probing, or the provider type catalog the console renders.
---

# Adding or changing a PROxyLLM provider

A provider is a type string plus accounts of that type (ADR-009). Adding a
type touches a fixed set of places; missing one leaves a type that half-works
— it appears in the console but never routes, or routes but never syncs.

## The checklist (verify every item, in this order)

1. **Type + config parsing** — `internal/server/webui/providervalid.go`:
   the type constant, a `case` in `parseProviderConfig` (without one the
   default branch rejects the type), AND the two error strings there that
   enumerate every type by name — they list the new type too or the refusal
   message lies.
2. **Console catalog** — `internal/server/webui/providertypes.go`:
   type, label, description, `Kind` (`llm` or `web_tool`), `Auth`
   (`api_key`/`oauth`), settings fields, account fields, default
   capabilities. This drives the Providers page forms.
3. **The implementation** — under `internal/provider/<name>`: `ID`,
   `Capabilities`, `Complete`, `Stream`, `RefreshCredentials` (no-op for
   static keys). `TypedProvider.Type()` MUST return the new type string —
   chain expansion and the model-sync prefix key off it, and the OpenAI-
   compatible `custom` client hardcodes `"custom-openai"`, so an api-key type
   is either a thin wrapper package or a type-parameterised constructor.
   `CredentialedProvider` is optional (ADR-009 decision 4): a type without
   it lets keyless accounts join chain expansion.
4. **Registration** — `internal/server/providers.go` `buildProvider` case
   plus an explicit `defaultCaps` case (the default branch is documented as
   custom-openai's baseline). A type not built there is dead config.
5. **Model sync** — `internal/modelsync` + the models table: without it the
   type has no model rows and never matches a chain entry
   (ADR-010; capability fallbacks learn per model there).
6. **Usage/quota (optional)** — a probe in `internal/server/usageprobe.go`
   plus `UsageProbeType` in `internal/server/webui/adminapi_usage.go` give the
   type a Quota card. No probe = no card, and an account's usage fields are
   stored but never displayed.
7. **Secrets** — keys/tokens only through the encrypted token store; the
   master key never appears in a config blob, and no secret reaches a log
   (`internal/observability/redact`).
8. **Tests** — provider round trips against a mock upstream; the catalog test
   `adminapi_providertypes_test.go` hard-asserts the TYPE COUNT — bump it and
   cover the new card. Derive expectations from the vendor's current API docs
   and pin wire facts in fixtures, not constants.
9. **Docs** — `docs/api/admin.md` enumerates the per-type parse rules; the
   new type's requirements land there in the same change.
10. **E2E** — the Providers-page screenshot includes the type cards: a new
    entry shifts the visual baseline (`webui/e2e/routes.spec.ts`); refresh
    it through the `update-visual-baselines` workflow, never by hand.

## Non-negotiables

- Adding a provider type must never require changes to routing or the API
  surfaces (the registry contract in `iface.go` exists for that).
- Vendor capability strings and error codes live in configuration.
- Web-tool backends (`Kind: web_tool`) own no models — the console refuses a
  model row on one for the same reason.

## Verify

```bash
go test ./internal/provider/... ./internal/modelsync/... ./internal/server/...
make vet lint test
```