# Operating PROxyLLM

The console is the operator surface: providers and accounts, model
inventory, routing, keys, quotas and the request log. It is served by the
same binary as the API, on the same port, at `/`.

## At a glance

| Page | What it is for |
|---|---|
| **Overview** | Recent traffic: requests, failures and token totals for the last two weeks, plus today's numbers |
| **Providers** | Provider-type cards and the accounts inside them; OAuth sign-in lives here |
| **Models** | The model inventory: identity, provider, capabilities, priority — plus the model sync |
| **Routing** | Routing rules, the global strategy, and the alias map |
| **Keys** | Client keys: create (shown once), scope, revoke |
| **Logs** | The request log: rows, the trace timeline, sub-operations, captured bodies, live tail |
| **Quota** | Per-account usage and limits, with the account's cooldown state |
| **Settings** | The key/value store the runtime reads live |

## First run

The first visit offers **bootstrap**: create the admin account. That window
is open only while no admin exists — afterwards the endpoint answers `409`,
so an exposed instance cannot be taken over by a second registration.

Sessions last 30 minutes idle and 12 hours absolute. Logging out invalidates
the session immediately; changing the password invalidates every *other*
session of that admin.

## Providers and accounts

Providers are grouped by **type** — Custom OpenAI-compatible, Ollama Cloud,
ChatGPT/Codex (OAuth), and the web-tool backends Firecrawl and Ollama Cloud
Web Tools. An **account** is one credential of a type: several API keys or
several Codex sign-ins are several accounts, and routing rules can name
either a single account or the whole type.

Shared settings that belong to the type (an issuer, a base URL, a model
prefix, aliases) live on the type card; an account's own configuration
overrides them key by key. The model prefix decides the model identity the
type contributes (`ollamacloud/glm-5.3`), so changing it renames the models
that type imports.

Codex accounts sign in with the device flow: start it, open the verification
URL, enter the code. The account shows "sign-in required" until the token
exists — and an unsigned account is excluded from routing, because sending
a request into a guaranteed auth failure helps nobody.

## Models

Every model row has an **identity** (what clients and rules name), the
**provider account** that serves it, a **capability set**, and a
**priority**. The page lists **chat** providers only: the web-tool backends
(Firecrawl, Ollama Cloud Web Tools) emulate `web_search` / `web_fetch` and
own no models, so they are neither a group here nor a choice when you add a
model. Two provenances exist, shown as a badge:

- **synced** — imported and maintained by the model sync. Upstream
  additions appear, upstream removals disappear, and detected facts
  (capabilities, context window, and whether the model declares the
  decision surface) are filled in.
- **manual** — written by hand. The sync never touches or deletes it.

Saving a row in the console makes it manual. The sync then leaves it alone
**as long as its ID is not one the upstream offers** — a manual row whose ID
is exactly an upstream model ID (say `ollamacloud/glm-5.3`) is re-adopted as
synced on the next run: its operator edits survive, but it becomes tracked,
and an upstream removal would delete it. To keep a row permanently out of
the sync, give it an ID the upstream never reports (for example a routing
alias such as `glm`).

The **model sync** card runs at startup and on a configurable interval,
and **Sync now** runs a pass immediately. Its summary names, per account,
how many models were added, updated and removed — or why the account was
skipped (a dead upstream, a bad credential).

Detected facts never overwrite an operator decision: a value you saved, and
anything you pinned, survives every run. What a capability flag *does* is
documented in [model compatibility](reference/model-compat.md).

## Routing

A rule matches a model name (exact or wildcard) and carries an ordered
**chain** of accounts or provider types, a priority, an enabled flag and a
strategy. Two rules for the same name resolve by priority; rules always win
over the implicit route a model row provides.

**Aliases** rewrite the client's model name before any rule matches — the
map a Claude Code setup needs, since its auxiliary requests fire under
versioned IDs pinned by its own releases. A key is either an exact name or a
`*` pattern; an exact entry wins, and among patterns the most literal one
does.

The **strategy** decides how several accounts in one chain share load:
round-robin rotates per request, fill-first stays on the first entry until
the health gate opens or a rate limit exhausts the retry budget.

Semantics — capability filtering, failover, what opens the breaker — are in
[routing and fallbacks](routing-and-fallbacks.md).

## Keys

A client key (`pk_…`) is created with a name and displayed **once**: it is
stored as a keyed MAC, so it cannot be shown again. Revoking takes effect on
the next request. The log shows which key opened a request, by name.

## Logs

One row per request, newest first, with status, model, provider, protocol,
account, key, tokens, throughput and duration. Rejected requests are rows
too — a 404 is as visible as a 200.

Opening a row shows the request's facts, the **routing timeline** (every
attempt, failover and substitution, with the raw JSON behind a
disclosure), and its **sub-operations**: a failover, a capability fallback,
an emulated web-tool call — each as its own row, marked with an `↳` in the
list. A sub-operation opens its own view with what that step sent and
received.

**Captured bodies** are on by default: the conversation and the answer are
stored beside the row, redacted and capped, and retained for a configurable
number of days. Turning capture off keeps every metadata row and only stops
the bodies. Images are recorded by type and size, never by bytes.

The **live tail** streams new rows as they finish. It is a plain SSE
connection, so it reconnects by itself; an open Logs tab also keeps the
admin session warm.

## Quota

One card per chat account. For Ollama Cloud accounts with a stored session
cookie the card shows the live usage and limit figures from the account's
own settings page, refreshed on demand; without one it shows the local
count for the current month, labeled as such. Percentages that are unknown
render as `—`, never as zero. A cooldown — the account is out of quota, or
its breaker is open — is named on the card, which is the fastest way to
explain a wave of failures.

## Troubleshooting

| Symptom | Likely cause | Where to look |
|---|---|---|
| A model answers `404 model: …` | No rule matches and no model row exists for that name | Models (does the row exist?) and Routing (does a rule match the post-alias name?) |
| Claude Code reports an opaque error, or an empty answer | The model ID it asked for is not covered by an alias, or the target's thinking policy needs a pin | Logs (the row shows the requested model and the trace), Routing (alias patterns), Models (compat) |
| A request fails over and the client sees a truncated answer | The upstream rejected an input modality or the request exceeded its window | Logs: the trace names the class per attempt; Routing and fallbacks explains the walk |
| Web tools return nothing | No web-tool account, or every configured account failed | Providers (is a Firecrawl / Ollama Web Tools account configured and credentialed?), Logs (the `emulated_tool_attempt` events name each account and its outcome) |
| Every request fails at once for one account | Health cooldown after repeated hard failures | Quota (the card names the cooldown), Logs (the trace's `skipped` reason) |

## Day 2

- **Back up** the data directory *and* the master key together — one without
  the other is not a restore.
- **Rotate a provider credential** by editing the account; the old one is
  replaced in the encrypted store.
- **Upgrade** by pulling the new image and restarting: migrations run on
  open, and the master key must still be the same file.
- **Capacity**: the proxy is a stateless front for one SQLite file; the
  sizing numbers and the log-at-scale measurements are in
  [deployment](deployment.md).
