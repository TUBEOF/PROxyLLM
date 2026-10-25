import { useCallback, useEffect, useRef, useState } from 'react';
import type { ReactNode } from 'react';
import { ApiError, apiCall } from './api';
import { useAuth } from './components/AuthContext';
import { IconAlert } from './icons';

// Shared data-fetch plumbing for the admin routes:
// - `useAdminData` loads any GET endpoint with loading/error/empty states.
// - A 401 anywhere flips the session off via onSessionLost → /login
//   (the only session-expiry path — no probe endpoint, S-2).
// - `useMutation` wraps writes with a pending flag and the same 401 path.

type Refetch<T> = (signal?: AbortSignal) => Promise<T>;

export interface DataState<T> {
  data: T | null;
  loading: boolean;
  error: string | null;
  /** Reload now; `quiet` keeps the current cards mounted instead of showing the loading state. */
  reload: (opts?: { quiet?: boolean }) => void;
}

export function useAdminData<T>(fetcher: Refetch<T>, deps: unknown[] = []): DataState<T> {
  const { onSessionLost } = useAuth();
  const [data, setData] = useState<T | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [tick, setTick] = useState(0);
  // Keep the latest fetcher without re-running on every render.
  const fetcherRef = useRef(fetcher);
  fetcherRef.current = fetcher;
  // onSessionLost is stable (useCallback in AuthContext) but capture it in
  // the ref pattern anyway to keep the effect deps minimal.
  const sessionLostRef = useRef(onSessionLost);
  sessionLostRef.current = onSessionLost;

  useEffect(() => {
    const controller = new AbortController();
    let cancelled = false;
    setLoading(true);
    setError(null);
    fetcherRef.current(controller.signal)
      .then((result) => {
        if (!cancelled) {
          setData(result);
          setLoading(false);
        }
      })
      .catch((err: unknown) => {
        if (cancelled) return;
        if (err instanceof ApiError && err.status === 401) {
          sessionLostRef.current();
          return;
        }
        setError(err instanceof Error ? err.message : 'Request failed');
        setLoading(false);
      });
    return () => {
      cancelled = true;
      controller.abort();
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps -- deps are the caller's reload triggers + tick
  }, [tick, ...deps]);

  const reload = useCallback(() => setTick((t) => t + 1), []);
  return { data, loading, error, reload };
}

export function useMutation() {
  const { onSessionLost } = useAuth();
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<string | null>(null);

  // run resolves with the mutation's return value on success, or null on
  // failure/401 — callers that only need a success flag treat it as boolean.
  const run = useCallback(
    async <T,>(fn: () => Promise<T>): Promise<T | null> => {
      setPending(true);
      setError(null);
      try {
        const result = await fn();
        setPending(false);
        return result;
      } catch (err) {
        setPending(false);
        if (err instanceof ApiError && err.status === 401) {
          onSessionLost();
          return null;
        }
        setError(err instanceof Error ? err.message : 'Request failed');
        return null;
      }
    },
    [onSessionLost],
  );

  return { pending, error, run, clearError: () => setError(null) };
}

// PageHead is the standard route header (kit .workspace-head): title,
// one-line honest subtitle, optional actions. Every route uses it so the
// hierarchy reads consistently.
export function PageHead({ title, sub, actions }: { title: string; sub: string; actions?: ReactNode }) {
  return (
    <div className="workspace-head">
      <div>
        <h1>{title}</h1>
        <p className="sub">{sub}</p>
      </div>
      {actions !== undefined && <div className="workspace-actions">{actions}</div>}
    </div>
  );
}

// EmptyState is the honest empty placeholder (kit .empty) — never fabricated
// metrics, always a sentence saying what is missing and where to create it.
// The icon and action are part of the kit's empty vocabulary; the live region
// announces the state to assistive technology.
export function EmptyState({
  title,
  body,
  action,
  icon,
}: {
  title: string;
  body: string;
  action?: ReactNode;
  icon?: ReactNode;
}) {
  return (
    <div className="empty" role="status">
      <span className="empty-ic" aria-hidden="true">
        {icon ?? <IconAlert width="18" height="18" />}
      </span>
      <h3 className="empty-t">{title}</h3>
      <p className="empty-h">{body}</p>
      {action !== undefined && <div className="empty-action">{action}</div>}
    </div>
  );
}

// ErrorNote shows a failed fetch inline — honest, with the server's message.
export function ErrorNote({ message }: { message: string }) {
  return (
    <div className="empty" role="alert">
      <span className="empty-ic" aria-hidden="true"><IconAlert width="18" height="18" /></span>
      <h3 className="empty-t">Could not load data</h3>
      <p className="empty-h">{message}</p>
    </div>
  );
}

// fmtDay renders YYYY-MM-DD (UTC) from a unix timestamp — the usage
// endpoint's day format.
export function fmtDay(ts: number): string {
  return new Date(ts * 1000).toISOString().slice(0, 10);
}

// useNow is the sanctioned way for a component to read the wall clock:
// Date.now() during render is impure (react-hooks/purity) and a value that
// only changes when a poll answers would freeze with the poll. The state
// carries the clock; the interval advances it. Minute-precision consumers
// pass 30_000; a 1 Hz tick is only for staleness thresholds (LiveBadge).
export function useNow(intervalMs: number): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const t = setInterval(() => setNow(Date.now()), intervalMs);
    return () => clearInterval(t);
  }, [intervalMs]);
  return now;
}

// CONFIG_LIVE_MS is the config pages' shared light-refresh interval — they
// change rarely, so a slow poll keeps their LIVE badge honest without
// churn (one constant, not one per page: a window tune must not drift the
// pages apart).
export const CONFIG_LIVE_MS = 15000;

// useLiveData polls a GET endpoint while the page is mounted: a fixed interval, paused while the tab is hidden (the
// poll resumes on visibility), in-flight aborted on unmount. Same 401 →
// session-loss path as useAdminData, same DataState contract — live pages
// use this instead of useAdminData. Transient poll errors surface in the
// error state but never stop the loop. `updatedAt` feeds the page's live
// badge. Deterministic in e2e: the poll interval is the caller's constant
// and the fetcher the fixturesrv's fixed snapshot — no wall clock in the
// rendering.
export function useLiveData<T>(fetcher: Refetch<T>, intervalMs: number, deps: unknown[] = []): DataState<T> & { updatedAt: number | null } {
  const { onSessionLost } = useAuth();
  const [data, setData] = useState<T | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [updatedAt, setUpdatedAt] = useState<number | null>(null);
  const [tick, setTick] = useState(0);
  const quietReload = useRef(false);
  const fetcherRef = useRef(fetcher);
  fetcherRef.current = fetcher;
  const sessionLostRef = useRef(onSessionLost);
  sessionLostRef.current = onSessionLost;

  const apply = useCallback((result: T) => {
    setData(result);
    setLoading(false);
    setError(null);
    setUpdatedAt(Date.now());
  }, []);

  // Manual (deps/tick) load — the same effect useAdminData runs.
  useEffect(() => {
    const controller = new AbortController();
    let cancelled = false;
    // A quiet reload keeps the page frame and cards mounted (the Quota
    // refresh path); a normal reload shows the loading state.
    if (!quietReload.current) setLoading(true);
    quietReload.current = false;
    setError(null);
    fetcherRef.current(controller.signal)
      .then((result) => {
        if (!cancelled) apply(result);
      })
      .catch((err: unknown) => {
        if (cancelled) return;
        if (err instanceof ApiError && err.status === 401) {
          sessionLostRef.current();
          return;
        }
        setError(err instanceof Error ? err.message : 'Request failed');
        setLoading(false);
      });
    return () => {
      cancelled = true;
      controller.abort();
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps -- deps are the caller's reload triggers + tick
  }, [tick, ...deps]);

  // The poll loop: a fixed interval while mounted; a hidden tab pauses,
  // a visible one resumes immediately; failures never stop the loop
  // (transient log contention etc.); the 401 path is the session-loss one.
  useEffect(() => {
    if (intervalMs <= 0) return;
    let stopped = false;
    let timer: ReturnType<typeof setTimeout> | null = null;
    let inFlight: AbortController | null = null;
    const poll = () => {
      if (stopped || document.hidden) return;
      inFlight = new AbortController();
      fetcherRef.current(inFlight.signal)
        .then((result) => {
          if (!stopped) apply(result);
        })
        .catch((err: unknown) => {
          if (stopped) return;
          if (err instanceof ApiError && err.status === 401) {
            sessionLostRef.current();
            return;
          }
          if (err instanceof Error && err.name !== 'AbortError') {
            setError(err.message);
          }
        })
        .finally(() => {
          inFlight = null;
        });
    };
    const schedule = () => {
      if (stopped) return;
      timer = setTimeout(() => {
        poll();
        schedule();
      }, intervalMs);
    };
    const onVisible = () => {
      if (!document.hidden) poll();
    };
    schedule();
    document.addEventListener('visibilitychange', onVisible);
    return () => {
      stopped = true;
      if (timer !== null) clearTimeout(timer);
      if (inFlight !== null) inFlight.abort();
      document.removeEventListener('visibilitychange', onVisible);
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [intervalMs, apply, ...deps, tick]);

  const reload = useCallback((opts?: { quiet?: boolean }) => {
    quietReload.current = Boolean(opts?.quiet);
    setTick((t) => t + 1);
  }, []);
  return { data, loading, error, reload, updatedAt };
}

export { apiCall };