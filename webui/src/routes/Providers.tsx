import { FormEvent, useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { apiCall } from '../api';
import type { OAuthPollResult, OAuthStartResult, Provider, ProvidersResponse } from '../types';
import { EmptyState, ErrorNote, PageHead, useAdminData, useMutation } from '../hooks';
import { Drawer } from '../components/Overlays';
import { IconCheck, IconCopy, IconExternal, IconPlus } from '../icons';

// Providers (plan §9): table + drawer editor. The admin API stores the
// config blob as raw JSON — the editor round-trips it through a textarea
// with client-side validation before PUT. Saved codex-oauth providers get
// the X20 device-flow sign-in inside the same drawer: start surfaces the
// operator pair (user_code + verification URL), the SPA polls at the
// issuer's own pace, and the tokens land encrypted server-side — the
// browser never sees token material.

const PROVIDER_TYPES = [
  'custom-openai',
  'ollama-cloud',
  'codex-oauth',
] as const;

function statusOf(p: Provider): { label: string; cls: string } {
  // Honest status derivation: a provider row exists with a config → it is
  // configured; health states arrive with the live health gate (Phase 1
  // routing surface) and are not part of this table yet.
  const hasConfig = p.config !== null && p.config !== undefined && Object.keys(p.config as object).length > 0;
  if (!hasConfig) {
    return { label: 'No config', cls: 'paused' };
  }
  return { label: 'Configured', cls: 'info' };
}

function jsonOr(value: unknown, fallback: string): string {
  if (value === null || value === undefined) return fallback;
  try {
    return JSON.stringify(value, null, 2) ?? fallback;
  } catch {
    return fallback;
  }
}

// One stepwise device sign-in (X20). The SPA never drives the pace itself:
// every poll waits out the issuer-provided interval, so the proxy relays
// exactly one issuer round trip per call (RFC 8628 §3.5). The flow ends in
// exactly one terminal state — approved (tokens sealed server-side) or a
// surfaced error; either way polling stops and only an explicit restart
// (or a fresh start call, which replaces the server-side flow) begins a
// new one.
function CodexSignIn({ providerId, onApproved }: { providerId: string; onApproved: () => void }) {
  const start = useMutation();
  const [code, setCode] = useState<string | null>(null);
  const [url, setUrl] = useState<string | null>(null);
  const [intervalSec, setIntervalSec] = useState<number | null>(null);
  const [polling, setPolling] = useState(false);
  const [pollError, setPollError] = useState<string | null>(null);
  const [approvedAccount, setApprovedAccount] = useState<string | null>(null);
  const [copied, setCopied] = useState(false);
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
    try {
      await navigator.clipboard.writeText(code);
      setCopied(true);
    } catch {
      // Clipboard access can fail (insecure context, permission); the code
      // stays selectable for manual entry instead of surfacing an error.
      setCopied(false);
    }
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
            <p role="alert" style={{ color: 'var(--status-down)', fontSize: 'var(--fs-sm)', margin: 0 }}>
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
        <label>User code</label>
        <div style={{ display: 'flex', gap: 8, alignItems: 'center' }}>
          <input className="input mono" type="text" value={code} readOnly onFocus={(e) => e.target.select()} />
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
            <p role="alert" style={{ color: 'var(--status-down)', fontSize: 'var(--fs-sm)', margin: 0 }}>
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

export default function Providers() {
  const { data, loading, error, reload } = useAdminData<ProvidersResponse>(
    (signal) => apiCall<ProvidersResponse>('GET', '/api/admin/providers', undefined, signal),
  );
  const providers = useMemo(() => data?.providers ?? [], [data]);

  const [editing, setEditing] = useState<Provider | null>(null);
  const [creating, setCreating] = useState(false);
  const [form, setForm] = useState({ id: '', type: 'custom-openai', name: '', config: '{}' });
  const [formError, setFormError] = useState<string | null>(null);

  const save = useMutation();
  const del = useMutation();

  const openCreate = useCallback(() => {
    setForm({ id: '', type: 'custom-openai', name: '', config: '{}' });
    setCreating(true);
    setEditing(null);
  }, []);

  const openEdit = useCallback((p: Provider) => {
    setForm({ id: p.id, type: p.type, name: p.name, config: jsonOr(p.config, '{}') });
    setEditing(p);
    setCreating(false);
  }, []);

  const closeDrawer = useCallback(() => {
    setEditing(null);
    setCreating(false);
    setFormError(null);
    save.clearError();
  }, [save]);

  const submit = useCallback(
    async (e: FormEvent) => {
      e.preventDefault();
      let configBlob: unknown;
      try {
        configBlob = form.config.trim() === '' ? undefined : JSON.parse(form.config);
      } catch {
        setFormError('Config is not valid JSON');
        return;
      }
      const id = creating ? form.id.trim() : form.id;
      if (id === '') {
        setFormError('ID is required');
        return;
      }
      setFormError(null);
      const ok = await save.run(() =>
        apiCall('PUT', `/api/admin/providers/${encodeURIComponent(id)}`, {
          type: form.type,
          name: form.name,
          config: configBlob,
        }),
      );
      if (ok) {
        closeDrawer();
        reload();
      }
    },
    [form, creating, save, closeDrawer, reload],
  );

  const remove = useCallback(
    async (p: Provider) => {
      const ok = await del.run(() => apiCall('DELETE', `/api/admin/providers/${encodeURIComponent(p.id)}`));
      if (ok) reload();
    },
    [del, reload],
  );

  const drawerError = formError ?? save.error ?? del.error;
  const drawerPending = save.pending || del.pending;

  return (
    <>
      <PageHead
        title="Providers"
        sub="Upstream providers this proxy can route to. Configs are stored encrypted at rest; secrets never come back to the browser."
        actions={
          <button type="button" className="btn btn-primary" onClick={openCreate}>
            <IconPlus width="16" height="16" />
            Add provider
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
            <EmptyState title="Loading providers" body="Fetching the provider list." />
          </div>
        ) : providers.length === 0 ? (
          <div className="w-full">
            <EmptyState
              title="No providers configured"
              body="Add a provider so the proxy has an upstream to route requests to. Custom OpenAI-compatible endpoints need a base URL in their config."
              action={
                <button type="button" className="btn btn-primary btn-sm" onClick={openCreate}>
                  <IconPlus width="14" height="14" />
                  Add the first provider
                </button>
              }
            />
          </div>
        ) : (
          <div className="w-full">
            <div className="table-wrap">
              <div className="table-scroll">
                <table className="table">
                  <thead>
                    <tr>
                      <th>Name</th>
                      <th>ID</th>
                      <th>Type</th>
                      <th>Status</th>
                      <th />
                    </tr>
                  </thead>
                  <tbody>
                    {providers.map((p) => {
                      const st = statusOf(p);
                      return (
                        <tr key={p.id} onClick={() => openEdit(p)}>
                          <td className="primary">{p.name}</td>
                          <td className="mono">{p.id}</td>
                          <td>{p.type}</td>
                          <td>
                            <span className={`badge ${st.cls}`}>
                              <span className="b-dot" />
                              {st.label}
                            </span>
                          </td>
                          <td>
                            <span className="row-actions">
                              <button
                                type="button"
                                className="btn btn-danger btn-sm"
                                onClick={(e) => {
                                  e.stopPropagation();
                                  void remove(p);
                                }}
                              >
                                Delete
                              </button>
                            </span>
                          </td>
                        </tr>
                      );
                    })}
                  </tbody>
                </table>
              </div>
            </div>
          </div>
        )}
      </div>

      {(editing !== null || creating) && (
        <Drawer
          title={creating ? 'Add provider' : `Edit ${editing?.name}`}
          onClose={closeDrawer}
          footer={
            <>
              {!creating && editing !== null && (
                <button
                  type="button"
                  className="btn btn-danger"
                  onClick={() => void remove(editing)}
                  disabled={drawerPending}
                >
                  Delete
                </button>
              )}
              <button type="button" className="btn btn-secondary" onClick={closeDrawer} disabled={drawerPending}>
                Cancel
              </button>
              <button type="submit" className="btn btn-primary" form="provider-form" disabled={drawerPending}>
                Save
              </button>
            </>
          }
        >
          <form className="form" id="provider-form" onSubmit={submit}>
            {drawerError !== null && (
              <p role="alert" style={{ color: 'var(--status-down)', fontSize: 'var(--fs-sm)', margin: 0 }}>
                {drawerError}
              </p>
            )}
            <div className="form-row">
              <div className="field">
                <label htmlFor="p-id">ID</label>
                <input
                  id="p-id"
                  className="input mono"
                  type="text"
                  value={form.id}
                  onChange={(e) => setForm((f) => ({ ...f, id: e.target.value }))}
                  disabled={!creating}
                  required
                  placeholder="e.g. ollama-cloud"
                />
                <span className="hint">Stable identifier used by models and routing rules.</span>
              </div>
              <div className="field has-select">
                <label htmlFor="p-type">Type</label>
                <select
                  id="p-type"
                  value={form.type}
                  onChange={(e) => setForm((f) => ({ ...f, type: e.target.value }))}
                >
                  {PROVIDER_TYPES.map((t) => (
                    <option key={t} value={t}>
                      {t}
                    </option>
                  ))}
                </select>
                <span className="hint">
                  Type-specific settings: codex-oauth needs an issuer (e.g. https://auth.openai.com).
                </span>
              </div>
            </div>
            <div className="field">
              <label htmlFor="p-name">Name</label>
              <input
                id="p-name"
                className="input"
                type="text"
                value={form.name}
                onChange={(e) => setForm((f) => ({ ...f, name: e.target.value }))}
                required
                placeholder="Display name"
              />
            </div>
            <div className="field">
              <label htmlFor="p-config">Config (JSON)</label>
              <textarea
                id="p-config"
                className="textarea mono"
                rows={8}
                value={form.config}
                onChange={(e) => setForm((f) => ({ ...f, config: e.target.value }))}
                spellCheck={false}
              />
              <span className="hint">
                Type-specific settings (e.g. base_url for custom-openai). Values are stored encrypted at rest.
                Secret-shaped keys are rejected server-side.
              </span>
            </div>
            {!creating && editing !== null && form.type === 'codex-oauth' && (
              <fieldset className="fieldset">
                <legend>ChatGPT / Codex sign-in</legend>
                <CodexSignIn providerId={editing.id} onApproved={reload} />
              </fieldset>
            )}
          </form>
        </Drawer>
      )}
    </>
  );
}