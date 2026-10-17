package httpapi

// The compat editor is REMOVED from the WebUI — the
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

// Emulation is the default for EVERY target: the web-tool families are served
// by the proxy's web-tool accounts, never by a chat model's upstream. Letting a
// dated server-tool definition ride native makes the request unroutable (no
// account declares native web_search/web_fetch, so the router's capability
// filter empties the chain) or hands the client a tool call it cannot answer —
// so a stored legacy `native` reads as emulate, and only `off` stays an
// operator pin.
func TestEmulationDefaultIsEmulateForEveryTarget(t *testing.T) {
	for _, stored := range []string{"", string(routing.EmulationNative)} {
		if got := emulationFor(stored); got != routing.EmulationEmulateFirecrawl {
			t.Errorf("emulationFor(%q) = %q, want %q", stored, got, routing.EmulationEmulateFirecrawl)
		}
	}
	if got := emulationFor(string(routing.EmulationOff)); got != string(routing.EmulationOff) {
		t.Fatalf("the explicit off pin was lost: %q", got)
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

// seedCompatModel stores a provider row plus one model row on it, returning a
// store the compat lookup can read. Provider rows are the FK target; the
// compat blob is what the policy resolution consumes.
func seedCompatModel(t *testing.T, providerID, modelID string, caps, compat json.RawMessage) *storage.Store {
	t.Helper()
	st, err := storage.Open(config.StorageConfig{Path: t.TempDir() + "/test.db"})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	if err := st.EnsureProvider(ctx, providerID, "ollama-cloud", providerID); err != nil {
		t.Fatalf("ensure provider: %v", err)
	}
	if err := st.PutModel(ctx, storage.Model{
		ID:           modelID,
		ProviderID:   providerID,
		Name:         modelID,
		Capabilities: caps,
		Compat:       compat,
	}); err != nil {
		t.Fatalf("put model: %v", err)
	}
	return st
}

// The policy belongs to the model the request LANDS on. The alias table is
// followed one hop — the same resolution the router routes on — so a
// client-typed Claude Code ID (which has no row of its own) picks up its
// target row's policy instead of silently dropping to the zero Compat. That
// drop is what made every web-tool request under an alias unroutable: the
// server-tool definitions rode native, the capability filter emptied the
// chain, and no emulation or fallback ever ran.
func TestCompatFor_AliasTargetRowDrivesPolicy(t *testing.T) {
	caps := json.RawMessage(`{"streaming":true,"tools":true,"thinking":true}`)
	st := seedCompatModel(t, "acc-ollama", "ollama-cloud/m", caps, nil)
	router := routing.NewRouter(routing.Config{Aliases: map[string]string{
		"claude-haiku-5-5": "ollama-cloud/m",
		"claude-sonnet-*":  "ollama-cloud/m",
	}})
	a := &api{store: st, router: router}

	for _, model := range []string{
		"claude-haiku-5-5",     // exact alias entry
		"claude-sonnet-4-6",    // wildcard alias entry
		"claude-haiku-5-5[1M]", // marker-stripped form of the exact entry
	} {
		c := a.compatFor(context.Background(), model)
		if c.ThinkingHandling != routing.ThinkingMapToEffort {
			t.Errorf("compatFor(%q).ThinkingHandling = %q, want %q (the landed row is thinking-capable)",
				model, c.ThinkingHandling, routing.ThinkingMapToEffort)
		}
		if c.ServerToolEmulation != routing.EmulationEmulateFirecrawl {
			t.Errorf("compatFor(%q).ServerToolEmulation = %q, want %q",
				model, c.ServerToolEmulation, routing.EmulationEmulateFirecrawl)
		}
	}

	// A stored blob on the LANDED row still wins (legacy overrides).
	st2 := seedCompatModel(t, "acc-ollama", "ollama-cloud/m",
		json.RawMessage(`{"streaming":true,"tools":true,"thinking":true}`),
		json.RawMessage(`{"server_tool_emulation":"off","thinking_handling":"block"}`))
	a2 := &api{store: st2, router: router}
	c := a2.compatFor(context.Background(), "claude-haiku-5-5")
	if c.ServerToolEmulation != string(routing.EmulationOff) || c.ThinkingHandling != routing.ThinkingBlock {
		t.Fatalf("stored pins on the landed row were lost: %+v", c)
	}
}

// One hop, exactly like the router: an alias target is never resolved again,
// so a map a→b→c reads b's row and never c's — reading through would apply a
// policy the request will not route under.
func TestCompatFor_AliasResolvesOneHopOnly(t *testing.T) {
	st := seedCompatModel(t, "acc-ollama", "b",
		json.RawMessage(`{"streaming":true,"tools":true}`),
		json.RawMessage(`{"server_tool_emulation":"off"}`))
	// c would answer thinking-capable + emulate; it must not be consulted.
	ctx := context.Background()
	if err := st.EnsureProvider(ctx, "acc-ollama", "ollama-cloud", "acc-ollama"); err != nil {
		t.Fatalf("ensure provider: %v", err)
	}
	if err := st.PutModel(ctx, storage.Model{
		ID: "c", ProviderID: "acc-ollama", Name: "c",
		Capabilities: json.RawMessage(`{"thinking":true}`),
	}); err != nil {
		t.Fatalf("put model c: %v", err)
	}
	router := routing.NewRouter(routing.Config{Aliases: map[string]string{"a": "b", "b": "c"}})
	a := &api{store: st, router: router}
	c := a.compatFor(context.Background(), "a")
	if c.ServerToolEmulation != string(routing.EmulationOff) {
		t.Fatalf("ServerToolEmulation = %q, want the off pin from b's row (one hop only)", c.ServerToolEmulation)
	}
	if c.ThinkingHandling != "" {
		t.Fatalf("ThinkingHandling = %q, want the strip default — c's row must never be read", c.ThinkingHandling)
	}
}

// A landed model without a row (every alias target the map names that the sync
// has not imported, and any manual name) takes the DERIVED defaults: an unknown
// model is not an unconfigured one. A nil router is the fixture/test shape and
// must not panic.
func TestCompatFor_MissingRowTakesDerivedDefaults(t *testing.T) {
	st, err := storage.Open(config.StorageConfig{Path: t.TempDir() + "/test.db"})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = st.Close() }()

	router := routing.NewRouter(routing.Config{Aliases: map[string]string{"claude-*": "nowhere/at-all"}})
	a := &api{store: st, router: router}
	c := a.compatFor(context.Background(), "claude-haiku-5-5")
	if c.ServerToolEmulation != routing.EmulationEmulateFirecrawl {
		t.Fatalf("row-less landed model: ServerToolEmulation = %q, want %q",
			c.ServerToolEmulation, routing.EmulationEmulateFirecrawl)
	}
	if c.ThinkingHandling != "" || c.CacheControl != "" {
		t.Fatalf("row-less landed model: unexpected policy %+v", c)
	}

	// Nil router (no alias table wired): no panic, same derived defaults.
	c = (&api{store: st}).compatFor(context.Background(), "claude-haiku-5-5")
	if c.ServerToolEmulation != routing.EmulationEmulateFirecrawl {
		t.Fatalf("nil router: ServerToolEmulation = %q, want %q",
			c.ServerToolEmulation, routing.EmulationEmulateFirecrawl)
	}
}
