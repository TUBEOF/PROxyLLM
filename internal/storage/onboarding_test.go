package storage

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestOnboardingState_RoundTrip(t *testing.T) {
	ctx := context.Background()
	st, err := Open(testCfg(filepath.Join(t.TempDir(), "test.db")))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = st.Close() }()

	if _, ok, err := st.GetOnboardingState(ctx); err != nil || ok {
		t.Fatalf("missing state: ok=%v err=%v, want false/nil", ok, err)
	}

	before := time.Now().Unix()
	if err := st.PutOnboardingState(ctx, OnboardingState{
		Completed: []string{"provider", "model"}, Dismissed: false,
	}); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, ok, err := st.GetOnboardingState(ctx)
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	if got.Version != OnboardingStateVersion {
		t.Errorf("version = %d, want %d", got.Version, OnboardingStateVersion)
	}
	if len(got.Completed) != 2 || got.Completed[0] != "provider" {
		t.Errorf("completed = %v", got.Completed)
	}
	if got.UpdatedAt < before {
		t.Errorf("updated_at = %d, want >= %d", got.UpdatedAt, before)
	}

	// Malformed state degrades to the first-run state, never an error.
	if err := st.SetConfigValue(ctx, onboardingStateKey, "{not json"); err != nil {
		t.Fatalf("seed malformed: %v", err)
	}
	if _, ok, err := st.GetOnboardingState(ctx); err != nil || ok {
		t.Fatalf("malformed state: ok=%v err=%v, want false/nil", ok, err)
	}
}
