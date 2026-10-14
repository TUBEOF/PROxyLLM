package httpapi

// Wire-name resolution (model identity): the client-facing model
// ID is a prefixed routing identity ("ollamacloud/glm-5.3"); the upstream
// request body must carry the bare wire name ("glm-5.3"). Rows created by
// the model sync store the wire name in models.name; legacy unprefixed rows
// (and operator rows without a store entry) pass through unchanged — the
// fallback keeps every pre-existing configuration routing exactly as
// before.
//
// The resolution rewrites the CHAIN ENTRIES' model strings, not the request:
// the executor stamps each entry's model onto the request at dispatch
// (executor withModel), so the wire name reaches the upstream only through
// the entries. The client-facing echo and the request-log row keep the
// routing ID, and the trace records the resolution.

import (
	"context"
	"strings"

	"github.com/tubeof/proxyllm/internal/routing"
)

// resolveChainWireModels rewrites every chain entry's model from its routing
// ID to the upstream wire name, recording the resolution on the trace.
// Rules for one entry's model:
//   - a models-table row with this ID: its Name is authoritative (synced
//     rows carry the bare upstream name; a manual row may set any wire
//     name);
//   - no row: if the ID contains "/", the tail after the LAST slash is the
//     wire name ("ollamacloud/glm-5.3" → "glm-5.3"; a wire name with a
//     slash itself, e.g. "openrouter/qwen/qwen-3", keeps its shape only
//     via a store row);
//   - neither: the ID is its own wire name (legacy behavior).
//
// All entries of one resolved chain share the same model string (the
// router stamps the post-alias model onto every entry), so one store read
// resolves the whole chain. A store failure degrades to the fallback rules
// — never a hard request failure (the last-known-good read contract).
func (a *api) resolveChainWireModels(ctx context.Context, chain []routing.ChainEntry, trace *routing.Trace) []routing.ChainEntry {
	if len(chain) == 0 {
		return chain
	}
	routingID := chain[0].Model
	resolved := routingID
	if row, err := a.store.GetModel(ctx, routingID); err == nil && row.Name != "" {
		resolved = row.Name
	} else if i := strings.LastIndex(routingID, "/"); i >= 0 && i+1 < len(routingID) {
		resolved = routingID[i+1:]
	}
	if resolved != routingID {
		trace.ModelResolved(routingID, resolved)
		for i := range chain {
			chain[i].Model = resolved
		}
	}
	return chain
}
