# PROxyLLM

**One proxy for every AI coding CLI.** Claude Code, Codex, OpenCode and any
OpenAI-compatible client point at PROxyLLM instead of at a vendor; the proxy
holds the provider accounts, decides which model serves each request, and
records what happened. Models stop being tied to the CLI that talks to them:
Claude Code can run an open model, Codex can run a Claude model, and an
OpenAI-SDK script reaches the same inventory. Nothing leaves your network
unless you configured the provider that receives it.

[![CI](https://github.com/TUBEOF/PROxyLLM/actions/workflows/ci.yml/badge.svg)](https://github.com/TUBEOF/PROxyLLM/actions/workflows/ci.yml)
[![Release](https://github.com/TUBEOF/PROxyLLM/actions/workflows/release.yml/badge.svg)](https://github.com/TUBEOF/PROxyLLM/actions/workflows/release.yml)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)
[![Go version](https://img.shields.io/badge/go-1.27-blue.svg)](go.mod)

## Why PROxyLLM

- **One binary and one data directory.** Go, SQLite, an embedded React
  console, CGO-free static builds. No gateway cluster, no Redis, no sidecar.
- **OAuth stays server-side.** Codex device sign-in and Anthropic
  Teams/Enterprise browser sign-in happen in the console; access and refresh
  tokens are sealed with AES-256-GCM and never reach a client.
- **Routing you can audit.** Every request resolves through an ordered rule
  chain and every transition — a skip, a retry, a failover, a vision
  substitution — is recorded in its `route_trace`. Nothing falls back
  silently.
- **Translation with the losses written down.** One canonical IR, one codec
  pair per protocol, and a [translation matrix](docs/reference/translation-matrix.md)
  that names every degradation instead of hiding it.
- **Testable without a provider account.** The conformance matrix drives
  official SDKs against MockServer wire simulators, and the golden fixtures
  are transcribed from vendor documentation with source and date.

## What it connects

| Client | How it reaches the proxy |
|---|---|
| **Claude Code** | `ANTHROPIC_BASE_URL`, `ANTHROPIC_AUTH_TOKEN`, gateway model discovery |
| **Codex** | `~/.codex/config.toml` provider with `wire_api = "responses"` and `model_catalog_url` |
| **OpenCode** | `opencode.json` custom provider with static model blocks |
| **Any OpenAI SDK** | `base_url` + a `pk_` key |

| Provider type | Kind | Credential |
|---|---|---|
| `ollama-cloud` | chat | API key (multiple accounts) |
| `ollama-cloud-web` | web tools | API key (multiple accounts) |
| `codex-oauth` | chat | OAuth device flow, multiple sign-ins |
| `anthropic` | chat | API key (multiple keys) |
| `anthropic-oauth` | chat | Teams/Enterprise OAuth browser sign-in |
| `firecrawl` | web tools | API key |
| `custom-openai` | chat | API key, any OpenAI-compatible endpoint |
| `ollama-native` | chat | native `/api/chat` server |

Web-tool accounts (`firecrawl`, `ollama-cloud-web`) serve the emulated
`web_search` / `web_fetch` calls for models that have no native server tools.

## Quickstart

```bash
curl -O https://raw.githubusercontent.com/TUBEOF/PROxyLLM/main/docker/compose.yaml
docker compose up -d
curl http://localhost:8080/healthz
```

Open `http://localhost:8080`: the first visit bootstraps the admin account,
and the setup wizard walks provider → web tools → fallbacks → aliases → key →
**Connect a CLI**. Every step is skippable and the progress survives a
reload.

### Connect a CLI

<details>
<summary><strong>Claude Code</strong></summary>

```bash
export ANTHROPIC_BASE_URL="http://localhost:8080"
export ANTHROPIC_AUTH_TOKEN="pk_..."          # Bearer (recommended)
export ANTHROPIC_MODEL="anthropic/glm-5.3"
export ANTHROPIC_DEFAULT_HAIKU_MODEL="anthropic/gpt-oss:20b"
export CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY=1
```

Or persist the same block in `~/.claude/settings.json` under `env`. The
discovery flag makes `/model` read `GET /v1/models`; Claude Code shows only
ids containing `claude` or `anthropic`, which is why the examples use routing
aliases. Full guide: [Claude Code](docs/cli-claude-code.md).
</details>

<details>
<summary><strong>Codex</strong></summary>

```toml
# ~/.codex/config.toml
model = "anthropic/glm-5.3"
model_provider = "proxyllm"
model_reasoning_effort = "medium"

[model_providers.proxyllm]
name = "PROxyLLM"
base_url = "http://localhost:8080/v1"
wire_api = "responses"
env_key = "PROXYLLM_API_KEY"
model_catalog_url = "http://localhost:8080/v1/model_catalog"

[features]
api_key_model_discovery = true
```

```bash
export PROXYLLM_API_KEY="pk_..."
```

Full guide: [Codex](docs/cli-codex.md).
</details>

<details>
<summary><strong>OpenCode</strong></summary>

```json
{
  "$schema": "https://opencode.ai/config.json",
  "provider": {
    "proxyllm": {
      "npm": "@opencode-ai/openai-compatible",
      "name": "PROxyLLM",
      "options": { "baseURL": "http://localhost:8080/v1" },
      "models": { "anthropic/glm-5.3": {} }
    }
  },
  "model": "proxyllm/anthropic/glm-5.3"
}
```

Export `PROXYLLM_API_KEY` (or `opencode auth login proxyllm`). OpenCode does
not fetch `/v1/models` for custom providers; the `models` block is the native
mechanism. Full guide: [OpenCode](docs/cli-opencode.md).
</details>

<details>
<summary><strong>Any OpenAI-compatible client</strong></summary>

```python
from openai import OpenAI
client = OpenAI(base_url="http://localhost:8080/v1", api_key="pk_...")
```

</details>

### Or let the installer do it

```bash
curl -fsSL https://raw.githubusercontent.com/TUBEOF/PROxyLLM/main/scripts/install.sh | bash
```

It detects the installed CLIs, probes `/v1/models`, writes each config with a
backup, and supports `--dry-run` and `--uninstall`. The key is never printed;
Codex and OpenCode read it from `PROXYLLM_API_KEY`. Details:
[CLI setup](docs/cli-setup.md).

## Routing: one model name, many sources

```
client ──▶ rule chain ──▶ account strategy ──▶ health gate ──▶ provider
             │                 │                    │
          model match      round robin /        cooldown,
          + capability     fill-first           failover
```

Rules choose by exact model, by alias, or by provider type. An alias is how a
CLI's hard-coded name reaches a different model:

```json
{"claude-*": "anthropic/glm-5.3", "gpt-*": "ollamacloud/kimi-k2"}
```

The same model can be served by several accounts; the strategy spreads or
drains them, and a failing account cools down while the chain fails over.
Read [routing and fallbacks](docs/routing-and-fallbacks.md) for the exact
semantics and [route trace](docs/reference/route-trace.md) for the events.

## Accounts, keys and sign-in

- Clients authenticate with **`pk_` keys**, created in the console and shown
  exactly once (only a one-way keyed hash is stored).
- Provider credentials never come from a client request. The proxy's
  header allow-list forwards `anthropic-*`, `openai-beta` and `x-stainless-*`;
  `Authorization`, `x-api-key` and cookies never reach an upstream.
- **OAuth sign-in happens only in the console.** Codex uses the device flow;
  Anthropic OAuth (Teams/Enterprise) uses a browser flow with a pasted
  callback, because the browser often cannot reach a server install's
  loopback port. Consumer Anthropic subscriptions are blocked by Anthropic
  for third-party use since January 2026 — the console says so before the
  first sign-in. See [Anthropic OAuth](docs/providers/anthropic-oauth.md) and
  [security](docs/security.md).

## Documentation

| | |
|---|---|
| **Try it** | [Getting started](docs/getting-started.md) · [Deployment](docs/deployment.md) · [Upgrading from v1](docs/release-migration.md) |
| **Run it** | [Configuration](docs/configuration.md) · [Operating the console](docs/operating.md) · [Security](docs/security.md) · [Routing and fallbacks](docs/routing-and-fallbacks.md) |
| **Call it** | [API reference](docs/api/README.md) · [CLI setup](docs/cli-setup.md) · [Translation matrix](docs/reference/translation-matrix.md) |
| **Build it** | [Development](docs/development.md) · [Architecture decision records](docs/adr/README.md) · [Test matrix](docs/test-matrix.md) |

The full index is [docs/README.md](docs/README.md).

## Contributing

Contributions are welcome — see [CONTRIBUTING.md](CONTRIBUTING.md). Wire
facts are captured as cited golden fixtures under `testdata/golden/`, and
tests derive from those specifications rather than from the implementation's
own output.

## License

[MIT](LICENSE)
