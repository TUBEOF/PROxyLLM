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

Each phase is a separate CLI invocation with a fresh run directory. Prompts are
English only. After every phase the driver checks the CLI exit code, the
`request_logs` row (200, empty `error_redacted`, no `failed`/`rejected` in the
trace) and the proxy log window (no `ERROR`).

| Phase | Prompt | Model | Expected trace |
|---|---|---|---|
| `hello` | `Hello` | chat | 200, no error, no proxy `ERROR` |
| `vision` | `What is in this image? Describe it in one sentence.` | blind fallback model | `capability_fallback` with `capability="vision"` **and** `image_caption` |
| `web_search` | `Search the web for today's date and the current top news headline. Cite the source.` | chat | `emulated_tool` or `emulated_tool_attempt` with `to="web_search"` |
| `web_fetch` | `Fetch https://example.com and summarize the first paragraph.` | chat | `emulated_tool` or `emulated_tool_attempt` with `to="web_fetch"` |

The image is a real 512×512 PNG (a 1×1 image makes vision upstreams fail).
The primary model is deliberately blind, and the vision-fallback list points
at a vision-capable model, so the fallback must trigger.

**Hard rule:** if the request reached the proxy, the model is marked
non-native for the capability, and the expected fallback event is absent, the
phase is `fail` — not a warning. The fallback may only be absent when the
model natively serves the capability (the matrix marks such a cell
`native: true`) or when the CLI cannot perform the phase headlessly
(`skipped_unsupported`).

## CLI coverage

- **Hello:** all nine runnable adapters — Claude Code, Codex, OpenCode, Goose,
  Kimi, Aider, Qwen, Continue, Cline.
- **Image:** Codex (`codex exec -i`) today; other adapters are
  `skipped_unsupported` until their wire path can attach an image.
- **Web search/fetch:** Claude Code (`--allowedTools WebSearch WebFetch`) and
  Codex (`--search`) today; other adapters are `skipped_unsupported` until
  their adapter exposes a headless web tool.

Factory Droid stays `skipped_unsupported` (needs `FACTORY_API_KEY`).

## Local reproduction

The workflow is the source of truth. To reproduce against a disposable
instance:

```bash
python3 -I .github/live-cli/bootstrap.py --home /tmp/proxyllm-live \
  --chat-model nemotron-3-ultra --fallback-model deepseek-v4.1-flash:1017
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
Markdown table lists every CLI, its exact installed version and the result of
each phase. The Overall column is `PASS` only when all four phases pass,
`PARTIAL` when at least one passed but some are unsupported, and `FAIL` when
any phase failed. `skipped_unsupported` means the CLI has no headless path for
that phase. The release workflow runs this suite on the tagged commit and
appends that table to the GitHub release notes. The process exits non-zero
when any check is `fail`. The client key never enters the report, and
`run_cli.py` redacts the key from captured CLI output. Each CLI runs with a
fresh HOME/XDG/config/work directory and a canary snapshot of its real config
paths; a change to a real config path is a failure.

## Related

- Client matrix and invocation details: `.github/live-cli/adapters/*.json`
- Field mapping: [translation matrix](reference/translation-matrix.md)
- Trace events: [route trace](reference/route-trace.md)
- Configuration layers: [configuration](configuration.md)
- Claude Code against non-Claude models: [claude-code](claude-code.md)
