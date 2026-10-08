// RouteTrace (2026-10-07): the structured renderer for a request's
// route_trace — a chain summary plus a human-readable routing timeline —
// with the raw JSON kept as a disclosure for debugging and for trace shapes
// this renderer does not recognise (the pre-v2 unversioned rows older
// instances still hold). The trace object is deliberately `unknown` on the
// wire (types.ts); this module is the one place that reads it, so an
// unexpected shape degrades to the raw view instead of breaking the drawer.
import type { ChildLog } from '../types';

export interface TraceEvent {
  type?: string;
  reason?: string;
  provider_id?: string;
  model?: string;
  from?: string;
  to?: string;
  class?: string;
  error_code?: string;
  http_status?: number;
  failure_position?: string;
  capability?: string;
  latency_ms?: number;
}

// parseEvents returns the trace's events, or null when the blob is not the
// {version, events:[...]} object this renderer knows (→ raw fallback).
export function parseEvents(trace: unknown): TraceEvent[] | null {
  if (trace === null || trace === undefined || typeof trace !== 'object') return null;
  const events = (trace as { events?: unknown }).events;
  if (!Array.isArray(events)) return null;
  return events.filter((e): e is TraceEvent => e !== null && typeof e === 'object');
}

// eventTitle renders one event's human title.
export function eventTitle(ev: TraceEvent): string {
  const arrow = (a?: string, b?: string): string => `${a || '—'} → ${b || '—'}`;
  switch (ev.type) {
    case 'alias':
      return `Alias ${arrow(ev.from, ev.to)}`;
    case 'model_resolved':
      return `Resolved ${arrow(ev.from, ev.to)}`;
    case 'attempt':
      return `Attempt ${ev.provider_id || ev.model || '—'}`;
    case 'failed':
      return `Failed ${ev.provider_id || '—'}${ev.http_status ? ` (${ev.http_status})` : ''}`;
    case 'failover':
      return `Failover ${arrow(ev.from, ev.to)}`;
    case 'retry':
      return `Retry ${ev.provider_id || '—'}`;
    case 'compat_policy':
      return `Compat ${ev.from || '—'} = ${ev.to || '—'}`;
    case 'expand':
      return `Expand ${arrow(ev.from, ev.to)}`;
    case 'rotation':
      return `Rotation ${arrow(ev.from, ev.to)}`;
    case 'emulated_tool':
      return `Emulated ${ev.to || 'tool'}`;
    case 'rejected':
      return 'Rejected';
    case 'capability_fallback':
      return `Capability fallback: ${ev.capability || '—'}`;
    case 'implicit_route':
      return `Implicit route ${arrow(ev.from, ev.to)}`;
    case 'context_override':
      return `Context marker stripped ${arrow(ev.from, ev.to)}`;
    default:
      return ev.type || 'event';
  }
}

// eventMeta is the event's secondary line (reason / class / code / latency).
export function eventMeta(ev: TraceEvent): string {
  const parts: string[] = [];
  if (ev.reason) parts.push(ev.reason);
  if (ev.class) parts.push(ev.class);
  if (ev.error_code) parts.push(ev.error_code);
  if (ev.failure_position) parts.push(ev.failure_position);
  if (typeof ev.latency_ms === 'number' && ev.latency_ms > 0) parts.push(`${ev.latency_ms} ms`);
  return parts.join(' · ');
}

// eventSeverity maps an event to a .feed-item severity class.
function eventSeverity(ev: TraceEvent): string {
  switch (ev.type) {
    case 'failed':
    case 'rejected':
      return 'sev-down';
    case 'skipped':
    case 'compat_policy':
    case 'failover':
    case 'retry':
    case 'capability_fallback':
      return 'sev-warn';
    case 'emulated_tool':
      return 'sev-up';
    default:
      return 'sev-info';
  }
}

// chainProviders derives the attempted provider sequence from the events
// (the order providers were attempted), for the chain summary. The last one
// is the serving provider.
function chainProviders(events: TraceEvent[]): string[] {
  const seen = new Set<string>();
  const out: string[] = [];
  for (const ev of events) {
    if (ev.type === 'attempt' && ev.provider_id && !seen.has(ev.provider_id)) {
      seen.add(ev.provider_id);
      out.push(ev.provider_id);
    }
  }
  return out;
}

// ChainSummary renders the attempted provider chain as pills, highlighting
// the serving (last-attempted) provider. Renders nothing when no attempt
// was made (a rejected request — the timeline carries that story).
export function ChainSummary({ events }: { events: TraceEvent[] }) {
  const chain = chainProviders(events);
  if (chain.length === 0) return null;
  return (
    <div className="chain-summary">
      {chain.map((p, i) => (
        <span key={`${p}-${i}`} className={`chain-pill${i === chain.length - 1 ? ' serving' : ''}`}>
          {p}
        </span>
      ))}
    </div>
  );
}

// TraceTimeline renders every event as a feed item, in order.
export function TraceTimeline({ events }: { events: TraceEvent[] }) {
  if (events.length === 0) return <p className="hint">No routing decisions recorded.</p>;
  return (
    <div className="feed">
      {events.map((ev, i) => (
        <div key={i} className={`feed-item ${eventSeverity(ev)}`}>
          <span className="f-ic" aria-hidden="true" />
          <div className="f-main">
            <p className="f-title">{eventTitle(ev)}</p>
            {eventMeta(ev) !== '' && <p className="f-meta">{eventMeta(ev)}</p>}
          </div>
        </div>
      ))}
    </div>
  );
}

// RawTrace is the disclosure holding the exact stored JSON.
export function RawTrace({ trace }: { trace: unknown }) {
  return (
    <details className="raw-trace">
      <summary>Raw JSON</summary>
      <pre className="mono" style={{ margin: 0, whiteSpace: 'pre-wrap', wordBreak: 'break-word', fontSize: 12 }}>
        {JSON.stringify(trace, null, 2)}
      </pre>
    </details>
  );
}

// childTitle renders a stored sub-operation's human title (the drawer's
// list mirrors the table's nested rows).
export function childTitle(c: ChildLog): string {
  switch (c.kind) {
    case 'failover':
      return `Failover ${c.from_ref || '—'} → ${c.to_ref || '—'}`;
    case 'capability_fallback':
      return `Capability fallback: ${c.detail || 'unknown'}`;
    case 'emulated_tool':
      return `Emulated ${c.to_ref || 'tool'}`;
    case 'retry':
      return `Retry ${c.provider_id || c.model || ''}`.trim();
    default:
      return c.kind;
  }
}

// childCls is the kind-specific chip class shared by the table row and the
// drawer list.
export function childCls(kind: string): string {
  return `chip child-${kind.replace(/_/g, '-')}`;
}

// childMeta is a sub-operation's secondary line (the emulation invocation
// summary, or the executor's transition reason).
export function childMeta(c: ChildLog): string {
  if (c.kind === 'emulated_tool') return c.detail || '';
  if (c.kind === 'capability_fallback') return c.detail ? `family: ${c.detail}` : '';
  return c.detail || '';
}
