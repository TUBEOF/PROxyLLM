package routing

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/tubeof/proxyllm/internal/provider"
	"github.com/tubeof/proxyllm/internal/translate/ir"
)

// Executor is the request-level failover loop (plan Analysis E failover
// semantics; research facts FO-1/FO-3/FO-5). It walks the resolved chain
// and, per failed attempt, applies the FO-2 classification:
//
//	retry_same    → re-attempt the SAME provider, up to the retry budget,
//	                honoring Retry-After (O10), capped by the backoff
//	                ceiling (FO-3)
//	failover      → move to the next chain entry
//	terminal      → end the request: content_filter (compliance), client
//	                errors (invalid at any provider), mid-stream failures
//	                (the client already holds partial output, FO-1)
//
// Streaming requests are never retried once dispatched (FO-5): the stream
// path knows only dispatch-failure failover (before the first event) and
// terminal. Every transition is recorded in the Trace (FO-4) and every
// attempt outcome is reported to the HealthTracker.
type Executor struct {
	cfg ExecutorConfig

	// emuMu guards cfg.Emulator for the live-swap (SetEmulator): the
	// provider reload rebuilds the emulator pool without restart while
	// in-flight emulation rounds read the field. Callers already hold no
	// executor mutex — the swap is a single pointer write under a lock.
	emuMu sync.RWMutex
}

// ExecutorConfig configures the failover loop.
type ExecutorConfig struct {
	// Registry resolves chain entries to providers.
	Registry provider.Registry

	// Health receives per-attempt outcomes (circuit-breaker input).
	Health *HealthTracker

	// MaxRetriesPerProvider bounds same-provider retries (FO-3: 1–2).
	// Zero means the default (2).
	MaxRetriesPerProvider int

	// MaxProviders bounds how many chain entries are attempted (FO-3:
	// 2–3). Zero means the default (3).
	MaxProviders int

	// BackoffCeiling caps the same-provider retry delay (FO-3: 2–4 s).
	// Zero means the default (3 s). Retry-After is honored but capped
	// here (O10).
	BackoffCeiling time.Duration

	// RetryBackoff is the base delay between same-provider retries;
	// each retry doubles it up to the ceiling. Zero means the default
	// (250 ms).
	RetryBackoff time.Duration

	// RetrySleeper suspends the retry backoff (defaults to time.Sleep).
	// Injectable so tests exercise the retry discipline without real
	// waiting (Analysis F determinism).
	RetrySleeper func(time.Duration)

	// Emulator executes emulated web tools (§6.2 Firecrawl fallback) for
	// the tool-call round-trip loop in Execute. Nil disables actual
	// emulation but keeps the loop: model tool calls against emulated
	// tools are answered with a protocol-conform "unavailable" error
	// result (the CLAUDE.md degrade contract: no Firecrawl key →
	// documented unavailable errors), then re-dispatched.
	Emulator Emulator

	// CapabilityFallbacks are the model-level fallback chain entries
	// (2026-09-21 automatic capabilities): consulted when the rule chain
	// exhausted with capability_gap failures only. Each entry is a fully
	// resolved (provider, wire-model) pair — the httpapi layer resolves
	// the stored model IDs before Execute. Nil/empty disables the model
	// fallback (the request terminates with the last gap error). Swappable
	// at runtime via SetCapabilityFallbacks.
	CapabilityFallbacks []ChainEntry

	// OnCapabilityGap is invoked once per chain entry whose attempt failed
	// with the capability_gap class (2026-09-21 learning): the wiring layer
	// persists the gap into the model row's capabilities (vision=false
	// learning). It receives the full chain entry — provider ID plus the
	// upstream wire model, the pair the storage layer needs to find the
	// model row. Optional; nil disables learning. The callback must not
	// block — it is called inline on the failover path.
	OnCapabilityGap func(entry ChainEntry)

	// now stamps attempt latencies; injectable for tests.
	now func() time.Time
}

// FO-3 defaults: research bounds say 1–2 retries per provider, 2–3
// providers total, 2–4 s backoff ceiling. We encode the upper bounds so
// operators tighten via config rather than the research being silently
// narrower than shipped behavior.
const (
	defaultMaxRetriesPerProvider = 2
	defaultMaxProviders          = 3
	defaultBackoffCeiling        = 3 * time.Second
	defaultRetryBackoff          = 250 * time.Millisecond
)

// ErrChainExhausted is the terminal error when every attempt in the chain
// failed within its budget. It wraps the last attempt's error.
var ErrChainExhausted = errors.New("routing: chain exhausted")

// exhaustedError carries the last attempt's error for callers that need
// the upstream cause (error surfacing per §5.3).
type exhaustedError struct {
	last error
}

func (e *exhaustedError) Error() string { return ErrChainExhausted.Error() + ": " + e.last.Error() }
func (e *exhaustedError) Unwrap() error { return e.last }

// NewExecutor returns an Executor for cfg, filling FO-3 defaults.
func NewExecutor(cfg ExecutorConfig) *Executor {
	if cfg.MaxRetriesPerProvider == 0 {
		cfg.MaxRetriesPerProvider = defaultMaxRetriesPerProvider
	}
	if cfg.MaxProviders == 0 {
		cfg.MaxProviders = defaultMaxProviders
	}
	if cfg.BackoffCeiling == 0 {
		cfg.BackoffCeiling = defaultBackoffCeiling
	}
	if cfg.RetryBackoff == 0 {
		cfg.RetryBackoff = defaultRetryBackoff
	}
	if cfg.RetrySleeper == nil {
		cfg.RetrySleeper = time.Sleep
	}
	if cfg.now == nil {
		cfg.now = time.Now
	}
	return &Executor{cfg: cfg}
}

// Execute performs a non-streaming request against the chain with the
// full retry/failover discipline (FO-2/FO-3), plus the §6.2 emulated-tool
// round-trip loop: when the request carries emulated web tools (the
// applyEmulation emulate-firecrawl policy) and the model answers with
// tool calls against them, the executor answers the calls via the
// configured Emulator and re-dispatches until the model answers without
// them, budgets refuse with protocol-conform error results, or the round
// cap surfaces the last response. The returned Trace records every
// transition (FO-4).
func (e *Executor) Execute(ctx context.Context, chain []ChainEntry, req ir.Request) (ir.Response, *Trace, error) {
	trace := &Trace{}
	resp, err := e.executeWithEmulation(ctx, chain, req, trace)
	return resp, trace, err
}

// executeOnce walks the chain once with the full retry/failover
// discipline — one upstream answer (or the terminal error). When the walk
// ends with ONLY capability_gap failures (2026-09-21 automatic
// capabilities), the configured capability fallback models get one walk
// of their own, appended after the regular chain — the request itself is
// valid, another model can serve it.
func (e *Executor) executeOnce(ctx context.Context, chain []ChainEntry, req ir.Request, trace *Trace) (ir.Response, error) {
	return e.walkChain(ctx, chain, req, trace, true)
}

// walkChain is executeOnce's core with the fallback gate: allowFallback
// is false on the fallback models' own walk (no recursive fallback).
func (e *Executor) walkChain(ctx context.Context, chain []ChainEntry, req ir.Request, trace *Trace, allowFallback bool) (ir.Response, error) {
	var lastErr error

	// pendingBackoff is the delay before the next same-provider retry,
	// computed from the failed attempt's Retry-After when present (O10).
	var pendingBackoff time.Duration

	// Gap bookkeeping (2026-09-21): a chain is gap-exhausted when every
	// attempt failed with the capability_gap class — the model cannot
	// serve the request, but another (fallback) model may.
	gapOnly := true
	attempted := 0

	for ci, entry := range chain {
		if ci >= e.cfg.MaxProviders {
			break
		}
		p, ok := e.cfg.Registry.Get(entry.ProviderID)
		if !ok {
			trace.Skip(entry.ProviderID, "provider not registered")
			continue
		}

		attempts := 1 + e.cfg.MaxRetriesPerProvider
		for attempt := 0; attempt < attempts; attempt++ {
			if attempt > 0 {
				e.cfg.RetrySleeper(pendingBackoff)
			}
			trace.Attempt(entry.ProviderID, entry.Model)
			attempted++

			started := e.cfg.now()
			resp, err := p.Complete(ctx, withModel(req, entry.Model))
			latency := e.cfg.now().Sub(started).Milliseconds()
			if err == nil {
				e.reportSuccess(entry.ProviderID)
				return resp, nil
			}
			lastErr = err
			c := ClassifyError(err, PreStream)
			status, code, _ := AsUpstreamError(err)
			trace.Failed(entry.ProviderID, c, status, code, latency)
			e.reportFailure(entry.ProviderID)
			e.reportQuota(entry.ProviderID, err, c)
			if c.Class != ClassCapabilityGap {
				gapOnly = false
			}
			if c.Class == ClassCapabilityGap && e.cfg.OnCapabilityGap != nil {
				e.cfg.OnCapabilityGap(entry)
			}

			switch c.Action {
			case ActionTerminal:
				return ir.Response{}, err
			case ActionRetrySame:
				if attempt+1 < attempts {
					pendingBackoff = e.retryDelay(err, attempt+1)
					trace.Retry(entry.ProviderID, "rate_limit: retry same provider within budget")
					continue
				}
				// Budget spent: fail over (FO-2).
			case ActionFailOver:
				// Budget is irrelevant: move on immediately.
			}
			if ci+1 < len(chain) && ci+1 < e.cfg.MaxProviders {
				trace.Failover(entry.ProviderID, chain[ci+1].ProviderID, "chain failover after "+string(c.Class))
			}
			break
		}
	}

	// Capability fallback (2026-09-21): only a gap-exhausted chain (every
	// attempt a capability_gap, at least one attempt made) consults the
	// fallback models — a 401 or content_filter must not be "rescued" by
	// another model. The fallback walk is a fresh walk over the fallback
	// entries with the fallback itself disabled (no recursion: fallback
	// models that reject the modality too terminate the request), so it
	// inherits the full retry/failover discipline.
	if gapOnly && attempted > 0 && allowFallback {
		fallbacks := e.currentCapabilityFallbacks()
		if len(fallbacks) > 0 {
			traceCapabilityFallback(trace, lastProviderOf(chain), fallbacks)
			return e.walkChain(ctx, fallbacks, req, trace, false)
		}
	}
	return ir.Response{}, &exhaustedError{last: lastErr}
}

// traceCapabilityFallback records the capability-fallback consultation and
// the resulting fallback-model walk, for both the complete and the stream
// path.
//
// One consultation event, then exactly ONE failover edge onto the first
// fallback model actually about to be tried: the walk visits the fallback
// entries in order and traces its own per-entry failovers, so emitting an
// edge per configured fallback here would report models that are never
// reached. The edge is also suppressed when it would be a self-edge — the
// exhausted chain's last provider frequently IS the first fallback account,
// and "A → A" tells the operator nothing.
func traceCapabilityFallback(trace *Trace, from string, fallbacks []ChainEntry) {
	trace.CapabilityFallback("vision",
		"chain exhausted on capability gaps; trying the capability fallback models")
	if len(fallbacks) == 0 {
		return
	}
	to := fallbacks[0].ProviderID
	if to == "" || to == from {
		return
	}
	trace.Failover(from, to, "capability fallback model")
}

// lastProviderOf returns the last chain entry's provider ID for the
// failover event (empty chain → "" — the trace omits empty From fields).
func lastProviderOf(chain []ChainEntry) string {
	if len(chain) == 0 {
		return ""
	}
	return chain[len(chain)-1].ProviderID
}

// SetCapabilityFallbacks atomically replaces the live capability fallback
// entries (2026-09-21: the admin API's fallback write pushes the freshly
// stored model list into the executor without restart — same last-known-
// good live-swap contract as SetEmulator).
func (e *Executor) SetCapabilityFallbacks(entries []ChainEntry) {
	e.emuMu.Lock()
	defer e.emuMu.Unlock()
	e.cfg.CapabilityFallbacks = entries
}

// currentCapabilityFallbacks reads the live fallback entries under the
// swap lock.
func (e *Executor) currentCapabilityFallbacks() []ChainEntry {
	e.emuMu.RLock()
	defer e.emuMu.RUnlock()
	return e.cfg.CapabilityFallbacks
}

// SetEmulator atomically replaces the live web-tool emulator (ADR-010:
// the provider reload rebuilds the emulator pool from the stored
// firecrawl accounts without restart). Same last-known-good live-swap
// contract as the router's Set* hooks.
func (e *Executor) SetEmulator(emu Emulator) {
	e.emuMu.Lock()
	defer e.emuMu.Unlock()
	e.cfg.Emulator = emu
}

// currentEmulator reads the live emulator under the swap lock.
func (e *Executor) currentEmulator() Emulator {
	e.emuMu.RLock()
	defer e.emuMu.RUnlock()
	return e.cfg.Emulator
}

// ReportStreamOutcome records the final outcome of a stream that was
// dispatched successfully: a nil streamErr reports success (idempotent
// with the dispatch-time success report — no double counting), a
// non-nil streamErr corrects the premature dispatch success into a
// breaker failure, because a mid-stream upstream failure is a real
// signal about provider health even though the client-facing stream is
// never retried (FO-5). httpapi calls this after relaying the stream,
// with the provider resolved from the trace and the streamErr surfaced
// by the relay (streamErr is *ir.UpstreamError, not a plain error,
// because mid-stream failures surface as typed upstream errors).
func (e *Executor) ReportStreamOutcome(providerID string, streamErr *ir.UpstreamError) {
	if providerID == "" {
		return
	}
	if streamErr == nil {
		e.reportSuccess(providerID)
		return
	}
	e.reportFailure(providerID)
}

// ExecuteStream performs a streaming request against the chain. FO-5:
// streams are never retried once dispatched — dispatch failures (before
// the first event) fail over; anything after the first event is terminal
// and surfaces as an Error event on the returned channel.
//
// The capability fallback (2026-09-21 automatic capabilities) applies here
// as on the complete path: a chain whose every DISPATCH failed with a
// capability_gap (a 400 modality rejection happens before the first byte,
// so FO-1 permits the swap) gets one walk over the fallback models via
// p.Stream — no recursion, fallback dispatch gaps terminate.
//
// The returned cancel must be called when the caller is done with the
// channel to release upstream resources. Dispatch-time success is
// reported to the HealthTracker immediately; once the caller has fully
// relayed the stream it must call ReportStreamOutcome so a mid-stream
// failure still counts (ReportStreamOutcome).
func (e *Executor) ExecuteStream(ctx context.Context, chain []ChainEntry, req ir.Request) (<-chan ir.StreamEvent, func(), *Trace, error) {
	// Streaming with emulated web tools (§6.2, 2026-10-06): the emulation
	// round-trip loop only exists on the complete path — a streamed tool
	// call would relay raw to the client with no data (the reported bug:
	// streaming Claude Code requests got bared tool_use). Once the first
	// tool_use is on the wire it cannot be retrofitted, so requests whose
	// tools include emulated defs ride the COMPLETE emulation loop to
	// finality; the final response synthesizes the event stream below.
	// Latency trade: no token streaming for this request class (the model
	// runs its whole answer before the first byte) — documented, and the
	// trade for protocol-conform tool RESULTS.
	if len(emulatedToolDefs(req.Tools)) > 0 {
		return e.executeEmulatedAsStream(ctx, chain, req)
	}
	trace := &Trace{}
	ch, cancel, err := e.walkChainStream(ctx, chain, req, trace, true)
	if ch == nil && err != nil {
		return nil, cancel, trace, err
	}
	return ch, cancel, trace, err
}

// executeEmulatedAsStream runs the complete-path emulation loop (full
// failover/retry discipline) and produces the wire stream from the final
// response: Start → BlockStart/Delta/BlockStop per content part →
// MessageDelta → Done. Cancellation: the caller's ctx aborts between
// rounds (executeWithEmulation honors it on the dispatches; a ctx.Done
// while encoding closes the channel) and cancel() stops nothing extra —
// everything the loop ran has released itself on return.
func (e *Executor) executeEmulatedAsStream(ctx context.Context, chain []ChainEntry, req ir.Request) (<-chan ir.StreamEvent, func(), *Trace, error) {
	// The client's stream flag rides the IR request — the complete-path
	// decoders set it for streamed requests, and a complete dispatch that
	// still carries true makes real upstreams answer chunked SSE (which
	// the non-streaming decode cannot parse). Clear it on the detour's
	// COPY (the caller's request object is not mutated).
	req.Stream = false
	trace := &Trace{}
	resp, err := e.executeWithEmulation(ctx, chain, req, trace)
	if err != nil {
		// Pre-stream failure semantics: no bytes were sent (the channel
		// did not open), so ExecuteStream returns the error directly.
		return nil, func() {}, trace, err
	}

	ch := make(chan ir.StreamEvent) // unbuffered: backpressure to the caller
	cancel := func() {}
	go func() {
		defer func() { cancel() }()
		synthesizeStream(ctx, ch, resp, resp.Usage)
	}()
	return ch, cancel, trace, nil
}

// synthesizeStream writes the response as a canonical event sequence — the
// same vocabulary the relay paths consume. honors ctx: a cancelled
// caller stops writing (the channel close is the loop's exit).
func synthesizeStream(ctx context.Context, ch chan<- ir.StreamEvent, resp ir.Response, usage ir.Usage) {
	defer close(ch)
	if err := ctx.Err(); err != nil {
		ch <- ir.StreamEvent{Type: ir.Error, Error: &ir.UpstreamError{
			Type: "cancelled", Message: "request cancelled",
		}}
		return
	}
	// Start.
	respFull := resp
	respFull.Usage = ir.Usage{} // usage rides the terminal MessageDelta
	ch <- ir.StreamEvent{Type: ir.Start, Response: &respFull}
	for i, part := range resp.Content {
		if err := ctx.Err(); err != nil {
			return
		}
		switch p := part.(type) {
		case ir.TextPart:
			ch <- ir.StreamEvent{Type: ir.BlockStart, Index: i, Part: ir.TextPart{}}
			ch <- ir.StreamEvent{Type: ir.Delta, Index: i,
				Delta: ir.DeltaPayload{Kind: ir.DeltaText, Text: p.Text}}
			ch <- ir.StreamEvent{Type: ir.BlockStop, Index: i}
		case ir.ThinkingPart:
			ch <- ir.StreamEvent{Type: ir.BlockStart, Index: i, Part: ir.ThinkingPart{}}
			ch <- ir.StreamEvent{Type: ir.Delta, Index: i,
				Delta: ir.DeltaPayload{Kind: ir.DeltaThinking, Text: p.Thinking}}
			ch <- ir.StreamEvent{Type: ir.BlockStop, Index: i}
		case ir.ToolCallPart:
			ch <- ir.StreamEvent{Type: ir.BlockStart, Index: i, Part: ir.ToolCallPart{ID: p.ID, Name: p.Name}}
			ch <- ir.StreamEvent{Type: ir.Delta, Index: i,
				Delta: ir.DeltaPayload{Kind: ir.DeltaToolInput, PartialJSON: string(p.Input)}}
			ch <- ir.StreamEvent{Type: ir.BlockStop, Index: i}
		case ir.ServerToolUsePart:
			ch <- ir.StreamEvent{Type: ir.BlockStart, Index: i, Part: p}
		case ir.ServerToolResultPart:
			ch <- ir.StreamEvent{Type: ir.BlockStart, Index: i, Part: p}
			ch <- ir.StreamEvent{Type: ir.BlockStop, Index: i}
		default:
			// Unknown parts skip (the round-trip loop never produces others).
		}
	}
	if err := ctx.Err(); err != nil {
		return
	}
	md := ir.MessageDeltaPayload{StopReason: resp.StopReason, Usage: usage}
	ch <- ir.StreamEvent{Type: ir.MessageDelta, MessageDelta: &md}
	ch <- ir.StreamEvent{Type: ir.Done}
}

// walkChainStream is ExecuteStream's core with the fallback gate:
// allowFallback is false on the fallback models' own walk (no recursion).
func (e *Executor) walkChainStream(ctx context.Context, chain []ChainEntry, req ir.Request, trace *Trace, allowFallback bool) (<-chan ir.StreamEvent, func(), error) {
	var lastErr error

	// Gap bookkeeping (2026-09-21): same contract as the complete path —
	// only an ALL-gap dispatch history consults the fallback models.
	gapOnly := true
	attempted := 0

	for ci, entry := range chain {
		if ci >= e.cfg.MaxProviders {
			break
		}
		p, ok := e.cfg.Registry.Get(entry.ProviderID)
		if !ok {
			trace.Skip(entry.ProviderID, "provider not registered")
			continue
		}

		trace.Attempt(entry.ProviderID, entry.Model)
		attempted++
		started := e.cfg.now()
		ch, cancel, err := p.Stream(ctx, withModel(req, entry.Model))
		latency := e.cfg.now().Sub(started).Milliseconds()
		if err == nil {
			// Dispatch succeeded: no failover from here (FO-1/FO-5).
			// Health reports success once the stream terminates cleanly;
			// a dispatch success with a later mid-stream failure still
			// counts as one failure.
			e.reportSuccess(entry.ProviderID)
			return ch, cancel, nil
		}
		lastErr = err
		c := ClassifyError(err, PreStream)
		status, code, _ := AsUpstreamError(err)
		trace.Failed(entry.ProviderID, c, status, code, latency)
		e.reportFailure(entry.ProviderID)
		e.reportQuota(entry.ProviderID, err, c)
		if c.Class != ClassCapabilityGap {
			gapOnly = false
		}
		if c.Class == ClassCapabilityGap && e.cfg.OnCapabilityGap != nil {
			e.cfg.OnCapabilityGap(entry)
		}

		switch c.Action {
		case ActionTerminal:
			if cancel != nil {
				cancel()
			}
			return nil, func() {}, err
		case ActionFailOver:
			// Release the failed attempt's upstream resources before
			// moving on (4c: a dispatch error may still have opened a
			// connection; httpx hands out nil cancel only when it never
			// started one).
			if cancel != nil {
				cancel()
			}
			if ci+1 < len(chain) && ci+1 < e.cfg.MaxProviders {
				trace.Failover(entry.ProviderID, chain[ci+1].ProviderID, "chain failover after "+string(c.Class))
			}
			// FO-5: no same-provider retry for streams; on to the next
			// chain entry (or terminal exhaustion below).
		case ActionRetrySame:
			// FO-5: a 429-class dispatch failure on a stream still never
			// retries the same provider — "partial data may already have
			// been sent" upstream. Fail over if a next entry exists.
			if cancel != nil {
				cancel()
			}
			if ci+1 < len(chain) && ci+1 < e.cfg.MaxProviders {
				trace.Failover(entry.ProviderID, chain[ci+1].ProviderID, "stream rate_limit: fail over, never retry (FO-5)")
			}
		}
	}

	// Capability fallback (2026-09-21): identical gate to the complete
	// path — only a gap-exhausted chain consults the fallback models.
	if gapOnly && attempted > 0 && allowFallback {
		fallbacks := e.currentCapabilityFallbacks()
		if len(fallbacks) > 0 {
			traceCapabilityFallback(trace, lastProviderOf(chain), fallbacks)
			return e.walkChainStream(ctx, fallbacks, req, trace, false)
		}
	}
	return nil, func() {}, &exhaustedError{last: lastErr}
}

// retryDelay computes the delay before the next same-provider retry: the
// upstream-advertised Retry-After when present (O10), capped by the FO-3
// backoff ceiling; otherwise the exponential backoff for the attempt.
func (e *Executor) retryDelay(err error, attempt int) time.Duration {
	var ue *provider.UpstreamError
	if errors.As(err, &ue) && ue.RetryAfter > 0 {
		return min(ue.RetryAfter, e.cfg.BackoffCeiling)
	}
	return e.backoff(attempt)
}

// backoff computes the same-provider retry delay for attempt n (1-based),
// doubling from the base and capped by the ceiling.
func (e *Executor) backoff(attempt int) time.Duration {
	d := e.cfg.RetryBackoff
	for i := 1; i < attempt; i++ {
		d *= 2
		if d >= e.cfg.BackoffCeiling {
			return e.cfg.BackoffCeiling
		}
	}
	return min(d, e.cfg.BackoffCeiling)
}

// reportSuccess forwards to the health tracker when configured.
func (e *Executor) reportSuccess(pid string) {
	if e.cfg.Health != nil {
		e.cfg.Health.ReportSuccess(pid)
	}
}

// reportFailure forwards to the health tracker when configured.
func (e *Executor) reportFailure(pid string) {
	if e.cfg.Health != nil {
		e.cfg.Health.ReportFailure(pid)
	}
}

// reportQuota marks a class-insufficient_quota rejection as a dedicated
// account cooldown (2026-10-06): the next request must not re-pay the
// exhaustion; the account re-probes at the announced reset window (the
// upstream Retry-After when carried) or hourly.
func (e *Executor) reportQuota(pid string, err error, c Classification) {
	if e.cfg.Health == nil || c.Class != ClassInsufficientQuota {
		return
	}
	var until time.Time
	var ue *provider.UpstreamError
	if errors.As(err, &ue) && ue.RetryAfter > 0 {
		until = e.cfg.now().Add(ue.RetryAfter)
	}
	e.cfg.Health.ReportQuotaExhausted(pid, until)
}

// withModel stamps the chain entry's target model onto the request (the
// alias-resolved or rule-selected model for that provider).
func withModel(req ir.Request, model string) ir.Request {
	req.Model = model
	return req
}
