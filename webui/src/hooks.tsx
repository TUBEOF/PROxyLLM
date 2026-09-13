import { useCallback, useEffect, useRef, useState } from 'react';
import type { ReactNode } from 'react';
import { ApiError, apiCall } from './api';
import { useAuth } from './components/AuthContext';

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
  reload: () => void;
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
export function EmptyState({ title, body, action }: { title: string; body: string; action?: ReactNode }) {
  return (
    <div className="empty">
      <h3>{title}</h3>
      <p>{body}</p>
      {action}
    </div>
  );
}

// ErrorNote shows a failed fetch inline — honest, with the server's message.
export function ErrorNote({ message }: { message: string }) {
  return (
    <div className="empty">
      <h3>Could not load data</h3>
      <p>{message}</p>
    </div>
  );
}

// fmtDay renders YYYY-MM-DD (UTC) from a unix timestamp — the usage
// endpoint's day format.
export function fmtDay(ts: number): string {
  return new Date(ts * 1000).toISOString().slice(0, 10);
}

export { apiCall };