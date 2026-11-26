# Anthropic OAuth (Teams/Enterprise)

The `anthropic-oauth` provider signs in to an Anthropic **Teams or
Enterprise** workspace with an OAuth browser flow. The proxy holds the tokens
server-side and sends them as a Bearer credential with
`anthropic-beta: oauth-2025-04-20`; clients never see them.

## Who this is for — and who it is not

| Account | Supported | Why |
|---|---|---|
| Claude Teams / Enterprise | **Yes** | The legitimate OAuth path; the workspace must allow OAuth for the organization. |
| Claude Free / Pro / Max (consumer) | **No** | Since 2026-01-09 Anthropic's servers reject consumer OAuth credentials outside Claude.ai and the Claude Code CLI ("This credential is only authorized for use with Claude Code…"), and Anthropic's terms prohibit consumer-plan OAuth in third-party tools. |
| Anthropic Console API key | Yes, but use the **Anthropic** provider | An API key is simpler and has no OAuth restrictions. |

The proxy implements **no** user-agent or client-identity cloaking. Using a
consumer account here is a terms-of-service violation and can get the account
restricted; the console shows this warning before the first sign-in and
requires an explicit acknowledgement (persisted server-side, versioned).

## Before you start

- A Teams/Enterprise workspace whose policy allows OAuth.
- The organization has not disabled OAuth ("OAuth authentication is currently
  not allowed for this organization" is an organization setting).
- The proxy is reachable from the operator's browser for the console; the
  OAuth callback itself does not have to be.

## Flow

1. **Providers → Anthropic OAuth (Teams/Enterprise) → Add account.** The
   first click shows the risk notice; acknowledge it once per notice version.
2. **Open the sign-in URL.** The console shows an authorize URL and a paste
   field.
3. **Sign in in the browser.**
   - If the browser and the proxy are on the same machine, the loopback
     callback (`http://localhost:53692/callback`, `127.0.0.1` only) finishes
     the flow by itself.
   - Otherwise the browser lands on a "localhost not found" page. That is
     expected. Copy the full address-bar URL (or the `code#state` value the
     page shows) and paste it into the console.
4. **Complete.** The server validates `state` against the pending flow,
   exchanges the code, seals the access and refresh tokens in the token
   store, and the account becomes routable.

A login flow lives 15 minutes; starting a new flow replaces the pending one.
There is no device flow: Anthropic does not offer one.

## What the proxy sends

- `Authorization: Bearer <access token>` — never `x-api-key`.
- `anthropic-version: 2023-06-01`.
- `anthropic-beta: oauth-2025-04-20` (merged with any client `anthropic-beta`
  value).
- The client's own `Authorization` / `x-api-key` is never forwarded: the
  proxy's key authenticates the client, and the OAuth bearer authenticates
  the upstream account.

## Token lifecycle

- Access tokens last 8 hours (`expires_in: 28800`); the proxy refreshes them
  5 minutes before expiry.
- Refresh is single-flight and server-side; a rotated refresh token is
  persisted together with the new access token in one transaction.
- `invalid_grant` marks the account as requiring a new sign-in; the console
  asks the operator to sign in again. Tokens are not deleted automatically,
  so the failure remains diagnosable.

## Open questions (verified per deployment)

The wire facts and their sources are recorded in
[research/anthropic-oauth.md](../research/anthropic-oauth.md). Five items are
configuration or documentation until confirmed against a real Teams account:
whether a third-party proxy may use the Claude Code client id, the minimal
scope set, accepted localhost ports, refresh-rotation semantics, and whether
the beta header is required on every endpoint. The defaults are the
documented values; the client id is overridable.
