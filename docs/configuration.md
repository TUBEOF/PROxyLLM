# Configuration Reference

> Status: Phase 2 — all four layers implemented.

## Layers (later overrides earlier)

1. **Built-in defaults** — see below
2. **Config file** — JSON, `config.json` in the working directory by
   default; override the path with `PROXYLLM_CONFIG_FILE`
3. **Environment variables** — `PROXYLLM_*`
4. **Database overrides** — via WebUI settings; highest precedence;
   exportable as a config file

## Config file (layer 2)

PROxyLLM reads a JSON config file on startup. The file path resolves as:

1. `PROXYLLM_CONFIG_FILE` ENV, if set
2. `config.json` in the working directory, if it exists
3. No file — defaults then ENV apply (the normal case; nothing is
   written to disk automatically)

The file carries the non-secret settings only. Unknown keys and
trailing data are a startup failure (strict parsing — a typo in
`config.json` must never silently change behavior). Secrets (master
key, provider credentials) are never read from the file: the master
key comes from ENV or the keyfile, provider credentials live in the
encrypted store.

```json
{
  "server":  {"addr": ":8080"},
  "storage": {"path": "data/proxyllm.db", "master_key_path": "data/master.key"},
  "log":     {"level": "info", "format": "json"},
  "auth":    {"disabled": false}
}
```

## Defaults

| Setting | Default | Notes |
|---|---|---|
| `server.addr` | `:8080` | HTTP listen address |
| `storage.path` | `data/proxyllm.db` | SQLite file (bind-mounted in Docker) |
| `storage.master_key_path` | `data/master.key` | Envelope key file (0600) |
| `log.level` | `info` | `debug`, `info`, `warn`, `error` |
| `log.format` | `json` | `json` (structured) or `text` (dev) |
| `auth.disabled` | `false` | Never default-off; opt-out via ENV only |

## Environment variables

| Variable | Maps to | Notes |
|---|---|---|
| `PROXYLLM_CONFIG_FILE` | config file path | Overrides the `config.json` default |
| `PROXYLLM_SERVER_ADDR` | `server.addr` | e.g. `:9090` |
| `PROXYLLM_STORAGE_PATH` | `storage.path` | Docker default: `/data/proxyllm.db` |
| `PROXYLLM_MASTER_KEY_PATH` | `storage.master_key_path` | Docker default: `/data/master.key` |
| `PROXYLLM_MASTER_KEY` | *(direct key)* | 32-byte key, hex/base64; takes precedence over keyfile |
| `PROXYLLM_LOG_LEVEL` | `log.level` | |
| `PROXYLLM_LOG_FORMAT` | `log.format` | |
| `PROXYLLM_AUTH_DISABLED` | `auth.disabled` | `1` disables auth — loud startup warning is emitted |

> `PROXYLLM_AUTH_DISABLED=1` is the only way to turn authentication off.
> There is no config-file or WebUI path to an unauthenticated proxy.

## Database overrides (layer 4)

The settings KV (WebUI-editable) is the highest layer. Two keys are
live-routing overrides — a successful write reaches the runtime without
restart:

| Key | Values | Effect |
|---|---|---|
| `model_aliases` | JSON object alias → model-id | Routing-side alias resolution (WebUI "Models & Compatibility"): the target model's rule chain routes the alias request; an invalid blob logs and routes without aliases until it is fixed |
| `anthropic_surface_non_claude` | `allow` (default) / `warn` / `block` | Global switch on the Anthropic surface (plan §6.4): `warn` annotates route_trace with a compat_policy event; `block` refuses non-Claude model names with a 400 — Claude targets are always spared; a missing or invalid value is `allow` (never fails closed) |

Every applied policy (including `warn`) is recorded in the request's
`route_trace` — policy application is never silent.

## Master key

The AES-256-GCM envelope master key (provider credentials at rest) is
resolved in this order:

1. `PROXYLLM_MASTER_KEY` ENV (hex- or base64-encoded 32 bytes)
2. Keyfile at `storage.master_key_path` (0600 permissions, generated on
   first start if absent)

Rotation: replace the key material and run the re-encryption task
*(planned for Phase 3)*; the envelope stores a `key_id` per row for
exactly this purpose.