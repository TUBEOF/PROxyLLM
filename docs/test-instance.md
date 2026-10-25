# Local test instance

This page is the stable procedure for standing up a disposable PROxyLLM
instance, configuring it against real upstreams, and running the multi-CLI live
test. The executable harness lives in the `proxyllm-test-instance` skill; this
page owns the invariants it implements.

Use it when you need to reproduce a client bug, verify a fallback or emulated
web tool against real upstreams, or exercise a CLI that cannot be pointed at
production.

## Prerequisites

- A checkout of this repository and Go available (`make build`).
- An Ollama Cloud API key. The key is written to `<instance>/.env` (0600) and is
  never printed. `init --key-file ~/.config/proxyllm/keys.env` reads only
  `OLLAMA_CLOUD_API_KEY` from an existing dotenv file.
- Optional: the client CLIs you want to exercise. The harness never installs
  one without an explicit `--yes-install`; missing CLIs are reported with their
  install command and skipped.

## Instance layout

The default home is `~/proxyllm-test-instance`; set `--home PATH` on any
command or export `PROXYLLM_TEST_HOME`. Runtime state never goes into the
repository.

```text
<home>/
  .env                     0600: OLLAMA_CLOUD_API_KEY, ADMIN_USER,
                           ADMIN_PASSWORD, CLIENT_KEY
  instance.json            repo, port, requested/resolved models, binary SHA
  bin/proxyllm             copied build + SHA256
  data/proxyllm.db         SQLite store; master.key is created 0600
  logs/proxyllm.log        proxy log; logs/instance.log harness actions
  run/                     pid, lock, cookie jar (0600), temp files
  clients/<cli>/<run-id>/  isolated HOME/XDG/config per CLI run
  artifacts/<run-id>/      report.json, report.md, CLI captures, images
```

A marker file (`.proxyllm-test-instance`) lets teardown refuse an arbitrary
path.

## Create the instance

```bash
.agents/skills/proxyllm-test-instance/scripts/test-instance.sh init \
  --home ~/proxyllm-test-instance \
  --key-file ~/.config/proxyllm/keys.env \
  --chat-model deepseek-v4.1-flash:1017 \
  --fallback-model nemotron-3-ultra
```

`init` prompts for the path on a TTY (default shown) and asks for the key
source and models when the flags are absent. It generates a fresh admin
password and writes `.env` 0600. It does not write a client key; `setup` reuses
a valid one or creates a new one.

## Start it

```bash
.agents/skills/proxyllm-test-instance/scripts/test-instance.sh start --build
.agents/skills/proxyllm-test-instance/scripts/test-instance.sh status
```

`start --build` rebuilds the current sources and copies the binary into the
instance. The server listens on `127.0.0.1:18080` by default; if the port is
taken, choose another with `init --port`.

## Configure it

```bash
.agents/skills/proxyllm-test-instance/scripts/test-instance.sh setup
```

`setup` performs these writes in order:

1. `PUT /api/admin/providers/acc-ollama-cloud` and
   `PUT /api/admin/providers/acc-ollama-web` with the same key
   (`ollama-cloud` and `ollama-cloud-web`), then enables both.
2. `POST /api/admin/model-sync/run`; a skipped chat provider is a failure.
3. Resolves the requested model names against the synced rows and refuses to
   guess when a name is ambiguous.
4. Pins the fallback-test row `vision:false` with `pinned.vision:true`.
5. Merges `claude-*`, `proxy-chat`, `sonnet/opus/haiku/fable`,
   `fallback-test` and the requested names into `model_aliases`; a conflicting
   existing alias is kept unless `--force-aliases` is passed.
6. Writes the vision and web-tool fallback lists in **one**
   `PUT /api/admin/capability-fallbacks` call.
7. Verifies an existing client key against `GET /v1/models` or creates a new
   one with `POST /api/admin/keys` (display-once), then stores it in `.env`.
8. Probes both model rows via `POST /api/admin/models/test/{id}`.

`dry-run` prints the current providers, model count, settings keys, fallback
lists and both resolved models without config writes. `setup` is idempotent; it
rewrites the fallback lists to force the live reload.

### Two invariants

- **Never write `vision_fallback` through `PUT /api/admin/settings`.** It
  persists, but `ReloadSettings` does not call `SetCapabilityFallbacks`; the
  live list stays stale. Use `PUT /api/admin/capability-fallbacks`.
- **A fallback-test model must be known blind.** The pre-dispatch image
  substitution runs only when every chain entry is pinned or detected
  `vision:false`; the harness pins it explicitly.

## Run the test pattern

```bash
.agents/skills/proxyllm-test-instance/scripts/test-instance.sh test
```

| Phase | What it proves |
|---|---|
| 0 | Health, permissions, provider inventory, client key, fallback state, CLI inventory |
| 1 | “Hallo” through each selected CLI, with a 200 request and the expected protocol |
| 2 | Watermark-scoped log/DB scan and the console log read path |
| 3 | Alias resolution, vision fallback with a real PNG, and failover injection |
| 4 | Protocol-level `web_search`/`web_fetch` and Claude Code web tools |
| 5 | Three vision runs on the blind model |
| 6 | The full Neon-UX prompt through Claude Code (or the protocol fallback) |

Reports land in `artifacts/<run-id>/report.json` and `report.md`. Statuses are
`pass`, `fail`, `warn`, `skipped`, `skipped_missing`, `skipped_unsupported`,
`upstream`, `model_behavior` and `client_config`. A fetch blocked upstream
(202/403/0 bytes) is reported as `upstream` with its URL and status; it is not
hidden and does not become a proxy bug.

The failover-injection part of phase 3 starts a local HTTP server that returns
500, so the instance must be started with private provider URLs enabled. Pass
`--allow-private-provider-urls` to `init` (or set
`"allow_private_provider_urls": true` in `instance.json` and restart); without
it the phase is reported `skipped` with that reason. The opt-out only affects a
disposable loopback instance.

## Inspect and stop

```bash
.agents/skills/proxyllm-test-instance/scripts/test-instance.sh logs 100
.agents/skills/proxyllm-test-instance/scripts/db.py --db \
  ~/proxyllm-test-instance/data/proxyllm.db recent --limit 20
.agents/skills/proxyllm-test-instance/scripts/test-instance.sh stop
.agents/skills/proxyllm-test-instance/scripts/test-instance.sh teardown --purge --yes
```

`teardown` without `--purge` only stops the process. Purge requires the marker,
the data directory, `--yes`, and a path under `$HOME`; it removes the instance
home.

## Client safety

Each CLI runs with `env -i`, a fresh HOME/XDG/config directory under
`clients/`, and a canary snapshot of its real config paths. A change to
`~/.codex`, `~/.config/opencode` or any other real config path is a hard
`client_config` failure. Claude Code skips the canary when the harness runs
inside Claude Code itself (the parent session writes `~/.claude` and
`~/.claude.json` concurrently); its isolation is `CLAUDE_CONFIG_DIR` plus a
fresh HOME. The client key reaches the CLI environment
(`PROXYLLM_TEST_CLIENT_KEY`), never a command-line argument. Installation is
opt-in and never automatic.

## Related

- Client matrix and invocation details:
  `.agents/skills/proxyllm-test-instance/references/cli-adapters.md`
- Failure classification:
  `.agents/skills/proxyllm-test-instance/references/troubleshooting.md`
- Admin endpoints and CSRF: [api/admin.md](api/admin.md)
- Configuration layers and keys: [configuration.md](configuration.md)
- Claude Code against non-Claude models: [claude-code.md](claude-code.md)
