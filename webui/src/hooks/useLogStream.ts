// useLogStream (Logs-page redesign, 2026-10-06): the live SSE subscription
// over GET /api/admin/logs/stream. Native EventSource (zero deps,
// auto-reconnect built in); a hidden tab closes the stream and visibility
// reopens it (the useLiveData pause doctrine); repeated failures probe the
// page endpoint once for a 401 — the session-loss path EventSource cannot
// surface itself.
import { useCallback, useEffect, useRef, useState } from 'react';
import { apiCall, ApiError } from '../api';
import type { RunningRequest, RequestLog } from '../types';
import { useAuth } from '../components/AuthContext';

export type LogStreamState = {
  connected: boolean;
};

export type LogStreamHandlers = {
  onRunning: (running: RunningRequest[]) => void;
  onLog: (row: RequestLog) => void;
};

export function useLogStream(handlers: LogStreamHandlers, enabled: boolean): LogStreamState {
  const { onSessionLost } = useAuth();
  const [connected, setConnected] = useState(false);
  const handlersRef = useRef(handlers);
  handlersRef.current = handlers;
  const sessionLostRef = useRef(onSessionLost);
  sessionLostRef.current = onSessionLost;

  // Failures without a successful connect probe the page endpoint once:
  // 401 → session lost (EventSource cannot surface status codes itself).
  const probeSession = useCallback(() => {
    apiCall('GET', '/api/admin/logs?limit=1')
      .then(() => undefined)
      .catch((err: unknown) => {
        if (err instanceof ApiError && err.status === 401) {
          sessionLostRef.current();
        }
      });
  }, []);

  useEffect(() => {
    if (!enabled) {
      setConnected(false);
      return;
    }
    let closed = false;
    let es: EventSource | null = null;
    let failureCount = 0;

    const connect = () => {
      if (closed || document.hidden) return;
      es = new EventSource('/api/admin/logs/stream');
      es.addEventListener('hello', () => {
        failureCount = 0;
        setConnected(true);
      });
      es.addEventListener('running', (ev) => {
        try {
          const rows = JSON.parse((ev as MessageEvent<string>).data) as RunningRequest[];
          handlersRef.current.onRunning(Array.isArray(rows) ? rows : []);
        } catch {
          // one bad frame never kills the stream
        }
      });
      es.addEventListener('log', (ev) => {
        try {
          const row = JSON.parse((ev as MessageEvent<string>).data) as RequestLog;
          handlersRef.current.onLog(row);
        } catch {
          // ignore
        }
      });
      es.onerror = () => {
        setConnected(false);
        failureCount++;
        if (failureCount === 3) {
          probeSession();
        }
        // EventSource auto-reconnects; nothing to schedule here.
      };
    };

    const onVisible = () => {
      if (!document.hidden && es === null) connect();
    };
    connect();
    document.addEventListener('visibilitychange', onVisible);
    return () => {
      closed = true;
      document.removeEventListener('visibilitychange', onVisible);
      if (es !== null) {
        es.close();
        es = null;
      }
      setConnected(false);
    };
  }, [enabled, probeSession]);

  return { connected };
}