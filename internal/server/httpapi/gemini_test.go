package httpapi

// Parser unit tests for the Gemini surface's path convention (the action
// carries inside the model segment — ServeMux cannot express it).

import "testing"

func TestParseGeminiPath(t *testing.T) {
	for _, tc := range []struct {
		in                 string
		wantModel, wantAct string
		wantOK             bool
	}{
		{"/v1beta/models/gemini-2:generateContent", "gemini-2", "generateContent", true},
		{"/v1beta/models/gemini-2:streamGenerateContent", "gemini-2", "streamGenerateContent", true},
		{"/v1beta/models", "", "", false},
		{"/v1beta/models/gemini-2", "", "", false},
		{"/v1beta/models/:generateContent", "", "", false},
		{"/api/chat", "", "", false},
	} {
		m, a, ok := parseGeminiPath(tc.in)
		if ok != tc.wantOK || (ok && (m != tc.wantModel || a != tc.wantAct)) {
			t.Errorf("parseGeminiPath(%q) = %q %q %v, want %q %q %v",
				tc.in, m, a, ok, tc.wantModel, tc.wantAct, tc.wantOK)
		}
	}
}
