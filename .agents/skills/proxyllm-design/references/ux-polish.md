# UX polish checklist

Use this on any console change and as the audit list for a dedicated polish
pass. It adds interaction and feedback rules to the visual system in
`design/DESIGN.md`; it does not replace it.

## P0 — interaction correctness

- **Keyboard path for every row that opens something.** Logs parent rows
  (`components/LogRow.tsx`) and Routing rule rows (`routes/Routing.tsx`) need
  `tabIndex=0`, `role="button"`, an accessible name, and Enter/Space handling.
- **No hover-only action.** Row actions must appear on `:focus-within` as well
  as hover (`styles/kit.css`); audit drag handles and edit/delete buttons.
- **Dialog contract.** `components/Overlays.tsx` traps focus, sets initial
  focus, closes on Escape, restores focus to the trigger, labels the dialog,
  and uses a scroll-lock counter for nested dialogs.
- **Named collapsed navigation.** Every `NavLink` in `components/Shell.tsx` has
  an `aria-label`/`title`; hidden text must not remove the accessible name.
- **Destructive actions confirm and name the object.** Models remove,
  ProviderType account remove and Routing rule remove use a shared
  `ConfirmDialog`; warn when the object is referenced, but do not block.
- **Feedback for every mutation.** Pending labels (`Refreshing…`, `Syncing…`,
  `Saving…`), `aria-busy`, and a `role="status"` outcome; a successful refresh
  reloads the data instead of waiting for the next poll.

## P1 — structure and feedback

- `EmptyState` renders an icon, a next action and `role="status"`; every empty
  route offers a path forward (Overview → setup, Providers → setup/account,
  Models/Routing/Keys → the relevant setup step).
- Provider cards are links (`Link`), not `<button>` containing block content;
  names wrap instead of truncating.
- `PageHead` survives loading and error states; early returns stay inside the
  page frame.
- Replace inline layout styles in touched routes with kit classes; do not
  rewrite all 139 at once.
- Models groups: the first group is open when there are several; the empty
  state links to Providers/Setup.
- Logs: filters (status, model, provider) and real sorting only when the
  backend supports `order`; otherwise split into a follow-up.
- Sidebar collapse is a desktop preference persisted in localStorage and is
  independent of the mobile drawer; navigation closes only the drawer.
- First-run login is a visible secondary action, not a hidden toggle.

## P2 — finish

- `.btn:disabled` has a visible disabled state.
- Help text stops using 12px `--neutral-500` for sentences; use
  `--neutral-400`.
- Actionable glyphs are inline SVG icons, not `✕`/`→`/`−`.
- Quota has its own nav icon (not the Providers plug).
- Mobile density works at 390×844; no horizontal page scroll.
- Decorative motion is removed; only state-carrying animations remain.
- Doc/test drift is corrected (token-sync test name, route lists).

## Definition of done for a console change

1. Every action has a keyboard path and a visible focus state.
2. Every destructive action confirms and names the object.
3. No hover-only affordance.
4. `PageHead` renders in loading, error and empty states.
5. Colours are tokens; status hues are discrete, never interpolated into a
   forbidden hue window.
6. The route is in `design-rules.spec.ts` and the relevant screenshot/empty
   spec; new blocking specs are added to `visual-regression.yml`.
7. `make webui-lint webui-typecheck`, `go test ./webui` and the assertion specs
   pass; baselines were refreshed through the CI workflow.
