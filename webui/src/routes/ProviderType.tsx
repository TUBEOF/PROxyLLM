import { FormEvent, useCallback, useEffect, useMemo, useRef, useState } from 'react';
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
import { IconCheck, IconClose, IconPlus } from '../icons';

// The how-to for an ollama.com session cookie lives in the repository's
// operations guide — the console carries the short version and the link, so
// there is one place where the procedure is explained.
const COOKIE_DOCS_URL = 'https://github.com/TUBEOF/PROxyLLM/blob/main/docs/operating.md#the-session-cookie';

// Per-type provider settings (ADR-009): shared connection settings form
// (rendered from the catalog — no JSON editing), the account list of the
// type, and the add-account flow: a simple name+key form for api_key
// types, or a guided device sign-in for oauth types. Accounts are provider
// rows; a chain entry naming the type expands to all of them at request
// time.

// asStringMap narrows the settings blob to the form's string fields.
// aliasRowsFrom reads the alias map out of a settings blob. The aliases are an
// object field beside the catalog's text fields, so the value arrives as
// `unknown` and has to be narrowed before it can become rows.
function aliasRowsFrom(settings: unknown): { alias: string; target: string }[] {
  const raw = (settings as Record<string, unknown> | null)?.aliases;
  const rows: { alias: string; target: string }[] = [];
  if (raw !== null && raw !== undefined && typeof raw === 'object' && !Array.isArray(raw)) {
    for (const [alias, target] of Object.entries(raw as Record<string, unknown>)) {
      if (typeof target === 'string') rows.push({ alias, target });
    }
  }
  return rows;
}

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

  // Settings form state, seeded once per loaded type via effect (a
  // render-phase setState from props would be a side effect).
  const [settingsForm, setSettingsForm] = useState<Record<string, string>>({});
  const [settingsError, setSettingsError] = useState<string | null>(null);
  const saveSettings = useMutation();

  // Type aliases (object field in the settings blob, not a catalog text
  // field): seeded alongside the string fields.
  const [aliasRows, setAliasRows] = useState<{ alias: string; target: string }[]>([]);

  // The catalog — which names the fields — and the detail — which carries their
  // values — arrive from two independent requests, and the detail is refetched
  // when the type changes while keeping its previous value until the new one
  // lands. So the form is filled only when both are in and neither is in flight,
  // and it is refilled when the type changes.
  //
  // Getting this wrong is not cosmetic: filling the form from a catalog that
  // arrived first leaves every field empty, and saving that form writes the
  // emptiness back over the saved settings.
  const seededType = useRef<string | null>(null);
  useEffect(() => {
    if (meta === null || detail.loading || detail.data === null || seededType.current === type) return;
    const seed: Record<string, string> = {};
    for (const f of meta.settings_fields) seed[f.key] = savedSettings[f.key] ?? '';
    setSettingsForm(seed);
    setAliasRows(aliasRowsFrom(detail.data.settings));
    seededType.current = type;
  }, [type, meta, detail.loading, detail.data, savedSettings]);

  const submitSettings = useCallback(
    async (e: FormEvent) => {
      e.preventDefault();
      if (meta === null) return;
      const settings: Record<string, unknown> = {};
      for (const f of meta.settings_fields) {
        const value = settingsForm[f.key] ?? '';
        if (value !== '') settings[f.key] = value;
      }
      // Aliases: only complete rows ride (an empty alias or target row is a
      // not-yet-typed draft, not an invalid entry — the server 400s
      // half-typed pairs so the client filters them here).
      const aliases: Record<string, string> = {};
      for (const row of aliasRows) {
        const alias = row.alias.trim();
        const target = row.target.trim();
        if (alias === '' || target === '') continue;
        if (aliases[alias] !== undefined) {
          setSettingsError(`Duplicate alias "${alias}"`);
          return;
        }
        aliases[alias] = target;
      }
      if (Object.keys(aliases).length > 0) settings.aliases = aliases;
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
    [meta, settingsForm, aliasRows, saveSettings, type, detail],
  );

  // Add-account flow state.
  const [adding, setAdding] = useState(false);
  const [accountName, setAccountName] = useState('');
  const [apiKey, setApiKey] = useState('');
  const [usageCookie, setUsageCookie] = useState('');
  const [addError, setAddError] = useState<string | null>(null);
  const [oauthDraftId, setOauthDraftId] = useState<string | null>(null);
  const addAccount = useMutation();

  const closeAdd = useCallback(() => {
    setAdding(false);
    setAccountName('');
    setApiKey('');
    setUsageCookie('');
    setAddError(null);
    setOauthDraftId(null);
    addAccount.clearError();
  }, [addAccount]);

  // Edit-account flow (name, credential, session cookie) and the pool
  // switch. The switch is its own row control: it is the one edit an
  // operator makes repeatedly, while the rest of the account is set once.
  const [editingAccount, setEditingAccount] = useState<Account | null>(null);
  const [editName, setEditName] = useState('');
  const [editKey, setEditKey] = useState('');
  // The cookie field's three states: '' with clear=false keeps the stored
  // one, a value replaces it, clear=true removes it — what the API expects
  // (absent / set / "").
  const [editCookie, setEditCookie] = useState('');
  const [clearCookie, setClearCookie] = useState(false);
  const [editError, setEditError] = useState<string | null>(null);
  const saveAccount = useMutation();

  const openEditAccount = useCallback((a: Account) => {
    setEditingAccount(a);
    setEditName(a.name);
    setEditKey('');
    setEditCookie('');
    setClearCookie(false);
    setEditError(null);
  }, []);

  const closeEditAccount = useCallback(() => {
    setEditingAccount(null);
    setEditError(null);
    saveAccount.clearError();
  }, [saveAccount]);

  const submitEditAccount = useCallback(
    async (e: FormEvent) => {
      e.preventDefault();
      if (editingAccount === null) return;
      if (editName.trim() === '') {
        setEditError('Name is required');
        return;
      }
      setEditError(null);
      const body: Record<string, unknown> = {
        type: editingAccount.type,
        name: editName.trim(),
        config: {},
      };
      if (editKey !== '') body.api_key = editKey;
      if (meta?.usage_cookie === true) {
        if (clearCookie) body.usage_cookie = '';
        else if (editCookie !== '') body.usage_cookie = editCookie;
      }
      const ok = await saveAccount.run(() =>
        apiCall('PUT', `/api/admin/providers/${encodeURIComponent(editingAccount.id)}`, body),
      );
      if (ok !== null) {
        closeEditAccount();
        detail.reload();
      }
    },
    [editingAccount, editName, editKey, editCookie, clearCookie, meta, saveAccount, closeEditAccount, detail],
  );

  const toggleAccount = useCallback(
    async (a: Account, enabled: boolean) => {
      const ok = await addAccount.run(() =>
        apiCall('PUT', `/api/admin/providers/${encodeURIComponent(a.id)}/enabled`, { enabled }),
      );
      if (ok !== null) detail.reload();
    },
    [addAccount, detail],
  );

  const submitAdd = useCallback(
    async (e: FormEvent) => {
      e.preventDefault();
      if (accountName.trim() === '') {
        setAddError('Name is required');
        return;
      }
      setAddError(null);
      const body: { name: string; api_key?: string; usage_cookie?: string } = { name: accountName.trim() };
      if (meta?.auth === 'api_key' && apiKey !== '') body.api_key = apiKey;
      if (meta?.usage_cookie === true && usageCookie !== '') body.usage_cookie = usageCookie;
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
    [accountName, apiKey, usageCookie, meta, addAccount, type, closeAdd, detail],
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
                <p role="alert" className="form-note">
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
              {meta.kind === 'llm' && (
                <div className="field">
                  <label>Model aliases</label>
                  <span className="hint">
                    Client-facing names routed to this type&apos;s models — clients may send the alias instead of the prefixed ID.
                  </span>
                  <div style={{ display: 'flex', flexDirection: 'column', gap: 6 }}>
                    {aliasRows.map((row, i) => (
                      <div key={i} style={{ display: 'flex', gap: 6, alignItems: 'center' }}>
                        <input
                          className="input mono"
                          style={{ flex: 1 }}
                          placeholder="alias"
                          value={row.alias}
                          aria-label={`Alias ${i + 1}`}
                          onChange={(e) =>
                            setAliasRows((rows) => rows.map((r, j) => (j === i ? { ...r, alias: e.target.value } : r)))
                          }
                        />
                        <span aria-hidden>→</span>
                        <input
                          className="input mono"
                          style={{ flex: 1 }}
                          placeholder="model id"
                          value={row.target}
                          aria-label={`Target ${i + 1}`}
                          onChange={(e) =>
                            setAliasRows((rows) => rows.map((r, j) => (j === i ? { ...r, target: e.target.value } : r)))
                          }
                        />
                        <button
                          type="button"
                          className="btn btn-danger btn-sm"
                          aria-label={`Remove alias ${i + 1}`}
                          onClick={() => setAliasRows((rows) => rows.filter((_, j) => j !== i))}
                        >
                          ✕
                        </button>
                      </div>
                    ))}
                    <div>
                      <button
                        type="button"
                        className="btn btn-sm"
                        onClick={() => setAliasRows((rows) => [...rows, { alias: '', target: '' }])}
                      >
                        <IconPlus width="14" height="14" />
                        Add alias
                      </button>
                    </div>
                  </div>
                </div>
              )}
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
              Chain entries naming “{meta.type}” spread requests over all accounts that are in the pool.
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
                        {meta.usage_cookie && <th>Session cookie</th>}
                        <th>In the pool</th>
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
                          {meta.usage_cookie && (
                            <td>
                              {/* Presence only — the cookie itself never
                                  comes back to the browser. */}
                              {a.has_usage_cookie === true ? (
                                <span className="badge up" aria-label="Session cookie set">
                                  <IconCheck width="12" height="12" />
                                  set
                                </span>
                              ) : (
                                <span className="badge paused" aria-label="No session cookie">
                                  <IconClose width="12" height="12" />
                                  not set
                                </span>
                              )}
                            </td>
                          )}
                          <td>
                            <label className="toggle" title={a.enabled ? 'In the pool — this account serves traffic' : 'Out of the pool — it serves nothing until switched back on'}>
                              <input
                                type="checkbox"
                                checked={a.enabled}
                                onChange={(e) => void toggleAccount(a, e.target.checked)}
                                disabled={addAccount.pending}
                                aria-label={`${a.name} in the pool`}
                              />
                              <span className="tswitch" />
                            </label>
                          </td>
                          <td>
                            <span className="row-actions">
                              <button
                                type="button"
                                className="btn btn-secondary btn-sm"
                                onClick={() => openEditAccount(a)}
                              >
                                Edit
                              </button>
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

      {editingAccount !== null && (
        <Drawer
          title={`Edit ${editingAccount.name}`}
          onClose={closeEditAccount}
          footer={
            <>
              <button type="button" className="btn btn-secondary" onClick={closeEditAccount} disabled={saveAccount.pending}>
                Cancel
              </button>
              <button type="submit" className="btn btn-primary" form="account-edit-form" disabled={saveAccount.pending}>
                Save
              </button>
            </>
          }
        >
          <form className="form" id="account-edit-form" onSubmit={submitEditAccount}>
            {(editError ?? saveAccount.error) !== null && (
              <p role="alert" className="form-note">
                {editError ?? saveAccount.error}
              </p>
            )}
            <div className="field">
              <label htmlFor="ae-name">Account name</label>
              <input
                id="ae-name"
                className="input"
                type="text"
                value={editName}
                onChange={(e) => setEditName(e.target.value)}
                required
              />
              <span className="hint">A label to recognize this account by.</span>
            </div>
            {meta.auth === 'api_key' && (
              <div className="field">
                <label htmlFor="ae-key">API key</label>
                <input
                  id="ae-key"
                  className="input mono"
                  type="password"
                  value={editKey}
                  onChange={(e) => setEditKey(e.target.value)}
                  autoComplete="off"
                  placeholder="Leave empty to keep the stored key"
                />
                <span className="hint">
                  A value replaces the stored key (write-only — it never comes back to the browser).
                </span>
              </div>
            )}
            {meta.usage_cookie && (
              <fieldset className="fieldset">
                <legend>Session cookie</legend>
                <p className="hint" style={{ marginTop: 0 }}>
                  {editingAccount.has_usage_cookie === true
                    ? 'A cookie is stored. It feeds this account’s usage windows on the Quota page.'
                    : 'No cookie stored yet — the Quota card counts locally until one is.'}
                </p>
                <div className="field">
                  <label htmlFor="ae-cookie">Set or replace it</label>
                  <input
                    id="ae-cookie"
                    className="input mono"
                    type="password"
                    value={editCookie}
                    onChange={(e) => {
                      setEditCookie(e.target.value);
                      if (e.target.value !== '') setClearCookie(false);
                    }}
                    autoComplete="off"
                    disabled={clearCookie}
                    placeholder={editingAccount.has_usage_cookie === true ? 'Leave empty to keep the stored cookie' : '__Secure-session value'}
                  />
                  <span className="hint">Stored encrypted; it never comes back to the browser.</span>
                </div>
                {editingAccount.has_usage_cookie === true && (
                  <label className="check" style={{ display: 'flex', marginBottom: 8 }}>
                    <input
                      type="checkbox"
                      checked={clearCookie}
                      onChange={(e) => {
                        setClearCookie(e.target.checked);
                        if (e.target.checked) setEditCookie('');
                      }}
                    />
                    <span className="cbox">
                      <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="3.5" aria-hidden="true">
                        <path d="m5 13 4 4 10-10" />
                      </svg>
                    </span>
                    Remove the stored cookie
                  </label>
                )}
                <ol className="hint" style={{ margin: 0, paddingLeft: 18 }}>
                  <li>Sign in at ollama.com in your browser.</li>
                  <li>
                    Open the developer tools &rarr; Application &rarr; Cookies &rarr; https://ollama.com and
                    copy the value of <span className="mono">__Secure-session</span>.
                  </li>
                  <li>Paste it above and save.</li>
                </ol>
                <p className="hint" style={{ marginBottom: 0 }}>
                  <a href={COOKIE_DOCS_URL} target="_blank" rel="noreferrer">
                    Full instructions in the documentation
                  </a>
                </p>
              </fieldset>
            )}
          </form>
        </Drawer>
      )}

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
                <p role="alert" className="form-note">
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
              {meta.usage_cookie && (
                <div className="field">
                  <label htmlFor="acc-cookie">Session cookie (optional)</label>
                  <input
                    id="acc-cookie"
                    className="input mono"
                    type="password"
                    value={usageCookie}
                    onChange={(e) => setUsageCookie(e.target.value)}
                    autoComplete="off"
                  />
                  <span className="hint">
                    The ollama.com session cookie that feeds this account&apos;s usage windows on the
                    Quota page. Stored encrypted, never shown again — and you can leave it out now
                    and set it later on the account row.{' '}
                    <a href={COOKIE_DOCS_URL} target="_blank" rel="noreferrer">
                      How to get it
                    </a>
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