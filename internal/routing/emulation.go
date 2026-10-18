package routing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/tubeof/proxyllm/internal/translate/ir"
)

// Emulator executes emulated web tools (the configured web-tool accounts:
// Firecrawl or Ollama Cloud Web Tools, walked in fallback-list order). It is
// declared here, not in the Firecrawl client package, so the routing
// engine stays decoupled from the tool backend (dependency inversion):
// tests drive it with fakes, production wires the Firecrawl adapter.
type Emulator interface {
	// Search answers a web_search invocation, honoring the definition
	// knobs (domain filters map to engine include/exclude lists).
	Search(ctx context.Context, query string, st ir.ServerToolConfig) ([]ir.WebSearchResultItem, error)

	// Fetch answers a web_fetch invocation with the normalized page
	// payload.
	Fetch(ctx context.Context, url string, st ir.ServerToolConfig) (*ir.WebFetchContent, error)
}

// maxEmulationRounds bounds the emulation round-trip loop as an internal
// safety net: a model that keeps re-issuing emulated calls despite error
// results cannot pin the request forever. Not a wire contract —
// operators bound invocations with max_uses.
const maxEmulationRounds = 32

// emulatedToolInput is the documented input object of both web tool
// families: query for web_search, url for web_fetch.
type emulatedToolInput struct {
	Query string `json:"query"`
	URL   string `json:"url"`
}

// Documented server-tool error codes the loop emits as
// protocol-conform error results — never upstream errors.
const (
	errCodeMaxUsesExceeded     = "max_uses_exceeded"
	errCodeInvalidToolInput    = "invalid_tool_input"
	errCodeURLNotInPriorCtx    = "url_not_in_prior_context"
	errCodeURLNotAllowed       = "url_not_allowed"
	errCodeURLNotAccessible    = "url_not_accessible"
	errCodeEmulatorUnavailable = "unavailable"
)

// executeWithEmulation runs executeOnce, then loops while the model
// answers with tool calls against emulated tool defs: each call is
// answered via the Emulator, the results ride back as a client-shaped
// assistant/tool_result exchange, and the request re-dispatches until
// the model answers without emulated calls.
//
// The client-facing response accumulates the synthesized server-tool
// blocks (server_tool_use + result, wire order preserved, other parts
// copied through) before the final answer, so the substitution is
// protocol-conform and visible — never silent (trace: EventEmulatedTool).
//
// Responses containing tool calls that are NOT emulated pass through
// untouched: those belong to the client, and the loop must not answer
// (or swallow) them.
func (e *Executor) executeWithEmulation(ctx context.Context, chain []ChainEntry, req ir.Request, state *visionToolState, trace *Trace) (ir.Response, error) {
	resp, err := e.executeOnce(ctx, chain, req, trace)
	if err != nil {
		return ir.Response{}, err
	}
	defs := emulatedToolDefs(req.Tools)
	if len(defs) == 0 {
		return resp, nil
	}

	// From here the request is an emulation loop, and it gets one time budget:
	// the round cap bounds how many times a model may call tools, this bounds
	// how long the request may spend doing it (ExecutorConfig
	// .EmulationTotalBudget). Set AFTER the early return, so an ordinary
	// request is never subject to an emulation deadline.
	loopCtx, cancelLoop := context.WithTimeout(ctx, e.cfg.EmulationTotalBudget)
	defer cancelLoop()
	ctx = loopCtx

	var synth []ir.Part      // client-facing blocks synthesized across rounds
	var usage ir.Usage       // honest cross-round token accounting
	uses := map[string]int{} // invocations per tool family (max_uses budget)

	for round := 0; ; round++ {
		usage = addUsage(usage, resp.Usage)

		// Split the response's tool calls: web-tool emulations (answered via
		// the emulator), the vision tool (answered via the vision fallbacks,
		// never client-facing, ADR-018), and foreign calls (the client's own).
		var emuCalls, imgCalls []ir.ToolCallPart
		foreign := false
		for _, part := range resp.Content {
			call, ok := part.(ir.ToolCallPart)
			if !ok {
				continue
			}
			switch {
			case call.Name == analyzeImageToolName && state != nil:
				imgCalls = append(imgCalls, call)
			default:
				if _, emulated := defs[call.Name]; emulated {
					emuCalls = append(emuCalls, call)
				} else {
					foreign = true
				}
			}
		}
		if len(emuCalls) == 0 && len(imgCalls) == 0 {
			// Nothing this loop answers — possibly a client tool call only.
			_ = foreign
			return finishEmulated(resp, synth, usage), nil
		}
		if foreign {
			if len(imgCalls) > 0 {
				// Mixed analyze_image + client tool calls: the client must
				// never see the injected tool, so the vision calls are
				// dropped (traced) and only the client's own calls surface;
				// the model re-issues them next round.
				var surfaced []ir.Part
				for _, part := range resp.Content {
					if call, ok := part.(ir.ToolCallPart); ok && call.Name == analyzeImageToolName {
						trace.AnalyzeImage("", "",
							"not answered: mixed with client tool calls; the client answers its own", 0, ir.Usage{})
						continue
					}
					surfaced = append(surfaced, part)
				}
				return finishEmulated(respWithContent(resp, surfaced), synth, usage), nil
			}
			// Mixed emulated + client tool calls: the client must answer
			// its own; surface the whole response unchanged.
			return finishEmulated(resp, synth, usage), nil
		}
		if round >= maxEmulationRounds {
			trace.EmulatedTool("emulation_loop", "",
				fmt.Sprintf("round cap %d reached; returning last upstream response", maxEmulationRounds), 0)
			return finishEmulated(resp, synth, usage), nil
		}
		// The other half of the safety net: the loop's own deadline (set when the
		// loop started). Everything synthesized so far is honest output, so the
		// request ends with it rather than with an error — the same posture the
		// round cap takes.
		if ctx.Err() != nil {
			trace.EmulatedTool("emulation_loop", "",
				"emulation budget exhausted ("+e.cfg.EmulationTotalBudget.String()+
					"); returning what the tools answered so far", 0)
			return finishEmulated(resp, synth, usage), nil
		}

		// Synthesize the client-facing blocks for this round in wire
		// order: each emulated call becomes server_tool_use + result;
		// other parts (text, thinking) copy through.
		var roundParts []ir.Part
		var results []ir.ToolResultPart
		for _, part := range resp.Content {
			call, ok := part.(ir.ToolCallPart)
			if !ok {
				roundParts = append(roundParts, part)
				continue
			}
			if call.Name == analyzeImageToolName && state != nil {
				tr, spent := e.answerAnalyzeImage(ctx, call, callConfig(defs[call.Name]), state, uses, trace)
				if tr.ToolUseID != "" {
					results = append(results, tr)
				}
				usage = addUsage(usage, spent)
				// No client-facing blocks: the tool belongs to no client.
				continue
			}
			st := callConfig(defs[call.Name])
			use, res, tr := e.answerEmulated(ctx, call, st, req, uses, trace)
			roundParts = append(roundParts, use, res)
			results = append(results, tr)
			// Honest accounting: every successful emulated invocation
			// counts against the request's web-request usage counters —
			// the client's bill must match the work actually done.
			if !res.IsError {
				switch call.Name {
				case "web_search":
					usage.WebSearchRequests++
				case "web_fetch":
					usage.WebFetchRequests++
				}
			}
		}
		synth = append(synth, roundParts...)

		// Re-dispatch: the model sees its own tool calls plus the
		// client-shaped tool_result answers (assistant + user exchange).
		req = appendRoundTrip(req, resp, results)
		resp, err = e.executeOnce(ctx, chain, req, trace)
		if err != nil {
			// The round-trip request itself failed; everything
			// synthesized so far is still honest output — but a partial
			// answer with a trailing upstream error cannot be a conform
			// response. Surface the error (failover discipline already
			// ran inside executeOnce).
			return ir.Response{}, err
		}
	}
}

// answerEmulated answers one emulated tool call: budget check (max_uses),
// input validation, the web_fetch prior-context and domain guards, then
// the emulator invocation. It returns the client-facing use/result pair
// plus the client-shaped tool result that rides the re-dispatch.
func (e *Executor) answerEmulated(ctx context.Context, call ir.ToolCallPart, st ir.ServerToolConfig, req ir.Request, uses map[string]int, trace *Trace) (ir.ServerToolUsePart, ir.ServerToolResultPart, ir.ToolResultPart) {
	use := ir.ServerToolUsePart(call)
	res := ir.ServerToolResultPart{ToolUseID: call.ID, ToolName: call.Name}

	// One invocation is one logged step, so the elapsed time covers everything
	// this call did — the guards, the account walk and the emulator's answer.
	started := e.cfg.now()
	elapsed := func() int64 { return e.cfg.now().Sub(started).Milliseconds() }

	fail := func(code, detail string) (ir.ServerToolUsePart, ir.ServerToolResultPart, ir.ToolResultPart) {
		res.IsError = true
		res.ErrorCode = code
		trace.EmulatedTool(call.Name, "", "error "+code+": "+detail, elapsed())
		tr := ir.ToolResultPart{ToolUseID: call.ID, IsError: true,
			Content: []ir.Part{ir.TextPart{Text: `{"error_code":` + strconvQuote(code) + `}`}}}
		return use, res, tr
	}

	// Degrade contract (ADR-015): no emulator configured (no Firecrawl
	// key) → emulated tool calls get the documented "unavailable" error
	// result, never a nil-pointer pass-through to the client.
	emu := e.currentEmulator()
	if emu == nil {
		return fail(errCodeEmulatorUnavailable, "web-tool emulation unavailable: no emulator configured")
	}

	// One budget for the whole invocation, however many accounts are configured:
	// the pool walks its accounts internally, so this bounds the walk — without
	// it, N accounts meant N × the client timeout inline on the request path
	// (ExecutorConfig.EmulationCallBudget).
	callCtx, cancelCall := context.WithTimeout(ctx, e.cfg.EmulationCallBudget)
	defer cancelCall()

	// max_uses budget: exceeded calls get the protocol-conform
	// error result, never an upstream error, and the loop continues so
	// the model can recover.
	if st.MaxUses > 0 && uses[call.Name] >= st.MaxUses {
		return fail(errCodeMaxUsesExceeded, "tool budget exhausted")
	}

	var in emulatedToolInput
	if err := json.Unmarshal(call.Input, &in); err != nil {
		return fail(errCodeInvalidToolInput, "input is not a JSON object")
	}

	switch call.Name {
	case "web_search":
		if strings.TrimSpace(in.Query) == "" {
			return fail(errCodeInvalidToolInput, "missing query")
		}
		items, by, err := searchVia(callCtx, emu, in.Query, st, trace)
		if err != nil {
			return fail(errCodeEmulatorUnavailable, "emulator search failed: "+callErrReason(callCtx, err, e.cfg.EmulationCallBudget))
		}
		res.Search = items
		uses[call.Name]++
		trace.EmulatedTool(call.Name, by, emuSummary(fmt.Sprintf("query %q → %d result(s)", in.Query, len(items)), by), elapsed())

	case "web_fetch":
		if strings.TrimSpace(in.URL) == "" {
			return fail(errCodeInvalidToolInput, "missing url")
		}
		// Security rule: web_fetch may only fetch
		// URLs that appeared in prior conversation context — never a URL
		// the model invented (SSRF / prompt-injection guard).
		if _, ok := priorContextURLs(req)[in.URL]; !ok {
			return fail(errCodeURLNotInPriorCtx, "url not present in prior conversation context")
		}
		allowed, code := domainAllowed(in.URL, st)
		if !allowed {
			return fail(code, "url rejected by domain filters")
		}
		fc, by, err := fetchVia(callCtx, emu, in.URL, st, trace)
		if err != nil {
			return fail(errCodeEmulatorUnavailable, "emulator fetch failed: "+callErrReason(callCtx, err, e.cfg.EmulationCallBudget))
		}
		// An account that has nothing to give hands back a non-nil page with
		// no content: Firecrawl answers HTTP 200 even for a page it could not
		// serve, and an all-empty account walk returns that page with err ==
		// nil. That is a FAILED fetch, and the reference enumerates it —
		// "url_not_accessible: Failed to fetch content (HTTP error)". Encoding
		// it as a success would emit a document block with no source.data, a
		// shape the reference never produces.
		//
		// The pool rule that an empty answer outranks a later failure is
		// untouched: it decides which ACCOUNT serves the call, not what the
		// client is told.
		if fc == nil || strings.TrimSpace(fc.Data) == "" {
			return fail(errCodeURLNotAccessible, "the page could not be retrieved")
		}
		// max_content_tokens bounds what enters the context (reference:
		// "If the fetched content exceeds this limit, the tool truncates
		// it"). Enforced here, at the single point where the page is
		// accepted, so it bounds BOTH the model-facing re-dispatch payload
		// and the result block the client receives — an unbounded page was
		// what pushed the upstream into an empty turn.
		//
		// The ratio is the repo's established 4 chars/token
		// (ir.EstimateTokens), which is also the ratio the reference's own
		// size examples use. The parameter is optional: zero means no limit,
		// and the limit applies to text, never to binary content.
		//
		// The budget is compared against the page BEFORE multiplying, and that
		// ordering is load-bearing: max_content_tokens arrives from the client,
		// and a value near MaxInt would overflow the ×4 into a NEGATIVE byte
		// limit, which truncateRunes would then use as a slice index — a panic
		// out of untrusted input. Bounding the comparison by len(fc.Data) keeps
		// the product small, and a budget at or above the page's own byte count
		// could only ever have been a no-op anyway.
		if st.MaxContentTokens > 0 && st.MaxContentTokens < len(fc.Data) {
			fc.Data = truncateRunes(fc.Data, st.MaxContentTokens*4)
		}
		res.Fetch = fc
		uses[call.Name]++
		trace.EmulatedTool(call.Name, by, emuSummary("fetched "+in.URL, by), elapsed())

	default:
		return fail(errCodeInvalidToolInput, "unknown emulated tool "+call.Name)
	}

	// The re-dispatched tool result carries a compact, schema-shaped JSON
	// of the emulator answer so the model can reason over it: the
	// Anthropic result schema (tool_use_id / tool name / url|query /
	// content) — NOT Go field names (ir.ServerToolResultPart's Go-field
	// JSON told the model nothing and invited URL rewrites feeding the
	// prior-context guard). Marshaled with HTML escaping OFF: a URL with
	// & must ride verbatim, or the guard's verbatim set and the model's
	// echo drift apart (the escaped form vs the decoded form).
	payload := resultPayloadForModel(call, res)
	// The operator must be able to read what this step actually sent and got
	// back — the parent request's capture shows the conversation, not the tool
	// exchange that answered a call inside it.
	trace.EmulatedToolPayload(call.Name, string(call.Input), payload)
	tr := ir.ToolResultPart{ToolUseID: call.ID,
		Content: []ir.Part{ir.TextPart{Text: payload}}}
	return use, res, tr
}

// searchVia and fetchVia call the emulator, using the pool's traced variants
// when the configured emulator IS a pool. The interface stays two methods (a
// single emulator has no account walk to report), and the pool records each
// account's outcome in the trace while telling the caller which one answered.
func searchVia(ctx context.Context, emu Emulator, query string, st ir.ServerToolConfig, trace *Trace) ([]ir.WebSearchResultItem, string, error) {
	if p, ok := emu.(*EmulatorPool); ok {
		return p.SearchTraced(ctx, query, st, trace)
	}
	items, err := emu.Search(ctx, query, st)
	return items, "", err
}

func fetchVia(ctx context.Context, emu Emulator, url string, st ir.ServerToolConfig, trace *Trace) (*ir.WebFetchContent, string, error) {
	if p, ok := emu.(*EmulatorPool); ok {
		return p.FetchTraced(ctx, url, st, trace)
	}
	page, err := emu.Fetch(ctx, url, st)
	return page, "", err
}

// emuSummary appends the account that answered, when the walk named one. The
// operator's question about a web tool is usually "why did it find nothing" —
// and WHICH account was asked is half of that answer.
func emuSummary(summary, account string) string {
	if account == "" {
		return summary
	}
	return summary + " via " + account
}

// callErrReason names a budget exhaustion for what it is. A bare
// "context deadline exceeded" in the log reads like an upstream failure, when in
// fact this proxy stopped waiting — the operator needs to tell those apart.
func callErrReason(ctx context.Context, err error, budget time.Duration) string {
	if errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
		return "web-tool call budget exhausted (" + budget.String() + "): " + err.Error()
	}
	return err.Error()
}

// resultPayloadForModel renders the result block the MODEL sees on the
// re-dispatch: the documented wire schema (tool_use_id, the search array
// or the fetch object), html-escape free. Error results
// keep the {"error_code":...} shape the loop's recovery path documents.
func resultPayloadForModel(call ir.ToolCallPart, res ir.ServerToolResultPart) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "")
	if res.IsError {
		_ = enc.Encode(map[string]string{"error_code": res.ErrorCode})
		return strings.TrimSpace(buf.String())
	}
	if res.ToolName == "web_search" {
		items := res.Search
		if items == nil {
			items = []ir.WebSearchResultItem{}
		}
		wire := struct {
			ToolUseID string           `json:"tool_use_id"`
			Content   []wireSearchItem `json:"content"`
		}{ToolUseID: call.ID, Content: make([]wireSearchItem, 0, len(items))}
		for _, it := range items {
			wire.Content = append(wire.Content, wireSearchItem{
				URL: it.URL, Title: it.Title, PageAge: it.PageAge,
			})
		}
		_ = enc.Encode(wire)
		return strings.TrimSpace(buf.String())
	}
	// web_fetch: the single result object (the wire asymmetry).
	fetch := struct {
		ToolUseID string        `json:"tool_use_id"`
		Content   wireFetchBody `json:"content"`
	}{ToolUseID: call.ID}
	if res.Fetch != nil {
		fetch.Content = wireFetchBody{
			URL:       res.Fetch.URL,
			Title:     res.Fetch.Title,
			Source:    mapSource(res.Fetch.Source),
			MediaType: mapMediaType(res.Fetch.MediaType),
			Data:      res.Fetch.Data,
		}
	}
	_ = enc.Encode(fetch)
	return strings.TrimSpace(buf.String())
}

// wireSearchItem is one content array entry of the model-facing
// web_search result (the documented url/title/page_age block).
type wireSearchItem struct {
	URL     string `json:"url"`
	Title   string `json:"title"`
	PageAge string `json:"page_age,omitempty"`
}

// wireFetchBody is the model-facing web_fetch document object (the
// documented source/media_type/text block, the document-block semantics).
type wireFetchBody struct {
	URL       string `json:"url,omitempty"`
	Title     string `json:"title,omitempty"`
	Source    string `json:"source,omitempty"`
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
}

// mapSource/mapMediaType translate the IR's field spellings onto the
// documented wire values ("text" → "text", "text/plain" → "text/plain").
func mapSource(s string) string {
	if s == "" {
		return "text"
	}
	return s
}

func mapMediaType(mt string) string {
	if mt == "" {
		return "text/plain"
	}
	return mt
}

// respWithContent returns resp with its content replaced (the mixed-corner
// filter works on a copy; the caller's response is not mutated).
func respWithContent(resp ir.Response, content []ir.Part) ir.Response {
	resp.Content = content
	return resp
}

// appendRoundTrip extends the request with the model's assistant turn
// and the synthesized tool-result answers (client-tool exchange shape:
// assistant tool calls → user tool_result message).
func appendRoundTrip(req ir.Request, resp ir.Response, results []ir.ToolResultPart) ir.Request {
	req.Messages = append(req.Messages, ir.Message{Role: ir.RoleAssistant, Parts: resp.Content})
	req.Messages = append(req.Messages, ir.Message{Role: ir.RoleUser, Parts: partsOf(results)})
	return req
}

// partsOf lifts tool results into a part slice.
func partsOf(results []ir.ToolResultPart) []ir.Part {
	out := make([]ir.Part, len(results))
	for i, r := range results {
		out[i] = r
	}
	return out
}

// finishEmulated assembles the client-facing response: synthesized
// blocks first (wire order: tool use/results, then the final answer),
// honest cross-round token usage, and the web-request counters.
func finishEmulated(resp ir.Response, synth []ir.Part, usage ir.Usage) ir.Response {
	// Honest cross-round accounting regardless of whether the round trips
	// synthesized client-facing blocks: an analyze_image exchange (ADR-018)
	// costs real calls and synthesizes nothing.
	resp.Usage = usage
	if len(synth) == 0 {
		return resp
	}
	resp.Content = append(append([]ir.Part{}, synth...), resp.Content...)
	return resp
}

// addUsage sums token counters across round-trip dispatches — every
// upstream call really consumed tokens, and the client's accounting must
// not under-report it.
func addUsage(a, b ir.Usage) ir.Usage {
	a.InputTokens += b.InputTokens
	a.OutputTokens += b.OutputTokens
	a.CacheCreationInputTokens += b.CacheCreationInputTokens
	a.CacheReadInputTokens += b.CacheReadInputTokens
	a.ReasoningTokens += b.ReasoningTokens
	a.WebSearchRequests += b.WebSearchRequests
	a.WebFetchRequests += b.WebFetchRequests
	return a
}

// emulatedToolDefs indexes the emulated tool definitions by family name.
func emulatedToolDefs(tools []ir.Tool) map[string]ir.Tool {
	defs := map[string]ir.Tool{}
	for _, t := range tools {
		if t.Emulated {
			defs[t.Name] = t
		}
	}
	return defs
}

// callConfig dereferences a def's knobs (nil → provider defaults).
func callConfig(t ir.Tool) ir.ServerToolConfig {
	if t.ServerTool == nil {
		return ir.ServerToolConfig{}
	}
	return *t.ServerTool
}

// urlPattern matches http(s) URLs embedded in conversation text.
var urlPattern = regexp.MustCompile(`https?://[^\s"'<>\\)\]]+`)

// trimTrailingPunct strips the sentence punctuation the URL pattern's
// "not whitespace" run can swallow — "fetch https://a/b." must not store
// (and then require) the trailing dot the model will never re-emit.
func trimTrailingPunct(u string) string {
	return strings.TrimRight(u, ".,;:!?")
}

// unescapeHTML normalizes the HTML entities json.Marshal's default
// escaping produces (the re-dispatch payload is marshaled WITHOUT
// SetEscapeHTML(false), so a URL like ?a=1&b=2 rides as &amp;b=2). The
// model echoes the decoded form — both spellings must collapse to one
// set member. Entity handling is minimal on purpose: the three escapes
// Go's encoder emits for text, no more.
func unescapeHTML(s string) string {
	if !strings.Contains(s, "&") {
		return s
	}
	r := strings.NewReplacer("&amp;", "&", "&lt;", "<", "&gt;", ">", "&#39;", "'", "&quot;", `"`)
	return r.Replace(s)
}

// priorContextURLs collects every URL embedded in prior conversation
// context — user messages, client tool results, prior search/fetch
// results — never model-generatable output or the system prompt (fixture
// security rule). Synthesized tool results ride user-role messages, so
// their URLs count for later fetches; on the NEXT client request the same
// results ride ASSISTANT content (the wire encodes them into the
// assistant turn), so assistant messages' tool-RESULT parts are scanned
// too — never assistant text (a URL the model invented in its own prose
// must stay un-fetchable — the SSRF/prompt-injection guard).
func priorContextURLs(req ir.Request) map[string]struct{} {
	set := map[string]struct{}{}
	add := func(s string) {
		for _, u := range urlPattern.FindAllString(unescapeHTML(s), -1) {
			set[trimTrailingPunct(u)] = struct{}{}
		}
	}
	for _, msg := range req.Messages {
		switch msg.Role {
		case ir.RoleSystem:
			continue
		}
		for _, part := range msg.Parts {
			switch p := part.(type) {
			case ir.TextPart:
				// Assistant text is model output — never context (the
				// guard's security rule); user/system text is.
				if msg.Role != ir.RoleAssistant {
					add(p.Text)
				}
			case ir.ImagePart:
				// Assistant-side images: an upstream echo of a prior user
				// image or a generated one — its URL is model-generatable
				// output, so only NON-assistant image sources count (same
				// security rule as TextPart above).
				if msg.Role != ir.RoleAssistant {
					add(p.URL)
				}
			case ir.ToolResultPart:
				for _, sub := range p.Content {
					if tp, ok := sub.(ir.TextPart); ok {
						add(tp.Text)
					}
				}
			case ir.ServerToolResultPart:
				// The synthesized blocks (assistant-side replay):
				// the search results' and the fetched page's own URLs.
				for _, item := range p.Search {
					add(item.URL)
				}
				if p.Fetch != nil {
					add(p.Fetch.URL)
				}
			}
		}
	}
	return set
}

// domainAllowed enforces the web_fetch allowed/blocked domain knobs
// (error code url_not_allowed). A malformed or non-http(s) URL is
// invalid input, not a domain verdict.
func domainAllowed(rawURL string, st ir.ServerToolConfig) (bool, string) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return false, errCodeInvalidToolInput
	}
	host := strings.ToLower(u.Hostname())
	if domainMatch(st.BlockedDomains, host) {
		return false, errCodeURLNotAllowed
	}
	if len(st.AllowedDomains) > 0 && !domainMatch(st.AllowedDomains, host) {
		return false, errCodeURLNotAllowed
	}
	return true, ""
}

// domainMatch reports whether host equals a listed domain or is a
// subdomain of one (case-insensitive).
func domainMatch(domains []string, host string) bool {
	for _, d := range domains {
		d = strings.ToLower(strings.TrimSpace(d))
		if d == "" {
			continue
		}
		if host == d || strings.HasSuffix(host, "."+d) {
			return true
		}
	}
	return false
}

// strconvQuote is json.Marshal for a single string without the import
// dance at every error-result call site.
func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
