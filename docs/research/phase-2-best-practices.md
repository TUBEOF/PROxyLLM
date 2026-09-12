# Phase 2 Best-Practice Research Pass

Per plan §11 (binding doctrine): live, cited research before every phase. All
facts below were researched live via Firecrawl on **2026-09-12** (source URL +
access date per fact), mirroring phase-1-best-practices.md. Topics per Analysis F
Phase 2 examples: React embedding patterns, session auth (OWASP), strict CSP with
an embedded Vite SPA, SQLite-in-server admin-API practices, Playwright visual
regression, bundle budget, UpGuard kit integration.

---

## 1. go:embed — embedding a Vite build into the Go binary

Source: https://pkg.go.dev/embed (accessed 2026-09-12)
Corroborated by the Flipt engineering post "Embedding Our New React UI in Go"
(https://blog.flipt.io/embedding-react-in-go, accessed 2026-09-12)

| # | Fact → PROxyLLM consequence |
|---|---|
| E-1 | Without a prefix, patterns do NOT match files whose names begin with `.` or `_`. Use `//go:embed all:dist` to include everything (Vite builds emit `_`-prefixed assets only rarely, but the Flipt post documents exactly this class of failure: underscore-prefixed Next.js assets silently 404'd after embedding). → the webui embed directive uses `all:`. |
| E-2 | Patterns are relative to the package directory and must use forward slashes, even on Windows; patterns must not contain `.` or `..` elements, may not match outside the module or into `vendor/`. → keep the embed declaration in a package whose directory directly contains `dist/`. |
| E-3 | `embed.FS` implements `fs.FS`, so it plugs directly into `http.FileServer(http.FS(...))`/`http.FS` adapters. → no custom file plumbing; the spa handler decorates the stock file server only for the SPA fallback decision. |
| E-4 | Flipt: three SPA-routing strategies with an embedded single-binary app — (1) serve index.html for all HTML paths, (2) file-system-routing fs.FS decorator, (3) hash-based routing (single index.html, zero server-side route knowledge). Flipt chose hash routing for their vanilla-React app because it needs no Go-side changes. → PROxyLLM decides in §5 (this document): history-API routing with a Go-side index fallback, because plan §9 specifies real paths and CSP/UX reasons favor it. |

## 2. OWASP Session Management Cheat Sheet — admin session design

Source:
https://cheatsheetseries.owasp.org/cheatsheets/Session_Management_Cheat_Sheet.html
(accessed 2026-09-12)

| # | Fact → PROxyLLM consequence |
|---|---|
| S-1 | Session IDs need ≥ 64 bits of entropy from a CSPRNG; self-rolled IDs should use a CSPRNG with ≥ 128 bits and be unique. → 256-bit (32 random bytes) session IDs via `crypto/rand`; hashed (SHA-256) before storage so the store never holds the raw bearer. |
| S-2 | Session ID name should be generic (avoid framework fingerprinting). → cookie name `id` (per the cheat sheet's own example), not `proxyllm_session`. |
| S-3 | `__Host-` cookie prefix RECOMMENDED for session IDs: requires `Secure`, no `Domain`, `Path=/` — binds the cookie to the origin and blocks subdomain forgery + HTTPS-downgrade cookie planting. → cookie `__Host-id`. Caveat: `__Host-` requires Secure, which requires TLS. PROxyLLM serves plain HTTP on the LAN by default (single-service constraint, TLS termination external per plan §7); on plain HTTP the browser rejects the `__Host-` prefix. → set `__Host-id` when the request arrived over TLS (or behind a TLS-terminating proxy that forwards the scheme), else plain `id` with `Secure` omitted — the attribute set is chosen per-request from the arrival scheme, and this split is documented and tested. |
| S-4 | `HttpOnly` (mandatory, blocks XSS cookie theft) + non-persistent session cookie (no `Expires`/`Max-Age` → dies with the browser). → both always. |
| S-5 | `SameSite=Strict` preferred (or Lax); never rely on the browser default. → `SameSite=Strict` for the admin session: the WebUI is a single-origin admin app; no cross-site entry flows exist (OAuth happens server-side; the browser flow callback is a separate, one-shot authorization response the SPA never needs to accompany). |
| S-6 | Never accept a session ID the server did not generate (strict mode): reject unknown IDs with a freshly generated one + a suspicious-activity log line. → sessions are server-side rows keyed by the hashed ID; an unknown cookie simply fails auth (401) — plus a rate-limited warning log. |
| S-7 | Session ID must be meaningless; state lives server-side. → sessions table stores created_at, last_seen_at, admin_id, expiry; the cookie carries only the random ID. |
| S-8 | Renew the session ID after any privilege change (login = privilege change). → new session ID issued at every successful login; old IDs for the admin are invalidated on explicit logout (server-side delete, plus client-side cookie clearing per the cheat sheet's logout recipe). |
| S-9 | Idle timeout (2–5 min high-value; 15–30 min low-risk) + absolute timeout (4–8 h full-day use) enforced SERVER-side. → admin session defaults: 30 min idle, 12 h absolute; both checked against DB timestamps on every request, never client-side. |
| S-10 | Expiration/invalidation must act on both sides: client (clear cookie value, `Max-Age=0` past-dated) and server (delete the session row). → logout deletes the row AND clears the cookie. |
| S-11 | Considerations for multiple cookies: verify all cookies and enforce relationships before granting access. → we set exactly one session cookie; no pre-auth anonymous cookie exists (the login is the first cookie-setting request). |

## 3. OWASP CSRF Prevention Cheat Sheet — admin API CSRF design

Source:
https://cheatsheetseries.owasp.org/cheatsheets/Cross_Site_Request_Forgery_Prevention_Cheat_Sheet.html
(accessed 2026-09-12)

| # | Fact → PROxyLLM consequence |
|----|---|
| C-1 | Signed Double-Submit Cookie RECOMMENDED: token = HMAC(serverSecret, session-dependent value + random anti-collision value); explicitly bind to the session (session-independent tokens remain vulnerable to cookie injection). Naive double-submit DISCOURAGED (bypassable via subdomain cookie injection). → CSRF token = HMAC-SHA-256(master key, sessionID ‖ random 128-bit); the random part rides an HttpOnly-free cookie (so the SPA can echo it in a header), the HMAC is server-verified. |
| C-2 | HMAC CSRF token components: session-dependent value (never static user email/ID), secret key stored server-side, random anti-collision value per token. Timestamps in CSRF tokens are a misconception — a new session gets a new token, no expiry needed. |
| C-3 | Custom request headers for AJAX/API: a custom header (e.g. `X-CSRF-Token`) forces a CORS preflight for browser requests, so its presence proves the request is not cross-site-forged. Well suited to JSON APIs. → the admin API requires `Content-Type: application/json` (a non-simple content type, C-4) AND validates the `X-CSRF-Token` header against the HMAC; both layers in place. |
| C-4 | Disallowing simple content types: JSON APIs must NOT accept `text/plain` (or `application/x-www-form-urlencoded`/`multipart/form-data`) — those are "simple" request content types browsers send cross-site without preflight. → admin API accepts `application/json` only on all state-changing endpoints. |
| C-5 | SameSite is defense-in-depth, never a replacement for a CSRF token: Lax does not block safe methods (any state-changing GET defeats it), SameSite is registrable-domain-scoped, top-level-navigation tricks exist, client-side CSRF unaffected. → we ship the token check regardless of S-5's SameSite=Strict; no admin endpoint mutates state via GET (all mutations are POST/PUT/PATCH/DELETE). |
| C-6 | Origin/Referer verification: if Origin present, it must match the target origin exactly (trailing-slash/full-origin match, so `example.org.attacker.com` fails). → admin middleware additionally checks Origin/Referer on state-changing requests when present and rejects mismatches (403), as a third layer. |

## 4. Strict CSP with an embedded Vite SPA (Analysis G mandate)

Sources: https://stackoverflow.com/questions/79830548/... (accessed 2026-09-12);
https://www.stackhawk.com/blog/react-content-security-policy-guide-what-it-is-and-how-to-enable-it/ (accessed 2026-09-12);
Auth0 SPA CSP article (https://auth0.com/blog/deploying-csp-in-spa/, accessed 2026-09-12)

| # | Fact → PROxyLLM consequence |
|---|---|
| P-1 | The React Router v7 strict-CSP friction exists ONLY in RR7 Framework Mode (`root.tsx` + `<Scripts />` renders an inline ES module — nonce-exempt only with a per-response nonce; the SO thread has no clean answer, 2025-11). PROxyLLM uses classic Vite SPA mode: static `index.html` + `<script type="module" src="/assets/main-<hash>.js">` — zero inline scripts in the built app. → `script-src 'self'` is achievable without `unsafe-inline` or nonces. |
| P-2 | Vite `build.assetsInlineLimit: 0` disables asset inlining (no base64 `data:` URIs injected into HTML/CSS) → keeps `img-src`/`font-src` clean `'self'` without `data:` exceptions. Small font/asset files otherwise become `data:` URIs. → set `assetsInlineLimit: 0` in vite.config.ts; self-hosted fonts are emitted as real files. |
| P-3 | Auth0 strict-SPA pattern: `script-src 'self'` + all scripts as external hashed files. A Vite module bundle satisfies this directly. → our CSP: `default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; font-src 'self'; connect-src 'self'; object-src 'none'; base-uri 'self'; form-action 'self'; frame-ancestors 'none'`. `img-src data:` is kept for inline SVG placeholder icons if needed; design rule "inline SVG" means SVG components, not `data:` URIs, so `'self'` may suffice — verify during implementation and tighten to `'self'` only if no `data:` images ship. |
| P-4 | React inline `style="..."` attributes on components can require `style-src-attr 'unsafe-inline'` unless the app avoids runtime-generated inline styles. → UpGuard kit is class-based CSS (kit.css), no CSS-in-JS; the app avoids runtime inline `style` attributes (kit classes only). If an unavoidable dynamic positioning style appears (drawer/modal), scope it via `style-src-attr 'unsafe-inline'` documented in ADR — checked at implementation. |
| P-5 | `connect-src 'self'` is sufficient: the SPA talks only to the same-origin `/api/admin/*` endpoints. No third-party fetches exist (fonts self-hosted, no CDN per design rules). |

## 5. SPA routing decision: history routing with Go-side index fallback (closes E-4)

Analysis (criteria → options → decision, per plan §3 doctrine):

**Criteria**: real, shareable URLs for the 8 routes (plan §9); strict CSP compatibility; no duplicated route knowledge (8 SPA routes + Go fallback list); deep links work on hard refresh; Playwright visual tests target real paths.

**Options**:
1. *Hash routing* (Flipt's choice): zero Go-side changes, single index.html served everywhere. Rejected: URLs like `/#/models` are not real paths — plan §9 specifies `/models` etc.; hash routes leak into copy/docs as gateway-style URLs and complicate the visual-test deep links; the SPA owns all routing with no server awareness.
2. *History routing with a Go SPA fallback*: Go serves `index.html` for GET requests that accept HTML and match no file and no `/v1`/`/api` path. Standard `try_files $uri /index.html` equivalent (nginx SPA pattern); the kit's own index.html launcher assumes real paths.
3. *fs.FS decorator with per-route file resolution* (Flipt option 2): rejected — reimplements route knowledge in Go for zero benefit at 8 routes.

**Decision: option 2.** Concretely: the embedded FS serves `/assets/*` files directly; any other GET that maps to no file, is not under `/v1`/`/api`, and `Accept: text/html` → `index.html` (200, not 404 — the SPA renders the route client-side and shows its own 404 view for unknown paths). This matches the kit's preview architecture, keeps `script-src 'self'` trivially (one index.html, external hashed bundles), and deep links (`/providers/refresh` after OAuth callback pages) work on hard refresh. Rejected alternatives documented here per doctrine.

## 6. SQLite-in-server admin-API practices

Sources: https://riverqueue.com/docs/sqlite (accessed 2026-09-12);
https://www.sqlite.org/pragma.html (referenced); berthub.eu SQLITE_BUSY article
(https://berthub.eu/articles/posts/a-brief-post-on-sqlite3-database-locked-despite-timeout/,
accessed 2026-09-12)

| # | Fact → PROxyLLM consequence |
|---|---|
| Q-1 | WAL mode: readers do not block the single writer and vice versa — "paramount" under concurrency; without it SQLITE_BUSY is inevitable. → already live in `internal/storage/store.go:38` (`journal_mode(WAL)`), confirmed by the store tests. |
| Q-2 | Single connection pool (`SetMaxOpenConns(1)`) is an effective SQLITE_BUSY mitigation for SQLite-under-a-server; keep transactions short-lived (single-digit ms) because an open transaction monopolizes the pool. → already live (`store.go:48`, ADR-003); the admin API introduces no long transactions: each handler is one short write. |
| Q-3 | `BEGIN IMMEDIATE` avoids deferred-transaction upgrade deadlocks; with a single connection it is largely unnecessary. → no change; documented as confirmed by research. |
| Q-4 | The storage layer already implements every recommended practice (WAL + busy_timeout(5000) + foreign_keys(1) + single connection). → **research topic closed as "confirmed, implemented"** — the Phase 2 admin API builds on the existing store without new concurrency primitives. |

## 7. Playwright visual regression (Analysis F mandate)

Sources: https://oneuptime.com/blog/post/2026-01-30-... (visual-regression guide,
accessed 2026-09-12); testquality.com and bug0.com Playwright guides (accessed
2026-09-12); Playwright docs (playwright.dev) via the above.

| # | Fact → PROxyLLM consequence |
|---|---|----|
| V-1 | `toHaveScreenshot()` pixelmatch-diffs full-page screenshots against baselines; `maxDiffPixelRatio: 0.001` is a proven strict threshold; `animations: 'disabled'` and `caret`/`scale: 'css'` stabilize rendering. → CI suite config pins these. |
| V-2 | Mask dynamic content: `mask: [page.locator('.kpi-value')]` + `maskColor` for live counters/timestamps; freeze time via `addInitScript` Date mock where a clock is rendered. → the Overview KPI widgets and log timestamps get masked; the rest of the canvas is pixel-asserted. |
| V-3 | Multi-viewport: one `testProject` per viewport (Desktop Chrome 1280×720, phone width ~390×844) with per-project baselines. → §9 requires desktop + phone; we pin Desktop Chrome 1280×720 and iPhone 13 (390×844, DPR 3) projects. |
| V-3 | Baselines MUST be generated in the CI environment, not locally: OS rendering differences (Mac/Windows antialiasing vs Linux runner) produce false diffs. → baselines are committed from a CI "update-baselines" workflow (bot commit), never from a dev machine; local runs render a diff report artifact for review. |
| V-4 | CI: `npx playwright install --with-deps chromium` (chromium only); `wait-on` the app port before the suite; upload `test-results/` + `playwright-report/` artifacts on failure, retention ~7 days. → CI job shape pinned. |
| V-5 | Baseline updates: `--update-snapshots` on main after a deliberate design change, reviewed via the PR's image diff; never blanket-updated. → CONTRIBUTING note + CI workflow gate. |
| V-6 | Design-rule assertions as automatable checks: computed-style color assertions (every painted color resolves to a `colors_and_type.css` token value), no-emoji scan over rendered text, dark-only (`color-scheme: dark`) check. → implemented as a small Playwright companion suite (not screenshots) asserting the §9 design rules against the rendered DOM. |
| V-7 | `toHaveScreenshot` needs deterministic fonts: self-hosted fonts must be loaded before capture; `await page.evaluate(() => document.fonts.ready)` before shooting. → helper in the visual suite. |

## 8. Bundle budget

Sources: React lazy/Suspense route-splitting guidance + Vite build options
(search-verified 2026-09-12, multiple sources incl.
https://www.stackhawk.com/... and Vite docs references therein)

| # | Fact → PROxyLLM consequence |
|---|---|
| B-1 | React 18.3.1 + ReactDOM ≈ 46–47 KB gzip vendored runtime. → the ≤ 200 KB gzip initial budget (plan §8) leaves ~150 KB for app code + kit CSS — ample for 8 routes. |
| B-2 | Route-based code splitting via `React.lazy` + `Suspense`: each of the 8 routes becomes its own chunk loaded on navigation; don't split chunks below ~30 KB (more requests cost more than they save). → `manualChunks` splits `react`/`react-dom` (and `react-router` if used) into one vendor chunk; the 8 route chunks stay as one bundle unless a route grows heavy (charts route is the candidate to split first). |
| B-3 | Vite `build.reportCompressedSize: true` (default) prints gzip sizes per chunk at build time → CI parses this output; the budget gate fails the build on initial-load > 200 KB gzip. |
| B-3 | `assetsInlineLimit: 0` (P-2) also serves the budget/CSP goal jointly; no separate minification settings needed beyond Vite defaults. |

## 9. UpGuard kit → React integration (source of truth in-repo)

Source: `design/UpGuard/ui_kits/dashboard/README.md` + `DESIGN.md` + kit files
(in-repo, the binding design contract; no external research needed)

| # | Fact → PROxyLLM consequence |
|---|---|
| K-1 | Kit workflow: copy `kit.css` + fonts, `@import` tokens first (colors_and_type.css), kit class names used verbatim (`.appshell`, `.widget`, `.table`, `.kpi`, `.badge`, `.btn`, `.field`, …). → `webui/` copies kit.css unchanged and imports `colors_and_type.css` verbatim; React components bind `className` to kit classes — no CSS rewrites, no inline styles. |
| K-2 | Component map (README): App shell → `.appshell`/`.sidebar`/`.sidebar-link`/`.topbar`/`.search`/`.avatar`; widgets → `.canvas`/`.w-*`/`.widget`/`.empty`; KPI → `.kpi`/`.delta`/`.spark`/`.ring`/`.bars`/`.heat`; status → `.badge.up/.warn/.down/.paused/.info`; tables/forms/controls/buttons/overlays/nav per the README table. → the 8 §9 routes map one-to-one onto these classes; each React component is a typed wrapper of the kit markup. |
| K-3 | Design rules binding for the visual suite (V-6): single green accent `#3bd671`, tokens-only colors, dark-only `color-scheme: dark` + bg `#111821`, Plus Jakarta Sans display / Inter body / JetBrains Mono numbers, inline SVG icons (no emoji), honest empty states, `prefers-reduced-motion` honored, sidebar active bar = only left-stripe exception. → automated assertions + baselines captured from kit previews. |
| K-4 | Fonts are self-hosted (SIL OFL) from `design/UpGuard/fonts/` → copied into `webui/public/fonts/` (or emitted via `assetsInlineLimit: 0` as real files); `font-src 'self'`. |

## 10. Binding implementation consequences (checklist for Phase 2)

1. WebUI embed: `//go:embed all:dist` in the `internal/server/webui` package
   (E-1/E-2), served through `http.FS` (E-3) with the §5 history-routing SPA
   fallback for HTML GETs outside `/v1` and `/api`.
2. Session auth: 256-bit CSPRNG session IDs, SHA-256-hashed at rest, generic
   cookie name `id` (`__Host-id` on TLS arrivals), `HttpOnly` + non-persistent +
   `SameSite=Strict`, strict-mode (unknown ID → 401 + warning log), renew at
   login, logout = server row delete + cookie clear, 30 min idle / 12 h absolute
   server-side timeouts (S-1…S-11).
3. CSRF: signed double-submit — `X-CSRF-Token` header required on every
   state-changing admin request, validated as HMAC-SHA-256(master key,
   sessionID ‖ random); random part in a JS-readable cookie; `application/json`
   only (C-1…C-4); Origin/Referer match as a third layer (C-6); no
   state-changing GETs (C-5).
4. CSP (P-1…P-5): `default-src 'self'; script-src 'self'; style-src 'self';
   img-src 'self' data:; font-src 'self'; connect-src 'self'; object-src 'none';
   base-uri 'self'; form-action 'self'; frame-ancestors 'none'` — classic Vite
   SPA mode (static index.html, external hashed bundles, `assetsInlineLimit: 0`),
   no inline scripts/styles at runtime; P-4's `style-src-attr` question resolved
   at implementation with an ADR note if needed.
5. Admin API: builds on the existing store (WAL + single-connection, Q-4 closed);
   short per-handler transactions only.
6. Visual regression (V-1…V-7): Playwright `toHaveScreenshot`, Desktop 1280×720
   + iPhone 13 projects, `maxDiffPixelRatio: 0.001`, animations disabled, masked
   KPI/timestamps, `document.fonts.ready` before capture, chromium-only CI install,
   baselines generated in CI, artifacts on failure, design-rule companion suite
   (tokens-only colors, no emoji, dark-only).
7. Bundle budget (B-1…B-3): vendor chunk via `manualChunks`, route-level
   `React.lazy` splitting where a route justifies it, CI gate parses Vite's
   compressed-size report and fails > 200 KB gzip initial.
8. Kit integration (K-1…K-4): kit.css + colors_and_type.css copied verbatim,
   class names unchanged, fonts self-hosted from the repo's design/UpGuard/fonts.

---

### Deviations / open items deferred to implementation with justification

- **React Router version**: P-1's findings concern React Router v7 Framework Mode.
  PROxyLLM uses classic SPA mode. Whether we adopt react-router v7 library mode
  (BrowserRouter) or keep a minimal custom router is decided at implementation:
  both satisfy `script-src 'self'` (P-1); the decision will be recorded as an ADR
  note with the chosen package + version pinned in package.json.
- **V-3/V-4 numbering**: table rows renumbered after drafting; IDs V-1…V-7 are
  the binding set referenced from §10.
- **S-3 TLS split**: the `__Host-` prefix requires TLS; PROxyLLM's default LAN
  deployment is plain HTTP behind an external TLS terminator. The per-request
  prefix split (TLS → `__Host-id`, plain → `id`) is a documented deviation from
  the always-`__Host-` recommendation, justified by the single-service/LAN
  constraint, and covered by tests for both arrival schemes.
- **P-3 img-src data:** retained provisionally for inline-SVG placeholders;
  tighten to `'self'` at implementation if no `data:` images actually ship
  (checked during the CSP header test).