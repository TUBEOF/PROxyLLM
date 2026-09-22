package webui

// ADR-010 admin surface: capability fallback lists + model sync config and
// manual run. Validation lives at this write boundary; the storage reads
// degrade to safe defaults (never de-route on store noise).

import (
	"context"
	"errors"
	"net/http"

	"github.com/tubeof/proxyllm/internal/storage"
)

// capabilityFallbacksDTO is the wire shape of the three fallback lists
// (Routing page): vision = MODEL IDs (routing identities, 2026-09-21
// automatic capabilities) tried by the executor after a real upstream
// capability gap; web_search / web_fetch = web_tool account IDs in pool
// order.
type capabilityFallbacksDTO struct {
	Vision    []string `json:"vision"`
	WebSearch []string `json:"web_search"`
	WebFetch  []string `json:"web_fetch"`
}

// getCapabilityFallbacks serves the stored lists (nil reads → empty
// arrays — the wire shape never carries nulls).
func (a *AdminAPI) getCapabilityFallbacks(w http.ResponseWriter, r *http.Request) {
	vision, err := a.store.GetVisionFallback(r.Context())
	if err != nil {
		writeStoreError(w, err)
		return
	}
	fallbacks, err := a.store.GetWebToolFallbacks(r.Context())
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, capabilityFallbacksDTO{
		Vision:    nonNil(vision),
		WebSearch: nonNil(fallbacks.Search),
		WebFetch:  nonNil(fallbacks.Fetch),
	})
}

// putCapabilityFallbacks validates and persists all three lists in one
// write. The web-tool lists are validated at the write boundary against
// web_tool-kind accounts (the storage layer is deliberately schema-blind);
// vision entries must be existing MODEL rows (2026-09-21 automatic
// capabilities — the executor resolves each ID to provider + wire name at
// reload) — a typo must fail the save, never silently skip at route time.
func (a *AdminAPI) putCapabilityFallbacks(w http.ResponseWriter, r *http.Request) {
	var req capabilityFallbacksDTO
	if err := decodeBody(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}

	if err := a.validateVisionFallback(r.Context(), req.Vision); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := a.validateWebToolList(r.Context(), req.WebSearch); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := a.validateWebToolList(r.Context(), req.WebFetch); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if err := a.store.PutVisionFallback(r.Context(), req.Vision); err != nil {
		writeStoreError(w, err)
		return
	}
	if err := a.store.PutWebToolFallbacks(r.Context(), storage.WebToolFallbacks{
		Search: req.WebSearch,
		Fetch:  req.WebFetch,
	}); err != nil {
		writeStoreError(w, err)
		return
	}
	if a.reloadFallbacks != nil {
		a.reloadFallbacks()
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// validateVisionFallback checks every entry against the models table
// (2026-09-21 automatic capabilities): a vision fallback entry is a MODEL
// ID (routing identity), not a provider account or type — the fallback
// names which MODEL serves images when the chain's model rejected them.
// Web-tool accounts cannot own model rows, so the old kind refusal is
// implied; a defensive type check keeps a hand-seeded odd row out.
func (a *AdminAPI) validateVisionFallback(ctx context.Context, list []string) error {
	if len(list) == 0 {
		return nil
	}
	for _, entry := range list {
		row, err := a.store.GetModel(ctx, entry)
		if err != nil {
			return errors.New("vision fallback entry " + entry + " is not a known model")
		}
		// Defensive: a model row on a web-tool account would route an
		// emulator backend into a chat fallback — refuse loudly.
		if p, err := a.store.GetProvider(ctx, row.ProviderID); err == nil {
			if desc, isType := providerTypeByID(p.Type); isType && desc.Kind == KindWebTool {
				return errors.New("vision fallback entry " + entry + " belongs to a web-tool provider, not a chat model")
			}
		}
	}
	return nil
}

// validateWebToolList checks that every entry is an existing account whose
// type is a web_tool kind (firecrawl accounts today) — the pool must only
// contain emulator backends.
func (a *AdminAPI) validateWebToolList(ctx context.Context, list []string) error {
	if len(list) == 0 {
		return nil
	}
	providers, err := a.store.ListProviders(ctx)
	if err != nil {
		return err
	}
	types := make(map[string]string, len(providers)) // id → type
	for _, p := range providers {
		types[p.ID] = p.Type
	}
	for _, entry := range list {
		typ, ok := types[entry]
		if !ok {
			return errors.New("web-tool fallback entry " + entry + " is not a known provider account")
		}
		desc, isType := providerTypeByID(typ)
		if !isType || desc.Kind != KindWebTool {
			return errors.New("web-tool fallback entry " + entry + " is not a web-tool account")
		}
	}
	return nil
}

// nonNil maps a nil slice to an empty one (wire shape: [] not null).
func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// modelSyncDTO is the wire shape of the sync config + last-run state.
type modelSyncDTO struct {
	Enabled       bool                   `json:"enabled"`
	IntervalHours int                    `json:"interval_hours"`
	LastRun       storage.ModelSyncState `json:"last_run"`
}

func (a *AdminAPI) getModelSync(w http.ResponseWriter, r *http.Request) {
	enabled, err := a.store.GetModelSyncEnabled(r.Context())
	if err != nil {
		writeStoreError(w, err)
		return
	}
	hours, err := a.store.GetModelSyncIntervalHours(r.Context())
	if err != nil {
		writeStoreError(w, err)
		return
	}
	state, err := a.store.GetModelSyncState(r.Context())
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, modelSyncDTO{
		Enabled:       enabled,
		IntervalHours: hours,
		LastRun:       state,
	})
}

// putModelSync persists the toggle + cadence and relies on the scheduler's
// per-tick re-read for the changes to take effect without restart.
func (a *AdminAPI) putModelSync(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled       *bool `json:"enabled"`
		IntervalHours *int  `json:"interval_hours"`
	}
	if err := decodeBody(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if req.Enabled != nil {
		if err := a.store.SetModelSyncEnabled(r.Context(), *req.Enabled); err != nil {
			writeStoreError(w, err)
			return
		}
	}
	if req.IntervalHours != nil {
		if err := a.store.SetModelSyncIntervalHours(r.Context(), *req.IntervalHours); err != nil {
			writeStoreError(w, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// runModelSync triggers one synchronous sync pass (the WebUI "Sync now"
// button). Unwired sync (no master key / test harness) → uniform 404, the
// admin-gate's degrade contract.
func (a *AdminAPI) runModelSync(w http.ResponseWriter, r *http.Request) {
	if a.syncNow == nil {
		writeError(w, http.StatusNotFound, "model sync is not wired")
		return
	}
	state, err := a.syncNow(r.Context())
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"last_run": state})
}
