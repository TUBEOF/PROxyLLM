import { useMemo, useState } from 'react';
import { copyText } from '../clipboard';
import { IconCheck, IconCopy } from '../icons';

// ConnectSnippets renders the copy-paste configuration for one client, with
// the console's own origin and a model chosen from the proxy's list. It is
// shared by /connect, the display-once key dialog and the setup wizard so the
// three surfaces cannot drift (ADR-026).

type Tab = 'claude' | 'codex' | 'opencode' | 'generic';

const TABS: { id: Tab; label: string }[] = [
  { id: 'claude', label: 'Claude Code' },
  { id: 'codex', label: 'Codex' },
  { id: 'opencode', label: 'OpenCode' },
  { id: 'generic', label: 'Generic OpenAI' },
];

export interface ConnectSnippetsProps {
  baseUrl: string;
  // The display-once key, or a placeholder when the key is not in hand.
  apiKey: string;
  models: string[];
  defaultModel?: string;
  // Hide the model picker where the caller already chose one.
  showModelPicker?: boolean;
}

function haikuOf(models: string[], main: string): string {
  for (const m of models) {
    if (m === main) continue;
    if (/haiku|flash|mini|small/i.test(m)) return m;
  }
  return models[0] ?? main;
}

export default function ConnectSnippets({
  baseUrl,
  apiKey,
  models,
  defaultModel,
  showModelPicker = true,
}: ConnectSnippetsProps) {
  const [tab, setTab] = useState<Tab>('claude');
  const [model, setModel] = useState(defaultModel ?? models[0] ?? 'your-model');
  const [copied, setCopied] = useState<string | null>(null);

  const haiku = useMemo(() => haikuOf(models, model), [models, model]);

  const snippets = useMemo<Record<Tab, string>>(
    () => ({
      claude: `{
  "env": {
    "ANTHROPIC_BASE_URL": "${baseUrl}",
    "ANTHROPIC_AUTH_TOKEN": "${apiKey}",
    "ANTHROPIC_MODEL": "${model}",
    "ANTHROPIC_DEFAULT_HAIKU_MODEL": "${haiku}",
    "CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY": "1"
  }
}`,
      codex: `model = "${model}"
model_provider = "proxyllm"
model_reasoning_effort = "medium"

[model_providers.proxyllm]
name = "PROxyLLM"
base_url = "${baseUrl}/v1"
wire_api = "responses"
env_key = "PROXYLLM_API_KEY"
model_catalog_url = "${baseUrl}/v1/model_catalog"

[features]
api_key_model_discovery = true`,
      opencode: `{
  "$schema": "https://opencode.ai/config.json",
  "provider": {
    "proxyllm": {
      "npm": "@opencode-ai/openai-compatible",
      "name": "PROxyLLM",
      "options": { "baseURL": "${baseUrl}/v1" },
      "models": { "${model}": {} }
    }
  },
  "model": "proxyllm/${model}"
}`,
      generic: `# Any OpenAI-compatible SDK
base_url = ${baseUrl}/v1
api_key  = ${apiKey}`,
    }),
    [baseUrl, apiKey, model, haiku],
  );

  async function copy(which: string, text: string) {
    const outcome = await copyText(text);
    setCopied(outcome === 'copied' ? which : null);
  }

  const installer = `PROXYLLM_URL=${baseUrl} PROXYLLM_CLIS=${tab === 'generic' ? 'none' : tab} \\
  bash <(curl -fsSL https://raw.githubusercontent.com/TUBEOF/PROxyLLM/main/scripts/install.sh)`;

  return (
    <div className="connect-block">
      <div className="seg" role="tablist" aria-label="Client">
        {TABS.map((t) => (
          <button
            key={t.id}
            type="button"
            role="tab"
            aria-selected={tab === t.id}
            className={tab === t.id ? 'active' : ''}
            onClick={() => setTab(t.id)}
          >
            {t.label}
          </button>
        ))}
      </div>

      {showModelPicker && models.length > 0 && (
        <label className="field">
          <span>Model</span>
          <select className="select" value={model} onChange={(e) => setModel(e.target.value)}>
            {models.map((m) => (
              <option key={m} value={m}>{m}</option>
            ))}
          </select>
          <span className="hint">
            The Claude Code picker only shows ids containing <code>claude</code> or <code>anthropic</code>; use a
            routing alias such as <code>anthropic/{model.replace(/^.*\//, '')}</code> when it must appear there.
          </span>
        </label>
      )}

      <div className="connect-snippet">
        <pre>{snippets[tab]}</pre>
        <button
          type="button"
          className="btn btn-secondary btn-sm"
          onClick={() => void copy(tab, snippets[tab])}
          aria-label={`Copy the ${tab} configuration`}
        >
          {copied === tab ? <IconCheck width={14} height={14} /> : <IconCopy width={14} height={14} />}
          {copied === tab ? 'Copied' : 'Copy'}
        </button>
      </div>

      {tab !== 'generic' && (
        <div className="connect-snippet">
          <p className="hint">
            Or let the installer write this file: it asks for the key (this snippet embeds it), keeps a backup and can
            undo itself. Export <code>PROXYLLM_API_KEY</code> first; the installer never prints it.
          </p>
          <pre>{installer}</pre>
          <button
            type="button"
            className="btn btn-secondary btn-sm"
            onClick={() => void copy('installer', installer)}
            aria-label="Copy the installer command"
          >
            {copied === 'installer' ? <IconCheck width={14} height={14} /> : <IconCopy width={14} height={14} />}
            {copied === 'installer' ? 'Copied' : 'Copy'}
          </button>
        </div>
      )}

      {tab === 'claude' && (
        <p className="hint">
          The gateway discovery flag makes Claude Code read <code>/v1/models</code> at start; the small-fast model
          answers its background requests.
        </p>
      )}
      {tab === 'codex' && (
        <p className="hint">
          Export <code>PROXYLLM_API_KEY</code> before starting Codex; the key is never written to the TOML file.
        </p>
      )}
      {tab === 'opencode' && (
        <p className="hint">
          Export <code>PROXYLLM_API_KEY</code>, or run <code>opencode auth login proxyllm</code>. Custom providers
          use static model blocks; OpenCode does not fetch <code>/v1/models</code> for them.
        </p>
      )}
    </div>
  );
}
