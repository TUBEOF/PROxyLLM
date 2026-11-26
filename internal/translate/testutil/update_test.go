package testutil

import "testing"

// GOLDEN_UPDATE is local-only (ADR-028). The CI guard is the whole point:
// a pipeline that regenerated the expectation would check nothing.
func TestUpdateGuardRefusesInCI(t *testing.T) {
	t.Setenv("CI", "true")
	if err := updateGuard(); err == nil {
		t.Fatal("updateGuard must refuse when CI=true")
	}
	t.Setenv("CI", "0")
	if err := updateGuard(); err != nil {
		t.Fatalf("CI=0 must be allowed: %v", err)
	}
}

func TestUpdateModeOffByDefault(t *testing.T) {
	t.Setenv("GOLDEN_UPDATE", "")
	if UpdateMode(t) {
		t.Fatal("update mode must be off without GOLDEN_UPDATE=1")
	}
}

func TestExpectedPathGuard(t *testing.T) {
	if err := expectedPathGuard("openai/chat-request.json"); err == nil {
		t.Fatal("a source fixture must be refused")
	}
	if err := expectedPathGuard("openai/chat-stream.expected.json"); err != nil {
		t.Fatalf("a derived fixture must be allowed: %v", err)
	}
}
