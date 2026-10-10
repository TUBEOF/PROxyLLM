package server

// Retention for captured request payloads (2026-10-10 Logs page).
//
// Capturing a conversation per request is only defensible if something bounds
// how long it is kept. The per-payload size cap bounds one row; this bounds the
// table. Both are needed: a cap without retention grows without limit, and a
// retention window without a cap lets a handful of long sessions dominate the
// disk for as long as the window lasts.
//
// Pruning removes payloads, never the request_logs row beside them. The row is
// the record that a request happened — the audit trail — and it must outlive the
// conversation that was captured for reading.

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/tubeof/proxyllm/internal/storage"
)

const (
	// defaultPayloadRetentionDays is how long a captured conversation is kept
	// when the operator has not said otherwise. A week covers the "what did
	// that request look like last Tuesday" question without turning the
	// database into an archive of every conversation the proxy ever carried.
	defaultPayloadRetentionDays = 7

	// payloadRetentionInterval is how often the prune runs. Payloads are
	// diagnostic, so an hour of lag is immaterial — and a long interval keeps
	// the sweep off the request path entirely.
	payloadRetentionInterval = time.Hour

	// payloadRetentionKey overrides defaultPayloadRetentionDays (decimal days).
	payloadRetentionKey = "request_payload_retention_days"
)

// runPayloadRetention prunes captured payloads older than the configured window,
// once at startup and then hourly, until ctx is cancelled.
func runPayloadRetention(ctx context.Context, store *storage.Store, logger *slog.Logger) {
	prune := func() {
		days := defaultPayloadRetentionDays
		// A malformed or unreadable setting keeps the default: store noise must
		// not silently turn retention off.
		if v, ok, err := store.GetConfigValue(ctx, payloadRetentionKey); err == nil && ok {
			if n, perr := strconv.Atoi(strings.TrimSpace(v)); perr == nil && n > 0 {
				days = n
			}
		}
		cutoff := time.Now().Add(-time.Duration(days) * 24 * time.Hour).Unix()
		n, err := store.PruneRequestPayloads(ctx, cutoff)
		if err != nil {
			logger.Error("captured-payload prune failed; rows stay until the next sweep", "error", err)
			return
		}
		if n > 0 {
			logger.Info("pruned captured request payloads", "rows", n, "older_than_days", days)
		}
	}

	prune()
	ticker := time.NewTicker(payloadRetentionInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			prune()
		}
	}
}
