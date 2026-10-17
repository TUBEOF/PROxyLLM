# Deployment

## Docker Compose (recommended)

[`docker/compose.yaml`](../docker/compose.yaml) runs the published
multi-arch image (`ghcr.io/tubeof/proxyllm:<tag>`) as a single hardened
service — the copy-paste starting point, no source checkout needed:

```bash
docker compose up -d
```

From a source checkout, `make docker` builds the image locally and runs
the same compose file against it (via the `IMAGE` override).

Hardening in place: `read_only` rootfs, tmpfs `/tmp`, `mem_limit`,
healthcheck + resource limits. The only writable path is the
bind-mounted `./data` (SQLite database + master key file — back both
up; together they are the full state).

Healthcheck: the in-binary probe (`/proxyllm -healthcheck`, 3 s timeout,
30 s interval, 5 s start period) — distroless has no shell/curl.

## Prebuilt images

Release tags publish multi-arch images (amd64 + arm64):

```bash
docker run -d -p 8080:8080 -v ./data:/data ghcr.io/tubeof/proxyllm:v1.15.1
```

The image is distroless (no shell). Configuration via ENV or the
optional `config.json` (see [configuration.md](configuration.md)).

## Binary archives

GitHub Releases ship static binary archives per OS/arch (the WebUI is
embedded — one file, no external assets):

```bash
tar -xzf proxyllm_linux_amd64.tar.gz && ./proxyllm
```

Checksums are in `checksums.txt` per release.

## Reverse proxy / TLS

PROxyLLM v1 serves plain HTTP on its listen address. For anything beyond
a trusted LAN, terminate TLS in front (nginx/Caddy/Traefik) and forward
the standard headers; set HSTS at the terminator. The WebUI's security
headers (CSP, nosniff, frame-ancestors) are already set by the server.

## Resource sizing

Measured budgets:

- proxy-added latency p99 ≤ 10 ms; streaming first-byte overhead ≤ 15 ms
- RSS ≤ 128 MB at 100 concurrent streams
- default container memory limit sized for that budget

### The request log at scale

The log grows with traffic, and the Logs page polls it while open, so what it
costs is worth knowing rather than assuming. Measured against 200,000 rows with
the indexes the schema ships:

| Read | Plan | Time |
|---|---|---|
| The unfiltered first page (what the poll runs) | index walk, stops at the limit | 0.02 ms |
| The live-tail cursor feed | rowid seek | — |
| Filtered by model | index seek, then a sort of every match | 7 ms |
| Filtered by provider and status | index seek, then a sort of every match | 16 ms |

The single-column filter indexes do not provide the requested order, so a
filtered page sorts its whole match set before taking one page. That is not
worth another index today: the query the page actually repeats is the unfiltered
one, and every index added is write amplification on the hot insert path of
every proxied request. The trigger to revisit is a filtered page becoming a
repeated read rather than an interactive one, or a log well past this size — the
fix is a composite `(filter column, ts)` index, and the measurement above is
what should justify it.

## Operations

- Logs are structured JSON to stdout (`PROXYLLM_LOG_LEVEL`,
  `PROXYLLM_LOG_FORMAT`).
- Retention: request logs and usage are in SQLite; size the volume for
  your retention window (WebUI Settings offers retention controls).
- Upgrades: pull the new tag, restart; SQLite migrations run on open. The
  master key must persist — without it the encrypted rows cannot open.