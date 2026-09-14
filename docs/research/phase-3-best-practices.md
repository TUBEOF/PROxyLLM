# Phase 3 Best-Practice Research Pass

Per plan §11 (binding doctrine): live, cited research before every phase. All facts
below were researched live via Firecrawl on **2026-09-14** (source URL + access date
per fact), mirroring phase-1/phase-2-best-practices.md. Topics per plan §11 Phase 3:
Go security scanning in CI (gosec/govulncheck), HTTP server timeout hardening,
release pipeline (multi-arch images, GHCR), compose production polish.

---

## 1. Go security scanning stack in CI (gosec + govulncheck pairing)

Source: https://github.com/securego/gosec (accessed 2026-09-14)
Corroborated: https://appsecsanta.com/sast-tools/sast-tools-for-go (accessed 2026-09-14)

| # | Fact → PROxyLLM consequence |
|---|---|
| SEC-1 | gosec is the baseline SAST for Go (30+ checks, G1xx–G7xx categories incl. G7xx taint analysis for SQLi/command injection/SSRF/XSS); runs in seconds, free. → add gosec as a blocking CI gate; run over `cmd internal webui` (same GO_PKGS scope as vet — node_modules must not be descended into). |
| SEC-2 | govulncheck (Go team) reports only vulnerabilities REACHABLE from your code — dependency CVEs that the code never calls are not reported, cutting noise vs. dependency-CVE scanners. → already in CI (Gate 2); pairing gosec (code patterns) + govulncheck (reachable CVEs) is the documented standard pairing. |
| SEC-3 | gosec exit codes: 0 = clean, 1 = ≥1 unsuppressed finding; `#nosec [RuleList] [-- justification]` inline annotations suppress verified false positives per rule; `-nosec-require-rules`/`-nosec-require-justification` (opt-in) reject naked/justification-free suppressions. → adopt: every suppression must name the rule + justification (discipline, auditable). |
| SEC-4 | gosec supports SARIF output for GitHub code scanning (upload via codeql-action/upload-sarif). → CodeQL (plan Gate 5) is provided separately by GitHub Actions `github/codeql-action/init+analyze` with `language: go` — standard free setup. |
| SEC-5 | gosec requires Go 1.25+ (install: `go install github.com/securego/gosec/v2/cmd/gosec@latest`); GitHub Action `securego/gosec@master` is version-pinned to the latest stable release. → run gosec via its official action in CI (local Windows runs optional; the blocking gate lives in CI). |

## 2. Go HTTP server timeouts & streaming (what the proxy may and may not set)

Source: https://adam-p.ca/blog/2022/01/golang-http-server-timeouts/ (accessed 2026-09-14)
Corroborated: https://blog.cloudflare.com/the-complete-guide-to-golang-net-http-timeouts/ (accessed 2026-09-14)

| # | Fact → PROxyLLM consequence |
|---|---|
| T-1 | Zero-value http.Server timeouts mean NO timeout — "shouldn't be used for anything but toy servers"; ReadHeaderTimeout specifically mitigates Slowloris (header read deadlined before any handler runs). → already set (10s ReadHeaderTimeout in cmd/proxyllm); keep, and verify with a test that slow headers are rejected. |
| T-2 | ReadTimeout/WriteTimeout are ABSOLUTE per-connection deadlines: a streaming proxy CANNOT set WriteTimeout — it kills legitimate long streams (Cloudflare: a server that streams must leave WriteTimeout unset; no per-Write deadline access from ServeHTTP without net.Conn). → PROxyLLM streams SSE: WriteTimeout and ReadTimeout (body) must stay 0; the streaming watchdog (C2 300 s relayed-bytes rule) lives in the executor/stream layer, not the server socket. Documented as a deliberate timeout policy. |
| T-3 | IdleTimeout covers keep-alive idle connections (Cloudflare recommends it); without it, idle connections accumulate indefinitely. → set IdleTimeout (e.g. 120 s) — bounded connection table on the LAN surface. |
| T-4 | Graceful shutdown pattern: signal.NotifyContext → server.Shutdown(ctx with timeout) — stops accepting, closes idle keep-alives, waits for active requests, deadline-forcibly closes. → already implemented in cmd/proxyllm (10 s shutdown budget). |

## 3. Release pipeline: GoReleaser + GitHub Actions + GHCR multi-arch

Source: https://goreleaser.com/resources/cookbooks/multi-platform-docker-images/ (accessed 2026-09-14)
Corroborated: https://schoenwald.aero/posts/2025-01-25_effortless-multi-arch-docker-images-with-goreleaser-and-github-actions/ (accessed 2026-09-14)

| # | Fact → PROxyLLM consequence |
|---|---|
| R-1 | Tag-triggered GoReleaser on GitHub Actions is the standard Go release pipeline: binary archives per GOOS/GOARCH + Docker multi-platform builds pushed to GHCR in one workflow run. → PROxyLLM's release workflow triggers on `v*` tags. |
| R-2 | Multi-arch images need docker/setup-qemu-action (or native arm64 runners) AND a multi-platform FROM in the Dockerfile (distroless/static-debian12 IS multi-platform). → amd64 + arm64 image variants, QEMU builder. |
| R-3 | PROxyLLM ships ONE artifact that matters: the single static CGO-free binary with the embedded WebUI. A dedicated GoReleaser binary-archive matrix is nice-to-have; the container (plan's deployment unit) is the release core. → v1 release workflow: build+push multi-arch image to GHCR on tag, attach binary archives as GitHub Release assets via GoReleaser (same tag). |
| R-4 | Release workflows need explicit `permissions: contents: write` (create release) + `packages: write` (GHCR push) — least privilege otherwise (CI default is contents: read). |

## 4. Compose production polish (healthcheck + resource limits pairing)

Source: https://oneuptime.com/blog/post/2026-02-02-docker-resource-limits/view (accessed 2026-09-14)

| # | Fact → PROxyLLM consequence |
|---|---|
| D-1 | Healthchecks and resource limits work best together: a container that wedges under resource pressure is detected and restarted. → compose already pairs both (healthcheck + pids_limit + read_only + cap_drop). Add explicit memory limit per plan §10 ("compose polish: healthcheck, resource limits"). |
| D-2 | `start_period` gives slow-start services a grace window before failures count; `interval/timeout/retries` control steady-state. → already configured (5 s start_period, 30 s/5 s/3). |
| D-3 | Distroless images have no shell/curl — healthchecks must use an in-binary probe (`/proxyllm -healthcheck`), which PROxyLLM already implements (no extra tooling in the image). |

## 5. Documentation set for a v1 OSS release

Source: repo plan §10 + GitHub OSS conventions (accessed 2026-09-14, repo-internal)

- README quickstart must be verifiable from a clean clone (plan Phase 3 DoD) —
  currently references only getting-started.md; the docs set needs the translation
  matrix, api-reference, security guide, and the non-Claude routing guide the plan
  §10 promises.
- SECURITY.md must describe the disclosure channel (private vulnerability reporting)
  and the supported scope (Analysis G documented out-of-scope: internet-scale
  multi-tenant exposure).