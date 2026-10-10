package ir

import "strings"

// Image extraction and substitution (image-to-text fallback): a model that
// cannot take image input can still answer if something describes the image
// first. These two helpers are the pair that makes that possible — find every
// image, then swap each one for its description — and they are deliberately
// written as a matched pair: extraction and replacement walk the tree the same
// way and in the same order, so a caller that produces one description per
// returned ref cannot land a description on the wrong image.
//
// The tree walk descends into ToolResultPart content for the same reason: a
// tool result may carry images, and an image the walk does not see is an image
// the replacement cannot reach.

// ImageRef locates one image part in a request tree, and carries the part so a
// caller never has to look it up a second time.
//
// Path is a part-index path: Path[0] indexes Request.Messages, and the
// remaining indices walk down through nested ToolResultPart content. A
// top-level part therefore has a two-segment path.
type ImageRef struct {
	Path  []int
	Image ImagePart
}

// DecodedBytes is the size of the part's base64 payload once decoded — the fact
// worth keeping about an image that is not going to be stored (a log records an
// image by media type and size, never its bytes). 0 for a URL-sourced part,
// whose payload was never carried.
func (p ImagePart) DecodedBytes() int {
	if p.Data == "" {
		return 0
	}
	// 4 base64 characters carry 3 bytes, minus the padding.
	n := len(p.Data) / 4 * 3
	if pad := strings.Count(p.Data, "="); pad > 0 && pad <= 2 {
		n -= pad
	}
	return n
}

// ImageRefs returns every image part in the request, in document order across
// messages.
func (r Request) ImageRefs() []ImageRef {
	var out []ImageRef
	for i, msg := range r.Messages {
		collectImageRefs(msg.Parts, []int{i}, &out)
	}
	return out
}

func collectImageRefs(parts []Part, prefix []int, out *[]ImageRef) {
	for i, part := range parts {
		path := make([]int, len(prefix)+1)
		copy(path, prefix)
		path[len(prefix)] = i

		switch v := part.(type) {
		case ImagePart:
			*out = append(*out, ImageRef{Path: path, Image: v})
		case ToolResultPart:
			collectImageRefs(v.Content, path, out)
		}
	}
}

// ReplaceImages returns a copy of the request with the image at refs[i]
// replaced by a text part carrying descs[i].
//
// A nil descs[i] — or a descs slice shorter than refs — leaves that image in
// place. That is the failure path, not an oversight: a description that could
// not be produced must leave the original image available to a later,
// vision-capable attempt rather than silently dropping the input or standing
// in a placeholder that says nothing.
//
// The messages are copied before writing. Request is passed by value through
// the executor while Messages and Parts stay shared with the caller, so
// writing in place would rewrite a request the caller still holds.
func (r Request) ReplaceImages(refs []ImageRef, descs []*string) Request {
	if len(refs) == 0 || len(descs) == 0 {
		return r
	}

	out := r
	out.Messages = make([]Message, len(r.Messages))
	for i, msg := range r.Messages {
		out.Messages[i] = Message{Role: msg.Role, Parts: copyParts(msg.Parts)}
	}

	// Walk only the overlap, so both indices are in range by construction.
	// (gosec's range analysis does not follow the min() through the local,
	// hence the directive.)
	limit := min(len(refs), len(descs))
	for i := range limit {
		//nolint:gosec // i < limit <= len(descs), asserted by the min() above
		desc := descs[i]
		if desc == nil || *desc == "" {
			continue
		}
		replacePartAt(out.Messages, refs[i].Path, TextPart{Text: *desc})
	}
	return out
}

// copyParts copies a part slice deeply enough that a replacement cannot reach
// the source. ToolResultPart is the only variant holding a nested slice, and
// the recursion covers an arbitrarily deep tool-result chain.
func copyParts(parts []Part) []Part {
	out := make([]Part, len(parts))
	for i, part := range parts {
		if tr, ok := part.(ToolResultPart); ok {
			tr.Content = copyParts(tr.Content)
			out[i] = tr
			continue
		}
		out[i] = part
	}
	return out
}

// replacePartAt writes text at the location a ref describes. Every index is
// bounds-checked: a path that does not resolve (a ref from a different request,
// a tree the caller mutated in between) is dropped rather than panicking.
func replacePartAt(messages []Message, path []int, text TextPart) {
	if len(path) < 2 {
		return
	}
	mi := path[0]
	if mi < 0 || mi >= len(messages) {
		return
	}

	parts := messages[mi].Parts
	for _, idx := range path[1 : len(path)-1] {
		if idx < 0 || idx >= len(parts) {
			return
		}
		container, ok := parts[idx].(ToolResultPart)
		if !ok {
			return
		}
		parts = container.Content
	}

	last := path[len(path)-1]
	if last < 0 || last >= len(parts) {
		return
	}
	parts[last] = text
}
