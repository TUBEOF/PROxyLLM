package modelsync

// Contract tests for the ADR-010 model sync, derived from the ADR policy
// (not implementation behavior): upstream-added models land as synced rows,
// operator edits (capabilities/compat) of existing rows survive, upstream-
// dropped synced rows are deleted, and ID collisions with another
// provider's row are skipped — one dead upstream never fails the run.
//
// Identity spec (2026-09-21): row IDs are prefixed with the provider type
// ("custom-openai/model-x"); the Name column carries the bare wire name.
// The tests below assert the prefixed IDs accordingly.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"reflect"
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
func (s *stubProvider) Type() string                        { return "custom-openai" }
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

	m := mustGet(t, s, "custom-openai/model-x")
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
		ID: "custom-openai/model-x", ProviderID: "acc-a", Name: "model-x",
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

	m := mustGet(t, s, "custom-openai/model-x")
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
		ID: "custom-openai/shared-name", ProviderID: "acc-b", Name: "shared-name",
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

	m := mustGet(t, s, "custom-openai/shared-name")
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
	if _, err := s.GetModel(context.Background(), "custom-openai/model-z"); err != nil {
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

// ---- 2026-10-06 context/capability auto-detection persistence ----
// (plan task 3) Detected facts (provider.ModelInfo from ollamacloud
// /api/show, verified live 2026-10-06: glm-5.3-flash → context_length
// 1048576, capabilities thinking/tools/vision) persist into model rows.
// Operator pins win; manual rows are never touched.

// detectGLM is the detected-info fixture pinning the live /api/show facts.
func detectGLM() provider.ModelInfo {
	return provider.ModelInfo{
		Name:          "glm-5.3-flash",
		ContextLength: 1048576,
		Thinking:      true,
		Tools:         true,
		Vision:        true,
	}
}

// assertBlob compares a JSON blob against a map via deep equality; the
// want map round-trips through JSON so numbers compare as the decoder
// sees them (float64), not as literal int values.
func assertBlob(t *testing.T, label, raw string, want map[string]any) {
	t.Helper()
	var got map[string]any
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("%s blob is not JSON: %v (%s)", label, err, raw)
	}
	wantJSON, _ := json.Marshal(want)
	var wantNorm map[string]any
	if err := json.Unmarshal(wantJSON, &wantNorm); err != nil { // cannot fail: marshaled JSON
		t.Fatalf("normalize want: %v", err)
	}
	if !reflect.DeepEqual(got, wantNorm) {
		t.Errorf("%s blob = %s, want %v", label, raw, want)
	}
}

// New models import with the detected capabilities and context window
// (plan contract: capabilities {"streaming":true,"tools":det,"vision":det,
// "thinking":det} + server tools false; compat {"thinking_handling":"strip",
// "context_window":N}).
func TestSync_NewRowCarriesDetectedFacts(t *testing.T) {
	s := openStore(t, "acc-a")
	reg := &fakeRegistry{}
	reg.Register((&stubProvider{id: "acc-a", list: []provider.ModelInfo{detectGLM()}}).asLister())

	if _, err := New(s, reg, testLogger()).Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	m := mustGet(t, s, "custom-openai/glm-5.3-flash")
	assertBlob(t, "capabilities", string(m.Capabilities), map[string]any{
		"streaming": true, "tools": true, "vision": true, "thinking": true,
		"web_search": false, "web_fetch": false, "code_execution": false,
	})
	assertBlob(t, "compat", string(m.Compat), map[string]any{
		"thinking_handling": "strip", "context_window": 1048576,
	})
}

// Review Focus 2: a model synced BEFORE the feature (empty capabilities
// blob) must receive detected caps + context on the next sync.
func TestSync_PreFeatureSyncedRowReceivesDetection(t *testing.T) {
	s := openStore(t, "acc-a")
	if err := s.PutModel(context.Background(), storage.Model{
		ID: "custom-openai/glm-5.3-flash", ProviderID: "acc-a", Name: "glm-5.3-flash",
		Origin: storage.ModelSyncedOrigin, // pre-feature sync: empty blobs
	}); err != nil {
		t.Fatal(err)
	}
	reg := &fakeRegistry{}
	reg.Register((&stubProvider{id: "acc-a", list: []provider.ModelInfo{detectGLM()}}).asLister())

	if _, err := New(s, reg, testLogger()).Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	m := mustGet(t, s, "custom-openai/glm-5.3-flash")
	assertBlob(t, "capabilities", string(m.Capabilities), map[string]any{
		"streaming": true, "tools": true, "vision": true, "thinking": true,
		"web_search": false, "web_fetch": false, "code_execution": false,
	})
	assertBlob(t, "compat", string(m.Compat), map[string]any{
		"thinking_handling": "strip", "context_window": 1048576,
	})
}

// Review Focus 3: an operator-edited context_window (non-zero in the
// compat blob) must survive every sync run.
func TestSync_OperatorContextWindowPinSurvives(t *testing.T) {
	s := openStore(t, "acc-a")
	if err := s.PutModel(context.Background(), storage.Model{
		ID: "custom-openai/glm-5.3-flash", ProviderID: "acc-a", Name: "glm-5.3-flash",
		Capabilities: []byte(`{"vision":true}`),
		Compat:       []byte(`{"thinking_handling":"block","context_window":131072}`),
		Origin:       storage.ModelSyncedOrigin,
	}); err != nil {
		t.Fatal(err)
	}
	reg := &fakeRegistry{}
	reg.Register((&stubProvider{id: "acc-a", list: []provider.ModelInfo{detectGLM()}}).asLister())

	if _, err := New(s, reg, testLogger()).Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	m := mustGet(t, s, "custom-openai/glm-5.3-flash")
	if string(m.Compat) != `{"thinking_handling":"block","context_window":131072}` {
		t.Errorf("operator context pin was overwritten: %s", m.Compat)
	}
	if string(m.Capabilities) != `{"vision":true}` {
		t.Errorf("operator capabilities blob was overwritten: %s", m.Capabilities)
	}
}

// Review Focus 4: a manual-origin row with the same ID must never be
// touched by detection.
func TestSync_ManualRowNeverMerged(t *testing.T) {
	s := openStore(t, "acc-a")
	if err := s.PutModel(context.Background(), storage.Model{
		ID: "custom-openai/glm-5.3-flash", ProviderID: "acc-a", Name: "glm-5.3-flash",
		Capabilities: []byte(`{"tools":false}`), // manual operator choice
		Compat:       []byte(`{"thinking_handling":"block"}`),
		Origin:       storage.ModelManualOrigin,
	}); err != nil {
		t.Fatal(err)
	}
	reg := &fakeRegistry{}
	reg.Register((&stubProvider{id: "acc-a", list: []provider.ModelInfo{detectGLM()}}).asLister())

	if _, err := New(s, reg, testLogger()).Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	m := mustGet(t, s, "custom-openai/glm-5.3-flash")
	if string(m.Compat) != `{"thinking_handling":"block"}` || string(m.Capabilities) != `{"tools":false}` {
		t.Errorf("manual row was touched by detection: caps=%s compat=%s", m.Capabilities, m.Compat)
	}
}

// An existing synced row whose compat blob carries other keys but no
// context_window receives the detected value only — the other keys and a
// non-empty capabilities blob stay untouched.
func TestSync_MergesContextWindowIntoPopulatedCompat(t *testing.T) {
	s := openStore(t, "acc-a")
	if err := s.PutModel(context.Background(), storage.Model{
		ID: "custom-openai/glm-5.3-flash", ProviderID: "acc-a", Name: "glm-5.3-flash",
		Capabilities: []byte(`{"vision":true}`),               // operator blob: untouched
		Compat:       []byte(`{"thinking_handling":"block"}`), // no pin
		Origin:       storage.ModelSyncedOrigin,
	}); err != nil {
		t.Fatal(err)
	}
	reg := &fakeRegistry{}
	reg.Register((&stubProvider{id: "acc-a", list: []provider.ModelInfo{detectGLM()}}).asLister())

	if _, err := New(s, reg, testLogger()).Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	m := mustGet(t, s, "custom-openai/glm-5.3-flash")
	assertBlob(t, "compat", string(m.Compat), map[string]any{
		"thinking_handling": "block", "context_window": 1048576,
	})
	if string(m.Capabilities) != `{"vision":true}` {
		t.Errorf("non-empty capabilities blob must not merge: %s", m.Capabilities)
	}
}

// Repeated runs are idempotent: after detection has been persisted, the
// next run makes no further writes (Updated stays 0).
func TestSync_DetectionMergeIsIdempotent(t *testing.T) {
	s := openStore(t, "acc-a")
	reg := &fakeRegistry{}
	reg.Register((&stubProvider{id: "acc-a", list: []provider.ModelInfo{detectGLM()}}).asLister())
	syncer := New(s, reg, testLogger())

	if _, err := syncer.Run(context.Background()); err != nil {
		t.Fatalf("Run 1: %v", err)
	}
	state, err := syncer.Run(context.Background())
	if err != nil {
		t.Fatalf("Run 2: %v", err)
	}
	if state.Providers[0].Updated != 0 {
		t.Errorf("second run re-wrote rows: Updated=%d", state.Providers[0].Updated)
	}
}
