package routing

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tubeof/proxyllm/internal/httpguard"
	"github.com/tubeof/proxyllm/internal/translate/ir"
)

// Image-to-text captioning (ADR-014): the rescue for a chain that cannot take
// image input.
//
// The design it replaces re-sent the whole conversation to a vision model as
// the fallback. That ties the rescue to the fallback model's context window —
// and the case the rescue exists for is exactly a conversation too large for
// it, so a long agentic session that grows a screenshot mid-flight could not
// be rescued at all. Captioning inverts the dependency: the vision model sees
// one image and the instruction that goes with it, nothing else, so its window
// never enters the question, and the model that answers is still the one the
// operator routed to.

const (
	// defaultCaptionPrompt asks for what a code screenshot is usually opened
	// for: a short, specific description. "In 2-3 sentences" is deliberate —
	// an unbounded description competes with the conversation for context and
	// arrives in every subsequent turn, because the caption replaces the image
	// in the request the client re-sends.
	defaultCaptionPrompt = "Describe this image concisely in 2-3 sentences. Focus on the most relevant visual details."

	// defaultCaptionTimeout bounds one caption call. A vision model that has
	// not answered in this window is not going to rescue the request in time
	// to be worth the client's wait.
	defaultCaptionTimeout = 30 * time.Second

	// defaultCaptionMaxTokens caps a single description.
	defaultCaptionMaxTokens = 1024

	// captionTaskHintLimit bounds the client text copied into the caption
	// prompt. The prompt is a steering hint, not a transcript of the request:
	// past a couple of sentences the extra text stops narrowing the
	// description and starts inviting the model to answer the question itself.
	captionTaskHintLimit = 500

	// captionTaskHintTail is appended, with the client's last turn, so the
	// description targets what was actually asked instead of being generically
	// thorough. Visible-text transcription is called out because a screenshot
	// is usually attached for the text in it.
	captionTaskHintTail = " Focus your description on what is relevant to answering this, and transcribe any text visible in the image."

	// captionImageFetchTimeout bounds downloading a remote image so the
	// describer can be handed inline bytes.
	captionImageFetchTimeout = 15 * time.Second

	// captionImageMaxBytes caps a downloaded image. An image larger than this
	// is refused rather than inlined: the caption call would be spending the
	// client's latency on a payload no describer needs at full resolution.
	captionImageMaxBytes = 10 << 20 // 10 MiB
)

// CaptionConfig tunes the captioning step. The zero value is valid and yields
// the defaults above.
type CaptionConfig struct {
	// Prompt is the base instruction the vision model receives.
	Prompt string

	// TaskAware appends the client's most recent text turn to the prompt.
	// nil means enabled — the description should answer the question that came
	// with the image, not describe it in the abstract.
	TaskAware *bool

	// Timeout bounds one caption call.
	Timeout time.Duration

	// MaxTokens caps a single caption.
	MaxTokens int
}

func (c CaptionConfig) prompt() string {
	if c.Prompt == "" {
		return defaultCaptionPrompt
	}
	return c.Prompt
}

func (c CaptionConfig) taskAware() bool {
	return c.TaskAware == nil || *c.TaskAware
}

func (c CaptionConfig) timeout() time.Duration {
	if c.Timeout <= 0 {
		return defaultCaptionTimeout
	}
	return c.Timeout
}

func (c CaptionConfig) maxTokens() int {
	if c.MaxTokens <= 0 {
		return defaultCaptionMaxTokens
	}
	return c.MaxTokens
}

// captionImages describes every image in the request and returns the request
// with each image replaced by its description, plus the token spend of the
// caption calls so the caller can account for it.
//
// ok is false when the request holds no images, no vision fallback model is
// configured, or no image could be described — the caller then surfaces the
// original capability-gap error rather than a side effect of a rescue that
// never happened.
//
// A per-image failure keeps that image in place instead of standing in a
// placeholder. The request is then still valid for a vision-capable attempt,
// and the worst case is an unchanged request, never a silently impoverished
// one.
func (e *Executor) captionImages(ctx context.Context, req ir.Request, trace *Trace) (ir.Request, ir.Usage, bool) {
	refs := req.ImageRefs()
	if len(refs) == 0 {
		return req, ir.Usage{}, false
	}
	models := e.currentCapabilityFallbacks()
	if len(models) == 0 {
		return req, ir.Usage{}, false
	}
	cfg := e.currentCaptionConfig()

	prompt := composeCaptionPrompt(cfg.prompt(), lastUserText(req), cfg.taskAware())

	descs := make([]*string, len(refs))
	var usage ir.Usage
	captioned := 0
	for i, ref := range refs {
		inline, rerr := resolveImageForCaption(ctx, ref.Image)
		if rerr != nil {
			// The describer cannot be handed this image. Saying so is the only
			// honest option: a provider that silently drops an image part
			// would leave the vision model describing something it never
			// received, and the answer would be built on that fiction.
			trace.CapabilityFallback("vision", "image left in place: "+rerr.Error())
			continue
		}
		text, entry, spent, err := e.captionImage(ctx, models, inline, prompt, cfg)
		usage = addUsage(usage, spent)
		if err != nil {
			if entry.ProviderID != "" {
				trace.Skip(entry.ProviderID, "image caption failed: "+err.Error())
			}
			continue
		}
		descs[i] = &text
		captioned++
		// The operator must be able to see what the answering model was told
		// the image showed (ADR-014): the trace event is the timeline entry,
		// the payload is the detail view — the instruction, the image's type
		// and size, and the caption itself.
		trace.ImageCaption(entry.ProviderID, entry.Model, len(text))
		trace.CaptionPayload(entry.ProviderID, entry.Model, prompt,
			inline.MediaType, inline.DecodedBytes(), text)
	}

	if captioned == 0 {
		trace.CapabilityFallback("vision",
			"chain exhausted on capability gaps; no configured vision model could describe the image")
		return req, usage, false
	}
	trace.CapabilityFallback("vision",
		"chain exhausted on capability gaps; described "+strconv.Itoa(captioned)+" image(s) and retrying the chain with the text in place of the image")
	return req.ReplaceImages(refs, descs), usage, true
}

// resolveImageForCaption returns the image in the form the describer must be
// handed: an inline base64 payload. A part that already carries bytes passes
// through; a remote URL is downloaded and inlined.
//
// The download is not a convenience. Wire encoders differ in what they carry —
// one drops a URL-sourced image outright — so handing the describer a URL means
// some providers receive no image at all and describe the void. Inlining
// removes that question. The fetch goes through the guarded client (private
// ranges and redirects refused), the same guard every other operator- or
// request-influenced outbound path uses.
func resolveImageForCaption(ctx context.Context, img ir.ImagePart) (ir.ImagePart, error) {
	if img.Source != "url" || img.URL == "" {
		return img, nil
	}

	u, err := url.Parse(img.URL)
	if err != nil {
		return img, fmt.Errorf("unparseable image URL")
	}
	if err := httpguard.GuardLocation(ctx, u); err != nil {
		return img, fmt.Errorf("image URL refused: %w", err)
	}

	fetchCtx, cancel := context.WithTimeout(ctx, captionImageFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, img.URL, nil)
	if err != nil {
		return img, fmt.Errorf("image request: %w", err)
	}
	resp, err := httpguard.NewClient(captionImageFetchTimeout).Do(req)
	if err != nil {
		return img, fmt.Errorf("image fetch: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return img, fmt.Errorf("image fetch: HTTP %d", resp.StatusCode)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, captionImageMaxBytes+1))
	if err != nil {
		return img, fmt.Errorf("image read: %w", err)
	}
	if len(data) > captionImageMaxBytes {
		return img, fmt.Errorf("image exceeds %d bytes", captionImageMaxBytes)
	}

	mediaType := resp.Header.Get("Content-Type")
	if i := strings.IndexByte(mediaType, ';'); i >= 0 {
		mediaType = mediaType[:i]
	}
	if mediaType = strings.TrimSpace(mediaType); mediaType == "" {
		// Fall back to whatever the client labelled it with.
		mediaType = img.MediaType
	}
	return ir.ImagePart{
		Source:    "base64",
		MediaType: mediaType,
		Data:      base64.StdEncoding.EncodeToString(data),
		Detail:    img.Detail,
	}, nil
}

// captionImage describes one image, trying the configured vision models in
// order until one answers. Each model receives only the image and the
// instruction — never the conversation.
func (e *Executor) captionImage(ctx context.Context, models []ChainEntry, img ir.ImagePart, prompt string, cfg CaptionConfig) (string, ChainEntry, ir.Usage, error) {
	if err := ctx.Err(); err != nil {
		return "", ChainEntry{}, ir.Usage{}, err
	}

	fragment := ir.Request{
		Messages:  []ir.Message{{Role: ir.RoleUser, Parts: []ir.Part{img, ir.TextPart{Text: prompt}}}},
		MaxTokens: cfg.maxTokens(),
	}

	var lastErr error
	var lastEntry ChainEntry
	for _, entry := range models {
		p, ok := e.cfg.Registry.Get(entry.ProviderID)
		if !ok {
			continue
		}
		lastEntry = entry

		callCtx, cancel := context.WithTimeout(ctx, cfg.timeout())
		resp, err := p.Complete(callCtx, withModel(fragment, entry.Model))
		cancel()

		if err == nil && textOfResponse(resp) != "" {
			// Health and quota accounting ride the same tracker as the chain
			// attempts: a vision model that is down must be learnable, or the
			// next request pays the same timeout again.
			e.reportSuccess(entry.ProviderID)
			return textOfResponse(resp), entry, resp.Usage, nil
		}
		if err == nil {
			err = errors.New("the vision model returned no text")
		}
		lastErr = err
		e.reportFailure(entry.ProviderID)
		e.reportQuota(entry.ProviderID, err, ClassifyError(err, PreStream))
	}

	if lastErr == nil {
		lastErr = errors.New("no configured vision fallback model is registered")
	}
	return "", lastEntry, ir.Usage{}, lastErr
}

// composeCaptionPrompt builds the instruction the vision model receives. With
// taskAware the client's own last turn is appended and the wording turns
// towards answering it: a generic description of a screenshot tends to report
// the window chrome and miss the error line the client actually asked about.
func composeCaptionPrompt(base, lastUserText string, taskAware bool) string {
	text := strings.TrimSpace(lastUserText)
	if !taskAware || text == "" {
		return base
	}
	return base + "\n\nThe user asked: \"" + truncateRunes(text, captionTaskHintLimit) + "\"." + captionTaskHintTail
}

// truncateRunes shortens s to at most limit bytes, never splitting a rune, and
// marks the cut.
func truncateRunes(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// lastUserText returns the text of the client's most recent user turn — the
// question the image is meant to help answer.
func lastUserText(req ir.Request) string {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		msg := req.Messages[i]
		if msg.Role != ir.RoleUser {
			continue
		}
		if text := joinTextParts(msg.Parts); text != "" {
			return text
		}
	}
	return ""
}

// joinTextParts concatenates a message's text parts.
func joinTextParts(parts []ir.Part) string {
	var b strings.Builder
	for _, part := range parts {
		t, ok := part.(ir.TextPart)
		if !ok || t.Text == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(t.Text)
	}
	return b.String()
}

// textOfResponse concatenates a response's text parts — a caption is whatever
// prose the vision model produced.
func textOfResponse(resp ir.Response) string {
	return strings.TrimSpace(joinTextParts(resp.Content))
}

// addUsageToStream passes a stream through, folding the caption calls' token
// spend into the terminal usage. Every upstream call really consumed tokens,
// and the client's accounting must not under-report it — the captions are
// billed to the request that needed them.
//
// The passthrough runs on its own goroutine, so it honors ctx on both sides:
// a caller that stops reading releases it rather than leaving it parked on a
// send that will never be received.
func addUsageToStream(ctx context.Context, in <-chan ir.StreamEvent, extra ir.Usage) <-chan ir.StreamEvent {
	out := make(chan ir.StreamEvent) // unbuffered: backpressure to the caller
	go func() {
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-in:
				if !ok {
					return
				}
				if ev.Type == ir.MessageDelta && ev.MessageDelta != nil {
					md := *ev.MessageDelta
					md.Usage = addUsage(md.Usage, extra)
					ev.MessageDelta = &md
				}
				select {
				case out <- ev:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out
}
