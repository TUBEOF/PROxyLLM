package routing

import (
	"context"
	"errors"
	"time"

	"github.com/proxyllm/proxyllm/internal/provider"
	"github.com/proxyllm/proxyllm/internal/translate/ir"
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
// discipline — one upstream answer (or the terminal error).
func (e *Executor) executeOnce(ctx context.Context, chain []ChainEntry, req ir.Request, trace *Trace) (ir.Response, error) {
	var lastErr error

	// pendingBackoff is the delay before the next same-provider retry,
	// computed from the failed attempt's Retry-After when present (O10).
	var pendingBackoff time.Duration

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
	return ir.Response{}, &exhaustedError{last: lastErr}
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
// The returned cancel must be called when the caller is done with the
// channel to release upstream resources. Dispatch-time success is
// reported to the HealthTracker immediately; once the caller has fully
// relayed the stream it must call ReportStreamOutcome so a mid-stream
// failure still counts (ReportStreamOutcome).
func (e *Executor) ExecuteStream(ctx context.Context, chain []ChainEntry, req ir.Request) (<-chan ir.StreamEvent, func(), *Trace, error) {
	trace := &Trace{}
	var lastErr error

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
		started := e.cfg.now()
		ch, cancel, err := p.Stream(ctx, withModel(req, entry.Model))
		latency := e.cfg.now().Sub(started).Milliseconds()
		if err == nil {
			// Dispatch succeeded: no failover from here (FO-1/FO-5).
			// Health reports success once the stream terminates cleanly;
			// a dispatch success with a later mid-stream failure still
			// counts as one failure.
			e.reportSuccess(entry.ProviderID)
			return ch, cancel, trace, nil
		}
		lastErr = err
		c := ClassifyError(err, PreStream)
		status, code, _ := AsUpstreamError(err)
		trace.Failed(entry.ProviderID, c, status, code, latency)
		e.reportFailure(entry.ProviderID)

		switch c.Action {
		case ActionTerminal:
			if cancel != nil {
				cancel()
			}
			return nil, func() {}, trace, err
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
	return nil, func() {}, trace, &exhaustedError{last: lastErr}
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

// withModel stamps the chain entry's target model onto the request (the
// alias-resolved or rule-selected model for that provider).
func withModel(req ir.Request, model string) ir.Request {
	req.Model = model
	return req
}
