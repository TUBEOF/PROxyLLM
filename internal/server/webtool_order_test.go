package server

// TestServer_WebToolFallbackListOrdersPool: the stored web_fetch/web_search
// fallback KVs name WEB-TOOL ACCOUNT IDs; the pool walks them in the STORED
// order (ADR-010 decision 3 — previously validated-but-ignored: the pool
// was assembled in registry insertion order and the lists were never read).
// Seeded: fetch list = [fc-b, fc-a] → account fc-b answers first even
// though fc-a is first in the registry.
//
// ID attribution: the pool carries plain Emulator values, so the test maps
// pool entries back to the account emulators by identity (fc-a/fc-b are
// distinct *firecrawl.Emulator values from loadProviders).

import (
	"context"
	"log/slog"
	"testing"

	"github.com/tubeof/proxyllm/internal/routing"
	"github.com/tubeof/proxyllm/internal/storage"
)

func TestServer_WebToolFallbackListOrdersPool(t *testing.T) {
	ctx := context.Background()
	cfg := newConfig(t)

	st, err := storage.Open(cfg.Storage)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()

	// Two web_tool accounts; fc-a first in insertion order.
	if err := st.EnsureProvider(ctx, "fc-a", "firecrawl", "FC A"); err != nil {
		t.Fatalf("ensure fc-a: %v", err)
	}
	if err := st.EnsureProvider(ctx, "fc-b", "firecrawl", "FC B"); err != nil {
		t.Fatalf("ensure fc-b: %v", err)
	}

	// Store the fetch list: fc-b BEFORE fc-a (the operator's order).
	if err := st.PutWebToolFallbacks(ctx, storage.WebToolFallbacks{
		Fetch: []string{"fc-b", "fc-a"},
	}); err != nil {
		t.Fatalf("put webtool fallbacks: %v", err)
	}

	named := loadEmusForTest(ctx, t, st)
	// Reverse the account→emulator map for identity attribution.
	byEmu := map[routing.Emulator]string{}
	for _, n := range named {
		byEmu[n.Emulator] = n.ID
	}

	// The pool is assembled from the stored lists — the same call path
	// New and the admin reload hooks use.
	pool := buildEmulatorPool(ctx, named, nil, st, testLogger(t))
	rp, ok := pool.(*routing.EmulatorPool)
	if !ok {
		t.Fatalf("pool type = %T, want *routing.EmulatorPool", pool)
	}
	var ids []string
	for _, e := range rp.FetchOrder() {
		ids = append(ids, byEmu[e])
	}
	if len(ids) != 2 || ids[0] != "fc-b" || ids[1] != "fc-a" {
		t.Fatalf("fetch pool order = %v, want [fc-b fc-a] (stored list order)", ids)
	}
}

// loadEmusForTest runs loadProviders and returns the NamedEmulator slice.
func loadEmusForTest(ctx context.Context, t *testing.T, st *storage.Store) []NamedEmulator {
	t.Helper()
	_, emus, err := loadProviders(ctx, st, testLogger(t))
	if err != nil {
		t.Fatalf("load providers: %v", err)
	}
	return emus
}

// testLogger silences the wiring test's warn lines.
func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.DiscardHandler)
}
