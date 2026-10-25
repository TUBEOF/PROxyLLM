# ADR-020: First-run onboarding wizard

## Context

A fresh PROxyLLM installation has an admin but no providers, models or client
keys. The console's empty states described the problem but offered no guided
path: the operator had to discover Providers, Models, Routing, the web-tool
accounts and API keys in the right order, and a first-time user could not tell
whether a provider actually worked until a client failed.

## Decision

- **A dedicated `/setup` page, not a modal.** The wizard carries provider
  forms, a device sign-in, a display-once key and an optional end-to-end check;
  a modal would fight focus and scrolling. The route is lazy-loaded so the
  initial bundle stays under budget.
- **Progress lives in the config KV** under `onboarding_state`:
  `{version, completed[], dismissed, completed_at, updated_at}`. No migration.
  A dedicated `GET/PUT /api/admin/onboarding` validates the closed step
  vocabulary (`provider`, `model`, `web_tools`, `fallbacks`, `routing`, `key`,
  `done`) and returns the live counts.
- **The counts are the truth.** `needs_setup` is recomputed from the provider,
  model and client-key lists on every read; a dismissed flag hides the prompt
  where the instance is configured. Deleting a provider later re-opens the
  Overview card even if the wizard once completed.
- **Entry points:** the first admin is redirected to `/setup` immediately after
  bootstrap; existing admins get an Overview card and a Providers action. There
  is no global redirect gate — an operator can always navigate away and use the
  console directly.
- **Steps and connection tests:**
  1. Provider: type settings, an API-key or OAuth account, model sync, then a
     real model test.
  2. Web tools: Firecrawl or Ollama Cloud Web Tools; the credential check is
     the usage probe (key + usage read), not a synthetic search.
  3. Fallbacks (optional): vision model and the web-tool account in the live
     capability-fallback lists.
  4. Aliases: `claude-*` / `gpt-*` / family presets merged into
     `model_aliases`, targeting the tested model.
  5. Client key: display once, copy, never persisted in the wizard state.
  6. Done: finish (sets `completed_at`) or skip (sets `dismissed`).
- **Every step is skippable**, and progress is written after each step, so an
  interrupted setup resumes with the completed markers intact.

## Consequences

- The wizard reuses the existing admin endpoints; no new provider, model or
  routing code was needed. The only new persistence is the small KV state.
- The Overview card and the Providers action are the durable entry points; the
  post-bootstrap redirect is a one-time convenience.
- The `needs_setup` counts deliberately require an LLM provider, a model and a
  client key; routing rules are optional because implicit routes exist.
- A future onboarding step (for example a first request) adds a state id and a
  UI step, not a schema migration.
