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