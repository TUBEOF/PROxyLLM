package routing

import (
	"errors"
	"sort"
	"strings"
	"sync"

	"github.com/proxyllm/proxyllm/internal/provider"
)

// Rule is one routing rule (Analysis E rule model; storage:
// routing_rules table). The deterministic resolution order is:
// key scope → model match → capability filter → chain → health gate.
type Rule struct {
	// MatchModel is the model pattern to match: exact name or wildcard
	// with a single trailing `*` (e.g. `claude-*`).
	MatchModel string

	// Chain is the ordered fallback chain of provider IDs.
	Chain []string

	// Priority orders rules with overlapping matches; higher wins.
	// Rules of equal priority resolve in registration order.
	Priority int

	// Enabled gates the rule; disabled rules are skipped.
	Enabled bool
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
type Router struct {
	cfg    Config
	reg    provider.Registry
	health *HealthTracker

	// mu guards rules — the live rule set is replaceable at runtime
	// (WebUI routing writes reload it). Readers take RLock; the reload
	// path takes Lock for one whole-set swap. Aliases and KeyScopes stay
	// startup-fixed: only rules are DB-editable today.
	mu    sync.RWMutex
	rules []Rule
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
	r := &Router{cfg: cfg, rules: cfg.Rules}
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

// ResolveChain resolves model to its fallback chain — alias resolution and
// rule matching only, no capability/health filtering (the plain rule-model
// view; used for chain preview in tests and the WebUI).
func (r *Router) ResolveChain(model string) ([]ChainEntry, error) {
	// Step 1+2 of the shared pipeline: alias resolution, then rule match.
	// The chain rides through unfiltered: this view answers "what does the
	// rule say", not "what is currently usable".
	if target, ok := r.cfg.Aliases[model]; ok {
		model = target
	}
	rules, err := r.matchingRules(model)
	if err != nil {
		return nil, err
	}
	entries := make([]ChainEntry, 0, len(rules[0].Chain))
	for _, pid := range rules[0].Chain {
		entries = append(entries, ChainEntry{ProviderID: pid, Model: model})
	}
	return entries, nil
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

	// Steps 4+5: filter the chain by capability and health, recording
	// every skip (never silent).
	entries := make([]ChainEntry, 0, len(rules[0].Chain))
	for _, pid := range rules[0].Chain {
		p, ok := r.lookup(pid)
		if !ok {
			trace.add(Event{Type: EventSkipped, ProviderID: pid, Reason: "provider not registered"})
			continue
		}
		if missing := missingCapabilities(p.Capabilities(), required); missing != "" {
			trace.add(Event{Type: EventSkipped, ProviderID: pid, Reason: "capability gap: " + missing})
			continue
		}
		if r.health != nil && r.health.Cooldown(pid) {
			trace.add(Event{Type: EventSkipped, ProviderID: pid, Reason: "health cooldown"})
			continue
		}
		entries = append(entries, ChainEntry{ProviderID: pid, Model: model})
	}
	if len(entries) == 0 {
		trace.add(Event{Type: EventRejected, Reason: "chain empty after capability/health filtering", Model: model})
		return nil, trace, ErrNoRoute
	}
	return entries, trace, nil
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
