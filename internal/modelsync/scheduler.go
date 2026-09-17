package modelsync

import (
	"context"
	"log/slog"
	"time"

	"github.com/tubeof/proxyllm/internal/storage"
)

// Scheduler runs the sync on a wall-clock cadence (ADR-010): one run at
// startup, then a ticker per the configured interval. The loop is
// context-bound — server shutdown cancels it; nothing leaks goroutines.
type Scheduler struct {
	syncer *Syncer
	store  *storage.Store
	logger *slog.Logger
}

// NewScheduler returns a Scheduler over the syncer. The interval is read
// from the store on every tick decision, so a WebUI interval write takes
// effect without restart.
func NewScheduler(syncer *Syncer, store *storage.Store, logger *slog.Logger) *Scheduler {
	return &Scheduler{syncer: syncer, store: store, logger: logger}
}

// Run blocks until ctx is canceled. Each iteration checks the enable
// switch (model_sync_enabled KV — the documented opt-out), so a toggle
// write is effective at the next tick boundary. Startup runs the sync once
// regardless of the tick cadence (fresh data before first traffic), unless
// the sync is disabled.
func (s *Scheduler) Run(ctx context.Context) {
	hours, err := s.store.GetModelSyncIntervalHours(ctx)
	if err != nil {
		s.logger.Error("model sync: interval unreadable — using default", "error", err)
		hours = 24
	}
	ticker := time.NewTicker(time.Duration(hours) * time.Hour)
	defer ticker.Stop()

	s.tick(ctx) // startup run

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Re-read the interval: a WebUI write reshapes the cadence.
			h, err := s.store.GetModelSyncIntervalHours(ctx)
			if err == nil && h != hours {
				hours = h
				ticker.Reset(time.Duration(hours) * time.Hour)
			}
			s.tick(ctx)
		}
	}
}

// tick runs one sync unless the operator disabled it. Failures are logged,
// never fatal — the proxy keeps serving.
func (s *Scheduler) tick(ctx context.Context) {
	enabled, err := s.store.GetModelSyncEnabled(ctx)
	if err != nil {
		s.logger.Error("model sync: enable-switch unreadable — skipping this tick", "error", err)
		return
	}
	if !enabled {
		return
	}
	state, err := s.syncer.Run(ctx)
	if err != nil {
		s.logger.Error("model sync: run failed", "error", err)
		return
	}
	for _, ps := range state.Providers {
		if ps.Skipped > 0 {
			s.logger.Warn("model sync: provider skipped",
				"provider_id", ps.ProviderID, "reason", ps.SkippedReason)
			continue
		}
		s.logger.Info("model sync: provider synced",
			"provider_id", ps.ProviderID,
			"added", ps.Added, "updated", ps.Updated, "removed", ps.Removed)
	}
}
