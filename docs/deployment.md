# Deployment

Three deployment shapes are supported: the compose file (recommended), the
published container image, and static binary archives. All three run the
same single binary with the console embedded, and all three keep their
entire state in one directory — the SQLite database plus the envelope master
key.

## Docker Compose (recommended)

[`docker/compose.yaml`](../docker/compose.yaml) runs the published
multi-arch image as one hardened service. It is the copy-paste starting
point and needs no source checkout:

```bash
docker compose up -d
```

From a source checkout, `make docker` builds `proxyllm:dev` and starts the
same file through the `IMAGE` override — there is no second compose file.

The image tag is pinned in the file rather than tracking `latest`, so a
deployment is reproducible and upgrades are a deliberate edit (the release
step re-points the pin, see [release](development.md#release)).

`./data` is bind-mounted at `/data` and is the only writable path. It holds
the SQLite database and the master key file; together they are the full
state, so back up the directory — not one file of it
([Day 2](operating.md#day-2)).

## Container hardening

The image is distroless static: no shell, no package manager, nothing to
exec into. It declares `USER nonroot:nonroot` and pre-creates `/data` owned
by that UID, so a named volume works unmodified.

The compose profile adds, with the reason each one is there:

- **`user: "0:0"`** — this looks like the opposite of hardening and is the
  deliberate choice. The bind-mounted `./data` belongs to the invoking user
  (or is created by Docker as root), and a fixed non-root UID inside the
  container cannot be made to match every host — including Docker Desktop
  file sharing, where the UID has no host meaning at all. Running as root
  sidesteps the ownership question entirely and works on any host. Nothing
  else in the profile is relaxed for it: the root filesystem is still
  read-only and the only writable path is the data directory. If the host
  directory's ownership is under your control, `user: "65532:65532"` — the
  image's own non-root user — is a supported tightening.
- **`read_only: true`** — the image's own filesystem is not writable. A
  process that could rewrite its binary or drop a file in `/` cannot.
- **`tmpfs: /tmp:size=16m,noexec,nosuid`** — runtime scratch is a
  size-capped, non-executable, non-setuid tmpfs, so scratch cannot grow
  without bound and nothing left there survives a restart.
- **`mem_limit: 384m` with the healthcheck** — the two work as a pair. The
  limit bounds what the container can take from the host; the healthcheck
  notices when a container has stopped answering, and
  `restart: unless-stopped` replaces it instead of leaving it wedged
  forever. The limit sits roughly three times above the measured budget
  below, so it constrains a runaway, not ordinary traffic.

**The healthcheck is the binary itself**: `["CMD", "/proxyllm",
"-healthcheck"]`, 30 s interval, 5 s timeout, 3 retries, 5 s start period.
It is in-binary because the distroless image has no shell and no `curl` to
probe with. It resolves its target in this order:

1. `PROXYLLM_HEALTHCHECK_ADDR`
2. `PROXYLLM_SERVER_ADDR`
3. `localhost:8080`

A leading colon is rewritten, so the container's own `:8080` bind is probed
as `localhost:8080`. It then issues one `GET /healthz`, gives up after 3 s,
and exits 0 or 1. See
[configuration](configuration.md#environment-variables).

## Prebuilt images

Every release publishes a multi-arch manifest list (amd64 and arm64) to
GHCR:

```bash
docker run -d -p 8080:8080 -v ./data:/data ghcr.io/tubeof/proxyllm:v1.16.0
```

`:latest` tracks the newest release; a version tag is the reproducible
choice. The image is distroless, so there is no shell to configure it from —
configuration comes from environment variables or a mounted `config.json`
([configuration](configuration.md)). Note that a host bind mount keeps the
host's ownership, which the image's non-root user cannot write; either give
the directory to UID 65532 or run the container as the invoking user, as
the compose file does.

## Binary archives

GitHub Releases also ship per-OS/arch archives (`proxyllm_<os>_<arch>.tar.gz`,
zip on Windows, with `checksums.txt` beside them). The console is embedded
in the binary, so there is nothing else to install:

```bash
tar -xzf proxyllm_linux_amd64.tar.gz && ./proxyllm
```

## TLS

The server speaks plain HTTP on its listen address: it serves no TLS and
issues no HSTS header. For anything beyond a trusted network, terminate TLS
in front of it (nginx, Caddy, Traefik) and set HSTS at the terminator. The
console's own security headers — CSP, `nosniff`, `Referrer-Policy` — are set
by the server on every response ([Security](security.md#network-and-http-hardening)).

The login cookie is marked `Secure` and prefixed `__Host-` only when the
connection to the proxy itself arrives over TLS, which a terminator in front
does not give it: behind one, the session cookie is a plain host cookie.
The terminator is therefore the only thing standing between that cookie and
the network, and must not be reachable over plain HTTP itself.

## Resource sizing

The budgets the benchmarks measure against:

| Budget | Ceiling |
|---|---|
| Proxy-added latency, p99 | 10 ms |
| Streaming first-byte overhead | 15 ms |
| Memory at 100 concurrent streams | 128 MB |

They are measured over the fully assembled stack — authentication, API,
router, executor, and an upstream over real HTTP — with a raw HTTP client,
so the number is the proxy's own overhead and not an SDK's or a network's.
The release gate hard-asserts the two timing ceilings; the memory figure is
reported by the concurrency benchmark as the process's `MemStats.Sys`, the
same metric the release job compares against.

**One honest caveat on the 10 ms ceiling.** It is the one number a shared,
loaded container cannot calibrate. Re-measured on such a machine, the same
code that the release runner historically measured at 2.6-5.9 ms reads
**16.98 ms** best-of-five batches (individual batches 17.0, 19.6, 26.3, 17.4
and 25.9 ms), while the first-byte budget stays far under its ceiling at
6.3 µs. That is the host's scheduling noise, not a regression and not a
property of the proxy — the p99 is hostage to a handful of stalls, and the
measurement takes the best of several batches to isolate the proxy's own
cost. Recalibrating the ceiling needs a pass on a quiet runner; until then
the release gate is where it bites, and the main-branch benchmark job runs
in report mode ([performance](development.md#performance)).

### The request log at scale

The log grows with traffic, and the Logs page polls it while open, so its
cost is worth knowing rather than assuming. Measured against 200,000 rows
with the indexes the schema ships:

| Read | Plan | Time |
|---|---|---|
| The unfiltered first page (what the poll runs) | index walk, stops at the limit | 0.02 ms |
| The live-tail cursor feed | rowid seek | — |
| Filtered by model | index seek, then a sort of every match | 7 ms |
| Filtered by provider and status | index seek, then a sort of every match | 16 ms |

The plan column is the part that carries: the same measurement on a shared
container lands several times higher in absolute terms while the plans are
identical, because the cost is dominated by the host rather than by the
shape.

The single-column filter indexes do not provide the requested order, so a
filtered page sorts its whole match set before taking one page. That is not
worth another index today: the query the page actually repeats is the
unfiltered one, and every index added is write amplification on the hot
insert path of every proxied request. The trigger to revisit is a filtered
page becoming a repeated read rather than an interactive one, or a log well
past this size — the fix is a composite `(filter column, ts)` index, and
the measurement above is what should justify it.

## Operations

- **Logs** are structured JSON to stdout, at the level and in the format
  [configuration](configuration.md#environment-variables) sets.
- **Retention.** Request logs and usage live in SQLite; size the volume for
  the retention window you configure
  ([Logs](operating.md#logs)).
- **Backup** is the data directory *and* the master key. The database
  without the key cannot read its own encrypted rows, and the key without
  the database is not a restore.
- **Upgrades** are a tag change: pull the new image (or unpack the new
  archive), restart, and migrations run on open. The master key must still
  be the same file — without it the encrypted rows cannot be opened.
