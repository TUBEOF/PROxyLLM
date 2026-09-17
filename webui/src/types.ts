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
}

// ADR-009: one account is a providers row of a type; has_credential is a
// presence probe of the encrypted token store (the secret never rides).
export interface Account {
  id: string;
  type: string;
  name: string;
  has_credential: boolean;
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
  auth: 'api_key' | 'oauth';
  settings_fields: ProviderTypeField[];
  account_fields: ProviderTypeField[];
  default_capabilities: Record<string, boolean>;
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
  revoked: boolean;
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
}
export interface SettingsResponse {
  entries: SettingsEntry[];
}