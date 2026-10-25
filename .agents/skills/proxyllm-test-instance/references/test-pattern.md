# Test pattern

`test-instance.sh test` records watermarks (`max(request_logs.id)`,
`max(request_child_logs.id)`, binary SHA, git HEAD) and writes everything under
`artifacts/<run-id>/`. Phases can be selected with `--phases 0,1,2`.

Every stimulus is a **real CLI process** against the local instance. The
harness does not call `/v1/messages` itself, start a stub upstream, or replay a
fixture. DB/log rows are evidence for assertions, not a second test surface.

## Phase 0 — preflight

- `GET /healthz` returns 200; the PID belongs to this instance; `.env` is 0600
  and the instance home 0700.
- The binary SHA is compared with `instance.json`. A mismatch (the running
  process predates the last rebuild) is a `warn`: rerun `restart --build`.
- Provider, capability-fallback and CLI inventory are read; a missing CLI is
  `skipped_missing` with its install command.

## Phase 1 — “Sag nur Hallo.” through every selected CLI

Each adapter runs in a fresh `<home>/clients/<cli>/<run-id>/<call>/` with a
minimal environment and its own config dir. Pass criteria:

- exit 0 and non-empty stdout;
- a new `request_logs` row after the watermark with `status = 200` and the
  expected protocol (`anthropic`, `openai`, `openai-responses`);
- Claude Code additionally shows an `alias` event and `model_resolved`;
- the real config canary is unchanged. Claude Code skips the canary when the
  harness itself runs inside Claude Code; isolation is `CLAUDE_CONFIG_DIR`
  plus a fresh HOME.

## Phase 2 — logs and database scan

- No `status >= 500` rows in the window at this point.
- No `level=error` or panic in the proxy log; warnings are triaged.
- `route_trace` version is 9 everywhere.
- One `GET /api/admin/logs` read exercises the console path.

## Phase 3 — a file tool through every CLI

Each CLI whose non-interactive mode has agent tools (`claude-code`, `codex`,
`opencode`, `goose`, `kimi`, `qwen`, `continue`, `cline`) gets a work dir with
`README.txt` containing `PROXYLLM-TOOL-CANARY-42`. The prompt asks for its
exact first line. Pass criteria:

- the CLI's result contains the canary (an exit-code-only pass is not enough);
- a new `status=200` request row exists in the window.

Aider's `--message` mode has no agent file tool, so it is
`skipped_unsupported`. Factory Droid needs `FACTORY_API_KEY` and is
`skipped_unsupported` without it.

## Phase 4 — web tools via Claude Code

Three search+fetch prompts and six mixed web+file prompts, all with
`--allowedTools WebSearch WebFetch Read`:

- A proxy-served call leaves `request_child_logs` rows `kind='emulated_tool'`,
  `to_ref` `web_search`/`web_fetch`, and trace `emulated_tool` events.
- The Claude Code session transcript is scanned. A **client** `tool_use` named
  lowercase `web_fetch`/`web_search`, or a tool result containing
  `No such tool available: web_fetch|web_search`, is a **fail** — the proxy
  handed the CLI a name it never declared.
- No child row and no error is `model_behavior`: Claude Code's on-demand
  ToolSearch executed its own `WebSearch`/`WebFetch`, or the model chose not to
  call a tool. It is not a proxy failure.

The mixed prompts are the regression for the emulation loop: a web call and a
foreign client call in the same model turn must be answered by the proxy, not
passed through.

## Phase 5 — the full Neon session

Run the user's prompt through Claude Code with web tools allowed and a 900 s
budget:

> Suche ein Bild einer guten und modernen UX Web App in Neon Farben. Zeige mir
> verschiedene Versionen, sodass du diese im Anschluss auf Basis darauf bauen
> kannst.

Assert: the CLI passes, at least one 200 row, **no** 5xx or 529 rows anywhere in
the window, and no web-tool unknown-name error in the transcript. At least one
`web_search` child row is `pass`; none is `model_behavior` (the model may choose
not to search). The final answer is saved truncated and redacted.

## Phase 6 — failover through a real CLI

Create `acc-unreachable-test` (`custom-openai`, base URL
`https://example.com:1/v1`), a manual model `failover-cli-test`, and a rule with
strategy `fill_first` and chain
`["acc-unreachable-test", "acc-ollama-cloud"]`. Run Codex with
`--model failover-cli-test`. The connection refusal is a real network failure,
not a stub. Expect `failed` → `failover` → a second `attempt` → `status=200`
with a `failover` event. Delete the fixtures afterwards.

## Phase 7 — vision through a real CLI

Generate a real 512×512 PNG with `make_png.py` and pass it through
`codex exec -i` on the known-blind `fallback-test` model. Expect a
`capability_fallback` with `capability: "vision"`, an `image_caption` child and,
when the model calls it, an `analyze_image` child. If the CLI cannot attach the
image or the model never calls the tool, report `model_behavior`/`fail` with
the observed output; never use a 1×1 PNG.

## Phase 8 — transport-timeout mapping, last

Temporarily set `upstream_response_header_timeout_ms=100` through the admin
settings KV and re-apply the provider (a settings write alone does not rebuild
the live clients). Run a Claude Code web prompt: the request rows must contain
a **504**, never a **500**. Restore the old value (or delete the key), re-apply
the provider, wait out the breaker cooldown (retry up to ~60 s) and require a
warm-up CLI call to return 200. This phase runs last because it deliberately
opens the breaker.

## Statuses

`pass`, `fail`, `warn`, `skipped_missing`, `skipped_unsupported`,
`model_behavior`, `client_config`.

Exit 0 when no `fail` or `client_config` exists; exit 1 when a check fails;
exit 2 only for usage or harness script errors. `--strict-upstream` is retained
for older reports but no phase currently emits `upstream`.
