import { useMemo } from 'react';
import { apiCall } from '../api';
import type { UsageResponse, UsageRow } from '../types';
import { EmptyState, ErrorNote, PageHead, useLiveData } from '../hooks';
import { LiveBadge } from '../components/LiveBadge';
import { IconBoxes, IconGauge, IconPlug, IconRoute } from '../icons';

// Overview: honest usage dashboard (plan §9). Data source is the single
// usage endpoint (GET /api/admin/usage?from&to) — day-granular rows. What
// the day rows support gets shown; what they cannot (hourly heatmap, live
// per-minute series) gets an honest empty state, never fabricated metrics.

function sum(rows: UsageRow[], field: keyof UsageRow): number {
  let total = 0;
  for (const r of rows) {
    total += Number(r[field]);
  }
  return total;
}

// Sparkline over the day rows: polyline + area fill (kit .spark classes,
// hand-built SVG per the chart section).
function Spark({ values }: { values: number[] }) {
  const W = 300;
  const H = 54;
  const PAD = 4;
  const n = values.length;
  if (n === 0) return null;
  const max = Math.max(...values, 1);
  const step = n === 1 ? 0 : (W - PAD * 2) / (n - 1);
  const pts = values.map((v, i) => {
    const x = PAD + i * step;
    const y = H - PAD - (v / max) * (H - PAD * 2);
    return [x, y] as const;
  });
  const line = pts.map(([x, y]) => `${x.toFixed(1)},${y.toFixed(1)}`).join(' ');
  const area = `M${PAD},${H - PAD} L${line.replaceAll(' ', ' L')} L${(W - PAD).toFixed(1)},${H - PAD} Z`;
  return (
    <svg className="spark" viewBox={`0 0 ${W} ${H}`} preserveAspectRatio="none" aria-hidden="true">
      <path className="spark-area" d={area} />
      <polyline className="spark-line" points={line} />
      {pts.map(([x, y], i) => (
        <circle key={i} className="spark-pt" cx={x} cy={y} r="2" />
      ))}
    </svg>
  );
}

// Failure ring (kit .ring): share of failed requests, green segment on
// neutral track — one green accent, status colors only where earned.
function Ring({ pct }: { pct: number }) {
  const R = 54;
  const C = 2 * Math.PI * R;
  const seg = C * Math.min(Math.max(pct, 0), 1);
  return (
    <svg className="ring" viewBox="0 0 128 128" aria-hidden="true">
      <circle className="ring-track" cx="64" cy="64" r={R} />
      <circle
        className="ring-seg"
        cx="64"
        cy="64"
        r={R}
        stroke="var(--chart-line)"
        strokeDasharray={`${seg} ${C}`}
        transform="rotate(-90 64 64)"
      />
    </svg>
  );
}

// POLL_MS is the Overview's live-update poll interval.
const POLL_MS = 5000;

export default function Overview() {
  // Last 30 days, inclusive — the honest window the usage endpoint serves.
  // Live updates: the snapshot poll replaces the manual
  // reload; the day-window recomputes only when a poll crosses midnight
  // (the data itself is keyed by day, so a stale window label is the only
  // cost — acceptable against a hidden-tab-paused poll).
  const { from, to } = useMemo(() => {
    const today = new Date();
    const start = new Date(today.getTime() - 29 * 86_400_000);
    return { from: start.toISOString().slice(0, 10), to: today.toISOString().slice(0, 10) };
  }, []);

  const { data, loading, error, reload, updatedAt } = useLiveData<UsageResponse>(
    (signal) => apiCall<UsageResponse>('GET', `/api/admin/usage?from=${from}&to=${to}`, undefined, signal),
    POLL_MS,
  );

  const rows = useMemo(() => data?.rows ?? [], [data]);

  // Day-granular series for the sparkline (requests per day).
  const perDay = useMemo(() => {
    const byDay = new Map<string, number>();
    for (const r of rows) {
      byDay.set(r.day, (byDay.get(r.day) ?? 0) + r.requests);
    }
    return [...byDay.entries()].sort(([a], [b]) => (a < b ? -1 : 1)).map(([, v]) => v);
  }, [rows]);

  const totals = useMemo(
    () => ({
      requests: sum(rows, 'requests'),
      inputTokens: sum(rows, 'input_tokens'),
      outputTokens: sum(rows, 'output_tokens'),
      failures: sum(rows, 'failures'),
    }),
    [rows],
  );

  // Per-model request share (kit .bars): green fill for the top entry,
  // neutral comparison for the rest.
  const byModel = useMemo(() => {
    const by = new Map<string, number>();
    for (const r of rows) {
      by.set(r.model, (by.get(r.model) ?? 0) + r.requests);
    }
    const entries = [...by.entries()].sort((a, b) => b[1] - a[1]);
    return { list: entries.slice(0, 6), max: entries[0]?.[1] ?? 1 };
  }, [rows]);

  const failurePct = totals.requests === 0 ? 0 : totals.failures / totals.requests;

  return (
    <>
      <PageHead
        title="Overview"
        sub={`Requests, tokens and failures over the last 30 days (${from} to ${to}).`}
        actions={
          <>
            <LiveBadge updatedAt={updatedAt} intervalMs={POLL_MS} />
            <button type="button" className="btn btn-ghost btn-sm" onClick={() => reload()}>
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
            <EmptyState title="Loading usage data" body="Fetching the last 30 days from the proxy." />
          </div>
        ) : rows.length === 0 ? (
          <div className="w-full">
            <EmptyState
              title="No usage recorded yet"
              body="Send requests through the proxy — token counts, request volumes and failure rates appear here once traffic flows. Configure a provider and a routing rule first."
            />
          </div>
        ) : (
          <>
            <div className="widget w-1">
              <div className="widget-head">
                <h3 className="title">
                  <span className="ic">
                    <IconGauge width="16" height="16" />
                  </span>
                  Requests
                </h3>
              </div>
              <div className="widget-body">
                <div className="kpi">
                  <div className="kpi-value">{totals.requests.toLocaleString('en-US')}</div>
                  <div className="kpi-foot">
                    <span className="delta neu">{perDay.length} active days</span>
                  </div>
                </div>
                <Spark values={perDay} />
              </div>
            </div>

            <div className="widget w-1">
              <div className="widget-head">
                <h3 className="title">
                  <span className="ic">
                    <IconBoxes width="16" height="16" />
                  </span>
                  Input tokens
                </h3>
              </div>
              <div className="widget-body">
                <div className="kpi">
                  <div className="kpi-value">{totals.inputTokens.toLocaleString('en-US')}</div>
                  <div className="kpi-foot">
                    <span className="delta neu">30-day total</span>
                  </div>
                </div>
              </div>
            </div>

            <div className="widget w-1">
              <div className="widget-head">
                <h3 className="title">
                  <span className="ic">
                    <IconBoxes width="16" height="16" />
                  </span>
                  Output tokens
                </h3>
              </div>
              <div className="widget-body">
                <div className="kpi">
                  <div className="kpi-value">{totals.outputTokens.toLocaleString('en-US')}</div>
                  <div className="kpi-foot">
                    <span className="delta neu">30-day total</span>
                  </div>
                </div>
              </div>
            </div>

            <div className="widget w-1">
              <div className="widget-head">
                <h3 className="title">
                  <span className="ic">
                    <IconRoute width="16" height="16" />
                  </span>
                  Failure rate
                </h3>
              </div>
              <div className="widget-body">
                <div className="kpi">
                  <div className="kpi-value">{(failurePct * 100).toFixed(1)}%</div>
                  <Ring pct={failurePct} />
                </div>
              </div>
            </div>

            <div className="widget w-2">
              <div className="widget-head">
                <h3 className="title">
                  <span className="ic">
                    <IconPlug width="16" height="16" />
                  </span>
                  Requests by model
                </h3>
              </div>
              <div className="widget-body">
                {byModel.list.length === 0 ? (
                  <EmptyState title="No model traffic" body="Requests appear here once models serve traffic." />
                ) : (
                  <div className="bars">
                    {byModel.list.map(([model, count], i) => (
                      <div key={model} className="bar-row">
                        <span className="bar-lab">{model}</span>
                        <span className="bar-val">{count.toLocaleString('en-US')}</span>
                        <div className="bar-track">
                          <div
                            className={`bar-fill${i === 0 ? '' : ' alt'}`}
                            style={{ width: `${(count / byModel.max) * 100}%` }}
                          />
                        </div>
                      </div>
                    ))}
                  </div>
                )}
              </div>
            </div>
          </>
        )}
      </div>
    </>
  );
}