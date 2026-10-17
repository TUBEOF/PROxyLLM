# Getting started

PROxyLLM is one binary serving three client surfaces — OpenAI-compatible,
Anthropic-compatible and decision — with translation and routing in
between, plus the operator console on the same port. All state lives in one
data directory.

## What you need

| Path | Requirements |
|---|---|
| Container (recommended) | Docker with Compose v2 — no checkout, no toolchain |
| From source | Go 1.27 or newer, and Node 26 or newer to build the console |
| Prebuilt binary | A release archive for your OS/arch, see [deployment](deployment.md#binary-archives) |

## Run it

### Container

The deployment compose file runs the published multi-arch image:

```bash
curl -O https://raw.githubusercontent.com/TUBEOF/PROxyLLM/main/docker/compose.yaml
docker compose up -d
```

It publishes port 8080 and bind-mounts `./data` into the container, which
is the only writable path. From a source checkout, `make docker` builds the
image locally and runs the same file against it.

### From source

```bash
git clone https://github.com/TUBEOF/PROxyLLM.git
cd PROxyLLM
make webui-install webui-build   # builds the console into webui/dist
make build                       # embeds it into ./proxyllm
./proxyllm
```

`make run` builds and starts the binary in one step. The console is
compiled in: a `go build` without `make webui-build` embeds a placeholder
page that says so, and the API still works — only the console is missing.
Build and test mechanics are in [development](development.md).

## First run

The server listens on `:8080` (`PROXYLLM_SERVER_ADDR` changes that). Its
health endpoint is unauthenticated:

```bash
curl http://localhost:8080/healthz
# {"status":"ok"}
```

Open `http://localhost:8080`. The first visit offers the console's
bootstrap form: create the admin account. That window is open only while no
admin exists. What each page of the console does is documented in
[operating PROxyLLM](operating.md); from here on this page only walks the
first request through.

On first start the process creates its data directory under the working
directory it was started from (in the container, inside the bind mount):

- `data/proxyllm.db` — the SQLite database, journal mode WAL, so
  `-wal` and `-shm` sidecar files may appear next to it;
- `data/master.key` — the envelope master key, generated with mode 0600.

Both paths are configuration (see [configuration](configuration.md)); both
belong in the backup.

## Create a client key

In the console, open **Keys** and create one. The `pk_…` secret is shown
exactly once — it is stored as a MAC and cannot be displayed again — so
copy it now. Keys are revocable at any time, and revocation takes effect on
the next request.

## Point a client at it

**Claude Code** speaks the Anthropic protocol; this is the whole setup:

```bash
export ANTHROPIC_BASE_URL="http://localhost:8080"
export ANTHROPIC_AUTH_TOKEN="pk_<prefix>_<secret>"
export ANTHROPIC_MODEL="claude-sonnet-5"   # any model row, or an alias
```

Model names it sends are rewritten by the alias map before routing, which
is what lets a non-Claude target answer the ID Claude Code asks for — the
mapping and its failure modes are in
[Claude Code](claude-code.md).

**An OpenAI SDK client** needs the base URL and the same key:

```python
from openai import OpenAI

client = OpenAI(base_url="http://localhost:8080/v1", api_key="pk_<prefix>_<secret>")
```

**curl**, for a first look:

```bash
export KEY=pk_<prefix>_<secret>

curl -sS http://localhost:8080/v1/models -H "Authorization: Bearer $KEY"

curl -sS http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer $KEY" -H 'content-type: application/json' \
  -d '{"model":"<model-id>","messages":[{"role":"user","content":"Say OK"}]}'
```

The model id is the identity from the console's Models page, not the name
the upstream uses.

## Add a provider

Until a provider and a model exist, every request answers a model-not-found
error. In the console, open **Providers** and add one:

- **Custom OpenAI-compatible** — any endpoint, configured by URL and key;
- **Ollama Cloud** — an API key;
- **ChatGPT / Codex** — an OAuth device-flow sign-in;
- **Firecrawl** or **Ollama Cloud Web Tools** — a web-tool backend rather
  than a chat provider, which serves emulated `web_search`/`web_fetch` for
  models that have no native server tools.

The model sync mirrors each chat provider's upstream list where that
provider exposes model discovery, at startup and on its interval, so model
rows appear on their own; **Sync now** runs a pass on demand. Adding,
editing and testing accounts is covered in
[operating PROxyLLM](operating.md#providers-and-accounts).

## Verify

1. `GET /v1/models` returns a non-empty `data` array. An empty list means
   no model row exists yet — check that the provider accepted a connection
   and that the sync ran.
2. Send one chat request (the curl call above, or a Claude Code prompt).
3. Open the console's **Logs** page: the request is a row, with its status,
   the model and provider that served it, token counts, and the routing
   timeline behind a disclosure. Rejected requests are rows too.

## Production notes

- **Keep the master key.** It seals provider credentials at rest, signs
  console sessions and derives the client-key MAC, so it must survive every
  upgrade and it is immutable in this release. Back up the data directory
  and the key together — one without the other is not a restore. See
  [the master key](configuration.md#master-key).
- **Web-tool emulation needs an account.** `web_search`/`web_fetch`
  emulation is inactive until a web-tool account exists in the console or
  `PROXYLLM_FIRECRAWL_API_KEY` is set; without either, emulated tool
  requests receive the documented "unavailable" error results, and startup
  logs a warning when that environment variable is unset. The behavior is
  described in
  [web-tool emulation](routing-and-fallbacks.md#tool-emulation).
- **Authentication stays on.** `PROXYLLM_AUTH_DISABLED=1` is the documented
  opt-out and it emits a loud startup warning, but it means every client
  can use the proxy without a key: never set it on a network you do not
  control. For anything beyond a trusted LAN, terminate TLS in front and
  read [security](security.md) and [deployment](deployment.md).

## Where next

- [Configuration](configuration.md) — layers, environment variables, database overrides
- [Operating PROxyLLM](operating.md) — the console, page by page
- [API reference](api/README.md) — endpoints, authentication, errors, OpenAPI description
- [Claude Code](claude-code.md) — non-Claude models behind an Anthropic client
- [Routing and fallbacks](routing-and-fallbacks.md) — rules, chains, failover
- [Deployment](deployment.md) — images, TLS, sizing, upgrades
- [Security](security.md) — threat model and controls
