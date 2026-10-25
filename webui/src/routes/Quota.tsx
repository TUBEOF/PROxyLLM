import { useMemo, useState } from 'react';
import { apiCall } from '../api';
import { formatCountdown, relativeAge } from '../time';
import type {
  AccountUsage,
  AccountUsageResponse,
  Cooldown,
  UsageWindow,
} from '../types';
import { EmptyState, ErrorNote, PageHead, useLiveData, useNow } from '../hooks';
import { LiveBadge } from '../components/LiveBadge';
import { UsageBar } from '../components/UsageBar';
import { IconBoxes, IconGauge, IconPlug, IconRefresh } from '../icons';

// Quota (ADR-012): per-account LIMIT cards. The card shows what the account
// itself reports — Ollama's session/weekly/monthly windows, Firecrawl's
// credits, Codex's header windows — and nothing the proxy measured on its
// own. Each window renders the REMAINING share as a shrinking bar: green
// while there is room, orange in the last half, red under 20%. A cooldown
// badge carries the deadline, not just the word.
//
// The page groups by provider type (chat before web tools, live groups
// before empty ones), keeps a single 30 s clock for every countdown, and
// refreshes one card without remounting the page.

const POLL_MS = 5000;

function clampPct(v: number): number {
  return Math.max(0, Math.min(100, v));
}

function windowPct(w: UsageWindow): number | null {
  if (w.remaining_pct !== undefined && w.remaining_pct !== null) {
    return clampPct(w.remaining_pct);
  }
  if (w.used_pct !== undefined && w.used_pct !== null) {
    return clampPct(100 - w.used_pct);
  }
  if (
    (w.kind === 'credits' || w.kind === 'usd') &&
    w.remaining !== undefined &&
    w.remaining !== null &&
    w.limit !== undefined &&
    w.limit !== null &&
    w.limit > 0
  ) {
    return clampPct((w.remaining / w.limit) * 100);
  }
  return null;
}

function formatNumber(value: number, digits = 0): string {
  return value.toLocaleString(undefined, { maximumFractionDigits: digits });
}

function windowReading(w: UsageWindow): string {
  if (w.unlimited) return 'unlimited';
  if (w.kind === 'usd') {
    if (w.remaining === undefined || w.remaining === null) return '—';
    if (w.limit !== undefined && w.limit !== null) {
      return `$${formatNumber(w.remaining, 2)} of $${formatNumber(w.limit, 2)} left`;
    }
    return `$${formatNumber(w.remaining, 2)} left`;
  }
  if (w.kind === 'credits') {
    if (w.remaining === undefined || w.remaining === null) return '—';
    return `${formatNumber(w.remaining)} credits left`;
  }
  const pct = windowPct(w);
  return pct === null ? '—' : `${Math.round(pct)}% left`;
}

function sourceLabel(source: string | undefined): string {
  switch (source) {
    case 'ollama_balance':
      return 'Ollama balance';
    case 'ollama_settings':
      return 'Ollama settings';
    case 'firecrawl_credit_usage':
      return 'Firecrawl credits';
    case 'codex_headers':
      return 'Codex headers';
    case 'codex_sse':
      return 'Codex stream';
    case 'legacy':
      return 'cached reading';
    default:
      return source ?? '';
  }
}

// accountWindows falls back to the legacy session/weekly/credits fields when
// an older server does not send the uniform windows list (rolling deploy).
function accountWindows(a: AccountUsage): UsageWindow[] {
  const live = a.live;
  if (live?.windows !== undefined && live.windows.length > 0) return live.windows;
  const out: UsageWindow[] = [];
  if (!live) return out;
  if (live.session_pct !== undefined || (live.session_reset ?? 0) > 0) {
    out.push({
      key: 'session',
      label: 'Session',
      kind: 'percent',
      used_pct: live.session_pct ?? null,
      remaining_pct: live.session_pct === undefined || live.session_pct === null
        ? null
        : 100 - live.session_pct,
      reset_at: live.session_reset,
      source: 'legacy',
    });
  }
  if (live?.weekly_pct !== undefined || (live?.weekly_reset ?? 0) > 0) {
    out.push({
      key: 'weekly',
      label: 'Weekly',
      kind: 'percent',
      used_pct: live.weekly_pct ?? null,
      remaining_pct: live.weekly_pct === undefined || live.weekly_pct === null
        ? null
        : 100 - live.weekly_pct,
      reset_at: live.weekly_reset,
      source: 'legacy',
    });
  }
  if (live?.credits) {
    out.push({
      key: 'credits',
      label: 'Credits',
      kind: 'credits',
      unit: 'credits',
      remaining: live.credits.remaining ?? null,
      limit: live.credits.limit ?? null,
      reset_at: live.credits.reset,
      source: 'legacy',
    });
  }
  return out;
}

function groupSource(windows: UsageWindow[]): string {
  const labels = [...new Set(windows.map((w) => sourceLabel(w.source)).filter(Boolean))];
  return labels.length > 0 ? labels.join(' · ') : 'no live windows';
}

function healthBadge(a: AccountUsage, nowMs: number): { label: string; cls: string } | null {
  if (!a.enabled) return null;
  const now = Math.floor(nowMs / 1000);
  const countdown = (until: number | undefined): string | null => {
    if (until === undefined || until <= 0) return null;
    return formatCountdown(Math.max(0, until - now));
  };
  if (a.health === 'quota_exhausted') {
    const until = a.cooldown?.quota_until ?? a.health_until;
    const left = countdown(until);
    return { label: left === null ? 'quota' : `quota · resets in ${left}`, cls: 'warn' };
  }
  if (a.health === 'breaker') {
    const until = a.cooldown?.breaker_until ?? a.health_until;
    const left = countdown(until);
    return { label: left === null ? 'cooling' : `cooling · ${left}`, cls: 'down' };
  }
  if (a.health === 'degraded') return { label: 'degraded', cls: 'warn' };
  return null;
}

function cooldownOf(a: AccountUsage): Cooldown | undefined {
  return a.cooldown ?? undefined;
}

export default function Quota() {
  const { data, loading, error, reload, updatedAt } = useLiveData<AccountUsageResponse>(
    (signal) => apiCall<AccountUsageResponse>('GET', '/api/admin/accounts/usage', undefined, signal),
    POLL_MS,
  );
  const nowMs = useNow(30_000);
  const accounts = useMemo(() => data?.accounts ?? [], [data]);
  const [pendingIds, setPendingIds] = useState<Set<string>>(() => new Set());
  const [refreshNote, setRefreshNote] = useState<string | null>(null);
  const [refreshError, setRefreshError] = useState<string | null>(null);
  const [toggleError, setToggleError] = useState<string | null>(null);
  const [progress, setProgress] = useState<{ done: number; total: number } | null>(null);

  const summary = useMemo(() => {
    const inPool = accounts.filter((a) => a.enabled).length;
    const reporting = accounts.filter((a) => accountWindows(a).length > 0).length;
    const cooling = accounts.filter(
      (a) => a.enabled && (a.health === 'quota_exhausted' || a.health === 'breaker'),
    ).length;
    const degraded = accounts.filter((a) => a.enabled && a.health === 'degraded').length;
    return { inPool, reporting, cooling, degraded };
  }, [accounts]);

  const groups = useMemo(() => {
    type Group = {
      key: string;
      label: string;
      kind: string;
      order: number;
      accounts: AccountUsage[];
      live: boolean;
    };
    const map = new Map<string, Group>();
    for (const a of accounts) {
      const key = a.type;
      const group = map.get(key) ?? {
        key,
        label: a.type_label ?? a.type,
        kind: a.kind ?? '',
        order: a.type_order ?? Number.MAX_SAFE_INTEGER,
        accounts: [],
        live: false,
      };
      group.accounts.push(a);
      group.live = group.live || accountWindows(a).length > 0;
      map.set(key, group);
    }
    const out = [...map.values()];
    for (const group of out) {
      group.accounts.sort((a, b) => {
        const liveDelta = Number(accountWindows(b).length > 0) - Number(accountWindows(a).length > 0);
        return liveDelta || a.name.localeCompare(b.name) || a.account_id.localeCompare(b.account_id);
      });
    }
    out.sort((a, b) => {
      const kindDelta = Number(a.kind !== 'llm') - Number(b.kind !== 'llm');
      const liveDelta = Number(b.live) - Number(a.live);
      return kindDelta || liveDelta || a.order - b.order || a.label.localeCompare(b.label);
    });
    return out;
  }, [accounts]);

  const markPending = (id: string, pending: boolean) => {
    setPendingIds((prev) => {
      const next = new Set(prev);
      if (pending) next.add(id);
      else next.delete(id);
      return next;
    });
  };

  const refreshOne = async (a: AccountUsage): Promise<boolean> => {
    markPending(a.account_id, true);
    setRefreshError(null);
    try {
      const res = await apiCall<{ ok: boolean; skipped?: boolean; retry_after?: number }>(
        'POST',
        `/api/admin/accounts/usage/refresh/${encodeURIComponent(a.account_id)}`,
        {},
      );
      if (res.skipped) {
        const wait = res.retry_after;
        setRefreshNote(
          wait !== undefined && wait > 0
            ? `Refreshed recently — cached reading is current (retry in ${formatCountdown(wait)}).`
            : 'Refreshed recently — cached reading is current.',
        );
      } else {
        setRefreshNote(null);
      }
      reload({ quiet: true });
      return true;
    } catch (err) {
      setRefreshError(err instanceof Error ? err.message : 'Refresh failed');
      return false;
    } finally {
      markPending(a.account_id, false);
    }
  };

  const refreshAll = async () => {
    const list = accounts.filter((a) => a.refreshable);
    if (list.length === 0) return;
    setRefreshError(null);
    setRefreshNote(null);
    setProgress({ done: 0, total: list.length });
    let failed = 0;
    for (let i = 0; i < list.length; i += 1) {
      const account = list[i];
      if (account === undefined) continue;
      const ok = await refreshOne(account);
      if (!ok) failed += 1;
      setProgress({ done: i + 1, total: list.length });
    }
    setProgress(null);
    setRefreshNote(failed > 0 ? `${failed} of ${list.length} refreshes failed.` : `Refreshed ${list.length} account(s).`);
    reload({ quiet: true });
  };

  const toggleAccount = async (a: AccountUsage, enabled: boolean) => {
    setToggleError(null);
    try {
      await apiCall('PUT', `/api/admin/providers/${encodeURIComponent(a.account_id)}/enabled`, { enabled });
      reload({ quiet: true });
    } catch (err) {
      setToggleError(err instanceof Error ? err.message : 'Pool switch failed');
    }
  };

  const note = toggleError ?? refreshError ?? refreshNote;
  const busy = progress !== null || pendingIds.size > 0;

  return (
    <>
      <PageHead
        title="Quota"
        sub="Per-account limits — the windows each provider reports, live when the account can be scraped."
        actions={
          <>
            <LiveBadge updatedAt={updatedAt} intervalMs={POLL_MS} />
            <button type="button" className="btn btn-ghost btn-sm" onClick={() => void refreshAll()} disabled={busy || accounts.length === 0}>
              Refresh all
            </button>
          </>
        }
      />
      {note !== null && (
        <p role="status" className="form-note usage-page-note">
          {note}
          {progress !== null ? ` Refreshing ${progress.done} of ${progress.total}…` : ''}
        </p>
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
            <div className="usage-summary">
              <div className="widget usage-summary-tile">
                <div className="kpi">
                  <div className="kpi-label">
                    <span className="ic"><IconBoxes width="14" height="14" /></span>
                    Accounts
                  </div>
                  <div className="kpi-value">{accounts.length}</div>
                  <div className="usage-summary-foot">{summary.inPool} in pool</div>
                </div>
              </div>
              <div className="widget usage-summary-tile">
                <div className="kpi">
                  <div className="kpi-label">
                    <span className="ic"><IconGauge width="14" height="14" /></span>
                    Reporting
                  </div>
                  <div className="kpi-value">{summary.reporting}</div>
                  <div className="usage-summary-foot">of {accounts.length} accounts</div>
                </div>
              </div>
              <div className="widget usage-summary-tile">
                <div className="kpi">
                  <div className="kpi-label">
                    <span className="ic"><IconRefresh width="14" height="14" /></span>
                    Cooling
                  </div>
                  <div className="kpi-value">{summary.cooling}</div>
                  <div className="usage-summary-foot">
                    {summary.degraded > 0 ? `${summary.degraded} degraded` : 'none degraded'}
                  </div>
                </div>
              </div>
            </div>

            {groups.map((group) => (
              <section key={group.key} className="usage-group" aria-labelledby={`usage-group-${group.key}`}>
                <div className="usage-group-head">
                  <h2 className="usage-group-title" id={`usage-group-${group.key}`}>
                    {group.label}
                  </h2>
                  <span className="usage-group-note">
                    {group.accounts.length} account{group.accounts.length === 1 ? '' : 's'} ·{' '}
                    {groupSource(group.accounts.flatMap((a) => accountWindows(a)))}
                  </span>
                </div>
                <div className="usage-group-grid">
                  {group.accounts.map((a) => {
                    const windows = accountWindows(a);
                    const hb = healthBadge(a, nowMs);
                    const pending = pendingIds.has(a.account_id);
                    const metaSource = windows.length > 0 ? ` · ${groupSource(windows)}` : '';
                    return (
                      <article key={a.account_id} className="widget usage-card" data-account-id={a.account_id}>
                        <div className="usage-card-head">
                          <div className="usage-card-title">
                            <span className="ic"><IconPlug width="14" height="14" /></span>
                            <h3 className="title">{a.name}</h3>
                            <span className="badge neutral">{a.type_label ?? a.type}</span>
                            {hb && <span className={`badge ${hb.cls}`}>{hb.label}</span>}
                            {!a.enabled ? (
                              <span className="badge paused">out of the pool</span>
                            ) : windows.length > 0 ? (
                              <span className="badge up">live</span>
                            ) : null}
                          </div>
                          <label
                            className="toggle"
                            title={a.enabled ? 'In the pool — this account serves traffic' : 'Out of the pool — it serves nothing until switched back on'}
                          >
                            <input
                              type="checkbox"
                              checked={a.enabled}
                              onChange={(e) => void toggleAccount(a, e.target.checked)}
                              aria-label={`${a.name} in the pool`}
                            />
                            <span className="tswitch" />
                          </label>
                        </div>
                        <div className="usage-card-meta">
                          {a.fetched_at !== undefined && a.fetched_at > 0
                            ? `Updated ${relativeAge(a.fetched_at, nowMs)}`
                            : 'Never fetched'}
                          {metaSource}
                        </div>
                        {windows.length > 0 ? (
                          <div className="usage-windows">
                            {windows.map((w) => (
                              <UsageBar
                                key={w.key}
                                label={w.label ?? w.key}
                                pct={windowPct(w)}
                                reading={windowReading(w)}
                                resetAt={w.reset_at}
                                windowName={w.label ?? w.key}
                                nowMs={nowMs}
                                unlimited={w.unlimited}
                                note={w.note}
                              />
                            ))}
                          </div>
                        ) : (
                          <EmptyState
                            title="No live usage"
                            body={
                              a.error !== undefined && a.error !== ''
                                ? `Last scrape failed: ${a.error}.`
                                : a.refreshable
                                  ? 'No limit window reported yet — use Refresh now to fetch it.'
                                  : 'This provider does not report usage windows.'
                            }
                          />
                        )}
                        <div className="usage-card-actions">
                          {a.refreshable && (
                            <button
                              type="button"
                              className="btn btn-ghost btn-sm"
                              onClick={() => void refreshOne(a)}
                              disabled={pending}
                              aria-busy={pending}
                            >
                              {pending ? 'Refreshing…' : 'Refresh now'}
                            </button>
                          )}
                          {cooldownOf(a)?.reason === 'quota_exhausted' && (
                            <span className="hint">Quota cooldown is process-local; a restart clears the badge.</span>
                          )}
                        </div>
                      </article>
                    );
                  })}
                </div>
              </section>
            ))}
          </div>
        )}
      </div>
    </>
  );
}
