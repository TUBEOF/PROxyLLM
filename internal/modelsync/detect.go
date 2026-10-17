package modelsync

import (
	"encoding/json"

	"github.com/tubeof/proxyllm/internal/provider"
	"github.com/tubeof/proxyllm/internal/storage"
)

// Context auto-detect persistence: detected
// facts from provider discovery persist into model rows. The persistence
// contract mirrors ADR-010's operator-edit survival — detection fills
// gaps, operator pins win, manual rows are never touched.

// detectedCaps blobs for a model with the detected capabilities. The
// server-tool families stream through provider capability fallbacks, not
// per-model rows, and default to false for a detected row. Decision is a
// per-model registry fact (Ollama's /api/show lists "decision" for the
// System One / Jev class, e.g. clef:cloud) and is recorded as such;
// routing still gates decision requests on the ACCOUNT's capability
// (ADR-017), so the flag documents the model rather than opening a route.
func detectedCaps(info provider.ModelInfo) json.RawMessage {
	blob, err := json.Marshal(map[string]any{
		"streaming":      true,
		"tools":          info.Tools,
		"vision":         info.Vision,
		"thinking":       info.Thinking,
		"decision":       info.Decision,
		"web_search":     false,
		"web_fetch":      false,
		"code_execution": false,
	})
	if err != nil { // cannot happen for a map[string]any
		return json.RawMessage("{}")
	}
	return blob
}

// detectedCompat blob: the detected context window (omitted when
// undetected). The thinking policy is deliberately NOT written here:
// httpapi's compatFor derives thinking handling from
// the capability blob — a hardcoded "strip" would pin every synced row
// and neutralize the capability-driven default forever.
func detectedCompat(info provider.ModelInfo) json.RawMessage {
	m := map[string]any{}
	if info.ContextLength > 0 {
		m["context_window"] = info.ContextLength
	}
	blob, err := json.Marshal(m)
	if err != nil {
		return json.RawMessage("{}")
	}
	return blob
}

// blobIsEmpty reports whether a stored JSON blob carries no content —
// missing, null, or the empty object the write boundary normalizes empty
// values to. An empty blob is operator-unset: detection may fill it.
func blobIsEmpty(raw json.RawMessage) bool {
	return len(raw) == 0 || string(raw) == "null" || string(raw) == "{}"
}

// mergeDetectedNewRow returns the row for a newly imported model with the
// detected capabilities and context window baked in (detection is part of
// the import — there is no operator state to preserve in a fresh row).
func mergeDetectedNewRow(row storage.Model, info provider.ModelInfo) storage.Model {
	row.Capabilities = detectedCaps(info)
	row.Compat = detectedCompat(info)
	return row
}

// backfillDecision writes the one capability key that postdates the
// detection vocabulary into the rows the sync itself imported before it
// existed. Every blob detectedCaps has ever written carries "streaming",
// so a blob that carries that fact but says nothing about "decision" is a
// pre-decision-vocabulary sync blob: it gets the detected value, and the
// write is idempotent (the key's presence ends it).
//
// The rule is deliberately narrow in both directions. A blob WITHOUT
// "streaming" was not written by the sync's detection — the WebUI editor
// writes all eight keys, learning adds single facts — so it is operator
// authorship and stays untouched (ADR-010). And no other capability is
// backfilled: writing an absent "vision" would flip the router's
// modelCapabilityFlag from unknown to a known false and switch its vision
// gate on for rows that were never classified, which is exactly what the
// empty-blob-only rule exists to prevent.
func backfillDecision(raw json.RawMessage, detected bool) (json.RawMessage, bool) {
	var blob map[string]any
	if err := json.Unmarshal(raw, &blob); err != nil {
		return raw, false // malformed: store noise, never a rewrite target
	}
	if _, ok := blob["streaming"]; !ok {
		return raw, false
	}
	if _, stated := blob["decision"]; stated {
		return raw, false
	}
	// A pin without a value is still a statement (the operator reserved
	// the flag for themselves); learning and detection both honor it.
	if pinned, ok := blob["pinned"].(map[string]any); ok && pinned["decision"] == true {
		return raw, false
	}
	blob["decision"] = detected
	merged, err := json.Marshal(blob)
	if err != nil {
		return raw, false
	}
	return merged, true
}

// mergeDetected merges detected facts into an EXISTING synced row:
// capabilities only when the row's blob is empty — plus the one decision
// backfill below — context_window only when the compat blob is empty or
// carries no non-zero pin. Operator edits always win (ADR-010). Returns
// the merged row and whether anything changed.
func mergeDetected(row storage.Model, info provider.ModelInfo) (storage.Model, bool) {
	emptyCaps := blobIsEmpty(row.Capabilities)
	changed := false
	if emptyCaps {
		row.Capabilities = detectedCaps(info)
		changed = true
	} else if merged, wrote := backfillDecision(row.Capabilities, info.Decision); wrote {
		row.Capabilities = merged
		changed = true
	}
	pinned := storage.ContextWindowFromCompat(row.Compat) > 0
	emptyCompat := blobIsEmpty(row.Compat)
	if info.ContextLength > 0 && !pinned {
		// Merge into the existing blob so other operator keys survive;
		// an empty/null blob starts from the detected default.
		var base map[string]any
		if emptyCompat {
			_ = json.Unmarshal(detectedCompat(info), &base)
		} else {
			_ = json.Unmarshal(row.Compat, &base) // malformed degrades to base == nil
		}
		if base == nil {
			base = map[string]any{}
		}
		base["context_window"] = info.ContextLength
		if blob, err := json.Marshal(base); err == nil {
			row.Compat = blob
			changed = true
		}
	} else if emptyCompat {
		// No detection and no pin: the empty blob still gets the default
		// thinking handling so the row's policy is explicit.
		row.Compat = json.RawMessage(`{"thinking_handling":"strip"}`)
		changed = true
	}
	return row, changed
}
