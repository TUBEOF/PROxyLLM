# OpenCode against PROxyLLM

OpenCode's custom providers are configured statically: it does **not** fetch
`/v1/models` for them, so the model list is written into `opencode.json`.

## 1. Install

Follow [opencode.ai/docs](https://opencode.ai/docs) for your platform.

## 2. Configure `~/.config/opencode/opencode.json`

```json
{
  "$schema": "https://opencode.ai/config.json",
  "provider": {
    "proxyllm": {
      "npm": "@opencode-ai/openai-compatible",
      "name": "PROxyLLM",
      "options": { "baseURL": "http://<proxy>:8080/v1" },
      "models": {
        "anthropic/glm-5.3": {},
        "ollamacloud/kimi-k2": { "name": "Kimi (via proxy)" }
      }
    }
  },
  "model": "proxyllm/anthropic/glm-5.3"
}
```

The `models` keys are the proxy's model ids; `model` names one of them with
the provider prefix.

## 3. Credential

Either export the key or use the CLI's own login:

```bash
export PROXYLLM_API_KEY="pk_..."
opencode auth login proxyllm --method key   # alternative
```

## 4. Verify

Start `opencode`; `/models` lists the configured entries, and the first
request appears in the console's **Logs** page.

The provider layout is version-sensitive (OpenCode has shipped more than one
provider schema). If your build rejects `npm`/`options`, check the current
OpenCode documentation; the proxy side is unchanged.

The installer generates the `models` blocks from `/v1/models`:
[CLI setup](cli-setup.md). A community discovery plugin exists but is not
required.
