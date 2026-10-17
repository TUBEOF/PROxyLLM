# Admin API

The console's backend: `/api/admin/`, JSON, session-authenticated. It is
the same API the operator's browser talks to, so anything the console can
do can be scripted with a session cookie and a CSRF token.

## Authentication

The whole subtree is mounted **only when a master key is configured**.
Without one, every `/api/admin/*` request answers the uniform
`401 {"error":"unauthorized"}` — never a 404, because the difference
would reveal whether the console exists at all.

| Route | Gate |
|---|---|
| `POST /bootstrap` | none — first admin only |
| `POST /login` | none |
| everything else | session cookie + CSRF on mutations |

**Sessions.** The cookie is `id` (`__Host-id` when the request arrived over
TLS): `HttpOnly`, `SameSite=Strict`, non-persistent, and only its SHA-256
hash is stored. A session lives 30 minutes idle and 12 hours absolute; the
clock runs server-side, and every authenticated request — including an SSE
connect — refreshes the idle window. A login always mints a fresh session
ID.

**CSRF.** Mutations (`POST`, `PUT`, `PATCH`, `DELETE`) require
`X-CSRF-Token: <random>.<base64url HMAC>`, and the random half also rides a
non-`HttpOnly` `csrf` cookie the console reads. On a mutation the checks
run in this order: a *present* `Content-Type` must be `application/json`
(415 otherwise — an absent header is legal); a *present* `Origin` must match
the host exactly (403); then the token itself (403). Every authenticated
response carries a freshly issued `X-CSRF-Token` header, so a page refresh
recovers the token without a new login. GET routes need no CSRF header.

**Errors.** `{"error":"<message>"}`. A store miss is `404 not found`, a
validation refusal `400 invalid input`, anything else `500 internal error`.
Request bodies are capped at 1 MiB and decoded **strictly**: an unknown
field is a `400`, so a typo'd key can never be silently dropped. A session
failure of any kind answers the same `401 unauthorized`.

**Live apply.** No admin write needs a restart. Provider and type-settings
writes reload the provider set (and the router's aliases), rule writes
reload routing, strategy and fallback writes swap the live values, and the
model-sync configuration is re-read on every scheduler tick. Reload hooks
run only after a successful write, and a failed re-read keeps the last
known-good live state — a transient store error can never de-route a
running proxy.

## Sessions and account

| Route | Notes |
|---|---|
| `POST /bootstrap` | `{"username","password"}`; 4 KiB body cap. Creates the first admin and logs in. `409 bootstrap unavailable` when an admin already exists — the check and the insert run in one transaction, so the zero-admin window cannot be raced. |
| `POST /login` | `{"username","password"}`. Unknown username and wrong password answer the same `401` *and* cost the same time (an unknown user is verified against a never-matching hash), so the response cannot be used to enumerate admins. |
| `POST /logout` | Session + CSRF — a logout is a mutation. Deletes the session and clears both cookies. Answers `200` with an empty body. |
| `GET /me` | `{"username":"…"}` for the session's admin. A session whose admin row is gone answers `401`, never `404`. |
| `PUT /password` | `{"current_password","new_password"}`; the new password must be at least 8 characters. Every rejection path costs two hash verifications, so a `400` cannot distinguish "wrong current password" from "too short". On success it revokes **every other session of this admin**; the requesting session survives. |

## Providers

| Route | Notes |
|---|---|
| `GET /providers` | `{"providers":[{"id","type","name","config"}]}`, `[]` when empty. |
| `PUT /providers/{id}` | `{"type","name","config","api_key","usage_cookie"}`, upsert. A non-empty `api_key` is sealed into the encrypted token store as kind `access_token` — it never enters `config`. Secrets in `config` are refused by the store. `usage_cookie` follows **presence, not value**: absent keeps the stored cookie (an API-key rotation must not drop it), `""` clears it, a value sets or replaces it — and a cookie on a type that never reads one (anything but the ollama.com types) is `400`. Type and config are validated (see [validation](#provider-config-validation)). |
| `PUT /providers/{id}/enabled` | `{"enabled":true|false}` — the pool switch. A disabled account is not constructed into the live registry at all: no routing, no chain expansion of its type, no health tracking, no model sync, no web-tool emulation pool. Its rows stay, so re-enabling restores it whole. The provider set is reloaded after the write; an unknown id is `404`. |
| `DELETE /providers/{id}` | Cascades the account's tokens and model rows in one transaction, drops any in-flight OAuth flow for it, reloads providers. |
| `POST /providers/{id}/oauth/start` | Device flow for a `codex-oauth` account. `{"user_code","verification_url","interval_seconds"}`, no token material. One flow per account; a second start replaces the first; a flow expires after 15 minutes. `400` when the issuer is unknown to the catalogue (an operator action, not a retry), `502` for any other issuer failure. |
| `POST /providers/{id}/oauth/poll` | One issuer round trip per call. `{"status":"pending"}` or `{"status":"approved","account_id":"…"}` — on approval the tokens are already stored and the flow is gone (the auth code is single-use). `400` when no flow is active, `502` on a terminal failure. |

## Provider types

| Route | Notes |
|---|---|
| `GET /provider-types` | The catalogue: type, label, description, kind (`llm` / `web_tool`), auth kind, form field descriptors, and the type's default capability pins. A `web_tool` type is a web-tool backend, not a chat provider. |
| `GET /provider-types/{type}` | `{"settings": <blob or null>, "accounts":[…]}`. Each account carries `has_credential` (a presence probe — the secret itself never leaves the store), `enabled` (the pool switch) and, for the ollama.com types, `has_usage_cookie`. An unknown type is `404`, never an empty form. |
| `PUT /provider-types/{type}/settings` | `{"settings": {…}}` — the type-wide shared settings every account of that type inherits (account fields win per key). Validated like an account config, plus a `model_prefix` uniqueness check across LLM types. Reloads providers *and* settings, because type settings may carry aliases. |
| `POST /provider-types/{type}/accounts` | `{"name","api_key","config","usage_cookie"}`; the server mints the account ID. The name is required; an OAuth type refuses an API key and an API-key type requires one (an explicit empty `config` object is legal for a keyless local endpoint). `usage_cookie` is stored under its own token kind so rotating the API key never touches the scrape credential. Answers `{"account":{…}}`. |

## Models

| Route | Notes |
|---|---|
| `GET /models` | `{"models":[{"id","provider_id","name","capabilities","compat","priority","origin"}]}`. `origin` is the provenance badge: `manual` or `synced`. |
| `PUT /models/{id...}` | The wildcard captures the full multi-segment model ID. **Presence, not value, is the signal for `compat`**: an absent field keeps the stored blob, an explicit one replaces it. The write is operator authorship: the row becomes `manual`, which keeps the sync's hands off it unless its ID is an upstream model ID (then the next sync re-adopts it as `synced`). An unknown `provider_id` is a deterministic `400`, never a database error string. A `provider_id` naming a `web_tool` account is refused the same way — an emulator backend cannot serve a chat model. |
| `DELETE /models/{id...}` | `404` for an unknown ID. |
| `POST /models/test/{id...}` | One minimal round trip against the model's provider: 16-token budget, 15 s timeout. The client requests no stream; a streaming-only backend (ChatGPT/Codex) is served by an internally streamed round trip assembled into one response. `{"ok","latency_ms","reply","error","request","detail"}` — `ok:false` is still HTTP 200, because the request itself succeeded in reaching the provider. `request` is the wire body that went out, redacted and bounded, present on success too (empty when the provider renders no preview); `detail` carries the upstream's own account of a failure — `http_status` (0 when no response arrived), the decoded `type`/`code`, and the redacted `message`. Answers `404 model test is not wired` when the server runs without a master key. |

## Routing

| Route | Notes |
|---|---|
| `GET /routing` | `{"rules":[{"id","match_model","chain","priority","enabled","strategy"}]}`. An empty `strategy` inherits the global default. |
| `PUT /routing/{id}` | Chain entries must resolve to an account ID or a known provider *type*; a `web_tool` account in a chat chain is refused — it would be a guaranteed skip. On success the live router is reloaded. |
| `DELETE /routing/{id}` | |
| `GET /routing-strategy` / `PUT /routing-strategy` | The global default: `round_robin` (default) or `fill_first`. Anything else is `400`. Swaps live. |

## Capability fallbacks and model sync

| Route | Notes |
|---|---|
| `GET /capability-fallbacks` | `{"vision":[model ids],"web_search":[account ids],"web_fetch":[account ids]}` — three ordered lists, no envelope. Never `null`. |
| `PUT /capability-fallbacks` | Entries are validated: vision entries must be existing model rows, web entries existing `web_tool` accounts. The web lists set the account order of the emulation pool; the vision list names the models that describe images (see [routing and fallbacks](../routing-and-fallbacks.md)). Reloads live. |
| `GET /model-sync` | `{"enabled","interval_hours"}`, where `interval_hours` defaults to 24 and `enabled` to true. `enabled` and the interval take effect without a restart: the scheduler re-reads them per tick. |
| `PUT /model-sync` | Both fields are optional; each present field is written independently. A non-positive interval is `400` (a ticker cannot run on it). |
| `POST /model-sync/run` | Runs one synchronous pass — the console's "Sync now". Answers `{"last_run":{…}}` with per-account counts and the reason an account was skipped. |

## Client keys

| Route | Notes |
|---|---|
| `POST /keys` | `{"name"}` → `{"id","key"}`. **The only response that ever contains a client key**: the secret is generated, stored as a keyed MAC and returned once. |
| `GET /keys` | `{"keys":[{"id","name","key_prefix","revoked"}]}` — non-secret columns only. An audit record, not a key-recovery surface. |
| `POST /keys/{id}/revoke` | |

## Observability

`GET /usage?from=YYYY-MM-DD&to=YYYY-MM-DD` — daily aggregates per client
key, provider and model: requests, failures, input/output/cache-read
tokens. Both bounds are required and inclusive. No display names: the only
consumer aggregates numbers, and resolving names would cost two config
reads per poll.

`GET /overview` — the dashboard's single poll: `generated_at`, the last 14
UTC days of `usage` rows, and today's totals.

`GET /accounts/usage` — one card per non-web-tool account: the account's own
limit snapshot (`live`, present only when the last scrape produced a window —
an error-only blob answers with `error` instead, and `null` percentages render
as "—", never as zero), and a `health` string naming an active cooldown reason
(`quota_exhausted`, `breaker`) or empty when the account is healthy. The card
carries nothing the proxy measured itself (ADR-012 amendment; local usage
belongs to `/overview`).

`POST /accounts/usage/refresh/{id...}` — scrapes that account's usage now
(60 s budget). The ollama.com account types only, and only with a stored
usage cookie; answers `502 usage scrape failed: …` and persists the failure
so the card shows the honest error state instead of a stale bar.

### Logs

`GET /logs` returns a page of request rows:

| Parameter | Default | Notes |
|---|---|---|
| `limit` / `offset` | 50 / 0 | malformed values are `400` |
| `status` | 0 | 0 = no constraint |
| `since` / `until` | 0 | unix seconds, 0 = unbounded |
| `model`, `provider_id` | `""` | exact filter |
| `after` | absent | cursor mode: rows with a larger row id, newest first |

Two modes matter. Offset mode orders `ts DESC, id DESC` and returns
`total` — the count of **requests only**; sub-operations are never counted
as requests, so pagination is unaffected by them. Cursor mode (`after=`)
omits `total` entirely and is what the console's live tail uses.

A row carries the request's facts (`timestamp`, `client_key_id`,
`provider_id`, `model`, `status`, `latency_ms`, `usage`, `route_trace`), the
Logs-page additions (`conversation_id`, `protocol`, `features`,
`error_redacted`) and display projections (`key_name`, `key_prefix`,
`provider_name`, `provider_kind`) resolved at read time from the provider
catalogue — a renamed account shows its new name on the next page fetch,
and a ref that resolves to nothing projects no name while the raw value
stays authoritative.

Sub-operations ride beside the page as `children`, a map keyed by the
parent row id as a decimal string:

```json
{"logs":[…],"total":3,
 "children":{"19":[{"id":1,"parent_id":19,"ts":1760000000,"kind":"emulated_tool",
                    "to_ref":"web_fetch","detail":"fetched https://example.com via acc-ollama-web",
                    "provider_name":"Ollama Web Tools (E2E)","provider_kind":"Ollama Cloud (Web Tools)"}]}}
```

The map is omitted entirely when no row on the page has a sub-operation, so
a page without them is byte-identical to the pre-`children` shape. It is
read in one batch for the whole page. `kind` is `failover`,
`capability_fallback` or `emulated_tool`. Children inherit the request's
own facts (`protocol`, client key) rather than storing them per step. When
the children read fails, the response says `children_unavailable: true`
instead of looking like a request that routed cleanly.

`GET /log-payload?owner=request|child&id=<row id>` returns the captured
body of one request, or of one sub-operation (`owner=child` — a sub-step's
own call and result). The response is always the same shape:

```json
{"found":true,"capture_enabled":true,"client_request":"{…}","client_response":"{…}",
 "bytes":4096,"truncated":false}
```

`found:false` means nothing was stored; `capture_enabled` tells the console
*why* — capture was off when the request ran, or the row predates the
feature. Each side is capped at 64 KB (a cut is stored as a marked
envelope, never a silent truncation) and both pass the same redactor every
log site uses: an image in a captured payload is recorded by media type and
size, never by bytes.

`GET /logs/stream` is the live feed as SSE — `event: hello`, then
`event: running` (an in-flight snapshot; each snapshot replaces the
previous list, it is state and not a delta), then one `event: log` per
finished request, with a `: ping` comment every 15 s. It is a GET precisely
so a native `EventSource` works without custom headers; no CSRF header is
needed. Rows carry the same display projections as the page endpoint, so a
live row renders exactly like the same row after the next page fetch. A
slow client is dropped at the first failed flush and `EventSource`
reconnects. An open Logs tab keeps the admin session warm — a documented
side effect of the idle-window refresh, not a bug.

## Settings

| Route | Notes |
|---|---|
| `GET /settings` | `{"entries":[{"key","value"}]}`, key-ascending. |
| `PUT /settings` | `{"key","value"}` — the generic key/value store. Any key is accepted; only an empty key is `400`. |
| `DELETE /settings/{key}` | Deleting a missing key is a successful no-op. |

The keys the console and the runtime read are documented once in
[configuration](../configuration.md#database-overrides-layer-4) — that page
is their home, this one only describes the API that writes them.

## Provider config validation

Every route that writes a provider config (account upsert, type settings,
account creation) runs the same validator. It rejects unknown fields and
trailing data, requires `base_url` for `custom-openai` and `issuer` for
`codex-oauth`, refuses the other type's URL field, and validates
`model_prefix` (lowercase letters, digits and hyphens, at most 64
characters) and the alias map (both sides non-empty).

Capability pins are limited to the eight known names — `streaming`,
`tools`, `vision`, `web_search`, `web_fetch`, `code_execution`, `thinking`,
`decision` — and unknown names are refused rather than stored.

URL fields are checked for shape (absolute `http`/`https` with a host) and
then resolved: a provider endpoint that resolves to a private address is
refused unless `PROXYLLM_ALLOW_PRIVATE_PROVIDER_URLS=1` opts out. The
guard runs on the write path only — a transient DNS failure at startup
must never drop a provider that passed the guard when it was saved.
