package server

// Capability-fallback wiring (2026-09-21 automatic capabilities, ADR-010
// amendment): the stored vision_fallback KV holds MODEL IDs (routing
// identities, "ollamacloud/glm-5.3"); the executor needs fully resolved
// (provider, wire-model) chain entries. This file owns that translation:
// store.GetModel per ID → ChainEntry{ProviderID, Model: row.Name}, with
// last-known-good semantics — unknown IDs are skipped with a log line
// (v1.0.2 lists of provider IDs/Types do not resolve as model rows and are
// dropped until the operator re-saves), a store read failure leaves the
// live fallback untouched. Learning rides the same pair: the executor's
// OnCapabilityGap carries the chain entry, the storage layer locates the
// row by (provider, wire name).

import (
	"context"
	"log/slog"
	"time"

	"github.com/tubeof/proxyllm/internal/routing"
	"github.com/tubeof/proxyllm/internal/storage"
)

// resolveCapabilityFallbacks translates the stored vision_fallback model
// IDs into executor chain entries. Unknown model IDs are skipped with a
// log line (never a de-route of the whole list); the store read failure is
// returned for the caller's last-known-good policy.
func resolveCapabilityFallbacks(ctx context.Context, store *storage.Store, logger *slog.Logger) ([]routing.ChainEntry, error) {
	ids, err := store.GetVisionFallback(ctx)
	if err != nil {
		return nil, err
	}
	entries := make([]routing.ChainEntry, 0, len(ids))
	for _, id := range ids {
		row, err := store.GetModel(ctx, id)
		if err != nil {
			// The log rides the redacting handler (all logging does); the
			// model ID itself is operator data, not a secret.
			if logger != nil {
				logger.Warn("capability fallback entry is not a known model — skipping", "model", id)
			}
			continue
		}
		entries = append(entries, routing.ChainEntry{ProviderID: row.ProviderID, Model: row.Name})
	}
	return entries, nil
}

// learnTimeout bounds one learning write; a slow store must not pile up
// goroutines behind the failover path.
const learnTimeout = 5 * time.Second

// learnCapabilityGap persists one upstream capability-gap observation into
// the model row (vision=false learning). It is the executor's
// OnCapabilityGap callback: it runs on its own goroutine with a bounded
// context so the failover path never blocks on the store, and failures are
// logged, never surfaced into the request.
func (s *Server) learnCapabilityGap(entry routing.ChainEntry) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), learnTimeout)
		defer cancel()
		if err := s.store.MergeLearnedCapability(ctx, entry.ProviderID, entry.Model, "vision", false); err != nil {
			s.logger.Error("capability-gap learning failed (model row left unchanged)",
				"provider", entry.ProviderID, "error", err)
		}
	}()
}
