package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// RoutingRule is one persisted routing rule (plan Analysis C schema +
// Analysis E rule model, extended by ADR-009). Chain is the ordered
// fallback chain of provider IDs OR provider types (a type entry expands to
// all registered accounts of that type at resolve time); the router
// resolves it against the live registry at request time.
type RoutingRule struct {
	ID                string
	MatchModel        string
	MatchCapabilities json.RawMessage
	Chain             []string
	Priority          int
	Enabled           bool
	// Strategy is the per-rule routing strategy (ADR-009): "" inherits the
	// global default (config-KV routing_strategy), otherwise "round_robin"
	// or "fill_first".
	Strategy string
}

// validStrategies are the canonical per-rule strategy values. The empty
// string (inherit) is legal and validated separately — the router layer
// owns the default it resolves to.
var validStrategies = map[string]bool{"round_robin": true, "fill_first": true}

// PutRoutingRule upserts one rule (WebUI editor surface). A rule without a
// chain can never resolve and is refused here — at the persistence boundary,
// before the router ever sees it. The strategy value is validated the same
// way: a typo'd strategy must fail the save, never reach the resolver.
func (s *Store) PutRoutingRule(ctx context.Context, r RoutingRule) error {
	if r.ID == "" {
		return fmt.Errorf("routing rule without id: %w", ErrInvalid)
	}
	if len(r.Chain) == 0 {
		return fmt.Errorf("routing rule %s: empty chain: %w", r.ID, ErrInvalid)
	}
	if r.Strategy != "" && !validStrategies[r.Strategy] {
		return fmt.Errorf("routing rule %s: unknown strategy %q (round_robin, fill_first, or empty to inherit): %w",
			r.ID, r.Strategy, ErrInvalid)
	}
	if len(r.MatchCapabilities) == 0 {
		r.MatchCapabilities = json.RawMessage("{}")
	}
	chain, err := json.Marshal(r.Chain)
	if err != nil {
		return fmt.Errorf("storage: encode chain: %w", err)
	}
	enabled := 0
	if r.Enabled {
		enabled = 1
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO routing_rules (id, match_model, match_capabilities, chain, priority, enabled, strategy)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			match_model        = excluded.match_model,
			match_capabilities = excluded.match_capabilities,
			chain              = excluded.chain,
			priority           = excluded.priority,
			enabled            = excluded.enabled,
			strategy           = excluded.strategy`,
		r.ID, r.MatchModel, string(r.MatchCapabilities), string(chain), r.Priority, enabled, r.Strategy)
	return err
}

// ListRoutingRules returns all rules in deterministic order: priority
// descending, ID ascending as the tie-break. The router depends on this
// stability — equal-priority rules must never resolve in insertion-order
// accidents (SQLite's physical row order is not a contract).
func (s *Store) ListRoutingRules(ctx context.Context) ([]RoutingRule, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, match_model, match_capabilities, chain, priority, enabled, strategy
		FROM routing_rules
		ORDER BY priority DESC, id ASC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []RoutingRule
	for rows.Next() {
		var r RoutingRule
		var caps, chain string
		var enabled int
		var matchModel, strategy sql.NullString
		if err := rows.Scan(&r.ID, &matchModel, &caps, &chain, &r.Priority, &enabled, &strategy); err != nil {
			return nil, err
		}
		r.MatchModel = matchModel.String
		r.Strategy = strategy.String
		r.MatchCapabilities = json.RawMessage(caps)
		r.Enabled = enabled != 0
		if err := json.Unmarshal([]byte(chain), &r.Chain); err != nil {
			return nil, fmt.Errorf("storage: decode chain of rule %s: %w", r.ID, err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// DeleteRoutingRule removes one rule. Missing rows are ErrNotFound.
func (s *Store) DeleteRoutingRule(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM routing_rules WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
