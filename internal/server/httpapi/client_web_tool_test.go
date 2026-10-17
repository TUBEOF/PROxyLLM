package httpapi

// The client-tool claim: Claude Code ships its web tools as ORDINARY client
// tools — a raw capture of its requests shows `WebFetch`/`WebSearch` among ~59
// function definitions with no server-tool `type` — and fetches the page in the
// CLI itself. Without the claim the proxy sees no web-tool request at all, so
// there is no web-tool account walk and no fallback row in the log; with it the
// model's call is answered by the proxy and the row appears, exactly as for a
// dated server-tool definition.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/tubeof/proxyllm/internal/routing"
	"github.com/tubeof/proxyllm/internal/testutil/mockprovider"
	"github.com/tubeof/proxyllm/internal/translate/ir"
)

// stubEmulator stands in for a web-tool account (the instance's acc-ollama-web).
type stubEmulator struct {
	fetched []string
	queries []string
}

func (s *stubEmulator) Search(_ context.Context, query string, _ ir.ServerToolConfig) ([]ir.WebSearchResultItem, error) {
	s.queries = append(s.queries, query)
	return []ir.WebSearchResultItem{{URL: "https://example.com", Title: "Example Domain"}}, nil
}

func (s *stubEmulator) Fetch(_ context.Context, url string, st ir.ServerToolConfig) (*ir.WebFetchContent, error) {
	s.fetched = append(s.fetched, url)
	return &ir.WebFetchContent{
		URL: url, Title: "Example Domain", Source: "text", MediaType: "text/plain",
		Data: "Example Domain body text", CitationsEnabled: st.CitationsEnabled,
	}, nil
}

// clientWebToolRequest is what Claude Code sends for a WebFetch turn: the URL
// in the conversation (the emulation refuses URLs the client never mentioned)
// and the CLI's own tool, not a dated server-tool definition.
const clientWebToolRequest = `{
  "model": "ollama-cloud/m",
  "max_tokens": 256,
  "messages": [{"role": "user", "content": "fetch https://example.com and quote the title"}],
  "tools": [
    {"name": "WebFetch", "description": "Fetches content from a specified URL and processes it using an AI model",
     "input_schema": {"type": "object", "properties": {"url": {"type": "string"}, "prompt": {"type": "string"}}, "required": ["url", "prompt"]}},
    {"name": "get_weather", "description": "weather", "input_schema": {"type": "object", "properties": {}}}
  ]
}`

// TestClientWebToolIsClaimedAndServedThroughTheProxy is the regression: the
// model's call to the CLI's own WebFetch must be served by the proxy's web-tool
// account (child row in the log), not handed back to the client.
func TestClientWebToolIsClaimedAndServedThroughTheProxy(t *testing.T) {
	e := newAliasWebToolEnv(t)
	emu := &stubEmulator{}
	e.exec.SetEmulator(emu)

	const toolCall = `{"id":"x","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"web_fetch","arguments":"{\"url\":\"https://example.com\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`
	const answer = `{"id":"y","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"The title is Example Domain."},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`
	e.mock.Enqueue(
		mockprovider.NewScript().JSONResponse(200, toolCall, nil),
		mockprovider.NewScript().JSONResponse(200, answer, nil),
	)

	status, body := e.post(t, "/v1/messages", clientWebToolRequest)
	if status != 200 {
		t.Fatalf("status = %d, body %s", status, body)
	}
	if len(emu.fetched) != 1 || emu.fetched[0] != "https://example.com" {
		t.Fatalf("the proxy did not fetch through the web-tool account: %v", emu.fetched)
	}

	reqs := e.mock.Requests()
	if len(reqs) != 2 {
		t.Fatalf("upstream requests = %d, want 2 (the loop answers the call and re-dispatches)", len(reqs))
	}
	first := reqs[0].Body
	if strings.Contains(first, `"WebFetch"`) {
		t.Errorf("the CLI's own client tool rode upstream unconverted: %s", first)
	}
	if !strings.Contains(first, `"web_fetch"`) || !strings.Contains(first, `"url"`) {
		t.Errorf("the emulated web_fetch definition is missing upstream: %s", first)
	}
	if !strings.Contains(first, `"get_weather"`) {
		t.Errorf("an unclaimed client tool must ride untouched: %s", first)
	}
	// The client gets the family's result blocks — the shape it renders as a
	// web fetch the API performed.
	if !strings.Contains(body, "web_fetch_tool_result") || !strings.Contains(body, "Example Domain body text") {
		t.Errorf("the client did not receive the fetched page: %s", body)
	}

	// The fallback row: one child op naming the account that answered.
	logs, err := e.store.ListRequestLogs(context.Background(), 5)
	if err != nil || len(logs) != 1 {
		t.Fatalf("request log rows = %d (err %v), want 1", len(logs), err)
	}
	kids, err := e.store.ListChildLogsForParents(context.Background(), []int64{logs[0].ID})
	if err != nil {
		t.Fatalf("list child logs: %v", err)
	}
	rows := kids[logs[0].ID]
	if len(rows) != 1 {
		t.Fatalf("child rows = %d, want 1 (the web-tool step must be logged)", len(rows))
	}
	if rows[0].Kind != string(routing.OpEmulatedTool) || rows[0].ToRef != "web_fetch" {
		t.Errorf("child row = %+v, want an emulated_tool op for web_fetch", rows[0])
	}
	if !strings.Contains(string(logs[0].RouteTrace), "client web-tool def") {
		t.Errorf("the claim is not traced: %s", logs[0].RouteTrace)
	}
}

// The claim is the operator's: an empty mapping (settings KV
// `client_web_tools`) leaves the client's tool untouched, and then the proxy
// neither fetches nor logs a web-tool row.
func TestClientWebToolClaimOffInTheMapping(t *testing.T) {
	e := newAliasWebToolEnv(t)
	emu := &stubEmulator{}
	e.exec.SetEmulator(emu)
	e.handler.SetClientWebTools(map[string]string{}) // the operator turned the claim off

	const toolCall = `{"id":"x","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"WebFetch","arguments":"{\"url\":\"https://example.com\",\"prompt\":\"title?\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`
	e.mock.Enqueue(mockprovider.NewScript().JSONResponse(200, toolCall, nil))

	status, body := e.post(t, "/v1/messages", clientWebToolRequest)
	if status != 200 {
		t.Fatalf("status = %d, body %s (the client's own tool call is the client's to answer)", status, body)
	}
	if len(emu.fetched) != 0 {
		t.Errorf("the proxy fetched although the claim is off: %v", emu.fetched)
	}
	if !strings.Contains(body, `"WebFetch"`) {
		t.Errorf("the client's own tool call must ride back to the client: %s", body)
	}
	logs, err := e.store.ListRequestLogs(context.Background(), 5)
	if err != nil || len(logs) != 1 {
		t.Fatalf("request log rows = %d (err %v), want 1", len(logs), err)
	}
	var trace struct {
		Events []struct {
			Type   string `json:"type"`
			Reason string `json:"reason"`
		} `json:"events"`
	}
	if err := json.Unmarshal(logs[0].RouteTrace, &trace); err != nil {
		t.Fatalf("route_trace: %v", err)
	}
	for _, ev := range trace.Events {
		if strings.Contains(ev.Reason, "client web-tool def") {
			t.Errorf("the claim fired although the mapping is empty: %+v", ev)
		}
	}
}
