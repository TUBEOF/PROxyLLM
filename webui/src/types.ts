// Wire types mirroring the admin API DTOs (internal/server/webui/adminapi.go
// — the Go structs are the contract; these mirror them field-for-field).
// Raw JSON blobs (config/capabilities/compat/usage/route_trace) stay `unknown`
// on purpose: the API round-trips them verbatim and the editors treat them
// as editable JSON, not as typed objects.

export interface Provider {
  id: string;
  type: string;
  name: string;
  config: unknown;
  // The account's pool switch: a disabled account is not built into the live
  // registry at all — no routing, no chain expansion, no health, no sync.
  enabled: boolean;
}

// ADR-009: one account is a providers row of a type; has_credential is a
// presence probe of the encrypted token store (the secret never rides).
export interface Account {
  id: string;
  type: string;
  name: string;
  has_credential: boolean;
  enabled: boolean;
  // ADR-012: the ollama.com session cookie behind the Quota page's live
  // windows — presence only, the value never leaves the encrypted store.
  has_usage_cookie?: boolean;
}

// Provider-type catalog entry (admin GET /api/admin/provider-types): the
// metadata the Providers cards and the per-type forms render from.
export interface ProviderTypeField {
  key: string;
  label: string;
  kind: 'text' | 'url';
  required: boolean;
  hint: string;
}

export interface ProviderType {
  type: string;
  label: string;
  description: string;
  // ADR-010: "llm" (routable chat models) or "web_tool" (emulator backend
  // for web_search/web_fetch fallback lists).
  kind: 'llm' | 'web_tool';
  auth: 'api_key' | 'oauth';
  settings_fields: ProviderTypeField[];
  account_fields: ProviderTypeField[];
  // The type's accounts can hold an ollama.com session cookie (ADR-012).
  usage_cookie: boolean;
  default_capabilities: Record<string, boolean> | null;
}

export interface ProviderTypesResponse {
  types: ProviderType[];
}

export interface ProviderTypeDetailResponse {
  settings: unknown;
  accounts: Account[];
}

export interface Model {
  id: string;
  provider_id: string;
  name: string;
  capabilities: unknown;
  compat: unknown;
  priority: number;
  // ADR-010 provenance: "manual" (operator row, sync never touches it) or
  // "synced" (imported/updated by the model-sync run).
  origin: 'manual' | 'synced';
}

export interface RoutingRule {
  id: string;
  match_model: string;
  chain: string[];
  priority: number;
  enabled: boolean;
  // ADR-009: '' inherits the global default; otherwise 'round_robin' or
  // 'fill_first'.
  strategy: string;
}

// ADR-009 routing strategies: round_robin rotates the expanded candidate
// list per request; fill_first keeps the chain order (sticky to the first
// entry until the health gate opens or a 429 fails over).
export type Strategy = 'round_robin' | 'fill_first';

export interface RoutingStrategyResponse {
  strategy: Strategy;
}

export interface ClientKey {
  id: string;
  name: string;
  key_prefix: string;
  /** Unix seconds; the row is only listed while current (hard delete). */
  created_at: number;
  /** Unix seconds; omitted when never used. */
  last_used_at?: number | null;
}

export interface CreatedKey {
  id: string;
  key: string;
}

export interface UsageRow {
  day: string;
  client_key_id: string;
  provider_id: string;
  model: string;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  requests: number;
  failures: number;
}

export interface RequestLog {
  id: number;
  timestamp: number;
  client_key_id: string;
  provider_id: string;
  model: string;
  status: number;
  latency_ms: number;
  usage: unknown;
  route_trace: unknown;
  // Logs-page redesign (additive; pre-patch rows omit these).
  conversation_id?: string;
  protocol?: string;
  features?: unknown;
  error_redacted?: string;
  // Display projections from the admin API (raw ids stay the fallback).
  key_name?: string;
  key_prefix?: string;
  provider_name?: string;
  // provider_kind is the provider TYPE's human label (e.g. "Ollama Cloud")
  // — the PROVIDER column; provider_name is the account name the operator
  // set — the ACCOUNT column.
  provider_kind?: string;
}

// RunningRequest is one in-flight request on the SSE stream (a pseudo-row
// at its started_at position; converts to a RequestLog on the log event).
// The id is the tracker's run_… string — never collides with the store's
// numeric rowids. (conv_key lost its consumer 2026-10-08: the gutter's
// conversation-adjacency chain marks were removed — chaining must show
// how REQUESTS relate, not which chat they belong to.)
export interface RunningRequest {
  id: string;
  started_at: number;
  client_key_id: string;
  model: string;
  protocol?: string;
  conversation_id?: string;
  conv_key?: string;
}

// ChildLog is one sub-operation of a request (GET /api/admin/logs →
// `children`, keyed by parent row id): a chain failover, a capability
// (vision) fallback model walk, or an emulated web-tool call. Rendered as
// its own nested row under the parent request.
export interface ChildLog {
  id: number;
  parent_id: number;
  ts: number;
  kind: 'failover' | 'capability_fallback' | 'emulated_tool' | string;
  provider_id?: string;
  model?: string;
  from_ref?: string;
  to_ref?: string;
  detail?: string;
  // Display projections of the refs above (raw refs stay the fallback).
  provider_name?: string;
  provider_kind?: string;
  from_name?: string;
  to_name?: string;
  // What the step cost: token usage in the same shape a request row carries,
  // and its duration. Present for a describer call and a web-tool call, absent
  // for a transition.
  usage?: unknown;
  latency_ms?: number;
  // Inherited from the request this step belongs to: the surface it arrived on
  // and the client key that opened it.
  protocol?: string;
  client_key_id?: string;
  key_name?: string;
  key_prefix?: string;
}

export interface SettingsEntry {
  key: string;
  value: string;
}

// OAuth device-flow payloads (X20): start surfaces the operator pair plus
// the issuer-prescribed poll pace; poll reports pending/approved with the
// account id from the id_token claim. No token material ever rides these.
export interface OAuthStartResult {
  user_code: string;
  verification_url: string;
  interval_seconds: number;
}

export interface OAuthPollResult {
  status: 'pending' | 'approved';
  account_id?: string;
}

// List-response envelopes: {"providers": [...]}, {"models": [...]}, etc.
export interface ProvidersResponse {
  providers: Provider[];
}
export interface ModelsResponse {
  models: Model[];
}
export interface RulesResponse {
  rules: RoutingRule[];
}
export interface KeysResponse {
  keys: ClientKey[];
}
export interface UsageResponse {
  rows: UsageRow[];
}
export interface LogsResponse {
  logs: RequestLog[];
  total: number;
  /** Sub-operations keyed by parent row id (absent when none in the page). */
  children?: Record<string, ChildLog[]>;
  /**
   * True when the sub-operation read failed: the rows carry no children
   * data, and that must not read as "this request had no fallbacks".
   */
  children_unavailable?: boolean;
}
export interface SettingsResponse {
  entries: SettingsEntry[];
}

/**
 * Captured conversation and answer for one log row
 * (GET /api/admin/log-payload?owner=request|child&id=N).
 *
 * found=false means nothing was stored for that row — either it predates the
 * capture or capture was off; capture_enabled tells the two apart, so an empty
 * viewer is never mistaken for an empty request.
 */
export interface LogPayload {
  found: boolean;
  capture_enabled: boolean;
  client_request?: string;
  client_response?: string;
  bytes?: number;
  truncated?: boolean;
}

// ADR-010 capability fallback lists (GET/PUT /api/admin/capability-fallbacks).
export interface CapabilityFallbacks {
  vision: string[];
  web_search: string[];
  web_fetch: string[];
}

// ADR-010 model-sync config + last-run summary
// (GET /api/admin/model-sync, POST /api/admin/model-sync/run).
export interface ModelSyncProviderState {
  provider_id: string;
  added: number;
  updated: number;
  removed: number;
  skipped: number;
  skipped_reason?: string;
}

export interface ModelSyncState {
  ran_at: number;
  providers?: ModelSyncProviderState[];
}

export interface ModelSyncConfig {
  enabled: boolean;
  interval_hours: number;
  last_run: ModelSyncState;
}

export interface ModelSyncRunResponse {
  last_run: ModelSyncState;
}
// Model test round trip (POST /api/admin/models/test/{id}): one minimal
// "Sag OK" completion against the model's provider — the Models page's
// test button. `request` is the exact wire body that went out (redacted,
// bounded; absent when the provider cannot encode a preview), `detail` is
// the upstream's own account of a failure.
export interface ModelTestResponse {
  ok: boolean;
  latency_ms: number;
  reply?: string;
  error?: string;
  request?: string;
  detail?: ModelTestFailureDetail;
}

export interface ModelTestFailureDetail {
  http_status?: number;
  type?: string;
  code?: string;
  message?: string;
}

// ADR-012 Quota page: per-account limit snapshot (one card per account).
// The card carries the account's OWN limits (the scrape) and nothing the
// proxy measured itself — the local month aggregate was retired 2026-10-17
// (ADR-012 amendment).
export interface CreditsUsage {
  remaining?: number | null;
  limit?: number | null;
  reset?: number;
}
export interface LiveUsage {
  session_pct?: number | null;
  session_reset?: number;
  weekly_pct?: number | null;
  weekly_reset?: number;
  /** Firecrawl team credits (remaining/plan + billing-period end). */
  credits?: CreditsUsage | null;
}
export interface AccountUsage {
  account_id: string;
  type: string;
  name: string;
  // The account's pool switch: a disabled account serves no traffic, so the
  // card says that rather than showing a cooldown it cannot earn.
  enabled: boolean;
  live?: LiveUsage | null;
  error?: string;
  health?: string;
  fetched_at?: number;
  /** The production prober can scrape this account type. */
  refreshable?: boolean;
}
export interface AccountUsageResponse {
  accounts: AccountUsage[];
}
