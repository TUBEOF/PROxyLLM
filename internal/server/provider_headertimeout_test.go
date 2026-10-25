package server

// The upstream response-header wall is operator-configurable via the
// `upstream_response_header_timeout_ms` settings KV (ADR-018): an upstream
// that accepts the connection and then says nothing stops holding the
// request when the configured timeout elapses, instead of burning the
// shared five-minute default. Absent or malformed keeps that default — the same
// settings-read contract as every other KV here.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tubeof/proxyllm/internal/storage"
	"github.com/tubeof/proxyllm/internal/translate/ir"
)

func TestServer_ProviderLoadHonorsUpstreamHeaderTimeoutKV(t *testing.T) {
	// A custom-openai upstream that accepts the connection but produces
	// headers slower than the configured wall — the exact shape the
	// configurable timeout exists for.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(1200 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer ts.Close()

	ctx := context.Background()
	cfg := newConfig(t)
	st, err := storage.Open(cfg.Storage)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()

	if err := st.PutProvider(ctx, storage.Provider{
		ID: "custom-hang", Type: "custom-openai", Name: "Custom Hang",
		Config: json.RawMessage(`{"base_url":"` + ts.URL + `"}`),
	}); err != nil {
		t.Fatalf("put provider: %v", err)
	}

	if err := st.SetConfigValue(ctx, "upstream_response_header_timeout_ms", "300"); err != nil {
		t.Fatalf("set KV: %v", err)
	}

	provs, _, err := loadProviders(ctx, st, testLogger(t))
	if err != nil {
		t.Fatalf("loadProviders: %v", err)
	}
	if len(provs) != 1 {
		t.Fatalf("providers = %d, want the one custom account", len(provs))
	}

	start := time.Now()
	_, err = provs[0].Complete(ctx, ir.Request{Model: "m", Messages: []ir.Message{{Role: ir.RoleUser, Parts: []ir.Part{ir.TextPart{Text: "hi"}}}}})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("Complete succeeded, want a header-timeout failure after ~300ms")
	}
	if elapsed >= time.Second {
		t.Errorf("Complete returned after %v, want the ~300ms configured wall, not the 1.2s upstream delay", elapsed)
	}

	// Without a KV the shared five-minute default applies and the same upstream
	// (headers after 1.2s < 30s) answers successfully.
	if err := st.DeleteConfigValue(ctx, "upstream_response_header_timeout_ms"); err != nil {
		t.Fatalf("delete KV: %v", err)
	}

	provs, _, err = loadProviders(ctx, st, testLogger(t))
	if err != nil {
		t.Fatalf("reload providers: %v", err)
	}
	resp, err := provs[0].Complete(ctx, ir.Request{Model: "m", Messages: []ir.Message{{Role: ir.RoleUser, Parts: []ir.Part{ir.TextPart{Text: "hi"}}}}})
	if err != nil {
		t.Fatalf("Complete with the default wall: %v (upstream answers after 1.2s)", err)
	}
	if len(resp.Content) == 0 {
		t.Error("empty response from delayed upstream")
	}
}
