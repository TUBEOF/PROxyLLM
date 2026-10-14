package server

// Capability-fallback wiring (ADR-010, with automatic capabilities):
// the stored vision_fallback KV holds MODEL IDs (routing
// identities, "ollamacloud/glm-5.3"); the executor needs fully resolved
// (provider, wire-model) chain entries. This file owns that translation:
// store.GetModel per ID → ChainEntry{ProviderID, Model: row.Name}, with
// last-known-good semantics — unknown IDs are skipped with a log line
// (v1.0.2 lists of provider IDs/Types do not resolve as model rows and are
// dropped until the operator re-saves), a store read failure leaves the
// live fallback untouched. Learning rides the same pair: the executor's
// OnCapabilityGap carries the chain entry, the storage layer locates the
// row by (provider, wire name).

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/tubeof/proxyllm/internal/routing"
	"github.com/tubeof/proxyllm/internal/storage"
)

// Image-caption settings keys (ADR-014). They live in the same config KV as
// every other operator setting, so the generic settings API can write them
// without a bespoke endpoint.
const (
	captionPromptKey    = "vision_caption_prompt"
	captionTaskAwareKey = "vision_caption_task_aware"
	captionTimeoutMSKey = "vision_caption_timeout_ms"
)

// resolveCaptionConfig reads the caption settings, keeping the built-in default
// for anything absent or malformed. Store noise must never change how a request
// is described — the same contract as every other settings read here.
func resolveCaptionConfig(ctx context.Context, store *storage.Store) routing.CaptionConfig {
	var cfg routing.CaptionConfig

	if v, ok, err := store.GetConfigValue(ctx, captionPromptKey); err == nil && ok && strings.TrimSpace(v) != "" {
		cfg.Prompt = v
	}
	// Absent keeps the default (task-aware on), so only an explicit "0" turns
	// the question hint off.
	if v, ok, err := store.GetConfigValue(ctx, captionTaskAwareKey); err == nil && ok {
		on := strings.TrimSpace(v) != "0"
		cfg.TaskAware = &on
	}
	if v, ok, err := store.GetConfigValue(ctx, captionTimeoutMSKey); err == nil && ok {
		if ms, perr := strconv.Atoi(strings.TrimSpace(v)); perr == nil && ms > 0 {
			cfg.Timeout = time.Duration(ms) * time.Millisecond
		}
	}
	return cfg
}

// resolveCapabilityFallbacks translates the stored vision_fallback model
// IDs into executor chain entries. Unknown model IDs are skipped with a
// log line (never a de-route of the whole list); the store read failure is
// returned for the caller's last-known-good policy.
//
// Since ADR-014 these entries name the models that DESCRIBE an image; they
// never receive the conversation, so nothing about their context window
// belongs here. The model row's declared window stays a property of the row
// (the model sync keeps writing it) — it simply no longer gates the rescue.
func resolveCapabilityFallbacks(ctx context.Context, store *storage.Store, logger *slog.Logger) ([]routing.ChainEntry, error) {
	ids, err := store.GetVisionFallback(ctx)
	if err != nil {
		return nil, err
	}
	entries := make([]routing.ChainEntry, 0, len(ids))
	for _, id := range ids {
		row, err := store.GetModel(ctx, id)
		if err != nil {
			// The log rides the redacting handler (all logging does); the
			// model ID itself is operator data, not a secret.
			if logger != nil {
				logger.Warn("capability fallback entry is not a known model — skipping", "model", id)
			}
			continue
		}
		entries = append(entries, routing.ChainEntry{
			ProviderID: row.ProviderID,
			Model:      row.Name,
		})
	}
	return entries, nil
}

// resolveWebToolFallbacks orders the emulator pool per the stored
// web_search_fallback / web_fetch_fallback KVs (ADR-010 decision 3): each
// entry is a WEB-TOOL ACCOUNT ID resolved to its emulator among the just-
// loaded accounts. Unknown IDs are skipped with a warn (same
// last-known-good contract as resolveCapabilityFallbacks; the Put path
// already validated kind membership — a stale row can only exist from a
// pre-validation database or a since-deleted account). An empty/unresolvable
// list falls back to the account insertion order (the pre-list behavior).
// The store read failure is returned; callers keep the live pool.
func resolveWebToolFallbacks(ctx context.Context, store *storage.Store, logger *slog.Logger, emus []NamedEmulator) (search, fetch []NamedEmulator, err error) {
	all := map[string]routing.Emulator{}
	for _, e := range emus {
		all[e.ID] = e.Emulator
	}
	insertion := append([]NamedEmulator(nil), emus...)

	lists, err := store.GetWebToolFallbacks(ctx)
	if err != nil {
		return insertion, insertion, err
	}
	order := func(ids []string) []NamedEmulator {
		out := make([]NamedEmulator, 0, len(ids))
		for _, id := range ids {
			emu, ok := all[id]
			if !ok {
				if logger != nil {
					logger.Warn("web-tool fallback entry is not a known account — skipping", "account", id)
				}
				continue
			}
			out = append(out, NamedEmulator{ID: id, Emulator: emu})
		}
		return out
	}
	s, f := order(lists.Search), order(lists.Fetch)
	if len(s) == 0 {
		s = insertion
	}
	if len(f) == 0 {
		f = insertion
	}
	return s, f, nil
}

// learnTimeout bounds one learning write; a slow store must not pile up
// goroutines behind the failover path.
const learnTimeout = 5 * time.Second

// learnConcurrency caps the in-flight learning writes: a sustained flood
// of capability-gap requests must not grow goroutines (each holds a store
// connection for up to learnTimeout) without bound. The cap is generous —
// 8 covers a chain storm; excess observations are DROPPED, not queued:
// learning is opportunistic (the next gap observation re-writes the same
// fact), so skipping is always safe.
const learnConcurrency = 8

// learnSlots is the bounded semaphore for learnCapabilityGap.
var learnSlots = make(chan struct{}, learnConcurrency)

// learnCapabilityGap persists one upstream capability-gap observation into
// the model row (vision=false learning). It is the executor's
// OnCapabilityGap callback: it runs on its own goroutine with a bounded
// context so the failover path never blocks on the store; in-flight writes
// are capped (excess gaps are dropped — the fact re-observes itself on the
// next gap), and failures are logged, never surfaced into the request.
func (s *Server) learnCapabilityGap(entry routing.ChainEntry) {
	select {
	case learnSlots <- struct{}{}:
	default:
		return // write slot busy — drop this observation (opportunistic learning)
	}
	go func() {
		defer func() { <-learnSlots }()
		ctx, cancel := context.WithTimeout(context.Background(), learnTimeout)
		defer cancel()
		if err := s.store.MergeLearnedCapability(ctx, entry.ProviderID, entry.Model, "vision", false); err != nil {
			s.logger.Error("capability-gap learning failed (model row left unchanged)",
				"provider", entry.ProviderID, "error", err)
		}
	}()
}
