package routing

// Spec: ADR-009 — the routing strategy concept: round robin (rotate the
// expanded candidate list per request, distributing first-position load
// across all accounts of a type) and fill first (today's ordered behavior,
// named: sticky to the first entry until the health gate opens or a 429
// fails over). Per rule, with the global default in the config-KV; an
// empty rule strategy inherits the global one.

// Strategy is the per-request candidate-ordering strategy.
type Strategy string

const (
	// StrategyInherit defers to the global default (config-KV
	// routing_strategy; storage default round_robin). The zero value, so
	// rules written before ADR-009 inherit by construction.
	StrategyInherit Strategy = ""

	// StrategyRoundRobin rotates the candidate list by a per-rule counter
	// before capability and health filtering.
	StrategyRoundRobin Strategy = "round_robin"

	// StrategyFillFirst keeps the chain order — sticky to the first
	// candidate until the health gate opens (breaker cooldown) or a 429
	// exhausts the same-provider retry budget (ADR-005 FO-2/FO-3).
	StrategyFillFirst Strategy = "fill_first"
)

// Resolve returns the effective strategy: the rule's own when set, the
// global default otherwise.
func (s Strategy) Resolve(global Strategy) Strategy {
	if s != StrategyInherit {
		return s
	}
	return global
}