package routing_test

// Context-window classification contract tests (FO-6, 2026-10-09): an
// upstream rejection saying the INPUT is larger than the target model's
// window is not the request's fault — the request is well-formed, the model
// is too small. Spec sources:
//
//   - testdata/golden/openai/context-length-exceeded.json (code
//     context_length_exceeded, accessed 2026-10-09)
//   - testdata/golden/anthropic/context-length-exceeded.json (message only,
//     accessed 2026-10-09)
//   - the vendor wording list in LiteLLM's
//     ExceptionCheckers.is_error_str_context_window_exceeded (read
//     2026-10-09) for the vendors PROxyLLM reaches through OpenAI-compatible
//     endpoints (Gemini, Mistral, OpenRouter, llama.cpp).
//
// It must fail over, not terminate: another chain entry — or a capability
// fallback model with a larger window — may serve the request. This is the
// one error class that reaches the capability-fallback walk with a live
// request, so classifying it terminal is what makes a vision fallback die
// on the first fallback model instead of trying the next one.

import (
	"testing"

	"github.com/tubeof/proxyllm/internal/provider"
	"github.com/tubeof/proxyllm/internal/routing"
)

func TestClassifyError_ContextWindowExceeded(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"openai body code", upstreamErr(400, "context_length_exceeded",
			"This model's maximum context length is 131072 tokens. However, your messages resulted in 400123 tokens. Please reduce the length of the messages.")},
		{"openai message only", upstreamErr(400, "",
			"This model's maximum context length is 8192 tokens.")},
		{"anthropic prompt too long", upstreamErr(400, "",
			"prompt is too long: 400123 tokens > 200000 maximum")},
		{"gemini input token count", upstreamErr(400, "",
			"The input token count (1048576) exceeds the maximum number of tokens allowed (131072).")},
		{"mistral exceeds maximum context length", upstreamErr(400, "",
			"The number of tokens in the prompt exceeds the model's maximum context length of 32768. Please use a shorter prompt.")},
		{"openrouter endpoint maximum", upstreamErr(400, "",
			"This endpoint's maximum context length is 128000 tokens. However, you requested about 400123 tokens")},
		{"llamacpp input length", upstreamErr(400, "",
			"the input length exceeds the context length")},
		{"bedrock input too long", upstreamErr(400, "",
			"Input is too long for requested model")},
		{"azure exceeds maximum length", upstreamErr(400, "",
			"The request exceeds the model's maximum context length.")},
	}
	for _, tc := range cases {
		c := routing.ClassifyError(tc.err, routing.PreStream)
		if c.Class != routing.ClassContextWindow {
			t.Errorf("%s: class = %q, want context_window_exceeded", tc.name, c.Class)
		}
		if c.Action != routing.ActionFailOver {
			t.Errorf("%s: action = %q, want failover", tc.name, c.Action)
		}
	}
}

// TestClassifyError_ContextWindowDoesNotShadowOtherClasses pins the class
// boundaries in both directions: the modality heuristic and the window
// heuristic read the same message text, so neither may swallow the other,
// and an unrelated 400 must stay terminal.
func TestClassifyError_ContextWindowDoesNotShadowOtherClasses(t *testing.T) {
	modality := routing.ClassifyError(upstreamErr(400, "", "this model does not support image input"), routing.PreStream)
	if modality.Class != routing.ClassCapabilityGap {
		t.Errorf("image rejection = %q, want capability_gap", modality.Class)
	}

	unrelated := routing.ClassifyError(upstreamErr(400, "", "max_tokens must be greater than 0"), routing.PreStream)
	if unrelated.Class != routing.ClassClientError || unrelated.Action != routing.ActionTerminal {
		t.Errorf("generic 400 = %q/%q, want client_error/terminal", unrelated.Class, unrelated.Action)
	}
}

// TestClassifyError_ContextWindowMidStreamIsTerminal: FO-1 outranks the
// class — once content reached the client there is no swap, however
// attractive one looks.
func TestClassifyError_ContextWindowMidStreamIsTerminal(t *testing.T) {
	c := routing.ClassifyError(
		&provider.UpstreamError{HTTPStatus: 400, Message: "prompt is too long: 400123 tokens > 200000 maximum"},
		routing.MidStream)
	if c.Action != routing.ActionTerminal {
		t.Errorf("mid-stream context overflow action = %q, want terminal", c.Action)
	}
}
