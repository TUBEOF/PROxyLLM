# PROxyLLM

A modular, high-performance LLM proxy written in Go — a lean, open-source
alternative to overloaded existing proxies.

> **Status: Phase 0 (foundation)** — actively being built. Not yet usable
> end-to-end. See the [roadmap](#roadmap) below.

## What it will do

PROxyLLM exposes **two client-facing API surfaces** from one service:

- **OpenAI-compatible**: `/v1/chat/completions`, `/v1/models`, `/v1/responses`
- **Anthropic-compatible**: `/v1/messages`, `/v1/messages/count_tokens`,
  `/v1/models`

…with a full **bidirectional translation engine** between the two protocol
families. Claude Code works out of the box via `ANTHROPIC_BASE_URL`; any
OpenAI-SDK client works against the OpenAI-compatible surface. Routing Claude
Code to non-Claude models is a **first-class feature** with explicit,
configurable compatibility handling.

Built-in providers:

- **ChatGPT / Codex OAuth** — full PKCE browser flow, native in the proxy
- **Ollama Cloud** — native `https://ollama.com/api/chat`
- **Firecrawl v2** — universal web-tool fallback (web-search / web-fetch
  emulation for models without native server tools)
- **Custom** — arbitrary OpenAI-compatible endpoints by configuration alone

Core properties:

- **Routing engine**: per-model and per-capability fallback chains,
  capability-based routing, health-aware failover, retry/backoff — and
  *observable, never silent* fallback transitions
- **Security by default**: WebUI login and per-client API keys default-on,
  provider credentials AES-256-GCM-encrypted at rest, client keys stored as
  Argon2id hashes only, structured logs through a tested redaction layer
- **Streaming-first**: incremental SSE relay, no full buffering, backpressure
  via bounded channels, cancellation propagated end-to-end
- **Single service**: one static CGO-free binary (embedded WebUI) + one
  SQLite data directory

## Quickstart (once Phase 0 lands)

```bash
docker compose up
# health check
curl http://localhost:8080/healthz
```

## Roadmap

| Phase | Scope |
|---|---|
| 0 | Foundation: repo skeleton, CI, layered config, SQLite storage, envelope crypto, redaction layer, health endpoint |
| 1 | Proxy core: providers, both API surfaces, translation engine with streaming, routing with fallback chains, Firecrawl tool emulation |
| 2 | WebUI: UpGuard-designed admin interface incl. "Models & Compatibility" editor |
| 3 | Hardening & release: security audit, docs, release pipeline, v1.0 |

## Documentation

- [Getting started](docs/getting-started.md)
- [Architecture decision records](docs/adr/README.md)
- [Golden fixture policy](testdata/golden/README.md)

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). All external API facts this project
relies on are captured as cited golden fixtures under `testdata/golden/` —
tests derive from specifications, never from generated code behavior.

## License

[MIT](LICENSE)