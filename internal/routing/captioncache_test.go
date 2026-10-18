package routing_test

// The caption cache's behaviour at the executor: an image that has been
// described for a question is not described again when the same turn is
// re-sent, and the text that replaces it is byte-identical every time.
//
// The failure this pins is the one an agentic client produces without trying:
// it re-sends its own transcript on every turn, image parts included, so a
// conversation that carries a screenshot used to pay one vision call per turn
// for as long as it lived. The cache must not go further than that: a NEW
// question needs a description written for it (ChangedQuestionReDescribesTheImage).

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tubeof/proxyllm/internal/routing"
	"github.com/tubeof/proxyllm/internal/translate/ir"
)

// captionExecutor is the arrangement every test here starts from: a text-only
// model whose first dispatch is rejected for the image, and a vision model that
// describes it. errs is scripted per dispatch so a test can make the rejection
// happen on the turns it wants.
func captionExecutor(main *recordingProvider, vision *recordingProvider) *routing.Executor {
	return routing.NewExecutor(routing.ExecutorConfig{
		Registry:     regOf(main, vision),
		Health:       routing.NewHealthTracker(),
		RetrySleeper: func(time.Duration) {},
		CapabilityFallbacks: []routing.ChainEntry{
			{ProviderID: vision.id, Model: "vl-1"},
		},
	})
}

func TestExecutor_Caption_SecondIdenticalRequestServesTheCachedDescription(t *testing.T) {
	// Turn 1: rejected, described, served. Turn 2: rejected again, served from
	// the cache — no second description.
	main := &recordingProvider{id: "main", errs: []error{gapErr(), nil, gapErr(), nil}}
	vision := &recordingProvider{id: "vision", text: "a stack trace pointing at nil.go:42"}

	e := captionExecutor(main, vision)
	chain := []routing.ChainEntry{{ProviderID: "main", Model: "m"}}

	if _, _, err := e.Execute(context.Background(), chain, imageRequest("what does this error say?")); err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	_, trace2, err := e.Execute(context.Background(), chain, imageRequest("what does this error say?"))
	if err != nil {
		t.Fatalf("turn 2: %v", err)
	}

	if vision.calls != 1 {
		t.Fatalf("vision calls = %d, want 1: the second turn must reuse the description", vision.calls)
	}
	// The answering model still gets the description in place of the image, and
	// that text is the same one turn 1 produced.
	first := partStrings(main.seen[1].Messages[0].Parts)
	second := partStrings(main.seen[3].Messages[0].Parts)
	if len(second) != 2 || second[1] != first[1] {
		t.Errorf("turn 2 parts = %q, want the same description as turn 1 (%q)", second, first)
	}

	caps := eventsOf(trace2, routing.EventImageCaption)
	if len(caps) != 1 {
		t.Fatalf("turn 2 image_caption events = %d, want 1 (the substitution is never silent)", len(caps))
	}
	if !caps[0].CacheHit {
		t.Error("turn 2 caption event is not marked as a cache hit")
	}
	if caps[0].Usage != nil || caps[0].LatencyMS != 0 {
		t.Errorf("a cache hit reports spend it did not incur: usage=%v latency=%d", caps[0].Usage, caps[0].LatencyMS)
	}
	if caps[0].ProviderID != "vision" || caps[0].Model != "vl-1" {
		t.Errorf("cached event = %s/%s, want the describer that produced the text", caps[0].ProviderID, caps[0].Model)
	}
}

// A description is written to answer the question it was asked for. When the
// client re-sends the same image with a NEW question — what an agentic loop
// does on every turn — the previous question's description must not stand in
// for the current one: it is focused on something the client is no longer
// asking about, and the answering model cannot see what the new question needs.
func TestExecutor_Caption_ChangedQuestionReDescribesTheImage(t *testing.T) {
	main := &recordingProvider{id: "main", errs: []error{gapErr(), nil, gapErr(), nil}}
	vision := &recordingProvider{id: "vision", text: "description for the first question"}

	e := captionExecutor(main, vision)
	chain := []routing.ChainEntry{{ProviderID: "main", Model: "m"}}

	if _, _, err := e.Execute(context.Background(), chain, imageRequest("what does the first page show?")); err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	vision.text = "description for the second question"
	if _, _, err := e.Execute(context.Background(), chain, imageRequest("which numbers are in the table?")); err != nil {
		t.Fatalf("turn 2: %v", err)
	}

	if vision.calls != 2 {
		t.Fatalf("vision calls = %d, want 2: a changed question must not be served the previous question's description", vision.calls)
	}
	second := vision.seen[1].Messages[0].Parts[1].(ir.TextPart).Text
	if !strings.Contains(second, "which numbers are in the table?") {
		t.Errorf("turn 2 prompt did not carry the new question: %q", second)
	}
	got := partStrings(main.seen[3].Messages[0].Parts)
	if len(got) != 2 || got[1] != "text:description for the second question" {
		t.Errorf("turn 2 parts = %q, want the newly produced description", got)
	}
}

// A description that came from the cache is a substitution without an
// operation, so it produces no sub-operation row — neither a caption row nor,
// by the subsumption rule, the consultation row it replaced.
func TestChildOps_CacheHitCreatesNoChildRow(t *testing.T) {
	ops := routing.ChildOps([]routing.Event{
		{Type: routing.EventAttempt, ProviderID: "main", Model: "m"},
		{Type: routing.EventFailed, ProviderID: "main", Class: "capability_gap"},
		{Type: routing.EventImageCaption, ProviderID: "vision", Model: "vl-1",
			CacheHit: true, Reason: "184 character(s) reused from the caption cache"},
		{Type: routing.EventCapabilityFallback, Capability: "vision",
			Reason: "chain exhausted on capability gaps; reused 1 cached description(s)"},
		{Type: routing.EventAttempt, ProviderID: "main", Model: "m"},
	})
	for _, op := range ops {
		if op.Kind == routing.OpImageCaption || op.Kind == routing.OpCapabilityFallback {
			t.Errorf("cache-hit rescue produced a %q row; only real requests belong in the log", op.Kind)
		}
	}
}

// A genuinely produced description still becomes a row, with what it cost —
// this is the case the row exists for.
func TestChildOps_FreshCaptionCarriesItsUsageAndLatency(t *testing.T) {
	spent := ir.Usage{InputTokens: 41, OutputTokens: 12}
	ops := routing.ChildOps([]routing.Event{
		{Type: routing.EventImageCaption, ProviderID: "vision", Model: "vl-1",
			Reason: "184 character(s) described", LatencyMS: 820, Usage: &spent},
	})
	if len(ops) != 1 || ops[0].Kind != routing.OpImageCaption {
		t.Fatalf("ops = %+v, want one image_caption row", ops)
	}
	if ops[0].LatencyMS != 820 || ops[0].Usage == nil || ops[0].Usage.OutputTokens != 12 {
		t.Errorf("row = %+v, want the describer call's latency and tokens", ops[0])
	}
}

// Descriptions are scoped to the client key that provoked them: the describer
// is shown that client's question as a steering hint, and its answer can quote
// what it was shown.
func TestExecutor_Caption_CacheIsScopedToTheClientKey(t *testing.T) {
	main := &recordingProvider{id: "main", errs: []error{gapErr(), nil, gapErr(), nil}}
	vision := &recordingProvider{id: "vision"}

	e := captionExecutor(main, vision)
	chain := []routing.ChainEntry{{ProviderID: "main", Model: "m"}}

	if _, _, err := e.Execute(routing.WithClientKeyID(context.Background(), "key-a"), chain, imageRequest("q1")); err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	if _, _, err := e.Execute(routing.WithClientKeyID(context.Background(), "key-b"), chain, imageRequest("q2")); err != nil {
		t.Fatalf("turn 2: %v", err)
	}
	if vision.calls != 2 {
		t.Errorf("vision calls = %d, want 2: a second client must not be served another client's description", vision.calls)
	}
}

func TestExecutor_Caption_DifferentImageIsNotCachedHit(t *testing.T) {
	main := &recordingProvider{id: "main", errs: []error{gapErr(), nil, gapErr(), nil}}
	vision := &recordingProvider{id: "vision"}

	e := captionExecutor(main, vision)
	chain := []routing.ChainEntry{{ProviderID: "main", Model: "m"}}

	if _, _, err := e.Execute(context.Background(), chain, imageRequest("q1")); err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	other := imageRequest("q2")
	other.Messages[0].Parts[1] = ir.ImagePart{Source: "base64", MediaType: "image/png", Data: "QUJDRA=="}
	if _, _, err := e.Execute(context.Background(), chain, other); err != nil {
		t.Fatalf("turn 2: %v", err)
	}
	if vision.calls != 2 {
		t.Errorf("vision calls = %d, want 2: a new image is a new description", vision.calls)
	}
}

// The description is billed to the turn that needed it, and only to that turn.
func TestExecutor_Caption_CacheHitAddsNoUsage(t *testing.T) {
	captionSpend := ir.Usage{InputTokens: 900, OutputTokens: 40}
	main := &recordingProvider{id: "main", errs: []error{gapErr(), nil, gapErr(), nil}}
	vision := &recordingProvider{id: "vision", usage: captionSpend}

	e := captionExecutor(main, vision)
	chain := []routing.ChainEntry{{ProviderID: "main", Model: "m"}}

	first, _, err := e.Execute(context.Background(), chain, imageRequest("q1"))
	if err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	second, _, err := e.Execute(context.Background(), chain, imageRequest("q1"))
	if err != nil {
		t.Fatalf("turn 2: %v", err)
	}

	if first.Usage.OutputTokens != captionSpend.OutputTokens {
		t.Errorf("turn 1 output tokens = %d, want the caption's %d folded in",
			first.Usage.OutputTokens, captionSpend.OutputTokens)
	}
	if second.Usage.OutputTokens != 0 {
		t.Errorf("turn 2 output tokens = %d, want 0: nothing was described", second.Usage.OutputTokens)
	}
}

// What shapes a description is the base prompt and the describer list. Changing
// either must not serve the old text.
func TestExecutor_Caption_ConfigChangeInvalidatesTheCache(t *testing.T) {
	main := &recordingProvider{id: "main", errs: []error{gapErr(), nil, gapErr(), nil}}
	vision := &recordingProvider{id: "vision"}
	vision2 := &recordingProvider{id: "vision-two"}

	e := routing.NewExecutor(routing.ExecutorConfig{
		Registry:     regOf(main, vision, vision2),
		Health:       routing.NewHealthTracker(),
		RetrySleeper: func(time.Duration) {},
		CapabilityFallbacks: []routing.ChainEntry{
			{ProviderID: "vision", Model: "vl-1"},
		},
	})
	chain := []routing.ChainEntry{{ProviderID: "main", Model: "m"}}

	if _, _, err := e.Execute(context.Background(), chain, imageRequest("q1")); err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	e.SetCapabilityFallbacks([]routing.ChainEntry{{ProviderID: "vision-two", Model: "vl-2"}})
	if _, _, err := e.Execute(context.Background(), chain, imageRequest("q1")); err != nil {
		t.Fatalf("turn 2: %v", err)
	}
	if vision2.calls != 1 {
		t.Errorf("the newly configured describer was called %d times, want 1", vision2.calls)
	}
}

// When every candidate's model row has learned it cannot see images, the
// rejection is a known outcome rather than a discovery. The images are then
// replaced before dispatch: no rejected round trip, and no vision call.
func TestExecutor_Caption_KnownBlindChainSkipsTheDispatch(t *testing.T) {
	main := &recordingProvider{id: "main", errs: []error{gapErr(), nil}}
	vision := &recordingProvider{id: "vision"}

	e := routing.NewExecutor(routing.ExecutorConfig{
		Registry:     regOf(main, vision),
		Health:       routing.NewHealthTracker(),
		RetrySleeper: func(time.Duration) {},
		CapabilityFallbacks: []routing.ChainEntry{
			{ProviderID: "vision", Model: "vl-1"},
		},
		ModelVisionLookup: func(context.Context, string, string) (bool, bool) {
			return false, true // known to be blind
		},
	})
	chain := []routing.ChainEntry{{ProviderID: "main", Model: "m"}}

	// Turn 1 discovers the image: rejected once, described once, served.
	if _, _, err := e.Execute(context.Background(), chain, imageRequest("q1")); err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	// Turn 2 cannot be rejected for the image any more — it is gone before the
	// dispatch, and the chain is walked exactly once. The question is unchanged,
	// which is what makes the description already known.
	_, trace2, err := e.Execute(context.Background(), chain, imageRequest("q1"))
	if err != nil {
		t.Fatalf("turn 2: %v", err)
	}

	if main.calls != 3 {
		t.Errorf("chain dispatches = %d, want 3 (turn 1 rejected + retried, turn 2 once)", main.calls)
	}
	if vision.calls != 1 {
		t.Errorf("vision calls = %d, want 1", vision.calls)
	}
	if hasEvent(trace2, routing.EventFailed) {
		t.Error("turn 2 recorded a failed attempt: the rejection was known in advance and must not be paid for")
	}
	if got := partStrings(main.seen[2].Messages[0].Parts); len(got) != 2 || got[1] == "image:iVBORw0KGgo=" {
		t.Errorf("turn 2 dispatched parts = %q, want the image already replaced", got)
	}
	// The trace must not describe a rejection that never happened. Turn 2 was
	// never refused for the image — nothing was attempted until after the
	// replacement — so "the chain was exhausted and is being retried" would be a
	// fiction an operator would chase.
	for _, ev := range eventsOf(trace2, routing.EventCapabilityFallback) {
		if strings.Contains(ev.Reason, "exhausted") || strings.Contains(ev.Reason, "retrying") {
			t.Errorf("pre-dispatch substitution traced as %q", ev.Reason)
		}
	}
}

// The streaming surface shares the rescue, so it shares the cache.
func TestExecutor_Caption_StreamCacheHitDispatchesNoVisionCall(t *testing.T) {
	main := &recordingProvider{id: "main", streamErrs: []error{gapErr(), nil, gapErr(), nil}}
	vision := &recordingProvider{id: "vision"}

	e := captionExecutor(main, vision)
	chain := []routing.ChainEntry{{ProviderID: "main", Model: "m"}}

	drain := func() {
		t.Helper()
		ch, cancel, _, err := e.ExecuteStream(context.Background(), chain, imageRequest("q"))
		if err != nil {
			t.Fatalf("ExecuteStream: %v", err)
		}
		defer cancel()
		for range ch {
		}
	}
	drain()
	drain()

	if vision.calls != 1 {
		t.Errorf("vision calls = %d, want 1 across two streamed turns", vision.calls)
	}
}
