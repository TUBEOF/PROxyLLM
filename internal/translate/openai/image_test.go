package openai

import (
	"testing"

	"github.com/tubeof/proxyllm/internal/translate/ir"
)

// A data: URI carries the base64 bytes inline. The decoder must split it into
// payload + media type, because the encoder rebuilds the URI from ImagePart's
// Data and MediaType fields. A part that keeps the whole URI in URL and leaves
// Data empty round-trips to "data:;base64," — the image bytes are gone.
func TestPartsFromContent_DataURIImageKeepsPayload(t *testing.T) {
	const uri = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUg=="

	parts := partsFromContent(wireContent{Parts: []wireContentPart{
		{Type: "image_url", ImageURL: &wireImageURL{URL: uri, Detail: "high"}},
	}})
	if len(parts) != 1 {
		t.Fatalf("want 1 part, got %d", len(parts))
	}
	img, ok := parts[0].(ir.ImagePart)
	if !ok {
		t.Fatalf("want an ImagePart, got %T", parts[0])
	}
	if img.Source != "base64" {
		t.Errorf("Source: want base64, got %q", img.Source)
	}
	if img.Data != "iVBORw0KGgoAAAANSUhEUg==" {
		t.Errorf("Data: payload not split out of the URI, got %q", img.Data)
	}
	if img.MediaType != "image/png" {
		t.Errorf("MediaType: want image/png, got %q", img.MediaType)
	}
	if img.Detail != "high" {
		t.Errorf("Detail: want high, got %q", img.Detail)
	}
	if got := imageURL(img); got != uri {
		t.Errorf("round trip: want %q, got %q", uri, got)
	}
}

// A remote URL is not a data: URI and must stay one.
func TestPartsFromContent_RemoteURLStaysURL(t *testing.T) {
	const uri = "https://example.com/shot.png"

	parts := partsFromContent(wireContent{Parts: []wireContentPart{
		{Type: "image_url", ImageURL: &wireImageURL{URL: uri}},
	}})
	img, ok := parts[0].(ir.ImagePart)
	if !ok {
		t.Fatalf("want an ImagePart, got %T", parts[0])
	}
	if img.Source != "url" || img.URL != uri || img.Data != "" {
		t.Errorf("remote URL mangled: %+v", img)
	}
}

// A data: URI may omit the media type. The payload still has to survive, and
// the type is recoverable from the payload's magic bytes.
func TestPartsFromContent_DataURIWithoutMediaTypeSniffs(t *testing.T) {
	parts := partsFromContent(wireContent{Parts: []wireContentPart{
		{Type: "image_url", ImageURL: &wireImageURL{URL: "data:;base64,iVBORw0KGgoAAAANSUhEUg=="}},
	}})
	img, ok := parts[0].(ir.ImagePart)
	if !ok {
		t.Fatalf("want an ImagePart, got %T", parts[0])
	}
	if img.Data != "iVBORw0KGgoAAAANSUhEUg==" {
		t.Errorf("Data: payload lost, got %q", img.Data)
	}
	if img.MediaType != "image/png" {
		t.Errorf("MediaType: want the sniffed image/png, got %q", img.MediaType)
	}
}

// The Responses surface decodes images through its own content type and had
// the same defect.
func TestResponsesPartsFromContent_DataURIImageKeepsPayload(t *testing.T) {
	const uri = "data:image/jpeg;base64,/9j/4AAQSkZJRg=="

	parts := responsesPartsFromContent(&responsesWireContent{Parts: []responsesWirePart{
		{Type: "input_image", ImageURL: uri, Detail: "auto"},
	}})
	if len(parts) != 1 {
		t.Fatalf("want 1 part, got %d", len(parts))
	}
	img, ok := parts[0].(ir.ImagePart)
	if !ok {
		t.Fatalf("want an ImagePart, got %T", parts[0])
	}
	if img.Source != "base64" || img.MediaType != "image/jpeg" || img.Data != "/9j/4AAQSkZJRg==" {
		t.Errorf("payload not split out of the URI: %+v", img)
	}
	if got := imageURL(img); got != uri {
		t.Errorf("round trip: want %q, got %q", uri, got)
	}
}
