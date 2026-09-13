import { useCallback, useMemo, useState } from 'react';
import { apiCall } from '../api';
import type { LogsResponse, RequestLog } from '../types';
import { EmptyState, ErrorNote, PageHead, useAdminData } from '../hooks';
import { Drawer } from '../components/Overlays';
import { IconChevronLeft, IconChevronRight } from '../icons';

// Logs (plan §9): paginated request log with the route_trace drawer — the
// surface where fallback transitions and compat degradations become
// visible (Analysis E: never silent). Pagination rides the API's
// limit/offset contract; `total` from the envelope drives the pager.

const PAGE_SIZE = 50;

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

  const { data, loading, error, reload } = useAdminData<LogsResponse>(
    (signal) =>
      apiCall<LogsResponse>('GET', `/api/admin/logs?limit=${PAGE_SIZE}&offset=${offset}`, undefined, signal),
    [offset],
  );

  const logs = useMemo(() => data?.logs ?? [], [data]);
  const total = data?.total ?? 0;
  const page = Math.floor(offset / PAGE_SIZE) + 1;
  const pages = Math.max(Math.ceil(total / PAGE_SIZE), 1);
  const canPrev = offset > 0;
  const canNext = offset + PAGE_SIZE < total;

  const openTrace = useCallback((l: RequestLog) => setSelected(l), []);
  const closeTrace = useCallback(() => setSelected(null), []);

  return (
    <>
      <PageHead
        title="Logs"
        sub="Every request with its routing trace — fallback transitions and compatibility degradations are always visible here."
        actions={
          <button type="button" className="btn btn-ghost btn-sm" onClick={reload}>
            Refresh
          </button>
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
        ) : logs.length === 0 ? (
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
                <table className="table">
                  <thead>
                    <tr>
                      <th>Time</th>
                      <th>Model</th>
                      <th>Provider</th>
                      <th>Status</th>
                      <th className="num">Latency</th>
                      <th className="num">ID</th>
                    </tr>
                  </thead>
                  <tbody>
                    {logs.map((l) => {
                      const st = statusBadge(l.status);
                      return (
                        <tr key={l.id} onClick={() => openTrace(l)}>
                          <td className="mono">{fmtTime(l.timestamp)}</td>
                          <td className="primary">{l.model}</td>
                          <td>{l.provider_id}</td>
                          <td>
                            <span className={`badge ${st.cls}`}>
                              <span className="b-dot" />
                              {st.label}
                            </span>
                          </td>
                          <td className="num">{l.latency_ms} ms</td>
                          <td className="num">{l.id}</td>
                        </tr>
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
              {selected.route_trace === null || selected.route_trace === undefined ? (
                <p className="hint">No routing decisions recorded.</p>
              ) : (
                <pre className="mono" style={{ margin: 0, whiteSpace: 'pre-wrap', wordBreak: 'break-word', fontSize: 12 }}>
                  {JSON.stringify(selected.route_trace, null, 2)}
                </pre>
              )}
            </div>
          </div>
        </Drawer>
      )}
    </>
  );
}