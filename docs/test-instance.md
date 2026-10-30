# Live CLI workflow

The live CLI suite is `.github/workflows/live-cli.yml`. It builds the current
sources, starts an ephemeral PROxyLLM instance on `127.0.0.1:18080`, configures
Ollama Cloud through the admin API, and runs every supported CLI through four
phases. The reusable scripts live under `.github/live-cli/`; this page owns the
invariants.

## Trigger and secrets

- `push` and `pull_request`, path-filtered to `internal/translate/**`,
  `internal/provider/**`, `internal/routing/**`, `internal/server/**`,
  `internal/auth/**`, `internal/storage/**`, `internal/smoke/**`, `cmd/**`,
  `go.mod`, `go.sum` and the harness itself.
- `workflow_dispatch` for a manual run.
- The protected GitHub Environment `live-cli` supplies
  `OLLAMA_CLOUD_API_KEY`. Fork PRs are skipped because they never receive the
  secret.
- `concurrency` cancels a superseded run on the same ref.

## Phases

Each task is a separate CLI invocation with a fresh run directory, and every
task exercises exactly one feature. Prompts are English only. After every task
the driver checks the CLI exit code, the `request_logs` row (200, empty
`error_redacted`, no `failed`/`rejected` in the trace) and the proxy log window
(no `ERROR`).

| Feature | Tasks | Model | Expected evidence |
|---|---|---|---|
| `hello` | 1 | chat | 200, no error, no proxy `ERROR` |
| `tools` | 1 (read `notes.txt`) | chat | the marker file's line appears in the CLI output |
| `vision` | 1 | blind model + vision fallback | `capability_fallback` with `capability="vision"` **and** `image_caption` |
| `web_search` | 3 independent queries | chat | a **successful** `emulated_tool` with `to="web_search"` and more than zero results, **and** a cited `http(s)://` URL in the CLI output |
| `web_fetch` | 2 independent pages | chat | a **successful** `emulated_tool` with `to="web_fetch"` and its `fetched` reason, **and** a value only the live page can contain (a fresh random number / string) in the CLI output |

The image is a real 512×512 PNG (a 1×1 image makes vision upstreams fail).
The primary model is deliberately blind, and the vision-fallback list points
at a vision-capable model, so the fallback must trigger. The image is copied
into the run's working directory and attached by the adapter's own mechanism
(`codex exec -i`; the other CLIs' file tools and `--file` flags did not put
the image into the model request).

**Hard rules:**

- `emulated_tool_attempt` is **not** evidence: it also records `failed` and
  `empty` outcomes. Only a successful `emulated_tool` with content, plus the
  answer evidence in the CLI's own output, passes a web task.
- There is **no retry**. A model that declines the tool on one task fails
  that task; a suite that retries hides exactly the flakiness it exists to
  catch.
- A feature is skipped only when the adapter declares no headless path for
  it, and the report carries that reason.

## CLI coverage

Every runnable adapter is attempted for every feature; an adapter that cannot
run one declares it in its manifest and the report names the reason.

| CLI | Tools | Vision | Web search | Web fetch |
|---|---|---|---|---|
| Claude Code | yes | no — `Read` did not attach the image | yes (`WebSearch`) | no — the model intermittently keeps `WebFetch`, which the CLI runs client-side |
| Codex | yes | yes (`exec -i`) | yes (`--search`) | no — no fetch tool |
| OpenCode | yes | no — `--file` did not attach the image | no | yes (`webfetch`, claimed) |
| Qwen Code | yes | no | no | no — not exposed to the model in one-shot mode |
| Cline | yes | no | no | yes (`fetch_web_content`, claimed) |
| Continue | yes | no | no | yes (`Fetch`, claimed) |
| Goose | yes | no — `read_image` does not reach the model request | no | no |
| Kimi Code | yes | no — `Read` returns text, not an image part | no | yes (`FetchURL`, claimed) |
| Aider | no | no | no | no |

Client-declared web tools are served through the web-tool accounts: the
harness writes one `client_web_tools` mapping covering the CLIs' tool names
(`admin.py`), and the proxy claims them on every surface. A CLI whose tool is
not mapped would fetch client-side and never reach the proxy — that is what
the successful `emulated_tool` assertion catches.

Factory Droid stays `skipped_unsupported` (needs `FACTORY_API_KEY`).

## Local reproduction

The workflow is the source of truth. To reproduce against a disposable
instance:

```bash
python3 -I .github/live-cli/bootstrap.py --home /tmp/proxyllm-live \
  --chat-model deepseek-v4.1-flash:1017 --fallback-model nemotron-3-ultra
# start ./proxyllm with PROXYLLM_SERVER_ADDR=127.0.0.1:18080,
# PROXYLLM_STORAGE_PATH, PROXYLLM_MASTER_KEY_PATH,
# PROXYLLM_LOG_FORMAT=json (see the workflow's "Start instance" step)
python3 -I .github/live-cli/admin.py --home /tmp/proxyllm-live setup
python3 -I .github/live-cli/live_cli.py --home /tmp/proxyllm-live
```

`bootstrap.py` writes `.env` (0600) from `OLLAMA_CLOUD_API_KEY`, generates the
admin password, and writes `instance.json`. It is idempotent: rerunning it
preserves existing secrets and the resolved model ids. `admin.py` bootstraps
the admin, creates the two Ollama Cloud accounts (chat and web tools), syncs
models, pins the blind test model, merges the Claude aliases, writes the
capability fallbacks, and mints the client key. `live_cli.py` runs the phases.

## Report and safety

Reports land in `<home>/artifacts/<run-id>/report.json` and `report.md`; the
Markdown table lists every CLI, its exact installed version, the result of
each feature, and a Notes column with the skip reason per feature. The Overall
column reads `PASS n/m` (passed/supported features) or `FAIL n/m`; there is no
`PARTIAL`, because it used to make "three of four tested" indistinguishable
from "hello only". A supported feature is `pass` only when every task of that
feature passed. The release workflow runs this suite on the tagged commit and
appends that table to the GitHub release notes. The process
exits non-zero when any check is `fail`. The client key never enters the
report, and `run_cli.py` redacts the key from captured CLI output. Each CLI
runs with a fresh HOME/XDG/config/work directory and a canary snapshot of its
real config paths; a change to a real config path is a failure.

## Related

- Client matrix and invocation details: `.github/live-cli/adapters/*.json`
- Field mapping: [translation matrix](reference/translation-matrix.md)
- Trace events: [route trace](reference/route-trace.md)
- Configuration layers: [configuration](configuration.md)
- Claude Code against non-Claude models: [claude-code](claude-code.md)
