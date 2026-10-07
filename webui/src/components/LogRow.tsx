// LogRow (Logs-page redesign, 2026-10-06): one table row of the request
// log — the OmniRoute-style dense layout. Both a finished RequestLog and a
// running pseudo-row render through this component (running = the
// `…`-badge row with em-dash cells). Pure render: all derivation (chain
// marks, healed, row click) arrives via the props.
import { memo } from 'react';
import type { RequestLog, RunningRequest } from '../types';

export type LogRowShape =
  | { kind: 'done'; row: RequestLog; chainStart: boolean; chainCont: boolean; healed: boolean }
  | { kind: 'running'; run: RunningRequest; chainStart: boolean; chainCont: boolean };

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

const tpsOf = (output: number | undefined, latencyMS: number): string => {
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

const providerLabel = (p: string | undefined): string => (p ? p.toUpperCase() : '—');

function statusBadge(status: number): { label: string; cls: string } {
  if (status >= 500) return { label: String(status), cls: 'down' };
  if (status >= 400) return { label: String(status), cls: 'warn' };
  return { label: String(status), cls: 'up' };
}

// convShort renders the conversation cell: conv_ + the 8-hex tail.
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
  const chainAttrs = {
    'data-chain': shape.chainStart || shape.chainCont ? '1' : undefined,
    'data-chain-start': shape.chainStart || undefined,
    'data-chain-cont': shape.chainCont || undefined,
  };

  if (shape.kind === 'running') {
    const { run } = shape;
    return (
      <tr {...chainAttrs} data-running className="log-row">
        <td className="td-gutter" />
        <td>
          <span className="badge info"><span className="b-dot b-dot-spin" />…</span>
        </td>
        <td className="primary">{run.model}</td>
        <td className="mono">{run.model}</td>
        <td><span className="pill">—</span></td>
        <td><span className="pill pill-warm">{protocolLabel(run.protocol)}</span></td>
        <td>—</td>
        <td className="mono">—</td>
        <td className="mono">—</td>
        <td className="mono num">—</td>
        <td className="mono num">—</td>
        <td className="mono col-time">{fmtTime(run.started_at)}</td>
        <td className="mono">—</td>
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
  const account = l.provider_name ?? l.provider_id;

  return (
    <tr {...chainAttrs} className="log-row" onClick={onOpen ? () => onOpen(l) : undefined}>
      <td className="td-gutter" />
      <td>
        <span className={`badge ${st.cls}`}><span className="b-dot" />{st.label}</span>
        {shape.healed && <span className="chip chip-healed">healed</span>}
      </td>
      <td className="primary">{l.model}</td>
      <td className="mono">{requestedModel(l)}</td>
      <td><span className="pill">{providerLabel(l.provider_id)}</span></td>
      <td><span className="pill pill-warm">{protocolLabel(l.protocol)}</span></td>
      <td>{account}</td>
      <td className="mono">{apiKey}</td>
      <td className="mono">
        <span className="txt-muted">TI:</span> <span className="num-ti">{ti}</span>{' '}
        <span className="txt-muted">TO:</span> <span className="num-to">{to}</span>{' '}
        <span className="txt-muted">CR:</span> <span className="num-cr">{cr}</span>
      </td>
      <td className="mono num">{tpsOf(u.output_tokens, l.latency_ms)}</td>
      <td className="mono num">{(l.latency_ms / 1000).toFixed(1)}s</td>
      <td className="mono col-time">{fmtTime(l.timestamp)}</td>
      <td className="mono">{convShort(l.conversation_id)}</td>
    </tr>
  );
}

export const LogRow = memo(LogRowInner);