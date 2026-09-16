# Getting Started

PROxyLLM is a modular, high-performance LLM proxy: one binary exposes an
OpenAI-compatible surface and an Anthropic-compatible surface, with full
bidirectional translation, routing with fallback chains, and an embedded
admin WebUI.

## Quickstart (Docker)

```bash
git clone https://github.com/TUBEOF/PROxyLLM.git
cd PROxyLLM
docker compose -f docker/compose.yaml up -d
```

Open `http://localhost:8080` — the WebUI's first-run bootstrap creates the
admin account. The `./data` directory (bind-mounted) holds the SQLite
database and the generated master key (0600).

The container runs hardened: read-only rootfs, dropped capabilities,
non-root, pids/memory limits (see [docker/compose.yaml](../docker/compose.yaml)).

## Run from source

Requirements: Go 1.26+, Node 22+ (WebUI build).

```bash
cd webui && npm ci && npm run build && cd ..
go build -o proxyllm ./cmd/proxyllm
./proxyllm
```

The server listens on `:8080` (`GET /healthz` is the unauthenticated
health probe).

## First API key

In the WebUI: **Keys** → create — the `pk_...` secret is shown exactly
once. Use it as:

- OpenAI-SDK clients: `base_url = http://localhost:8080/v1`,
  `Authorization: Bearer pk_...`
- Claude Code:
  ```bash
  export ANTHROPIC_BASE_URL="http://localhost:8080"
  export ANTHROPIC_AUTH_TOKEN="pk_..."
  ```

## Providers

Add providers in the WebUI (**Providers**): ChatGPT/Codex (OAuth flow in
the UI), Ollama Cloud (API key), or any custom OpenAI-compatible endpoint.
Credentials are AES-256-GCM encrypted at rest. The web-tool emulation
(web_search/web_fetch via Firecrawl) activates when
`PROXYLLM_FIRECRAWL_API_KEY` is set — without it, emulated tool requests
receive the documented "unavailable" error results (loud, never silent).

## Next

- [Configuration reference](configuration.md) — layers, ENV, DB overrides
- [API reference](api-reference.md) — both surfaces + admin API
- [Claude Code with non-Claude models](claude-code-with-non-claude-models.md)
- [Routing & fallbacks](routing-and-fallbacks.md)
- [Security guide](security.md)
- [Deployment](deployment.md)

## Production notes

- Set `PROXYLLM_MASTER_KEY` (32 bytes, hex/base64) or let the keyfile be
  generated — keep `/data` backed up (SQLite file + keyfile = full state).
- Without a Firecrawl key, web-tool emulation stays inactive (logged at
  startup).
- `PROXYLLM_AUTH_DISABLED=1` is the only way to disable auth and emits a
  loud startup warning — never do this on a network you do not control.