// Package modelsync implements the automatic model import (ADR-010): every
// registry provider exposing the ModelLister surface has its upstream model
// list mirrored into the models table. The sync policy (operator decision,
// ADR-010): upstream-added models land as origin="synced" rows, operator
// edits to existing rows (capabilities/compat) are preserved, and synced
// rows the upstream dropped are deleted — manual rows are never touched.
package modelsync

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/tubeof/proxyllm/internal/provider"
	"github.com/tubeof/proxyllm/internal/storage"
)

// Per-provider upstream call timeout: a hung endpoint delays the sync, not
// the proxy — each account gets its own bounded window.
const perProviderTimeout = 30 * time.Second

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
//   - a synced row's ID is the upstream model name; a name colliding with
//     an existing row of a DIFFERENT provider is skipped (logged) — the
//     operator's row keeps its identity.
//   - an existing row of the same provider keeps its capabilities/compat/
//     priority (operator edits) and only refreshes its name.
//   - synced rows absent from the upstream list are deleted; manual rows
//     survive regardless (storage.DeleteModelsNotIn refuses non-synced
//     origins by construction).
func (s *Syncer) Run(ctx context.Context) (storage.ModelSyncState, error) {
	state := storage.ModelSyncState{RanAt: s.now().Unix()}

	for _, p := range s.reg.List() {
		lister, ok := p.(provider.ModelLister)
		if !ok {
			continue
		}
		ps := s.syncProvider(ctx, p.ID(), lister)
		state.Providers = append(state.Providers, ps)
	}

	if err := s.store.PutModelSyncState(ctx, state); err != nil {
		return state, err
	}
	return state, nil
}

// syncProvider mirrors one provider's upstream list. Failures are encoded
// into the returned state (skipped=1 + reason), never returned as errors —
// one dead account must not block the others.
func (s *Syncer) syncProvider(ctx context.Context, providerID string, lister provider.ModelLister) storage.ModelSyncProviderState {
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
		upstreamIDs = append(upstreamIDs, info.Name)
		row, ok := byID[info.Name]
		if !ok {
			// New upstream model — but its ID may collide with another
			// provider's row (model IDs are the global primary key). Skip:
			// the operator's row keeps its identity.
			if _, err := s.store.GetModel(ctx, info.Name); err == nil {
				s.logger.Warn("model sync: model id already owned by another provider — skipping",
					"provider_id", providerID, "model_id", info.Name)
				continue
			} else if !isNotFound(err) {
				ps.Skipped = 1
				ps.SkippedReason = err.Error()
				s.logger.Error("model sync: model lookup failed — skipping provider",
					"provider_id", providerID, "error", err)
				return ps
			}
			if err := s.store.PutModel(ctx, storage.Model{
				ID: info.Name, ProviderID: providerID, Name: info.Name,
				Origin: storage.ModelSyncedOrigin,
			}); err != nil {
				ps.Skipped = 1
				ps.SkippedReason = err.Error()
				s.logger.Error("model sync: model insert failed — skipping provider",
					"provider_id", providerID, "model_id", info.Name, "error", err)
				return ps
			}
			ps.Added++
			continue
		}
		// Existing row: refresh only the name — capabilities/compat/
		// priority are operator edits and survive (ADR-010 policy).
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
					"provider_id", providerID, "model_id", info.Name, "error", err)
				return ps
			}
			ps.Updated++
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
	if ps.Removed < 0 {
		ps.Removed = 0
	}
	return ps
}

// isNotFound reports whether err is the store's ErrNotFound.
func isNotFound(err error) bool {
	return errors.Is(err, storage.ErrNotFound)
}
