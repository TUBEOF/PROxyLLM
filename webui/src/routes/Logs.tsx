import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { apiCall } from '../api';
import type { ChildLog, LogsResponse, RequestLog, RunningRequest } from '../types';
import { EmptyState, ErrorNote, PageHead, useLiveData } from '../hooks';
import { useLogStream } from '../hooks/useLogStream';
import { Drawer } from '../components/Overlays';
import { LiveBadge } from '../components/LiveBadge';
import { IconChevronLeft, IconChevronRight } from '../icons';
import { LogRow, tpsOf, type LogRowShape, type ParentShape } from '../components/LogRow';
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

  // Render shapes: children expand inline directly under their parent
  // request as nested `child` shapes (2026-10-07). Chain marks are GONE
  // (2026-10-08 owner decision): conversation adjacency chained unrelated
  // requests (same system prompt ⇒ same conv id) — the gutter now renders
  // ONLY the parent→sub-op tree, i.e. how requests relate (retry /
  // fallback / emulated web tool), not which chat they belong to.
  const shapes = useMemo<LogRowShape[]>(() => {
    const children = data?.children ?? {};
    // One source for both the chip count and the expansion — they can never
    // disagree about how many children a row has.
    const kidsOf = (id: number): ChildLog[] => children[String(id)] ?? [];
    const parents: ParentShape[] = [
      ...running.map((run) => ({ kind: 'running', run }) as ParentShape),
      ...logs.map((row) => ({
        kind: 'done',
        row,
        healed: intraRowHealed(row),
        childCount: kidsOf(row.id).length,
      }) as ParentShape),
    ];
    const out: LogRowShape[] = [];
    for (const s of parents) {
      out.push(s);
      if (s.kind === 'done') {
        const parent = s.row;
        const kids = kidsOf(s.row.id);
        kids.forEach((child, ci) => {
          out.push({
            kind: 'child',
            child,
            last: ci === kids.length - 1,
            onOpenParent: () => openTraceRef.current(parent),
          });
        });
      }
    }
    return out;
  }, [logs, running, data]);

  // names maps a raw account id to its display name, for the trace
  // renderers (route_trace stores ids; the row payload carries the names).
  // Built from the page's own rows — no extra request.
  const names = useMemo(() => {
    const m = new Map<string, string>();
    for (const row of logs) {
      if (row.provider_name) m.set(row.provider_id, row.provider_name);
    }
    for (const row of data?.logs ?? []) {
      if (row.provider_name) m.set(row.provider_id, row.provider_name);
    }
    return m;
  }, [logs, data]);

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
            <LiveBadge connected={connected} />
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
                      <th className="td-gutter" aria-label="chain">
                        {/* A real, sized child: the tree graphics are
                            absolutely positioned, so an empty cell lets the
                            column collapse to zero and clip them away. */}
                        <span className="gutter-spacer" aria-hidden="true" />
                      </th>
                      <th>Status</th>
                      <th>Model</th>
                      <th>Provider</th>
                      <th>Protocol</th>
                      <th>Account</th>
                      <th>API Key</th>
                      <th>Tokens</th>
                      <th className="num">TPS</th>
                      <th className="num">Duration</th>
                      <th>Time ↓</th>
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
          <LogDetail
            selected={selected}
            subOps={data?.children?.[String(selected.id)] ?? []}
            names={names}
          />
        </Drawer>
      )}
    </>
  );
}

// buildShapes was removed (2026-10-08 owner decision): the conversation-
// adjacency chain marks chained UNRELATED requests — the `sys:` fallback key
// groups by (client key, system prompt), so every turn of every CLI session
// shared one conv id and the gutter linked everything into one line. The
// gutter now renders only the parent→sub-op tree (retry / fallback /
// emulated web tool), which is how requests actually relate; the
// conversation identity stays in the drawer's Conversation field.

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

function LogDetail({ selected, subOps, names }: { selected: RequestLog; subOps: ChildLog[]; names: Map<string, string> }) {
  const features = (selected.features ?? {}) as Record<string, boolean | undefined>;
  const featureKeys = ['streaming', 'tools', 'thinking', 'web_search', 'web_fetch', 'vision'] as const;
  const active = featureKeys.filter((k) => features[k] === true);
  const events = parseEvents(selected.route_trace);
  const u = (selected.usage ?? {}) as { input_tokens?: number; output_tokens?: number; cache_read_input_tokens?: number };
  const tps = tpsOf(u.output_tokens, selected.latency_ms);
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
          <dd style={{ margin: 0 }}>
            {selected.provider_kind ?? selected.provider_id}
            {selected.provider_name && selected.provider_name !== selected.provider_kind && (
              <span className="muted"> · {selected.provider_name}</span>
            )}
          </dd>
          <dt className="muted">Account</dt>
          <dd style={{ margin: 0 }} title={selected.provider_id}>{selected.provider_name ?? selected.provider_id}</dd>
          <dt className="muted">Protocol</dt>
          <dd style={{ margin: 0 }}>{selected.protocol ?? '—'}</dd>
          <dt className="muted">Conversation</dt>
          <dd className="mono" style={{ margin: 0 }}>{selected.conversation_id ?? '—'}</dd>
          <dt className="muted">Client key</dt>
          <dd className="mono" style={{ margin: 0 }} title={selected.client_key_id}>
            {selected.key_name
              ? selected.key_prefix
                ? `${selected.key_name} (${selected.key_prefix})`
                : selected.key_name
              : selected.client_key_id}
          </dd>
          <dt className="muted">Status</dt>
          <dd style={{ margin: 0 }}>
            <span className={`badge ${statusBadge(selected.status).cls}`}>
              <span className="b-dot" />
              {selected.status}
            </span>
          </dd>
          <dt className="muted">Duration</dt>
          <dd className="mono" style={{ margin: 0 }}>{selected.latency_ms} ms</dd>
          <dt className="muted">TPS</dt>
          <dd className="mono" style={{ margin: 0 }}>{tps}</dd>
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
            <ChainSummary events={events} names={names} />
            <TraceTimeline events={events} names={names} />
          </>
        )}
        {subOps.length > 0 && (
          <div className="child-ops">
            <p className="hint">Sub-operations</p>
            <ul className="child-op-list">
              {subOps.map((c) => (
                <li key={c.id}>
                  <span className={childCls(c.kind)}>{childTitle(c)}</span>
                  {(c.model ?? '') !== '' && <span className="mono">{c.model}</span>}
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