import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { apiCall } from '../api';
import type { ChildLog, LogPayload, LogsResponse, RequestLog, RunningRequest, SettingsResponse } from '../types';
import { EmptyState, ErrorNote, PageHead, useAdminData, useLiveData, useMutation } from '../hooks';
import { useLogStream } from '../hooks/useLogStream';
import { Drawer } from '../components/Overlays';
import { LiveBadge } from '../components/LiveBadge';
import { IconChevronLeft, IconChevronRight } from '../icons';
import { LogRow, keyDisplay, tokensCell, tpsOf, type LogRowShape, type ParentShape } from '../components/LogRow';
import {
  ChainSummary,
  TraceTimeline,
  RawTrace,
  aliasHop,
  parseEvents,
  childTitle,
  childCls,
  childMeta,
} from '../components/RouteTrace';

// CAPTURE_KEY is the KV the request path reads on every request to decide
// whether to store the conversation and the answer beside the log row; the
// same key the admin payload endpoint reports on.
const CAPTURE_KEY = 'request_logging_enabled';

// Logs (Logs-page redesign, 2026-10-06): the dense request log — status /
// model (an alias request names its configured target inline) / provider /
// protocol / account / api key / tokens / tps / duration / time ↓ /
// conversation, chained rows in the gutter, healing on retried
// conversations, in-flight rows live over SSE.
// Pagination rides the API's limit/offset contract (the page fetch stays
// the paging/backfill source). On page 1 the SSE feed maintains the
// running pseudo-rows and prepends finished rows; any page fetch (poll or
// manual Refresh) resets that merge — the fetched page is the truth.

const DEFAULT_PAGE_SIZE = 50;

// PAGE_SIZE_OPTIONS are the rows-per-page the operator can pick. The backend
// takes any positive limit, so this list is a UI affordance, not a bound.
const PAGE_SIZE_OPTIONS = [25, 50, 100, 200];

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
  const [pageSize, setPageSize] = useState(DEFAULT_PAGE_SIZE);
  const [selected, setSelected] = useState<RequestLog | null>(null);
  const [running, setRunning] = useState<RunningRequest[]>([]);

  const { data, loading, error, reload } = useLiveData<LogsResponse>(
    (signal) =>
      apiCall<LogsResponse>('GET', `/api/admin/logs?limit=${pageSize}&offset=${offset}`, undefined, signal),
    POLL_MS,
    [offset, pageSize],
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
      return logs.slice(0, pageSize);
    });
  }, [offset, pageSize]);
  const dataRef = useRef<RequestLog[] | null>(null);
  dataRef.current = merged ?? data?.logs ?? null;
  const { connected } = useLogStream({ onRunning, onLog }, offset === 0);

  const logs = useMemo(() => {
    if (offset !== 0) return data?.logs ?? [];
    return merged ?? data?.logs ?? [];
  }, [data, merged, offset]);
  const total = data?.total ?? 0;
  const page = Math.floor(offset / pageSize) + 1;
  const pages = Math.max(Math.ceil(total / pageSize), 1);
  const canPrev = offset > 0;
  const canNext = offset + pageSize < total;

  // Render shapes: children expand inline directly under their parent
  // request as nested `child` shapes. Chain marks are GONE
  //: conversation adjacency chained unrelated
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
            onOpenChild: () => openChildRef.current(child, parent),
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

  // Exactly one drawer at a time: the two views are a step and the request it
  // belongs to, and each links to the other.
  const openTrace = useCallback((l: RequestLog) => {
    setSelectedChild(null);
    setSelected(l);
  }, []);
  const closeTrace = useCallback(() => setSelected(null), []);
  const openTraceRef = useRef(openTrace);
  openTraceRef.current = openTrace;

  // A sub-operation opens its OWN detail view: what that step sent and received
  // is a payload of its own, and the parent's conversation cannot answer it.
  // Plain functions, not useCallback: both reach the render tree through a ref
  // that is refreshed every render, so a stable identity buys nothing.
  const [selectedChild, setSelectedChild] = useState<{ child: ChildLog; parent: RequestLog } | null>(null);
  const openChild = (child: ChildLog, parent: RequestLog) => {
    setSelected(null);
    setSelectedChild({ child, parent });
  };
  const closeChild = () => setSelectedChild(null);
  const openChildRef = useRef(openChild);
  openChildRef.current = openChild;

  // Capture switch: the same KV the request path reads per
  // request, so a flip takes effect on the next request with no restart. It
  // writes through the generic settings API — no bespoke endpoint for one
  // boolean.
  const { data: settingsData, reload: reloadSettings } = useAdminData<SettingsResponse>(
    (signal) => apiCall<SettingsResponse>('GET', '/api/admin/settings', undefined, signal),
  );
  const captureEnabled = useMemo(() => {
    const raw = settingsData?.entries.find((e) => e.key === CAPTURE_KEY)?.value ?? '';
    return raw !== '0';
  }, [settingsData]);
  const saveCapture = useMutation();
  const toggleCapture = useCallback(
    async (enabled: boolean) => {
      const ok = await saveCapture.run(() =>
        apiCall('PUT', '/api/admin/settings', { key: CAPTURE_KEY, value: enabled ? '1' : '0' }),
      );
      if (ok !== null) reloadSettings();
    },
    [saveCapture, reloadSettings],
  );

  return (
    <>
      <PageHead
        title="Logs"
        sub="Every request with its routing trace — fallback transitions and compatibility degradations are always visible here."
        actions={
          <>
            <label
              className="toggle"
              title="Store each request's conversation and answer beside its log row. Turning it off keeps the log; only the captured text stops."
            >
              <input
                type="checkbox"
                checked={captureEnabled}
                onChange={(e) => void toggleCapture(e.target.checked)}
                disabled={saveCapture.pending}
              />
              <span className="tswitch" />
              capture bodies
            </label>
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
            {/* List controls at the top of the list they control: how many
                rows, and which page. The page fetch is the paging source, so
                both drive the same offset/limit the query already takes. */}
            <div className="table-toolbar">
              <span className="count">
                {total.toLocaleString('en-US')} requests — page {page} of {pages}
              </span>
              <div className="pg-nav">
                <label className="hint" htmlFor="log-page-size">Rows</label>
                <span className="select">
                  <select
                    id="log-page-size"
                    value={pageSize}
                    onChange={(e) => {
                      setPageSize(Number(e.target.value));
                      // A new page size invalidates the current offset.
                      setOffset(0);
                    }}
                  >
                    {PAGE_SIZE_OPTIONS.map((n) => (
                      <option key={n} value={n}>{n}</option>
                    ))}
                  </select>
                </span>
                <button
                  type="button"
                  className="btn btn-secondary btn-sm"
                  onClick={() => setOffset(Math.max(offset - pageSize, 0))}
                  disabled={!canPrev}
                >
                  <IconChevronLeft width="14" height="14" />
                  Previous
                </button>
                <button
                  type="button"
                  className="btn btn-secondary btn-sm"
                  onClick={() => setOffset(offset + pageSize)}
                  disabled={!canNext}
                >
                  Next
                  <IconChevronRight width="14" height="14" />
                </button>
              </div>
            </div>
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
            </div>
          </div>
        )}
      </div>

      {selected !== null && (
        <Drawer title={`Request #${selected.id}`} onClose={closeTrace} size="panel">
          <LogDetail
            selected={selected}
            subOps={data?.children?.[String(selected.id)] ?? []}
            names={names}
            onOpenChild={(c) => openChild(c, selected)}
          />
        </Drawer>
      )}

      {selectedChild !== null && (
        <Drawer title={childTitle(selectedChild.child)} onClose={closeChild} size="panel">
          <ChildDetail
            child={selectedChild.child}
            parent={selectedChild.parent}
            names={names}
            onOpenParent={() => openTrace(selectedChild.parent)}
          />
        </Drawer>
      )}
    </>
  );
}

// buildShapes was removed: the conversation-
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

// PayloadSection renders a captured body — the conversation a request carried
// and the answer it produced, or, with owner="child", what one sub-operation
// sent and received. Fetched when the drawer opens rather than
// shipped with the row: a payload is prompt-sized, and the list is polled every
// few seconds.
//
// The states are kept distinct on purpose — capture off, nothing captured for
// this row, or captured — so an empty viewer is never read as an empty request.
function PayloadSection({ id, owner = 'request' }: { id: number; owner?: 'request' | 'child' }) {
  const { data, loading } = useAdminData<LogPayload>(
    (signal) =>
      apiCall<LogPayload>('GET', `/api/admin/log-payload?owner=${owner}&id=${id}`, undefined, signal),
  );
  const what = owner === 'child' ? 'this step' : 'this request';
  // A step's payload is what it sent and received; a request's is the
  // conversation and the answer. Same viewer, honest labels.
  const legend = owner === 'child' ? 'Sent & received' : 'Context & answer';
  const firstTitle = owner === 'child' ? 'Sent' : 'Context';
  const secondTitle = owner === 'child' ? 'Received' : 'Answer';

  if (loading) {
    return (
      <div className="fieldset">
        <legend>{legend}</legend>
        <p className="hint">Loading the captured {owner === 'child' ? 'exchange' : 'conversation'}…</p>
      </div>
    );
  }
  if (data === null || !data.found) {
    return (
      <div className="fieldset">
        <legend>{legend}</legend>
        <p className="hint">
          {data !== null && data.capture_enabled === false
            ? owner === 'request'
              ? 'Body capture is off, so only the metadata was recorded. Turn it on above to capture from the next request on.'
              : 'Body capture is off, so only the metadata was recorded — nothing was stored for this step either.'
            : `Nothing was captured for ${what}.`}
        </p>
      </div>
    );
  }
  return (
    <>
      {(data.client_request !== undefined || data.client_response !== undefined) && (
        <div className="fieldset">
          <legend>{legend}</legend>
          {data.truncated === true && (
            <p className="hint">
              Truncated at 64 KB per side — the preview is the start of what was stored.
            </p>
          )}
          {data.client_request !== undefined && <CapturedBlock title={firstTitle} body={data.client_request} />}
          {data.client_response !== undefined && <CapturedBlock title={secondTitle} body={data.client_response} />}
        </div>
      )}
    </>
  );
}

// CapturedBlock renders one captured payload. The stored shape is a display
// projection — part kinds, tool names, image media types and sizes, never image
// bytes — so it is shown as JSON rather than reassembled into a transcript: a
// projection read as a transcript would misrepresent what the model received.
function CapturedBlock({ title, body }: { title: string; body: string }) {
  let pretty = body;
  try {
    pretty = JSON.stringify(JSON.parse(body), null, 2);
  } catch {
    // A truncation envelope or an unexpected value: show it as it was stored.
  }
  return (
    <div style={{ marginBottom: 10 }}>
      <div className="muted" style={{ fontSize: 12, marginBottom: 4 }}>{title}</div>
      <pre
        className="mono"
        style={{ margin: 0, whiteSpace: 'pre-wrap', wordBreak: 'break-word', fontSize: 12, maxHeight: 260, overflow: 'auto' }}
      >
        {pretty}
      </pre>
    </div>
  );
}

// ChildDetail is one sub-operation's own view: what the step was
// (kind, model, account), when it ran, and — the reason the row opens at all —
// what it sent and received. The parent request is one button away: the step
// answers "what did this do", the request answers "what was said".
function ChildDetail({
  child,
  parent,
  names,
  onOpenParent,
}: {
  child: ChildLog;
  parent: RequestLog;
  names: Map<string, string>;
  onOpenParent: () => void;
}) {
  const account = child.provider_name ?? (child.provider_id ? names.get(child.provider_id) : undefined);
  const childTokens = tokensCell(child.usage);
  return (
    <div className="form">
      <div className="fieldset">
        <legend>Step</legend>
        <dl style={{ display: 'grid', gridTemplateColumns: 'auto 1fr', gap: '6px 16px', margin: 0, fontSize: 'var(--fs-sm)' }}>
          <dt className="muted">Kind</dt>
          <dd style={{ margin: 0 }}>
            <span className={childCls(child.kind)}>{childTitle(child)}</span>
          </dd>
          <dt className="muted">Model</dt>
          <dd style={{ margin: 0 }}>{child.model ?? '—'}</dd>
          <dt className="muted">Provider</dt>
          <dd style={{ margin: 0 }}>{child.provider_kind ?? child.provider_id ?? '—'}</dd>
          <dt className="muted">Account</dt>
          <dd className="mono" style={{ margin: 0 }} title={child.provider_id ?? undefined}>
            {account ?? child.provider_id ?? '—'}
          </dd>
          <dt className="muted">When</dt>
          <dd className="mono" style={{ margin: 0 }}>{fmtTime(child.ts)}</dd>
          <dt className="muted">Duration</dt>
          <dd className="mono" style={{ margin: 0 }}>
            {child.latency_ms ? `${child.latency_ms} ms` : '—'}
          </dd>
          <dt className="muted">Tokens</dt>
          <dd className="mono" style={{ margin: 0 }}>
            <span className="txt-muted">TI:</span> {childTokens[0]}{' '}
            <span className="txt-muted">TO:</span> {childTokens[1]}{' '}
            <span className="txt-muted">CR:</span> {childTokens[2]}
          </dd>
          <dt className="muted">Key</dt>
          <dd className="mono" style={{ margin: 0 }}>
            {keyDisplay(child.key_name, child.key_prefix, child.client_key_id)}
          </dd>
          <dt className="muted">Request</dt>
          <dd className="mono" style={{ margin: 0 }}>#{parent.id} · {parent.model}</dd>
        </dl>
      </div>
      {/* The step's own body: the tool exchange it answered, or the caption it
          produced. This is what the row could never show. */}
      <PayloadSection id={child.id} owner="child" />
      <div className="fieldset">
        <legend>Parent request</legend>
        <button type="button" className="btn btn-secondary" onClick={onOpenParent}>
          Open request #{parent.id}
        </button>
        <p className="hint">
          The body of this step is above. The request holds the conversation and the answer it
          belongs to.
        </p>
      </div>
    </div>
  );
}

function LogDetail({ selected, subOps, names, onOpenChild }: { selected: RequestLog; subOps: ChildLog[]; names: Map<string, string>; onOpenChild?: (c: ChildLog) => void }) {
  const features = (selected.features ?? {}) as Record<string, boolean | undefined>;
  const featureKeys = ['streaming', 'tools', 'thinking', 'web_search', 'web_fetch', 'vision', 'decision'] as const;
  const active = featureKeys.filter((k) => features[k] === true);
  const events = parseEvents(selected.route_trace);
  const alias = aliasHop(selected.route_trace);
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
          <dd style={{ margin: 0 }}>
            {selected.model}
            {/* The alias hop: the model field names what the client asked
                for; this line names the configured target the alias entry
                resolved it to (the trace's alias event). */}
            {alias !== null && <div className="muted alias-hop">alias → {alias.to}</div>}
          </dd>
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
      {/* The conversation and the answer, before the derived views: what was
          actually said is what an operator opens a log entry to read. */}
      <PayloadSection id={selected.id} />
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
                  {/* Each entry opens that step's own view — the
                      same destination as its nested row in the table. */}
                  <button
                    type="button"
                    className="child-op-open"
                    onClick={onOpenChild ? () => onOpenChild(c) : undefined}
                    disabled={!onOpenChild}
                    title="Show what this step sent and received"
                  >
                    <span className={childCls(c.kind)}>{childTitle(c)}</span>
                    {(c.model ?? '') !== '' && <span className="mono">{c.model}</span>}
                    {childMeta(c) !== '' && <span className="child-op-detail mono">{childMeta(c)}</span>}
                  </button>
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