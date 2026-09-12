# Phase 1 Best-Practice Research Pass

Per plan §11 (binding doctrine): live, cited research before every phase. All
facts below were researched live via Firecrawl on **2026-09-10** (source URL +
access date per fact). This document extends the plan's §2 research digest with
Phase 1 implementation-relevant findings and closes research gaps OP-3 and
(largely) OP-4.

---

## 1. SSE wire format — parsing rules (WHATWG HTML Standard)

Source: https://html.spec.whatwg.org/multipage/server-sent-events.html
(accessed 2026-09-10)

Binding for `internal/translate/*/sse reader`:

| # | Fact |
|---|---|
| SSE-1 | Line endings: CRLF pair, lone LF, or lone CR — all three must terminate lines. Streams are UTF-8. |
| SSE-2 | A leading U+FEFF BOM must be stripped once. |
| SSE-3 | Lines starting with `:` are comments — ignored (but relay them for keepalive semantics; clients ignore them). |
| SSE-4 | Field parsing: split at the FIRST `:`; strip ONE leading space from the value. A line without `:` is a field name with empty value. Field names compare literally (case-sensitive). |
| SSE-5 | Multi-line `data:` fields concatenate with an appended LF per field. Event dispatches on a blank line. |
| SSE-6 | At EOF, a pending (non-blank-line-terminated) event is DISCARDED, not dispatched. |
| SSE-7 | Buffering advice from the spec: line buffering on LF is safe; block buffering delays dispatch. (For our proxy: read incrementally, dispatch per event — no block buffering.) |

## 2. HTTP/2 + Flush: the SSE-comment trick

Sources: https://github.com/modelcontextprotocol/go-sdk/issues/937 and
https://github.com/modelcontextprotocol/go-sdk/issues/938 (accessed 2026-09-10);
references therein: golang/go#31125, caddyserver/caddy#4247

| # | Fact |
|---|---|
| H2-1 | Behind HTTP/2-aware reverse proxies (Envoy, Caddy, Go `httputil.ReverseProxy` in H2 mode), `Flush()` alone does NOT forward response headers: proxies hold the HTTP/2 HEADERS frame until a DATA frame arrives. |
| H2-2 | Established mitigation: after `WriteHeader`, write a single SSE comment (`": ok\n\n"`) before flushing. Comments are spec-ignored by clients (SSE-3) but produce a DATA frame that releases the HEADERS frame. TTFB behind HTTP/2 proxies drops from request-timeout to ~1 ms. |
| H2-3 | Binding for PROxyLLM: our SSE writer emits `": ok\n\n"` immediately after `WriteHeader(200)` — cheap, spec-conform, removes a whole failure class for operators fronting the proxy with HTTP/2 proxies. |

## 3. Failover doctrine — early-stream vs. mid-stream

Sources: https://api.avemujica.moe/blog/llm-api-failover-strategy (accessed
2026-09-10); corroborated by gomodel resilience docs
(https://github.com/enterpilot/gomodel/blob/1ff820b016f98fa846b3d323b504c1c8a7030511/docs/advanced/resilience.mdx)
and ex_llm circuit-breaker migration guide
(https://github.com/azmaveth/ex_llm/blob/3cc5063d58e705abc988eabfdd4b31b7c2cd7979/docs/migration_guides/circuit_breaker_migration.md),
both accessed 2026-09-10.

Confirms and sharpens plan Analysis E:

| # | Fact → PROxyLLM consequence |
|---|---|
| FO-1 | Early-stream failures (before first content byte: 408, 429, 5xx, TLS/connection reset before headers) are safe to retry/fail over. Mid-stream failures (after content reached the client) must NOT be retried or provider-swapped: stop cleanly, log partial token count, surface a protocol-conform error. → matches our "no mid-stream swap" rule; add partial-token accounting to the terminal error's request log. |
| FO-2 | Status codes alone are insufficient — parse the structured error body (`error.code`): `rate_limit_exceeded` → backoff+failover; `server_error` → failover fast; `content_filter`/`content_policy_violation` → do NOT fail over (compliance risk — the same prompt triggers the same filter elsewhere); `insufficient_quota`/billing errors → fail over immediately; 400/401/403/404/422 → never retry (fix request / rotate credentials / update mapping). → routing error-classification table gains a body-error-code tier above the HTTP-status tier. |
| FO-3 | Retry budgets: max 1–2 retries per provider, max 2–3 providers total, backoff ceiling 2–4 s for interactive surfaces, total timeout including time-to-first-token. → encode as routing config defaults, observable in route_trace. |
| FO-4 | Every failover event must leave an audit trail: original + fallback provider, status + body error code, early- vs mid-stream, tokens consumed so far, per-attempt and total latency. → route_trace schema (plan §4.3) gains `error_code` and `failure_position` (early/mid-stream) fields. |
| FO-5 | Streaming requests are never retried once dispatched ("partial data may already have been sent") — gomodel docs state this as their hard rule; identical to ours. |

## 4. OAuth 2.0 — RFC 9700 (Security BCP)

Source: https://oauth.net/2/oauth-best-practice/ → RFC 9700
(https://www.rfc-editor.org/rfc/rfc9700) (accessed 2026-09-10)

| # | Fact → consequence for the Codex provider |
|---|---|
| OA-1 | Always use PKCE (mandated for all authorization code flows). Exact redirect-URI matching. Never Implicit or Password grants. → our PKCE S256 + exact `http://localhost:{port}/auth/callback` registration per X1/X3 is conformant. |
| OA-2 | Refresh tokens should live server-side only; long-lived tokens increase theft risk → tokens only in the encrypted store (ADR-007 control 7), refresh server-side only (plan §6.3). |

## 5. OP-3 CLOSED: Codex refresh-grant body + rotation semantics

Primary source: openai/codex CLI source,
`codex-rs/login/src/auth/manager.rs`
(https://raw.githubusercontent.com/openai/codex/main/codex-rs/login/src/auth/manager.rs,
accessed 2026-09-10); rotation/single-use behavior corroborated by
zeroclaw-labs/zeroclaw issue #9492
(https://github.com/zeroclaw-labs/zeroclaw/issues/9492, accessed 2026-09-10).

| # | Fact |
|---|---|
| X9 | Refresh request: `POST {refresh_token_endpoint}` (default `https://auth.openai.com/oauth/token`), **`Content-Type: application/json`**, JSON body `{"client_id": <oauth_client_id>, "grant_type": "refresh_token", "refresh_token": <token>}` — NOT form-encoded (third-party sources claiming form-encoding are wrong on this endpoint). |
| X10 | Refresh response fields: `id_token?`, `access_token?`, `refresh_token?` — all optional in the response shape. |
| X11 | **Refresh tokens are single-use and rotate**: each successful refresh returns a NEW refresh token and invalidates the old one. Documented failure codes: `refresh_token_expired` (expired), `refresh_token_reused` (consumed elsewhere → exhausted), `refresh_token_invalidated` (revoked). |
| X12 | Consequence (binding): the PROxyLLM Codex provider must (a) persist the rotated refresh token ATOMICALLY with the access token (one DB transaction, encrypted columns), (b) single-flight refresh (one in-flight refresh per account; concurrent requests wait), (c) on `refresh_token_reused` surface an operator action ("re-login required"), never silently loop, (d) an operator must not share one OAuth identity between the Codex CLI and PROxyLLM — the CLI's own refresh invalidates our token. Document this in the provider guide. |

### 5b. Codex wire details re-verified live at implementation time (2026-09-11)

Primary sources: openai/codex CLI source at `main` —
`codex-rs/login/src/auth/default_client.rs`
(https://raw.githubusercontent.com/openai/codex/main/codex-rs/login/src/auth/default_client.rs,
accessed 2026-09-11), `codex-rs/login/src/auth/manager.rs`
(https://raw.githubusercontent.com/openai/codex/main/codex-rs/login/src/auth/manager.rs,
accessed 2026-09-11), `codex-rs/login/src/server.rs`
(https://raw.githubusercontent.com/openai/codex/main/codex-rs/login/src/server.rs,
accessed 2026-09-11), `codex-rs/core/src/client.rs`
(https://raw.githubusercontent.com/openai/codex/main/codex-rs/core/src/client.rs,
accessed 2026-09-11).

| # | Fact |
|---|---|
| X13 | OAuth client ID: `CLIENT_ID = "app_EMoamEEZ73f0CkXaXp7hrann"` (codex-rs/login/src/auth/manager.rs); overridable via `CODEX_APP_SERVER_LOGIN_CLIENT_ID`. PROxyLLM pins it as the default and allows operator override in config. |
| X14 | Default headers on every backend request (`default_client.rs::default_headers`): `originator: codex_cli_rs` (the default originator value) and a `User-Agent` of the shape `{originator}/{version} ({os_type} {os_version}; {arch}) {terminal}`. The originator value also rides the authorize URL as the `originator` query param. |
| X15 | ChatGPT-auth requests attach `chatgpt-account-id` (lowercase header name, from the `chatgpt_account_id` JWT claim of the id_token) alongside `Authorization: Bearer <access_token>` (codex-rs core client auth path; corroborated by third-party ports: earendil-works/pi#1828, jcode OAUTH.md, both accessed 2026-09-11). |
| X16 | Token exchange (authorization_code grant): `POST {issuer}/oauth/token`, `Content-Type: application/x-www-form-urlencoded`, form body `grant_type=authorization_code&code={code}&redirect_uri={redirect_uri}&client_id={client_id}&code_verifier={verifier}` (server.rs) — form-encoded for the exchange, JSON for refresh (X9). |
| X17 | Interactive login: authorize URL params exactly `response_type=code`, `client_id`, `redirect_uri=http://localhost:{port}/auth/callback`, `scope=openid profile email offline_access api.connectors.read api.connectors.invoke`, `code_challenge` (S256), `code_challenge_method=S256` (confirmed explicitly sent as its own param, server.rs `build_authorize_url`, accessed 2026-09-12), `id_token_add_organizations=true`, `codex_cli_simplified_flow=true`, `state`, `originator` (ports 1455 default, 1457 fallback; the listener binds 127.0.0.1 only). |
| X18 | Refresh cadence in the CLI: refresh interval 8 (minutes, REFRESH_INTERVAL constant) with an access-token refresh window of 5 minutes; refresh failure codes map to operator-facing messages ("refresh token has expired", reused → re-login). PROxyLLM refreshes lazily (on demand) plus proactively within the window, with single-flight + atomic persistence per X12. |
| X19 | Codex backend endpoint (third-party corroborated, codex-lb and pi repos): `https://chatgpt.com/backend-api/codex/responses`, speaking the Responses-API request/response shape. The exact first-party request body remains unpublished (X7 residue) — PROxyLLM sends the Responses-API wire shape via the OpenAI Responses codec and pins request/response details via the ENV-gated live smoke (OP-3 plan). |
| X20 | Device flow, exact wire shape (`codex-rs/login/src/device_code_auth.rs`, accessed 2026-09-12): request a user code via `POST {issuer}/api/accounts/deviceauth/usercode` with JSON body `{"client_id": <id>}` → response `{"device_auth_id": …, "user_code": …, "interval": <string>}` (interval arrives as a STRING parsed to integer; serde alias `usercode`). Verification URL: `{issuer}/codex/device`. Poll `POST {issuer}/api/accounts/deviceauth/token` with JSON body `{"device_auth_id": …, "user_code": …}` → on 2xx returns `{"authorization_code": …, "code_challenge": …, "code_verifier": …}` — **the PKCE codes are SERVER-provided in the device flow** (the client generates none); exchange then uses `redirect_uri = {issuer}/deviceauth/callback`. On 403/404 keep polling (sleep `min(interval, remaining)`); hard 15-minute overall timeout; any other non-2xx is a fatal error. A 404 on the initial usercode request means device login is disabled for the client → PROxyLLM surfaces that as an operator-facing error rather than retrying. |
| X21 | PKCE, exact algorithm confirmed (`codex-rs/login/src/pkce.rs`, accessed 2026-09-12): verifier = 64 random bytes, base64url-encoded WITHOUT padding (86 chars, within the RFC 7636 43–128 range); challenge = base64url-no-pad of SHA-256 of the verifier STRING bytes (`S256`). Refines X2 with the unpadded encoding detail. |
| X22 | The codex-rs workspace `Cargo.toml` pins `version = "0.0.0"` (build-time placeholder injected at release; accessed 2026-09-12) — there is no stable CLI version string to mirror. Consequence (binding): PROxyLLM pins its OWN version constant for the X14 `User-Agent` (e.g. `codex_cli_rs/proxyllm/<semver>` shape-compatible with `{originator}/{version} ({os_type} {os_version}; {arch}) {terminal}`) rather than tracking CLI releases. |
| X23 | Backend request shape re-verified against the current codex-api crate (`codex-rs/codex-api/src/endpoint/responses.rs` + `provider.rs` + `core/src/config/mod.rs`, accessed 2026-09-12): `ResponsesEndpoint::Responses.path() = "/responses"` is PROVIDER-RELATIVE; the chatgpt provider's base URL defaults to `https://chatgpt.com/backend-api/` (config `chatgpt_base_url`), yielding the X19 endpoint `{base}codex/responses`. Streaming requests send `Accept: text/event-stream`. Optional headers exist for features PROxyLLM does not replicate (`session-id`, `thread-id`, `x-client-request-id`, `x-codex-turn-state`, `x-codex-beta-features`, `x-openai-subagent`, `x-codex-routing-hint`, attestation) — none is required for a plain request; PROxyLLM sends none of them and pins the minimal set via the OP-3 smoke. |
| X24 | **Correction to plan §2.4 X7**: the `OAI-Product-Sku` header is NO LONGER part of ChatGPT-backend Responses requests in current codex-rs (grep of `core/src/client.rs` and `codex-api` crate: zero hits; the config field `apps_mcp_product_sku` forwards only to the host-owned apps MCP server). PROxyLLM therefore does NOT send `OAI-Product-Sku`; the auth-relevant headers remain X14 (originator, User-Agent) + X15 (`chatgpt-account-id`, Bearer). |

## 6. OP-4 LARGELY CLOSED: Ollama native /api/chat now fully documented

Source: https://docs.ollama.com/api/chat (accessed 2026-09-10)

| # | Fact |
|---|---|
| L7 | Response fields (non-streaming AND final stream chunk): `model`, `created_at`, `message{role, content, thinking, tool_calls[{function{name, description?, arguments}}], images}`, `done`, `done_reason`, `total_duration`, `load_duration`, `prompt_eval_count`, `prompt_eval_cached_count`, `prompt_eval_duration`, `eval_count`, `eval_duration`, optional `logprobs`. |
| L8 | **`stream` defaults to `true`** — a non-streaming request MUST send `"stream": false` explicitly. |
| L9 | `think` request param: boolean or `"high"/"medium"/"low"/"max"` → maps cleanly onto the compat setting `thinking_handling: map-to-reasoning-effort` (§6.4): Anthropic `budget_tokens` → Ollama `think` level. `message.thinking` carries separate thinking output. |
| L10 | Streaming: each chunk is a standalone JSON object (newline-delimited, NOT SSE) with partial `message.content`; the final chunk carries `done: true`, `done_reason`, and full usage counts. Contract tests pin exact chunk shapes; remaining residual risk (exact `done_reason` value strings) still warrants the local-instance smoke from the original OP-4 plan. |
| L11 | `prompt_eval_cached_count` maps to IR usage cache-read tokens; `prompt_eval_count`/`eval_count` to input/output tokens. |

## 7. Mock-provider harness pattern (corroborates Analysis F)

Sources: axe mock-provider integration spec
(https://github.com/jrswab/axe/blob/f1994042fd56e1d0f26e2e8609e082ed08b913e8/docs/plans/009_mock_provider_integration_spec.md
and .../ISS-59_m4_anthropic_streaming_implement.md, accessed 2026-09-10);
RAGflow httptest driver tests
(https://github.com/infiniflow/ragflow/issues/15072, accessed 2026-09-10).

| # | Fact |
|---|---|
| MH-1 | Reusable mock server in `internal/testutil` configurable via a scripted response queue; each request pops the next canned response (multi-turn without closures/counters). Real HTTP via `httptest`; tests exercise real code paths — the mock is the only fake. Matches our planned `internal/testutil/mockprovider` exactly. |
| MH-2 | Anthropic SSE test matrix worth adopting verbatim: unmarshal each of the 10 event types; mid-stream error mapping table; request-format capture assertions (`stream:true`, model, max_tokens, system, tools); pre-stream HTTP-error classes; text-delta sequences (start → block_start → 2×delta → block_stop → message_delta → message_stop); edge cases: empty `partial_json`, malformed JSON in `data:`. |

## 8. Re-validation of §2 facts

All plan §2 research facts were captured 2026-09-10 (today) — the ~90-day
re-validation rule does not apply yet. The two open research gaps OP-3 and OP-4
are addressed above; OP-1 (server-tool string maintenance process) remains a
Phase 1 implementation-time decision as planned.

## 9. Binding implementation consequences (checklist for Phase 1)

1. SSE reader: WHATWG parsing rules SSE-1…SSE-7 (CR/LF/CRLF, BOM strip, comment
   lines, first-colon split, one-space strip, blank-line dispatch, EOF discard).
2. SSE writer: emit `": ok\n\n"` after `WriteHeader` before first event (H2-2);
   flush per event; relay upstream comments/pings.
3. Routing error classification: two tiers — HTTP status (FO-1) then body
   `error.code` (FO-2); `content_filter` never fails over; retry budgets FO-3;
   route_trace records `error_code` + `failure_position` (FO-4).
4. Codex provider: JSON refresh grant (X9), atomic rotation persistence +
   single-flight + `refresh_token_reused` operator message (X11/X12).
5. Ollama provider: explicit `stream:false` for non-streaming (L8); `think`
   mapping for `map-to-reasoning-effort` compat (L9); usage mapping incl.
   `prompt_eval_cached_count` (L11); NDJSON chunk parsing (L10).
6. Mock provider: scripted queue harness (MH-1) + the MH-2 Anthropic test matrix.