package routing

import "github.com/tubeof/proxyllm/internal/translate/ir"

// Child operations: the upstream operations the operator needs as their own
// log rows rather than as trace-only detail — a chain failover, an image
// description produced by a vision fallback model (ADR-014), and an emulated
// web-tool call. This mapper is the single source of truth for that mapping;
// the persist path (httpapi.writeRequestLog → storage.InsertChildLogs)
// consumes it.
//
// A same-provider retry is deliberately NOT a row: it is one attempt of the
// same operation, not a separable one, and a retry storm floods the log.
// It stays a timeline event in the detail drawer, alongside everything else
// the trace records (alias, model_resolved, attempt, failed, skipped,
// expand, rotation, compat_policy, rejected, implicit_route,
// context_override).

// OpKind enumerates the trace event types that become their own log row.
type OpKind string

const (
	// OpFailover: the request moved to the next chain entry (including onto
	// a capability fallback model, whose Failover follows the consultation).
	OpFailover OpKind = "failover"

	// OpCapabilityFallback: the rule chain was exhausted on capability gaps
	// and the configured capability fallback models were consulted
	// (ADR-010). Detail carries the capability
	// family (e.g. "vision").
	OpCapabilityFallback OpKind = "capability_fallback"

	// OpEmulatedTool: a server tool was served by the emulation layer
	// (the model's tool call answered via a web-tool account). To carries
	// the tool name, Detail the invocation summary (query/url, outcome).
	OpEmulatedTool OpKind = "emulated_tool"

	// OpImageCaption: a vision fallback model DESCRIBED an image and the
	// description replaced it in the request (ADR-014). ProviderID/Model name
	// the describer, Detail the description's length. This is the row that
	// answers "what was the answering model told the image showed?".
	OpImageCaption OpKind = "image_caption"
)

// ChildOp is one separable sub-operation of a request, derived from its
// route_trace. It is provider/model-identity level data only — never
// request bodies, headers, or credentials (the child row is rendered in the
// WebUI and stored alongside request_logs).
type ChildOp struct {
	Kind OpKind

	// ProviderID/Model name the entry this op concerns, when the event
	// names one (a retry names its provider; a capability-fallback
	// consultation names none).
	ProviderID string
	Model      string

	// From/To carry the transition for failover ops (provider or account
	// identity), the tool name for emulated-tool ops (To only).
	From string
	To   string

	// Detail is the operator-facing summary: the failover/retry reason, the
	// capability family, or the emulation invocation detail. It is
	// upstream-influenced text (the emulated-tool path carries the
	// emulator's error text and a model-supplied URL), so the write path
	// MUST mask it before persisting — see httpapi.ChildOpsFromTrace.
	Detail string

	// LatencyMS is how long this sub-operation took, when it was one that
	// took time: a describer call, an emulated tool invocation. Zero for a
	// transition, which is not an operation.
	LatencyMS int64

	// Usage is the token spend of the sub-operation, when it spent tokens.
	// A web-tool call spends none; a describer call spends what the vision
	// model reported.
	Usage *ir.Usage
}

// ChildOps maps a request's recorded trace events to its child operations,
// in trace order. Only the op kinds above are selected; a request that
// succeeded through its primary provider yields no ops.
//
// Model attribution: attempt events carry the
// provider+wire model actually dispatched, so the walk feeds the attempts
// into the ops — a capability-fallback op names the fallback model its walk
// served (the next attempt's model), a failover names the model the target
// provider then attempted, and an emulated-tool op names the model that
// issued the tool call. The Logs page renders that on the child row;
// provider/model identity is the op's point (never request bodies).
func ChildOps(events []Event) []ChildOp {
	// One vision rescue records TWO events — a caption per described image and
	// a consultation for the rescue as a whole — but they are one operation, so
	// only the caption becomes a row. The consultation survives on its own only
	// when no vision model could describe the image: then it is the sole
	// evidence the fallback was consulted at all, and dropping it would hide the
	// failure.
	captionPresent := false
	for _, ev := range events {
		if ev.Type == EventImageCaption {
			captionPresent = true
			break
		}
	}

	var ops []ChildOp
	for i, ev := range events {
		switch ev.Type {
		case EventFailover:
			ops = append(ops, ChildOp{
				Kind: OpFailover,
				From: ev.From, To: ev.To, Detail: ev.Reason,
				// The model the failover's target actually attempted.
				ProviderID: ev.To,
				Model:      nextAttemptModel(events, i, ev.To),
			})
		case EventCapabilityFallback:
			if captionPresent {
				continue
			}
			ops = append(ops, ChildOp{
				Kind: OpCapabilityFallback,
				// The event's Reason is the same boilerplate for every
				// consultation; the capability family is the specific fact.
				Detail: ev.Capability,
				// The fallback model the walk then served.
				Model: nextAttemptModel(events, i, ""),
			})
		case EventImageCaption:
			if ev.CacheHit {
				// A cached description replaced the image without a call.
				// Recording it as an operation would put a row in the log for a
				// request that never left the proxy.
				continue
			}
			ops = append(ops, ChildOp{
				Kind:   OpImageCaption,
				Detail: ev.Reason,
				// The vision model that produced the description — the
				// event names it directly, unlike the walk-derived ops.
				ProviderID: ev.ProviderID,
				Model:      ev.Model,
				LatencyMS:  ev.LatencyMS,
				Usage:      ev.Usage,
			})
		case EventEmulatedTool:
			ops = append(ops, ChildOp{
				Kind: OpEmulatedTool,
				To:   ev.To, Detail: ev.Reason,
				// The emulating account that answered, so the row names the web
				// provider and account actually used. Model stays empty: the
				// model whose round issued the tool call has nothing to do with
				// where the call went, and naming it there made the log claim a
				// fallback that never happened.
				ProviderID: ev.ProviderID,
				LatencyMS:  ev.LatencyMS,
			})
		}
	}
	return ops
}

// nextAttemptModel finds the model of the attempt event after index i
// (the first attempt when to is "" — any provider — matching the given
// provider otherwise). "" when the walk never attempted one.
func nextAttemptModel(events []Event, i int, to string) string {
	for j := i + 1; j < len(events); j++ {
		ev := events[j]
		if ev.Type != EventAttempt || ev.Model == "" {
			continue
		}
		if to == "" || ev.ProviderID == to {
			return ev.Model
		}
	}
	return ""
}
