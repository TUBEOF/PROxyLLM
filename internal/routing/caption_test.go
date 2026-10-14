package routing_test

// Image-to-text captioning contract (ADR-014). When a rule chain exhausts on
// capability_gap rejections caused by image input, the configured vision
// fallback models DESCRIBE the images; the descriptions replace the images and
// the SAME chain runs again. The vision model never receives the conversation,
// so whether the rescue works does not depend on its context window — the
// property the previous whole-conversation resend could not offer.
//
// Every step is traced (image_caption + capability_fallback), and a rescue
// that cannot run leaves the request untouched and surfaces the original gap
// error rather than a side effect.

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tubeof/proxyllm/internal/provider"
	"github.com/tubeof/proxyllm/internal/routing"
	"github.com/tubeof/proxyllm/internal/translate/ir"
)

// recordingProvider scripts per-attempt outcomes and keeps every request it
// was handed, so a test can assert what each model actually saw.
type recordingProvider struct {
	id         string
	errs       []error // per-Complete result (exhausted → success)
	streamErrs []error // per-Stream dispatch error
	text       string  // Complete's answer text ("" → a default description)
	usage      ir.Usage // what every Complete reports as its spend

	calls int
	seen  []ir.Request
}

func (p *recordingProvider) ID() string                                 { return p.id }
func (p *recordingProvider) Capabilities() provider.Capabilities        { return provider.Capabilities{} }
func (p *recordingProvider) RefreshCredentials(_ context.Context) error { return nil }

func (p *recordingProvider) Complete(_ context.Context, req ir.Request) (ir.Response, error) {
	i := p.calls
	p.calls++
	p.seen = append(p.seen, req)
	if i < len(p.errs) && p.errs[i] != nil {
		return ir.Response{}, p.errs[i]
	}
	text := p.text
	if text == "" {
		text = "a terminal window showing a nil-pointer stack trace"
	}
	return ir.Response{ID: "ok", Content: []ir.Part{ir.TextPart{Text: text}}, Usage: p.usage}, nil
}

func (p *recordingProvider) Stream(_ context.Context, req ir.Request) (<-chan ir.StreamEvent, func(), error) {
	i := p.calls
	p.calls++
	p.seen = append(p.seen, req)
	if i < len(p.streamErrs) && p.streamErrs[i] != nil {
		return nil, func() {}, p.streamErrs[i]
	}
	ch := make(chan ir.StreamEvent, 2)
	ch <- ir.StreamEvent{Type: ir.Start}
	ch <- ir.StreamEvent{Type: ir.Done}
	close(ch)
	return ch, func() {}, nil
}

// imageRequest is the shape the rescue exists for: a text turn plus an image
// (a screenshot the client wants read).
func imageRequest(question string) ir.Request {
	return ir.Request{Model: "m", Messages: []ir.Message{
		{Role: ir.RoleUser, Parts: []ir.Part{
			ir.TextPart{Text: question},
			ir.ImagePart{Source: "base64", MediaType: "image/png", Data: "iVBORw0KGgo="},
		}},
	}}
}

func partStrings(parts []ir.Part) []string {
	var out []string
	for _, p := range parts {
		switch v := p.(type) {
		case ir.TextPart:
			out = append(out, "text:"+v.Text)
		case ir.ImagePart:
			out = append(out, "image:"+v.Data)
		default:
			out = append(out, "other")
		}
	}
	return out
}

func eventsOf(trace *routing.Trace, typ routing.EventType) []routing.Event {
	var out []routing.Event
	for _, ev := range trace.Events() {
		if ev.Type == typ {
			out = append(out, ev)
		}
	}
	return out
}

func TestExecutor_Caption_DescribesImageAndRetriesTheChain(t *testing.T) {
	// The routed model rejects the image once, then serves the captioned retry.
	main := &recordingProvider{id: "main", errs: []error{gapErr()}}
	vision := &recordingProvider{id: "vision", text: "a stack trace pointing at nil.go:42"}

	e := routing.NewExecutor(routing.ExecutorConfig{
		Registry:     regOf(main, vision),
		Health:       routing.NewHealthTracker(),
		RetrySleeper: func(time.Duration) {},
		CapabilityFallbacks: []routing.ChainEntry{
			{ProviderID: "vision", Model: "vl-1"},
		},
	})

	resp, trace, err := e.Execute(context.Background(),
		[]routing.ChainEntry{{ProviderID: "main", Model: "m"}}, imageRequest("what does this error say?"))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if resp.ID != "ok" {
		t.Errorf("the routed chain did not serve the retry: %+v", resp)
	}
	if main.calls != 2 {
		t.Fatalf("routed model calls = %d, want 2 (rejected, then served the retry)", main.calls)
	}
	if vision.calls != 1 {
		t.Fatalf("vision model calls = %d, want 1", vision.calls)
	}

	// The retry the routed model received carries the description where the
	// image was — the client's own text stays put.
	got := partStrings(main.seen[1].Messages[0].Parts)
	want := []string{"text:what does this error say?", "text:a stack trace pointing at nil.go:42"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("retried request parts = %q, want %q", got, want)
	}

	// Never silent: the description and the consultation are both traced, and
	// the caption event names the model that produced it.
	caps := eventsOf(trace, routing.EventImageCaption)
	if len(caps) != 1 {
		t.Fatalf("image_caption events = %d, want 1", len(caps))
	}
	if caps[0].ProviderID != "vision" || caps[0].Model != "vl-1" {
		t.Errorf("caption event = %+v, want the vision model that answered", caps[0])
	}
	if consults := eventsOf(trace, routing.EventCapabilityFallback); len(consults) != 1 {
		t.Errorf("capability_fallback events = %d, want 1", len(consults))
	}
}

// The whole point of the design: the vision model receives the image and the
// instruction, never the conversation. A long session is exactly the case that
// broke the previous resend, so this is the property to pin.
func TestExecutor_Caption_VisionModelSeesOnlyTheImage(t *testing.T) {
	main := &recordingProvider{id: "main", errs: []error{gapErr()}}
	vision := &recordingProvider{id: "vision"}

	// A conversation far larger than any small vision model's window.
	var history []ir.Message
	for i := 0; i < 400; i++ {
		history = append(history, ir.Message{Role: ir.RoleUser, Parts: []ir.Part{
			ir.TextPart{Text: strings.Repeat("x", 500)},
		}})
	}
	history = append(history, ir.Message{Role: ir.RoleUser, Parts: []ir.Part{
		ir.TextPart{Text: "and this screenshot?"},
		ir.ImagePart{Source: "base64", MediaType: "image/png", Data: "iVBORw0KGgo="},
	}})

	e := routing.NewExecutor(routing.ExecutorConfig{
		Registry:     regOf(main, vision),
		Health:       routing.NewHealthTracker(),
		RetrySleeper: func(time.Duration) {},
		CapabilityFallbacks: []routing.ChainEntry{
			{ProviderID: "vision", Model: "vl-1"},
		},
	})

	if _, _, err := e.Execute(context.Background(),
		[]routing.ChainEntry{{ProviderID: "main", Model: "m"}},
		ir.Request{Model: "m", Messages: history}); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if len(vision.seen) != 1 {
		t.Fatalf("vision dispatches = %d, want 1", len(vision.seen))
	}
	sent := vision.seen[0]
	if len(sent.Messages) != 1 {
		t.Fatalf("vision request messages = %d, want 1 — the conversation must not be forwarded", len(sent.Messages))
	}
	parts := sent.Messages[0].Parts
	if len(parts) != 2 {
		t.Fatalf("vision request parts = %d, want the image and the instruction", len(parts))
	}
	img, ok := parts[0].(ir.ImagePart)
	if !ok {
		t.Fatalf("vision request part 0 = %T, want the image", parts[0])
	}
	if img.Data != "iVBORw0KGgo=" || img.MediaType != "image/png" {
		t.Errorf("vision request image mangled: %+v", img)
	}
	if _, ok := parts[1].(ir.TextPart); !ok {
		t.Errorf("vision request part 1 = %T, want the instruction", parts[1])
	}
}

// The instruction is steered by the client's own question: a generic
// description of a screenshot misses the line the client asked about.
func TestExecutor_Caption_PromptCarriesTheClientQuestion(t *testing.T) {
	main := &recordingProvider{id: "main", errs: []error{gapErr()}}
	vision := &recordingProvider{id: "vision"}

	e := routing.NewExecutor(routing.ExecutorConfig{
		Registry:     regOf(main, vision),
		Health:       routing.NewHealthTracker(),
		RetrySleeper: func(time.Duration) {},
		CapabilityFallbacks: []routing.ChainEntry{
			{ProviderID: "vision", Model: "vl-1"},
		},
	})

	if _, _, err := e.Execute(context.Background(),
		[]routing.ChainEntry{{ProviderID: "main", Model: "m"}}, imageRequest("why is the deploy red?")); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	prompt := vision.seen[0].Messages[0].Parts[1].(ir.TextPart).Text
	if !strings.Contains(prompt, "why is the deploy red?") {
		t.Errorf("caption prompt does not carry the client's question: %q", prompt)
	}
	if !strings.Contains(prompt, "transcribe any text visible") {
		t.Errorf("caption prompt does not ask for visible text: %q", prompt)
	}
}

// A request without images has nothing to describe: the original gap error
// surfaces and the vision models are never called.
func TestExecutor_Caption_NoImagesSurfacesTheGapError(t *testing.T) {
	main := &recordingProvider{id: "main", errs: []error{gapErr()}}
	vision := &recordingProvider{id: "vision"}

	e := routing.NewExecutor(routing.ExecutorConfig{
		Registry:     regOf(main, vision),
		Health:       routing.NewHealthTracker(),
		RetrySleeper: func(time.Duration) {},
		CapabilityFallbacks: []routing.ChainEntry{
			{ProviderID: "vision", Model: "vl-1"},
		},
	})

	_, trace, err := e.Execute(context.Background(),
		[]routing.ChainEntry{{ProviderID: "main", Model: "m"}},
		ir.Request{Model: "m", Messages: []ir.Message{
			{Role: ir.RoleUser, Parts: []ir.Part{ir.TextPart{Text: "no image here"}}},
		}})
	if err == nil {
		t.Fatal("a gap-exhausted chain must fail")
	}
	if vision.calls != 0 {
		t.Errorf("vision calls = %d, want 0 — there is nothing to describe", vision.calls)
	}
	if len(eventsOf(trace, routing.EventImageCaption)) != 0 {
		t.Error("an image_caption event was recorded without any image")
	}
}

// When no vision model can describe the image, the request must come back
// UNCHANGED and the ORIGINAL gap error must surface — never a mangled request
// and never a side effect of a rescue that did not happen.
func TestExecutor_Caption_AllVisionModelsFailKeepsRequestAndGapError(t *testing.T) {
	main := &recordingProvider{id: "main", errs: []error{gapErr(), gapErr()}}
	vision := &recordingProvider{id: "vision", errs: []error{errors.New("vision upstream down")}}

	e := routing.NewExecutor(routing.ExecutorConfig{
		Registry:     regOf(main, vision),
		Health:       routing.NewHealthTracker(),
		RetrySleeper: func(time.Duration) {},
		CapabilityFallbacks: []routing.ChainEntry{
			{ProviderID: "vision", Model: "vl-1"},
		},
	})

	_, trace, err := e.Execute(context.Background(),
		[]routing.ChainEntry{{ProviderID: "main", Model: "m"}}, imageRequest("read this"))
	if err == nil {
		t.Fatal("an unrescuable gap chain must fail")
	}
	var up *provider.UpstreamError
	if !errors.As(err, &up) || up.HTTPStatus != 400 {
		t.Errorf("terminal error = %v, want the original typed 400 gap error", err)
	}
	if main.calls != 1 {
		t.Errorf("routed model calls = %d, want 1 — no retry without a description", main.calls)
	}
	// A skipped vision model is reported, not swallowed.
	if skips := eventsOf(trace, routing.EventSkipped); len(skips) == 0 {
		t.Error("a failed caption attempt was not traced")
	}
	if len(eventsOf(trace, routing.EventImageCaption)) != 0 {
		t.Error("an image_caption event was recorded for a call that failed")
	}
}

// One substitution pass, no recursion: a chain that rejects the transcript too
// ends the request instead of captioning again.
func TestExecutor_Caption_NoSecondRoundWhenTheChainStillRejects(t *testing.T) {
	main := &recordingProvider{id: "main", errs: []error{gapErr(), gapErr(), gapErr()}}
	vision := &recordingProvider{id: "vision"}

	e := routing.NewExecutor(routing.ExecutorConfig{
		Registry:     regOf(main, vision),
		Health:       routing.NewHealthTracker(),
		RetrySleeper: func(time.Duration) {},
		CapabilityFallbacks: []routing.ChainEntry{
			{ProviderID: "vision", Model: "vl-1"},
		},
	})

	if _, _, err := e.Execute(context.Background(),
		[]routing.ChainEntry{{ProviderID: "main", Model: "m"}}, imageRequest("read this")); err == nil {
		t.Fatal("a chain that keeps rejecting must fail")
	}
	if vision.calls != 1 {
		t.Errorf("vision calls = %d, want exactly 1 — the rescue runs once", vision.calls)
	}
}

// A 401 exhausted chain is never rescued, images or not: credentials are the
// request's problem and a description would not change them.
func TestExecutor_Caption_NonGapFailuresDoNotTrigger(t *testing.T) {
	main := &recordingProvider{id: "main", errs: []error{
		&provider.UpstreamError{HTTPStatus: 401, Message: "bad key"},
	}}
	vision := &recordingProvider{id: "vision"}

	e := routing.NewExecutor(routing.ExecutorConfig{
		Registry:     regOf(main, vision),
		Health:       routing.NewHealthTracker(),
		RetrySleeper: func(time.Duration) {},
		CapabilityFallbacks: []routing.ChainEntry{
			{ProviderID: "vision", Model: "vl-1"},
		},
	})

	_, trace, err := e.Execute(context.Background(),
		[]routing.ChainEntry{{ProviderID: "main", Model: "m"}}, imageRequest("read this"))
	if err == nil {
		t.Fatal("a 401 chain must fail")
	}
	if vision.calls != 0 {
		t.Error("vision models must not be consulted for a non-gap failure")
	}
	for _, ev := range trace.Events() {
		if ev.Type == routing.EventCapabilityFallback || ev.Type == routing.EventImageCaption {
			t.Errorf("fallback event recorded for a 401 chain: %+v", ev)
		}
	}
}

// An image nested in a tool result is an image too — the walk descends into
// tool-result content, so it is described like any other.
func TestExecutor_Caption_DescribesImagesNestedInToolResults(t *testing.T) {
	main := &recordingProvider{id: "main", errs: []error{gapErr()}}
	vision := &recordingProvider{id: "vision", text: "a rendered chart"}

	e := routing.NewExecutor(routing.ExecutorConfig{
		Registry:     regOf(main, vision),
		Health:       routing.NewHealthTracker(),
		RetrySleeper: func(time.Duration) {},
		CapabilityFallbacks: []routing.ChainEntry{
			{ProviderID: "vision", Model: "vl-1"},
		},
	})

	req := ir.Request{Model: "m", Messages: []ir.Message{
		{Role: ir.RoleUser, Parts: []ir.Part{ir.TextPart{Text: "check the build"}}},
		{Role: ir.RoleTool, Parts: []ir.Part{ir.ToolResultPart{ToolUseID: "t1", Content: []ir.Part{
			ir.TextPart{Text: "screenshot attached"},
			ir.ImagePart{Source: "base64", MediaType: "image/png", Data: "iVBORw0KGgo="},
		}}}},
	}}
	if _, _, err := e.Execute(context.Background(),
		[]routing.ChainEntry{{ProviderID: "main", Model: "m"}}, req); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if vision.calls != 1 {
		t.Fatalf("vision calls = %d, want 1 — the nested image was missed", vision.calls)
	}
	retried := main.seen[1].Messages[1].Parts[0].(ir.ToolResultPart)
	got := partStrings(retried.Content)
	if len(got) != 2 || got[1] != "text:a rendered chart" {
		t.Errorf("nested image not substituted: %q", got)
	}
}

// A remote image is downloaded and inlined before the describer sees it, so a
// provider whose encoder drops URL-sourced images still receives the bytes.
func TestExecutor_Caption_InlinesRemoteImage(t *testing.T) {
	t.Setenv("PROXYLLM_ALLOW_PRIVATE_PROVIDER_URLS", "1")

	const payload = "not-really-a-png"
	images := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte(payload))
	}))
	defer images.Close()

	main := &recordingProvider{id: "main", errs: []error{gapErr()}}
	vision := &recordingProvider{id: "vision", text: "a screenshot"}

	e := routing.NewExecutor(routing.ExecutorConfig{
		Registry:     regOf(main, vision),
		Health:       routing.NewHealthTracker(),
		RetrySleeper: func(time.Duration) {},
		CapabilityFallbacks: []routing.ChainEntry{
			{ProviderID: "vision", Model: "vl-1"},
		},
	})

	req := ir.Request{Model: "m", Messages: []ir.Message{
		{Role: ir.RoleUser, Parts: []ir.Part{
			ir.ImagePart{Source: "url", URL: images.URL + "/shot.png", MediaType: "image/png"},
		}},
	}}
	if _, _, err := e.Execute(context.Background(),
		[]routing.ChainEntry{{ProviderID: "main", Model: "m"}}, req); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if len(vision.seen) != 1 {
		t.Fatalf("vision dispatches = %d, want 1", len(vision.seen))
	}
	img, ok := vision.seen[0].Messages[0].Parts[0].(ir.ImagePart)
	if !ok {
		t.Fatalf("vision dispatch part 0 = %T, want the image", vision.seen[0].Messages[0].Parts[0])
	}
	if img.Source != "base64" {
		t.Fatalf("the remote image was not inlined: %+v", img)
	}
	if img.Data != base64.StdEncoding.EncodeToString([]byte(payload)) {
		t.Errorf("inlined payload = %q, want the fetched bytes", img.Data)
	}
	if img.MediaType != "image/png" {
		t.Errorf("MediaType = %q, want the response's image/png", img.MediaType)
	}
}

// A remote image the guard refuses is left in place and the refusal is traced:
// the describer must never be asked to comment on an image it may not have
// received, because a fabricated description would be indistinguishable from a
// real one downstream.
func TestExecutor_Caption_RefusesGuardedImageURL(t *testing.T) {
	images := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("png"))
	}))
	defer images.Close()

	main := &recordingProvider{id: "main", errs: []error{gapErr()}}
	vision := &recordingProvider{id: "vision"}

	e := routing.NewExecutor(routing.ExecutorConfig{
		Registry:     regOf(main, vision),
		Health:       routing.NewHealthTracker(),
		RetrySleeper: func(time.Duration) {},
		CapabilityFallbacks: []routing.ChainEntry{
			{ProviderID: "vision", Model: "vl-1"},
		},
	})

	req := ir.Request{Model: "m", Messages: []ir.Message{
		{Role: ir.RoleUser, Parts: []ir.Part{
			ir.ImagePart{Source: "url", URL: images.URL + "/shot.png"},
		}},
	}}
	_, trace, err := e.Execute(context.Background(),
		[]routing.ChainEntry{{ProviderID: "main", Model: "m"}}, req)
	if err == nil {
		t.Fatal("an unrescuable gap chain must fail")
	}
	if vision.calls != 0 {
		t.Error("the describer was called for an image the guard refused")
	}
	// Never silent: the refusal names its reason in the trace.
	var reason string
	for _, ev := range eventsOf(trace, routing.EventCapabilityFallback) {
		if strings.Contains(ev.Reason, "image left in place") {
			reason = ev.Reason
		}
	}
	if reason == "" {
		t.Error("the refused image was not traced")
	}
}
