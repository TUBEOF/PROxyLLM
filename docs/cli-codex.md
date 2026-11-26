# Codex against PROxyLLM

Codex speaks the OpenAI Responses wire and reads a model catalog when one is
configured.

## 1. Install

```bash
npm install -g @openai/codex
codex --version
```

## 2. Configure `~/.codex/config.toml`

```toml
model = "anthropic/glm-5.3"
model_provider = "proxyllm"
model_reasoning_effort = "medium"          # low / medium / high / xhigh

[model_providers.proxyllm]
name = "PROxyLLM"
base_url = "http://<proxy>:8080/v1"        # Codex appends /responses
wire_api = "responses"                     # the only supported value now
env_key = "PROXYLLM_API_KEY"               # the key stays out of the file
model_catalog_url = "http://<proxy>:8080/v1/model_catalog"

[features]
api_key_model_discovery = true
```

```bash
export PROXYLLM_API_KEY="pk_..."
```

Both `model_catalog_url` **and** `api_key_model_discovery` are required for
the `/model` picker to show proxy models. The catalog endpoint answers the
Codex-native shape (top-level `models`), and the proxy always emits
`supported_reasoning_levels` (empty when unknown) because Codex rejects a
catalog whose entries omit it.

`model_catalog_json` (a local file) is the alternative when the proxy is not
reachable at start.

## 3. Verify

Run `codex`, open `/model` and pick a proxy model, or pass `codex -m <model>`.
The first request appears in the console's **Logs** page.

Install the file automatically: [CLI setup](cli-setup.md).
