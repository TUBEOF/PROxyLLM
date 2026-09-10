# Getting Started

> Status: Phase 0 — the foundation is being built. Instructions will grow as
> phases complete.

## Run from source

Requirements: Go 1.22+, Node 20+ (for the WebUI build, Phase 2+).

```bash
go build ./cmd/proxyllm
./proxyllm
```

The server listens on `:8080` by default and exposes:

- `GET /healthz` — health check

## Configuration layers

Configuration is layered, with later layers overriding earlier ones:

1. **Built-in defaults**
2. **Config file** (`config.yaml`)
3. **Environment variables** (`PROXYLLM_*`)
4. **Database overrides** (via WebUI settings; highest precedence, exportable)

## Docker

```bash
docker compose up
```

One service; the `./data` directory is bind-mounted and holds the SQLite
database plus the envelope-encryption master key.

## Next

- [Configuration reference](configuration.md) *(in progress)*
- [Providers](providers.md) *(Phase 1)*