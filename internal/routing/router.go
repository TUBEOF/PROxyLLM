package routing

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/tubeof/proxyllm/internal/provider"
)

// Rule is one routing rule (storage:
// routing_rules table, extended by ADR-009). The deterministic resolution
// order is: key scope → model match → chain expansion → strategy →
// capability filter → health gate.
type Rule struct {
	// ID is the rule's storage identity. It keys the round-robin rotation
	// counters (per-rule, ADR-009) — empty ID rules share one counter.
	ID string

	// MatchModel is the model pattern to match: an exact name, or a
	// wildcard pattern in which `*` matches any run of characters
	// (e.g. `claude-*`, `*sonnet`, `claude-*-4-6`).
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

// Router resolves requests to deterministic fallback chains: the resolution
// is reproducible and testable, which is exactly why score/weight fuzzy
// routing was rejected as the primary mechanism — it is neither.
// ADR-009 restates determinism per request sequence: the round-robin
// counters are the only sequence-dependent state, per-rule and observable.
type Router struct {
	cfg    Config
	reg    provider.Registry
	health *HealthTracker

	// modelLookup is the implicit-route source (ADR-011): maps a model ID
	// to the provider ID of its models-table row, honoring the request
	// context (client disconnects cancel the read). Immutable after
	// construction (option-injected, never swapped) — no lock needed.
	// Nil disables the implicit route entirely (pre-ADR-011 behavior).
	modelLookup func(ctx context.Context, modelID string) (providerID string, ok bool)

	// visionLookup reports whether the MODEL ROW for (provider, model) has
	// LEARNED whether it can see images (ADR-014). Same injection contract as
	// modelLookup: option-set at construction, never swapped, nil disables the
	// vision gate entirely —
	// which is what the no-pre-stream-gate decision above rests on wherever
	// capabilities were never learned.
	visionLookup func(ctx context.Context, providerID, model string) (canSeeImages, known bool)

	// mu guards the rule set, the alias map, the global strategy and the
	// round-robin counters — all of them replaceable at runtime (WebUI
	// routing/settings writes reload them). Readers take RLock; the reload
	// path takes Lock for one whole-set swap. KeyScopes stay startup-fixed:
	// they are the only routing input no WebUI page writes.
	mu    sync.RWMutex
	rules []Rule
	// rrCounters holds one atomic counter per rule, keyed by rule id. A
	// sync.Map rather than a map+mutex: rotation is per request, and the
	// counter is the only mutable state the resolve path touches.
	rrCounters sync.Map
}

// Config carries the routing inputs.
type Config struct {
	// Rules are the routing rules, priority-ordered at resolution time.
	Rules []Rule

	// Aliases map requested model names to configured primary models
	// (Claude Code alias resolution). A key is an exact name or a `*`
	// wildcard pattern; an exact entry wins, otherwise the matching
	// pattern with the most literal characters (ties: lexicographic)
	// resolves.
	Aliases map[string]string

	// KeyScopes maps client key ID → allowed model patterns; a key with
	// no entry is unrestricted.
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

// WithModelLookup sets the implicit-route source (ADR-011): a lookup of
// model ID → owning provider ID from the models table, honoring the
// request context (WithRequestContext — a client disconnect cancels the
// read). When no rule matches, a row routes the request to its provider as
// a one-entry chain (traced implicit_route — never silent). A nil lookup,
// a missing row, or a lookup failure keeps the pre-ADR-011 behavior:
// no rule → no route.
func WithModelLookup(lookup func(ctx context.Context, modelID string) (providerID string, ok bool)) Option {
	return func(r *Router) { r.modelLookup = lookup }
}

// WithModelVisionLookup sets the model-row vision source for the vision gate:
// whether the row for (providerID, model) has learned whether that model can
// see images. Returning known=false means "no opinion" and is PERMISSIVE — the
// entry stays. Nil (the default) disables the gate, which keeps the
// no-pre-stream-gate behavior for callers that never wire a store.
//
// This is the half of ADR-014's learning that was missing: the executor wrote
// vision:false on every capability_gap and NOTHING ever read it back, so the
// describe-the-image rescue re-ran on every turn of an image-bearing
// conversation. The gate consumes what the learning produces.
func WithModelVisionLookup(lookup func(ctx context.Context, providerID, model string) (canSeeImages, known bool)) Option {
	return func(r *Router) { r.visionLookup = lookup }
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

	// Ctx is the request context, handed to the implicit-route lookup
	// (ADR-011) so a client disconnect cancels the models-table read
	// instead of leaving it running against the single SQLite connection.
	// Nil resolves to context.Background().
	Ctx context.Context
}

// WithKeyID resolves for a specific client key.
func WithKeyID(keyID string) ResolveOptions {
	return ResolveOptions{KeyID: keyID}
}

// WithRequestContext carries the request context into the resolver (the
// implicit-route lookup honors it — cancellation propagates end-to-end).
func WithRequestContext(ctx context.Context) ResolveOptions {
	return ResolveOptions{Ctx: ctx}
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
	if o.Ctx == nil {
		o.Ctx = context.Background()
	}
	return r.resolveWithTrace(model, required, o.KeyID, o.Ctx)
}

// StripContextMarker strips a trailing Claude Code context-window marker
// ("[1m]" / "[1M]", case-insensitive) from a model name. The marker is a
// client-side hint, never part of the model identity: hadMarker is true
// exactly when the returned base differs from the typed name.
func StripContextMarker(model string) (base string, hadMarker bool) {
	if len(model) >= 4 && strings.EqualFold(model[len(model)-4:], "[1m]") {
		return model[:len(model)-4], true
	}
	return model, false
}

// aliasTarget resolves model through the live alias table: the exact entry
// first, then the wildcard entries (the pattern with the most literal
// characters wins, ties lexicographic — one deterministic answer). One hop
// only: an alias target is never resolved again. The boolean reports whether
// any entry matched.
//
// Callers outside routing use AliasTarget to name the model a request will
// LAND on — the landed model's row is where the compatibility policy lives.
func (r *Router) aliasTarget(model string) (string, bool) {
	if r == nil {
		return "", false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if target, ok := r.cfg.Aliases[model]; ok {
		return target, true
	}
	return wildcardAlias(model, r.cfg.Aliases)
}

// AliasTarget resolves a client-typed model name the way resolveWithTrace
// step 2 does (see aliasTarget) and reports whether an alias matched. It
// exists so a caller that must resolve something PER LANDED MODEL — the
// per-model compatibility policy, which the client-typed name usually has no
// row for — reaches the same answer the router routes on, instead of a
// second, drifting implementation of alias resolution.
func (r *Router) AliasTarget(model string) (string, bool) {
	return r.aliasTarget(model)
}

// resolveWithTrace runs the deterministic pipeline:
//
//  1. key scope  — the key's allowed patterns must match the model
//  2. alias      — the requested name maps to the configured primary
//  3. rule match — exact before wildcard, priority before order
//  4. capability — skip chain entries lacking a required capability
//  5. health     — skip providers in cooldown
func (r *Router) resolveWithTrace(model string, required provider.Capabilities, keyID string, ctx context.Context) ([]ChainEntry, *Trace, error) {
	trace := &Trace{}

	// The Claude Code context-window marker ("[1m]"/"[1M]") is a
	// client-side hint, never part of the model identity: strip it and
	// route the base name, recording the override (never silent).
	typed := model
	base, hadMarker := StripContextMarker(model)
	if hadMarker {
		model = base
		trace.ContextOverride(typed, base)
	}

	// Step 1: key scope. A key with patterns must match; a key with no
	// entry is unrestricted. Checked against the TYPED name first, then
	// also against the base name when a marker was stripped — an
	// operator scope entry naming either form allows the request; a
	// scope without either still rejects.
	if pats := r.cfg.KeyScopes[keyID]; keyID != "" && len(pats) > 0 {
		if !matchAny(typed, pats) && (!hadMarker || !matchAny(base, pats)) {
			trace.add(Event{Type: EventRejected, Reason: "key scope: model not allowed for key", Model: model})
			return nil, trace, noRoute(model)
		}
	}

	// Step 2: alias resolution. The exact entry first; then the wildcard
	// entries, the pattern with the most literal characters winning (ties:
	// lexicographic — one deterministic answer). Claude Code fires auxiliary
	// requests (the WebFetch apply call, the auto-mode classifier) with
	// versioned Claude IDs — claude-sonnet-4-6, claude-haiku-5-5, the dated
	// legacy spelling alike — that no operator can enumerate, so a `claude-*`
	// or `claude-sonnet-*` pattern covers each family without an entry per
	// release. The resolution is traced like any alias, and an alias target
	// is never resolved again (no chains).
	requested := model
	target, aliased := r.aliasTarget(model)
	if aliased {
		model = target
		trace.add(Event{Type: EventAlias, From: requested, To: model})
	}

	// Step 3: rule matching. Exact matches beat wildcards; within a tier,
	// higher priority wins; registration order breaks remaining ties.
	var rule Rule
	rules, err := r.matchingRules(model)
	if err != nil {
		// No rule matched. ADR-011: a models-table row for the model
		// routes it to that row's provider as a one-entry chain — traced
		// (implicit_route, never silent) and subject to the unchanged
		// pipeline below (credential gate, capability filter, health
		// gate). Rules always win; this fires only when the rule tiers
		// were empty. A nil lookup, a missing row, or a lookup failure
		// keeps the no-route rejection.
		implicit := Rule{}
		if r.modelLookup != nil {
			if pid, ok := r.modelLookup(ctx, model); ok && pid != "" {
				implicit = Rule{Chain: []string{pid}}
				trace.ImplicitRoute(model, pid)
			}
		}
		if len(implicit.Chain) == 0 {
			trace.add(Event{Type: EventRejected, Reason: "no matching routing rule", Model: model})
			return nil, trace, noRoute(model)
		}
		rule = implicit
	} else {
		rule = rules[0]
	}

	// Step 4 (ADR-009): chain expansion. Each entry resolves as an exact
	// provider ID first (legacy rows whose id equals a type string keep
	// their behavior); otherwise a known provider type expands to all its
	// credentialed accounts in stable registry order. Every expansion and
	// every dead entry is traced — never silent.
	entries, expansionErr := r.expandChain(rule.Chain, trace, model)
	if expansionErr != nil {
		trace.add(Event{Type: EventRejected, Reason: "chain empty after expansion", Model: model})
		return nil, trace, noRoute(model)
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
	// strategy produced. Vision is not gated here: a vision request goes out, and
	// a real upstream rejection (capability_gap) fails over to the executor's
	// capability fallback models.
	out := make([]ChainEntry, 0, len(entries))
	// healthSkipped tracks whether every filter rejection was a health
	// cooldown — a chain that is merely cooled down is temporarily
	// unavailable (retryable, ErrChainOverloaded), unlike a configurational
	// gap (ErrNoRoute → the surfaces answer the non-retryable 404).
	healthSkipped := false
	nonHealthSkipped := false
	for _, entry := range entries {
		p, ok := r.lookup(entry)
		if !ok {
			trace.add(Event{Type: EventSkipped, ProviderID: entry, Reason: "provider not registered"})
			nonHealthSkipped = true
			continue
		}
		if missing := missingCapabilities(p.Capabilities(), required); missing != "" {
			trace.add(Event{Type: EventSkipped, ProviderID: entry, Reason: "capability gap: " + missing})
			nonHealthSkipped = true
			continue
		}
		if r.health != nil && r.health.Cooldown(entry) {
			trace.add(Event{Type: EventSkipped, ProviderID: entry, Reason: "health cooldown"})
			healthSkipped = true
			continue
		}
		out = append(out, ChainEntry{ProviderID: entry, Model: model})
	}

	// The vision gate runs LAST and only as a
	// preference among the survivors, because the two things that make it safe
	// are both about leaving the pipeline alone otherwise:
	//
	//   - It does nothing without a lookup, so an unlearned model row is
	//     UNKNOWN rather than blind and every candidate survives — the
	//     no-pre-stream-gate decision above still holds for it.
	//   - It never empties the chain. Every candidate being known-blind is
	//     exactly the case the image-caption rescue exists for: the request has
	//     to go out, be rejected with capability_gap, and be rescued. Emptying
	//     the chain there would turn a rescue into a hard no-route.
	//
	// What it buys: a model that has already been rejected for images once is
	// not asked again, so the conversation stops paying a rejection plus an
	// image caption on every single turn.
	if required.Vision && r.visionLookup != nil && len(out) > 1 {
		var kept, blind []ChainEntry
		for _, entry := range out {
			if canSee, known := r.visionLookup(ctx, entry.ProviderID, entry.Model); known && !canSee {
				blind = append(blind, entry)
				continue
			}
			kept = append(kept, entry)
		}
		if len(kept) > 0 {
			for _, entry := range blind {
				trace.add(Event{Type: EventSkipped, ProviderID: entry.ProviderID,
					Reason: "capability gap: vision (learned for " + entry.Model + ")"})
			}
			out = kept
		}
	}

	if len(out) == 0 {
		if healthSkipped && !nonHealthSkipped {
			// Every candidate was reachable-in-principle but cooled down:
			// the model exists and is routed, the providers are merely
			// pausing (circuit breaker). Surfaces answer 529/503 so
			// clients retry instead of switching models.
			trace.add(Event{Type: EventRejected, Reason: "all providers in health cooldown", Model: model})
			return nil, trace, ErrChainOverloaded
		}
		trace.add(Event{Type: EventRejected, Reason: "chain empty after capability/health filtering", Model: model})
		return nil, trace, noRoute(model)
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
	// An atomic per-rule counter, not the rule mutex: rotation happens on every
	// round-robin request, and taking the router's write lock there serialized
	// every concurrent resolve behind one counter increment.
	counter, _ := r.rrCounters.LoadOrStore(rule.ID, new(atomic.Uint64))
	offset := counter.(*atomic.Uint64).Add(1) - 1

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

// matchWildcard reports whether pattern matches model. `*` matches any run
// of characters — empty included — and the literal segments around it must
// appear in order: `*` alone matches everything, `prefix*` and `*suffix`
// keep their classic meaning, and interior patterns (`claude-*-4-6`) work
// too. A pattern WITHOUT a `*` matches on exact equality only — a plain
// name can never match as a substring.
func matchWildcard(pattern, model string) bool {
	if !strings.Contains(pattern, "*") {
		return pattern == model
	}
	segs := strings.Split(pattern, "*")
	if head := segs[0]; head != "" {
		if !strings.HasPrefix(model, head) {
			return false
		}
		model = model[len(head):]
	}
	tail := segs[len(segs)-1]
	for _, seg := range segs[1 : len(segs)-1] {
		if seg == "" {
			continue
		}
		i := strings.Index(model, seg)
		if i < 0 {
			return false
		}
		model = model[i+len(seg):]
	}
	return tail == "" || strings.HasSuffix(model, tail)
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

// wildcardAlias resolves a requested name through the alias map's wildcard
// entries: the pattern matching with the most literal (non-`*`) characters
// wins, an exact tie goes to the lexicographically smaller key — one
// deterministic answer, independent of map iteration order. Keys without a
// `*` are ignored here: resolveWithTrace tries the exact entry first, so a
// plain name is an exact alias only. A missing match keeps the caller on its
// documented no-route path.
func wildcardAlias(model string, aliases map[string]string) (string, bool) {
	bestKey, bestTarget, bestLiteral := "", "", -1
	for key, target := range aliases {
		if !strings.Contains(key, "*") || !matchWildcard(key, model) {
			continue
		}
		literal := len(key) - strings.Count(key, "*")
		if literal > bestLiteral || (literal == bestLiteral && key < bestKey) {
			bestKey, bestTarget, bestLiteral = key, target, literal
		}
	}
	if bestLiteral < 0 {
		return "", false
	}
	return bestTarget, true
}

// missingCapabilities names the first required capability the provider
// lacks ("" when all are present). Vision is deliberately NOT here: static
// provider caps cannot know which MODEL accepts images — a vision request goes
// out, and a real upstream rejection (capability_gap class) fails over to the
// configured fallback models in the executor. The remaining capabilities are
// deterministic surface/emulation facts the gate can decide up front.
func missingCapabilities(have, want provider.Capabilities) string {
	type pair struct {
		want bool
		have bool
		name string
	}
	for _, p := range []pair{
		{want.Streaming, have.Streaming, "streaming"},
		{want.Tools, have.Tools, "tools"},
		{want.WebSearch, have.WebSearch, "web_search"},
		{want.WebFetch, have.WebFetch, "web_fetch"},
		{want.CodeExecution, have.CodeExecution, "code_execution"},
		{want.Thinking, have.Thinking, "thinking"},
		{want.Decision, have.Decision, "decision"},
	} {
		if p.want && !p.have {
			return p.name
		}
	}
	return ""
}

// ErrNoRoute is the terminal no-route error: no rule matched (or the chain
// filtered to empty). Never a silent default model. The pipeline returns it
// as *NoRouteError (errors.Is keeps matching this sentinel) so the surfaces
// can name the model on the wire.
var ErrNoRoute = errors.New("routing: no route for model")

// NoRouteError is the terminal no-route outcome, naming the model the
// pipeline could not route. The name is load-bearing on the wire: the
// client-facing message is built from it ("model: <id>"), the reference's
// spelling for an unknown model — and the only message shape Claude Code
// classifies as model-not-found (404 + not_found_error + "model:"), which
// is the precondition for its own model fallback to fire. A prose message
// would read as an opaque 404 and leave client-side recovery dead.
type NoRouteError struct {
	// Model is the name the pipeline failed to route: the rewritten
	// (post-alias) target when an alias resolved, else the requested name.
	Model string
}

func (e *NoRouteError) Error() string { return "routing: no route for model " + e.Model }

// Is keeps the sentinel identity: every errors.Is(err, ErrNoRoute) check
// (surfaces, executor wiring, tests) stays true.
func (e *NoRouteError) Is(target error) bool { return target == ErrNoRoute }

// noRoute builds the terminal rejection naming model.
func noRoute(model string) error { return &NoRouteError{Model: model} }

// ErrChainOverloaded is the terminal temporarily-unavailable error: the
// model routed, but every chain provider is in health cooldown (circuit
// breaker open). Unlike ErrNoRoute this is retryable — surfaces answer
// 529 (Anthropic) / 503 (OpenAI) so clients back off and retry instead
// of treating the model as nonexistent.
var ErrChainOverloaded = errors.New("routing: all chain providers in health cooldown")
