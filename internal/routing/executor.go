package routing

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/tubeof/proxyllm/internal/provider"
	"github.com/tubeof/proxyllm/internal/translate/ir"
)

// Executor is the request-level failover loop. It walks the resolved chain
// and, per failed attempt, applies the two-tier error classification:
//
//	retry_same    → re-attempt the SAME provider, up to the retry budget,
//	                honoring Retry-After, capped by the backoff ceiling
//	failover      → move to the next chain entry
//	terminal      → end the request: content_filter (compliance), client
//	                errors (invalid at any provider), mid-stream failures
//	                (the client already holds partial output)
//
// Streaming requests are never retried once dispatched: the stream
// path knows only dispatch-failure failover (before the first event) and
// terminal. Every transition is recorded in the Trace and every
// attempt outcome is reported to the HealthTracker.
type Executor struct {
	cfg ExecutorConfig

	// captions holds descriptions of images the proxy has already seen, keyed
	// by image content. It is what keeps the image-to-text rescue from
	// describing the same image again on every turn of a conversation, and it
	// keeps the substituted text stable across turns — see captioncache.go.
	captions *captionCache

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

	// MaxRetriesPerProvider bounds same-provider retries (1–2).
	// Zero means the default (2).
	MaxRetriesPerProvider int

	// MaxProviders bounds how many chain entries are attempted (2–3).
	// Zero means the default (3).
	MaxProviders int

	// BackoffCeiling caps the same-provider retry delay (2–4 s).
	// Zero means the default (3 s). Retry-After is honored but capped
	// here.
	BackoffCeiling time.Duration

	// RetryBackoff is the base delay between same-provider retries;
	// each retry doubles it up to the ceiling. Zero means the default
	// (250 ms).
	RetryBackoff time.Duration

	// RetrySleeper suspends the retry backoff (defaults to time.Sleep).
	// Injectable so tests exercise the retry discipline without real
	// waiting (deterministic tests).
	RetrySleeper func(time.Duration)

	// Emulator executes emulated web tools (the web-tool accounts) for
	// the tool-call round-trip loop in Execute. Nil disables actual
	// emulation but keeps the loop: model tool calls against emulated
	// tools are answered with a protocol-conform "unavailable" error
	// result (the ADR-015 degrade contract: no Firecrawl key →
	// documented unavailable errors), then re-dispatched.
	Emulator Emulator

	// EmulationCallBudget bounds ONE emulated tool invocation across the whole
	// account walk. The pool is a single Emulator that walks its list
	// internally, so this is a per-invocation budget no matter how many
	// accounts are configured — without it, N accounts meant N × the client
	// timeout inline on the request path. Zero means the default (30 s).
	EmulationCallBudget time.Duration

	// EmulationTotalBudget bounds the entire emulation loop for one request:
	// every round trip and every web-tool call together. A safety net for a
	// model that keeps issuing tool calls (the round cap bounds the count, this
	// bounds the time); operators bound invocations per tool with max_uses.
	// Zero means the default (5 min).
	EmulationTotalBudget time.Duration

	// CapabilityFallbacks are the model-level fallback chain entries
	// (ADR-014): the vision models used
	// to DESCRIBE an image when the rule chain exhausted with capability_gap
	// failures only. Each entry is a fully resolved (provider, wire-model)
	// pair — the httpapi layer resolves the stored model IDs before Execute.
	// Nil/empty disables image captioning (the request terminates with the
	// last gap error). Swappable at runtime via SetCapabilityFallbacks.
	//
	// Since ADR-014 these models never receive the conversation, only the
	// image and the caption instruction, so their own context window is
	// irrelevant to whether they can serve.
	CapabilityFallbacks []ChainEntry

	// Caption tunes the image-captioning step (ADR-014). The zero value is
	// valid and yields the documented defaults.
	Caption CaptionConfig

	// OnCapabilityGap is invoked once per chain entry whose attempt failed
	// with the capability_gap class: the wiring layer
	// persists the gap into the model row's capabilities (vision=false
	// learning). It receives the full chain entry — provider ID plus the
	// upstream wire model, the pair the storage layer needs to find the
	// model row. Optional; nil disables learning. The callback must not
	// block — it is called inline on the failover path.
	OnCapabilityGap func(entry ChainEntry)

	// ModelVisionLookup reports whether a model row knows that it cannot see
	// images. The executor uses it for one thing: when every candidate of a
	// chain is known blind and the request's images are already described, the
	// rejection is certain, so the images are replaced before dispatch and the
	// round trip that would end in it is not paid. Nil keeps the reactive path
	// only, which is what a caller without a model table gets.
	ModelVisionLookup func(ctx context.Context, providerID, model string) (canSeeImages, known bool)

	// now stamps attempt latencies; injectable for tests.
	now func() time.Time
}

// Failover defaults: the documented bounds are 1–2 retries per provider,
// 2–3 providers total, 2–4 s backoff ceiling. We encode the upper bounds so
// operators tighten via config, rather than shipping behavior that is
// silently narrower than the documented bounds.
const (
	defaultMaxRetriesPerProvider = 2
	defaultMaxProviders          = 3
	defaultBackoffCeiling        = 3 * time.Second
	defaultRetryBackoff          = 250 * time.Millisecond

	// defaultEmulationCallBudget bounds one emulated tool invocation across the
	// account walk (see ExecutorConfig.EmulationCallBudget).
	defaultEmulationCallBudget = 30 * time.Second

	// defaultEmulationTotalBudget bounds the whole emulation loop for a request
	// (see ExecutorConfig.EmulationTotalBudget).
	defaultEmulationTotalBudget = 5 * time.Minute
)

// ErrChainExhausted is the terminal error when every attempt in the chain
// failed within its budget. It wraps the last attempt's error.
var ErrChainExhausted = errors.New("routing: chain exhausted")

// ErrNoDecisionProvider marks a resolved chain no entry of which can serve a
// decision request — no provider.Decider implementation, or the entry is not
// in the registry. A configuration defect (the capability flag promised what
// the implementation does not deliver), not an upstream failure.
var ErrNoDecisionProvider = errors.New("routing: no decision-capable provider in the chain")

// exhaustedError carries the last attempt's error for callers that need
// the upstream cause.
//
// last can be nil: a walk where no attempt ever ran (every chain entry skipped
// as unregistered, or the chain arrived already cancelled) reaches the same exit
// as a walk whose attempts all failed. That case used to panic in Error() — an
// error value that crashes when formatted crashes in the log path, which is
// exactly where the least is recoverable.
type exhaustedError struct {
	last error

	// classes records how each attempt in the chain failed, in order. The
	// client-visible status is derived from them rather than from the last
	// attempt alone: when every target failed the same way, that way is the
	// answer, but a chain that failed for several different reasons has no
	// single target to blame — the gateway is what could not deliver.
	classes []Class

	// timedOut records whether any attempt ran out of time.
	timedOut bool
}

func (e *exhaustedError) Error() string {
	if e.last == nil {
		return ErrChainExhausted.Error() + ": no attempt was made"
	}
	return ErrChainExhausted.Error() + ": " + e.last.Error()
}

func (e *exhaustedError) Unwrap() error { return e.last }

// ChainFailure reports how an exhausted chain failed, so the surface can choose
// the status the client sees.
//
// ok is false for any error that is not an exhausted chain. mixed is true when
// the attempts did not all fail the same way — the case where reporting the last
// target's status would point the client at a target that is not the problem.
// timedOut is true when any attempt ran out of time.
func ChainFailure(err error) (mixed, timedOut, ok bool) {
	var ex *exhaustedError
	if !errors.As(err, &ex) {
		return false, false, false
	}
	first := Class("")
	for _, c := range ex.classes {
		if first == "" {
			first = c
			continue
		}
		if c != first {
			mixed = true
			break
		}
	}
	return mixed, ex.timedOut, true
}

// record keeps the classification of one failed attempt; the walk calls it for
// every attempt it makes.
func (e *exhaustedError) record(c Classification, err error) {
	e.classes = append(e.classes, c.Class)
	if errors.Is(err, context.DeadlineExceeded) {
		e.timedOut = true
		return
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		e.timedOut = true
	}
}

// NewExecutor returns an Executor for cfg, filling the failover defaults.
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
	if cfg.EmulationCallBudget <= 0 {
		cfg.EmulationCallBudget = defaultEmulationCallBudget
	}
	if cfg.EmulationTotalBudget <= 0 {
		cfg.EmulationTotalBudget = defaultEmulationTotalBudget
	}
	if cfg.RetrySleeper == nil {
		cfg.RetrySleeper = time.Sleep
	}
	if cfg.now == nil {
		cfg.now = time.Now
	}
	return &Executor{cfg: cfg, captions: newCaptionCache(captionCacheEntries)}
}

// Execute performs a non-streaming request against the chain with the
// full retry/failover discipline, plus the emulated-tool
// round-trip loop: when the request carries emulated web tools (the
// applyEmulation emulate-firecrawl policy) and the model answers with
// tool calls against them, the executor answers the calls via the
// configured Emulator and re-dispatches until the model answers without
// them, budgets refuse with protocol-conform error results, or the round
// cap surfaces the last response. The returned Trace records every
// transition.
func (e *Executor) Execute(ctx context.Context, chain []ChainEntry, req ir.Request) (ir.Response, *Trace, error) {
	trace := &Trace{}
	req = e.substituteKnownBlind(ctx, chain, req, trace)
	resp, err := e.executeWithEmulation(ctx, chain, req, trace)
	if err != nil {
		return resp, trace, err
	}
	// The response is the client's: thinking blocks it never asked for are
	// dropped here — the reference returns thinking blocks only for
	// requests that enabled thinking.
	resp = applyResponseThinking(req, resp, trace)
	return resp, trace, nil
}

// ExecuteDecision walks a resolved chain for one decision operation (System
// One / Jev, the OpenAI Decisions API). Decision calls are non-streaming
// and single-shot: each entry is tried once — no same-provider retry — and
// a failure fails over to the next entry. Health is reported like any other
// attempt, and every transition is traced. Entries that cannot serve
// decisions (no provider.Decider, not in the registry, health cooldown) are
// skipped with a traced reason, never a panic.
func (e *Executor) ExecuteDecision(ctx context.Context, chain []ChainEntry, req ir.DecisionRequest) (ir.DecisionResponse, *Trace, error) {
	trace := &Trace{}
	var lastErr error
	attempted := 0
	for i, entry := range chain {
		p, ok := e.cfg.Registry.Get(entry.ProviderID)
		if !ok {
			trace.Skip(entry.ProviderID, "not registered")
			continue
		}
		dec, ok := p.(provider.Decider)
		if !ok {
			trace.Skip(entry.ProviderID, "no decision support")
			continue
		}
		if e.cfg.Health != nil && e.cfg.Health.Cooldown(entry.ProviderID) {
			trace.Skip(entry.ProviderID, "health cooldown")
			continue
		}
		attempted++
		trace.Attempt(entry.ProviderID, entry.Model)
		attempt := req
		attempt.Model = entry.Model
		start := time.Now()
		resp, err := dec.Decide(ctx, attempt)
		latency := time.Since(start).Milliseconds()
		if err == nil {
			e.reportSuccess(entry.ProviderID)
			return resp, trace, nil
		}
		c := ClassifyError(err, PreStream)
		status, code, _ := AsUpstreamError(err)
		trace.Failed(entry.ProviderID, c, status, code, latency)
		e.reportFailureFor(entry.ProviderID, c, status)
		lastErr = err
		// A cancelled context is the client's (or a budget of ours), never
		// an observation about the provider: terminal, no failover — the
		// chat path's rule, kept here.
		if ctx.Err() != nil {
			return ir.DecisionResponse{}, trace, err
		}
		if i+1 < len(chain) {
			trace.Failover(entry.ProviderID, chain[i+1].ProviderID, "decision failover after "+string(c.Class))
		}
	}
	if lastErr != nil {
		return ir.DecisionResponse{}, trace, lastErr
	}
	if attempted == 0 {
		return ir.DecisionResponse{}, trace, ErrNoDecisionProvider
	}
	return ir.DecisionResponse{}, trace, ErrChainOverloaded
}

// substituteKnownBlind replaces the images of a request that is certain to be
// rejected for them.
//
// The rescue in captionImages normally runs after the chain has failed, which
// costs one rejected round trip per turn for a conversation carrying an image.
// When every candidate's model row has already learned that it cannot see
// images, that rejection is not a discovery — it is a known outcome — and the
// only thing the round trip buys is latency. So the images are replaced up
// front instead.
//
// The gate is deliberately narrow. A single candidate that might see images, or
// whose row has no opinion, leaves the request untouched: an unnecessary
// description costs image fidelity, and the reactive rescue is still there to
// catch the rejection. One image without a cached description also leaves it
// untouched — a partial rewrite would show the model a conversation the client
// never sent.
func (e *Executor) substituteKnownBlind(ctx context.Context, chain []ChainEntry, req ir.Request, trace *Trace) ir.Request {
	if e.cfg.ModelVisionLookup == nil || len(chain) == 0 || len(req.ImageRefs()) == 0 {
		return req
	}
	for _, entry := range chain {
		canSee, known := e.cfg.ModelVisionLookup(ctx, entry.ProviderID, entry.Model)
		if !known || canSee {
			return req
		}
	}
	described, _, ok := e.captionImages(ctx, req, trace, true)
	if !ok {
		return req
	}
	return described
}

// executeOnce walks the chain once with the full retry/failover
// discipline — one upstream answer (or the terminal error). When the walk
// ends with ONLY capability_gap failures, the configured capability fallback
// models get one walk of their own, appended after the regular chain — the
// request itself is valid, another model can serve it.
func (e *Executor) executeOnce(ctx context.Context, chain []ChainEntry, req ir.Request, trace *Trace) (ir.Response, error) {
	return e.walkChain(ctx, chain, req, trace, true)
}

// walkChain is executeOnce's core with the fallback gate: allowFallback
// is false on the fallback models' own walk (no recursive fallback).
func (e *Executor) walkChain(ctx context.Context, chain []ChainEntry, req ir.Request, trace *Trace, allowFallback bool) (ir.Response, error) {
	var lastErr error
	ex := &exhaustedError{}

	// pendingBackoff is the delay before the next same-provider retry,
	// computed from the failed attempt's Retry-After when present.
	var pendingBackoff time.Duration

	// Gap bookkeeping: a chain is gap-exhausted when every
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
			ex.record(c, err)
			status, code, _ := AsUpstreamError(err)
			trace.Failed(entry.ProviderID, c, status, code, latency)
			e.reportFailureFor(entry.ProviderID, c, status)
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
				// A retry is only worth making inside the time we are willing to
				// wait. When the provider announces a window longer than that —
				// a daily quota that replenishes tonight, not in a minute —
				// retrying after our own ceiling would be a wasted round trip
				// against an account we already know is out. Cool it for as long
				// as it asked and move on.
				if reset := announcedReset(err); reset > e.cfg.BackoffCeiling {
					e.cfg.Health.ReportQuotaExhausted(entry.ProviderID, e.cfg.now().Add(reset))
					trace.Retry(entry.ProviderID,
						"announced reset "+reset.String()+" exceeds the retry budget: cooling the account")
					break
				}
				if attempt+1 < attempts {
					pendingBackoff = e.retryDelay(err, attempt+1)
					trace.Retry(entry.ProviderID, "rate_limit: retry same provider within budget")
					continue
				}
				// Budget spent: fail over.
			case ActionFailOver:
				// Budget is irrelevant: move on immediately.
			}
			if ci+1 < len(chain) && ci+1 < e.cfg.MaxProviders {
				trace.Failover(entry.ProviderID, chain[ci+1].ProviderID, "chain failover after "+string(c.Class))
			}
			break
		}
	}

	// Image-to-text rescue (ADR-014): only a gap-exhausted chain (every
	// attempt a capability_gap, at least one attempt made) is rescued — a 401
	// or a content_filter must not be "rescued" by another model. The vision
	// model describes each image, the descriptions take the images' place, and
	// the SAME chain runs again: the model that answers is the one the operator
	// routed to, and whether the rescue works never depends on a fallback
	// model's context window — the vision model only ever sees one image.
	//
	// One pass only: the re-dispatch runs with the fallback disabled, so a
	// model that rejects the transcript too ends the request rather than
	// starting a second caption round.
	if gapOnly && attempted > 0 && allowFallback {
		if described, captionUsage, ok := e.captionImages(ctx, req, trace, false); ok {
			resp, err := e.walkChain(ctx, chain, described, trace, false)
			resp.Usage = addUsage(resp.Usage, captionUsage)
			return resp, err
		}
	}
	ex.last = lastErr
	return ir.Response{}, ex
}

// SetCapabilityFallbacks atomically replaces the live capability fallback
// entries: the admin API's fallback write pushes the freshly stored model
// list into the executor without restart — same last-known-good live-swap
// contract as SetEmulator).
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

// SetCaptionConfig atomically replaces the live image-caption settings
// (ADR-014). Same last-known-good live-swap contract as the other setters:
// a settings write reaches the running executor without a restart.
func (e *Executor) SetCaptionConfig(cfg CaptionConfig) {
	e.emuMu.Lock()
	defer e.emuMu.Unlock()
	e.cfg.Caption = cfg
}

// currentCaptionConfig reads the live caption settings under the swap lock.
func (e *Executor) currentCaptionConfig() CaptionConfig {
	e.emuMu.RLock()
	defer e.emuMu.RUnlock()
	return e.cfg.Caption
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
// never retried. httpapi calls this after relaying the stream,
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

// ExecuteStream performs a streaming request against the chain: streams
// are never retried once dispatched — dispatch failures (before
// the first event) fail over; anything after the first event is terminal
// and surfaces as an Error event on the returned channel.
//
// The capability fallback applies here as on the complete path: a chain whose
// every DISPATCH failed with a capability_gap (a 400 modality rejection happens
// before the first byte, so the swap is safe) gets one walk over the fallback
// models via p.Stream — no recursion, fallback dispatch gaps terminate.
//
// The returned cancel must be called when the caller is done with the
// channel to release upstream resources. Dispatch-time success is
// reported to the HealthTracker immediately; once the caller has fully
// relayed the stream it must call ReportStreamOutcome so a mid-stream
// failure still counts (ReportStreamOutcome).
func (e *Executor) ExecuteStream(ctx context.Context, chain []ChainEntry, req ir.Request) (<-chan ir.StreamEvent, func(), *Trace, error) {
	// Streaming with emulated web tools: the emulation
	// round-trip loop only exists on the complete path — a streamed tool
	// call would relay raw to the client with no data (a bare tool_use the
	// client cannot use). Once the first tool_use is on the wire it cannot be
	// retrofitted, so requests whose
	// tools include emulated defs ride the COMPLETE emulation loop to
	// finality; the final response synthesizes the event stream below.
	// Latency trade: no token streaming for this request class (the model
	// runs its whole answer before the first byte) — documented, and the
	// trade for protocol-conform tool RESULTS.
	if len(emulatedToolDefs(req.Tools)) > 0 {
		return e.executeEmulatedAsStream(ctx, chain, req)
	}
	trace := &Trace{}
	req = e.substituteKnownBlind(ctx, chain, req, trace)
	ch, cancel, err := e.walkChainStream(ctx, chain, req, trace, true)
	if ch == nil && err != nil {
		return nil, cancel, trace, err
	}
	// The relayed events are the client's answer: an unsolicited thinking
	// block rides them all the way into the client's message and breaks the
	// client's web_fetch helper, so the filter sits on the stream.
	return applyResponseThinkingStream(ctx, req, ch, trace), cancel, trace, err
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
	// The synthesized stream IS the client's answer: filter it on the
	// response (the same rule the relayed path applies to its events).
	resp = applyResponseThinking(req, resp, trace)

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
			// BlockStart + the input_json_delta + BlockStop: every block is
			// closed (the golden stream fixture discipline) — a bare start
			// desynced strict parsers (Claude Code) on the next
			// content_block_start. The input rides the delta like tool_use,
			// not the content_block_start (the empty-init contract).
			ch <- ir.StreamEvent{Type: ir.BlockStart, Index: i, Part: ir.ServerToolUsePart{ID: p.ID, Name: p.Name}}
			ch <- ir.StreamEvent{Type: ir.Delta, Index: i,
				Delta: ir.DeltaPayload{Kind: ir.DeltaToolInput, PartialJSON: string(p.Input)}}
			ch <- ir.StreamEvent{Type: ir.BlockStop, Index: i}
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
	ex := &exhaustedError{}

	// Gap bookkeeping: same contract as the complete path —
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
			// Dispatch succeeded: no failover from here.
			// Health reports success once the stream terminates cleanly;
			// a dispatch success with a later mid-stream failure still
			// counts as one failure.
			e.reportSuccess(entry.ProviderID)
			return ch, cancel, nil
		}
		lastErr = err
		c := ClassifyError(err, PreStream)
		ex.record(c, err)
		status, code, _ := AsUpstreamError(err)
		trace.Failed(entry.ProviderID, c, status, code, latency)
		e.reportFailureFor(entry.ProviderID, c, status)
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
			// moving on: a dispatch error may still have opened a
			// connection, and httpx hands out nil cancel only when it never
			// started one.
			if cancel != nil {
				cancel()
			}
			if ci+1 < len(chain) && ci+1 < e.cfg.MaxProviders {
				trace.Failover(entry.ProviderID, chain[ci+1].ProviderID, "chain failover after "+string(c.Class))
			}
			// No same-provider retry for streams; on to the next
			// chain entry (or terminal exhaustion below).
		case ActionRetrySame:
			// A 429-class dispatch failure on a stream still never
			// retries the same provider — "partial data may already have
			// been sent" upstream. Fail over if a next entry exists.
			if cancel != nil {
				cancel()
			}
			if ci+1 < len(chain) && ci+1 < e.cfg.MaxProviders {
				trace.Failover(entry.ProviderID, chain[ci+1].ProviderID, "stream rate_limit: fail over, never retry")
			}
		}
	}

	// Image-to-text rescue (ADR-014): identical gate to the complete path.
	// The caption calls happen before the first byte of the retry, so the
	// stream discipline is untouched: nothing has been sent to the
	// client yet, and the re-dispatch is a fresh dispatch that either streams
	// or fails.
	if gapOnly && attempted > 0 && allowFallback {
		if described, captionUsage, ok := e.captionImages(ctx, req, trace, false); ok {
			ch, cancel, err := e.walkChainStream(ctx, chain, described, trace, false)
			if err != nil {
				return ch, cancel, err
			}
			return addUsageToStream(ctx, ch, captionUsage), cancel, nil
		}
	}
	ex.last = lastErr
	return nil, func() {}, ex
}

// announcedReset returns the reset window the upstream named, or zero when it
// named none. This is the authoritative half of a rejection: a provider that
// says when it will serve again has told us something no keyword heuristic can,
// which is why a retry decision reads it first.
func announcedReset(err error) time.Duration {
	var ue *provider.UpstreamError
	if errors.As(err, &ue) && ue.RetryAfter > 0 {
		return ue.RetryAfter
	}
	return 0
}

// retryDelay computes the delay before the next same-provider retry: the
// upstream-advertised Retry-After when present, capped by the backoff
// ceiling; otherwise the exponential backoff for the attempt.
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

// reportFailureFor records a failed attempt only when the failure says
// something about the provider's health — see countsTowardHealth for why a
// capability gap, a context-window rejection, a client error or a cancellation
// must not open the breaker. Every classified failure goes through here; the
// unclassified reportFailure below is for callers that have no classification
// to offer.
func (e *Executor) reportFailureFor(pid string, c Classification, status int) {
	if e.cfg.Health == nil || pid == "" || !countsTowardHealth(c, status) {
		return
	}
	e.cfg.Health.ReportFailureKind(pid, c.Class)
}

// reportQuota marks a class-insufficient_quota rejection as a dedicated
// account cooldown: the next request must not re-pay the
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
