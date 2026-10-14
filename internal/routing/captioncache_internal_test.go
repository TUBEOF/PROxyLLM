package routing

// The caption cache's own contract: keyed by image content, bounded, and
// never a source of wrong reuse. The behaviour it produces at the executor
// level is pinned in captioncache_test.go (package routing_test).

import (
	"testing"

	"github.com/tubeof/proxyllm/internal/translate/ir"
)

func TestCaptionCache_EvictsLeastRecentlyUsedAtTheBound(t *testing.T) {
	c := newCaptionCache(2)
	a := captionImageKey("", "v", ir.ImagePart{Data: "aaa="})
	b := captionImageKey("", "v", ir.ImagePart{Data: "bbb="})
	d := captionImageKey("", "v", ir.ImagePart{Data: "ccc="})

	c.put(a, &captionEntry{key: a, text: "A"})
	c.put(b, &captionEntry{key: b, text: "B"})
	// Touching A makes B the least recently used.
	if _, ok := c.get(a); !ok {
		t.Fatal("A missing right after it was stored")
	}
	c.put(d, &captionEntry{key: d, text: "D"})

	if _, ok := c.get(b); ok {
		t.Error("B survived: the bound must evict the least recently used entry")
	}
	if _, ok := c.get(a); !ok {
		t.Error("A was evicted although it had just been used")
	}
	if _, ok := c.get(d); !ok {
		t.Error("D (the newest entry) is missing")
	}
}

// A payload that is not canonical base64 cannot be decoded, so the key falls
// back to the text. That must still be stable across calls — a key that
// changed would re-describe the same image forever, which is the bug this
// cache exists to remove.
func TestCaptionCache_NonCanonicalBase64IsStillKeyedAndHit(t *testing.T) {
	c := newCaptionCache(4)
	// The OpenAI codec keeps a data URI's payload verbatim, invalid base64
	// included; this is the shape that used to arrive.
	img := ir.ImagePart{Source: "base64", MediaType: "image/png", Data: "x"}
	key := captionImageKey("", "v", img)

	c.put(key, &captionEntry{key: key, text: "described"})
	if again := captionImageKey("", "v", img); again != key {
		t.Error("the same non-canonical payload produced two different keys")
	}
	entry, ok := c.get(key)
	if !ok || entry.text != "described" {
		t.Errorf("lookup = %+v/%v, want the stored entry", entry, ok)
	}
}

// The key names content, so two encodings of the same bytes agree — which is
// what unifies an image fetched from a URL with the same image sent inline.
func TestCaptionCache_KeyIsContentAddressedAcrossEncodings(t *testing.T) {
	// "hello" as canonical base64, and the same bytes with different padding
	// conventions, decode to the same digest.
	plain := ir.ImagePart{Data: "aGVsbG8="}
	if captionImageKey("k", "v", plain) != captionImageKey("k", "v", plain) {
		t.Error("the same payload produced two different keys")
	}
	other := ir.ImagePart{Data: "aGVsbG9z"}
	if captionImageKey("k", "v", plain) == captionImageKey("k", "v", other) {
		t.Error("different image bytes collided")
	}
}

// Neither the client key nor the describer configuration is decoration: both
// partition the cache, so one cannot serve the other's text.
func TestCaptionCache_KeySeparatesClientAndConfiguration(t *testing.T) {
	img := ir.ImagePart{Data: "aGVsbG8="}
	if captionImageKey("k1", "v", img) == captionImageKey("k2", "v", img) {
		t.Error("two client keys share one cache slot")
	}
	if captionImageKey("k1", "v", img) == captionImageKey("k1", "v2", img) {
		t.Error("a changed describer configuration reuses the old entry")
	}
}

func TestCaptionVersion_CoversPromptAndDescriberList(t *testing.T) {
	models := []ChainEntry{{ProviderID: "vision", Model: "vl-1"}}
	base := captionVersion("describe this", models)

	if base == captionVersion("describe this in detail", models) {
		t.Error("a changed base prompt did not change the version")
	}
	if base == captionVersion("describe this", []ChainEntry{{ProviderID: "vision", Model: "vl-2"}}) {
		t.Error("a changed describer list did not change the version")
	}
	if base == captionVersion("describe this", []ChainEntry{{ProviderID: "other", Model: "vl-1"}}) {
		t.Error("a changed describer provider did not change the version")
	}
	if base != captionVersion("describe this", models) {
		t.Error("the version is not stable for the same configuration")
	}
}
