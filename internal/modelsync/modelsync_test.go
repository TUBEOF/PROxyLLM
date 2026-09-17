package modelsync

// Contract tests for the ADR-010 model sync, derived from the ADR policy
// (not implementation behavior): upstream-added models land as synced rows,
// operator edits (capabilities/compat) of existing rows survive, upstream-
// dropped synced rows are deleted, and ID collisions with another
// provider's row are skipped — one dead upstream never fails the run.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/tubeof/proxyllm/internal/config"
	"github.com/tubeof/proxyllm/internal/provider"
	"github.com/tubeof/proxyllm/internal/storage"
	"github.com/tubeof/proxyllm/internal/translate/ir"
)

// stubProvider is a registry member with an optional model list.
type stubProvider struct {
	id     string
	list   []provider.ModelInfo
	lstErr error
}

func (s *stubProvider) ID() string                          { return s.id }
func (s *stubProvider) Capabilities() provider.Capabilities { return provider.Capabilities{} }
func (s *stubProvider) Complete(_ context.Context, _ ir.Request) (ir.Response, error) {
	return ir.Response{}, nil
}
func (s *stubProvider) Stream(_ context.Context, _ ir.Request) (<-chan ir.StreamEvent, func(), error) {
	return nil, func() {}, nil
}
func (s *stubProvider) RefreshCredentials(_ context.Context) error { return nil }

// listerModelLister attaches the ModelLister surface.
type listerStub struct {
	*stubProvider
}

func (l *listerStub) ListModels(_ context.Context) ([]provider.ModelInfo, error) {
	return l.list, l.lstErr
}

func (s *stubProvider) asLister() provider.Provider { return &listerStub{stubProvider: s} }

// fakeRegistry is the provider.Registry over stubs.
type fakeRegistry struct{ items []provider.Provider }

func (r *fakeRegistry) Get(id string) (provider.Provider, bool) {
	for _, p := range r.items {
		if p.ID() == id {
			return p, true
		}
	}
	return nil, false
}
func (r *fakeRegistry) List() []provider.Provider { return r.items }
func (r *fakeRegistry) Register(p provider.Provider) {
	for _, existing := range r.items {
		if existing.ID() == p.ID() {
			panic("duplicate: " + p.ID())
		}
	}
	r.items = append(r.items, p)
}

// openStore opens a temp store with seeded provider rows.
func openStore(t *testing.T, providerIDs ...string) *storage.Store {
	t.Helper()
	dir := t.TempDir()
	s, err := storage.Open(config.StorageConfig{Path: dir + "/test.db"})
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	key := make([]byte, 32)
	e, err := storage.NewEnvelope(key)
	if err != nil {
		t.Fatalf("NewEnvelope: %v", err)
	}
	s.SetEnvelope(e)
	for _, id := range providerIDs {
		if err := s.PutProvider(context.Background(), storage.Provider{ID: id, Type: "custom-openai", Name: id}); err != nil {
			t.Fatalf("seed provider %s: %v", id, err)
		}
	}
	return s
}

// testLogger returns a quiet logger for assertions.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// jsonCaps marshals a minimal capabilities blob (operator edit stand-in).
func mustGet(t *testing.T, s *storage.Store, id string) storage.Model {
	t.Helper()
	m, err := s.GetModel(context.Background(), id)
	if err != nil {
		t.Fatalf("GetModel %s: %v", id, err)
	}
	return m
}

func TestSync_ImportsNewModelsAsSynced(t *testing.T) {
	s := openStore(t, "acc-a")
	reg := &fakeRegistry{}
	reg.Register((&stubProvider{id: "acc-a", list: []provider.ModelInfo{
		{Name: "model-x"}, {Name: "model-y"},
	}}).asLister())

	syncer := New(s, reg, testLogger())
	state, err := syncer.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	m := mustGet(t, s, "model-x")
	if m.ProviderID != "acc-a" || m.Origin != storage.ModelSyncedOrigin || m.Name != "model-x" {
		t.Errorf("imported model mismatch: %+v", m)
	}
	if state.Providers[0].Added != 2 {
		t.Errorf("added = %d, want 2", state.Providers[0].Added)
	}
}

func TestSync_PreservesOperatorEdits(t *testing.T) {
	s := openStore(t, "acc-a")
	// Operator-curated row: custom priority + compat.
	if err := s.PutModel(context.Background(), storage.Model{
		ID: "model-x", ProviderID: "acc-a", Name: "model-x",
		Capabilities: []byte(`{"vision":true}`), Compat: []byte(`{"thinking_handling":"block"}`),
		Priority: 9, Origin: storage.ModelSyncedOrigin,
	}); err != nil {
		t.Fatal(err)
	}
	reg := &fakeRegistry{}
	reg.Register((&stubProvider{id: "acc-a", list: []provider.ModelInfo{{Name: "model-x"}}}).asLister())

	if _, err := New(s, reg, testLogger()).Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	m := mustGet(t, s, "model-x")
	if string(m.Capabilities) != `{"vision":true}` || string(m.Compat) != `{"thinking_handling":"block"}` || m.Priority != 9 {
		t.Errorf("operator edits were clobbered: %+v (caps=%s compat=%s)", m, m.Capabilities, m.Compat)
	}
	if state, err := s.GetModelSyncState(context.Background()); err != nil || state.Providers[0].Updated != 0 {
		t.Errorf("no-op refresh counted as update: state=%+v err=%v", state, err)
	}
}

func TestSync_DeletesUpstreamDroppedSynced(t *testing.T) {
	s := openStore(t, "acc-a")
	if err := s.PutModel(context.Background(), storage.Model{
		ID: "gone", ProviderID: "acc-a", Name: "gone", Origin: storage.ModelSyncedOrigin,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutModel(context.Background(), storage.Model{
		ID: "manual-keeps", ProviderID: "acc-a", Name: "manual-keeps",
	}); err != nil {
		t.Fatal(err)
	}
	reg := &fakeRegistry{}
	reg.Register((&stubProvider{id: "acc-a", list: []provider.ModelInfo{}}).asLister())

	if _, err := New(s, reg, testLogger()).Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if _, err := s.GetModel(context.Background(), "gone"); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("dropped synced model should be deleted, got %v", err)
	}
	if _, err := s.GetModel(context.Background(), "manual-keeps"); err != nil {
		t.Errorf("manual model must survive: %v", err)
	}
}

func TestSync_SkipsIDCollisionWithOtherProvider(t *testing.T) {
	s := openStore(t, "acc-a", "acc-b")
	// acc-b owns "shared-name" (operator row); acc-a's upstream also lists it.
	if err := s.PutModel(context.Background(), storage.Model{
		ID: "shared-name", ProviderID: "acc-b", Name: "shared-name",
	}); err != nil {
		t.Fatal(err)
	}
	reg := &fakeRegistry{}
	reg.Register((&stubProvider{id: "acc-a", list: []provider.ModelInfo{{Name: "shared-name"}, {Name: "own-model"}}}).asLister())
	reg.Register((&stubProvider{id: "acc-b", list: nil}).asLister()) // no lister behavior needed

	state, err := New(s, reg, testLogger()).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	m := mustGet(t, s, "shared-name")
	if m.ProviderID != "acc-b" {
		t.Errorf("collision row was overwritten: provider=%s, want acc-b", m.ProviderID)
	}
	if state.Providers[0].Added != 1 {
		t.Errorf("added = %d, want 1 (the colliding import is skipped)", state.Providers[0].Added)
	}
}

func TestSync_UpstreamErrorSkipsProvider(t *testing.T) {
	s := openStore(t, "acc-a", "acc-b")
	reg := &fakeRegistry{}
	reg.Register((&stubProvider{id: "acc-a", lstErr: errors.New("upstream 500")}).asLister())
	reg.Register((&stubProvider{id: "acc-b", list: []provider.ModelInfo{{Name: "model-z"}}}).asLister())

	state, err := New(s, reg, testLogger()).Run(context.Background())
	if err != nil {
		t.Fatalf("one dead provider must not fail the run: %v", err)
	}
	if len(state.Providers) != 2 {
		t.Fatalf("providers in state = %d, want 2", len(state.Providers))
	}
	if state.Providers[0].Skipped != 1 || state.Providers[0].SkippedReason == "" {
		t.Errorf("dead provider not recorded as skipped: %+v", state.Providers[0])
	}
	if _, err := s.GetModel(context.Background(), "model-z"); err != nil {
		t.Errorf("the healthy provider's sync must still run: %v", err)
	}
}

func TestSync_NonListerProvidersIgnored(t *testing.T) {
	s := openStore(t, "acc-a")
	reg := &fakeRegistry{}
	reg.Register(&stubProvider{id: "acc-a"}) // no ModelLister

	state, err := New(s, reg, testLogger()).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(state.Providers) != 0 {
		t.Errorf("non-lister providers must not appear in the state: %+v", state.Providers)
	}
}
