package server

// The console catalog and the server's construction path are two copies of
// the same capability defaults. This test keeps them from drifting: a new
// provider type or a changed default must land in both.

import (
	"reflect"
	"testing"

	"github.com/tubeof/proxyllm/internal/provider"
	"github.com/tubeof/proxyllm/internal/server/webui"
)

func TestDefaultCaps_MatchProviderTypeCatalog(t *testing.T) {
	types := []string{
		webui.ProviderTypeCustomOpenAI,
		webui.ProviderTypeAnthropic,
		webui.ProviderTypeOpenAIResponses,
		webui.ProviderTypeOllamaCloud,
		webui.ProviderTypeOllamaNative,
		webui.ProviderTypeOpenAIDecisions,
		webui.ProviderTypeCodexOAuth,
		webui.ProviderTypeFirecrawl,
		webui.ProviderTypeOllamaCloudWeb,
	}
	for _, typ := range types {
		desc, ok := webui.ProviderTypeDescription(typ)
		if !ok {
			t.Fatalf("provider type %q missing from the catalog", typ)
		}
		want := desc.DefaultCapabilities
		if want == nil {
			want = map[string]bool{}
		}
		if got := capsMap(defaultCaps(typ)); !reflect.DeepEqual(got, want) {
			t.Errorf("%s default caps = %v, catalog = %v", typ, got, want)
		}
	}
}

func capsMap(c provider.Capabilities) map[string]bool {
	out := map[string]bool{}
	for name, on := range map[string]bool{
		"streaming":      c.Streaming,
		"tools":          c.Tools,
		"vision":         c.Vision,
		"web_search":     c.WebSearch,
		"web_fetch":      c.WebFetch,
		"code_execution": c.CodeExecution,
		"thinking":       c.Thinking,
		"decision":       c.Decision,
	} {
		if on {
			out[name] = true
		}
	}
	return out
}
