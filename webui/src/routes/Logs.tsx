import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { apiCall } from '../api';
import type { ChildLog, LogsResponse, RequestLog, RunningRequest } from '../types';
import { EmptyState, ErrorNote, PageHead, useLiveData } from '../hooks';
import { useLogStream } from '../hooks/useLogStream';
import { Drawer } from '../components/Overlays';
import { IconChevronLeft, IconChevronRight } from '../icons';
import { LogRow, type LogRowShape, type ParentShape } from '../components/LogRow';
import {
  ChainSummary,
  TraceTimeline,
  RawTrace,
  parseEvents,
  childTitle,
  childCls,
  childMeta,
} from '../components/RouteTrace';

// Logs (Logs-page redesign, 2026-10-06): the dense request log — status /
// model / requested / provider / protocol / account / api key / tokens /
// tps / duration / time ↓ / conversation, chained rows in the gutter,
// healing on retried conversations, in-flight rows live over SSE.
// Pagination rides the API's limit/offset contract (the page fetch stays
// the paging/backfill source). On page 1 the SSE feed maintains the
// running pseudo-rows and prepends finished rows; any page fetch (poll or
// manual Refresh) resets that merge — the fetched page is the truth.

const PAGE_SIZE = 50;

// POLL_MS is the fallback poll interval (the SSE stream is the live feed;
// the poll re-syncs the page and keeps the 401/session-loss probe).
const POLL_MS = 4000;

function fmtTime(ts: number): string {
  const d = new Date(ts * 1000);
  return d.toISOString().replace('T', ' ').slice(0, 19) + ' UTC';
}

function statusBadge(status: number): { label: string; cls: string } {
  if (status >= 500) return { label: String(status), cls: 'down' };
  if (status >= 400) return { label: String(status), cls: 'warn' };
  return { label: String(status), cls: 'up' };
}

export default function Logs() {
  const [offset, setOffset] = useState(0);
  const [selected, setSelected] = useState<RequestLog | null>(null);
  const [running, setRunning] = useState<RunningRequest[]>([]);

  const { data, loading, error, reload } = useLiveData<LogsResponse>(
    (signal) =>
      apiCall<LogsResponse>('GET', `/api/admin/logs?limit=${PAGE_SIZE}&offset=${offset}`, undefined, signal),
    POLL_MS,
    [offset],
  );

  // merged: the page-1 SSE overlay (finished rows prepended since the last
  // page fetch). A page fetch replaces it; offset changes to non-zero hide
  // it (paging is authoritative there).
  const [merged, setMerged] = useState<RequestLog[] | null>(null);

  // Any authoritative page load resets the merge (useLiveData's data is
  // the fetched truth; a non-null merged would shadow a fresh poll).
  useEffect(() => {
    setMerged(null);
  }, [data]);

  const onRunning = useCallback((rows: RunningRequest[]) => setRunning(rows), []);
  const onLog = useCallback((row: RequestLog) => {
    if (offset !== 0) return;
    setMerged((cur) => {
      const base = cur ?? dataRef.current;
      if (base === null) return base;
      const logs = [row, ...base.filter((l) => l.id !== row.id)];
      return logs.slice(0, PAGE_SIZE);
    });
  }, [offset]);
  const dataRef = useRef<RequestLog[] | null>(null);
  dataRef.current = merged ?? data?.logs ?? null;
  const { connected } = useLogStream({ onRunning, onLog }, offset === 0);

  const logs = useMemo(() => {
    if (offset !== 0) return data?.logs ?? [];
    return merged ?? data?.logs ?? [];
  }, [data, merged, offset]);
  const total = data?.total ?? 0;
  const page = Math.floor(offset / PAGE_SIZE) + 1;
  const pages = Math.max(Math.ceil(total / PAGE_SIZE), 1);
  const canPrev = offset > 0;
  const canNext = offset + PAGE_SIZE < total;

  // Render shapes: the grouping pass (chain marks + healed) runs over
  // running pseudo-rows + finished rows (running rows first — newest).
  // Sub-operations expand inline directly under their parent request as
  // nested `child` shapes (2026-10-07) — the parent→sub-op hierarchy.
  const shapes = useMemo<LogRowShape[]>(() => {
    const children = data?.children ?? {};
    // One source for both the chip count and the expansion — they can never
    // disagree about how many children a row has.
    const kidsOf = (id: number): ChildLog[] => children[String(id)] ?? [];
    const parents: ParentShape[] = [
      ...running.map((run) => ({ kind: 'running', run, chainStart: false, chainCont: false }) as ParentShape),
      ...logs.map((row) => ({
        kind: 'done',
        row,
        chainStart: false,
        chainCont: false,
        healed: intraRowHealed(row),
        childCount: kidsOf(row.id).length,
      }) as ParentShape),
    ];
    const marked = buildShapes(parents);
    // Expand after the marks: children never participate in the
    // conversation-chain adjacency (they are not requests). A child whose
    // parent continues the chain carries the cont mark so the conversation
    // line stays visually unbroken across the expansion.
    const out: LogRowShape[] = [];
    for (const s of marked) {
      out.push(s);
      if (s.kind === 'done') {
        const kidCont = s.chainCont === true;
        const parent = s.row;
        for (const child of kidsOf(s.row.id)) {
          out.push({ kind: 'child', child, chainCont: kidCont, onOpenParent: () => openTraceRef.current(parent) });
        }
      }
    }
    return out;
  }, [logs, running, data]);

  const openTrace = useCallback((l: RequestLog) => setSelected(l), []);
  const closeTrace = useCallback(() => setSelected(null), []);
  const openTraceRef = useRef(openTrace);
  openTraceRef.current = openTrace;

  return (
    <>
      <PageHead
        title="Logs"
        sub="Every request with its routing trace — fallback transitions and compatibility degradations are always visible here."
        actions={
          <>
            <span className={`badge ${connected ? 'up' : 'info'}`}>{connected ? 'LIVE' : 'POLLED'}</span>
            {data?.children_unavailable === true && (
              <span className="badge warn" title="The sub-operation read failed — fallback/emulation rows are missing from this page">
                sub-ops unavailable
              </span>
            )}
            <button type="button" className="btn btn-ghost btn-sm" onClick={reload}>
              Refresh
            </button>
          </>
        }
      />
      <div className="canvas">
        {error !== null ? (
          <div className="w-full">
            <ErrorNote message={error} />
          </div>
        ) : loading ? (
          <div className="w-full">
            <EmptyState title="Loading logs" body="Fetching the latest requests." />
          </div>
        ) : shapes.length === 0 ? (
          <div className="w-full">
            <EmptyState
              title="No requests logged"
              body="Once traffic flows through the proxy, each request appears here with status, latency, usage and its full route trace."
            />
          </div>
        ) : (
          <div className="w-full">
            <div className="table-wrap">
              <div className="table-scroll">
                <table className="table logs-table">
                  <thead>
                    <tr>
                      <th className="td-gutter" aria-label="chain" />
                      <th>Status</th>
                      <th>Model</th>
                      <th>Requested</th>
                      <th>Provider</th>
                      <th>Request Protocol</th>
                      <th>Account</th>
                      <th>API Key</th>
                      <th>Tokens</th>
                      <th className="num">TPS</th>
                      <th className="num">Duration</th>
                      <th>Time ↓</th>
                      <th>Conversation</th>
                    </tr>
                  </thead>
                  <tbody>
                    {shapes.map((shape, i) => {
                      const rowKey =
                        shape.kind === 'running'
                          ? shape.run.id
                          : shape.kind === 'child'
                            ? `c${shape.child.id}`
                            : shape.row.id;
                      return (
                        <LogRow
                          key={`${rowKey}-${i}`}
                          shape={shape}
                          onOpen={shape.kind === 'done' ? openTrace : undefined}
                        />
                      );
                    })}
                  </tbody>
                </table>
              </div>
              <div className="pagination">
                <span className="pg-info">
                  {total.toLocaleString('en-US')} requests — page {page} of {pages}
                </span>
                <div className="pg-nav">
                  <button
                    type="button"
                    className="btn btn-secondary btn-sm"
                    onClick={() => setOffset(Math.max(offset - PAGE_SIZE, 0))}
                    disabled={!canPrev}
                  >
                    <IconChevronLeft width="14" height="14" />
                    Previous
                  </button>
                  <button
                    type="button"
                    className="btn btn-secondary btn-sm"
                    onClick={() => setOffset(offset + PAGE_SIZE)}
                    disabled={!canNext}
                  >
                    Next
                    <IconChevronRight width="14" height="14" />
                  </button>
                </div>
              </div>
            </div>
          </div>
        )}
      </div>

      {selected !== null && (
        <Drawer title={`Request #${selected.id}`} onClose={closeTrace}>
          <LogDetail selected={selected} subOps={data?.children?.[String(selected.id)] ?? []} />
        </Drawer>
      )}
    </>
  );
}

// buildShapes: the client-side grouping pass over REQUEST rows (running
// pseudo-rows + finished rows; sub-operations never participate — they are
// not requests). Two jobs:
//  1. chain marks — adjacent rows (in render order) sharing a non-empty
//     conversation identity get start/cont marks; the gutter lines render
//     from them. A running row chains by its conv_key (its conversation_id
//     is only assigned at the terminal write).
//  2. cross-row heal — the previous row of the same conversation failed
//     (≥400) and this row is 2xx → healed chip (a retry recovered it).
function buildShapes(rows: ParentShape[]): ParentShape[] {
  const out = rows.map((s) => ({ ...s }));
  let prevConv: string | null = null;
  let prevFailed = false;
  for (const s of out) {
    // A running row chains by its conv_key (its real conversation_id is only
    // assigned at the terminal write); a finished row by its id.
    const conv =
      s.kind === 'done'
        ? (s.row.conversation_id ?? '')
        : s.kind === 'running'
          ? (s.run.conv_key ?? s.run.conversation_id ?? '')
          : '';
    if (conv !== '') {
      if (prevConv === conv) {
        s.chainCont = true;
      } else {
        s.chainStart = true;
      }
      prevConv = conv;
    } else {
      prevConv = null;
    }
    if (s.kind === 'done') {
      if (prevFailed && s.row.status < 400) {
        s.healed = true;
      }
      prevFailed = s.row.status >= 400;
    }
  }
  // Suppress start marks that never continue (no visible line otherwise).
  for (let i = 0; i < out.length; i++) {
    const s = out[i];
    if (s !== undefined && s.chainStart && !out[i + 1]?.chainCont) {
      s.chainStart = false;
    }
  }
  return out;
}

// intraRowHealed: request succeeded despite trace failures (failover,
// retry, capability_fallback, failed) — the request-level heal.
function intraRowHealed(row: RequestLog): boolean {
  if (row.status >= 400) return false;
  const trace = row.route_trace as { events?: Array<{ type?: string }> } | null;
  if (trace === null || trace === undefined || !Array.isArray(trace.events)) return false;
  return trace.events.some(
    (ev) =>
      ev !== null &&
      typeof ev === 'object' &&
      typeof ev.type === 'string' &&
      ['failed', 'failover', 'retry', 'capability_fallback'].includes(ev.type),
  );
}

function LogDetail({ selected, subOps }: { selected: RequestLog; subOps: ChildLog[] }) {
  const features = (selected.features ?? {}) as Record<string, boolean | undefined>;
  const featureKeys = ['streaming', 'tools', 'thinking', 'web_search', 'web_fetch', 'vision'] as const;
  const active = featureKeys.filter((k) => features[k] === true);
  const events = parseEvents(selected.route_trace);
  return (
    <div className="form">
      <div className="fieldset">
        <legend>Request</legend>
        <dl style={{ display: 'grid', gridTemplateColumns: 'auto 1fr', gap: '6px 16px', margin: 0, fontSize: 'var(--fs-sm)' }}>
          <dt className="muted">Time</dt>
          <dd className="mono" style={{ margin: 0 }}>{fmtTime(selected.timestamp)}</dd>
          <dt className="muted">Model</dt>
          <dd style={{ margin: 0 }}>{selected.model}</dd>
          <dt className="muted">Provider</dt>
          <dd style={{ margin: 0 }}>{selected.provider_id}</dd>
          <dt className="muted">Protocol</dt>
          <dd style={{ margin: 0 }}>{selected.protocol ?? '—'}</dd>
          <dt className="muted">Conversation</dt>
          <dd className="mono" style={{ margin: 0 }}>{selected.conversation_id ?? '—'}</dd>
          <dt className="muted">Client key</dt>
          <dd className="mono" style={{ margin: 0 }}>{selected.client_key_id}</dd>
          <dt className="muted">Status</dt>
          <dd style={{ margin: 0 }}>
            <span className={`badge ${statusBadge(selected.status).cls}`}>
              <span className="b-dot" />
              {selected.status}
            </span>
          </dd>
          <dt className="muted">Latency</dt>
          <dd className="mono" style={{ margin: 0 }}>{selected.latency_ms} ms</dd>
        </dl>
      </div>
      <div className="fieldset">
        <legend>Features</legend>
        {active.length === 0 ? (
          <p className="hint">No capability flags on this request.</p>
        ) : (
          <div style={{ display: 'flex', gap: 6, flexWrap: 'wrap' }}>
            {active.map((k) => (
              <span key={k} className="badge info">{k}</span>
            ))}
          </div>
        )}
      </div>
      {(selected.error_redacted ?? '') !== '' && (
        <div className="fieldset">
          <legend>Error (redacted)</legend>
          <pre className="mono" style={{ margin: 0, whiteSpace: 'pre-wrap', wordBreak: 'break-word', fontSize: 12 }}>
            {selected.error_redacted}
          </pre>
        </div>
      )}
      <div className="fieldset">
        <legend>Usage</legend>
        {selected.usage === null || selected.usage === undefined ? (
          <p className="hint">No usage recorded for this request.</p>
        ) : (
          <pre className="mono" style={{ margin: 0, whiteSpace: 'pre-wrap', wordBreak: 'break-word', fontSize: 12 }}>
            {JSON.stringify(selected.usage, null, 2)}
          </pre>
        )}
      </div>
      <div className="fieldset">
        <legend>Route trace</legend>
        {events === null ? (
          <p className="hint">No routing decisions recorded.</p>
        ) : (
          <>
            <ChainSummary events={events} />
            <TraceTimeline events={events} />
          </>
        )}
        {subOps.length > 0 && (
          <div className="child-ops">
            <p className="hint">Sub-operations</p>
            <ul className="child-op-list">
              {subOps.map((c) => (
                <li key={c.id}>
                  <span className={childCls(c.kind)}>{childTitle(c)}</span>
                  {childMeta(c) !== '' && <span className="child-op-detail mono">{childMeta(c)}</span>}
                </li>
              ))}
            </ul>
          </div>
        )}
        {selected.route_trace !== null && selected.route_trace !== undefined && (
          <RawTrace trace={selected.route_trace} />
        )}
      </div>
    </div>
  );
}