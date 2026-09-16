# First-Build Verification Report — WebUI Black Screen + Docker Build

**Date**: 2026-09-15
**Scope**: the two blocking first-build defects found by the first real Docker
test of the whole product, plus the layered verification run that proves them
fixed. The WebUI had **never rendered** before this report — both defects date
back to the original WebUI commit (`560541a`) and the Dockerfile commit, and
neither test layer caught them because CI has never executed (the repo has no
git remote; see phase-3 report §9).

---

## 1. Defects

### D-1 — SPA crashes on every route (black screen) — FIXED

`webui/src/main.tsx` never mounted `AuthProvider`. Every route reaches
`useAuth()` (`RequireAuth`, `Login`, `Shell`, the `useAdminData`/`useMutation`
hooks), which throws `'useAuth must be used inside AuthProvider'` when the
context is absent — React unmounts the whole tree into an empty `#root` on the
near-black page background (`--bg: #111821`). Symptom: a completely black
page on every URL.

**Fix**: `AuthProvider` wraps `BrowserRouter`/`App` in `main.tsx`.

### D-2 — Docker build of the Go binary cannot succeed — FIXED

`docker/Dockerfile`'s build stage copied `go.mod`, `cmd/`, `internal/` and
the webui-stage `dist/` — but not `webui/embed.go`, the embed package's only
Go source. The `github.com/proxyllm/proxyllm/webui` import (`server.go:17`)
had no Go files in the build context, so `go build ./cmd/proxyllm` failed
inside the image build. Any container image that ever ran was therefore not
built from the committed Dockerfile path.

**Fix**: `COPY webui/embed.go webui/` added to the build stage.

### D-3 — Modal unusable: backdrop covers it, swallows all clicks — FIXED

The kit defines `.modal-backdrop` as `position:fixed; z-index:200`, but
`.modal` itself is unpositioned (`kit.css:355-356`). The kit demo nests the
modal inside the backdrop, so the design never hit this — but `Overlays.tsx`
renders the pair as siblings, so the backdrop covered the modal and every
click landed on the backdrop (which closes the modal). Symptom: the client
key "Create" button could not be clicked at all. The drawer counterpart was
correct (`.drawer` z-160 over backdrop z-150); the token layer even defines
`--z-modal: 210` that was never applied.

**Fix**: `.modal { position: relative; z-index: var(--z-modal); }` in
`app.css` (webui-local layer; the kit file stays verbatim per K-1).

### D-4 — Brand link rendered in off-palette UA purple — FIXED

See §3 (found during Layer 3 calibration): `a.sidebar-brand` had no color
rule and fell back to Chromium's dark-mode UA link color.

### Investigated and ruled out

- The committed `webui/dist/index.html` placeholder — never shipped; the
  Dockerfile's node stage builds the real bundle and overwrites it.
- SPA serving (`internal/server/webui/spa.go`), CSP, Vite `base`, the
  `fs.Sub(embedui.Dist, "dist")` projection — all correct.
- All admin API paths used by the SPA are routed (`adminapi.go:84-126`);
  `types.ts` mirrors the DTOs.
- CSRF flow (body token on login/bootstrap, `X-CSRF-Token` re-issue on every
  authenticated response) — consistent between server and `api.ts`.
- `if (ok) reload()` mutation pattern in all routes — correct: the admin API
  answers mutations with `200 + {"ok": true}`, not 204 (verified at
  `revokeKey`, `adminapi.go:597-603`).

---

## 2. Verification run (layered)

### Layer 0 — build gates

| Gate | Result |
|---|---|
| `npm run build` (tsc strict + vite) | PASS — initial load ≈ 65 KB gzip (budget: 200 KB) |
| `go build ./cmd/proxyllm` | PASS |
| `go test ./cmd/... ./internal/... ./webui` | PASS (full suite, exit 0) |
| Embedded bundle check | local binary and Docker image both embed the fixed `index-Cu-A3mlM.js` bundle |

### Layer 1 — Docker path end-to-end

- `docker build -f docker/Dockerfile -t proxyllm:fix .` — PASS (previously
  failed in the build stage; now completes through the distroless final stage).
- `docker run -p 18080:8080` → `GET /healthz` → `{"status":"ok"}` — PASS.
- `GET /` serves the **real** Vite `index.html` referencing
  `/assets/index-Cu-A3mlM.js` (not the committed placeholder) — PASS.
- `GET /api/admin/settings` without a session → uniform `401` — PASS.
- Build-time lint note: distroless `ENV PROXYLLM_MASTER_KEY_PATH` triggers
  Docker's `SecretsUsedInArgOrEnv` heuristic; the value is a **path**, not a
  secret — noted, not a defect.

### Layer 2 — visual verification in a real browser

Playwright Chromium against the running Docker container (not a dev server):
login render, first-run bootstrap through the real form, all seven routes,
provider-create and client-key-create interaction smoke, logout.

*(Results recorded below after the run — see §3.)*

### Layer 3 — assertion-only design rules

`webui/e2e/design-rules.spec.ts` (dark-only, tokens-only, no-emoji on all
routes) runs against the fixture server without needing CI-only screenshot
baselines; the fixture server binary was rebuilt with the fixed bundle
(`go build -o webui/e2e/fixturesrv-bin.exe ./webui/e2e/fixturesrv`).

*(Results recorded below after the run — see §3.)*

---

## 3. Layer 2/3 results

### Layer 2 — visual verification (Playwright Chromium → Docker container)

15/15 steps passed against the final image (`index-xze-cR2H.js`, all fixes):

- login render (glass card, dark theme), first-run bootstrap → shell
- all 7 routes render with sidebar/topbar and honest empty states
- interactions: provider create, model register, routing rule create,
  client-key display-once modal (readable, clickable), key list refresh,
  sign-out → login
- screenshots visually read against the UpGuard design: token colors,
  no black screen, correct stacking

Note: the first interaction attempt with `base_url: https://example.invalid`
was **correctly** rejected by the server's SSRF guard (unresolvable host);
re-run with a resolvable host — the guard works as documented.

### Layer 3 — assertion-only design rules (against the fixture server)

3/3 passed (`dark-only`, `tokens-only`, `no emoji` on all 7 routes).

Two test-code defects in `design-rules.spec.ts` were found and fixed — the
suite had never executed (same reason as D-1/D-2: no CI):

- T-1: the tokens-only check referenced the loop variable `route` inside a
  `page.evaluate` callback (not serialized into the browser context) →
  `ReferenceError` on every run. Fixed by passing it as an argument.
- T-2: the color palette was enumerated as `rgb()` values, but Chromium
  serializes the kit's `oklch()` tokens as `oklch()` and their translucent
  composites as sRGB — the original list matched nothing (the check was
  vacuous even if T-1 hadn't crashed it). Replaced with a robust
  classification: token hue-family windows in the color's native space
  (oklch windows for oklch values, HSL windows for sRGB composites), plus
  animation/transition freezing in the test setup so `status-ping`
  mid-frame interpolations don't pollute computed styles.

Finding during T-2 calibration, fixed as **D-4**: `.sidebar-brand` is an
`<a>` whose color came from the UA dark-mode link style
(`rgb(158,158,255)`, off-palette purple) because only its child `.name`
span carried a color rule. Fixed in `app.css` (webui-local layer, kit stays
verbatim): the anchor now inherits `var(--fg)`.

---

## 4. Deliberately not covered

- `make test-race`, govulncheck, gitleaks, CodeQL — CI-only (no CGO/gcc
  locally); no Go code changed in this fix set.
- Screenshot baselines — CI-only by design (V-3/V-4); the local run uses
  assertions and manual visual reads, never baseline regeneration.
- The Playwright screenshot suites themselves (routes × viewports) still
  require CI baselines; nothing about that contract changed here.

## 5. Commits

| Commit | Scope |
|---|---|
| `fix(webui)` | mount AuthProvider (D-1) |
| `fix(docker)` | copy embed.go into the build stage (D-2) |
| `fix(webui)` | modal stacking over backdrop (D-3) + brand link color (D-4), design-rules test fixes (T-1/T-2), verification report |