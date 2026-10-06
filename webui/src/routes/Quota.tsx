import { useMemo } from 'react';
import { apiCall } from '../api';
import type { AccountUsage, AccountUsageResponse } from '../types';
import { EmptyState, ErrorNote, PageHead, useLiveData, useMutation } from '../hooks';
import { IconGauge, IconPlug } from '../icons';

// Quota (ADR-012): per-account usage/limit cards — the OmniRoute-style
// view. Live source (the ollama.com settings scrape) renders the session/
// weekly bars with reset timers + plan tier; accounts without a scrape
// render the LOCAL month window (labeled) — never fabricated metrics.
// Live updates (2026-10-06): the card list polls itself (POLL_MS).
// Health badge: breaker/quota cooldown from the routing HealthTracker.

const POLL_MS = 5000;

// UsageBar renders one percent bar (the kit .bar-* classes; the value
// rides .bar-val — masked for visual e2e determinism).
function UsageBar({ label, pct, reset }: { label: string; pct: number | null; reset?: number }) {
  return (
    <div className="bar-row">
      <span className="bar-lab">{label}</span>
      <span className="bar-val">{pct === null ? '—' : `${Math.round(pct)}%`}</span>
      <div className="bar-track">
        <div
          className={`bar-fill${pct !== null && pct >= 80 ? ' warn' : ''}`}
          style={{ width: `${Math.min(Math.max(pct ?? 0, 0), 100)}%` }}
        />
      </div>
      {reset !== undefined && reset > 0 && (
        <span className="hint" style={{ margin: 0, fontSize: 'var(--fs-xs)' }}>
          {`resets ${new Date(reset * 1000).toISOString().slice(0, 16).replace('T', ' ')} UTC`}
        </span>
      )}
    </div>
  );
}

function healthBadge(health: string | undefined): { label: string; cls: string } | null {
  if (health === 'quota_exhausted') return { label: 'quota', cls: 'warn' };
  if (health === 'breaker') return { label: 'cooling', cls: 'down' };
  return null;
}

export default function Quota() {
  const { data, loading, error, reload, updatedAt } = useLiveData<AccountUsageResponse>(
    (signal) => apiCall<AccountUsageResponse>('GET', '/api/admin/accounts/usage', undefined, signal),
    POLL_MS,
  );
  const refresh = useMutation();

  const accounts = useMemo(() => data?.accounts ?? [], [data]);
  const healthy = useMemo(() => accounts.filter((a) => a.health === undefined).length, [accounts]);
  const cooling = accounts.length - healthy;

  const refreshOne = (account: AccountUsage) =>
    refresh.run(() =>
      apiCall('POST', `/api/admin/accounts/usage/refresh/${encodeURIComponent(account.account_id)}`, {}),
    );
  const refreshAll = async () => {
    // Sequential per-card refreshes — the bounded-concurrency pool lives
    // server-side when a periodic refresher is added (ADR-012).
    for (const a of accounts) {
      if (a.type === 'ollama-cloud') {
        await refreshOne(a);
      }
    }
    reload();
  };

  return (
    <>
      <PageHead
        title="Quota"
        sub="Per-account usage and limits — live windows when the provider offers them, local counting otherwise."
        actions={
          <>
            {updatedAt !== null && <span className="badge info">live</span>}
            <button type="button" className="btn btn-ghost btn-sm" onClick={refreshAll} disabled={refresh.pending}>
              Refresh all
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
            <EmptyState title="Loading usage" body="Fetching the account snapshot." />
          </div>
        ) : accounts.length === 0 ? (
          <div className="w-full">
            <EmptyState
              title="No accounts"
              body="Add a provider account first — usage appears per account once one exists."
            />
          </div>
        ) : (
          <div className="w-full">
            {/* Honest summary tiles (kit .kpi; values masked in e2e). */}
            <div className="kpis" style={{ display: 'flex', gap: 8, marginBottom: 12 }}>
              <div className="widget w-1" style={{ padding: '10px 14px' }}>
                <div className="kpi">
                  <div>
                    <span className="hint">TOTAL</span>
                    <div className="kpi-value">{accounts.length}</div>
                  </div>
                  <span className="ic"><IconGauge width="18" height="18" /></span>
                </div>
              </div>
              <div className="widget w-1" style={{ padding: '10px 14px' }}>
                <div className="kpi">
                  <div>
                    <span className="hint">HEALTHY</span>
                    <div className="kpi-value">{healthy}</div>
                  </div>
                  <span className="ic"><IconPlug width="18" height="18" /></span>
                </div>
              </div>
              <div className="widget w-1" style={{ padding: '10px 14px' }}>
                <div className="kpi">
                  <div>
                    <span className="hint">COOLING</span>
                    <div className="kpi-value">{cooling}</div>
                  </div>
                  <span className="ic"><IconGauge width="18" height="18" /></span>
                </div>
              </div>
            </div>
            <div className="cards" style={{ display: 'grid', gap: 8 }}>
              {accounts.map((a) => {
                const hb = healthBadge(a.health);
                return (
                  <div key={a.account_id} className="widget w-full" style={{ padding: '12px 14px' }}>
                    <div className="widget-head" style={{ marginBottom: 6 }}>
                      <h3 className="title">
                        <span className="ic"><IconPlug width="14" height="14" /></span>
                        {a.name}
                        {a.live?.plan && (
                          <span className="badge info" style={{ marginLeft: 6 }}>{a.live.plan}</span>
                        )}
                        {hb && <span className={`badge ${hb.cls}`} style={{ marginLeft: 6 }}>{hb.label}</span>}
                        {a.source === 'ollama' ? (
                          <span className="badge up" style={{ marginLeft: 6 }}>live</span>
                        ) : (
                          <span className="badge paused" style={{ marginLeft: 6 }}>local</span>
                        )}
                        <span className="mono hint" style={{ marginLeft: 8, fontSize: 'var(--fs-xs)' }}>{a.account_id}</span>
                      </h3>
                    </div>
                    {a.live !== null && a.live !== undefined ? (
                      <div className="bars">
                        <UsageBar label="Session" pct={a.live.session_pct ?? null} reset={a.live.session_reset} />
                        <UsageBar label="Weekly" pct={a.live.weekly_pct ?? null} reset={a.live.weekly_reset} />
                      </div>
                    ) : (
                      <EmptyState
                        title="No live usage"
                        body={a.error !== undefined && a.error !== ''
                          ? `Last scrape failed: ${a.error}. Set a usage cookie on the account to enable live windows.`
                          : 'Set a usage cookie on this account (Provider type → account form) to fetch the live windows; the local count shows meanwhile.'}
                      />
                    )}
                    {a.local !== null && a.local !== undefined && (
                      <div className="hint" style={{ marginTop: 4, fontSize: 'var(--fs-xs)' }}>
                        {`local month: ${a.local.requests} requests (${a.local.failures} failed), ${a.local.input_tokens} in / ${a.local.output_tokens} out`}
                      </div>
                    )}
                    <div style={{ textAlign: 'right' }}>
                      <button
                        type="button"
                        className="btn btn-ghost btn-sm"
                        onClick={() => void refreshOne(a)}
                        disabled={refresh.pending}
                      >
                        Refresh now
                      </button>
                    </div>
                  </div>
                );
              })}
            </div>
          </div>
        )}
      </div>
    </>
  );
}