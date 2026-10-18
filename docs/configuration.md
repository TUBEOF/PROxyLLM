# Configuration

PROxyLLM reads its settings in four layers. A later layer wins over an
earlier one, and the layers have different scopes: the first three describe
the process (listener, storage, logging, authentication, web-tool
endpoint), the fourth holds the operator settings the running server reads
while it serves.

| # | Layer | Source | What it can set |
|---|---|---|---|
| 1 | Built-in defaults | compiled in, see [Defaults](#defaults) | every process setting |
| 2 | Config file | `PROXYLLM_CONFIG_FILE`, else `config.json` in the working directory | the non-secret process settings |
| 3 | Environment | `PROXYLLM_*` variables | the same process settings, plus the secrets and the SSRF-guard opt-out |
| 4 | Database overrides | the settings key/value store, written by the console or the [admin API](api/admin.md#settings) | the runtime settings in [Database overrides](#database-overrides-layer-4) |

The database layer does not restate the process settings: no key there
changes `server.addr`, `storage.path` or the log settings, which resolve at
startup from layers 1–3.

## Config file

The file is read once, at startup. Its path resolves as:

1. `PROXYLLM_CONFIG_FILE`, when set to a non-empty value;
2. `config.json` in the working directory, otherwise.

A missing file is not an error — including when `PROXYLLM_CONFIG_FILE`
named it — so a deployment without a config file is the normal case, and
the process never writes one. A file that exists but cannot be read or
parsed fails startup.

The file carries the non-secret process settings and nothing else:

```json
{
  "server":    { "addr": ":8080" },
  "storage":   { "path": "data/proxyllm.db", "master_key_path": "data/master.key" },
  "log":       { "level": "info", "format": "json" },
  "firecrawl": { "base_url": "https://api.firecrawl.dev" }
}
```

The values above are the effective defaults; every key is optional, and an
absent key leaves the layer below it in place. Parsing is **strict**: an
unknown key or trailing data after the object is a startup failure that
names the offending field. A typo in `config.json` must fail loudly, never
silently reroute traffic under a default.

- `server.addr` — the HTTP listen address.
- `storage.path` — the SQLite database file.
- `storage.master_key_path` — the envelope key file (see [Master key](#master-key)).
- `log.level`, `log.format` — the logger, see the [defaults](#defaults) for accepted values.
- Authentication is **not** configurable here. An `auth` group in the file
  is a startup failure naming `PROXYLLM_AUTH_DISABLED`, the environment
  variable that can turn authentication off — a security switch belongs to
  the deployment environment, not to a file that can be edited and copied
  around.
- `firecrawl.base_url` — the Firecrawl API host used by the web-tool
  emulator; empty selects the client's default, `https://api.firecrawl.dev`.

Secrets never come from the file. The master key and the Firecrawl API key
are held outside the configuration struct on purpose, so no config dump can
leak them; both resolve from the environment or a keyfile, and provider
credentials live encrypted in the database.

In the container the working directory is `/` and `/data` is the bind
mount, so the relative defaults resolve to `/data/proxyllm.db` and
`/data/master.key`.

## Defaults

| Setting | Default | Notes |
|---|---|---|
| `server.addr` | `:8080` | HTTP listen address |
| `storage.path` | `data/proxyllm.db` | SQLite file; its directory is created on first start |
| `storage.master_key_path` | `data/master.key` | envelope key file, mode 0600 |
| `log.level` | `info` | `debug`, `info`, `warn`, `error`; any other value fails startup |
| `log.format` | `json` | `text` selects the human-readable handler; anything else is `json` |
| `firecrawl.base_url` | *(empty)* | the Firecrawl client then uses `https://api.firecrawl.dev` |

## Environment variables

| Variable | Sets | Notes |
|---|---|---|
| `PROXYLLM_CONFIG_FILE` | config file path | an empty value falls back to `config.json` |
| `PROXYLLM_SERVER_ADDR` | `server.addr` | e.g. `:9090`; also the fallback probe target of `proxyllm -healthcheck` |
| `PROXYLLM_STORAGE_PATH` | `storage.path` | |
| `PROXYLLM_MASTER_KEY_PATH` | `storage.master_key_path` | |
| `PROXYLLM_LOG_LEVEL` | `log.level` | |
| `PROXYLLM_LOG_FORMAT` | `log.format` | |
| `PROXYLLM_AUTH_DISABLED` | *(no file key)* | boolean: `1`/`true`/`yes`/`on` and `0`/`false`/`no`/`off`/empty; any other value fails startup. `1` disables client-key auth and emits a loud startup warning |
| `PROXYLLM_FIRECRAWL_BASE_URL` | `firecrawl.base_url` | |
| `PROXYLLM_MASTER_KEY` | *(secret)* | 32 bytes as 64 hex characters or 44 base64 characters; takes precedence over the keyfile |
| `PROXYLLM_FIRECRAWL_API_KEY` | *(secret)* | activates the environment-wired web-tool emulator |
| `PROXYLLM_ALLOW_PRIVATE_PROVIDER_URLS` | *(guard opt-out)* | exactly `1` allows provider endpoints that resolve to private addresses, on both the config write and the redirect guard |
| `PROXYLLM_HEALTHCHECK_ADDR` | *(probe target)* | read only by `proxyllm -healthcheck`; falls back to `PROXYLLM_SERVER_ADDR`, then `localhost:8080` |

`PROXYLLM_AUTH_DISABLED=1` is for local experiments. There is no console
path to an unauthenticated proxy, and the proxy should never be reachable
without authentication on a network you do not control.

Test-time variables (`PROXYLLM_SMOKE*`, `PROXYLLM_PERF_BUDGETS`) are not
runtime configuration; they are documented in
[development](development.md#smoke-tests).

## Database overrides (layer 4)

The [admin API](api/admin.md#settings) writes into a generic key/value store
in the database. That store is the highest layer, and it is read by the
running process — not only at startup. A few keys have their own control in
the console (the capture switch on Logs, the alias map on Routing, the sync
cadence on Models); the rest are set through the API.

- Settings the request path consults, such as `anthropic_surface_non_claude`
  and `request_logging_enabled`, are read per request.
- The model-sync configuration is re-read on every scheduler tick.
- Aliases, caption settings, the global routing strategy and the fallback
  lists are pushed into the live runtime when they are saved, and a provider
  write reloads the provider set. Each of those routes is listed in the
  [admin API](api/admin.md).

A reload runs only after a successful store write, and a failed re-read
keeps the last known-good live value, so a transient store error cannot
de-route a running proxy. A value the runtime cannot parse degrades to the
documented default for that key — it never fails closed and never fails
startup. Deleting a key returns the setting to its lower layer, which for
these keys means the default.

| Key | Value | Meaning documented in |
|---|---|---|
| `model_aliases` | JSON object, alias → model ID | [Routing and fallbacks](routing-and-fallbacks.md#model-identity) |
| `client_web_tools` | JSON object, client tool name → web-tool family (default: Claude Code's `WebFetch`/`WebSearch`; `{}` turns the claim off) | [Claude Code](claude-code.md#web-tools-webfetch-and-websearch) |
| `routing_strategy` | `round_robin` (default) / `fill_first` | [Routing and fallbacks](routing-and-fallbacks.md#rule-model-evaluation-order) |
| `vision_fallback` | JSON array of model IDs | [Routing and fallbacks](routing-and-fallbacks.md#capability-fallback-lists) |
| `vision_caption_prompt` | string | [Routing and fallbacks](routing-and-fallbacks.md#the-vision-rescue) |
| `vision_caption_task_aware` | `0` disables it (default: on) | — the reactive vision rescue only; the pre-dispatch bootstrap describes each image once, question-targeted answers are the `analyze_image` tool's [Routing and fallbacks](routing-and-fallbacks.md#the-vision-rescue) |
| `vision_caption_timeout_ms` | integer > 0 (default `30000`) | [Routing and fallbacks](routing-and-fallbacks.md#the-vision-rescue) |
| `upstream_response_header_timeout_ms` | integer > 0 (default `30000`) | [Routing and fallbacks](routing-and-fallbacks.md#transport-limits) |
| `vision_tool` | `0` disables it (default: on) | [Routing and fallbacks](routing-and-fallbacks.md#the-vision-rescue) |
| `vision_placeholder_template` | string (`{n}` `{ref}` `{caption}` placeholders) | [Routing and fallbacks](routing-and-fallbacks.md#the-vision-rescue) |
| `web_search_fallback` | JSON array of web-tool account IDs | [Routing and fallbacks](routing-and-fallbacks.md#capability-fallback-lists) |
| `web_fetch_fallback` | JSON array of web-tool account IDs | [Routing and fallbacks](routing-and-fallbacks.md#capability-fallback-lists) |
| `anthropic_surface_non_claude` | `allow` (default) / `warn` / `block` | [Routing and fallbacks](routing-and-fallbacks.md#non-claude-routing--compatibility) |
| `model_sync_enabled` | `1` (default) / `0` | [Routing and fallbacks](routing-and-fallbacks.md#model-sync) |
| `model_sync_interval_hours` | integer ≥ 1 (default `24`) | [Routing and fallbacks](routing-and-fallbacks.md#model-sync) |
| `model_sync_state` | JSON, written by the sync (read-only) | [Operating: Models](operating.md#models) |
| `provider_type_settings` | JSON object keyed by provider type | [Operating: providers and accounts](operating.md#providers-and-accounts) |
| `request_logging_enabled` | `1` (default) / `0` | [Operating: logs](operating.md#logs) |
| `request_payload_retention_days` | integer ≥ 1 (default `7`) | [Operating: logs](operating.md#logs) |
| `account_usage:<provider-id>` | JSON, written by the usage prober (read-only) | [Operating: quota](operating.md#quota) |

The per-model compatibility settings (`thinking_handling`,
`server_tool_emulation`, `max_tokens_clamp`, `context_window`,
`cache_control`) are stored on the model row, not in this store — see
[model compatibility](reference/model-compat.md#the-settings).

## Master key

The AES-256-GCM envelope master key seals provider credentials at rest, and
is resolved in this order:

1. `PROXYLLM_MASTER_KEY`, decoded as 64 hex characters or 44 base64
   characters (exactly 32 bytes either way);
2. the keyfile at `storage.master_key_path` — created with mode 0600 on
   first start if it does not exist, containing the key as hex.

The same bytes have two further uses, so they are part of the backup, not
an implementation detail: they sign the console's session and CSRF tokens,
and they derive the MAC under which client keys are stored. Rotating the
master key therefore invalidates every client key, and the keys are
display-once, so they cannot be re-hashed — they must be reissued.

The envelope format reserves the version and key-id fields a rotation would
need, but the re-encryption task is not implemented in this release: treat
the master key as immutable and back it up together with the database. One
without the other is not a restore.

## Not on this page

- Endpoints and their request/response shapes: [API reference](api/README.md).
- Console pages and day-2 operations: [Operating PROxyLLM](operating.md).
- Build, test and release mechanics: [Development](development.md).
- Deployment, TLS and backup procedure: [Deployment](deployment.md).
