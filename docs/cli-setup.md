# CLI setup and the installer

The installer configures an already-installed CLI against the proxy. It does
**not** install the CLI itself.

```bash
curl -fsSL https://raw.githubusercontent.com/TUBEOF/PROxyLLM/main/scripts/install.sh | bash
# or download and review it first:
curl -fsSL -o install.sh https://raw.githubusercontent.com/TUBEOF/PROxyLLM/main/scripts/install.sh
bash install.sh
```

## What it asks and what it writes

1. **Proxy URL** (default `http://localhost:8080`) and a client key
   (`pk_…`, hidden input). It probes `GET /v1/models`; on failure it stops
   before writing anything.
2. **Installed CLIs** via `command -v claude codex opencode`; missing ones are
   named with their official install link.
3. **Selection** by number (`1 3`), `all` or `none`.
4. Per CLI: a default model from the probe (Claude Code prefers an id
   containing `claude`/`anthropic` for its gateway picker), then:

| CLI | File | What is written |
|---|---|---|
| Claude Code | `~/.claude/settings.json` | `env`: `ANTHROPIC_BASE_URL`, `ANTHROPIC_AUTH_TOKEN`, `ANTHROPIC_MODEL`, `ANTHROPIC_DEFAULT_HAIKU_MODEL`, `CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY=1` |
| Codex | `~/.codex/config.toml` | `model`, `model_provider = "proxyllm"`, `model_reasoning_effort`, `[model_providers.proxyllm]` (`base_url`, `wire_api = "responses"`, `env_key = "PROXYLLM_API_KEY"`, `model_catalog_url`), `[features] api_key_model_discovery = true` |
| OpenCode | `~/.config/opencode/opencode.json` | `provider.proxyllm` (OpenAI-compatible package, `baseURL`, static `models` blocks, max 20) and the default `model` |

The client key is written to Claude Code's settings (that file carries the
credential by design) and **never** to Codex or OpenCode: both read it from
the `PROXYLLM_API_KEY` environment variable. Existing keys you set yourself are
preserved; the installer changes only the keys above.

## Flags and environment

```
--dry-run     print a unified diff, write nothing
--uninstall   remove only the blocks the installer wrote (backups are kept)
--yes         accept defaults; answers come from the environment
```

| Variable | Meaning |
|---|---|
| `PROXYLLM_URL` | proxy base URL |
| `PROXYLLM_API_KEY` | the `pk_…` client key |
| `PROXYLLM_CLIS` | comma list, e.g. `claude,codex` (skips detection and prompts) |

Non-interactive example:

```bash
PROXYLLM_URL=http://proxy:8080 PROXYLLM_API_KEY=pk_... \
PROXYLLM_CLIS=claude,codex bash install.sh --yes
```

## Merge rules

- **Claude Code**: `settings.json` is parsed as JSON and only the `env` keys
  above are updated; every other setting survives.
- **Codex**: the top-level `model`, `model_provider` and
  `model_reasoning_effort` lines are replaced, the `[model_providers.proxyllm]`
  table is replaced, and `api_key_model_discovery` is set under `[features]`.
  All other tables and keys survive. Comments inside the managed block are
  marked `# managed by PROxyLLM: begin/end`.
- **OpenCode**: the `provider.proxyllm` object and the default `model` are
  replaced; other providers and settings survive.
- Every write starts with `file.bak.<epoch>`, and the result is `chmod 600`
  where the format allows.
- A second run is byte-identical (idempotent).

## Limits

- No CLI installation, no package managers.
- No Windows support; use WSL.
- JSON editing needs `python3`; the model list can also be read with `jq`.
  Without either, the script stops with a clear message.
- OpenCode's `@opencode-ai/openai-compatible` package and the `models` block
  layout are the documented v2 layout (verified against the OpenCode docs at
  the time of writing). If your OpenCode build uses the v1 provider layout,
  edit `opencode.json` by hand; the proxy side is unchanged.

## After installing

Start each CLI and check its first request in the console's **Logs** page.
Claude Code's `/model` picker shows gateway models only for ids containing
`claude` or `anthropic`; that filter is the client's, so use a routing alias
such as `anthropic/glm-5.3` when a model must appear there (see
[dynamic model discovery](adr/ADR-023-dynamic-model-discovery.md)).
