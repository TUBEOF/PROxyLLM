package ir

import "testing"

func img(data string) ImagePart {
	return ImagePart{Source: "base64", MediaType: "image/png", Data: data}
}

// A tool result can carry image parts (Anthropic allows them), so extraction
// must descend into ToolResultPart content, not only scan the top level.
func TestImageRefs_TopLevelAndNested(t *testing.T) {
	req := Request{Messages: []Message{
		{Role: RoleUser, Parts: []Part{TextPart{Text: "look at this"}, img("top")}},
		{Role: RoleTool, Parts: []Part{ToolResultPart{ToolUseID: "t1", Content: []Part{
			TextPart{Text: "tool output"},
			img("nested"),
		}}}},
	}}

	refs := req.ImageRefs()
	if len(refs) != 2 {
		t.Fatalf("want 2 refs, got %d", len(refs))
	}
	if refs[0].Image.Data != "top" {
		t.Errorf("ref 0 image: want %q, got %q", "top", refs[0].Image.Data)
	}
	if refs[1].Image.Data != "nested" {
		t.Errorf("ref 1 image: want %q, got %q", "nested", refs[1].Image.Data)
	}
	if len(refs[1].Path) != 3 {
		t.Errorf("nested ref path: want 3 segments (message, part, content), got %v", refs[1].Path)
	}
}

// Refs come back in document order — the order a caller's descriptions are
// produced in, and the order replacement must consume them in, or a
// description lands on the wrong image.
func TestImageRefs_DocumentOrder(t *testing.T) {
	req := Request{Messages: []Message{
		{Role: RoleUser, Parts: []Part{img("first"), TextPart{Text: "mid"}, img("second")}},
		{Role: RoleUser, Parts: []Part{img("third")}},
	}}

	refs := req.ImageRefs()
	want := []string{"first", "second", "third"}
	if len(refs) != len(want) {
		t.Fatalf("want %d refs, got %d", len(want), len(refs))
	}
	for i, w := range want {
		if refs[i].Image.Data != w {
			t.Errorf("ref %d: want %q, got %q", i, w, refs[i].Image.Data)
		}
	}
}

func TestImageRefs_NoImages(t *testing.T) {
	req := Request{Messages: []Message{
		{Role: RoleUser, Parts: []Part{TextPart{Text: "hello"}}},
	}}
	if refs := req.ImageRefs(); len(refs) != 0 {
		t.Fatalf("want no refs, got %d", len(refs))
	}
}

func TestReplaceImages_SubstitutesInOrder(t *testing.T) {
	req := Request{Messages: []Message{
		{Role: RoleUser, Parts: []Part{img("a"), img("b")}},
		{Role: RoleTool, Parts: []Part{ToolResultPart{ToolUseID: "t", Content: []Part{img("c")}}}},
	}}
	refs := req.ImageRefs()

	d1, d2, d3 := "described a", "described b", "described c"
	out := req.ReplaceImages(refs, []*string{&d1, &d2, &d3})

	texts := partTexts(out.Messages[0].Parts)
	if len(texts) != 2 || texts[0] != "described a" || texts[1] != "described b" {
		t.Fatalf("top-level substitution wrong: %q", texts)
	}
	tr, ok := out.Messages[1].Parts[0].(ToolResultPart)
	if !ok {
		t.Fatalf("nested tool result did not survive: %T", out.Messages[1].Parts[0])
	}
	if got := partTexts(tr.Content); len(got) != 1 || got[0] != "described c" {
		t.Fatalf("nested substitution wrong: %q", got)
	}
	if _, stillImage := out.Messages[1].Parts[0].(ToolResultPart).Content[0].(ImagePart); stillImage {
		t.Error("nested image survived replacement")
	}
}

// A nil description means the caption call failed for that image. The image
// stays in place so a vision-capable upstream can still serve it, and every
// other image is still substituted.
func TestReplaceImages_NilKeepsOriginal(t *testing.T) {
	req := Request{Messages: []Message{
		{Role: RoleUser, Parts: []Part{img("keep"), img("replace")}},
	}}
	refs := req.ImageRefs()

	desc := "described"
	out := req.ReplaceImages(refs, []*string{nil, &desc})

	if _, ok := out.Messages[0].Parts[0].(ImagePart); !ok {
		t.Errorf("nil description must keep the original image, got %T", out.Messages[0].Parts[0])
	}
	if got := partTexts(out.Messages[0].Parts)[0]; got != "described" {
		t.Errorf("second image: want %q, got %q", "described", got)
	}
}

// The request is passed by value through the executor, and Messages/Parts are
// slices shared with the caller. Substitution must copy before writing, or it
// rewrites the request the caller still holds.
func TestReplaceImages_DoesNotMutateReceiver(t *testing.T) {
	req := Request{Messages: []Message{
		{Role: RoleUser, Parts: []Part{img("original")}},
	}}
	refs := req.ImageRefs()

	desc := "described"
	_ = req.ReplaceImages(refs, []*string{&desc})

	if _, ok := req.Messages[0].Parts[0].(ImagePart); !ok {
		t.Fatalf("receiver was mutated: %T", req.Messages[0].Parts[0])
	}
	if req.Messages[0].Parts[0].(ImagePart).Data != "original" {
		t.Error("receiver's image payload changed")
	}
}

// A short descs slice never drops an image: the missing tail is kept as-is.
func TestReplaceImages_ShortDescsKeepsTail(t *testing.T) {
	req := Request{Messages: []Message{
		{Role: RoleUser, Parts: []Part{img("a"), img("b")}},
	}}
	refs := req.ImageRefs()

	desc := "only a"
	out := req.ReplaceImages(refs, []*string{&desc})

	if got := partTexts(out.Messages[0].Parts); len(got) != 1 || got[0] != "only a" {
		t.Errorf("first image: want the replacement, got %q", got)
	}
	if _, ok := out.Messages[0].Parts[1].(ImagePart); !ok {
		t.Errorf("tail image must be kept, got %T", out.Messages[0].Parts[1])
	}
}

func partTexts(parts []Part) []string {
	var out []string
	for _, p := range parts {
		if t, ok := p.(TextPart); ok {
			out = append(out, t.Text)
		}
	}
	return out
}
