import { useCallback, useEffect, useRef, useState } from 'react';
import { apiCall } from '../api';
import { copyText } from '../clipboard';
import type { OAuthPollResult, OAuthStartResult } from '../types';
import { useMutation } from '../hooks';
import { IconCheck, IconCopy, IconExternal } from '../icons';

// Guided device sign-in (X20 / ADR-009 decision 6): start surfaces the
// operator pair (user_code + verification URL), the SPA polls at the
// issuer's own pace, and the tokens land encrypted server-side — the
// browser never sees token material. Extracted from the provider drawer
// so the per-type account flow reuses the identical behavior; the polling
// semantics (one issuer round trip per call, terminal errors, explicit
// restart) are unchanged.

export default function OAuthSignIn({ providerId, onApproved }: { providerId: string; onApproved: () => void }) {
  const start = useMutation();
  const [code, setCode] = useState<string | null>(null);
  const [url, setUrl] = useState<string | null>(null);
  const [intervalSec, setIntervalSec] = useState<number | null>(null);
  const [polling, setPolling] = useState(false);
  const [pollError, setPollError] = useState<string | null>(null);
  const [approvedAccount, setApprovedAccount] = useState<string | null>(null);
  const [copied, setCopied] = useState(false);
  const [copyBlocked, setCopyBlocked] = useState(false);
  // Timer across polls: ref so cancel-on-unmount clears a pending tick.
  const timerRef = useRef<ReturnType<typeof setTimeout> | null>(null);

  const clearTimer = useCallback(() => {
    if (timerRef.current !== null) {
      clearTimeout(timerRef.current);
      timerRef.current = null;
    }
  }, []);

  useEffect(() => clearTimer, [clearTimer]);

  const doStart = useCallback(async () => {
    setApprovedAccount(null);
    setPollError(null);
    setCopied(false);
    setCopyBlocked(false);
    const result = await start.run(() =>
      apiCall<OAuthStartResult>(
        'POST',
        `/api/admin/providers/${encodeURIComponent(providerId)}/oauth/start`,
      ),
    );
    if (result === null) return;
    setCode(result.user_code);
    setUrl(result.verification_url);
    setIntervalSec(result.interval_seconds);
    setPolling(true);
  }, [providerId, start]);

  // One poll call, then schedule the next at the issuer's interval. The
  // interval has a server-provided floor; a tiny padding keeps the call on
  // the safe side of the throttle window. Errors are terminal server-side
  // (the flow drops out of the registry) — surfaced, never looped on.
  const pollOnce = useCallback(async () => {
    try {
      const result = await apiCall<OAuthPollResult>(
        'POST',
        `/api/admin/providers/${encodeURIComponent(providerId)}/oauth/poll`,
      );
      if (result.status === 'approved') {
        setPolling(false);
        setApprovedAccount(result.account_id ?? '');
        onApproved();
        return;
      }
      const wait = intervalSec !== null ? intervalSec * 1000 + 250 : 5250;
      timerRef.current = setTimeout(() => void pollOnce(), wait);
    } catch (err) {
      setPollError(err instanceof Error ? err.message : 'Poll failed');
    }
  }, [providerId, intervalSec, onApproved]);

  useEffect(() => {
    if (!polling || intervalSec === null) return;
    timerRef.current = setTimeout(() => void pollOnce(), intervalSec * 1000 + 250);
    return clearTimer;
  }, [polling, intervalSec, pollOnce, clearTimer]);

  const copy = useCallback(async () => {
    if (code === null) return;
    // copyText falls back for the insecure origin (plain-HTTP LAN console)
    // where navigator.clipboard does not exist; a failure is REPORTED rather
    // than swallowed, so the operator is never left guessing.
    const outcome = await copyText(code);
    setCopied(outcome === 'copied');
    setCopyBlocked(outcome === 'failed');
  }, [code]);

  const restart = useCallback(() => {
    clearTimer();
    setPolling(false);
    setCode(null);
    setUrl(null);
    setIntervalSec(null);
    setPollError(null);
    setApprovedAccount(null);
    void doStart();
  }, [clearTimer, doStart]);

  // Poll failures are terminal server-side (the flow drops out of the
  // registry); catch them here and surface instead of looping.
  useEffect(() => {
    if (pollError === null) return;
    clearTimer();
    setPolling(false);
  }, [pollError, clearTimer]);

  const failure = pollError ?? start.error;
  if (code === null) {
    return (
      <div className="form">
        <div className="field">
          <span className="hint">
            Device sign-in (RFC 8628): start shows a code to enter on the issuer page; the
            proxy polls until the tokens land encrypted server-side. The browser never sees
            token material.
          </span>
          {failure !== null && (
            <p role="alert" className="form-note">
              {failure}
            </p>
          )}
          <button type="button" className="btn btn-primary btn-sm" onClick={() => void doStart()} disabled={start.pending}>
            Start device sign-in
          </button>
        </div>
      </div>
    );
  }

  return (
    <div className="form">
      <div className="field">
        <label htmlFor="oauth-user-code">User code</label>
        <div style={{ display: 'flex', gap: 8, alignItems: 'center' }}>
          <input
            id="oauth-user-code"
            className="input mono"
            type="text"
            value={code}
            readOnly
            onFocus={(e) => e.target.select()}
          />
          <button
            type="button"
            className="btn btn-secondary btn-sm"
            onClick={() => void copy()}
            aria-label="Copy user code to clipboard"
          >
            {copied ? <IconCheck width="14" height="14" /> : <IconCopy width="14" height="14" />}
            {copied ? 'Copied' : 'Copy'}
          </button>
        </div>
        <span className="hint">
          Open the verification page and enter this code. It stays valid for 15 minutes.
        </span>
        {copyBlocked && (
          <p role="alert" className="form-note" style={{ margin: 0 }}>
            The browser blocked clipboard access on this origin — select the code and copy it with
            Ctrl/Cmd+C.
          </p>
        )}
      </div>
      <div className="field">
        <label>Verification page</label>
        {url !== null && (
          <a className="btn btn-secondary btn-sm" href={url} target="_blank" rel="noreferrer">
            <IconExternal width="14" height="14" />
            Open verification page
          </a>
        )}
        <span className="hint">The issuer opens and the proxy keeps polling here.</span>
      </div>
      <div className="field">
        {approvedAccount !== null ? (
          <>
            <span className="badge up">
              <span className="b-dot" />
              Signed in{approvedAccount !== '' ? ` — account ${approvedAccount}` : ''}
            </span>
            <span className="hint">Tokens are stored encrypted on the server. Sign-in complete.</span>
          </>
        ) : polling ? (
          <span className="badge info">
            <span className="b-dot" />
            Waiting for approval
          </span>
        ) : failure !== null ? (
          <>
            <span className="badge down">
              <span className="b-dot" />
              Sign-in failed
            </span>
            <p role="alert" className="form-note">
              {failure}
            </p>
          </>
        ) : (
          <span className="badge paused">
            <span className="b-dot" />
            Idle
          </span>
        )}
      </div>
      {(approvedAccount !== null || failure !== null) && (
        <div className="field">
          <button
            type="button"
            className="btn btn-secondary btn-sm"
            onClick={restart}
            disabled={start.pending}
          >
            Start over
          </button>
          <span className="hint">A new start replaces the previous flow server-side.</span>
        </div>
      )}
    </div>
  );
}