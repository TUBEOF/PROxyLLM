package anthropic_test

import (
	"bytes"
	"encoding/json"
	"testing"

	anth "github.com/tubeof/proxyllm/internal/translate/anthropic"
	"github.com/tubeof/proxyllm/internal/translate/ir"
	"github.com/tubeof/proxyllm/internal/translate/testutil"
)

// Server-tool contract tests (RED-GREEN per Analysis F). Every assertion
// derives from testdata/golden/anthropic/server-tool-web-*.json
// (provenance: docs.anthropic.com web-search-tool / web-fetch-tool pages,
// accessed 2026-09-12, facts A3/A8/A9/A10).

func TestDecodeRequest_WebSearchToolDef(t *testing.T) {
	raw := testutil.LoadGolden(t, "anthropic/server-tool-web-search-request.json")
	req, err := anth.Codec{}.DecodeRequest(raw)
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}

	if len(req.Tools) != 1 {
		t.Fatalf("tools = %d, want 1", len(req.Tools))
	}
	tool := req.Tools[0]
	if tool.ServerToolName() != "web_search" {
		t.Errorf("ServerToolName() = %q, want web_search (family identity, not the dated string %q)", tool.ServerToolName(), tool.Type)
	}
	if tool.Type != "web_search_20250305" {
		t.Errorf("type = %q, want verbatim web_search_20250305 (C3 open-list ride-along)", tool.Type)
	}
	if tool.Name != "web_search" {
		t.Errorf("name = %q, want web_search", tool.Name)
	}

	// The §6.2 emulation layer enforces these knobs (max_uses, domain
	// filters, user_location) — they must decode (fixture facts A8/A10).
	st := tool.ServerTool
	if st == nil {
		t.Fatal("ServerTool config missing")
	}
	if st.MaxUses != 5 {
		t.Errorf("max_uses = %d, want 5", st.MaxUses)
	}
	if len(st.AllowedDomains) != 2 || st.AllowedDomains[0] != "example.com" {
		t.Errorf("allowed_domains = %v", st.AllowedDomains)
	}
	if len(st.BlockedDomains) != 1 || st.BlockedDomains[0] != "untrustedsource.com" {
		t.Errorf("blocked_domains = %v", st.BlockedDomains)
	}
	if st.UserLocation == nil || st.UserLocation.City != "San Francisco" || st.UserLocation.Country != "US" {
		t.Errorf("user_location = %+v", st.UserLocation)
	}
}

func TestDecodeRequest_WebFetchToolDef(t *testing.T) {
	raw := testutil.LoadGolden(t, "anthropic/server-tool-web-fetch-request.json")
	req, err := anth.Codec{}.DecodeRequest(raw)
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}

	if len(req.Tools) != 1 {
		t.Fatalf("tools = %d, want 1", len(req.Tools))
	}
	tool := req.Tools[0]
	if tool.ServerToolName() != "web_fetch" {
		t.Errorf("ServerToolName() = %q, want web_fetch", tool.ServerToolName())
	}
	if tool.Type != "web_fetch_20250910" {
		t.Errorf("type = %q, want verbatim web_fetch_20250910", tool.Type)
	}

	st := tool.ServerTool
	if st == nil {
		t.Fatal("ServerTool config missing")
	}
	if st.MaxUses != 10 {
		t.Errorf("max_uses = %d, want 10", st.MaxUses)
	}
	if !st.CitationsEnabled {
		t.Error("citations.enabled lost")
	}
	if st.MaxContentTokens != 100000 {
		t.Errorf("max_content_tokens = %d, want 100000", st.MaxContentTokens)
	}
	if len(st.BlockedDomains) != 1 || st.BlockedDomains[0] != "private.example.com" {
		t.Errorf("blocked_domains = %v", st.BlockedDomains)
	}
}

func TestDecodeResponse_WebSearchSuccess(t *testing.T) {
	raw := testutil.LoadGolden(t, "anthropic/server-tool-web-search-response.json")
	resp, err := anth.Codec{}.DecodeResponse(raw)
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}

	// content: server_tool_use → web_search_tool_result (ARRAY content) → text
	if len(resp.Content) != 3 {
		t.Fatalf("content parts = %d, want 3 (server_tool_use, result, text)", len(resp.Content))
	}

	use, ok := resp.Content[0].(ir.ServerToolUsePart)
	if !ok {
		t.Fatalf("content[0] = %T, want ir.ServerToolUsePart", resp.Content[0])
	}
	if use.ID != "srvtoolu_01Ab3cD4eF5g6h7I8j9k" || use.Name != "web_search" {
		t.Errorf("server_tool_use id/name = %q/%q", use.ID, use.Name)
	}
	var input map[string]any
	if err := json.Unmarshal(use.Input, &input); err != nil {
		t.Fatalf("server_tool_use.input not JSON: %v", err)
	}
	if input["query"] != "latest Go release" {
		t.Errorf("input.query = %v", input["query"])
	}

	// web_search_tool_result content is an ARRAY of web_search_result blocks.
	res, ok := resp.Content[1].(ir.ServerToolResultPart)
	if !ok {
		t.Fatalf("content[1] = %T, want ir.ServerToolResultPart", resp.Content[1])
	}
	if res.ToolUseID != use.ID {
		t.Errorf("result tool_use_id = %q, want %q", res.ToolUseID, use.ID)
	}
	if res.IsError {
		t.Error("result must not be an error result")
	}
	if res.ToolName != "web_search" {
		t.Errorf("result tool name = %q, want web_search", res.ToolName)
	}
	if len(res.Search) != 2 {
		t.Fatalf("search results = %d, want 2", len(res.Search))
	}
	want := ir.WebSearchResultItem{
		URL:              "https://go.dev/dl/",
		Title:            "Downloads - The Go Programming Language",
		EncryptedContent: "GnO7PpF4sV4aFq9...",
		PageAge:          "2026-09-01",
	}
	if res.Search[0] != want {
		t.Errorf("search[0] = %+v, want %+v", res.Search[0], want)
	}

	// usage: server_tool_use.web_search_requests
	if resp.Usage.WebSearchRequests != 1 {
		t.Errorf("usage.web_search_requests = %d, want 1", resp.Usage.WebSearchRequests)
	}
}

func TestDecodeResponse_WebSearchError(t *testing.T) {
	raw := testutil.LoadGolden(t, "anthropic/server-tool-web-search-error-response.json")
	resp, err := anth.Codec{}.DecodeResponse(raw)
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}

	if len(resp.Content) != 2 {
		t.Fatalf("content parts = %d, want 2", len(resp.Content))
	}
	res, ok := resp.Content[1].(ir.ServerToolResultPart)
	if !ok {
		t.Fatalf("content[1] = %T, want ir.ServerToolResultPart", resp.Content[1])
	}
	// Error shape: content is a single error object, NOT an array (A10).
	if !res.IsError {
		t.Error("result must decode as error")
	}
	if res.ErrorCode != "max_uses_exceeded" {
		t.Errorf("error_code = %q, want max_uses_exceeded", res.ErrorCode)
	}
	if len(res.Search) != 0 {
		t.Errorf("error result must carry no search items, got %d", len(res.Search))
	}
	if resp.Usage.WebSearchRequests != 5 {
		t.Errorf("usage.web_search_requests = %d, want 5", resp.Usage.WebSearchRequests)
	}
}

func TestDecodeResponse_WebFetchSuccess(t *testing.T) {
	raw := testutil.LoadGolden(t, "anthropic/server-tool-web-fetch-response.json")
	resp, err := anth.Codec{}.DecodeResponse(raw)
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}

	// content: server_tool_use → web_fetch_tool_result (SINGLE-object content) → text
	if len(resp.Content) != 3 {
		t.Fatalf("content parts = %d, want 3", len(resp.Content))
	}

	use, ok := resp.Content[0].(ir.ServerToolUsePart)
	if !ok {
		t.Fatalf("content[0] = %T, want ir.ServerToolUsePart", resp.Content[0])
	}
	if use.Name != "web_fetch" {
		t.Errorf("server_tool_use name = %q, want web_fetch", use.Name)
	}

	// web_fetch_tool_result content is a SINGLE web_fetch_result object —
	// the wire asymmetry vs web_search's array (fixture meta note).
	res, ok := resp.Content[1].(ir.ServerToolResultPart)
	if !ok {
		t.Fatalf("content[1] = %T, want ir.ServerToolResultPart", resp.Content[1])
	}
	if res.Fetch == nil {
		t.Fatal("fetch content missing")
	}
	if res.Fetch.URL != "https://example.com/article" || res.Fetch.Title != "Article Title" {
		t.Errorf("fetch url/title = %q/%q", res.Fetch.URL, res.Fetch.Title)
	}
	if res.Fetch.Source != "text" || res.Fetch.MediaType != "text/plain" {
		t.Errorf("fetch source/media_type = %q/%q, want text/text/plain", res.Fetch.Source, res.Fetch.MediaType)
	}
	if res.Fetch.Data != "Full text content of the article..." {
		t.Errorf("fetch data = %q", res.Fetch.Data)
	}
	if !res.Fetch.CitationsEnabled {
		t.Error("citations.enabled lost")
	}
	if res.Fetch.RetrievedAt != "2025-08-25T10:30:00Z" {
		t.Errorf("retrieved_at = %q", res.Fetch.RetrievedAt)
	}
	if resp.Usage.WebFetchRequests != 1 {
		t.Errorf("usage.web_fetch_requests = %d, want 1", resp.Usage.WebFetchRequests)
	}
}

func TestDecodeResponse_WebFetchError(t *testing.T) {
	raw := testutil.LoadGolden(t, "anthropic/server-tool-web-fetch-error-response.json")
	resp, err := anth.Codec{}.DecodeResponse(raw)
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}

	if len(resp.Content) != 2 {
		t.Fatalf("content parts = %d, want 2", len(resp.Content))
	}
	res, ok := resp.Content[1].(ir.ServerToolResultPart)
	if !ok {
		t.Fatalf("content[1] = %T, want ir.ServerToolResultPart", resp.Content[1])
	}
	if !res.IsError || res.ErrorCode != "url_not_accessible" {
		t.Errorf("result error = %v/%q, want true/url_not_accessible", res.IsError, res.ErrorCode)
	}
	if resp.Usage.WebFetchRequests != 1 {
		t.Errorf("usage.web_fetch_requests = %d, want 1", resp.Usage.WebFetchRequests)
	}
}

func TestEncodeResponse_ServerToolBlocks(t *testing.T) {
	// Round-trip: encode IR server-tool parts back into wire blocks and
	// re-decode — the shapes must survive Anthropic→Anthropic (C3).
	resp := ir.Response{
		ID:         "msg_test",
		Model:      "claude-sonnet-5",
		StopReason: ir.StopEndTurn,
		Content: []ir.Part{
			ir.ServerToolUsePart{ID: "srvtoolu_01", Name: "web_search", Input: []byte(`{"query":"go release"}`)},
			ir.ServerToolResultPart{
				ToolUseID: "srvtoolu_01",
				ToolName:  "web_search",
				Search: []ir.WebSearchResultItem{
					{URL: "https://go.dev/", Title: "Go", EncryptedContent: "abc", PageAge: "2026-09-01"},
				},
			},
		},
		Usage: ir.Usage{InputTokens: 10, OutputTokens: 5, WebSearchRequests: 1},
	}
	raw, err := anth.Codec{}.EncodeResponse(resp)
	if err != nil {
		t.Fatalf("EncodeResponse: %v", err)
	}
	got, err := anth.Codec{}.DecodeResponse(raw)
	if err != nil {
		t.Fatalf("DecodeResponse(round trip): %v", err)
	}
	if len(got.Content) != 2 {
		t.Fatalf("round trip content = %d parts, want 2", len(got.Content))
	}
	use, ok := got.Content[0].(ir.ServerToolUsePart)
	if !ok || use.ID != "srvtoolu_01" || use.Name != "web_search" {
		t.Fatalf("round trip server_tool_use = %+v", got.Content[0])
	}
	res, ok := got.Content[1].(ir.ServerToolResultPart)
	if !ok || res.ToolUseID != "srvtoolu_01" || res.IsError || len(res.Search) != 1 || res.Search[0].URL != "https://go.dev/" {
		t.Fatalf("round trip search result = %+v", got.Content[1])
	}
	if got.Usage.WebSearchRequests != 1 {
		t.Errorf("round trip usage.web_search_requests = %d", got.Usage.WebSearchRequests)
	}
}

func TestEncodeRequest_ServerToolDef(t *testing.T) {
	// A server-tool def must round-trip its dated type string verbatim.
	req := ir.Request{
		Model:     "claude-sonnet-5",
		MaxTokens: 1024,
		Messages: []ir.Message{
			{Role: ir.RoleUser, Parts: []ir.Part{ir.TextPart{Text: "search the web"}}},
		},
		Tools: []ir.Tool{{Name: "web_search", Type: "web_search_20250305"}},
	}
	raw, err := anth.Codec{}.EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	got, err := anth.Codec{}.DecodeRequest(raw)
	if err != nil {
		t.Fatalf("DecodeRequest(round trip): %v", err)
	}
	if len(got.Tools) != 1 || got.Tools[0].Type != "web_search_20250305" || got.Tools[0].Name != "web_search" {
		t.Fatalf("round trip tool def = %+v", got.Tools)
	}
	if got.Tools[0].ServerToolName() != "web_search" {
		t.Errorf("round trip ServerToolName() = %q", got.Tools[0].ServerToolName())
	}
}

// Server-tool STREAM decode (facts A5–A7, verified live 2026-09-12 via
// platform.claude.com/docs/en/build-with-claude/streaming):
//   - server_tool_use content_block_start carries id + name with an EMPTY
//     "input":{} — the input arrives via input_json_delta
//   - web_search_tool_result arrives COMPLETE in content_block_start
//     (content = array of web_search_result blocks, no result deltas)
//   - message_delta usage nests server_tool_use.web_search_requests
func TestStreamDecode_ServerToolBlocks(t *testing.T) {
	wire := testutil.LoadGolden(t, "anthropic/server-tool-stream.sse")
	want := testutil.LoadExpectedEvents(t, "anthropic/server-tool-stream.expected.json")

	got := testutil.DrainStreamDecoder(t, anth.NewStreamDecoder(bytes.NewReader(wire)))
	testutil.AssertEventsEqual(t, "anthropic/server-tool-stream.sse", want, got)
}

// The decoded sequence must expose the server-tool specifics at the part
// level (pairing by tool_use id, complete search items, nested usage
// counters) — the canonical sequence alone could hide empty fields.
func TestStreamDecode_ServerToolBlocks_Details(t *testing.T) {
	wire := testutil.LoadGolden(t, "anthropic/server-tool-stream.sse")
	events := testutil.DrainStreamDecoder(t, anth.NewStreamDecoder(bytes.NewReader(wire)))

	var use *ir.ServerToolUsePart
	var res *ir.ServerToolResultPart
	var finalUsage *ir.Usage
	for _, ev := range events {
		switch {
		case ev.Type == ir.BlockStart:
			if p, ok := ev.Part.(ir.ServerToolUsePart); ok {
				use = &p
			}
			if p, ok := ev.Part.(ir.ServerToolResultPart); ok {
				res = &p
			}
		case ev.Type == ir.MessageDelta && ev.MessageDelta != nil:
			u := ev.MessageDelta.Usage
			finalUsage = &u
		}
	}
	if use == nil {
		t.Fatal("no server_tool_use block in stream")
	}
	if use.ID != "srvtoolu_014hJH82Qum7Td6UV8gDXThB" || use.Name != "web_search" {
		t.Errorf("server_tool_use = %q/%q", use.ID, use.Name)
	}
	if len(use.Input) != 0 && string(use.Input) != "{}" {
		t.Errorf("server_tool_use input at block_start = %q, want empty or {} (input rides input_json_delta)", use.Input)
	}
	if res == nil {
		t.Fatal("no web_search_tool_result block in stream")
	}
	if res.ToolUseID != use.ID || res.ToolName != "web_search" || res.IsError {
		t.Errorf("result = %+v, want web_search paired with %q", res, use.ID)
	}
	if len(res.Search) != 1 {
		t.Fatalf("search items = %d, want 1 (block arrives complete in content_block_start)", len(res.Search))
	}
	if res.Search[0].URL != "https://example.com" || res.Search[0].Title != "Quantum Computing Breakthroughs in 2025" {
		t.Errorf("search[0] = %+v", res.Search[0])
	}
	if res.Search[0].EncryptedContent != "Ev0DCioIAxgCIiQ3NmU4ZmI4OC1k..." {
		t.Errorf("search[0].encrypted_content = %q", res.Search[0].EncryptedContent)
	}
	if finalUsage == nil {
		t.Fatal("no message_delta usage")
	}
	if finalUsage.InputTokens != 10682 || finalUsage.OutputTokens != 510 {
		t.Errorf("usage tokens = %d/%d, want 10682/510", finalUsage.InputTokens, finalUsage.OutputTokens)
	}
	if finalUsage.WebSearchRequests != 1 {
		t.Errorf("usage.web_search_requests = %d, want 1 (nested server_tool_use counter)", finalUsage.WebSearchRequests)
	}
}
