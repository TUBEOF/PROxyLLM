# ADR-024: Auto-installer `scripts/install.sh`

- **Status**: Accepted
- **Date**: 2026-11-26

## Context

Configuring a CLI against the proxy means editing a different format for each
one (`~/.claude/settings.json`, `~/.codex/config.toml`, `opencode.json`) and
knowing which model ids the proxy exposes. This is the step where a new
operator gives up. The v2.0.0 work order asks for one script that detects the
installed CLIs, writes their configuration and can undo it.

The script runs on the operator's machine, often with no Go toolchain and no
Node package installed by it. It must never install a CLI, never log a key and
never destroy an existing configuration.

## Criteria

- P0: no secret in output, logs or process arguments beyond what the user
  typed.
- P0: idempotent — a second run produces the same files; existing settings are
  preserved.
- P0: a failed probe aborts before writing anything.
- P1: one file, no runtime beyond bash and `curl`; `python3` or `jq` used when
  present for JSON.
- P1: reversible — `--uninstall` removes only what the script wrote.

## Options considered

| Option | Strengths | Weaknesses |
|---|---|---|
| Documentation only | No code | The gap stays |
| A Go/Python installer | Robust parsing | A second runtime to install; contradicts "download and run" |
| A bash script with optional `python3`/`jq` (this ADR) | Runs anywhere bash runs; reviewable | JSON handling needs a fallback; three config formats to merge |

## Decision

`scripts/install.sh`, a single bash script with `set -euo pipefail`, invoked as
`curl -fsSL …/scripts/install.sh | bash` or downloaded and reviewed first.

Behaviour:

1. Ask for the proxy URL (default `http://localhost:8080`) and the pk key
   (hidden input; env `PROXYLLM_URL` / `PROXYLLM_API_KEY` for non-interactive
   use) and probe `GET /v1/models`. On failure, stop before writing anything.
2. Detect `claude`, `codex`, `opencode` with `command -v`; offer only the ones
   found and print the official install link for the others. The script never
   installs a CLI.
3. Numbered selection (`1 3`, `all`, `none`) with a confirmation that lists
   the files about to be written, and a `--dry-run` mode that prints the same
   changes as a diff.
4. Per CLI: list models from the probe, choose a default, then write the
   CLI-specific options — Claude Code gets the gateway discovery flag and a
   small/fast model for `ANTHROPIC_DEFAULT_HAIKU_MODEL`; Codex gets a reasoning
   effort and the catalog URL; OpenCode gets generated `models` blocks.
5. Merge, never overwrite: TOML blocks, JSON objects and the `env` block are
   updated in place, the rest of the file is preserved, a
   `file.bak.<epoch>` backup is written, and every block the script owns is
   marked `# managed by PROxyLLM` (or the JSON equivalent). `--uninstall`
   removes only marked blocks.
6. Summarize per CLI: files written, default model, start command, how to
   verify the first request in the console.

JSON parsing prefers `python3`, then `jq`, and otherwise refuses with a clear
message rather than a brittle `sed` parser. The `/v1/models` probe is the only
JSON the script must understand.

Out of scope: installing CLIs, Windows (WSL is documented), package managers,
browser flows.

## Consequences

- A new `scripts/` directory, a ShellCheck gate in CI (ADR-027) and an
  installer job that runs the script end-to-end against a freshly built proxy
  in a temporary `HOME` and asserts the three files, idempotency and
  `--uninstall`.
- `docs/cli-setup.md` documents flags, environment variables, merge rules and
  limits; the README carries the one-line invocation.
- The script is versioned with the binary; a config-format change in a CLI
  means a script change, a test change and a doc change in one commit.
