import { FormEvent, useCallback, useEffect, useMemo, useState } from 'react';
import { useParams } from 'react-router-dom';
import { apiCall } from '../api';
import type {
  Account,
  ProviderTypeDetailResponse,
  ProviderTypesResponse,
  RulesResponse,
} from '../types';
import { EmptyState, ErrorNote, PageHead, useAdminData, useMutation } from '../hooks';
import { Drawer } from '../components/Overlays';
import OAuthSignIn from '../components/OAuthSignIn';
import { IconPlus } from '../icons';

// Per-type provider settings (ADR-009): shared connection settings form
// (rendered from the catalog — no JSON editing), the account list of the
// type, and the add-account flow: a simple name+key form for api_key
// types, or a guided device sign-in for oauth types. Accounts are provider
// rows; a chain entry naming the type expands to all of them at request
// time.

// asStringMap narrows the settings blob to the form's string fields.
function asStringMap(v: unknown): Record<string, string> {
  if (v === null || typeof v !== 'object' || Array.isArray(v)) return {};
  const out: Record<string, string> = {};
  for (const [k, val] of Object.entries(v as Record<string, unknown>)) {
    if (typeof val === 'string') out[k] = val;
  }
  return out;
}

export default function ProviderTypePage() {
  const { type = '' } = useParams<{ type: string }>();

  // The catalog carries the form descriptors (labels, hints, kinds); the
  // detail endpoint carries the saved settings and the accounts.
  const { data: catalogData, error: catalogError, loading: catalogLoading } = useAdminData<ProviderTypesResponse>(
    (signal) => apiCall<ProviderTypesResponse>('GET', '/api/admin/provider-types', undefined, signal),
  );
  const meta = useMemo(() => catalogData?.types.find((t) => t.type === type) ?? null, [catalogData, type]);

  const detail = useAdminData<ProviderTypeDetailResponse>(
    (signal) =>
      apiCall<ProviderTypeDetailResponse>('GET', `/api/admin/provider-types/${encodeURIComponent(type)}`, undefined, signal),
    [type],
  );
  // Routing rules power the delete warning: an account referenced by an
  // exact-ID chain entry must not vanish without a hint.
  const rules = useAdminData<RulesResponse>(
    (signal) => apiCall<RulesResponse>('GET', '/api/admin/routing', undefined, signal),
  );

  const accounts = useMemo(() => detail.data?.accounts ?? [], [detail.data]);
  const savedSettings = useMemo(() => asStringMap(detail.data?.settings), [detail.data]);

  // Settings form state, seeded once per loaded detail via effect (a
  // render-phase setState from props would be a side effect).
  const [settingsForm, setSettingsForm] = useState<Record<string, string>>({});
  const [settingsSeeded, setSettingsSeeded] = useState(false);
  const [settingsError, setSettingsError] = useState<string | null>(null);
  const saveSettings = useMutation();

  useEffect(() => {
    if (settingsSeeded || meta === null) return;
    const seed: Record<string, string> = {};
    for (const f of meta.settings_fields) seed[f.key] = savedSettings[f.key] ?? '';
    setSettingsForm(seed);
    setSettingsSeeded(true);
  }, [settingsSeeded, meta, savedSettings]);

  const submitSettings = useCallback(
    async (e: FormEvent) => {
      e.preventDefault();
      if (meta === null) return;
      const settings: Record<string, string> = {};
      for (const f of meta.settings_fields) {
        const value = settingsForm[f.key] ?? '';
        if (value !== '') settings[f.key] = value;
      }
      // Required-field client check mirrors the server's 400 — instant
      // feedback, the server remains the authority.
      for (const f of meta.settings_fields) {
        if (f.required && settings[f.key] === undefined) {
          setSettingsError(`${f.label} is required`);
          return;
        }
      }
      setSettingsError(null);
      const ok = await saveSettings.run(() =>
        apiCall('PUT', `/api/admin/provider-types/${encodeURIComponent(type)}/settings`, { settings }),
      );
      if (ok !== null) detail.reload();
    },
    [meta, settingsForm, saveSettings, type, detail],
  );

  // Add-account flow state.
  const [adding, setAdding] = useState(false);
  const [accountName, setAccountName] = useState('');
  const [apiKey, setApiKey] = useState('');
  const [addError, setAddError] = useState<string | null>(null);
  const [oauthDraftId, setOauthDraftId] = useState<string | null>(null);
  const addAccount = useMutation();

  const closeAdd = useCallback(() => {
    setAdding(false);
    setAccountName('');
    setApiKey('');
    setAddError(null);
    setOauthDraftId(null);
    addAccount.clearError();
  }, [addAccount]);

  const submitAdd = useCallback(
    async (e: FormEvent) => {
      e.preventDefault();
      if (accountName.trim() === '') {
        setAddError('Name is required');
        return;
      }
      setAddError(null);
      const body: { name: string; api_key?: string } = { name: accountName.trim() };
      if (meta?.auth === 'api_key' && apiKey !== '') body.api_key = apiKey;
      const created = await addAccount.run(() =>
        apiCall<{ account: Account }>('POST', `/api/admin/provider-types/${encodeURIComponent(type)}/accounts`, body),
      );
      if (created === null) return;
      if (meta?.auth === 'oauth') {
        // Draft created — the guided sign-in attaches to it next.
        setOauthDraftId(created.account.id);
        detail.reload();
        return;
      }
      closeAdd();
      detail.reload();
    },
    [accountName, apiKey, meta, addAccount, type, closeAdd, detail],
  );

  const removeAccount = useCallback(
    async (a: Account) => {
      const ok = await addAccount.run(() =>
        apiCall('DELETE', `/api/admin/providers/${encodeURIComponent(a.id)}`),
      );
      if (ok !== null) detail.reload();
    },
    [addAccount, detail],
  );

  // Client-side delete warning: is this account's ID in any rule chain?
  const referencedIds = useMemo(() => {
    const ids = new Set<string>();
    for (const r of rules.data?.rules ?? []) {
      for (const entry of r.chain) ids.add((entry.split('/')[0] ?? '').trim());
    }
    return ids;
  }, [rules.data]);

  if (catalogError !== null) {
    return <ErrorNote message={catalogError} />;
  }
  if (catalogLoading || meta === null) {
    return <EmptyState title="Loading provider type" body="Fetching the provider catalog." />;
  }

  return (
    <>
      <PageHead
        title={meta.label}
        sub={meta.description}
      />
      <div className="canvas">
        <div className="w-full" style={{ display: 'flex', flexDirection: 'column', gap: 24 }}>
          {/* Connection settings — form-based, rendered from the catalog. */}
          <form className="form" onSubmit={submitSettings}>
            <fieldset className="fieldset">
              <legend>Connection settings</legend>
              <p className="hint" style={{ marginTop: 0 }}>
                Shared by all accounts of this type. Account-level fields override these.
              </p>
              {(settingsError ?? saveSettings.error) !== null && (
                <p role="alert" style={{ color: 'var(--status-down)', fontSize: 'var(--fs-sm)', margin: 0 }}>
                  {settingsError ?? saveSettings.error}
                </p>
              )}
              <div className="form-row">
                {meta.settings_fields.map((f) => (
                  <div className="field" key={f.key}>
                    <label htmlFor={`set-${f.key}`}>{f.label}{f.required ? ' *' : ''}</label>
                    <input
                      id={`set-${f.key}`}
                      className="input"
                      type={f.kind === 'url' ? 'url' : 'text'}
                      value={settingsForm[f.key] ?? ''}
                      onChange={(e) => setSettingsForm((s) => ({ ...s, [f.key]: e.target.value }))}
                      placeholder={f.hint}
                      required={f.required}
                    />
                    <span className="hint">{f.hint}</span>
                  </div>
                ))}
              </div>
              <button type="submit" className="btn btn-primary btn-sm" disabled={saveSettings.pending}>
                Save settings
              </button>
            </fieldset>
          </form>

          {/* Accounts — one row per provider row of this type. minw-0 keeps
              the flex column from being stretched past the viewport by the
              table's min-width on mobile (the scroll container handles it). */}
          <div style={{ minWidth: 0 }}>
            {/* Header row keeps ONLY fixed-width content (heading + button);
                the hint is a separate block below. A wrap-sensitive flex line
                here was the recurring mobile screenshot flake: font-metric
                drift between ubuntu-latest image rollouts tips the hint's
                break point and shifts everything below by one line (v1.0.1
                0b6679f, v1.0.2 regen) — block layout has no break point to
                tip. */}
            <div style={{ display: 'flex', alignItems: 'center', gap: 12, marginBottom: 4 }}>
              <h3 style={{ margin: 0 }}>Accounts</h3>
              <button
                type="button"
                className="btn btn-primary btn-sm"
                style={{ marginLeft: 'auto', flexShrink: 0 }}
                onClick={() => setAdding(true)}
              >
                <IconPlus width="14" height="14" />
                Add account
              </button>
            </div>
            <p className="hint" style={{ margin: '0 0 12px', overflowWrap: 'anywhere' }}>
              Chain entries naming “{meta.type}” spread requests over all accounts.
            </p>
            {detail.error !== null ? (
              <ErrorNote message={detail.error} />
            ) : accounts.length === 0 ? (
              <EmptyState
                title="No accounts yet"
                body={
                  meta.auth === 'oauth'
                    ? 'Add an account and complete the sign-in — tokens land encrypted on the server.'
                    : 'Add an account with its API key — the key is stored encrypted and never shown again.'
                }
              />
            ) : (
              <div className="table-wrap">
                <div className="table-scroll">
                  <table className="table">
                    <thead>
                      <tr>
                        <th>Name</th>
                        <th>ID</th>
                        <th>Status</th>
                        <th />
                      </tr>
                    </thead>
                    <tbody>
                      {accounts.map((a) => (
                        <tr key={a.id}>
                          <td className="primary">{a.name}</td>
                          <td className="mono">{a.id}</td>
                          <td>
                            {a.has_credential ? (
                              <span className="badge up">
                                <span className="b-dot" />
                                Connected
                              </span>
                            ) : (
                              <span className="badge paused">
                                <span className="b-dot" />
                                Sign-in required
                              </span>
                            )}
                          </td>
                          <td>
                            <span className="row-actions">
                              <button
                                type="button"
                                className="btn btn-danger btn-sm"
                                onClick={() => void removeAccount(a)}
                              >
                                Delete
                              </button>
                            </span>
                          </td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                </div>
              </div>
            )}
            {/* Referenced-account hint: cheap client-side check against the
                loaded rules; the server's chain validation is the gate. */}
            {accounts.some((a) => referencedIds.has(a.id)) && (
              <p className="hint" style={{ marginTop: 10, overflowWrap: 'anywhere' }}>
                One or more accounts are referenced by routing rules (exact-ID entries).
                Deleting them leaves those rule entries skipping with a traced reason.
              </p>
            )}
          </div>
        </div>
      </div>

      {adding && (
        <Drawer
          title={`Add ${meta.label} account`}
          onClose={closeAdd}
          footer={
            oauthDraftId === null ? (
              <>
                <button type="button" className="btn btn-secondary" onClick={closeAdd} disabled={addAccount.pending}>
                  Cancel
                </button>
                <button type="submit" className="btn btn-primary" form="account-form" disabled={addAccount.pending}>
                  {meta.auth === 'oauth' ? 'Create & sign in' : 'Add account'}
                </button>
              </>
            ) : (
              <button type="button" className="btn btn-secondary" onClick={closeAdd}>
                Close
              </button>
            )
          }
        >
          {oauthDraftId === null ? (
            <form className="form" id="account-form" onSubmit={submitAdd}>
              {(addError ?? addAccount.error) !== null && (
                <p role="alert" style={{ color: 'var(--status-down)', fontSize: 'var(--fs-sm)', margin: 0 }}>
                  {addError ?? addAccount.error}
                </p>
              )}
              <div className="field">
                <label htmlFor="acc-name">Account name</label>
                <input
                  id="acc-name"
                  className="input"
                  type="text"
                  value={accountName}
                  onChange={(e) => setAccountName(e.target.value)}
                  placeholder="e.g. Work account"
                  required
                />
                <span className="hint">A label to recognize this account by.</span>
              </div>
              {meta.auth === 'api_key' && (
                <div className="field">
                  <label htmlFor="acc-key">API key</label>
                  <input
                    id="acc-key"
                    className="input mono"
                    type="password"
                    value={apiKey}
                    onChange={(e) => setApiKey(e.target.value)}
                    autoComplete="off"
                  />
                  <span className="hint">
                    Stored encrypted on the server (write-only — it never comes back to the browser).
                  </span>
                </div>
              )}
              {meta.auth === 'oauth' && (
                <p className="hint">
                  Creating the account opens the guided device sign-in: you get a code to
                  enter on the issuer page, the proxy does the rest.
                </p>
              )}
            </form>
          ) : (
            <div className="form">
              <span className="badge up">
                <span className="b-dot" />
                Account created
              </span>
              <p className="hint">Step 2: complete the sign-in for the new account.</p>
              <fieldset className="fieldset">
                <legend>Sign in</legend>
                <OAuthSignIn providerId={oauthDraftId} onApproved={() => detail.reload()} />
              </fieldset>
            </div>
          )}
        </Drawer>
      )}

    </>
  );
}