package httpapi

// R5 (2026-10-06): the compat editor is REMOVED from the WebUI — the
// policy defaults are internal and CAPABILITY-AWARE. compatFor derives:
//   - thinking → map-to-reasoning-effort when the model row's blob says
//     thinking:true, else the strip default;
//   - server-tool emulation → emulate-firecrawl when a web-tool account
//     exists, else native;
//   - cache_control → "" (the drop default; forward was and stays an
//     Anthropic→Anthropic-only concern).
// A stored compat blob still wins per field (legacy rows read-only).

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/tubeof/proxyllm/internal/config"
	"github.com/tubeof/proxyllm/internal/routing"
	"github.com/tubeof/proxyllm/internal/storage"
)

func TestCompatFor_ThinkingCapabilitydrivesDefault(t *testing.T) {
	caps := func(t *testing.T, flags map[string]any) json.RawMessage {
		raw, err := json.Marshal(flags)
		if err != nil {
			t.Fatalf("marshal caps: %v", err)
		}
		return json.RawMessage(raw)
	}

	thinkingCaps := caps(t, map[string]any{
		"streaming": true, "tools": true, "thinking": true, "vision": false,
	})
	nonThinkingCaps := caps(t, map[string]any{
		"streaming": true, "tools": true, "thinking": false,
	})
	// No capabilities blob at all (a manually-created row).
	noneCaps := json.RawMessage("{}")

	if got := defaultThinkingFor(thinkingCaps, ""); got != routing.ThinkingMapToEffort {
		t.Fatalf("thinking-capable default = %q, want %q", got, routing.ThinkingMapToEffort)
	}
	if got := defaultThinkingFor(nonThinkingCaps, ""); got != "" {
		t.Fatalf("non-thinking default = %q, want the strip zero value", got)
	}
	if got := defaultThinkingFor(noneCaps, ""); got != "" {
		t.Fatalf("unknown-caps default = %q, want the strip zero value", got)
	}
	// Stored values win.
	if got := defaultThinkingFor(thinkingCaps, string(routing.ThinkingStrip)); got != string(routing.ThinkingStrip) {
		t.Fatalf("stored override lost: %q", got)
	}
}

// capabilityFlags must tolerate the `pinned` OBJECT that ADR-014's learning
// writes beside the boolean flags. Decoding straight into map[string]bool
// failed on every row that had ever learned anything, which read as "no
// capabilities" — so exactly the rows in active use silently lost their
// thinking mapping. Only a real boolean counts as a flag.
func TestCapabilityFlags_ToleratesThePinnedObject(t *testing.T) {
	learned := json.RawMessage(`{"streaming":true,"tools":true,"thinking":true,"vision":false,"pinned":{"vision":true}}`)
	got := capabilityFlags(learned)
	want := map[string]bool{"streaming": true, "tools": true, "thinking": true}
	seen := map[string]bool{}
	for _, f := range got {
		seen[f] = true
	}
	for name := range want {
		if !seen[name] {
			t.Errorf("capabilityFlags dropped %q from %s", name, learned)
		}
	}
	for name := range seen {
		if _, ok := want[name]; !ok {
			t.Errorf("capabilityFlags invented %q from %s (a false flag or the pinned map)", name, learned)
		}
	}
	// And the mapping it feeds still fires for such a row.
	if got := defaultThinkingFor(learned, ""); got != routing.ThinkingMapToEffort {
		t.Errorf("thinking default for a learned row = %q, want %q", got, routing.ThinkingMapToEffort)
	}
}

func TestCompatFor_EmulationDefaultFollowsAccounts(t *testing.T) {
	if got := emulationFor(true, ""); got != routing.EmulationEmulateFirecrawl {
		t.Fatalf("with web-tool accounts = %q, want %q", got, routing.EmulationEmulateFirecrawl)
	}
	if got := emulationFor(false, ""); got != routing.EmulationNative {
		t.Fatalf("without = %q, want %q", got, routing.EmulationNative)
	}
	// Stored values win (legacy rows).
	if got := emulationFor(true, string(routing.EmulationOff)); got != string(routing.EmulationOff) {
		t.Fatalf("stored override lost: %q", got)
	}
}

func TestCompatFor_CacheControlDefault(t *testing.T) {
	if got := defaultCacheControl(""); got != "" {
		t.Fatalf("default = %q, want the empty drop default", got)
	}
	if got := defaultCacheControl(string(routing.CacheControlForward)); got != string(routing.CacheControlForward) {
		t.Fatalf("stored forward lost: %q", got)
	}
}

// The full path: compatFor on a seeded thinking-capable model row with NO
// compat blob yields map-to-reasoning-effort.
func TestCompatFor_RowWithoutBlob(t *testing.T) {
	st, err := storage.Open(config.StorageConfig{Path: t.TempDir() + "/test.db"})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = st.Close() }()

	if err := st.EnsureProvider(context.Background(), "prov", "ollama-cloud", "P"); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if err := st.PutModel(context.Background(), storage.Model{
		ID:           "ollamacloud/m",
		ProviderID:   "prov",
		Name:         "m",
		Capabilities: json.RawMessage(`{"streaming":true,"tools":true,"thinking":true}`),
	}); err != nil {
		t.Fatalf("put model: %v", err)
	}

	a := &api{store: st}
	c := a.compatFor(context.Background(), "ollamacloud/m")
	if c.ThinkingHandling != routing.ThinkingMapToEffort {
		t.Fatalf("thinking handling = %q, want %q (capability-deriven)", c.ThinkingHandling, routing.ThinkingMapToEffort)
	}
	if c.CacheControl != "" {
		t.Fatalf("cache control = %q, want the drop default", c.CacheControl)
	}
}
