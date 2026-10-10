package openai

import (
	"strings"

	"github.com/tubeof/proxyllm/internal/translate/ir"
)

// imagePartFromURL builds an ImagePart from an OpenAI-style image URL,
// splitting a data: URI into its media type and base64 payload.
//
// The split is not cosmetic. The encoder rebuilds the URI from the part's Data
// and MediaType (imageURL), so a part that keeps the whole URI in URL and
// leaves Data empty is re-emitted as "data:;base64," — the image bytes
// silently dropped. That made every OpenAI-surface image lossy in transit
// (the Anthropic surface carries base64 natively and never hit it).
func imagePartFromURL(url, detail string) ir.ImagePart {
	if mediaType, data, ok := splitDataURI(url); ok {
		return ir.ImagePart{Source: "base64", MediaType: mediaType, Data: data, Detail: detail}
	}
	return ir.ImagePart{Source: "url", URL: url, Detail: detail}
}

// splitDataURI splits a base64 data: URI (RFC 2397) into its media type and
// payload. ok is false for anything else: a remote URL, a malformed value, or
// a data: URI that is not base64-encoded (this path only ever carries image
// payloads, which are base64 by construction).
func splitDataURI(uri string) (mediaType, data string, ok bool) {
	rest, isData := strings.CutPrefix(uri, "data:")
	if !isData {
		return "", "", false
	}
	meta, payload, hasComma := strings.Cut(rest, ",")
	if !hasComma || !strings.HasSuffix(meta, ";base64") {
		return "", "", false
	}
	mediaType = strings.TrimSuffix(meta, ";base64")
	if mediaType == "" {
		// A media type is optional in the URI. Recover it from the payload
		// rather than inventing one.
		mediaType = sniffImageMediaType(payload)
	}
	return mediaType, payload, true
}

// sniffImageMediaType recovers an image's media type from its base64 payload.
// The magics are the base64 encodings of the formats' leading bytes, so they
// are stable prefixes of the encoded text. Returns "" when the type is not
// recognisable: an empty media type is honest, a guessed one is not.
func sniffImageMediaType(b64 string) string {
	switch {
	case strings.HasPrefix(b64, "iVBORw0KGgo"):
		return "image/png"
	case strings.HasPrefix(b64, "/9j/"):
		return "image/jpeg"
	case strings.HasPrefix(b64, "R0lGOD"):
		return "image/gif"
	case strings.HasPrefix(b64, "UklGR"):
		return "image/webp"
	}
	return ""
}
