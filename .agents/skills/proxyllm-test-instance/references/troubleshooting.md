# Troubleshooting

Classify before fixing. The report records the class; a proxy bug and an
upstream refusal need different actions.

| Symptom | Likely class | Check |
|---|---|---|
| CLI 401, no `attempt` event | `client_config` / `proxy` | key prefix in `.env`, `GET /v1/models`, request log auth row |
| CLI cannot connect to `127.0.0.1` | `client_config` | CLI env isolation; blanked `ANTHROPIC_*`/`OPENAI_*`; base URL shape (`/v1` only for OpenAI/Codex) |
| CLI writes to `~/.claude`, `~/.codex`, … | `client_config` (hard fail) | canary diff; the runner refuses a missing isolation variable. Claude Code's canary is skipped when the harness runs inside Claude Code (the parent writes those paths); isolation is `CLAUDE_CONFIG_DIR` + fresh HOME |
| 404 `not_found_error` with `model: <id>` | `proxy` (alias gap) | `claude-*` wildcard, auxiliary Haiku alias |
| Vision request returns 500 | `upstream` | real PNG, not 1×1; vision model availability |
| `capability_fallback` missing | `proxy` / config | model row `vision:false`, `pinned.vision`, live `capability-fallbacks` list |
| Model does not call `analyze_image` | `model_behavior` | tool was injected; trace `vision_tool`; try an explicit prompt |
| `emulated_tool` child missing | `model_behavior` / `proxy` | claim + no child + no error = the CLI's ToolSearch ran its own web tool; claim + no child + a client `tool_use` named lowercase `web_fetch`/`web_search` = the proxy leak, and the phase fails |
| CLI reports `No such tool available: web_fetch`/`web_search` | `proxy` | the emulation loop leaked an emulated family name; check the mixed-response and round-cap paths in `internal/routing/emulation.go` |
| 500 with `http2: timeout awaiting response headers` | `proxy` (mapping) | a transport timeout must be 504, not 500; check `mapRouteError` and `upstream_response_header_timeout_ms` |
| 529 `all providers in health cooldown` | `proxy` state | breaker after 3 counted failures or a 429/quota cooldown; the body names the reason and sets `Retry-After`; inspect the provider health state |
| Web fetch returns 202/403/0 bytes | `upstream` | child detail has URL/status/bytes; bot protection is not a proxy bug |
| Balance refresh 429 | `upstream` | Ollama allows 10 req/min; back off and retry |
| `setup` persists `vision_fallback` but live list is empty | config | never use `PUT /settings`; use `PUT /capability-fallbacks` |
| `restart` through a pipe hangs | tooling | run `restart` unpiped or `stop` then `start`; the setsid daemon keeps the pipe open |
| `sqlite3` command missing | environment | use `python3 -I .agents/skills/proxyllm-test-instance/scripts/db.py` |
| Admin login 401 and bootstrap 409 | secrets | `.env` has the wrong admin password for this DB; do not guess, reset the instance or restore the right `.env` |
| Model resolver finds no exact match | upstream catalog | inspect `GET /admin/models`; the resolver prints the top five candidates and stops |

## Where to look

- Proxy log: `<home>/logs/proxyllm.log` (`PROXYLLM_LOG_FORMAT=text`).
- Request rows: `python3 -I .agents/skills/proxyllm-test-instance/scripts/db.py --db <db> recent`.
- Child rows: `python3 -I .agents/skills/proxyllm-test-instance/scripts/db.py --db <db> children --kind emulated_tool`.
- Trace: `python3 -I .agents/skills/proxyllm-test-instance/scripts/db.py --db <db> trace --id <request-id>`.
- Config: `python3 -I .agents/skills/proxyllm-test-instance/scripts/db.py --db <db> config --key model_aliases`.
- Report: `artifacts/<run-id>/report.md`.

Never select `provider_tokens.token_encrypted`, `client_keys.key_hash` or
`webui_admins.password_hash`.
