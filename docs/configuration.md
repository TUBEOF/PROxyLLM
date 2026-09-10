# Configuration Reference

> Status: Phase 0 — defaults + ENV layer. File and DB layers follow.

## Layers (later overrides earlier)

1. **Built-in defaults** — see below
2. **Config file** *(Phase 1)*
3. **Environment variables** — `PROXYLLM_*`
4. **Database overrides** — via WebUI settings; highest precedence;
   exportable as a config file *(Phase 2)*

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
| `PROXYLLM_SERVER_ADDR` | `server.addr` | e.g. `:9090` |
| `PROXYLLM_STORAGE_PATH` | `storage.path` | Docker default: `/data/proxyllm.db` |
| `PROXYLLM_MASTER_KEY_PATH` | `storage.master_key_path` | Docker default: `/data/master.key` |
| `PROXYLLM_MASTER_KEY` | *(direct key)* | 32-byte key, hex/base64; takes precedence over keyfile |
| `PROXYLLM_LOG_LEVEL` | `log.level` | |
| `PROXYLLM_LOG_FORMAT` | `log.format` | |
| `PROXYLLM_AUTH_DISABLED` | `auth.disabled` | `1` disables auth — loud startup warning is emitted |

> `PROXYLLM_AUTH_DISABLED=1` is the only way to turn authentication off.
> There is no config-file or WebUI path to an unauthenticated proxy.

## Master key

The AES-256-GCM envelope master key (provider credentials at rest) is
resolved in this order:

1. `PROXYLLM_MASTER_KEY` ENV (hex- or base64-encoded 32 bytes)
2. Keyfile at `storage.master_key_path` (0600 permissions, generated on
   first start if absent)

Rotation: replace the key material and run the re-encryption task
*(Phase 1)*; the envelope stores a `key_id` per row for exactly this
purpose.