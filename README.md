# PROxyLLM

A modular, high-performance LLM proxy written in Go — a lean, open-source
alternative to overloaded existing proxies.

[![CI](https://github.com/proxyllm/proxyllm/actions/workflows/ci.yml/badge.svg)](https://github.com/proxyllm/proxyllm/actions/workflows/ci.yml)
[![Release](https://github.com/proxyllm/proxyllm/actions/workflows/release.yml/badge.svg)](https://github.com/proxyllm/proxyllm/actions/workflows/release.yml)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)
[![Go version](https://img.shields.io/badge/go-1.26-blue.svg)](go.mod)

## What it does

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
- **Ollama Cloud** — native `https://ollama.com/api/chat` (note the spelling:
  `ollamacloud`)
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

## Quickstart

```bash
git clone https://github.com/proxyllm/proxyllm.git
cd proxyllm
docker compose -f docker/compose.yaml up -d
curl http://localhost:8080/healthz
```

Open `http://localhost:8080` for the admin WebUI (first-run bootstrap).
Create a client key (**Keys**), then point your client at the proxy:

```bash
# Claude Code
export ANTHROPIC_BASE_URL="http://localhost:8080"
export ANTHROPIC_AUTH_TOKEN="pk_..."

# OpenAI-SDK clients
# base_url = http://localhost:8080/v1
# api_key  = pk_...
```

Prebuilt multi-arch images are published per release tag
(`ghcr.io/proxyllm/proxyllm:<tag>`); binary archives on the
[Releases page](https://github.com/proxyllm/proxyllm/releases).

## Documentation

- [Getting started](docs/getting-started.md)
- [Configuration reference](docs/configuration.md)
- [API reference](docs/api-reference.md)
- [Routing & fallbacks](docs/routing-and-fallbacks.md)
- [Claude Code with non-Claude models](docs/claude-code-with-non-claude-models.md)
- [Translation matrix](docs/reference/translation-matrix.md)
- [Deployment](docs/deployment.md)
- [Security guide](docs/security.md) + [SECURITY.md](SECURITY.md)
- [Architecture decision records](docs/adr/README.md)
- [Golden fixture policy](testdata/golden/README.md)

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). All external API facts this project
relies on are captured as cited golden fixtures under `testdata/golden/` —
tests derive from specifications, never from generated code behavior.

## License

[MIT](LICENSE)