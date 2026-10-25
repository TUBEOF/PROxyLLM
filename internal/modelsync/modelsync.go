// Package modelsync implements the automatic model import (ADR-010): every
// registry provider exposing the ModelLister surface has its upstream model
// list mirrored into the models table. The sync policy follows ADR-010:
// upstream-added models land as origin="synced" rows, operator
// edits to existing rows (capabilities/compat) are preserved, and synced
// rows the upstream dropped are deleted — manual rows are never touched.
//
// Model identity: a synced row's ID is
// "<prefix>/<upstream-name>" where the prefix is the provider TYPE's
// model_prefix from the shared type settings (default: the type string,
// e.g. "ollama-cloud/glm-5.3"). The Name column carries the bare upstream
// wire name — what the upstream request body's model field must contain.
// Unprefixed legacy rows keep working (no migration): their wire name
// equals their ID.
package modelsync

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/tubeof/proxyllm/internal/provider"
	"github.com/tubeof/proxyllm/internal/storage"
)

// Per-provider upstream call timeout: a hung endpoint delays the sync, not
// the proxy — each account gets its own bounded window.
const perProviderTimeout = 30 * time.Second

// maxPrefixLen bounds the operator-set model prefix (sanity, not security).
const maxPrefixLen = 64

// Syncer mirrors upstream model lists into the store (ADR-010).
type Syncer struct {
	store  *storage.Store
	reg    provider.Registry
	logger *slog.Logger

	// now is injectable for tests (state stamps).
	now func() time.Time
}

// New returns a Syncer over store + registry.
func New(store *storage.Store, reg provider.Registry, logger *slog.Logger) *Syncer {
	if logger == nil {
		logger = slog.Default()
	}
	return &Syncer{store: store, reg: reg, logger: logger, now: time.Now}
}

// SetNow overrides the clock (tests).
func (s *Syncer) SetNow(now func() time.Time) { s.now = now }

// Run syncs every ModelLister provider once and persists the per-provider
// summary into the model_sync_state KV. Individual provider failures are
// recorded (skip + reason) — the run itself only fails on store errors,
// which the caller (scheduler / admin API) logs.
//
// Policy details:
//   - a synced row's ID is "<prefix>/<upstream-name>" (prefix = the type's
//     model_prefix, default the type string); the Name column is the bare
//     upstream wire name. An ID colliding with an existing row of a
//     DIFFERENT provider type is skipped (logged) — the operator's row
//     keeps its identity. A collision within the same type (another
//     account of the type already synced it) is harmless: the row is
//     refreshed in place.
//   - an existing row of the same provider keeps its capabilities/compat/
//     priority (operator edits) and only refreshes its name.
//   - synced rows absent from the upstream list are deleted; manual rows
//     survive regardless (storage.DeleteModelsNotIn refuses non-synced
//     origins by construction).
func (s *Syncer) Run(ctx context.Context) (storage.ModelSyncState, error) {
	state := storage.ModelSyncState{RanAt: s.now().Unix()}

	// One type-settings read for the whole run: every account of a type
	// shares the prefix, and the per-run snapshot keeps one run consistent
	// even if a WebUI write lands mid-run (the next run picks it up).
	prefixes := map[string]string{}
	for _, p := range s.reg.List() {
		tp, ok := p.(provider.TypedProvider)
		if !ok {
			continue
		}
		typ := tp.Type()
		if _, seen := prefixes[typ]; seen {
			continue
		}
		prefixes[typ] = s.typePrefix(ctx, typ)
	}

	for _, p := range s.reg.List() {
		lister, ok := p.(provider.ModelLister)
		if !ok {
			continue
		}
		typ := ""
		if tp, ok := p.(provider.TypedProvider); ok {
			typ = tp.Type()
		}
		ps := s.syncProvider(ctx, p.ID(), prefixes[typ], lister)
		state.Providers = append(state.Providers, ps)
	}

	if err := s.store.PutModelSyncState(ctx, state); err != nil {
		return state, err
	}
	return state, nil
}

// typePrefix resolves the type's model prefix from the shared type settings
// (key "model_prefix"); the default is the type string itself. Read errors
// and malformed blobs degrade to the default — the sync must not fail on
// store noise (the write boundary enforces the shape).
func (s *Syncer) typePrefix(ctx context.Context, typ string) string {
	if typ == "" {
		return ""
	}
	settings, err := s.store.GetProviderTypeSettings(ctx)
	if err != nil {
		return typ
	}
	raw := settings[typ]
	if len(raw) == 0 {
		return typ
	}
	var cfg struct {
		ModelPrefix string `json:"model_prefix"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return typ
	}
	prefix := strings.TrimSpace(cfg.ModelPrefix)
	if prefix == "" || len(prefix) > maxPrefixLen {
		return typ
	}
	// Defense in depth: re-validate the shape the write boundary
	// (providervalid.go) enforces — a hand-edited or bypassed blob must not
	// corrupt the model identity (a "/" in the prefix would build row IDs
	// with extra slashes). An invalid prefix degrades to the default type
	// string, exactly like store noise.
	if prefix != strings.ToLower(prefix) {
		return typ
	}
	for _, r := range prefix {
		if !validPrefixRune(r) {
			return typ
		}
	}
	return prefix
}

// validPrefixRune reports whether r is allowed in a model prefix (the same
// rune set the admin write boundary enforces: lowercase letters, digits,
// hyphens).
func validPrefixRune(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-'
}

// syncProvider mirrors one provider's upstream list. Failures are encoded
// into the returned state (skipped=1 + reason), never returned as errors —
// one dead account must not block the others. prefix is the type's model
// prefix; empty means unprefixed legacy behavior (row ID = wire name).
func (s *Syncer) syncProvider(ctx context.Context, providerID, prefix string, lister provider.ModelLister) storage.ModelSyncProviderState {
	ps := storage.ModelSyncProviderState{ProviderID: providerID}

	pctx, cancel := context.WithTimeout(ctx, perProviderTimeout)
	defer cancel()

	upstream, err := lister.ListModels(pctx)
	if err != nil {
		ps.Skipped = 1
		ps.SkippedReason = err.Error()
		s.logger.Warn("model sync: upstream list unavailable — skipping provider",
			"provider_id", providerID, "error", err)
		return ps
	}

	existing, err := s.store.ListModelsByProvider(ctx, providerID)
	if err != nil {
		ps.Skipped = 1
		ps.SkippedReason = err.Error()
		s.logger.Error("model sync: model rows unreadable — skipping provider",
			"provider_id", providerID, "error", err)
		return ps
	}
	byID := make(map[string]storage.Model, len(existing))
	for _, m := range existing {
		byID[m.ID] = m
	}

	upstreamIDs := make([]string, 0, len(upstream))
	for _, info := range upstream {
		// Identity: prefixed ID for the store, bare wire name upstream.
		rowID := info.Name
		if prefix != "" {
			rowID = prefix + "/" + info.Name
		}
		upstreamIDs = append(upstreamIDs, rowID)
		row, ok := byID[rowID]
		if !ok {
			// New upstream model — but its ID may collide with another
			// provider's row (model IDs are the global primary key). A
			// collision within the same type is harmless (the row belongs
			// to a sibling account and is refreshed below); a collision
			// with a different type is skipped: the operator's row keeps
			// its identity.
			if other, err := s.store.GetModel(ctx, rowID); err == nil {
				if other.ProviderID != providerID {
					s.logger.Warn("model sync: model id already owned by another provider — skipping",
						"provider_id", providerID, "model_id", rowID, "owner", other.ProviderID)
					continue
				}
			} else if !isNotFound(err) {
				ps.Skipped = 1
				ps.SkippedReason = err.Error()
				s.logger.Error("model sync: model lookup failed — skipping provider",
					"provider_id", providerID, "error", err)
				return ps
			}
			if err := s.store.PutModel(ctx, mergeDetectedNewRow(storage.Model{
				ID: rowID, ProviderID: providerID, Name: info.Name,
				Origin: storage.ModelSyncedOrigin,
			}, info)); err != nil {
				ps.Skipped = 1
				ps.SkippedReason = err.Error()
				s.logger.Error("model sync: model insert failed — skipping provider",
					"provider_id", providerID, "model_id", rowID, "error", err)
				return ps
			}
			ps.Added++
			continue
		}
		// Existing row: refresh only the name — capabilities/compat/
		// priority are operator edits and survive (ADR-010 policy).
		//
		// Detection persistence: rows synced by an earlier run merge the detected
		// capabilities/context into EMPTY blobs only, plus context_window
		// into a compat blob without a non-zero pin (operator pins win).
		// Manual rows are never touched by detection — the merge below
		// keys on the row's origin BEFORE the name-refresh conversion.
		wasSynced := row.Origin == storage.ModelSyncedOrigin
		if row.Name != info.Name || row.Origin != storage.ModelSyncedOrigin {
			// Manual rows converted by the upstream re-offering the same
			// name keep their settings but become synced-tracked; name
			// refresh is a no-op for identical strings.
			row.Name = info.Name
			row.Origin = storage.ModelSyncedOrigin
			if err := s.store.PutModel(ctx, row); err != nil {
				ps.Skipped = 1
				ps.SkippedReason = err.Error()
				s.logger.Error("model sync: model update failed — skipping provider",
					"provider_id", providerID, "model_id", rowID, "error", err)
				return ps
			}
			ps.Updated++
		}
		if wasSynced {
			if merged, changed := mergeDetected(row, info); changed {
				// Post-merge logging reads the transition, not the old row.
				if err := s.store.PutModel(ctx, merged); err != nil {
					ps.Skipped = 1
					ps.SkippedReason = err.Error()
					s.logger.Error("model sync: detection merge failed — skipping provider",
						"provider_id", providerID, "model_id", rowID, "error", err)
					return ps
				}
				ps.Updated++
			}
		}
	}

	// Drop synced rows the upstream no longer lists. The store refuses any
	// origin other than "synced" here — manual rows survive by construction.
	if err := s.store.DeleteModelsNotIn(ctx, providerID, upstreamIDs, storage.ModelSyncedOrigin); err != nil {
		ps.Skipped = 1
		ps.SkippedReason = err.Error()
		s.logger.Error("model sync: stale-model delete failed — skipping provider",
			"provider_id", providerID, "error", err)
		return ps
	}
	// Removed is derived: the delete ran without error, so everything not in
	// the upstream list and previously synced is gone; the exact count is
	// observable in the WebUI via the before/after lists. For an honest
	// counter we re-read the remaining rows.
	remaining, err := s.store.ListModelsByProvider(ctx, providerID)
	if err != nil {
		s.logger.Warn("model sync: post-delete re-read failed — removed count unverified",
			"provider_id", providerID, "error", err)
		return ps
	}
	ps.Removed = len(existing) + ps.Added - len(remaining)
	ps.Removed = max(ps.Removed, 0)
	return ps
}

// isNotFound reports whether err is the store's ErrNotFound.
func isNotFound(err error) bool {
	return errors.Is(err, storage.ErrNotFound)
}
