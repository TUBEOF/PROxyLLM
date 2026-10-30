package server

// Retention for captured payloads, request/audit rows and expired sessions.
//
// Capturing a conversation per request is only defensible if something bounds
// how long it is kept. The per-payload size cap bounds one row; this bounds the
// table. Both are needed: a cap without retention grows without limit, and a
// retention window without a cap lets a handful of long sessions dominate the
// disk for as long as the window lasts.
//
// The sweep prunes payloads (request_payload_retention_days), request_logs and
// their child rows (request_log_retention_days) and expired admin sessions.
// The audit row outlives the captured conversation; both windows are
// operator-configurable and the defaults keep the volume bounded.

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

	// defaultRequestLogRetentionDays bounds the audit table itself. The row is
	// the record that a request happened; it must outlive the captured
	// conversation, but not the volume. Operators override it with
	// request_log_retention_days.
	defaultRequestLogRetentionDays = 30

	// payloadRetentionInterval is how often the prune runs. Payloads are
	// diagnostic, so an hour of lag is immaterial — and a long interval keeps
	// the sweep off the request path entirely.
	payloadRetentionInterval = time.Hour

	// payloadRetentionKey overrides defaultPayloadRetentionDays (decimal days).
	payloadRetentionKey = "request_payload_retention_days"

	// requestLogRetentionKey overrides defaultRequestLogRetentionDays.
	requestLogRetentionKey = "request_log_retention_days"
)

// runRetention prunes captured payloads, old request/child rows and expired
// sessions — once at startup and then hourly, until ctx is cancelled.
func runRetention(ctx context.Context, store *storage.Store, logger *slog.Logger) {
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
		} else if n > 0 {
			logger.Info("pruned captured request payloads", "rows", n, "older_than_days", days)
		}

		logDays := defaultRequestLogRetentionDays
		if v, ok, err := store.GetConfigValue(ctx, requestLogRetentionKey); err == nil && ok {
			if n, perr := strconv.Atoi(strings.TrimSpace(v)); perr == nil && n > 0 {
				logDays = n
			}
		}
		logCutoff := time.Now().Add(-time.Duration(logDays) * 24 * time.Hour).Unix()
		if n, err := store.PruneRequestLogs(ctx, logCutoff); err != nil {
			logger.Error("request-log prune failed; rows stay until the next sweep", "error", err)
		} else if n > 0 {
			logger.Info("pruned request logs", "rows", n, "older_than_days", logDays)
		}

		if n, err := store.PurgeExpiredSessions(ctx, time.Now().Unix()); err != nil {
			logger.Error("expired-session prune failed", "error", err)
		} else if n > 0 {
			logger.Info("pruned expired sessions", "rows", n)
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
