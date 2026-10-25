package storage

// Onboarding state (the first-run wizard): a small structured blob in the
// config KV under "onboarding_state". No migration — the config table is the
// typed-free top layer. The state records which steps the operator finished
// and whether the wizard was dismissed; the live "is this instance still
// unconfigured?" answer is recomputed from the list endpoints, so a stale
// flag can never hide a broken setup.

import (
	"context"
	"encoding/json"
	"time"
)

const onboardingStateKey = "onboarding_state"

// OnboardingStateVersion is the current wizard state schema.
const OnboardingStateVersion = 1

// OnboardingState is the persisted wizard progress.
type OnboardingState struct {
	Version     int      `json:"version"`
	Completed   []string `json:"completed"`
	Dismissed   bool     `json:"dismissed"`
	CompletedAt int64    `json:"completed_at,omitempty"`
	UpdatedAt   int64    `json:"updated_at,omitempty"`
}

// GetOnboardingState reads the wizard state. Missing or malformed state is
// ok=false (the normal first-run case); readers fall back to the counts.
func (s *Store) GetOnboardingState(ctx context.Context) (OnboardingState, bool, error) {
	v, ok, err := s.GetConfigValue(ctx, onboardingStateKey)
	if err != nil || !ok {
		return OnboardingState{}, false, err
	}
	var st OnboardingState
	if err := json.Unmarshal([]byte(v), &st); err != nil {
		return OnboardingState{}, false, nil
	}
	if st.Version == 0 {
		st.Version = OnboardingStateVersion
	}
	return st, true, nil
}

// PutOnboardingState persists the wizard state, stamping updated_at.
func (s *Store) PutOnboardingState(ctx context.Context, st OnboardingState) error {
	if st.Version == 0 {
		st.Version = OnboardingStateVersion
	}
	st.UpdatedAt = time.Now().Unix()
	raw, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return s.SetConfigValue(ctx, onboardingStateKey, string(raw))
}
