# CLI adapters

The harness detects installed CLIs, asks before installing, and runs each one in
a fresh isolated config. Manifests live in `scripts/adapters/<name>.json`; the
generic runner renders `{{BASE_URL}}`, `{{CLIENT_KEY}}`, `{{MODEL}}`,
`{{PROMPT}}` and `{{RUN_DIR}}`.

## Matrix

| CLI | Tier | Isolation | Headless | Web tools | Vision |
|---|---|---|---|---|---|
| Claude Code | core | `CLAUDE_CONFIG_DIR`, fresh HOME | `claude -p … --output-format json` | `WebSearch`/`WebFetch` claimed by the proxy | via Codex `-i` |
| Codex | core | `CODEX_HOME` | `codex exec --ephemeral --skip-git-repo-check --sandbox danger-full-access --json` | `codex --search exec …` (top-level flag) | `codex exec -i FILE` |
| OpenCode | core | `OPENCODE_CONFIG_CONTENT`, fresh XDG dirs | `opencode run … --auto` | client-side; not claimed | no |
| Goose | recommended | `GOOSE_PATH_ROOT` | `goose run --no-session -t` | extensions only | no |
| Kimi Code | recommended | `KIMI_SHARE_DIR`, fresh HOME | `kimi -p … -m proxy-chat` | unverified | no |
| Aider | recommended | `--config`, fresh HOME/XDG, disposable workdir | `aider --message … --exit` | `/web` is client-side | no |
| Qwen Code | tier 2 | fresh HOME/XDG | `qwen -p` | unverified | no |
| Continue | tier 2 | `--config`, fresh HOME | `cn -p … --format json` | unverified | no |
| Cline | tier 2 | `--config`, `--data-dir` | `cline "…" -P openai -m proxy-chat --json --auto-approve true` | unverified | no |
| Factory Droid | optional | `--settings`, fresh HOME | `droid exec` | unverified | unverified |
| Crush, Roo, OpenHands | deferred | — | no documented headless mode | — | — |
| Gemini, Cursor, Copilot, Amp, Kiro, Zed | impossible | — | wrong protocol or proprietary endpoint | — | — |

## Per-CLI facts

### Claude Code

- Base URL is the proxy **root**, without `/v1`; Claude Code appends
  `/v1/messages`.
- Use `ANTHROPIC_AUTH_TOKEN` (Bearer), not `ANTHROPIC_API_KEY`; blank the
  latter and set `ANTHROPIC_DEFAULT_{HAIKU,SONNET,OPUS,FABLE}_MODEL` to the
  proxy model so auxiliary calls do not ask for a Claude ID.
- `CLAUDE_CONFIG_DIR` plus a fresh HOME keeps `~/.claude` and `~/.claude.json`
  untouched. Do **not** use `--bare`: it skips the auto-discovered tool set, so
  the client never offers `WebSearch`/`WebFetch` and the web-tool phases test
  nothing. The harness omits it.
- For web tools pass `--allowedTools WebSearch WebFetch`; the proxy claims the
  client definitions by name (`client_web_tools`). `--allowedTools` approves
  them but does not add them; a bare run exposes only `Bash`/`Edit`/`Read`.
- The canary for `~/.claude`/`~/.claude.json` is skipped when the harness runs
  inside Claude Code: the parent session writes those paths concurrently.
  Isolation is enforced by `CLAUDE_CONFIG_DIR` plus a fresh HOME.
- Install: `npm install -g @anthropic-ai/claude-code@2.1.310` (ask first).

### Codex

- Responses only: `wire_api = "responses"`, base URL **with** `/v1`.
- Write `$CODEX_HOME/config.toml` with a `[model_providers.proxyllm]` block,
  `env_key = "PROXYLLM_CLIENT_KEY"`, and the selected model.
- `codex exec --ephemeral --skip-git-repo-check --sandbox danger-full-access
  --json` is the headless path. The read-only sandbox needs user namespaces
  (`bwrap`), which many hosts refuse (`bwrap: No permissions to create a new
  namespace`); full access lets the CLI execute shell commands with the
  invoking user's privileges. The run directory must be disposable.
- `--search` is a **top-level** flag in 0.164.0 (`codex --search exec …`);
  `codex exec --search` is rejected.
- `codex exec -i FILE` attaches an image; the runner inserts `-i` after the
  `exec` subcommand.
- Install: `npm install -g @openai/codex@0.164.0` (ask first).

### OpenCode

- v1 schema: `provider.proxyllm.npm = "@ai-sdk/openai-compatible"`,
  `options.baseURL = "<base>/v1"`, `options.apiKey`, `models` map, `model =
  "proxyllm/proxy-chat"`.
- v2 uses a different schema (`providers`, `settings.baseURL`); this adapter
  pins v1 and does not claim v2 support.
- `OPENCODE_CONFIG_CONTENT` plus fresh XDG dirs avoids `~/.config/opencode` and
  `~/.local/share/opencode`. Add `--auto`, otherwise `opencode run` waits for
  tool permission and hangs in a non-interactive harness.
- Install: `npm install -g opencode-ai@1.19.4` (ask first). npm's default
  script policy may block the postinstall; run
  `npm install -g --allow-scripts=opencode-ai opencode-ai@1.19.4` if the binary
  reports that its postinstall was skipped.

### Goose

- `GOOSE_PATH_ROOT` relocates config, data and state in one variable.
- Built-in OpenAI path: `OPENAI_HOST=<base>`, `OPENAI_BASE_PATH=v1/chat/completions`,
  `OPENAI_API_KEY=<client key>`, `GOOSE_MODEL=<alias>`.
- `goose run --no-session -t "…"`.
- The official installer may exit non-zero while trying to configure through
  `/dev/tty`; the binary is already in `~/.local/bin/goose`. The harness treats
  a present binary as success.

### Kimi Code

- `KIMI_SHARE_DIR` relocates data, but Kimi Code 2.3.2 reads
  `$HOME/.kimi-code/config.toml`; the adapter writes the config under the
  isolated HOME. There is no `--config-file` flag in that release.
  `-p/--prompt` is the non-interactive mode.
- Provider type `openai_legacy` in the provider block; the model entry needs
  `protocol = "openai"` (Chat Completions), `"openai_responses"`, or
  `"anthropic"`. Base URL includes `/v1` for the OpenAI protocols.
- `kimi -p "…" -m proxy-chat` runs one prompt; run it in a disposable dir.

### Aider

- `OPENAI_API_BASE=<base>/v1`, `OPENAI_API_KEY=<client key>`, model prefix
  `openai/`.
- `--config` loads only the temp YAML; `--no-auto-commits --no-git
  --no-check-update` keeps the workdir disposable.
- Aider writes `.aider*` history/cache in the workdir; the runner removes the
  workdir.
- `--message` mode is a chat, not an agent: it has no file-reading tool. The
  tool phase is `skipped_unsupported`; a prompt that asks it to read a file
  gets a refusal, not a proxy failure.
- Aider 0.86.x requires Python >=3.10,<3.13. On a newer system Python install it
  with `uv tool install aider-chat==0.86.0 --python 3.12` (or pipx with a 3.12
  interpreter); the distro pip refuses PEP 668 installs.

### Tier 2

Qwen Code (`OPENAI_BASE_URL`, `OPENAI_API_KEY`, `OPENAI_MODEL`, isolated HOME;
verified with a tool prompt), Continue (`--config`, OpenAI-compatible model
entry; verified with a tool prompt), Cline (OpenAI-compatible provider, not the
Anthropic one: `cline auth -p openai -k <key> -m proxy-chat -b <base>/v1`,
written as `settings/providers.json` under `--data-dir`; the run uses
`-P openai -m proxy-chat --auto-approve true`). Cline's Anthropic provider
rejects a custom base URL (`base URL is only supported for OpenAI and
OpenAI-compatible providers`) and then fails with `invalid x-api-key`.

### Not testable

- Cursor, Copilot, Amp, Kiro: proprietary endpoints; no custom base URL.
- Gemini CLI: Gemini `generateContent` protocol; the proxy exposes Anthropic and
  OpenAI surfaces.
- Zed: no headless agent prompt.
- Crush: no documented non-interactive mode; PTY automation is out of scope.

## Safety

- The runner constructs a fresh environment from an explicit keep-list
  (`PATH`, `LANG`, `TERM`, `TMPDIR`, CA variables) rather than inheriting the
  caller's environment.
- Blank `ANTHROPIC_*` and `OPENAI_*` before setting the proxy values.
- Canary the real config paths (`~/.claude`, `~/.claude.json`, `~/.codex`,
  `~/.config/opencode`, `~/.local/share/opencode`, `~/.config/goose`, `~/.kimi`,
  `~/.qwen`, `~/.config/kimi`, `~/.continue`, `~/.cline`, `~/.factory`) before
  and after every run; any change is a hard failure.
