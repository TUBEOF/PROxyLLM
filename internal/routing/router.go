package routing

import (
	"errors"
	"sort"
	"strings"
	"sync"

	"github.com/tubeof/proxyllm/internal/provider"
)

// Rule is one routing rule (Analysis E rule model; storage:
// routing_rules table, extended by ADR-009). The deterministic resolution
// order is: key scope → model match → chain expansion → strategy →
// capability filter → health gate.
type Rule struct {
	// ID is the rule's storage identity. It keys the round-robin rotation
	// counters (per-rule, ADR-009) — empty ID rules share one counter.
	ID string

	// MatchModel is the model pattern to match: exact name or wildcard
	// with a single trailing `*` (e.g. `claude-*`).
	MatchModel string

	// Chain is the ordered fallback chain of provider IDs OR provider
	// types. A type entry expands to all credentialed accounts of that
	// type at resolve time (ADR-009); an exact provider ID always wins
	// over a type of the same name.
	Chain []string

	// Priority orders rules with overlapping matches; higher wins.
	// Rules of equal priority resolve in registration order.
	Priority int

	// Enabled gates the rule; disabled rules are skipped.
	Enabled bool

	// Strategy is the per-rule candidate-ordering strategy (ADR-009):
	// empty inherits GlobalStrategy, otherwise round_robin or fill_first.
	Strategy Strategy
}

// ChainEntry is one resolved fallback step: the provider and the model to
// request on it.
type ChainEntry struct {
	ProviderID string
	Model      string
}

// Router resolves requests to deterministic fallback chains (plan
// Analysis E: deterministic and testable; score/weight fuzzy routing was
// rejected as primary mechanism precisely because it is neither).
// ADR-009 restates determinism per request sequence: the round-robin
// counters are the only sequence-dependent state, per-rule and observable.
type Router struct {
	cfg    Config
	reg    provider.Registry
	health *HealthTracker

	// mu guards rules and the round-robin counters — the live rule set is
	// replaceable at runtime (WebUI routing writes reload it). Readers take
	// RLock; the reload path takes Lock for one whole-set swap. Aliases and
	// KeyScopes stay startup-fixed: only rules are DB-editable today.
	mu         sync.RWMutex
	rules      []Rule
	rrCounters map[string]uint64
}

// Config carries the routing inputs.
type Config struct {
	// Rules are the routing rules, priority-ordered at resolution time.
	Rules []Rule

	// Aliases map requested model names to configured primary models
	// (Claude Code alias resolution, C6).
	Aliases map[string]string

	// KeyScopes maps client key ID → allowed model patterns; a key with
	// no entry is unrestricted (Analysis E step 1).
	KeyScopes map[string][]string

	// GlobalStrategy is the strategy for rules whose own Strategy is
	// StrategyInherit (ADR-009; config-KV routing_strategy, storage
	// default round_robin). Swappable at runtime via SetStrategy.
	GlobalStrategy Strategy
}

// Option configures a Router.
type Option func(*Router)

// WithRegistry sets the provider registry the capability filter and
// health gate consult.
func WithRegistry(reg provider.Registry) Option {
	return func(r *Router) { r.reg = reg }
}

// WithHealth sets the health tracker the health gate consults.
func WithHealth(h *HealthTracker) Option {
	return func(r *Router) { r.health = h }
}

// NewRouter returns a Router for cfg.
func NewRouter(cfg Config, opts ...Option) *Router {
	r := &Router{cfg: cfg, rules: cfg.Rules, rrCounters: make(map[string]uint64)}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// SetRules atomically replaces the live rule set (the admin API's reload
// hook: WebUI routing writes push the freshly stored rules into the router
// without restart). The caller keeps ownership of its input slice; the
// swap stores it as one whole set.
func (r *Router) SetRules(rules []Rule) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rules = rules
}

// SetAliases atomically replaces the live alias map (the admin API's
// reload hook: WebUI settings writes push the freshly stored alias map
// into the router without restart — same contract as SetRules).
func (r *Router) SetAliases(aliases map[string]string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cfg.Aliases = aliases
}

// SetStrategy atomically replaces the live global strategy (the admin
// API's reload hook for the routing_strategy KV write — same live-swap
// contract as SetRules/SetAliases). Per-rule strategies ride SetRules.
func (r *Router) SetStrategy(s Strategy) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cfg.GlobalStrategy = s
}

// ResolveOptions carries per-request resolution modifiers.
type ResolveOptions struct {
	// KeyID is the client key; its scope gates the request (step 1).
	KeyID string
}

// WithKeyID resolves for a specific client key.
func WithKeyID(keyID string) ResolveOptions {
	return ResolveOptions{KeyID: keyID}
}

// ResolveChainFor resolves model to its fallback chain filtered by the
// required capabilities and health state. Skipped providers are recorded
// in the returned Trace (fallbacks are never silent).
//
// The Trace is returned by pointer: it embeds a mutex (concurrent-safe —
// the failover loop and the observability writer run on different
// goroutines once a stream is live), and copying a locked value is a bug.
func (r *Router) ResolveChainFor(model string, required provider.Capabilities, opts ...ResolveOptions) ([]ChainEntry, *Trace, error) {
	var o ResolveOptions
	for _, opt := range opts {
		o = opt
	}
	return r.resolveWithTrace(model, required, o.KeyID)
}

// resolveWithTrace runs the deterministic pipeline:
//
//  1. key scope  — the key's allowed patterns must match the model
//  2. alias      — the requested name maps to the configured primary
//  3. rule match — exact before wildcard, priority before order
//  4. capability — skip chain entries lacking a required capability
//  5. health     — skip providers in cooldown
func (r *Router) resolveWithTrace(model string, required provider.Capabilities, keyID string) ([]ChainEntry, *Trace, error) {
	trace := &Trace{}

	// Step 1: key scope. A key with patterns must match; a key with no
	// entry is unrestricted.
	if pats := r.cfg.KeyScopes[keyID]; keyID != "" && len(pats) > 0 {
		if !matchAny(model, pats) {
			trace.add(Event{Type: EventRejected, Reason: "key scope: model not allowed for key", Model: model})
			return nil, trace, ErrNoRoute
		}
	}

	// Step 2: alias resolution (C6).
	requested := model
	if target, ok := r.cfg.Aliases[model]; ok {
		model = target
		trace.add(Event{Type: EventAlias, From: requested, To: model})
	}

	// Step 3: rule matching. Exact matches beat wildcards; within a tier,
	// higher priority wins; registration order breaks remaining ties.
	rules, err := r.matchingRules(model)
	if err != nil {
		trace.add(Event{Type: EventRejected, Reason: "no matching routing rule", Model: model})
		return nil, trace, err
	}
	rule := rules[0]

	// Step 4 (ADR-009): chain expansion. Each entry resolves as an exact
	// provider ID first (legacy rows whose id equals a type string keep
	// their behavior); otherwise a known provider type expands to all its
	// credentialed accounts in stable registry order. Every expansion and
	// every dead entry is traced — never silent.
	entries, err := r.expandChain(rule.Chain, trace, model)
	if err != nil {
		trace.add(Event{Type: EventRejected, Reason: "chain empty after expansion", Model: model})
		return nil, trace, err
	}

	// Step 5 (ADR-009): strategy. Round robin rotates the expanded
	// candidates by the per-rule counter BEFORE the capability/health
	// filter — rotation never overrides the gates, it only reorders.
	strategy := rule.Strategy.Resolve(r.cfg.GlobalStrategy)
	if strategy == StrategyRoundRobin && len(entries) > 1 {
		entries = r.rotate(rule, entries, trace)
	}

	// Step 6: capability + health filter, recording every skip (never
	// silent). The filter loop is unchanged — it walks whatever order the
	// strategy produced.
	out := make([]ChainEntry, 0, len(entries))
	for _, entry := range entries {
		p, ok := r.lookup(entry)
		if !ok {
			trace.add(Event{Type: EventSkipped, ProviderID: entry, Reason: "provider not registered"})
			continue
		}
		if missing := missingCapabilities(p.Capabilities(), required); missing != "" {
			trace.add(Event{Type: EventSkipped, ProviderID: entry, Reason: "capability gap: " + missing})
			continue
		}
		if r.health != nil && r.health.Cooldown(entry) {
			trace.add(Event{Type: EventSkipped, ProviderID: entry, Reason: "health cooldown"})
			continue
		}
		out = append(out, ChainEntry{ProviderID: entry, Model: model})
	}
	if len(out) == 0 {
		trace.add(Event{Type: EventRejected, Reason: "chain empty after capability/health filtering", Model: model})
		return nil, trace, ErrNoRoute
	}
	return out, trace, nil
}

// expandChain resolves each chain entry to concrete provider IDs (ADR-009).
// An exact registry hit wins (the entry is that provider); a miss against a
// known type expands to the type's credentialed accounts in registry order;
// anything else is a traced skip. The terminal error fires only when every
// entry resolved to nothing.
func (r *Router) expandChain(chain []string, trace *Trace, model string) ([]string, error) {
	candidates := make([]string, 0, len(chain))
	for _, entry := range chain {
		if p, ok := r.lookup(entry); ok {
			// Exact ID beat: never expand a registered provider, even if
			// its ID collides with a type string (legacy compatibility).
			if cp, credentialed := p.(provider.CredentialedProvider); credentialed && !cp.HasCredential() {
				trace.add(Event{Type: EventSkipped, ProviderID: entry, Reason: "account has no credential"})
				continue
			}
			candidates = append(candidates, entry)
			continue
		}
		accounts, excluded := r.accountsOfType(entry)
		if len(accounts) == 0 && len(excluded) == 0 {
			trace.add(Event{Type: EventSkipped, ProviderID: entry, Reason: "no accounts for type " + entry})
			continue
		}
		for _, skipped := range excluded {
			trace.add(Event{Type: EventSkipped, ProviderID: skipped, Reason: "account has no credential (excluded from type expansion)"})
		}
		if len(accounts) == 0 {
			trace.add(Event{Type: EventSkipped, ProviderID: entry, Reason: "no credentialed accounts for type " + entry})
			continue
		}
		trace.add(Event{Type: EventExpand, From: entry, To: strings.Join(accounts, ","), Model: model})
		candidates = append(candidates, accounts...)
	}
	if len(candidates) == 0 {
		return nil, ErrNoRoute
	}
	return candidates, nil
}

// accountsOfType returns the registered accounts of one provider type in
// stable registry order (insertion = store id ASC), split into credentialed
// accounts and credentialless ones. Providers that do not expose a type
// never match; credentialless accounts (abandoned OAuth drafts, ADR-009
// decision 4) are excluded from expansion — routing live traffic to a
// guaranteed auth failure serves nobody — and returned separately so the
// caller can trace the exclusion (never silent).
func (r *Router) accountsOfType(typ string) (accounts, excluded []string) {
	if r.reg == nil {
		return nil, nil
	}
	for _, p := range r.reg.List() {
		tp, ok := p.(provider.TypedProvider)
		if !ok || tp.Type() != typ {
			continue
		}
		if cp, credentialed := p.(provider.CredentialedProvider); credentialed && !cp.HasCredential() {
			excluded = append(excluded, p.ID())
			continue
		}
		accounts = append(accounts, p.ID())
	}
	return accounts, excluded
}

// rotate applies the round-robin strategy to the expanded candidates: the
// per-rule counter picks the rotation offset, the list rotates left by it,
// and the rotation is traced (never silent) — including the offset-0 turn,
// because "this request's rotation landed on the first account" is exactly
// the observable the operator needs. Counter and read run under one Lock —
// the increment and the rotation must not interleave with a concurrent
// SetRules swap.
func (r *Router) rotate(rule Rule, entries []string, trace *Trace) []string {
	r.mu.Lock()
	offset := r.rrCounters[rule.ID]
	r.rrCounters[rule.ID] = offset + 1
	r.mu.Unlock()

	// Work in uint64 space: mod bounds the offset by the chain length, so
	// the slice math below is always in range. The single uint64→int
	// conversion is safe for any counter value because offset < n ≤ MaxInt;
	// gosec cannot see the bound, hence the reasoned suppression (repo
	// rule: never a bare nolint).
	n := uint64(len(entries))
	off := int(offset % n) //nolint:gosec // offset < n ≤ MaxInt by the mod bound
	rotated := make([]string, len(entries))
	copy(rotated, entries[off:])
	copy(rotated[len(entries)-off:], entries[:off])
	trace.add(Event{Type: EventRotation, From: entries[0], To: rotated[0],
		Reason: "round_robin: rotate " + rule.ID})
	return rotated
}

// matchingRules returns the rules matching model, ordered best-first:
// exact matches before wildcards, then priority descending, then
// registration order.
func (r *Router) matchingRules(model string) ([]Rule, error) {
	// Snapshot the live rule set under RLock, then match outside the lock
	// — the slice swap is atomic, so this snapshot is one coherent set.
	r.mu.RLock()
	all := r.rules
	r.mu.RUnlock()

	var exact, wildcard []Rule
	for _, rule := range all {
		if !rule.Enabled {
			continue
		}
		if rule.MatchModel == model {
			exact = append(exact, rule)
		} else if matchWildcard(rule.MatchModel, model) {
			wildcard = append(wildcard, rule)
		}
	}
	pool := exact
	if len(pool) == 0 {
		pool = wildcard
	}
	if len(pool) == 0 {
		return nil, ErrNoRoute
	}
	sort.SliceStable(pool, func(i, j int) bool { return pool[i].Priority > pool[j].Priority })
	return pool, nil
}

// lookup resolves a provider ID via the registry (nil registry: IDs pass
// through unfiltered — chain preview without a registry).
func (r *Router) lookup(pid string) (provider.Provider, bool) {
	if r.reg == nil {
		return nil, false
	}
	return r.reg.Get(pid)
}

// matchWildcard reports whether pattern (a `prefix*` glob) matches model.
func matchWildcard(pattern, model string) bool {
	if pattern == "*" {
		return true
	}
	if suffix, ok := strings.CutPrefix(pattern, "*"); ok {
		return strings.HasSuffix(model, suffix)
	}
	if prefix, ok := strings.CutSuffix(pattern, "*"); ok {
		return strings.HasPrefix(model, prefix)
	}
	return false
}

// matchAny reports whether model matches any pattern.
func matchAny(model string, patterns []string) bool {
	for _, p := range patterns {
		if p == model || matchWildcard(p, model) {
			return true
		}
	}
	return false
}

// missingCapabilities names the first required capability the provider
// lacks ("" when all are present).
func missingCapabilities(have, want provider.Capabilities) string {
	type pair struct {
		want bool
		have bool
		name string
	}
	for _, p := range []pair{
		{want.Streaming, have.Streaming, "streaming"},
		{want.Tools, have.Tools, "tools"},
		{want.Vision, have.Vision, "vision"},
		{want.WebSearch, have.WebSearch, "web_search"},
		{want.WebFetch, have.WebFetch, "web_fetch"},
		{want.CodeExecution, have.CodeExecution, "code_execution"},
		{want.Thinking, have.Thinking, "thinking"},
	} {
		if p.want && !p.have {
			return p.name
		}
	}
	return ""
}

// ErrNoRoute is the terminal no-route error: no rule matched (or the chain
// filtered to empty). Never a silent default model.
var ErrNoRoute = errors.New("routing: no route for model")
