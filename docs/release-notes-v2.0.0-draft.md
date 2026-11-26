# PROxyLLM v2.0.0 — release notes (draft)

> Draft. Do not publish before the maintainer tags the release.

## Breaking changes

v2.0.0 narrows the surface. Read [Upgrading from v1](release-migration.md)
before installing.

- **Removed provider types:** `gemini`, `openai-responses` (the generic
  provider), `openai-decisions`.
- **Removed client surfaces:** `/v1beta/*`, `/v1/systemone`, `/v1/decisions`,
  and the decision IR operation class.
- **M2 CLI matrix:** reduced to Claude Code, Codex and OpenCode plus a
  headless scenario layer; the nine extra adapters are gone.
- The `decision` capability flag is removed from model rows, the provider
  catalog and the console.

## Features

- **Anthropic OAuth (Teams/Enterprise)** — `anthropic-oauth` provider with a
  browser PKCE flow, loopback callback and pasted-callback fallback; a
  versioned risk notice gates the first sign-in; consumer subscriptions are
  documented as unsupported (blocked by Anthropic since January 2026).
- **Dynamic model discovery** — `display_name`/`description` on the
  Anthropic model list, and `GET /v1/model_catalog` (Codex-native shape) for
  Codex's `/model` picker.
- **Auto-installer** — `scripts/install.sh` configures Claude Code, Codex and
  OpenCode, with `--dry-run` and `--uninstall`.
- **Connect surfaces** — a lazy `/connect` page, a "Connect your CLI"
  disclosure in the key dialog, and a wizard step.
- **Test hardening** — golden-update guard (G1), header-preservation cell
  (G4), first-frame latency and disconnect leak cells (G5), contributor
  compose stack (G6), fake IdP (G7).

## Test matrix

- M1: 64 green pair cells plus the extras (web emulation, faults, Codex and
  Anthropic OAuth rotation, model-catalog schema, header/latency/disconnect).
- M2: Claude Code, Codex, OpenCode; every skip carries a manifest reason.
- Installer end-to-end, ShellCheck, docs links and the OpenAPI contract gate
  run in CI.

## Migration

[Upgrading from v1 to v2.0.0](release-migration.md): replace removed provider
rows with `custom-openai` (or delete them), fix routing rules that name a
removed type, and move clients off the removed surfaces.

## Maintainer checklist (not part of the published notes)

1. Merge the PR chain in order and confirm CI is green on `main`.
2. Tag `v2.0.0`; push tags; let Release publish the image and archives.
3. After the image exists, pin it: `docker/compose.yaml` (`v1.24.0` →
   `v2.0.0`), the `docker run` example in `docs/deployment.md`, and the
   README quickstart. Do **not** pin before the tag exists — the image would
   not resolve.
4. Move `SECURITY.md`'s supported line to 2.x only.
5. Smoke: `/healthz`, bootstrap, wizard, Connect a CLI, one request from an
   installed CLI.
6. Rename this file to the published release body (drop the draft/checklist
   sections).
