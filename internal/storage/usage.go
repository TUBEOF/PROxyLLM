package storage

import (
	"context"
)

// Spec: plan Analysis C schema — usage_daily aggregates per (day, key,
// provider, model) tuple. The proxy's request-completion path increments;
// the Overview dashboard queries day ranges. Honest empty state: a
// matching-free range returns an empty slice, never an error.

// UsageDaily is one usage-aggregation row.
type UsageDaily struct {
	Day             string // YYYY-MM-DD (UTC)
	ClientKeyID     string
	ProviderID      string
	Model           string
	InputTokens     int64
	OutputTokens    int64
	CacheReadTokens int64
	Requests        int64
	Failures        int64
}

// IncrUsageDaily upserts one aggregation row, ADDING the deltas to any
// existing counters (the PK is the full tuple, so the same tuple always
// accumulates into one row).
func (s *Store) IncrUsageDaily(ctx context.Context, u UsageDaily) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO usage_daily
			(day, client_key_id, provider_id, model,
			 input_tokens, output_tokens, cache_read_tokens, requests, failures)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(day, client_key_id, provider_id, model) DO UPDATE SET
			input_tokens      = input_tokens      + excluded.input_tokens,
			output_tokens     = output_tokens     + excluded.output_tokens,
			cache_read_tokens = cache_read_tokens + excluded.cache_read_tokens,
			requests          = requests          + excluded.requests,
			failures          = failures          + excluded.failures`,
		u.Day, u.ClientKeyID, u.ProviderID, u.Model,
		u.InputTokens, u.OutputTokens, u.CacheReadTokens, u.Requests, u.Failures)
	return err
}

// QueryUsageDaily returns all rows with day in [from, to] (inclusive,
// YYYY-MM-DD strings compare lexicographically), day-ascending so the
// dashboard chart renders a stable time axis.
func (s *Store) QueryUsageDaily(ctx context.Context, dayFrom, dayTo string) ([]UsageDaily, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT day, client_key_id, provider_id, model,
		       input_tokens, output_tokens, cache_read_tokens, requests, failures
		FROM usage_daily
		WHERE day >= ? AND day <= ?
		ORDER BY day ASC, client_key_id ASC, provider_id ASC, model ASC`,
		dayFrom, dayTo)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []UsageDaily
	for rows.Next() {
		var u UsageDaily
		if err := rows.Scan(&u.Day, &u.ClientKeyID, &u.ProviderID, &u.Model,
			&u.InputTokens, &u.OutputTokens, &u.CacheReadTokens, &u.Requests, &u.Failures); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// QueryUsageDailyByProvider is the provider-scoped variant (ADR-012): the
// Quota page's local windows aggregate ONE account's day rows per window.
// Same ordering contract as QueryUsageDaily.
func (s *Store) QueryUsageDailyByProvider(ctx context.Context, providerID, dayFrom, dayTo string) ([]UsageDaily, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT day, client_key_id, provider_id, model,
		       input_tokens, output_tokens, cache_read_tokens, requests, failures
		FROM usage_daily
		WHERE provider_id = ? AND day >= ? AND day <= ?
		ORDER BY day ASC, client_key_id ASC, provider_id ASC, model ASC`,
		providerID, dayFrom, dayTo)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []UsageDaily
	for rows.Next() {
		var u UsageDaily
		if err := rows.Scan(&u.Day, &u.ClientKeyID, &u.ProviderID, &u.Model,
			&u.InputTokens, &u.OutputTokens, &u.CacheReadTokens, &u.Requests, &u.Failures); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}
