# PROxyLLM

A single-binary LLM proxy in Go. It serves the OpenAI, Anthropic and decision
protocol families from one endpoint, translates between them, and routes every
request through operator-defined chains with an observable trail.

[![CI](https://github.com/TUBEOF/PROxyLLM/actions/workflows/ci.yml/badge.svg)](https://github.com/TUBEOF/PROxyLLM/actions/workflows/ci.yml)
[![Release](https://github.com/TUBEOF/PROxyLLM/actions/workflows/release.yml/badge.svg)](https://github.com/TUBEOF/PROxyLLM/actions/workflows/release.yml)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)
[![Go version](https://img.shields.io/badge/go-1.27-blue.svg)](go.mod)

## What it does

**Three client-facing surfaces, one service:**

- **OpenAI-compatible** — `POST /v1/chat/completions`, `POST /v1/responses`,
  `GET /v1/models`
- **Anthropic-compatible** — `POST /v1/messages`,
  `POST /v1/messages/count_tokens`, `GET /v1/models`
- **Decision models** — `POST /v1/systemone`, `POST /v1/decisions`: typed
  questions scored against shared evidence in one non-streaming pass

A canonical intermediate representation sits at the hub, so any client shape
can reach any upstream: Claude Code works through `ANTHROPIC_BASE_URL` against
whatever model you route it to, and an OpenAI-SDK client reaches the same
model inventory.

**Routing you can audit.** Requests resolve through an ordered rule chain —
model match, capability filter, account strategy, health gate — and every
transition (a skip, a retry, a failover, a substitution) is recorded in the
request's `route_trace` and rendered in the console. Nothing falls back
silently.

**Providers:**

| Type | What it is |
|---|---|
| **ChatGPT / Codex** | The ChatGPT backend via OAuth device flow, native in the proxy |
| **Ollama Cloud** | The hosted API through its OpenAI-compatible `/v1` endpoint |
| **Custom OpenAI-compatible** | Any endpoint, by configuration alone |
| **Firecrawl** and **Ollama Cloud Web Tools** | Web-tool backends that serve emulated `web_search` / `web_fetch` for models without native server tools |

**Properties that matter in production:**

- **Streaming-first** — SSE relayed as it arrives, bounded channels for
  backpressure, cancellation propagated from the client to the upstream.
- **Failover with discipline** — pre-first-byte retries, health-aware
  circuits, capability fallbacks (an image described by a vision model and
  substituted, so a long session can still be served), and a documented
  error model.
- **Secure by default** — authentication on unless explicitly disabled,
  provider credentials sealed with AES-256-GCM, client keys stored as keyed
  MACs and shown once, every log call through a tested redaction layer.
- **One binary, one data directory** — an embedded console (React, no runtime
  assets), SQLite, CGO-free static build.

## Quickstart

The fastest path runs the published image — no source checkout needed:

```bash
curl -O https://raw.githubusercontent.com/TUBEOF/PROxyLLM/main/docker/compose.yaml
docker compose up -d
curl http://localhost:8080/healthz
```

Open `http://localhost:8080`: the first visit bootstraps the admin account.
Connect a provider, create an API key (**API keys**), and point a client at
the proxy:

```bash
# Claude Code
export ANTHROPIC_BASE_URL="http://localhost:8080"
export ANTHROPIC_AUTH_TOKEN="pk_..."
export ANTHROPIC_MODEL="claude-sonnet-5"

# OpenAI-SDK clients
# base_url = http://localhost:8080/v1
# api_key  = pk_...
```

From a checkout, `make docker` builds the image locally and runs the same
compose file against it, and `make build` produces the binary. The multi-arch
image also runs through plain `docker run` (see
[deployment](docs/deployment.md)), and archives are on the
[releases page](https://github.com/TUBEOF/PROxyLLM/releases).

## Documentation

| | |
|---|---|
| **Try it** | [Getting started](docs/getting-started.md) · [Deployment](docs/deployment.md) |
| **Run it** | [Configuration](docs/configuration.md) · [Operating the console](docs/operating.md) · [Security](docs/security.md) · [Routing and fallbacks](docs/routing-and-fallbacks.md) |
| **Call it** | [API reference](docs/api/README.md) · [Claude Code](docs/claude-code.md) · [Translation matrix](docs/reference/translation-matrix.md) |
| **Build it** | [Development](docs/development.md) · [Architecture decision records](docs/adr/README.md) · [Route trace](docs/reference/route-trace.md) |

The full index is [docs/README.md](docs/README.md).

## Contributing

Contributions are welcome — see [CONTRIBUTING.md](CONTRIBUTING.md). The short
version: the API facts this proxy relies on are captured as cited golden
fixtures under `testdata/golden/`, and tests derive from those specifications
rather than from the implementation's own output.

## License

[MIT](LICENSE)
