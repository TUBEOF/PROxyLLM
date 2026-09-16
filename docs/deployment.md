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

Hardening in place: `read_only` rootfs, tmpfs `/tmp`, `pids_limit`,
`mem_limit`, `cap_drop: ALL`, `no-new-privileges`, non-root distroless
image. The only writable path is `/data` (SQLite database + master key
file — back both up; together they are the full state).

### Data: named volume (default) or host bind mount

The compose file ships a named volume by default — docker-managed,
works with the image's nonroot user (UID 65532) on every host OS, and
survives `docker compose down`.

For a plain host directory instead (direct access to the files, host
backup tooling), set `DATA` **and** `PUID`/`PGID` to the directory
owner's uid:gid — the nonroot default user cannot write into a
directory owned by the invoking user, and the container would exit with
`storage: ping /data/proxyllm.db: unable to open database file (14)`:

```bash
mkdir data
PUID=$(id -u) PGID=$(id -g) DATA=./data docker compose up -d
```

Host caveat: bind mounts go through the Docker VM's file sharing. On
Windows/macOS local drives this works with the PUID/PGID above; on
network shares (SMB/NFS-mounted host paths) non-root writes may be
rejected regardless of UID — use the named-volume default or a local
directory in that case.

Healthcheck: the in-binary probe (`/proxyllm -healthcheck`, 3 s timeout,
30 s interval, 5 s start period) — distroless has no shell/curl.

## Prebuilt images

Release tags publish multi-arch images (amd64 + arm64):

```bash
docker run -d -p 8080:8080 -v proxyllm-data:/data ghcr.io/tubeof/proxyllm:v1.0.0
```

The image is distroless non-root; no shell. Configuration via ENV or the
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

Plan §8 budgets, verified in the phase-3 report
([verification/phase-3-verification-report.md](verification/phase-3-verification-report.md)):

- proxy-added latency p99 ≤ 10 ms; streaming first-byte overhead ≤ 15 ms
- RSS ≤ 128 MB at 100 concurrent streams
- default container memory limit sized for that budget

## Operations

- Logs are structured JSON to stdout (`PROXYLLM_LOG_LEVEL`,
  `PROXYLLM_LOG_FORMAT`).
- Retention: request logs and usage are in SQLite; size the volume for
  your retention window (WebUI Settings offers retention controls).
- Upgrades: pull the new tag, restart; SQLite migrations run on open. The
  master key must persist — without it the encrypted rows cannot open.