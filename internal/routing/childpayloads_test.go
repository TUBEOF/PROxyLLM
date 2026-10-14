package routing_test

// Spec: a sub-operation's own detail view has to show what THAT step sent and
// received. The parent request's capture shows the conversation and the
// answer; it cannot show the tool exchange that answered a call inside it,
// because that exchange exists only transiently inside the emulation loop.
// The trace therefore collects a payload per emulated call.
//
// The second half of the contract is structural: these payloads must NOT reach
// route_trace. That field is a versioned wire schema the WebUI renders, and it
// stays metadata — a body in it would change the schema and leak content into a
// field nothing redacts on the way in.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tubeof/proxyllm/internal/routing"
	"github.com/tubeof/proxyllm/internal/translate/ir"
)

// The caption half of the same contract: the operator must be able to read what
// the describer was told and what it wrote. The event's reason is a count (a
// timeline fact, and the one field in this path that route_trace persists
// unredacted); the payload is the content the drawer shows, redacted on write
// like every other captured body.
func TestExecutor_Caption_CollectsPayload(t *testing.T) {
	const caption = "a stack trace pointing at nil.go:42"
	main := &recordingProvider{id: "main", errs: []error{gapErr()}}
	vision := &recordingProvider{id: "vision", text: caption}

	e := routing.NewExecutor(routing.ExecutorConfig{
		Registry:            regOf(main, vision),
		Health:              routing.NewHealthTracker(),
		RetrySleeper:        func(time.Duration) {},
		CapabilityFallbacks: []routing.ChainEntry{{ProviderID: "vision", Model: "vl-1"}},
	})

	if _, trace, err := e.Execute(context.Background(),
		[]routing.ChainEntry{{ProviderID: "main", Model: "m"}},
		imageRequest("what does this error say?")); err != nil {
		t.Fatalf("Execute: %v", err)
	} else {
		payloads := trace.Payloads()
		if len(payloads) != 1 {
			t.Fatalf("payloads = %d, want the one caption call", len(payloads))
		}
		p := payloads[0]
		if p.Kind != routing.OpImageCaption {
			t.Errorf("kind = %q, want %q", p.Kind, routing.OpImageCaption)
		}
		if p.ProviderID != "vision" || p.Model != "vl-1" {
			t.Errorf("payload identity = %s/%s, want the describer that answered", p.ProviderID, p.Model)
		}
		if !strings.Contains(p.Output, "nil.go:42") {
			t.Errorf("payload output = %q, want the caption the operator must be able to read", p.Output)
		}
		if strings.TrimSpace(p.Input) == "" {
			t.Error("payload input is empty — the instruction the describer received is half of what the step did")
		}
		if p.MediaType != "image/png" {
			t.Errorf("media type = %q, want the described image's type", p.MediaType)
		}
		if p.ImageBytes <= 0 {
			t.Errorf("image bytes = %d, want the decoded size (a fact worth keeping)", p.ImageBytes)
		}
		// The rule the parent capture follows, applied here too: an image is
		// recorded by type and size, never by its bytes — a caption payload is
		// no place for a base64 payload to reappear.
		if strings.Contains(p.Input+p.Output, "iVBORw0KGgo=") {
			t.Error("image bytes leaked into the caption payload")
		}
	}
}

func TestExecute_CollectsEmulatedToolPayloads(t *testing.T) {
	p := &searchThenFetchProvider{}
	emu := &fakeEmulator{
		searchAnswers: []string{`[{"URL":"https://example.com/article","Title":"Article","EncryptedContent":"","PageAge":""}]`},
		fetchAnswers:  []string{`{"URL":"https://example.com/article","Title":"Article","Source":"text","MediaType":"text/plain","Data":"body","CitationsEnabled":false,"RetrievedAt":""}`},
	}
	e := roundTripExecutor(p, emu)

	req := ir.Request{
		Model: "m",
		Tools: []ir.Tool{
			{Name: "web_search", ServerTool: &ir.ServerToolConfig{MaxUses: 5}, Emulated: true},
			{Name: "web_fetch", ServerTool: &ir.ServerToolConfig{MaxUses: 5}, Emulated: true},
		},
		Messages: []ir.Message{
			{Role: ir.RoleUser, Parts: []ir.Part{ir.TextPart{Text: "research this topic"}}},
		},
	}
	if _, trace, err := e.Execute(context.Background(), []routing.ChainEntry{{ProviderID: "stf"}}, req); err != nil {
		t.Fatalf("Execute: %v", err)
	} else {
		payloads := trace.Payloads()
		if len(payloads) != 2 {
			t.Fatalf("payloads = %d, want one per emulated call (search, fetch)", len(payloads))
		}

		// Collection order follows the calls: the search came first.
		search, fetch := payloads[0], payloads[1]
		if search.Kind != routing.OpEmulatedTool || search.Tool != "web_search" {
			t.Errorf("payload[0] = %+v, want an emulated_tool payload for web_search", search)
		}
		if !strings.Contains(search.Input, "example article") {
			t.Errorf("search payload input = %q, want the call's own arguments", search.Input)
		}
		if !strings.Contains(search.Output, "Article") {
			t.Errorf("search payload output = %q, want the result the model was handed", search.Output)
		}
		if fetch.Kind != routing.OpEmulatedTool || fetch.Tool != "web_fetch" {
			t.Errorf("payload[1] = %+v, want an emulated_tool payload for web_fetch", fetch)
		}
		if !strings.Contains(fetch.Input, "https://example.com/article") {
			t.Errorf("fetch payload input = %q, want the fetched url", fetch.Input)
		}
		if !strings.Contains(fetch.Output, "body") {
			t.Errorf("fetch payload output = %q, want the fetched document", fetch.Output)
		}

		// The structural half: nothing about these bodies may ride the stored
		// route_trace. The recorded events carry identity and the invocation
		// summary (query → N results), never the result payload itself.
		for _, ev := range trace.Events() {
			if ev.Type != routing.EventEmulatedTool {
				continue
			}
			if strings.Contains(ev.Reason, "Article") || strings.Contains(ev.Reason, "body") {
				t.Errorf("emulated-tool event reason carries payload content: %q", ev.Reason)
			}
		}
	}
}
