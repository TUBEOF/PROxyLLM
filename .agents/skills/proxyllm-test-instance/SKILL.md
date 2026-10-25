---
name: proxyllm-test-instance
description: Use when standing up, configuring, testing or tearing down a local PROxyLLM instance — including the multi-CLI live test pattern (Claude Code, Codex, OpenCode and other OpenAI/Anthropic-compatible CLIs), fallback/vision/web-tool verification, and the machine-readable test report.
---

# PROxyLLM test instance

A reproducible, disposable instance that proves the whole request path against
real upstreams and real client CLIs. The stable procedure and invariants live
in `docs/test-instance.md`; this skill is the map and the executable harness.

## Quickstart

```bash
cd <repo>
.agents/skills/proxyllm-test-instance/scripts/test-instance.sh init \
  --home ~/proxyllm-test-instance --key-file ~/.config/proxyllm/keys.env \
  --chat-model deepseek-v4.1-flash:1017 --fallback-model nemotron-3-ultra
.agents/skills/proxyllm-test-instance/scripts/test-instance.sh start --build
.agents/skills/proxyllm-test-instance/scripts/test-instance.sh setup
.agents/skills/proxyllm-test-instance/scripts/test-instance.sh test
```

`init` asks for the instance path, the Ollama key source and the models unless
the flags above are given. The key is written to `<home>/.env` (0600) and is
never printed. `test` writes `artifacts/<run-id>/report.json` and `report.md`.
Add `--allow-private-provider-urls` to `init` to enable the local-500 failover
injection; without it that phase is reported `skipped`.

## Read the right file

| Question | Read |
|---|---|
| Layout, setup order, invariants, test phases, status semantics | `docs/test-instance.md` |
| Which CLI, how it is isolated and invoked, install command | `references/cli-adapters.md` |
| Exact assertions and evidence per phase | `references/test-pattern.md` |
| Upstream vs proxy vs client vs model failure | `references/troubleshooting.md` |
| UX/design checks for the console | `references/ux-checks.md` and `proxyllm-design` |
| Endpoints, JSON shapes, CSRF | `docs/api/admin.md` |
| Config keys and layers | `docs/configuration.md` |
| Claude Code env and aliases | `docs/claude-code.md` |

## Non-negotiables

- **Ask before installing a CLI.** Missing CLIs are `skipped_missing` with the
  install command; never install silently.
- **No permanent CLI changes.** Every run gets a fresh HOME/XDG/config dir
  under `<home>/clients/`; the real config paths are checksummed before and
  after. Any change is a hard failure.
- **Secrets never reach argv, logs or the report.** Admin bodies are JSON
  request bodies; the runner receives the client key through
  `PROXYLLM_TEST_CLIENT_KEY`, not a command-line flag. `.env` and the cookie
  jar are 0600; the report contains prefixes only.
- **Fallbacks go through `PUT /api/admin/capability-fallbacks`.** Writing
  `vision_fallback` through `PUT /api/admin/settings` persists but does not
  reload the executor — the live list stays stale.
- **Pin the fallback-test model blind.** `vision:false` plus
  `pinned.vision:true`; the pre-dispatch substitution only runs when every
  chain entry is known blind.
- **Real PNGs only.** A 1x1 PNG makes the vision upstream return 500; use
  `scripts/make_png.py`.
- **Classify failures honestly.** An upstream 202/403/0-byte fetch is
  `upstream`, not a proxy bug; a missing emulation event is `proxy`; a model
  that ignores an offered tool is `model_behavior`.
- **No runtime state in the repo.** Instances live outside the repo or under
  the gitignored `data/` directory.

## Scripts are new in this repo

`scripts/` is a new convention: dependency-free bash + `python3 -I`, idempotent,
secret-safe, runnable from a fresh checkout. Keep that contract for every
script added here.
