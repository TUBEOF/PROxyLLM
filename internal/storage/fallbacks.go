package storage

import (
	"context"
	"encoding/json"
	"fmt"
)

// Spec: ADR-010 — capability fallback lists ride the config-KV override
// layer (same storage pattern as routing_strategy / model_aliases in
// providertypes.go). Reads degrade to safe defaults on corrupted rows
// (last-known-good contract: never de-route on store noise); validation
// lives at the write boundaries (admin API), never in storage.

// KV keys for the capability fallback lists and the model-sync config/state.
const (
	// visionFallbackKey holds the vision fallback chain as a JSON array of
	// provider IDs or provider types — appended to the resolved chain when
	// a request needs vision and the rule chain filtered to nothing.
	visionFallbackKey = "vision_fallback"

	// webSearchFallbackKey / webFetchFallbackKey hold the web-tool emulator
	// accounts (provider IDs of web_tool-kind accounts) per tool family as
	// JSON arrays — the EmulatorPool walks them in order.
	webSearchFallbackKey = "web_search_fallback"
	webFetchFallbackKey  = "web_fetch_fallback"

	// modelSyncEnabledKey holds "0"/"1" (default "1").
	modelSyncEnabledKey = "model_sync_enabled"

	// modelSyncIntervalKey holds the sync interval in hours as a decimal
	// integer (default 24).
	modelSyncIntervalKey = "model_sync_interval_hours"

	// modelSyncStateKey holds the last-run summary as a JSON object.
	modelSyncStateKey = "model_sync_state"
)

// RequestPayloadLoggingKey is the switch for capturing each request's
// conversation and answer beside its log row (Logs page). "0"
// disables it; absent means on — being able to read what a request said is the
// point of the capture, and the switch exists so an operator can turn it off
// rather than have to live without it.
//
// Exported because two packages read it: the request path decides whether to
// capture, and the admin API reports whether capture is on so the log drawer can
// tell "off" from "nothing captured here".
const RequestPayloadLoggingKey = "request_logging_enabled"

// DefaultModelSyncIntervalHours is the sync cadence when no override
// exists.
const DefaultModelSyncIntervalHours = 24

// ModelSyncState is the persisted summary of the last sync run (per
// provider counts + wall-clock stamps; ADR-010 observability contract).
type ModelSyncState struct {
	// RanAt is the last-run unix timestamp (0 = never ran).
	RanAt int64 `json:"ran_at"`

	// Providers carries the per-account outcome.
	Providers []ModelSyncProviderState `json:"providers,omitempty"`
}

// ModelSyncProviderState is one account's last-sync outcome.
type ModelSyncProviderState struct {
	ProviderID string `json:"provider_id"`
	// Added / Updated / Removed count the upserts and deletes.
	Added   int `json:"added"`
	Updated int `json:"updated"`
	Removed int `json:"removed"`
	// Skipped is nonzero when the account's upstream list was unavailable
	// (transport error, unsupported endpoint) — the reason rides SkippedReason.
	Skipped       int    `json:"skipped"`
	SkippedReason string `json:"skipped_reason,omitempty"`
}

// fallbackList reads one stored JSON array of strings; a missing or
// corrupted row degrades to nil (never de-route on store noise — the
// providertypes.go last-known-good contract).
func (s *Store) fallbackList(key string) ([]string, error) {
	v, ok, err := s.GetConfigValue(context.Background(), key)
	if err != nil || !ok {
		return nil, err
	}
	var out []string
	if err := json.Unmarshal([]byte(v), &out); err != nil {
		// Corrupted blob: empty list, never a hard failure — the write
		// boundary is where validity is enforced.
		return nil, nil
	}
	return out, nil
}

// putFallbackList persists one fallback list as a JSON array. A nil list
// stores "[]" (explicitly empty — the operator cleared it), never an absent
// key (which would read as "use the default" and resurrect the old list).
func (s *Store) putFallbackList(ctx context.Context, key string, entries []string) error {
	if entries == nil {
		entries = []string{}
	}
	raw, err := json.Marshal(entries)
	if err != nil {
		return fmt.Errorf("storage: encode %s: %w", key, err)
	}
	return s.SetConfigValue(ctx, key, string(raw))
}

// GetVisionFallback reads the vision fallback chain (provider IDs/types in
// priority order). Missing/corrupted → nil (no fallback — the documented
// ErrNoRoute behavior).
func (s *Store) GetVisionFallback(ctx context.Context) ([]string, error) {
	return s.fallbackList(visionFallbackKey)
}

// PutVisionFallback persists the vision fallback chain.
func (s *Store) PutVisionFallback(ctx context.Context, entries []string) error {
	return s.putFallbackList(ctx, visionFallbackKey, entries)
}

// WebToolFallbacks holds the per-family web-tool emulator account lists.
type WebToolFallbacks struct {
	// Search/Fetch are provider IDs of web_tool accounts in priority order.
	Search []string
	Fetch  []string
}

// GetWebToolFallbacks reads both web-tool fallback lists. Missing/corrupted
// rows degrade to nil lists (single-ENV-emulator behavior unchanged).
func (s *Store) GetWebToolFallbacks(ctx context.Context) (WebToolFallbacks, error) {
	search, err := s.fallbackList(webSearchFallbackKey)
	if err != nil {
		return WebToolFallbacks{}, err
	}
	fetch, err := s.fallbackList(webFetchFallbackKey)
	if err != nil {
		return WebToolFallbacks{}, err
	}
	return WebToolFallbacks{Search: search, Fetch: fetch}, nil
}

// PutWebToolFallbacks persists both web-tool fallback lists.
func (s *Store) PutWebToolFallbacks(ctx context.Context, f WebToolFallbacks) error {
	if err := s.putFallbackList(ctx, webSearchFallbackKey, f.Search); err != nil {
		return err
	}
	return s.putFallbackList(ctx, webFetchFallbackKey, f.Fetch)
}

// GetModelSyncEnabled reads the sync toggle. A missing value is enabled —
// the sync is default-ON, the documented operator opt-out is the explicit
// "0".
func (s *Store) GetModelSyncEnabled(ctx context.Context) (bool, error) {
	v, ok, err := s.GetConfigValue(ctx, modelSyncEnabledKey)
	if err != nil || !ok {
		return true, err
	}
	return v != "0", nil
}

// SetModelSyncEnabled persists the sync toggle ("0"/"1" only).
func (s *Store) SetModelSyncEnabled(ctx context.Context, enabled bool) error {
	v := "1"
	if !enabled {
		v = "0"
	}
	return s.SetConfigValue(ctx, modelSyncEnabledKey, v)
}

// GetModelSyncIntervalHours reads the sync cadence override. Missing or
// non-positive values fall back to DefaultModelSyncIntervalHours.
func (s *Store) GetModelSyncIntervalHours(ctx context.Context) (int, error) {
	v, ok, err := s.GetConfigValue(ctx, modelSyncIntervalKey)
	if err != nil || !ok {
		return DefaultModelSyncIntervalHours, err
	}
	var n int
	if _, err := fmt.Sscanf(v, "%d", &n); err != nil || n <= 0 {
		return DefaultModelSyncIntervalHours, nil
	}
	return n, nil
}

// SetModelSyncIntervalHours persists the sync cadence (positive hours
// only — the scheduler's Ticker panics on non-positive intervals).
func (s *Store) SetModelSyncIntervalHours(ctx context.Context, hours int) error {
	if hours <= 0 {
		return fmt.Errorf("storage: model sync interval must be positive: %w", ErrInvalid)
	}
	return s.SetConfigValue(ctx, modelSyncIntervalKey, fmt.Sprintf("%d", hours))
}

// GetModelSyncState reads the last-run summary. Missing/corrupted → zero
// value (never ran), never a hard failure.
func (s *Store) GetModelSyncState(ctx context.Context) (ModelSyncState, error) {
	v, ok, err := s.GetConfigValue(ctx, modelSyncStateKey)
	if err != nil || !ok {
		return ModelSyncState{}, err
	}
	var out ModelSyncState
	if err := json.Unmarshal([]byte(v), &out); err != nil {
		return ModelSyncState{}, nil
	}
	return out, nil
}

// PutModelSyncState persists the last-run summary.
func (s *Store) PutModelSyncState(ctx context.Context, st ModelSyncState) error {
	raw, err := json.Marshal(st)
	if err != nil {
		return fmt.Errorf("storage: encode model sync state: %w", err)
	}
	return s.SetConfigValue(ctx, modelSyncStateKey, string(raw))
}
