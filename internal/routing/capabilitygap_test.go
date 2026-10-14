package routing_test

// Capability-gap classification contract tests: an upstream 400 whose message
// names an unsupported input modality (image, in this spec) is NOT a generic
// client error — the request is fine, the model just lacks the capability.
// A request failing because the model cannot serve a capability (e.g. image
// input) must fail over to the configured fallback models instead of
// terminating. The signal is necessarily heuristic:
// vendors word these rejections differently, so the matcher consults the
// message text AND the body code, both redacted upstream sources.

import (
	"testing"

	"github.com/tubeof/proxyllm/internal/provider"
	"github.com/tubeof/proxyllm/internal/routing"
)

func TestClassifyError_CapabilityGap(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"image not supported message", &provider.UpstreamError{
			HTTPStatus: 400, Message: "This model does not support image input"}},
		{"images in context message", &provider.UpstreamError{
			HTTPStatus: 400, Message: "Images are not supported by this model"}},
		{"multimodal wording", &provider.UpstreamError{
			HTTPStatus: 400, Message: "The request contains multimodal content which this endpoint cannot process"}},
		{"unsupported content type", &provider.UpstreamError{
			HTTPStatus: 400, Message: "unsupported content type: image/png in message"}},
		{"body code modality", &provider.UpstreamError{
			HTTPStatus: 400, Code: "unsupported_content", Message: "content of this type is not supported"}},
		{"body code image", &provider.UpstreamError{
			HTTPStatus: 400, Code: "image_not_supported", Message: ""}},
	}
	for _, tc := range cases {
		c := routing.ClassifyError(tc.err, routing.PreStream)
		if c.Class != routing.ClassCapabilityGap {
			t.Errorf("%s: class = %q, want capability_gap", tc.name, c.Class)
		}
		if c.Action != routing.ActionFailOver {
			t.Errorf("%s: action = %q, want failover", tc.name, c.Action)
		}
	}
}

func TestClassifyError_Generic400StaysClientError(t *testing.T) {
	// A 400 WITHOUT a capability signal is still the request's fault —
	// terminal, unchanged.
	c := routing.ClassifyError(&provider.UpstreamError{
		HTTPStatus: 400, Message: "max_tokens must be greater than 0"}, routing.PreStream)
	if c.Class != routing.ClassClientError || c.Action != routing.ActionTerminal {
		t.Errorf("generic 400 = %q/%q, want client_error/terminal", c.Class, c.Action)
	}
}

func TestClassifyError_CapabilityGapMidStreamIsTerminal(t *testing.T) {
	// Position outranks everything: mid-stream never retries or swaps.
	c := routing.ClassifyError(&provider.UpstreamError{
		HTTPStatus: 400, Message: "image input not supported"}, routing.MidStream)
	if c.Action != routing.ActionTerminal {
		t.Errorf("mid-stream capability gap action = %q, want terminal", c.Action)
	}
}
