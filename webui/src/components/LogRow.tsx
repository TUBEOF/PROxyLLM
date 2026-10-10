// LogRow (Logs-page redesign, 2026-10-06): one table row of the request
// log — the dense layout. Both a finished RequestLog and a running
// pseudo-row render through this component (running = the `…`-badge row
// with em-dash cells). Sub-operations (2026-10-07) render as their own
// rows hanging under their parent: a failover, a capability (vision)
// fallback walk, or an emulated web-tool call. A sub-row keeps the parent's
// column count so the eye can read down the table, but fills only the
// columns the op owns — the request-scoped ones (endpoint, credentials,
// tokens, rates) stay empty, because repeating the parent's values there
// would attribute them to the wrong thing.
// Pure render: all derivation (chain marks, healed, row click) arrives via
// the props.
import { memo } from 'react';
import type { ChildLog, RequestLog, RunningRequest } from '../types';
import { childCls, childMeta, childTitle } from './RouteTrace';

export type LogRowShape =
  | { kind: 'done'; row: RequestLog; healed: boolean; childCount?: number }
  | { kind: 'running'; run: RunningRequest }
  | { kind: 'child'; child: ChildLog; last?: boolean; onOpenParent?: () => void };

// ParentShape is a request row (finished or running) — the rows the
// conversation-chain grouping pass operates on (sub-operations never
// participate: they are not requests).
export type ParentShape = Exclude<LogRowShape, { kind: 'child' }>;

interface UsageShape {
  input_tokens?: number;
  output_tokens?: number;
  cache_read_input_tokens?: number;
}

interface TraceEvent {
  type?: string;
  from?: string;
}

const fmtTime = (ts: number): string => {
  const d = new Date(ts * 1000);
  return d.toTimeString().slice(0, 8);
};

const fmtNum = (n: number | undefined): string =>
  typeof n === 'number' && Number.isFinite(n) ? n.toLocaleString('en-US') : '—';

// requestedModel: the client-requested model — the first model_resolved
// trace event's from, falling back to the stored (client-echoed) model.
function requestedModel(l: RequestLog): string {
  const trace = l.route_trace as { events?: TraceEvent[] } | null;
  if (trace !== null && trace !== undefined && Array.isArray(trace.events)) {
    for (const ev of trace.events) {
      if (
        ev !== null &&
        typeof ev === 'object' &&
        ev.type === 'model_resolved' &&
        typeof ev.from === 'string' &&
        ev.from !== ''
      ) {
        return ev.from;
      }
    }
  }
  return l.model;
}

// tokensCell parses the usage JSON into the TI/TO/CR triple (CR % =
// cache-read over input).
function tokensCell(usage: unknown): [string, string, string] {
  if (usage === null || usage === undefined || typeof usage !== 'object') {
    return ['—', '—', '—'];
  }
  const u = usage as UsageShape;
  const cr =
    typeof u.cache_read_input_tokens === 'number' && typeof u.input_tokens === 'number' && u.input_tokens > 0
      ? `${fmtNum(u.cache_read_input_tokens)} (${Math.round((u.cache_read_input_tokens / u.input_tokens) * 100)}%)`
      : fmtNum(u.cache_read_input_tokens);
  return [fmtNum(u.input_tokens), fmtNum(u.output_tokens), cr];
}

// tpsOf is the request's average output rate: output tokens over the whole
// request latency. Labelled TPS in the header; the drawer breaks the same
// number into its parts. One shared derivation — the table cell and the
// detail drawer must never disagree on the same request.
export const tpsOf = (output: number | undefined, latencyMS: number): string => {
  if (latencyMS <= 0 || typeof output !== 'number') return '—';
  return (output / (latencyMS / 1000)).toFixed(1);
};

const protocolLabel = (p: string | undefined): string => {
  if (!p) return '—';
  if (p === 'anthropic') return 'CLAUDE';
  if (p === 'openai-responses') return 'RESPONSES';
  if (p === 'openai') return 'OPENAI';
  return p.toUpperCase();
};

function statusBadge(status: number): { label: string; cls: string } {
  if (status >= 500) return { label: String(status), cls: 'down' };
  if (status >= 400) return { label: String(status), cls: 'warn' };
  return { label: String(status), cls: 'up' };
}

// convShort renders the conversation id: conv_ + the 8-hex tail.
function convShort(id: string | undefined): string {
  if (typeof id !== 'string' || !id.startsWith('conv_') || id.length < 5 + 8) return '—';
  return id.slice(0, 5 + 8);
}

function LogRowInner({
  shape,
  onOpen,
}: {
  shape: LogRowShape;
  onOpen?: (l: RequestLog) => void;
}) {
  if (shape.kind === 'child') {
    const c = shape.child;
    const meta = childMeta(c);
    // The child carries the same column count as its parent so its cells
    // sit directly under the parent's — the failover's target lines up
    // with the parent's provider, not in a merged blob. The MODEL column
    // shows the model this op actually served/used (2026-10-08: the trace
    // walk now attributes each op's model; empty → the meta as before).
    return (
      <tr
        className="log-row log-row-child"
        {...(shape.last ? { 'data-child-last': true } : {})}
      >
        <td className="td-gutter">
          <span className="child-elbow" aria-hidden="true" />
        </td>
        <td>
          <span className={childCls(c.kind)}>{childTitle(c)}</span>
        </td>
        <td className="primary child-model" title={c.model || meta || undefined}>
          {c.model || <span className="txt-muted">{meta || '—'}</span>}
          {c.model && meta !== '' && (
            <span className="cell-sub mono child-meta">{meta}</span>
          )}
        </td>
        {/* A sub-operation carries only what it owns. The columns that
            describe a request's endpoint, credentials and token spend have
            no meaning on a fallback/emulation op, so they stay empty rather
            than padded with an em-dash placeholder. */}
        <td />
        <td />
        <td />
        <td />
        <td />
        <td />
        <td />
        <td className="mono col-time">{fmtTime(c.ts)}</td>
      </tr>
    );
  }

  if (shape.kind === 'running') {
    const { run } = shape;
    return (
      <tr data-running className="log-row">
        <td className="td-gutter" />
        <td>
          <span className="badge info"><span className="b-dot b-dot-spin" />…</span>
        </td>
        <td className="primary">{run.model}</td>
        <td><span className="pill">—</span></td>
        <td><span className="pill pill-warm">{protocolLabel(run.protocol)}</span></td>
        <td>—</td>
        <td className="mono">—</td>
        <td className="mono">—</td>
        <td className="mono num">—</td>
        <td className="mono num">—</td>
        <td className="mono col-time">{fmtTime(run.started_at)}</td>
      </tr>
    );
  }

  const l = shape.row;
  const st = statusBadge(l.status);
  const u = (l.usage ?? {}) as UsageShape;
  const [ti, to, cr] = tokensCell(l.usage);
  const apiKey = l.key_name
    ? l.key_prefix
      ? `${l.key_name} (${l.key_prefix})`
      : l.key_name
    : l.client_key_id;
  // PROVIDER shows the endpoint TYPE ("Ollama Cloud"), ACCOUNT the name the
  // operator gave the account ("main-2"). Both fall back to the raw id so
  // a deleted account still says something actionable.
  const provider = l.provider_kind ?? l.provider_id;
  const account = l.provider_name ?? l.provider_id;
  const requested = requestedModel(l);
  const conv = convShort(l.conversation_id);

  return (
    <tr
      className="log-row"
      data-has-children={(shape.childCount ?? 0) > 0 || undefined}
      onClick={onOpen ? () => onOpen(l) : undefined}
      title={`conversation ${conv}`}
    >
      <td className="td-gutter" />
      <td>
        <span className={`badge ${st.cls}`}><span className="b-dot" />{st.label}</span>
        {shape.healed && <span className="chip chip-healed">healed</span>}
      </td>
      <td className="primary">
        {l.model}
        {requested !== l.model && <span className="cell-sub mono">{requested}</span>}
      </td>
      <td><span className="pill">{provider}</span></td>
      <td><span className="pill pill-warm">{protocolLabel(l.protocol)}</span></td>
      <td className="col-account" title={account}>{account}</td>
      <td className="mono col-key" title={apiKey}>{apiKey}</td>
      <td className="mono col-tokens">
        <span className="txt-muted">TI:</span> <span className="num-ti">{ti}</span>{' '}
        <span className="txt-muted">TO:</span> <span className="num-to">{to}</span>{' '}
        <span className="txt-muted">CR:</span> <span className="num-cr">{cr}</span>
      </td>
      <td className="mono num">{tpsOf(u.output_tokens, l.latency_ms)}</td>
      <td className="mono num">{l.latency_ms} ms</td>
      <td className="mono col-time">{fmtTime(l.timestamp)}</td>
    </tr>
  );
}

export const LogRow = memo(LogRowInner);
