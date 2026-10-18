package routing

// The analyze_image tool (ADR-018) and the proactive pre-dispatch image
// substitution it rides.
//
// One-shot captioning (ADR-014) answers the question the CLIENT happened to
// ask when the image arrived; the answering model sees second-hand prose and
// can never look again. This layer inverts the control: when every chain
// entry is known blind, each image becomes a placeholder text carrying a
// stable content ref plus the cached short caption, and the executor injects
// an emulated `analyze_image` tool. The routed model then sends image + its
// OWN prompt to the configured vision models, as often as the task needs —
// a Playwright screenshot can be asked for one detail at a time, on demand,
// inside the same request. The client never sees the tool: its calls are
// answered server-side, its blocks never reach the response.

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/tubeof/proxyllm/internal/translate/ir"
)

// analyzeImageToolName is the proxy's own tool surface. Unlike the web-tool
// families, nothing on any wire or in a client claims this name — it is
// injected by the executor and answered by the executor. It is therefore an
// interface constant, not a vendor fact.
const analyzeImageToolName = "analyze_image"

// analyzeImageMaxUses bounds how often one request's model may call the tool
// (the server-tool max_uses budget; the emulation loop enforces it).
const analyzeImageMaxUses = 8

// defaultVisionPlaceholderTemplate is the text an image becomes on an
// all-blind chain. {n} is the image's document ordinal (1-based), {ref} the
// stable content id, {caption} the bootstrap description. The wording is
// operator-facing behavior, so the template is overridable via the settings
// KV (vision_placeholder_template) — vendor-neutral text lives in
// configuration, not in constants.
const defaultVisionPlaceholderTemplate = "[Bild {n} (ref {ref}): {caption} — call the analyze_image tool with image_ref \"{ref}\" and your own prompt to inspect this image]"

// unavailableCaptionText stands in for {caption} when the bootstrap caption
// failed (no describer answered): the model must know the slot is empty for
// a reason, not that it saw nothing.
const unavailableCaptionText = "image content not available"

// analyzeImageToolDescription is what the routed model reads when deciding
// whether to call the tool. It must teach everything in one place: where the
// refs come from, what the prompt is for, and the one-call-at-a-time rule
// the emulation loop needs to keep the exchange invisible to the client.
const analyzeImageToolDescription = "Inspect an image that appears as \"[Bild N (ref xxxxxxxxxxxx): ...]\" in the conversation. Send one image_ref and your own prompt describing exactly what you need to know about that image (read text on it, locate an element, describe colors — anything). A vision model answers with only the answer to your prompt, so ask precisely. Call this tool ALONE, never alongside other tools, so its exchange can be answered inline; ask again with a different prompt for more detail."

// analyzeImageToolDef builds the tool definition the executor appends to a
// blind-chain request.
func analyzeImageToolDef() ir.Tool {
	schema, _ := json.Marshal(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"image_ref": map[string]any{
				"type":        "string",
				"description": "The content ref from the image's [Bild N (ref ...)] placeholder. \"N\" or \"Bild N\" also resolve to the Nth image.",
			},
			"prompt": map[string]any{
				"type":        "string",
				"description": "Your own instruction for what to extract from the image.",
			},
		},
		"required": []string{"image_ref", "prompt"},
	})
	return ir.Tool{
		Name:        analyzeImageToolName,
		Description: analyzeImageToolDescription,
		InputSchema: schema,
		ServerTool:  &ir.ServerToolConfig{MaxUses: analyzeImageMaxUses},
		Emulated:    true, // answered server-side; never surfaced to any client
	}
}

// analyzeImageInput mirrors the tool's input schema.
type analyzeImageInput struct {
	ImageRef string `json:"image_ref"`
	Prompt   string `json:"prompt"`
}

// visionToolState is the request-scoped store of every image the executor
// just substituted: ref id → the image's inline bytes. It exists only for the
// span of one Execute — the tool can only be called during the emulation
// rounds, and every image the model can reference arrived with the request it
// belongs to — so it is discarded with the request. No cross-request store:
// a content ref is stable because it is computed from the bytes, not because
// anything remembers it.
type visionToolState struct {
	images   map[string]ir.ImagePart
	ordinals map[string]string // "1", "2", ... → ref id
}

// resolve maps a model-supplied ref back to the image: the ref id itself, or
// the ordinal / "Bild N" form the placeholder also shows.
func (s *visionToolState) resolve(ref string) (ir.ImagePart, bool) {
	if s == nil {
		return ir.ImagePart{}, false
	}
	ref = strings.TrimSpace(ref)
	if img, ok := s.images[ref]; ok {
		return img, true
	}
	ordinal := strings.TrimSpace(strings.TrimPrefix(ref, "Bild"))
	if id, ok := s.ordinals[ordinal]; ok {
		img, ok := s.images[id]
		return img, ok
	}
	return ir.ImagePart{}, false
}

// analyzeImageRef derives the stable content id of an image: the first 12
// hex chars of the SHA-256 of its decoded bytes (the same digest the caption
// cache is keyed on). Stable across turns — the client re-sends the
// transcript, the bytes travel with it — and identical for an inline payload
// and a resolved-then-inlined URL of the same picture.
func analyzeImageRef(img ir.ImagePart) string {
	d := captionDigest(img.Data)
	id := d[:]
	return hex.EncodeToString(id[:6])
}

// renderImagePlaceholder fills the configured template for one image. An
// empty caption marks the boot strap's failure: the model gets the ref and
// the tool anyway — on an all-blind chain the raw image would be rejected
// for certain, so a placeholder that fails loudly beats one that never
// happens (the ADR-014 invariant it deliberately departs from).
func renderImagePlaceholder(cfg CaptionConfig, n int, ref, caption string) string {
	template := cfg.placeholderTemplate()
	if strings.TrimSpace(caption) == "" {
		caption = unavailableCaptionText
	}
	if strings.TrimSpace(ref) == "" {
		// No resolvable bytes behind this ordinal (an image the guarded
		// fetch refused): a tool pointer would set the model chasing an
		// image_ref that can never resolve. Say absence and stop there.
		return "[Bild " + strconv.Itoa(n) + ": " + caption +
			" — this image could not be loaded and cannot be inspected with analyze_image]"
	}
	text := strings.ReplaceAll(template, "{n}", strconv.Itoa(n))
	text = strings.ReplaceAll(text, "{ref}", ref)
	text = strings.ReplaceAll(text, "{caption}", caption)
	return text
}

func (c CaptionConfig) placeholderTemplate() string {
	if t := c.VisionPlaceholderTemplate; t != "" {
		return t
	}
	return defaultVisionPlaceholderTemplate
}

func (c CaptionConfig) visionToolEnabled() bool {
	return c.VisionTool == nil || *c.VisionTool
}

// substituteForKnownBlind is the pre-dispatch half of the vision workflow
// (ADR-018's decision matrix): images on a chain whose entries are ALL known
// blind never reach the dispatch — the rejection the old flow waited for is
// certain (and a hung upstream may never even send it, which is exactly the
// failure this feature fixes). It describes every image (cache first, then
// the configured vision models), replaces each with its placeholder, and —
// when enabled — injects the analyze_image tool def onto the request.
//
// The returned usage is the caption spend (cached images cost nothing); the
// caller folds it into the response's usage. The returned state is what the
// emulation loop needs to resolve analyze_image calls; nil state means no
// substitution happened and no tool was injected.
//
// Chains with any entry of unknown or sighted vision capability dispatch as
// they came (pass-through): the router's preference gate already ordered the
// sighted candidates first, and the reactive ADR-014 rescue stays the safety
// net for a sighted-looking entry that rejects after all.
func (e *Executor) substituteForKnownBlind(ctx context.Context, chain []ChainEntry, req ir.Request, trace *Trace) (ir.Request, ir.Usage, *visionToolState) {
	if e.cfg.ModelVisionLookup == nil || len(chain) == 0 {
		return req, ir.Usage{}, nil
	}
	for _, entry := range chain {
		canSee, known := e.cfg.ModelVisionLookup(ctx, entry.ProviderID, entry.Model)
		if !known || canSee {
			return req, ir.Usage{}, nil // pass-through: not every entry is known-blind
		}
	}
	refs := req.ImageRefs()
	if len(refs) == 0 {
		return req, ir.Usage{}, nil
	}
	models := e.currentCapabilityFallbacks()
	if len(models) == 0 {
		// Nothing could describe anything: the request goes out as-is and the
		// upstream's own capability_gap surfaces honestly (the day's case 4).
		return req, ir.Usage{}, nil
	}
	cfg := e.currentCaptionConfig()

	// Task hint: unchanged semantics from the caption cache's key — the
	// client's current question steers the bootstrap description.
	hint := ""
	if cfg.taskAware() {
		hint = truncateRunes(strings.TrimSpace(lastUserText(req)), captionTaskHintLimit)
	}
	prompt := composeCaptionPrompt(cfg.prompt(), hint, cfg.taskAware())
	version := captionVersion(cfg.prompt(), models)
	clientKey := clientKeyIDFromContext(ctx)

	// Resolve every image to inline bytes once: the fetch is what both the
	// dedupe (content ref) and the describer need.
	inlines := make([]*ir.ImagePart, len(refs))
	for i, ref := range refs {
		inline, err := resolveImageForCaption(ctx, ref.Image)
		if err != nil {
			trace.CapabilityFallback("vision", "image left unplumbed for substitution: "+err.Error())
			continue
		}
		inlines[i] = &inline
	}

	// Describe each image: cache first, then the describer walk.
	texts := make([]string, len(refs))
	var usage ir.Usage
	state := &visionToolState{
		images:   make(map[string]ir.ImagePart, len(refs)),
		ordinals: make(map[string]string, len(refs)),
	}
	for i := range refs {
		if inlines[i] == nil {
			continue // unresolvable: the placeholder still replaces it (case 3)
		}
		img := *inlines[i]
		refID := analyzeImageRef(img)
		state.images[refID] = img
		state.ordinals[strconv.Itoa(i+1)] = refID

		key := captionImageKey(clientKey, version, hint, img)
		if entry, ok := e.captions.get(key); ok {
			texts[i] = entry.text
			trace.ImageCaptionCached(entry.providerID, entry.model, len(entry.text))
			continue
		}
		started := e.cfg.now()
		text, entry, spent, err := e.captionImage(ctx, models, img, prompt, cfg)
		latency := e.cfg.now().Sub(started).Milliseconds()
		usage = addUsage(usage, spent)
		if err != nil {
			if entry.ProviderID != "" {
				trace.Skip(entry.ProviderID, "image caption failed: "+err.Error())
			}
			continue // no text: the unavailability placeholder stands in
		}
		texts[i] = text
		e.captions.put(key, &captionEntry{key: key, text: text,
			providerID: entry.ProviderID, model: entry.Model})
		trace.ImageCaption(entry.ProviderID, entry.Model, len(text), latency, spent)
		trace.CaptionPayload(entry.ProviderID, entry.Model, prompt,
			img.MediaType, img.DecodedBytes(), text)
	}

	// Compose the replacement texts and swap the images out. An image whose
	// caption failed substitutes the unavailability text (renderImage
	// renders it when the text is empty) — on an all-blind chain the raw
	// image can only be rejected, so the placeholder must exist either way.
	descs := make([]*string, len(refs))
	for i := range refs {
		place := renderImagePlaceholder(cfg, i+1, state.ordinals[strconv.Itoa(i+1)], texts[i])
		descs[i] = &place
	}
	req = req.ReplaceImages(refs, descs)

	if !cfg.visionToolEnabled() {
		trace.CapabilityFallback("vision", strconv.Itoa(len(refs))+" image(s) substituted pre-dispatch (vision_tool off; captions only)")
		return req, usage, nil // no tool, no state to resolve one with
	}

	// The tool rides the substitution: without it the model could still read
	// the bootstrap captions but could never ask its own questions.
	for _, t := range req.Tools {
		if t.Name == analyzeImageToolName {
			// A client-declared analyze_image owns the name: no second def
			// (dupes 400 on OpenAI-compatible upstreams) and no interception —
			// the loop only answers calls against OUR emulated def.
			trace.CapabilityFallback("vision",
				"analyze_image already declared by the client; injection skipped")
			return req, usage, nil
		}
	}
	tools := make([]ir.Tool, len(req.Tools))
	copy(tools, req.Tools)
	req.Tools = append(tools, analyzeImageToolDef())
	trace.CapabilityFallback("vision",
		strconv.Itoa(len(refs))+" image(s) substituted pre-dispatch; analyze_image injected")

	return req, usage, state
}

// answerAnalyzeImage answers one analyze_image call: resolve the ref against
// the request-scoped image store, then hand the image plus the model's OWN
// prompt to the configured vision models (the same walk the bootstrap caption
// uses). Answers are cached — the tool prompt is the cache's task, so an
// identical ask on a later turn (or a re-issued identical call) is free and a
// changed ask re-describes, mirroring the caption cache's 2026-10-17 rule.
//
// The result is always a protocol-conform model-facing tool result: error
// inputs and unavailable backends answer in-band, never as an upstream error,
// so the emulation loop can continue and the model can recover.
func (e *Executor) answerAnalyzeImage(ctx context.Context, call ir.ToolCallPart, st ir.ServerToolConfig, state *visionToolState, uses map[string]int, trace *Trace) (ir.ToolResultPart, ir.Usage) {
	started := e.cfg.now()
	elapsed := func() int64 { return e.cfg.now().Sub(started).Milliseconds() }

	fail := func(code, detail string) (ir.ToolResultPart, ir.Usage) {
		trace.AnalyzeImage("", "", "error "+code+": "+detail, elapsed(), ir.Usage{})
		return ir.ToolResultPart{ToolUseID: call.ID, IsError: true,
			Content: []ir.Part{ir.TextPart{Text: `{"error_code":` + strconvQuote(code) + `}`}}}, ir.Usage{}
	}

	if state == nil {
		return fail(errCodeEmulatorUnavailable, analyzeImageToolName+" ran without a substitution state")
	}
	// max_uses budget: like the web tools, exceeded calls get the conform
	// error result and the loop continues so the model can recover.
	if st.MaxUses > 0 && uses[analyzeImageToolName] >= st.MaxUses {
		return fail(errCodeMaxUsesExceeded, "tool budget exhausted")
	}

	var in analyzeImageInput
	if err := json.Unmarshal(call.Input, &in); err != nil {
		return fail(errCodeInvalidToolInput, "input is not a JSON object")
	}
	img, ok := state.resolve(in.ImageRef)
	if !ok {
		return fail(errCodeInvalidToolInput,
			"unknown image_ref: "+truncateRunes(strings.TrimSpace(in.ImageRef), 80))
	}
	prompt := strings.TrimSpace(in.Prompt)
	if prompt == "" {
		return fail(errCodeInvalidToolInput, "missing prompt")
	}

	models := e.currentCapabilityFallbacks()
	if len(models) == 0 {
		return fail(errCodeEmulatorUnavailable,
			"no configured vision fallback model to answer "+analyzeImageToolName)
	}
	cfg := e.currentCaptionConfig()

	// Cache first: the tool's prompt is the cache's task dimension.
	version := captionVersion(cfg.prompt(), models)
	// The FULL prompt enters the key: model-authored asks are deliberately
	// precise and can be long, and two asks sharing a 500-rune prefix must
	// not serve each other's answers. The hash makes truncation pointless —
	// hashing a truncated copy only bought collisions.
	key := captionImageKey(clientKeyIDFromContext(ctx), version, prompt, img)
	if entry, hit := e.captions.get(key); hit {
		uses[analyzeImageToolName]++
		trace.AnalyzeImage(entry.providerID, entry.model,
			"image_ref "+strings.TrimSpace(in.ImageRef)+"; cached answer", elapsed(), ir.Usage{})
		return analyzeImageAnswer(call, strings.TrimSpace(in.ImageRef), entry.text), ir.Usage{}
	}

	// One budget for the whole invocation across the describer walk, exactly
	// like a web-tool account walk (ExecutorConfig.EmulationCallBudget).
	callCtx, cancel := context.WithTimeout(ctx, e.cfg.EmulationCallBudget)
	defer cancel()
	text, entry, spent, err := e.captionImage(callCtx, models, img, prompt, cfg)
	if err != nil {
		return fail(errCodeEmulatorUnavailable,
			"vision fallback failed: "+callErrReason(callCtx, err, e.cfg.EmulationCallBudget))
	}
	e.captions.put(key, &captionEntry{key: key, text: text,
		providerID: entry.ProviderID, model: entry.Model})
	uses[analyzeImageToolName]++
	trace.AnalyzeImage(entry.ProviderID, entry.Model,
		"image_ref "+strings.TrimSpace(in.ImageRef)+"; prompt "+strconv.Itoa(len(prompt))+" chars answered",
		elapsed(), spent)
	trace.AnalyzeImagePayload(string(call.Input), text)
	return analyzeImageAnswer(call, strings.TrimSpace(in.ImageRef), text), spent
}

// analyzeImageResult is the tool_result payload the model reads back: the
// answer plus enough identity to correlate with the ask (the documented wire
// schema; html-escape free, like the web tools'). Errors keep the loop's
// {"error_code"} shape instead.
type analyzeImageResult struct {
	ToolUseID string `json:"tool_use_id"`
	ImageRef  string `json:"image_ref"`
	Analysis  string `json:"analysis"`
}

func analyzeImageAnswer(call ir.ToolCallPart, ref, analysis string) ir.ToolResultPart {
	wire := analyzeImageResult{ToolUseID: call.ID, ImageRef: ref, Analysis: analysis}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(wire)
	return ir.ToolResultPart{ToolUseID: call.ID,
		Content: []ir.Part{ir.TextPart{Text: strings.TrimSpace(buf.String())}}}
}
