package server

// The account pool switch: a disabled account is not constructed into the
// live registry at all — out of routing, out of the type's chain expansion,
// out of the health picture, out of the model sync and out of the web-tool
// emulator pool — while its rows stay in the store, so switching it back on
// restores it whole.

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/tubeof/proxyllm/internal/storage"
)

func TestServer_DisabledAccountLeavesTheRegistry(t *testing.T) {
	ctx := context.Background()
	cfg := newConfig(t)

	st, err := storage.Open(cfg.Storage)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()

	// One chat account per kind of construction: an LLM provider and a
	// web-tool emulator.
	if err := st.PutProvider(ctx, storage.Provider{
		ID: "llm-a", Type: "custom-openai", Name: "LLM A",
		Config: json.RawMessage(`{"base_url":"https://api.example.com/v1"}`),
	}); err != nil {
		t.Fatalf("put llm-a: %v", err)
	}
	if err := st.PutProvider(ctx, storage.Provider{
		ID: "llm-b", Type: "custom-openai", Name: "LLM B",
		Config: json.RawMessage(`{"base_url":"https://api.example.com/v1"}`),
	}); err != nil {
		t.Fatalf("put llm-b: %v", err)
	}
	if err := st.PutProvider(ctx, storage.Provider{ID: "fc-a", Type: "firecrawl", Name: "FC A"}); err != nil {
		t.Fatalf("put fc-a: %v", err)
	}

	ids := func() ([]string, []string) {
		t.Helper()
		provs, emus, err := loadProviders(ctx, st, testLogger(t))
		if err != nil {
			t.Fatalf("loadProviders: %v", err)
		}
		var pids []string
		for _, p := range provs {
			pids = append(pids, p.ID())
		}
		var eids []string
		for _, e := range emus {
			eids = append(eids, e.ID)
		}
		return pids, eids
	}

	pids, eids := ids()
	if len(pids) != 2 || len(eids) != 1 {
		t.Fatalf("baseline = %v / %v, want both chat accounts and the emulator", pids, eids)
	}

	// Switching the chat account off: it is gone from the constructed set...
	if err := st.SetProviderEnabled(ctx, "llm-a", false); err != nil {
		t.Fatalf("disable llm-a: %v", err)
	}
	// ...and the web-tool account with it (the switch is not kind-specific:
	// "not in the pool" means nowhere).
	if err := st.SetProviderEnabled(ctx, "fc-a", false); err != nil {
		t.Fatalf("disable fc-a: %v", err)
	}
	pids, eids = ids()
	if len(pids) != 1 || pids[0] != "llm-b" {
		t.Errorf("providers after disable = %v, want only llm-b", pids)
	}
	if len(eids) != 0 {
		t.Errorf("emulators after disable = %v, want none", eids)
	}

	// The rows survive — tokens, model rows and the account itself — so
	// switching it back on restores it without re-entering anything.
	if _, err := st.GetProvider(ctx, "llm-a"); err != nil {
		t.Errorf("the disabled account's row is gone: %v", err)
	}
	if err := st.SetProviderEnabled(ctx, "llm-a", true); err != nil {
		t.Fatalf("re-enable llm-a: %v", err)
	}
	pids, _ = ids()
	if len(pids) != 2 {
		t.Errorf("providers after re-enable = %v, want both chat accounts", pids)
	}
}
