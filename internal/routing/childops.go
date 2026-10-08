package routing

// Child operations (2026-10-07 Logs page): the upstream operations the
// operator needs as their own log rows rather than as trace-only detail —
// a chain failover, a capability (vision) fallback model walk, and an
// emulated web-tool call. This mapper is the single source of truth for
// that mapping; the persist path (httpapi.writeRequestLog →
// storage.InsertChildLogs) consumes it.
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
	// (ADR-010 / 2026-09-21 model fallback). Detail carries the capability
	// family (e.g. "vision").
	OpCapabilityFallback OpKind = "capability_fallback"

	// OpEmulatedTool: a server tool was served by the §6.2 emulation layer
	// (the model's tool call answered via the Firecrawl fallback). To carries
	// the tool name, Detail the invocation summary (query/url, outcome).
	OpEmulatedTool OpKind = "emulated_tool"
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
}

// ChildOps maps a request's recorded trace events to its child operations,
// in trace order. Only the four op kinds above are selected; a request that
// succeeded through its primary provider yields no ops.
func ChildOps(events []Event) []ChildOp {
	var ops []ChildOp
	for _, ev := range events {
		switch ev.Type {
		case EventFailover:
			ops = append(ops, ChildOp{
				Kind: OpFailover,
				From: ev.From, To: ev.To, Detail: ev.Reason,
			})
		case EventCapabilityFallback:
			ops = append(ops, ChildOp{
				Kind: OpCapabilityFallback,
				// The event's Reason is the same boilerplate for every
				// consultation; the capability family is the specific fact.
				Detail: ev.Capability,
			})
		case EventEmulatedTool:
			ops = append(ops, ChildOp{
				Kind: OpEmulatedTool,
				To:   ev.To, Detail: ev.Reason,
			})
		}
	}
	return ops
}
