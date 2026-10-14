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
import type { KeyboardEvent } from 'react';
import type { ChildLog, RequestLog, RunningRequest } from '../types';
import { childCls, childTitle } from './RouteTrace';

export type LogRowShape =
  | { kind: 'done'; row: RequestLog; healed: boolean; childCount?: number }
  | { kind: 'running'; run: RunningRequest }
  | { kind: 'child'; child: ChildLog; last?: boolean; onOpenChild?: () => void };

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
// cache-read over input). Shared by the request row, its steps and the step
// drawer, so one row never disagrees with another about the same usage object.
export function tokensCell(usage: unknown): [string, string, string] {
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

// keyDisplay names the client key a row was opened with: the operator's label
// and its display prefix, falling back to the raw id so a deleted key still
// says something actionable, and to a dash when the row has no key at all.
// Shared by the request row and its steps, which inherit it from the request.
export const keyDisplay = (
  name: string | undefined,
  prefix: string | undefined,
  id: string | undefined,
): string => {
  if (name) return prefix ? `${name} (${prefix})` : name;
  return id || '—';
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
    // PROVIDER/ACCOUNT name the endpoint THIS op ran on, which is the point of
    // a fallback row. The parent's columns would be wrong here: they describe
    // where the request started, not where a failover landed.
    const providerId = c.provider_id ?? '';
    const accountName = c.provider_name ?? c.provider_id ?? '';
    // What the step itself spent, and what it cost. A transition has neither,
    // and says so with a dash.
    const cu = (c.usage ?? {}) as UsageShape;
    const [cti, cto, ccr] = tokensCell(c.usage);
    const childKeyText = keyDisplay(c.key_name, c.key_prefix, c.client_key_id);
    // The child carries the same column count as its parent so its cells
    // sit directly under the parent's — the failover's target lines up
    // with the parent's provider, not in a merged blob. The MODEL column
    // shows the model this op actually served/used (2026-10-08: the trace
    // walk now attributes each op's model; empty → the meta as before).
    //
    // The row OPENS (2026-10-10): a sub-operation has its own detail view,
    // because what it sent and received lives in a payload of its own — the
    // parent's conversation and answer cannot answer "what did this step do".
    // Hence a button role and a keyboard path, not decoration.
    const open = shape.onOpenChild;
    return (
      <tr
        className="log-row log-row-child"
        {...(shape.last ? { 'data-child-last': true } : {})}
        {...(open
          ? {
              onClick: open,
              onKeyDown: (e: KeyboardEvent<HTMLTableRowElement>) => {
                if (e.key === 'Enter' || e.key === ' ') {
                  e.preventDefault();
                  open();
                }
              },
              tabIndex: 0,
              role: 'button',
              title: 'Show what this step sent and received',
            }
          : {})}
      >
        <td className="td-gutter">
          <span className="child-elbow" aria-hidden="true" />
        </td>
        <td>
          <span className={childCls(c.kind)}>{childTitle(c)}</span>
        </td>
        {/* Single line, like every row: the second line (the op's reason, the
            caption excerpt, `family: vision`) pushed the row taller than its
            parent and left the tree elbow floating between two baselines. The
            op's detail is not lost — it is the first thing the row's own
            drawer shows. The MODEL column names the model this op actually
            used; an op that has none says so with a dash rather than falling
            back to its detail text (a web-tool row would otherwise show the
            search query in the model column). */}
        <td className="primary child-model col-model" title={c.model || undefined}>
          {c.model || <span className="txt-muted">—</span>}
        </td>
        {/* A sub-operation shows what it owns and inherits what the request
            owns. PROVIDER/ACCOUNT name the endpoint the op itself ran on — for
            a web-tool call that is the emulating account, which is the answer
            to "which provider served this". PROTOCOL and API KEY belong to the
            request the step is part of, so they come from the parent row.
            TOKENS, TPS and DURATION are the step's own: a describer call and a
            web-tool call both take time, the describer also spends tokens, and
            a transition spends nothing — which reads as a dash, never as a
            blank cell. */}
        <td>{providerId && <span className="pill">{c.provider_kind ?? providerId}</span>}</td>
        <td><span className="pill pill-warm">{protocolLabel(c.protocol)}</span></td>
        <td className="col-account" title={accountName}>
          {accountName || '—'}
        </td>
        <td className="mono col-key" title={childKeyText}>
          {childKeyText}
        </td>
        <td className="mono col-tokens">
          <span className="txt-muted">TI:</span> <span className="num-ti">{cti}</span>{' '}
          <span className="txt-muted">TO:</span> <span className="num-to">{cto}</span>{' '}
          <span className="txt-muted">CR:</span> <span className="num-cr">{ccr}</span>
        </td>
        <td className="mono num">{tpsOf(cu.output_tokens, c.latency_ms ?? 0)}</td>
        <td className="mono num">{c.latency_ms ? `${c.latency_ms} ms` : '—'}</td>
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
  const apiKey = keyDisplay(l.key_name, l.key_prefix, l.client_key_id);
  // PROVIDER shows the endpoint TYPE ("Ollama Cloud"), ACCOUNT the name the
  // operator gave the account ("main-2"). Both fall back to the raw id so
  // a deleted account still says something actionable — and to a dash when the
  // request never reached an endpoint at all (a rejected route): a blank cell
  // would read as "not loaded", a dash reads as "there is none".
  const provider = l.provider_kind ?? l.provider_id ?? '';
  const account = l.provider_name ?? l.provider_id ?? '';
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
      <td className="primary col-model" title={requested !== l.model ? `requested ${requested}` : undefined}>
        {l.model}
      </td>
      <td>{provider && <span className="pill">{provider}</span>}</td>
      <td><span className="pill pill-warm">{protocolLabel(l.protocol)}</span></td>
      <td className="col-account" title={account || undefined}>{account || '—'}</td>
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
