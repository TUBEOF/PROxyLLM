package routing_test

// The vision workflow, v2 (ADR-018): when every chain entry is known blind,
// images are replaced BEFORE dispatch — each image becomes a placeholder text
// carrying a content ref — and the executor injects an `analyze_image` tool so
// the routed model can send the image plus ITS OWN prompt to a vision model,
// repeatedly, and get targeted answers instead of one second-hand caption.
// When any chain entry may see images, the request passes through untouched.
//
// The tool never reaches the client: its calls are answered server-side and
// its blocks are absent from the final response on every surface.

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tubeof/proxyllm/internal/provider"
	"github.com/tubeof/proxyllm/internal/routing"
	"github.com/tubeof/proxyllm/internal/translate/ir"
)

// toolScriptProvider returns a scripted response per Complete call — the
// failing/answering sequence an emulation round trip exercises.
type toolScriptProvider struct {
	id    string
	resps []ir.Response
	errs  []error // per-call error, taking precedence over the response
	// lastRepeats re-serves the final response instead of panicking on a
	// script overrun — the shape of a model that never stops asking.
	lastRepeats bool

	calls int
	seen  []ir.Request
}

func (p *toolScriptProvider) ID() string                          { return p.id }
func (p *toolScriptProvider) Capabilities() provider.Capabilities { return provider.Capabilities{} }
func (p *toolScriptProvider) RefreshCredentials(_ context.Context) error {
	return nil
}

func (p *toolScriptProvider) Complete(_ context.Context, req ir.Request) (ir.Response, error) {
	i := p.calls
	p.calls++
	p.seen = append(p.seen, req)
	if i < len(p.errs) && p.errs[i] != nil {
		return ir.Response{}, p.errs[i]
	}
	if i >= len(p.resps) {
		if !p.lastRepeats {
			panic("toolScriptProvider: script exhausted")
		}
		return p.resps[len(p.resps)-1], nil
	}
	return p.resps[i], nil
}

func (p *toolScriptProvider) Stream(_ context.Context, req ir.Request) (<-chan ir.StreamEvent, func(), error) {
	p.calls++
	p.seen = append(p.seen, req)
	return nil, func() {}, errors.New("stream not expected in this test")
}

// toolCall returns one analyze_image tool call part for the given input JSON.
func toolCall(input string) ir.Part {
	return ir.ToolCallPart{ID: "call-1", Name: "analyze_image", Input: json.RawMessage(input)}
}

// testRef computes the image ref the spec defines: the first 12 hex chars of
// the SHA-256 of the DECODED image bytes — what the placeholder shows and
// what the model then quotes back.
func testRef(data string) string {
	raw, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:6])
}

// blindLookup reports every chain entry as learned-blind.
func blindLookup() func(context.Context, string, string) (bool, bool) {
	return func(_ context.Context, _, _ string) (bool, bool) { return false, true }
}

func analyzeExecutor(main provider.Provider, vision provider.Provider, lookup func(context.Context, string, string) (bool, bool), mut func(*routing.ExecutorConfig)) *routing.Executor {
	cfg := routing.ExecutorConfig{
		Registry:     regOf(main, vision),
		Health:       routing.NewHealthTracker(),
		RetrySleeper: func(time.Duration) {},
		CapabilityFallbacks: []routing.ChainEntry{
			{ProviderID: "vision", Model: "vl-1"},
		},
		ModelVisionLookup: lookup,
	}
	if mut != nil {
		mut(&cfg)
	}
	return routing.NewExecutor(cfg)
}

func findTool(req ir.Request, name string) (ir.Tool, bool) {
	for _, t := range req.Tools {
		if t.Name == name {
			return t, true
		}
	}
	return ir.Tool{}, false
}

func TestExecute_AllBlindChainCaptionsAndInjectsAnalyzeImage(t *testing.T) {
	main := &toolScriptProvider{id: "main", resps: []ir.Response{
		{ID: "ok", Content: []ir.Part{ir.TextPart{Text: "the screenshot shows a build failure"}}, Usage: ir.Usage{InputTokens: 10, OutputTokens: 2}},
	}}
	vision := &recordingProvider{id: "vision", text: "a terminal with a stack trace",
		usage: ir.Usage{InputTokens: 10, OutputTokens: 5}}

	ctx := routing.WithClientKeyID(context.Background(), "client-a")
	e := analyzeExecutor(main, vision, blindLookup(), nil)

	resp, trace, err := e.Execute(ctx, []routing.ChainEntry{{ProviderID: "main", Model: "m"}}, imageRequest("what does this error say?"))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if main.calls != 1 {
		t.Fatalf("routed model calls = %d, want 1 — the image must not reach a known-blind model for rejection", main.calls)
	}

	// The model saw a placeholder with a content ref, not the image.
	sent := main.seen[0]
	if len(sent.Messages) != 1 {
		t.Fatalf("dispatch messages = %d, want 1", len(sent.Messages))
	}
	parts := partStrings(sent.Messages[0].Parts)
	if len(parts) == 2 && strings.HasPrefix(parts[1], "image:") {
		t.Fatalf("image still present in the dispatch: %q", parts[1])
	}
	var place string
	for _, s := range parts {
		if strings.Contains(s, "Bild 1 (ref ") {
			place = s
		}
	}
	if place == "" {
		t.Fatalf("no image placeholder found in dispatch: %q", parts)
	}
	place = strings.TrimPrefix(place, "text:")
	if !strings.Contains(place, "call the analyze_image tool") {
		t.Errorf("placeholder does not point the model at the tool: %q", place)
	}
	between := place[len("[Bild 1 (ref "):strings.Index(place, ")")]
	if len(between) != 12 || !isHex12(between) {
		t.Errorf("placeholder ref = %q, want a 12-hex content id", between)
	}

	def, ok := findTool(sent, "analyze_image")
	if !ok {
		t.Fatal("analyze_image def missing from the dispatch tools")
	}
	if !def.Emulated {
		t.Error("analyze_image def is not emulated — the client would receive its calls")
	}
	var schema struct {
		Type       string   `json:"type"`
		Required   []string `json:"required"`
		Properties map[string]struct {
			Type string `json:"type"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(def.InputSchema, &schema); err != nil {
		t.Fatalf("parse input schema: %v", err)
	}
	if schema.Type != "object" || len(schema.Required) != 2 {
		t.Errorf("input schema = %s, want object with image_ref+prompt required", def.InputSchema)
	}

	// The bootstrap caption ran over the configured vision model, uncached.
	if vision.calls != 1 {
		t.Fatalf("vision model calls = %d, want 1", vision.calls)
	}
	if len(vision.seen[0].Messages) != 1 {
		t.Error("vision model received more than image+instruction")
	}

	// Traced: the consultation names the substitution and the tool.
	consults := eventsOf(trace, routing.EventCapabilityFallback)
	if len(consults) != 1 || !strings.Contains(consults[0].Reason, "analyze_image") {
		t.Errorf("capability_fallback events = %+v, want the pre-dispatch substitution naming the tool", consults)
	}
	if len(eventsOf(trace, routing.EventImageCaption)) != 1 {
		t.Error("image_caption event missing")
	}

	// Usage: the bootstrap caption's spend must be folded in.
	if resp.Usage.InputTokens != 20 || resp.Usage.OutputTokens != 7 {
		t.Errorf("usage = %v, want the dispatch (10/2) + caption (10/5) sum", resp.Usage)
	}
}

func isHex12(s string) bool {
	for _, r := range s {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return false
		}
	}
	return len(s) == 12
}

func TestExecute_SightedChainPassesThroughUntouched(t *testing.T) {
	for _, tt := range []struct {
		name   string
		canSee bool
		known  bool
	}{
		{"known-sighted", true, true},
		{"unknown-capability", false, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			main := &toolScriptProvider{id: "main", resps: []ir.Response{{
				ID: "ok", Content: []ir.Part{ir.TextPart{Text: "seen it"}}, Usage: ir.Usage{InputTokens: 10, OutputTokens: 2},
			}}}
			vision := &recordingProvider{id: "vision"}
			lookup := func(context.Context, string, string) (bool, bool) { return tt.canSee, tt.known }
			e := analyzeExecutor(main, vision, lookup, nil)

			_, trace, err := e.Execute(context.Background(),
				[]routing.ChainEntry{{ProviderID: "main", Model: "m"}}, imageRequest("look"))
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}

			parts := partStrings(main.seen[0].Messages[0].Parts)
			if len(parts) != 2 || !strings.HasPrefix(parts[1], "image:") {
				t.Errorf("image not passed through untouched: %q", parts)
			}
			if _, ok := findTool(main.seen[0], "analyze_image"); ok {
				t.Error("analyze_image def injected on a sighted chain")
			}
			if vision.calls != 0 {
				t.Error("vision model called on a pass-through chain")
			}
			if consults := eventsOf(trace, routing.EventCapabilityFallback); len(consults) != 0 {
				t.Errorf("capability_fallback events = %d, want none", len(consults))
			}
		})
	}
}

func TestExecute_NoVisionFallbackLeavesImageRequestUntouched(t *testing.T) {
	main := &toolScriptProvider{id: "main", resps: []ir.Response{
		{ID: "ok", Content: []ir.Part{ir.TextPart{Text: "served blind request"}}},
	}}
	vision := &recordingProvider{id: "vision"}
	e := analyzeExecutor(main, vision, blindLookup(), func(c *routing.ExecutorConfig) {
		c.CapabilityFallbacks = nil
	})

	_, trace, err := e.Execute(context.Background(),
		[]routing.ChainEntry{{ProviderID: "main", Model: "m"}}, imageRequest("look"))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	parts := partStrings(main.seen[0].Messages[0].Parts)
	if len(parts) != 2 || !strings.HasPrefix(parts[1], "image:") {
		t.Errorf("image replaced without any vision fallback available: %q", parts)
	}
	if _, ok := findTool(main.seen[0], "analyze_image"); ok {
		t.Error("analyze_image def injected with no vision model to answer it")
	}
	if consults := eventsOf(trace, routing.EventCapabilityFallback); len(consults) != 0 {
		t.Errorf("capability_fallback events = %d, want none", len(consults))
	}
}

func TestExecute_FailedCaptionStillSubstitutesWithUnavailablePlaceholder(t *testing.T) {
	main := &toolScriptProvider{id: "main", resps: []ir.Response{
		{ID: "ok", Content: []ir.Part{ir.TextPart{Text: "I cannot see it, but the tool can help"}}},
	}}
	vision := &recordingProvider{id: "vision", errs: []error{gapErr(), gapErr(), gapErr(), gapErr(), gapErr(), gapErr(), gapErr(), gapErr()}}
	e := analyzeExecutor(main, vision, blindLookup(), nil)

	_, trace, err := e.Execute(context.Background(),
		[]routing.ChainEntry{{ProviderID: "main", Model: "m"}}, imageRequest("look"))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	parts := partStrings(main.seen[0].Messages[0].Parts)
	var place string
	for _, s := range parts {
		if strings.Contains(s, "Bild 1 (ref ") {
			place = s
		}
	}
	if place == "" || !strings.Contains(place, "not available") {
		t.Errorf("failed caption must still substitute with an unavailability placeholder: %q", parts)
	}
	if _, ok := findTool(main.seen[0], "analyze_image"); !ok {
		t.Error("analyze_image def missing despite a failed bootstrap caption")
	}
	if consults := eventsOf(trace, routing.EventCapabilityFallback); len(consults) != 1 {
		t.Errorf("capability_fallback events = %d, want 1", len(consults))
	}
}

func TestExecute_VisionToolDisabledSubstitutesWithoutTool(t *testing.T) {
	main := &toolScriptProvider{id: "main", resps: []ir.Response{
		{ID: "ok", Content: []ir.Part{ir.TextPart{Text: "described only"}}},
	}}
	vision := &recordingProvider{id: "vision", text: "a terminal with a stack trace"}
	e := analyzeExecutor(main, vision, blindLookup(), func(c *routing.ExecutorConfig) {
		off := false
		c.Caption.VisionTool = &off
	})

	_, _, err := e.Execute(context.Background(),
		[]routing.ChainEntry{{ProviderID: "main", Model: "m"}}, imageRequest("look"))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	parts := partStrings(main.seen[0].Messages[0].Parts)
	if !strings.Contains(strings.Join(parts, " "), "a terminal with a stack trace") {
		t.Errorf("caption not substituted with vision_tool off: %q", parts)
	}
	if _, ok := findTool(main.seen[0], "analyze_image"); ok {
		t.Error("analyze_image def injected while vision_tool is off")
	}
}

func TestExecute_AnalyzeImageRoundTripAnswersAndAccountsUsage(t *testing.T) {
	input, _ := json.Marshal(map[string]string{"image_ref": testRef("iVBORw0KGgo="), "prompt": "what error text?"})
	img := ir.ImagePart{Source: "base64", MediaType: "image/png", Data: "iVBORw0KGgo="}
	main := &toolScriptProvider{id: "main", resps: []ir.Response{
		// Round 1: the model asks the tool about the image it just received.
		{ID: "r1", Content: []ir.Part{toolCall(string(input))}, Usage: ir.Usage{InputTokens: 100, OutputTokens: 20}, StopReason: ir.StopToolUse},
		// Round 2: the answer came back; the model answers the client.
		{ID: "r2", Content: []ir.Part{ir.TextPart{Text: "the exact error is ERR_42"}}, Usage: ir.Usage{InputTokens: 50, OutputTokens: 9}},
	}}
	vision := &recordingProvider{id: "vision", text: "the error text is ERR_42",
		usage: ir.Usage{InputTokens: 10, OutputTokens: 5}}

	ctx := routing.WithClientKeyID(context.Background(), "client-a")
	e := analyzeExecutor(main, vision, blindLookup(), nil)

	resp, trace, err := e.Execute(ctx,
		[]routing.ChainEntry{{ProviderID: "main", Model: "m"}},
		ir.Request{Model: "m", Messages: []ir.Message{
			{Role: ir.RoleUser, Parts: []ir.Part{ir.TextPart{Text: "read the screenshot"}, img}},
		}})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// The client never sees an analyze_image block.
	for _, part := range resp.Content {
		if call, ok := part.(ir.ToolCallPart); ok && call.Name == "analyze_image" {
			t.Errorf("analyze_image tool call surfaced to the client: %+v", call)
		}
		switch part.(type) {
		case ir.ServerToolUsePart, ir.ServerToolResultPart:
			t.Errorf("server-shaped analyze_image block surfaced: %T", part)
		}
	}

	// The re-dispatch carried the answer in a tool_result.
	var resultTexts []string
	if len(main.seen) > 1 {
		last := main.seen[len(main.seen)-1]
		lastMsg := last.Messages[len(last.Messages)-1]
		if lastMsg.Role == ir.RoleUser {
			for _, p := range lastMsg.Parts {
				if tr, ok := p.(ir.ToolResultPart); ok {
					resultTexts = append(resultTexts, partStrings(tr.Content)...)
				}
			}
		}
	}
	if len(resultTexts) == 0 || !strings.Contains(strings.Join(resultTexts, " "), "ERR_42") {
		t.Errorf("re-dispatch tool_result texts = %q, want the vision model's answer", resultTexts)
	}

	// The routed model sent its OWN prompt to the vision model.
	var visionPrompts []string
	for _, seen := range vision.seen {
		for _, part := range seen.Messages[0].Parts {
			if tr, ok := part.(ir.TextPart); ok {
				visionPrompts = append(visionPrompts, tr.Text)
			}
		}
	}
	if !strings.Contains(strings.Join(visionPrompts, "\n"), "what error text?") {
		t.Errorf("vision prompts = %v, want the model's own tool prompt", visionPrompts)
	}

	// Usage: dispatches + bootstrap caption + tool answer, honestly summed.
	if got := resp.Usage; got.InputTokens != 170 || got.OutputTokens != 39 {
		t.Errorf("usage = %+v, want 170/39 (two dispatches + two vision calls)", got)
	}

	// Traced as its own event with the answering model + usage.
	anns := eventsOf(trace, routing.EventAnalyzeImage)
	if len(anns) != 1 {
		t.Fatalf("analyze_image events = %d, want 1", len(anns))
	}
	if anns[0].ProviderID != "vision" || anns[0].Model != "vl-1" {
		t.Errorf("analyze_image event = %+v, want the answering vision model", anns[0])
	}
	if anns[0].Usage == nil || anns[0].Usage.InputTokens != 10 {
		t.Errorf("analyze_image event usage = %+v, want the tool call's spend", anns[0].Usage)
	}

	// Child ops: an analyze_image row with its payload (drawer shows the
	// tool's own input and the vision answer, redacted upstream of the write).
	ops := routing.ChildOps(trace.Events())
	var annOps []routing.ChildOp
	for _, op := range ops {
		if op.Kind == routing.OpAnalyzeImage {
			annOps = append(annOps, op)
		}
	}
	if len(annOps) != 1 {
		t.Fatalf("analyze_image child ops = %d, want 1 (%v)", len(annOps), ops)
	}
	if annOps[0].ProviderID != "vision" || annOps[0].Usage == nil {
		t.Errorf("analyze_image child op = %+v, want provider + usage", annOps[0])
	}

	var annPayloads []routing.ChildPayload
	for _, p := range trace.Payloads() {
		if p.Tool == "analyze_image" {
			annPayloads = append(annPayloads, p)
		}
	}
	if len(annPayloads) != 1 || !strings.Contains(annPayloads[0].Output, "ERR_42") {
		t.Fatalf("analyze_image payload = %+v, want the vision answer", annPayloads)
	}
}

func TestExecute_AnalyzeImageErrorInputsAreErrorResults(t *testing.T) {
	main := &toolScriptProvider{id: "main", resps: []ir.Response{
		{ID: "r1", Content: []ir.Part{toolCall(`{"image_ref":"000000000000","prompt":"what?"}`)}},
		{ID: "mid", Content: []ir.Part{toolCall(`{"image_ref":"","prompt":""}`)}},
		{ID: "r2", Content: []ir.Part{ir.TextPart{Text: "recovered"}}},
	}}
	vision := &recordingProvider{id: "vision", text: "answer"}

	ctx := routing.WithClientKeyID(context.Background(), "client-a")
	e := analyzeExecutor(main, vision, blindLookup(), nil)

	resp, _, err := e.Execute(ctx, []routing.ChainEntry{{ProviderID: "main", Model: "m"}}, imageRequest("look"))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if main.calls != 3 {
		t.Fatalf("routed model calls = %d, want 3 — error results let the loop recover, never abort it", main.calls)
	}

	// Both error round trips must have carried {"error_code":"invalid_tool_input"}.
	errResults := 0
	for _, msg := range main.seen[len(main.seen)-1].Messages {
		if msg.Role != ir.RoleUser {
			continue
		}
		for _, p := range msg.Parts {
			tr, ok := p.(ir.ToolResultPart)
			if !ok || !tr.IsError {
				continue
			}
			if strings.Contains(strings.Join(partStrings(tr.Content), " "), "invalid_tool_input") {
				errResults++
			}
		}
	}
	if errResults != 2 {
		t.Errorf("invalid_tool_input error results = %d, want 2 (unknown ref; empty ref/prompt)", errResults)
	}
	if len(resp.Content) == 0 || !strings.Contains(strings.Join(partStrings(resp.Content), " "), "recovered") {
		t.Errorf("final response = %v", resp.Content)
	}
}

func TestExecute_AnalyzeImageCallsTheAnswerCache(t *testing.T) {
	// One executor, so the caption cache persists across the three turns the
	// test drives. All six dispatches come from one scripted chat provider.
	c := ir.ImagePart{Source: "base64", MediaType: "image/png", Data: "iVBORw0KGgo="}
	ref := testRef(c.Data)
	main := &toolScriptProvider{id: "main", resps: []ir.Response{
		// Turn 1 (question "read the screenshot").
		{ID: "r1", Content: []ir.Part{toolCall(`{"image_ref":"` + ref + `","prompt":"what error text?"}`)}},
		{ID: "r2", Content: []ir.Part{ir.TextPart{Text: "turn one done"}}, Usage: ir.Usage{InputTokens: 7, OutputTokens: 1}},
		// Turn 2: same image, a different client question — the bootstrap is
		// task-independent (ADR-018 addendum 2026-10-18) and hits the cache;
		// the model asks the SAME question about the image: cache too.
		{ID: "r3", Content: []ir.Part{toolCall(`{"image_ref":"` + ref + `","prompt":"what error text?"}`)}},
		{ID: "r4", Content: []ir.Part{ir.TextPart{Text: "turn two done"}}, Usage: ir.Usage{InputTokens: 7, OutputTokens: 1}},
		// Turn 3: same client question, the model asks a NEW question about
		// the image — only that ask is paid.
		{ID: "r5", Content: []ir.Part{toolCall(`{"image_ref":"` + ref + `","prompt":"how many windows?"}`)}},
		{ID: "r6", Content: []ir.Part{ir.TextPart{Text: "turn three done"}}, Usage: ir.Usage{InputTokens: 7, OutputTokens: 1}},
	}}
	vision := &recordingProvider{id: "vision", text: "the error text is ERR_42"}

	ctx := routing.WithClientKeyID(context.Background(), "client-a")
	e := analyzeExecutor(main, vision, blindLookup(), nil)

	imageTurn := func(question string) {
		t.Helper()
		if _, _, err := e.Execute(ctx, []routing.ChainEntry{{ProviderID: "main", Model: "m"}},
			ir.Request{Model: "m", Messages: []ir.Message{
				{Role: ir.RoleUser, Parts: []ir.Part{ir.TextPart{Text: question}, c}},
			}}); err != nil {
			t.Fatalf("Execute (%s): %v", question, err)
		}
	}

	imageTurn("read the screenshot")
	if got := vision.calls; got != 2 {
		t.Fatalf("vision calls after turn 1 = %d, want 2 (bootstrap caption + the tool's answer)", got)
	}

	imageTurn("and now?")
	if got := vision.calls - 2; got != 0 {
		t.Errorf("vision calls grew by %d on turn 2, want 0 — the bootstrap is task-independent (a cache hit), and the identical tool ask is cached too", got)
	}

	if _, _, err := e.Execute(ctx, []routing.ChainEntry{{ProviderID: "main", Model: "m"}},
		ir.Request{Model: "m", Messages: []ir.Message{{Role: ir.RoleUser, Parts: []ir.Part{ir.TextPart{Text: "and now?"}, c}}}}); err != nil {
		t.Fatalf("Execute (turn 3): %v", err)
	}
	if got := vision.calls - 2; got != 1 {
		t.Errorf("vision calls grew by %d on turn 3, want exactly 1 — the bootstrap is cached, but the changed tool ask must re-describe", got)
	}

	// The turn-3 answer for the new prompt must not equal the turn-1 cached
	// text: the describer saw a different instruction. Prove the cache did
	// not serve a stale entry by checking the vision model saw the new ask.
	lastSeen := vision.seen[len(vision.seen)-1]
	prompt := lastSeen.Messages[0].Parts[len(lastSeen.Messages[0].Parts)-1].(ir.TextPart).Text
	if !strings.Contains(prompt, "how many windows?") {
		t.Errorf("vision model's last instruction = %q, want the new ask verbatim", prompt)
	}
}

func TestExecute_MixedForeignAndAnalyzeImageSurfacesClientCallOnly(t *testing.T) {
	call := `{"prompt":"what error text?"}`
	foreign := ir.ToolCallPart{ID: "call-c", Name: "client_tool", Input: json.RawMessage(`{"x":1}`)}
	main := &toolScriptProvider{id: "main", resps: []ir.Response{
		{ID: "r1", Content: []ir.Part{
			ir.TextPart{Text: "thinking out loud"},
			foreign,
			toolCall(call),
		}, Usage: ir.Usage{InputTokens: 10, OutputTokens: 2}},
	}}
	vision := &recordingProvider{id: "vision", text: "answer"}
	e := analyzeExecutor(main, vision, blindLookup(), nil)

	resp, trace, err := e.Execute(context.Background(),
		[]routing.ChainEntry{{ProviderID: "main", Model: "m"}}, imageRequest("look"))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	foundForeign := false
	for _, part := range resp.Content {
		if c, ok := part.(ir.ToolCallPart); ok {
			if c.Name == "analyze_image" {
				t.Error("analyze_image tool call surfaced to the client in the mixed corner")
			}
			if c.Name == "client_tool" {
				foundForeign = true
			}
		}
	}
	if !foundForeign {
		t.Errorf("the client's own tool call must survive the mixed corner: %v", resp.Content)
	}
	if main.calls != 1 {
		t.Errorf("routed model calls = %d, want 1 — a mixed response is not re-dispatched", main.calls)
	}
	if anns := eventsOf(trace, routing.EventAnalyzeImage); len(anns) != 1 || !strings.Contains(anns[0].Reason, "mixed") {
		t.Errorf("the dropped mixed call must be traced: %+v", anns)
	}
}

func TestExecuteStream_AllBlindImageRequestRidesCompleteLoopAndSynthesizes(t *testing.T) {
	c := ir.ImagePart{Source: "base64", MediaType: "image/png", Data: "iVBORw0KGgo="}
	input, _ := json.Marshal(map[string]string{"image_ref": testRef("iVBORw0KGgo="), "prompt": "what error text?"})
	main := &toolScriptProvider{id: "main", resps: []ir.Response{
		{ID: "r1", Content: []ir.Part{toolCall(string(input))}, Usage: ir.Usage{InputTokens: 100, OutputTokens: 20}},
		{ID: "r2", Content: []ir.Part{ir.TextPart{Text: "the exact error is ERR_42"}}, Usage: ir.Usage{InputTokens: 50, OutputTokens: 9}},
	}}
	vision := &recordingProvider{id: "vision", text: "the error text is ERR_42",
		usage: ir.Usage{InputTokens: 10, OutputTokens: 5}}

	ctx := routing.WithClientKeyID(context.Background(), "client-a")
	e := analyzeExecutor(main, vision, blindLookup(), nil)

	ch, cancel, trace, err := e.ExecuteStream(ctx,
		[]routing.ChainEntry{{ProviderID: "main", Model: "m"}},
		ir.Request{Model: "m", Stream: true, Messages: []ir.Message{
			{Role: ir.RoleUser, Parts: []ir.Part{ir.TextPart{Text: "read the screenshot"}, c}},
		}})
	if err != nil {
		_ = cancel
		t.Fatalf("ExecuteStream: %v", err)
	}
	defer cancel()
	if ch == nil {
		t.Fatal("nil stream")
	}

	events := collectStreamEvents(ctx, ch)
	if len(events) == 0 {
		t.Fatal("no stream events")
	}

	// The dispatch saw the placeholder + the injected tool.
	parts := partStrings(main.seen[0].Messages[0].Parts)
	if strings.Contains(strings.Join(parts, " "), "image:iVBOR") {
		t.Errorf("image dispatched raw on an all-blind chain: %q", parts)
	}
	if _, ok := findTool(main.seen[0], "analyze_image"); !ok {
		t.Error("analyze_image def missing on the streamed dispatch")
	}
	if main.calls != 2 {
		t.Errorf("dispatches = %d, want 2 (tool round + final)", main.calls)
	}
	if vision.calls != 2 {
		t.Errorf("vision calls = %d, want 2 (bootstrap caption + tool answer)", vision.calls)
	}

	// The synthesized stream's terminal usage folds everything.
	var lastUsage *ir.Usage
	for _, ev := range events {
		if ev.Type == ir.MessageDelta && ev.MessageDelta != nil {
			lastUsage = &ev.MessageDelta.Usage
		}
	}
	if lastUsage == nil {
		t.Fatal("no MessageDelta usage in the synthesized stream")
	}
	if lastUsage.InputTokens != 170 || lastUsage.OutputTokens != 39 {
		t.Errorf("stream usage = %+v, want 170/39", *lastUsage)
	}

	// Substitution/injection are traced.
	if consults := eventsOf(trace, routing.EventCapabilityFallback); len(consults) != 1 {
		t.Errorf("capability_fallback events = %d, want 1", len(consults))
	}
	if len(eventsOf(trace, routing.EventAnalyzeImage)) != 1 {
		t.Error("analyze_image event missing from the streamed request")
	}
}

// collectStreamEvents drains the channel to Done/cancel.
func collectStreamEvents(ctx context.Context, in <-chan ir.StreamEvent) []ir.StreamEvent {
	var out []ir.StreamEvent
	for {
		select {
		case <-ctx.Done():
			return out
		case ev, ok := <-in:
			if !ok {
				return out
			}
			out = append(out, ev)
			if ev.Type == ir.Done {
				return out
			}
		}
	}
}

// The emulated loop's safety nets (round cap, emulation deadline) must apply
// the same rule the mixed corner does: the injected tool's unanswered calls
// are DROPPED from what surfaces — a client that never declared analyze_image
// must never receive a tool_use for it, no matter how degraded the vision
// fallback was (ADR-018: "an unknown tool_use must never reach the client").
func TestExecute_AnalyzeImageBudgetExhaustionDropsTheToolFromTheClient(t *testing.T) {
	ref := testRef("iVBORw0KGgo=")
	input, _ := json.Marshal(map[string]string{"image_ref": ref, "prompt": "what error text?"})
	// A model that keeps calling the tool and never writes text: the fake
	// repeats its only answer — one tool_call, forever.
	one := ir.Response{ID: "loop", Content: []ir.Part{toolCall(string(input))}, Usage: ir.Usage{InputTokens: 9, OutputTokens: 1}}
	main := &toolScriptProvider{id: "main", resps: []ir.Response{one}, lastRepeats: true}
	// A vision fallback that always fails (the degraded scenario).
	vision := &recordingProvider{id: "vision", errs: []error{gapErr(), gapErr(), gapErr(), gapErr(), gapErr(), gapErr(), gapErr(), gapErr(), gapErr(), gapErr(), gapErr(), gapErr(), gapErr(), gapErr(), gapErr(), gapErr(), gapErr(), gapErr()}}

	cfg := routing.ExecutorConfig{
		Registry:            regOf(main, vision),
		Health:              routing.NewHealthTracker(),
		RetrySleeper:        func(time.Duration) {},
		CapabilityFallbacks: []routing.ChainEntry{{ProviderID: "vision", Model: "vl-1"}},
		ModelVisionLookup:   blindLookup(),
		// Force the loop's deadline safety net, not the round cap.
		EmulationTotalBudget: 200 * time.Millisecond,
	}
	e := routing.NewExecutor(cfg)

	resp, trace, err := e.Execute(context.Background(),
		[]routing.ChainEntry{{ProviderID: "main", Model: "m"}}, imageRequest("look"))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	for _, part := range resp.Content {
		if call, ok := part.(ir.ToolCallPart); ok && call.Name == "analyze_image" {
			t.Errorf("budget exhaustion leaked the injected tool to the client: %+v", call)
		}
	}

	// Every unanswered call is traced as a decline: the budget-exhausted
	// drops carry that reason; the erroring tool calls carry their own.
	declines := 0
	for _, call := range eventsOf(trace, routing.EventAnalyzeImage) {
		if strings.Contains(call.Reason, "not answered: emulation") {
			declines++
		}
	}
	if declines == 0 {
		t.Errorf("no decline traced for the dropped terminal calls: %v", eventsOf(trace, routing.EventAnalyzeImage))
	}
}

// Two tool asks on the same image that agree on their first 500 runes but
// differ in the tail must NOT share the cached answer: the key hashes the
// FULL prompt (one truncated reuse is a missed reuse; a wrong reuse is a
// wrong answer served confidently).
func TestExecute_AnalyzeImageLongPromptsDoNotShareTheCache(t *testing.T) {
	ref := testRef("iVBORw0KGgo=")
	long := strings.Repeat("list every visible file path in the screenshot ", 12) // > 500 runes
	in1, _ := json.Marshal(map[string]string{"image_ref": ref, "prompt": long + "alphabetically"})
	in2, _ := json.Marshal(map[string]string{"image_ref": ref, "prompt": long + "by length"})
	main := &toolScriptProvider{id: "main", resps: []ir.Response{
		{ID: "r1", Content: []ir.Part{toolCall(string(in1))}},
		{ID: "r2", Content: []ir.Part{ir.TextPart{Text: "done 1"}}, Usage: ir.Usage{InputTokens: 7, OutputTokens: 1}},
		{ID: "r3", Content: []ir.Part{toolCall(string(in2))}},
		{ID: "r4", Content: []ir.Part{ir.TextPart{Text: "done 2"}}, Usage: ir.Usage{InputTokens: 7, OutputTokens: 1}},
	}}
	vision := &recordingProvider{id: "vision", text: "an answer"}

	ctx := routing.WithClientKeyID(context.Background(), "client-a")
	e := analyzeExecutor(main, vision, blindLookup(), nil)
	if _, _, err := e.Execute(ctx, []routing.ChainEntry{{ProviderID: "main", Model: "m"}}, imageRequest("q")); err != nil {
		t.Fatalf("Execute (ask 1): %v", err)
	}
	before := vision.calls
	if _, _, err := e.Execute(ctx, []routing.ChainEntry{{ProviderID: "main", Model: "m"}}, imageRequest("q")); err != nil {
		t.Fatalf("Execute (ask 2): %v", err)
	}
	if vision.calls != before+1 {
		t.Errorf("the second, differently-tailed ask consumed %d vision calls, want 1 (re-describe) — a shared cache key serves the first ask's answer as the second's", vision.calls-before)
	}
}

// A client that itself declares a tool named analyze_image owns that name:
// the executor must not inject a second same-name def (OpenAI-compat
// upstreams reject duplicates) and must not steal the client's calls —
// interception happens only against the executor's OWN injected, emulated
// def.
func TestExecute_ClientDeclaredAnalyzeImageIsNotHijacked(t *testing.T) {
	main := &toolScriptProvider{id: "main", resps: []ir.Response{
		// The model calls the CLIENT's analyze_image — foreign to the loop.
		{ID: "r1", Content: []ir.Part{ir.ToolCallPart{ID: "cc", Name: "analyze_image",
			Input: json.RawMessage(`{"image_ref":"whatever"}`)}}, Usage: ir.Usage{InputTokens: 10, OutputTokens: 2}},
	}}
	vision := &recordingProvider{id: "vision", text: "should never be asked"}
	e := analyzeExecutor(main, vision, blindLookup(), nil)

	req := imageRequest("look")
	req.Tools = []ir.Tool{{Name: "analyze_image", Description: "the client's own tool"}}
	resp, _, err := e.Execute(context.Background(),
		[]routing.ChainEntry{{ProviderID: "main", Model: "m"}}, req)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Exactly one analyze_image def rode the dispatch — the client's.
	for _, seen := range main.seen {
		count := 0
		for _, tl := range seen.Tools {
			if tl.Name == "analyze_image" {
				count++
			}
		}
		if count != 1 {
			t.Errorf("dispatch carries %d analyze_image defs, want exactly the client's one", count)
		}
	}
	found := false
	for _, part := range resp.Content {
		if call, ok := part.(ir.ToolCallPart); ok && call.Name == "analyze_image" {
			found = true // the client's call surfaces
		}
	}
	if !found {
		t.Error("the client's analyze_image call did not surface")
	}
	// The vision model saw exactly the bootstrap caption — never the
	// client's tool call.
	if vision.calls != 1 {
		t.Errorf("vision model calls = %d, want 1 (the bootstrap caption only)", vision.calls)
	}
	if len(vision.seen) == 1 {
		for _, part := range vision.seen[0].Messages[0].Parts {
			if t2, ok := part.(ir.TextPart); ok && strings.Contains(t2.Text, "whatever") {
				t.Errorf("the client's tool arguments reached the vision model: %q", t2.Text)
			}
		}
	}
}

// The observation the user filed (pinned end to end): a session that carries
// an image in its transcript receives three different client messages over
// it — and the VISION MODELS must be asked exactly once, on the first one.
// Question steering is the tool's job, not a re-caption with the newest
// message (ADR-018 addendum 2026-10-18). The placeholder text stays
// bytestable across all three turns.
func TestExecute_BlindChainImageIsDescribedOncePerSession(t *testing.T) {
	c := ir.ImagePart{Source: "base64", MediaType: "image/png", Data: "iVBORw0KGgo="}
	main := &toolScriptProvider{id: "main", resps: []ir.Response{
		{ID: "t1", Content: []ir.Part{ir.TextPart{Text: "turn one"}}, Usage: ir.Usage{InputTokens: 5, OutputTokens: 1}},
		{ID: "t2", Content: []ir.Part{ir.TextPart{Text: "turn two"}}, Usage: ir.Usage{InputTokens: 5, OutputTokens: 1}},
		{ID: "t3", Content: []ir.Part{ir.TextPart{Text: "turn three"}}, Usage: ir.Usage{InputTokens: 5, OutputTokens: 1}},
	}}
	vision := &recordingProvider{id: "vision", text: "a screenshot of a terminal"}

	ctx := routing.WithClientKeyID(context.Background(), "client-a")
	e := analyzeExecutor(main, vision, blindLookup(), nil)

	places := map[string]bool{}
	for i, question := range []string{"first message", "second message, different words", "third message again"} {
		resp, trace, err := e.Execute(ctx, []routing.ChainEntry{{ProviderID: "main", Model: "m"}},
			ir.Request{Model: "m", Messages: []ir.Message{
				{Role: ir.RoleUser, Parts: []ir.Part{ir.TextPart{Text: question}, c}},
			}})
		if err != nil {
			t.Fatalf("turn %d: %v", i+1, err)
		}
		if len(resp.Content) == 0 || !strings.Contains(strings.Join(partStrings(resp.Content), " "), "turn") {
			t.Errorf("turn %d did not serve the transcript: +%v", i+1, resp.Content)
		}
		// The dispatch body repeats the same placeholder text every turn.
		for _, seen := range main.seen {
			if strings.Contains(strings.Join(partStrings(seen.Messages[0].Parts), " "), "Bild 1 (ref ") {
				places[string(seen.Messages[0].Parts[1].(ir.TextPart).Text)] = true
			}
		}
		_ = trace
	}
	if vision.calls != 1 {
		t.Errorf("vision describe calls = %d, want exactly 1 (the first message) — re-captions keyed on the newest message are the defect this pins", vision.calls)
	}
	if len(places) != 1 {
		t.Errorf("placeholder text drifted between turns: %d distinct texts", len(places))
	}
}
