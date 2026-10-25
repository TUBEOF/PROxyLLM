# UX and design checks

The console's design system lives in `design/DESIGN.md`, `design/brand.json`
and `design/colors_and_type.css`; the agent-facing rules are in the
`proxyllm-design` skill. This file is the live-test checklist, not a second
copy of the design system.

## Every run

- Sign in and open every route: Overview, Quota, Providers, a provider detail,
  Models, Routing, Logs, API keys. No console error, no unhandled rejection.
- Loading, empty and error states keep the page frame (`PageHead`) and offer a
  next action; no dead end on a fresh instance.
- Every interactive row opens with Enter/Space; every hover-only action is
  reachable by keyboard; dialogs trap focus, close on Escape and restore focus.
- Destructive actions ask for confirmation and name the object.
- Colors come from tokens; one chromatic accent; status tones are discrete
  (green / orange / red); no emoji; inline SVG icons only.
- 390×844: one column, no horizontal page scroll, no variable-length text in a
  flex-wrap line above content.
- 200% zoom and reduced motion: layout holds, animations neutralised.
- Visual baselines: only via the `update-visual-baselines` workflow; never
  commit locally rendered screenshots.
- Bundle: initial load < 200 KB gzip; new routes are lazy.

## Usage and quota surfaces

- Every provider account has a card; web-tool accounts are grouped and labelled
  by provider type.
- A usage bar shows the **remaining** share, shrinks as quota is consumed and
  moves green → orange → red; the numeric reading and reset countdown are
  visible and accessible (`role="meter"`, `aria-valuetext`).
- The card says when it was last updated (`fetched_at`) and whether a refresh
  was skipped because the cached reading is current.
- A cooldown badge names the deadline; an upstream reset is shown as a reset,
  not as a breaker cooldown.
- Firecrawl shows credits; Ollama shows session/weekly or monthly windows;
  Codex shows header-derived windows after traffic; a provider without a usage
  source says so honestly.

## Definition of done for a console change

1. Interactive semantics and keyboard path exist for every action.
2. Destructive action confirms and names the object.
3. No hover-only affordance.
4. Page frame survives loading, error and empty states.
5. Tokens only; status hues are discrete.
6. The route is in `design-rules.spec.ts` and the relevant screenshot/empty
   spec.
7. `make webui-lint webui-typecheck`, `go test ./webui` and the assertion specs
   pass; baselines were refreshed through CI.
