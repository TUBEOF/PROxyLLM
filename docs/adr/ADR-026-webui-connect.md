# ADR-026: Console "Connect a CLI" surfaces

- **Status**: Accepted
- **Date**: 2026-11-26

## Context

The console can create a client key, but it does not tell the operator what to
do with it. The path "key created → CLI running" is the largest usability gap
in the product: the operator must know each CLI's configuration format, where
its file lives, which model ids the proxy exposes and which discovery flags
exist. The v2.0.0 work order asks for guided setup in three places: the key
dialog, the setup wizard and a dedicated page.

Every surface must respect the console's rules: display-once key semantics,
design tokens only, lazy loading for the initial bundle budget, and no token
material after the one display moment.

## Criteria

- P0: a key is shown exactly once, in the dialog that creates it; every later
  surface references it instead of re-rendering it.
- P0: the generated snippets use the real console origin and the selected
  model, so copy-paste works.
- P0: initial bundle stays under 200 KB gzip; the new page is lazy-loaded.
- P1: design-system tokens, block layout, focus handling, no emoji.

## Options considered

| Option | Strengths | Weaknesses |
|---|---|---|
| Docs only | No UI work | The gap stays; users leave the console |
| A single page, no wizard integration | One place to maintain | Misses the moment right after key creation |
| Key dialog + wizard step + `/connect` page (this ADR) | Meets the operator at each step | Three surfaces to keep consistent |

## Decision

1. **Keys page:** after a key is created, the display-once dialog gains a
   "Connect your CLI" area with one block per CLI: environment variables, the
   installer one-liner pre-filled with the console origin and the new key, and
   a link to the full page.
2. **Setup wizard:** a new skippable `connect` step between `key` and `done`.
   The onboarding state vocabulary gains the id; no schema migration.
3. **`/connect` route** (lazy-loaded), with tabs for Claude Code, Codex,
   OpenCode and a generic OpenAI client. Each tab is a step list: install →
   base URL and key → model choice from the admin model list → the finished
   config file content to copy → discovery notes. The key is the placeholder
   `pk_...` unless the page was opened from the display-once dialog.

Snippets are generated from `window.location.origin`, never a hard-coded host.
Claude Code's id convention (an id containing `claude`/`anthropic` for its
gateway picker) is stated as a note with the alias recipe, not enforced.

No token is rendered after the display-once moment; regeneration is linked,
not shown. Colour, spacing and components come from `design/DESIGN.md` and
`brand.json`, not from literals.

## Consequences

- New route and components, a sidebar entry in the Access section, one
  onboarding step id and its admin-API vocabulary, and Playwright specs for
  the connect page, the key dialog and the wizard step.
- Visual baselines change and are refreshed through the
  `update-visual-baselines` workflow, never by hand.
- The Connect page is lazy-loaded like Logs, so the initial bundle budget is
  unaffected; the measured number is reported in the PR.
- The admin API does not need a new endpoint: models and key creation already
  exist.
