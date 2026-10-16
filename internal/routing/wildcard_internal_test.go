package routing

// matchWildcard's own contract: `*` matches any run of characters — empty
// included — and the literal segments around it must appear in order; a
// pattern without `*` matches on exact equality only. The alias-selection
// tie-break is pinned here too. The behaviour these produce at the router
// level is pinned in alias_family_test.go (package routing_test).

import "testing"

func TestMatchWildcard_GlobSemantics(t *testing.T) {
	cases := []struct {
		pattern, model string
		want           bool
	}{
		{"*", "anything", true},
		{"*", "", true},
		{"claude-*", "claude-sonnet-4-6", true},
		{"claude-*", "gpt-oss", false},
		{"*sonnet", "claude-sonnet", true},
		{"*sonnet", "claude-sonnet-4-6", false},
		{"*sonnet*", "claude-3-5-sonnet-20241022", true},
		{"*-sonnet-*", "claude-3-5-sonnet-20241022", true},
		{"claude-*-4-6", "claude-sonnet-4-6", true},
		{"claude-*-4-6", "claude-sonnet-4-7", false},
		{"a*b*c", "aXbYc", true},
		{"a*b*c", "acb", false},
		{"a*a", "a", false},
		{"*a*", "", false},
		{"claude", "claude", true},
		{"claude", "claude-opus-5", false}, // no `*`: exact only, never a substring
	}
	for _, tc := range cases {
		if got := matchWildcard(tc.pattern, tc.model); got != tc.want {
			t.Errorf("matchWildcard(%q, %q) = %v, want %v", tc.pattern, tc.model, got, tc.want)
		}
	}
}

func TestWildcardAlias_MostLiteralThenLexicographic(t *testing.T) {
	aliases := map[string]string{
		"claude-*":        "broad",
		"claude-sonnet-*": "narrow",
		"ab*ef":           "left",
		"a*d*ef":          "first", // same literal count as ab*ef; lexicographically smaller
		"claude-opus-5":   "plain", // no `*`: never selected by wildcardAlias
	}
	if target, ok := wildcardAlias("claude-sonnet-4-6", aliases); !ok || target != "narrow" {
		t.Fatalf("wildcardAlias(claude-sonnet-4-6) = %q, %v; want narrow (most literal)", target, ok)
	}
	if target, ok := wildcardAlias("abcdef", aliases); !ok || target != "first" {
		t.Fatalf("wildcardAlias(abcdef) = %q, %v; want the lexicographically smaller tie winner", target, ok)
	}
	if _, ok := wildcardAlias("gpt-4o", aliases); ok {
		t.Error("no pattern matches gpt-4o; want no resolution")
	}
	plain := map[string]string{"abcdef": "exact"}
	if _, ok := wildcardAlias("abcdef", plain); ok {
		t.Error("a key without `*` must never be selected by wildcardAlias")
	}
}
