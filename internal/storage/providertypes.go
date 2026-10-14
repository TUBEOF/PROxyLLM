package storage

import (
	"context"
	"encoding/json"
	"fmt"
)

// Spec: ADR-009 — per-type shared provider settings and the global routing
// strategy ride the config-KV override layer (the top config layer; same
// storage pattern as model_aliases). Reads degrade to safe defaults on
// corrupted rows (last-known-good contract: never de-route on store
// noise); validation lives at the write boundaries.

// Routing strategy KV key (config override layer).
const routingStrategyKey = "routing_strategy"

// Provider type settings KV key: one JSON object {type: settings-blob}.
const providerTypeSettingsKey = "provider_type_settings"

// DefaultRoutingStrategy is the global default when no override exists
// (ADR-009 decision 3): round robin distributes load across all accounts
// of a type; fill_first is the pre-ADR-009 ordered behavior, opt-in per
// rule or globally.
const DefaultRoutingStrategy = "round_robin"

// GetRoutingStrategy reads the global routing strategy override. A missing
// or invalid value falls back to DefaultRoutingStrategy — the pre-ADR-009
// ordered behavior is an explicit choice, never a silent default, and a
// corrupted row must never de-route the proxy (the model_aliases
// invalid-blob-skip precedent).
func (s *Store) GetRoutingStrategy(ctx context.Context) (string, error) {
	v, ok, err := s.GetConfigValue(ctx, routingStrategyKey)
	if err != nil || !ok {
		return DefaultRoutingStrategy, err
	}
	if !validStrategies[v] {
		return DefaultRoutingStrategy, nil
	}
	return v, nil
}

// SetRoutingStrategy persists the global routing strategy. Invalid values
// are refused at this write boundary — the resolver must never see a
// strategy string it does not know how to apply.
func (s *Store) SetRoutingStrategy(ctx context.Context, strategy string) error {
	if !validStrategies[strategy] {
		return fmt.Errorf("storage: unknown routing strategy %q (round_robin, fill_first): %w", strategy, ErrInvalid)
	}
	return s.SetConfigValue(ctx, routingStrategyKey, strategy)
}

// GetProviderTypeSettings reads the per-type shared settings map. Unset or
// corrupted rows degrade to an empty map — accounts with no shared settings
// behave exactly as pre-ADR-009 (their own config is the whole story).
func (s *Store) GetProviderTypeSettings(ctx context.Context) (map[string]json.RawMessage, error) {
	v, ok, err := s.GetConfigValue(ctx, providerTypeSettingsKey)
	if err != nil || !ok {
		return map[string]json.RawMessage{}, err
	}
	var out map[string]json.RawMessage
	if err := json.Unmarshal([]byte(v), &out); err != nil {
		// Corrupted blob: empty map, never a hard failure (reads must not
		// de-route; the write boundary is where validity is enforced).
		return map[string]json.RawMessage{}, nil
	}
	if out == nil {
		out = map[string]json.RawMessage{}
	}
	return out, nil
}

// PutProviderTypeSettings persists the per-type shared settings map as one
// JSON object. Every value must be valid JSON — a half-typed settings blob
// must fail the save, not land in the KV for every later reader to trip
// over.
func (s *Store) PutProviderTypeSettings(ctx context.Context, settings map[string]json.RawMessage) error {
	for typ, blob := range settings {
		if !json.Valid(blob) {
			return fmt.Errorf("storage: provider type %s settings: not valid JSON: %w", typ, ErrInvalid)
		}
	}
	raw, err := json.Marshal(settings)
	if err != nil {
		return fmt.Errorf("storage: encode provider type settings: %w", err)
	}
	return s.SetConfigValue(ctx, providerTypeSettingsKey, string(raw))
}
