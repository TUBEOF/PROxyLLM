package routing

// Unit-level contracts of the ADR-018 helpers that the behavior tests drive
// only through the executor.

import (
	"context"
	"strings"
	"testing"

	"github.com/tubeof/proxyllm/internal/translate/ir"
)

// The ref the spec defines: 12 hex chars of the SHA-256 of the DECODED image
// bytes. The decoded form is what makes an inline payload and a resolved
// URL of the same picture share one ref — the round turns on it.
func TestAnalyzeImageRefIsTwelveHexOfTheDecodedBytes(t *testing.T) {
	img := ir.ImagePart{Source: "base64", MediaType: "image/png", Data: "iVBORw0KGgo="}
	ref := analyzeImageRef(img)
	if len(ref) != 12 {
		t.Fatalf("ref = %q, want 12 hex chars", ref)
	}
	if strings.Trim(ref, "0123456789abcdef") != "" {
		t.Fatalf("ref = %q, want lowercase hex", ref)
	}
	// Same bytes, different wire shape (a fetched URL that inlined to the
	// identical payload): same ref, or a conversation would reference one
	// image and lose it on the next turn.
	inlined := ir.ImagePart{Source: "base64", MediaType: "image/png", Data: "iVBORw0KGgo=", Detail: "high"}
	if analyzeImageRef(inlined) != ref {
		t.Errorf("ref depends on the part beyond its bytes")
	}
	// The digest is over the DECODED bytes: a whitespace-wrapped spelling of
	// the same payload must still unify. (A spelling too broken to decode
	// hashes its raw string instead — the documented digest contract: one
	// missed reuse, never a wrong one.)
	alt := ir.ImagePart{Source: "base64", Data: "iVBOR\nw0KGgo="}
	if analyzeImageRef(alt) != ref {
		t.Errorf("ref differs for a wrapped spelling of the same payload")
	}
	other := ir.ImagePart{Data: "aGVsbG8="}
	if analyzeImageRef(other) == ref {
		t.Error("different image bytes share a ref")
	}
}

func TestRenderImagePlaceholder(t *testing.T) {
	cfg := CaptionConfig{}
	got := renderImagePlaceholder(cfg, 3, "abc123def456", "a terminal with a build error")
	want := `[Bild 3 (ref abc123def456): a terminal with a build error — call the analyze_image tool with image_ref "abc123def456" and your own prompt to inspect this image]`
	if got != want {
		t.Errorf("placeholder = %q, want %q", got, want)
	}
	// A failed bootstrap renders the unavailability sentence, never an empty
	// slot: the model must know the slot is empty for a reason.
	failed := renderImagePlaceholder(cfg, 1, "abc123def456", "")
	if strings.Contains(failed, "{caption}") || !strings.Contains(failed, unavailableCaptionText) {
		t.Errorf("failed-caption placeholder = %q", failed)
	}
	// An image whose bytes could not be resolved gets NO tool pointer: an
	// empty ref can never resolve, so the template's chase-the-tool clause
	// would set the model on a doomed retry loop.
	unresolvable := renderImagePlaceholder(cfg, 1, "", "")
	if strings.Contains(unresolvable, "image_ref") || strings.Contains(unresolvable, "{ref}") {
		t.Errorf("uninspectable placeholder keeps the tool pointer: %q", unresolvable)
	}
	if !strings.Contains(unresolvable, "cannot be inspected") {
		t.Errorf("uninspectable placeholder = %q", unresolvable)
	}
	// The operator's template override rides verbatim (KV vision_placeholder_template).
	over := CaptionConfig{VisionPlaceholderTemplate: "Picture #{n} ref={ref} caption: {caption}"}
	if got := renderImagePlaceholder(over, 2, "zz", "cap"); got != "Picture #2 ref=zz caption: cap" {
		t.Errorf("overridden template = %q", got)
	}
}

func TestVisionToolStateResolve(t *testing.T) {
	s := &visionToolState{images: map[string]ir.ImagePart{"aa": {Data: "one"}, "bb": {Data: "two"}},
		ordinals: map[string]string{"1": "aa", "2": "bb"}}
	img, ok := s.resolve("aa")
	if !ok || img.Data != "one" {
		t.Errorf("ref resolve = %+v,%v", img, ok)
	}
	if img, ok = s.resolve("2"); !ok || img.Data != "two" {
		t.Errorf("ordinal resolve = %+v,%v", img, ok)
	}
	if img, ok = s.resolve(" Bild 1 "); !ok || img.Data != "one" {
		t.Errorf("Bild-ordinal resolve = %+v,%v", img, ok)
	}
	if _, ok = s.resolve("cc"); ok {
		t.Error("unknown ref resolved")
	}
	if _, ok = s.resolve("Bild 9"); ok {
		t.Error("out-of-range ordinal resolved")
	}
	var nilState *visionToolState
	if _, ok = nilState.resolve("aa"); ok {
		t.Error("nil state resolved")
	}
}

func TestAnswerAnalyzeImageMaxUsesGuard(t *testing.T) {
	e := NewExecutor(ExecutorConfig{Health: NewHealthTracker()})
	state := &visionToolState{images: map[string]ir.ImagePart{"aa": {Data: "one"}}}
	call := ir.ToolCallPart{ID: "c1", Name: analyzeImageToolName,
		Input: []byte(`{"image_ref":"aa","prompt":"q"}`)}
	uses := map[string]int{analyzeImageToolName: analyzeImageMaxUses}

	trace := &Trace{}
	tr, _ := e.answerAnalyzeImage(context.Background(), call,
		ir.ServerToolConfig{MaxUses: analyzeImageMaxUses}, state, uses, trace)
	if !tr.IsError {
		t.Fatal("an over-budget call answered instead of erroring")
	}
	text := tr.Content[0].(ir.TextPart).Text
	if !strings.Contains(text, "max_uses_exceeded") {
		t.Errorf("error result = %q, want the max_uses_exceeded code", text)
	}
	// No state: the tool cannot resolve anything, so the call errors
	// in-band (never a nil-pointer panic on the request path).
	tr2, _ := e.answerAnalyzeImage(context.Background(), call,
		ir.ServerToolConfig{}, nil, map[string]int{}, trace)
	if !tr2.IsError || !strings.Contains(tr2.Content[0].(ir.TextPart).Text, "unavailable") {
		t.Errorf("stateless call result = %+v", tr2)
	}
}
