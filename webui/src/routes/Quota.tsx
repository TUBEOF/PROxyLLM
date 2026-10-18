import { useId, useMemo, useState } from 'react';
import { apiCall } from '../api';
import { formatCountdown, formatResetInstant } from '../time';
import type { AccountUsage, AccountUsageResponse, CreditsUsage } from '../types';
import { EmptyState, ErrorNote, PageHead, useLiveData, useMutation, useNow } from '../hooks';
import { LiveBadge } from '../components/LiveBadge';
import { IconBoxes, IconPlug, IconRefresh } from '../icons';

// Quota (ADR-012, amendment 2026-10-17): per-account LIMIT cards. The card
// shows what the account itself reports — Ollama Cloud's session/weekly
// windows and Firecrawl's team credits — and nothing the proxy measured on
// its own (the local month aggregate was retired; the Overview page owns
// local usage). Each window renders the REMAINING share: the percent centered
// in a ring, the "… left" value on the left, the reset timer on the right.
// Live updates: the card list polls itself (POLL_MS); the countdown rides its
// own 30 s tick, so a failing poll cannot freeze it.
// Health badge: breaker/quota cooldown from the routing HealthTracker.

const POLL_MS = 5000;

// ResetCountdown renders a window's reset as a live countdown; the exact
// instant — in the READER's timezone, via the browser locale — is on the
// hover/focus tooltip. No timestamp (0/absent) → the caller renders nothing:
// an unknown reset is never guessed at.
function ResetCountdown({ reset, windowName }: { reset: number; windowName: string }) {
  const id = useId();
  // A 30 s tick keeps the minute reading honest without a per-second timer.
  const now = useNow(30_000);
  const secs = reset - Math.floor(now / 1000);
  return (
    <span
      className="tooltip hint"
      style={{ margin: 0, fontSize: 'var(--fs-xs)' }}
      tabIndex={0}
      aria-describedby={id}
    >
      {secs <= 0 ? 'resetting…' : `resets in ${formatCountdown(secs)}`}
      <span className="tt" role="tooltip" id={id}>
        {`${windowName} window resets ${formatResetInstant(reset)}`}
      </span>
    </span>
  );
}

// remainingPct turns the provider's "X% used" into the reading the card
// shows: what is LEFT. Null stays null — an unknown limit is never guessed.
function remainingPct(usedPct: number | null | undefined): number | null {
  if (usedPct === null || usedPct === undefined || Number.isNaN(usedPct)) return null;
  return Math.max(0, Math.min(100, 100 - usedPct));
}

function creditPct(credits: CreditsUsage): number | null {
  const remaining = credits.remaining ?? null;
  const limit = credits.limit ?? null;
  if (remaining === null || limit === null || limit <= 0) return null;
  return Math.max(0, Math.min(100, (remaining / limit) * 100));
}

function formatCredits(value: number): string {
  return value.toLocaleString(undefined, { maximumFractionDigits: 0 });
}

function percentLeft(pct: number | null): string {
  return pct === null ? '—' : `${Math.round(pct)}% left`;
}

function creditsLeft(credits: CreditsUsage): string {
  const remaining = credits.remaining ?? null;
  return remaining === null ? '—' : `${formatCredits(remaining)} credits left`;
}

// UsageRing: the kit's ring vocabulary at card size — a track, a segment for
// the remaining share, and the percent centered inside. Colors are tokens:
// accent while healthy, warn when little is left, down at zero. An unknown
// reading draws no segment and shows "—"; the honest-empty rule.
function UsageRing({ pct }: { pct: number | null }) {
  const R = 34;
  const C = 2 * Math.PI * R;
  const clamped = pct === null ? 0 : Math.min(Math.max(pct, 0), 100);
  const seg = (C * clamped) / 100;
  const tone =
    pct === null
      ? 'var(--neutral-700)'
      : clamped <= 0
        ? 'var(--status-down)'
        : clamped <= 20
          ? 'var(--status-warn)'
          : 'var(--chart-line)';
  return (
    <div className="usage-ring">
      <svg viewBox="0 0 80 80" aria-hidden="true">
        <circle className="usage-ring-track" cx="40" cy="40" r={R} />
        {pct !== null && clamped > 0 && (
          <circle
            className="usage-ring-seg"
            cx="40"
            cy="40"
            r={R}
            stroke={tone}
            strokeDasharray={`${seg} ${C}`}
            transform="rotate(-90 40 40)"
          />
        )}
      </svg>
      <span className="usage-ring-num">{pct === null ? '—' : `${Math.round(pct)}%`}</span>
    </div>
  );
}

// UsageWindow is one limit window: the window label and the remaining reading
// on the left, the ring centered, the reset timer on the right. The timer
// owns its grid column — no flex-wrap with variable-length text above the
// ring, which is the font-metric drift trap.
function UsageWindow({
  label,
  pct,
  left,
  reset,
}: {
  label: string;
  pct: number | null;
  left: string;
  reset?: number;
}) {
  return (
    <div className="usage-window">
      <div className="usage-window-copy">
        <span className="usage-window-label">{label}</span>
        <span className="usage-left">{left}</span>
      </div>
      <UsageRing pct={pct} />
      <div className="usage-reset">
        {reset !== undefined && reset > 0 ? <ResetCountdown reset={reset} windowName={label} /> : null}
      </div>
    </div>
  );
}

function healthBadge(health: string | undefined): { label: string; cls: string } | null {
  if (health === 'quota_exhausted') return { label: 'quota', cls: 'warn' };
  if (health === 'breaker') return { label: 'cooling', cls: 'down' };
  // Failing but still in rotation: the last failure before the breaker opens.
  if (health === 'degraded') return { label: 'degraded', cls: 'warn' };
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

  // The pool switch, right where the account's numbers are: an account that
  // is switched off serves nothing, which is exactly the reading an operator
  // wants while looking at its quota.
  const toggleAccount = (account: AccountUsage, enabled: boolean) =>
    refresh.run(() =>
      apiCall('PUT', `/api/admin/providers/${encodeURIComponent(account.account_id)}/enabled`, { enabled }),
    ).then(() => reload());

  const refreshOne = (account: AccountUsage) =>
    refresh.run(() =>
      apiCall('POST', `/api/admin/accounts/usage/refresh/${encodeURIComponent(account.account_id)}`, {}),
    );
  const refreshAll = async () => {
    // Sequential per-card refreshes — the bounded-concurrency pool lives
    // server-side when a periodic refresher is added (ADR-012).
    let failed = false;
    for (const a of accounts) {
      if (a.refreshable) {
        const ok = await refreshOne(a);
        if (ok === null) failed = true; // thrown error is the truth
      }
    }
    if (failed) setRefreshError('One or more accounts failed to refresh — see the card errors.');
    else setRefreshError(null);
    reload();
  };
  // Per-card refresh error surfacing: the mutation's error field is
  // shared, so the page note is set alongside (the v1.0.2 stale-error
  // lesson: branch on our own state).
  const [refreshError, setRefreshError] = useState<string | null>(null);

  return (
    <>
      <PageHead
        title="Quota"
        sub="Per-account limits — the windows each provider reports, live when the account can be scraped."
        actions={
          <>
            <LiveBadge updatedAt={updatedAt} intervalMs={POLL_MS} />
            <button type="button" className="btn btn-ghost btn-sm" onClick={refreshAll} disabled={refresh.pending}>
              Refresh all
            </button>
          </>
        }
      />
      {(refreshError !== null || refresh.error !== null) && (
        <div style={{ margin: '8px 0' }}>
          <p role="alert" className="form-note">{refreshError ?? refresh.error}</p>
        </div>
      )}
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
            {/* Honest summary tiles (kit .kpi; values masked in e2e). Three
                equal columns, and the icon rides the label line — the kit's
                kpi-label is built to carry it there. As a bare flex child of
                .kpi (a column) it used to wrap onto its own line under the
                value. Labels are title-case here; .kpi-label uppercases them. */}
            <div
              style={{
                display: 'grid',
                gridTemplateColumns: 'repeat(3, minmax(0, 1fr))',
                gap: 8,
                marginBottom: 12,
              }}
            >
              <div className="widget" style={{ padding: '10px 14px' }}>
                <div className="kpi">
                  <div className="kpi-label">
                    <span className="ic"><IconBoxes width="14" height="14" /></span>
                    Total
                  </div>
                  <div className="kpi-value">{accounts.length}</div>
                </div>
              </div>
              <div className="widget" style={{ padding: '10px 14px' }}>
                <div className="kpi">
                  <div className="kpi-label">
                    <span className="ic"><IconPlug width="14" height="14" /></span>
                    Healthy
                  </div>
                  <div className="kpi-value">{healthy}</div>
                </div>
              </div>
              <div className="widget" style={{ padding: '10px 14px' }}>
                <div className="kpi">
                  <div className="kpi-label">
                    <span className="ic"><IconRefresh width="14" height="14" /></span>
                    Cooling
                  </div>
                  <div className="kpi-value">{cooling}</div>
                </div>
              </div>
            </div>
            <div className="cards" style={{ display: 'grid', gap: 8 }}>
              {accounts.map((a) => {
                // A switched-off account is out of the pool: it earns no
                // cooldown, so the health badge is suppressed rather than
                // showing a state it cannot be in.
                const hb = a.enabled ? healthBadge(a.health) : null;
                const live = a.live;
                const sessionLeft = remainingPct(live?.session_pct);
                const weeklyLeft = remainingPct(live?.weekly_pct);
                return (
                  <div key={a.account_id} className="widget w-full" style={{ padding: '12px 14px' }}>
                    <div className="widget-head" style={{ marginBottom: 6 }}>
                      <h3 className="title">
                        <span className="ic"><IconPlug width="14" height="14" /></span>
                        {a.name}
                        {hb && <span className={`badge ${hb.cls}`} style={{ marginLeft: 6 }}>{hb.label}</span>}
                        {!a.enabled ? (
                          <span className="badge paused" style={{ marginLeft: 6 }}>out of the pool</span>
                        ) : a.live ? (
                          <span className="badge up" style={{ marginLeft: 6 }}>live</span>
                        ) : null}
                      </h3>
                      <label className="toggle" title={a.enabled ? 'In the pool — this account serves traffic' : 'Out of the pool — it serves nothing until switched back on'}>
                        <input
                          type="checkbox"
                          checked={a.enabled}
                          onChange={(e) => void toggleAccount(a, e.target.checked)}
                          disabled={refresh.pending}
                          aria-label={`${a.name} in the pool`}
                        />
                        <span className="tswitch" />
                      </label>
                    </div>
                    {live !== null && live !== undefined ? (
                      <div className="usage-windows">
                        {(live.session_pct !== undefined || (live.session_reset ?? 0) > 0) && (
                          <UsageWindow
                            label="Session"
                            pct={sessionLeft}
                            left={percentLeft(sessionLeft)}
                            reset={live.session_reset}
                          />
                        )}
                        {(live.weekly_pct !== undefined || (live.weekly_reset ?? 0) > 0) && (
                          <UsageWindow
                            label="Weekly"
                            pct={weeklyLeft}
                            left={percentLeft(weeklyLeft)}
                            reset={live.weekly_reset}
                          />
                        )}
                        {live.credits && (
                          <UsageWindow
                            label="Credits"
                            pct={creditPct(live.credits)}
                            left={creditsLeft(live.credits)}
                            reset={live.credits.reset}
                          />
                        )}
                      </div>
                    ) : (
                      <EmptyState
                        title="No live usage"
                        body={a.error !== undefined && a.error !== ''
                          ? `Last scrape failed: ${a.error}.`
                          : a.refreshable
                            ? 'No limit window reported yet — use Refresh now to fetch it.'
                            : 'This provider does not report usage windows.'}
                      />
                    )}
                    <div style={{ textAlign: 'right' }}>
                      {a.refreshable && (
                        <button
                          type="button"
                          className="btn btn-ghost btn-sm"
                          onClick={() => void refreshOne(a)}
                          disabled={refresh.pending}
                        >
                          Refresh now
                        </button>
                      )}
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
