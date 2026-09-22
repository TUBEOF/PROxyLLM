package main

// fixtureTestProvider answers the Models page's Test button
// deterministically (2026-09-22): a fixed "OK" reply for the mdl-test row,
// never a live upstream call (the fixturesrv determinism contract — the
// v1.0.2 lesson: real upstream calls poisoned the baselines). It implements
// the provider interface minimally; only Complete is ever exercised (the
// test request is non-streaming, the proxy surface is never driven by the
// visual suite against this account).

import (
	"context"

	"github.com/tubeof/proxyllm/internal/provider"
	"github.com/tubeof/proxyllm/internal/translate/ir"
)

// fixtureTestProvider is the prv-test account.
type fixtureTestProvider struct{}

func (p *fixtureTestProvider) ID() string { return "prv-test" }

func (p *fixtureTestProvider) Type() string { return "custom-openai" }

func (p *fixtureTestProvider) Capabilities() provider.Capabilities {
	return provider.Capabilities{
		Streaming: true, Tools: true, Vision: true, Thinking: true,
	}
}

// Complete answers the minimal test ask with a fixed acknowledgment.
func (p *fixtureTestProvider) Complete(_ context.Context, _ ir.Request) (ir.Response, error) {
	return ir.Response{
		ID:         "fixture-test",
		Model:      "ok-model",
		StopReason: ir.StopEndTurn,
		Content:    []ir.Part{ir.TextPart{Text: "OK"}},
	}, nil
}

// Stream is never exercised by the visual suite (the test request is
// non-streaming); a protocol-conform error keeps the surface honest if a
// future spec touches it.
func (p *fixtureTestProvider) Stream(_ context.Context, _ ir.Request) (<-chan ir.StreamEvent, func(), error) {
	return nil, func() {}, &provider.UpstreamError{Message: "fixture test provider does not stream"}
}

func (p *fixtureTestProvider) RefreshCredentials(_ context.Context) error { return nil }
