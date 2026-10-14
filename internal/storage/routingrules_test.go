package storage

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// Spec: schema (routing_rules: match_model, match_capabilities, chain,
// priority, enabled); rule state lives in the DB, WebUI-editable. The
// accessors must round-trip every field and preserve the
// deterministic rule order the router depends on (priority DESC, registration
// order for ties).

func TestRoutingRules_Empty(t *testing.T) {
	s := openTokenStore(t) // reuses the migrate+envelope wiring; rules need no tokens

	rules, err := s.ListRoutingRules(context.Background())
	if err != nil {
		t.Fatalf("ListRoutingRules: %v", err)
	}
	if len(rules) != 0 {
		t.Errorf("fresh store has %d rules, want 0", len(rules))
	}
}

func TestRoutingRule_RoundTrip(t *testing.T) {
	s := openTokenStore(t)
	ctx := context.Background()

	in := RoutingRule{
		ID:                "rule-1",
		MatchModel:        "claude-*",
		MatchCapabilities: json.RawMessage(`{"vision":true}`),
		Chain:             []string{"codex-1", "ollama-1"},
		Priority:          10,
		Enabled:           true,
	}
	if err := s.PutRoutingRule(ctx, in); err != nil {
		t.Fatalf("PutRoutingRule: %v", err)
	}

	rules, err := s.ListRoutingRules(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 1 {
		t.Fatalf("stored %d rules, want 1", len(rules))
	}
	got := rules[0]
	if got.ID != in.ID || got.MatchModel != in.MatchModel ||
		string(got.MatchCapabilities) != string(in.MatchCapabilities) ||
		got.Priority != in.Priority || got.Enabled != in.Enabled {
		t.Errorf("round-trip mismatch: got %+v want %+v", got, in)
	}
	if len(got.Chain) != 2 || got.Chain[0] != "codex-1" || got.Chain[1] != "ollama-1" {
		t.Errorf("chain round-trip: got %v want [codex-1 ollama-1]", got.Chain)
	}
}

func TestRoutingRule_UpsertOverwrites(t *testing.T) {
	s := openTokenStore(t)
	ctx := context.Background()

	first := RoutingRule{ID: "rule-1", MatchModel: "gpt-*", Chain: []string{"codex-1"}, Enabled: true}
	if err := s.PutRoutingRule(ctx, first); err != nil {
		t.Fatal(err)
	}
	second := RoutingRule{ID: "rule-1", MatchModel: "claude-*", Chain: []string{"ollama-1"}, Priority: 5, Enabled: false}
	if err := s.PutRoutingRule(ctx, second); err != nil {
		t.Fatal(err)
	}

	rules, err := s.ListRoutingRules(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 1 {
		t.Fatalf("upsert left %d rules, want 1", len(rules))
	}
	if rules[0].MatchModel != "claude-*" || rules[0].Enabled {
		t.Errorf("upsert did not overwrite: %+v", rules[0])
	}
}

func TestRoutingRule_ListOrdersByPriorityDescThenID(t *testing.T) {
	s := openTokenStore(t)
	ctx := context.Background()

	// Insert deliberately out of order; ties on priority resolve by ID
	// (deterministic secondary sort — the router must never see
	// registration-order-dependent results for equal priorities).
	seed := []RoutingRule{
		{ID: "rule-b", MatchModel: "m*", Chain: []string{"codex-1"}, Priority: 5},
		{ID: "rule-a", MatchModel: "n*", Chain: []string{"codex-1"}, Priority: 5},
		{ID: "rule-c", MatchModel: "o*", Chain: []string{"codex-1"}, Priority: 10},
		{ID: "rule-d", MatchModel: "p*", Chain: []string{"codex-1"}, Priority: 1},
	}
	for _, r := range seed {
		if err := s.PutRoutingRule(ctx, r); err != nil {
			t.Fatal(err)
		}
	}

	rules, err := s.ListRoutingRules(ctx)
	if err != nil {
		t.Fatal(err)
	}
	wantIDs := []string{"rule-c", "rule-a", "rule-b", "rule-d"}
	if len(rules) != len(wantIDs) {
		t.Fatalf("stored %d rules, want %d", len(rules), len(wantIDs))
	}
	for i, want := range wantIDs {
		if rules[i].ID != want {
			t.Errorf("rules[%d].ID = %s, want %s (order: %v)", i, rules[i].ID, want, ruleIDs(rules))
		}
	}
}

func TestRoutingRule_Delete(t *testing.T) {
	s := openTokenStore(t)
	ctx := context.Background()

	if err := s.PutRoutingRule(ctx, RoutingRule{ID: "rule-1", MatchModel: "x", Chain: []string{"codex-1"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteRoutingRule(ctx, "rule-1"); err != nil {
		t.Fatalf("DeleteRoutingRule: %v", err)
	}
	if err := s.DeleteRoutingRule(ctx, "rule-1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleting missing rule = %v, want ErrNotFound", err)
	}
}

func TestRoutingRule_EmptyChainRejected(t *testing.T) {
	s := openTokenStore(t)

	// A rule without a chain can never resolve — refuse at the
	// persistence boundary, not in the router.
	err := s.PutRoutingRule(context.Background(), RoutingRule{ID: "rule-1", MatchModel: "x", Chain: nil})
	if err == nil {
		t.Error("rule with empty chain must be rejected")
	}
}

func ruleIDs(rules []RoutingRule) []string {
	ids := make([]string, len(rules))
	for i, r := range rules {
		ids[i] = r.ID
	}
	return ids
}
