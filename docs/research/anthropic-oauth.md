# Anthropic OAuth wire facts

Read on 2026-11-26 for ADR-022. This note records only what the sources below
state; anything marked **unverified** must be confirmed against a Teams /
Enterprise test account before it is treated as a contract (work order
OQ-1…OQ-5).

## Endpoints and client

| Fact | Value | Source |
|---|---|---|
| Authorization endpoint | `https://claude.ai/oauth/authorize` (console mode: `https://console.anthropic.com/oauth/authorize`) | `akashmohan.com/writings/claude-code-oauth`; OpenCode plugin write-up (`forums.basehub.com/anomalyco/opencode/1`); Claude Code `oauth.ts` |
| Token endpoint | `https://platform.claude.com/v1/oauth/token`; the older `https://console.anthropic.com/v1/oauth/token` is interoperable | same sources; `huggingface.co/.../agent/anthropic_adapter.py` tries both |
| Client id | `9d1c250a-e61b-44d9-88ed-5944d1962f5e` (the Claude Code client) | all of the above |
| Grant types | `authorization_code`, `refresh_token` | same |
| Body encoding | **JSON**, not form-encoded (unlike Codex) | `akashmohan.com/writings/claude-code-oauth`; grain-agent OAuth spec |
| PKCE | `S256`; verifier 43–128 chars | RFC 7636; Claude Code sources |
| Redirect URI | `http://localhost:{port}/callback` for the loopback flow; `https://console.anthropic.com/oauth/code/callback` / `https://platform.claude.com/oauth/code/callback` for the hosted callback | `akashmohan.com/writings/claude-code-oauth`; leaked `oauth.ts` |
| CLI callback format | the hosted page shows `code#state`; the paste path splits on `#` | `akashmohan.com/writings/claude-code-oauth` |
| Code exchange body | `grant_type=authorization_code`, `code`, `state`, `client_id`, `redirect_uri`, `code_verifier` | same |
| Refresh body | `grant_type=refresh_token`, `refresh_token`, `client_id` | same; pwn/Ruby diffend notes "JSON body, same as codex" |
| Access token | `sk-ant-oat01-…`, `expires_in: 28800` (8 h) | token response documented by `akashmohan.com` |
| Refresh token | `sk-ant-ort01-…`, no documented expiry; response may rotate it | same; rotation semantics **unverified** (OQ-4) |
| Token response extras | `scope`, `organization{uuid,name}`, `account{uuid,email_address}` | same |
| Inference call | `Authorization: Bearer <access>` plus `anthropic-beta: oauth-2025-04-20` (Claude Code also sends `claude-code-20250219`) | `nanoclaw` issue, `dsh-anthropic-oauth`, `claude-oauth-proxy/main.go`, grain-agent OAuth spec |
| Beta on all endpoints | **unverified** (OQ-5); apply it uniformly to `/v1/messages`, `count_tokens` and `models` | — |
| Scopes | minimal `user:profile user:inference`; Claude Code also uses `org:create_api_key`, `user:sessions:claude_code`, `user:mcp_servers`, `user:file_upload` | `akashmohan.com`; leaked `oauth.ts`. Minimal set **unverified** (OQ-2) |
| Third-party use of the client id | **unverified** (OQ-1). No org-owned OAuth app is documented. | — |
| Arbitrary localhost ports | **unverified** (OQ-3); Claude Code uses 53692 (newer) / 54545 (older) | work order, `zot` notes |

## Consumer-plan restriction (verified)

Anthropic's servers reject OAuth tokens from Free/Pro/Max plans outside
Claude.ai and the Claude Code CLI with "This credential is only authorized for
use with Claude Code and cannot be used for other API requests" (server-side
change reported on 2026-01-09). Anthropic's legal/compliance documentation
prohibits consumer-plan OAuth tokens in third-party tools, and OpenCode removed
Claude Pro/Max OAuth for that reason. Teams/Enterprise workspaces remain the
legitimate OAuth path; an organization can still disable OAuth for itself
("OAuth authentication is currently not allowed for this organization").

Sources: `developersdigest.tech/errors/opencode/claude-code-credential-only`,
`midens.com/articles/2026-02-anthropic-oauth-ban-ai-tool-alternatives`,
`anthropics/claude-code` issue #8046, Anthropic legal-and-compliance docs.

## What this implementation does with the unverified items

- Only the documented minimal chat scopes are requested (`user:profile`,
  `user:inference`); `org:create_api_key` is not requested because the proxy
  does not create API keys.
- The client id and endpoints are configuration with the documented values as
  defaults, so a Teams deployment can override them without a code change.
- The refresh token returned by any rotation is always persisted, which is
  correct under both rotation semantics.
- The beta header is attached to every Anthropic request of this provider.
- No client-identity or user-agent cloaking is implemented.
