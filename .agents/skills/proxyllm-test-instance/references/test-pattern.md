# Test pattern

`test-instance.sh test` records watermarks (`max(request_logs.id)`,
`max(request_child_logs.id)`, start time, binary SHA, git HEAD) and writes
everything under `artifacts/<run-id>/`. Phases can be selected with
`--phases 0,1,2`.

## Phase 0 — preflight

- `GET /healthz` returns 200; the PID belongs to this instance; the binary SHA
  matches `instance.json`.
- `.env` is 0600, the instance home 0700; `GET /v1/models` with the client key
  returns 200.
- `dry-run` prints the current providers, model count, settings keys, fallback
  lists and both resolved models; it is a sanity check, not a diff against
  `instance.json`.
- CLI inventory: installed/version; missing CLIs are `skipped_missing` with the
  install command.

## Phase 1 — “Hallo” through every selected CLI

Each adapter runs in a fresh `<home>/clients/<cli>/<run-id>/` with `env -i`,
a 120 s timeout, and a read-only prompt. Pass criteria:

- exit 0 and non-empty stdout;
- a new `request_logs` row after the watermark with `status = 200` and the
  expected protocol (`anthropic`, `openai`, `openai-responses`);
- Claude Code additionally shows an `alias` event and `model_resolved` in the
  trace, and its auxiliary Haiku calls also return 200;
- the real config canary is unchanged. Claude Code skips the canary when the
  harness itself runs inside Claude Code (the parent writes those paths);
  isolation is `CLAUDE_CONFIG_DIR` plus a fresh HOME.

## Phase 2 — logs and database scan

- No unexpected `status >= 400` rows in the window.
- No `level=error` or panic in the proxy log; warnings are triaged.
- No unknown `route_trace` version; no `failed`/`skipped` outside deliberate
  tests.
- Cross-check one page through `GET /api/admin/logs?after=<id>` so the console
  read path is exercised.

## Phase 3 — fallbacks

**3a Alias.** Anthropic `POST /v1/messages` with `model: "claude-sonnet-4-6"` and
`"Hallo"`. Expect `alias` → chat ID, `model_resolved`, `status=200`.

**3b Vision fallback.** Use the `fallback-test` alias (Nemotron, pinned
`vision:false`) with a real PNG and a targeted question. Expect:

- `capability_fallback` with `capability: "vision"` and a reason containing
  `substituted pre-dispatch`;
- an `image_caption` child (cache hit allowed) and, when the model calls it, an
  `analyze_image` child;
- `status=200`, no upstream capability gap.

If the model never calls `analyze_image`, that is `model_behavior`: the tool
was injected, so verify the trace and `vision_tool` setting before blaming the
proxy.

**3c Failover injection.** Starts a local HTTP server that returns 500 and
creates `acc-broken-transport` (`custom-openai`, base URL
`http://127.0.0.1:<port>/v1`), a manual model `fallback-chain-test`, and a rule
with strategy `fill_first` and chain
`["acc-broken-transport", "acc-ollama-cloud"]`. A request to
`fallback-chain-test` must record `failed` → `failover` → a second `attempt` →
`status=200`, with a `failover` child row. Clean up the fixtures and stop the
server afterwards. This phase needs `--allow-private-provider-urls`; otherwise
it is reported `skipped`.

## Phase 4 — web tools

Protocol-level requests with the client key:

```bash
curl -sS "$BASE/v1/messages" -H "x-api-key: $CLIENT_KEY" \
  -H 'Content-Type: application/json' -d '{
    "model": "proxy-chat", "max_tokens": 512,
    "tools": [{"type":"web_search_20250305","name":"web_search","max_uses":3}],
    "messages": [{"role":"user","content":"Suche nach Neon-UI-Designbeispielen."}]
  }'
```

`web_fetch` uses `{"type":"web_fetch_20250910","name":"web_fetch"}` and a URL
that appears in the user message (the emulator refuses a URL absent from prior
context). Pass criteria:

- `request_child_logs` rows `kind='emulated_tool'`,
  `provider_id='acc-ollama-web'`, `to_ref` `web_search`/`web_fetch`, detail
  `query "…" → N result(s) via acc-ollama-web` or
  `fetched <url> via acc-ollama-web`;
- trace `emulated_tool_attempt` → `emulated_tool`;
- request `status=200`.

A bot-protected URL may return 202/403/0 bytes inside an upstream 200 body;
assert the failure is visible in the child detail and classify it `upstream`.

CLI level: Claude Code without `--bare` (a bare run exposes only
`Bash`/`Edit`/`Read` and never offers the web tools), with
`--allowedTools WebSearch WebFetch` and a prompt that forces search then fetch.
Codex `--search` is best effort; other CLIs are client-side and are marked
`skipped_unsupported`. A 200 response with no emulated child is
`model_behavior`, not a proxy failure.

## Phase 5 — vision ×3

Generate three real PNGs (512×512, >1 KB, different colors/layout) with
`make_png.py`. Run three Anthropic requests against `fallback-test`, each with
one image and a different question. Per run: `status=200`,
`capability_fallback`, `image_caption`; at least one `analyze_image` across the
three. Cache hits are allowed and recorded. Never use a 1×1 PNG.

## Phase 6 — full Neon prompt

Run the user's prompt through Claude Code with WebSearch/WebFetch allowed:

> Suche ein Bild einer guten und modernen UX Web App in Neon Farben. Zeige mir
> verschiedene Versionen, sodass du diese im Anschluss auf Basis darauf bauen
> kannst.

If Claude Code cannot run or its run fails (including `client_config`), send
the same prompt as an Anthropic request with both server tools. Assert at least
one `web_search`; every fetch attempt is reported with URL/status/bytes as
`upstream`; the final answer is saved truncated and redacted. The check is tool
flow and visibility, not whether the model produced UI code.

## Statuses

`pass`, `fail`, `warn`, `skipped`, `skipped_missing`, `skipped_unsupported`,
`upstream`, `model_behavior`, `client_config`.

Exit 0 when no `fail` or `client_config` exists; exit 1 when a check fails;
exit 2 only for usage or harness script errors. `--strict-upstream` upgrades
`upstream` to failure. Never hide an upstream failure: keep it in the summary
and print a banner.
