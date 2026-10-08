import { FormEvent, useCallback, useEffect, useMemo, useState } from 'react';
import { apiCall } from '../api';
import type {
  Model,
  ModelSyncConfig,
  ModelSyncRunResponse,
  ModelsResponse,
  ModelTestResponse,
  Provider,
  ProvidersResponse,
  SettingsResponse,
} from '../types';
import { CONFIG_LIVE_MS, EmptyState, ErrorNote, PageHead, useAdminData, useLiveData, useMutation } from '../hooks';
import { LiveBadge } from '../components/LiveBadge';

// The relocated Settings-editor keys (2026-10-06): model-surface policy,
// not compat knobs.
const NON_CLAUDE_KEY = 'anthropic_surface_non_claude';
const ALIASES_KEY = 'model_aliases';
const NON_CLAUDE_VALUES = ['allow', 'warn', 'block'] as const;
import { Drawer } from '../components/Overlays';
import { IconPlus } from '../icons';

// "Models" (plan §9) — the central per-model editor: identity, provider
// link, capabilities, priority. The compat knobs are NOT editable anymore
// (2026-10-06, owner decision "Alles raus, automatisch"): the per-model
// policy derives from the row's capabilities server-side (thinking
// handling, cache markers, server-tool emulation, clamps — every
// application still recorded in route_trace, never silent). Legacy compat
// blobs on existing rows keep working as read-only overrides.
//
// Layout (2026-09-22): provider-grouped collapsible sections with model
// CARDS (not a flat table) — the per-model test button needs a card-level
// footer, and the grouping matches how operators think (per account). Each
// card carries a Test button: one minimal "Sag OK" round trip against the
// model's provider, minimal output (status + reply or plain-language
// cause). Block layout throughout — no variable-length text in flex-wrap
// lines above other content (the v1.0.1/v1.0.2 screenshot lesson).

const CAPABILITY_FLAGS = ['streaming', 'tools', 'vision', 'thinking', 'web_search', 'web_fetch', 'code_execution'] as const;

function capsFromRaw(raw: unknown): Record<string, boolean> {
  const out: Record<string, boolean> = {};
  for (const flag of CAPABILITY_FLAGS) {
    out[flag] = false;
  }
  if (raw !== null && typeof raw === 'object') {
    const o = raw as Record<string, unknown>;
    if (Array.isArray(o.tools)) {
      for (const t of o.tools) {
        if (typeof t === 'string' && t in out) out[t] = true;
      }
    }
    for (const flag of CAPABILITY_FLAGS) {
      if (o[flag] === true) out[flag] = true;
    }
  }
  return out;
}

function parsePositiveInt(s: string): number | null {
  const n = Number(s);
  if (!Number.isInteger(n) || n < 0) return null;
  return n;
}

export default function Models() {
  const { data, loading, error, reload, updatedAt } = useLiveData<ModelsResponse>(
    (signal) => apiCall<ModelsResponse>('GET', '/api/admin/models', undefined, signal),
    CONFIG_LIVE_MS,
  );
  const { data: providersData } = useLiveData<ProvidersResponse>(
    (signal) => apiCall<ProvidersResponse>('GET', '/api/admin/providers', undefined, signal),
    CONFIG_LIVE_MS,
  );
  const models = useMemo(() => data?.models ?? [], [data]);
  const providers = useMemo(() => providersData?.providers ?? [], [providersData]);
  // providerNames: the sync summary names the accounts it touched; a
  // provider that has since been deleted falls back to its id.
  const providerNames = useMemo(
    () => new Map(providers.map((p) => [p.id, p.name])),
    [providers],
  );

  const [editing, setEditing] = useState<Model | null>(null);
  const [creating, setCreating] = useState(false);
  const [formError, setFormError] = useState<string | null>(null);
  const [form, setForm] = useState({
    id: '',
    provider_id: '',
    name: '',
    priority: '0',
    capabilities: {} as Record<string, boolean>,
  });

  const save = useMutation();
  const del = useMutation();

  const openCreate = useCallback(() => {
    setForm({
      id: '',
      provider_id: providers[0]?.id ?? '',
      name: '',
      priority: '0',
      capabilities: capsFromRaw(null),
    });
    setCreating(true);
    setEditing(null);
    setFormError(null);
  }, [providers]);

  const openEdit = useCallback((m: Model) => {
    setForm({
      id: m.id,
      provider_id: m.provider_id,
      name: m.name,
      priority: String(m.priority),
      capabilities: capsFromRaw(m.capabilities),
    });
    setEditing(m);
    setCreating(false);
    setFormError(null);
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
      const id = creating ? form.id.trim() : form.id;
      if (id === '') {
        setFormError('ID is required');
        return;
      }
      if (form.provider_id === '') {
        setFormError('Pick a provider first — models belong to one.');
        return;
      }
      const priority = parsePositiveInt(form.priority);
      if (priority === null) {
        setFormError('Priority must be a non-negative integer');
        return;
      }
      setFormError(null);
      const caps: Record<string, unknown> = { tools: Object.keys(form.capabilities).filter((k) => form.capabilities[k]) };
      // Operator save = manual selection: every flag written here is
      // pinned so the automatic capability learning (2026-09-21) stops
      // overwriting it. Unchecking removes the pin (learning resumes).
      const pinned: Record<string, boolean> = {};
      for (const flag of CAPABILITY_FLAGS) {
        const on = form.capabilities[flag] === true;
        caps[flag] = on;
        pinned[flag] = on;
      }
      caps.pinned = pinned;
      // NOTE: compat is NOT sent — the row's stored blob (legacy) stays
      // untouched; the server derives the policy from capabilities.
      const ok = await save.run(() =>
        apiCall('PUT', `/api/admin/models/${encodeURIComponent(id)}`, {
          provider_id: form.provider_id,
          name: form.name,
          capabilities: caps,
          priority,
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
    async (m: Model) => {
      const ok = await del.run(() => apiCall('DELETE', `/api/admin/models/${encodeURIComponent(m.id)}`));
      if (ok) reload();
    },
    [del, reload],
  );

  // ADR-010 model sync: config load + manual run. The sync status card
  // sits above the table; "Sync now" runs a pass synchronously and
  // refreshes the list (the run summary lands in the card).
  const { data: syncData, reload: reloadSync } = useAdminData<ModelSyncConfig>(
    (signal) => apiCall<ModelSyncConfig>('GET', '/api/admin/model-sync', undefined, signal),
  );
  const syncRun = useMutation();

  const runSync = useCallback(async () => {
    const ok = await syncRun.run(() =>
      apiCall<ModelSyncRunResponse>('POST', '/api/admin/model-sync/run', {}),
    );
    if (ok !== null) {
      reloadSync();
      reload();
    }
  }, [syncRun, reloadSync, reload]);

  const toggleSync = useCallback(
    async (enabled: boolean) => {
      const ok = await syncRun.run(() => apiCall('PUT', '/api/admin/model-sync', { enabled }));
      if (ok !== null) reloadSync();
    },
    [syncRun, reloadSync],
  );

  const lastRun = syncData?.last_run ?? null;
  const lastRunText = lastRun === null || lastRun.ran_at === 0
    ? 'never run'
    : new Date(lastRun.ran_at * 1000).toLocaleString();

  // Surface policy (relocated from the removed Settings page, 2026-10-06):
  // the non-Claude gate + the Claude Code alias map — model-surface
  // policy, not compat knobs. Same key/value writes; the editors' shapes
  // are unchanged.
  const { data: settingsData, reload: reloadSettings } = useAdminData<SettingsResponse>(
    (signal) => apiCall<SettingsResponse>('GET', '/api/admin/settings', undefined, signal),
  );
  const entries = useMemo(() => settingsData?.entries ?? [], [settingsData]);
  const [aliasesText, setAliasesText] = useState('');
  const [aliasesError, setAliasesError] = useState<string | null>(null);
  const saveAlias = useMutation();
  const saveSwitch = useMutation();

  const valueOf = useCallback(
    (key: string): string => entries.find((e) => e.key === key)?.value ?? '',
    [entries],
  );
  const nonClaudeValue = useMemo(() => {
    const raw = valueOf(NON_CLAUDE_KEY);
    return (NON_CLAUDE_VALUES as readonly string[]).includes(raw) ? raw : '';
  }, [valueOf]);
  const isDefault = nonClaudeValue === '';

  // Seed the alias textarea from the loaded override once per load — an
  // accidental save must not clobber an existing map with an empty object
  // (the Settings-page lesson carried over verbatim).
  useEffect(() => {
    setAliasesText(entries.find((e) => e.key === ALIASES_KEY)?.value ?? '');
  }, [entries]);

  const setNonClaude = useCallback(
    async (value: string) => {
      const ok = await saveSwitch.run(() => apiCall('PUT', '/api/admin/settings', { key: NON_CLAUDE_KEY, value }));
      if (ok !== null) reloadSettings();
    },
    [saveSwitch, reloadSettings],
  );

  const submitAliases = useCallback(
    async (e: FormEvent) => {
      e.preventDefault();
      let parsed: unknown;
      try {
        parsed = aliasesText.trim() === '' ? {} : JSON.parse(aliasesText);
      } catch {
        setAliasesError('Alias map is not valid JSON');
        return;
      }
      if (parsed === null || typeof parsed !== 'object' || Array.isArray(parsed)) {
        setAliasesError('Alias map must be a JSON object of alias → model id');
        return;
      }
      for (const [alias, target] of Object.entries(parsed as Record<string, unknown>)) {
        if (typeof target !== 'string' || target === '') {
          setAliasesError(`Target for "${alias}" must be a non-empty model id string`);
          return;
        }
      }
      setAliasesError(null);
      const ok = await saveAlias.run(() =>
        apiCall('PUT', '/api/admin/settings', { key: ALIASES_KEY, value: JSON.stringify(parsed) }),
      );
      if (ok !== null) reloadSettings();
    },
    [aliasesText, saveAlias, reloadSettings],
  );

  // Model test (2026-09-22): one minimal round trip per model card. State
  // is per-model: pending + the last result. Latency is NOT rendered in
  // the result badge (wall-clock noise in screenshots); the status text
  // alone carries the outcome.
  const [testing, setTesting] = useState<string | null>(null);
  const [testResults, setTestResults] = useState<Record<string, ModelTestResponse>>({});

  const runTest = useCallback(
    async (m: Model) => {
      setTesting(m.id);
      try {
        const res = await apiCall<ModelTestResponse>(
          'POST',
          `/api/admin/models/test/${encodeURIComponent(m.id)}`,
          {},
        );
        setTestResults((prev) => ({ ...prev, [m.id]: res }));
      } catch (err) {
        // Thrown errors are the truth (useMutation().error is stale by one
        // run — the v1.0.2 lesson).
        setTestResults((prev) => ({
          ...prev,
          [m.id]: { ok: false, latency_ms: 0, error: err instanceof Error ? err.message : 'Request failed' },
        }));
      }
      setTesting(null);
    },
    [],
  );

  // Provider grouping (2026-09-22): models bucket by their provider, in
  // provider-listing order; providers without models appear with an empty
  // hint so the grouping never hides an account.
  const groups = useMemo(() => {
    const byProvider = new Map<string, Model[]>();
    for (const m of models) {
      const list = byProvider.get(m.provider_id) ?? [];
      list.push(m);
      byProvider.set(m.provider_id, list);
    }
    const out: { provider: Provider; models: Model[] }[] = [];
    for (const p of providers) {
      out.push({ provider: p, models: byProvider.get(p.id) ?? [] });
    }
    // Models of deleted providers (dangling rows should not exist — the FK
    // guarantees it — but the UI never hides data it was given).
    const known = new Set(providers.map((p) => p.id));
    for (const [pid, list] of byProvider) {
      if (!known.has(pid)) {
        // A dangling group: the provider row is gone (deleted, or a
        // renamed id), so the id is all that remains. Named as such so the
        // header does not read like a real account name.
        out.push({ provider: { id: pid, type: '', name: `${pid} (unknown provider)`, config: null }, models: list });
      }
    }
    return out;
  }, [models, providers]);

  const drawerError = formError ?? save.error ?? del.error;
  const drawerPending = save.pending || del.pending;

  return (
    <>
      <PageHead
        title="Models & Compatibility"
        sub="Every model's routing identity, capabilities and compatibility policy — the single place non-Claude routing on the Anthropic surface is configured."
        actions={
          <>
            <LiveBadge updatedAt={updatedAt} intervalMs={CONFIG_LIVE_MS} />
            <button
              type="button"
              className="btn btn-secondary"
              onClick={() => void runSync()}
              disabled={syncRun.pending}
              title="Import the model lists of every provider that offers discovery (ADR-010)"
            >
              {syncRun.pending ? 'Syncing…' : 'Sync now'}
            </button>
            <button
              type="button"
              className="btn btn-primary"
              onClick={openCreate}
              disabled={providers.length === 0}
              title={providers.length === 0 ? 'Add a provider first — models belong to one.' : undefined}
            >
              <IconPlus width="16" height="16" />
              Add model
            </button>
          </>
        }
      />
      <div className="canvas">
        {/* ADR-010 model-sync status card. Block layout (the v1.0.1/v1.0.2
            lesson: variable-length text never rides a flex-wrap line above
            other content — the run list renders on its own lines). */}
        <div className="widget w-full" style={{ marginBottom: 8 }}>
          <div className="widget-head">
            <h4 style={{ margin: 0 }}>Auto-sync</h4>
            <label className="toggle" style={{ display: 'flex', gap: 6, alignItems: 'center' }}>
              <input
                type="checkbox"
                checked={syncData?.enabled ?? true}
                onChange={(e) => void toggleSync(e.target.checked)}
                disabled={syncRun.pending}
              />
              <span className="tswitch" />
              enabled
            </label>
          </div>
          <div className="widget-body" style={{ display: 'flex', flexDirection: 'column', gap: 4 }}>
            <span className="hint" style={{ margin: 0 }}>
              every {syncData?.interval_hours ?? 24}h · last run {lastRunText}
            </span>
            {lastRun !== null && lastRun.providers !== undefined && (
              <span className="hint" style={{ margin: 0 }}>
                {lastRun.providers
                  .map((p) => `${providerNames.get(p.provider_id) ?? p.provider_id.slice(0, 12)}: +${p.added} ~${p.updated} −${p.removed}`)
                  .join(' · ')}
              </span>
            )}
            {syncRun.error !== null && (
              <span role="alert" className="form-note">
                {syncRun.error}
              </span>
            )}
          </div>
        </div>

        {/* Surface policy (relocated from the removed Settings page): the
            non-Claude gate is the §6.4 safety net; the alias map is C6. */}
        <div className="widget w-full" style={{ marginBottom: 8 }}>
          <div className="widget-head">
            <h3 className="title">Non-Claude routing on the Anthropic surface</h3>
          </div>
          <div className="widget-body">
            <div className="rpills" role="radiogroup" aria-label="Non-Claude routing mode">
              {NON_CLAUDE_VALUES.map((mode) => (
                <button
                  key={mode}
                  type="button"
                  className={`rpill${(!isDefault && nonClaudeValue === mode) || (isDefault && mode === 'allow') ? ' active' : ''}`}
                  role="radio"
                  aria-checked={(!isDefault && nonClaudeValue === mode) || (isDefault && mode === 'allow')}
                  onClick={() => void setNonClaude(mode)}
                  disabled={saveSwitch.pending}
                >
                  {mode}
                </button>
              ))}
            </div>
            <p className="hint" style={{ marginBottom: 0 }}>
              {isDefault
                ? 'No override set — the default is allow.'
                : `Override: ${nonClaudeValue}. allow passes requests through; warn annotates the route trace; block rejects with a clear error.`}
            </p>
            {saveSwitch.error !== null && <ErrorNote message={saveSwitch.error} />}
          </div>
        </div>

        <div className="widget w-full" style={{ marginBottom: 8 }}>
          <div className="widget-head">
            <h3 className="title">Claude Code alias map</h3>
          </div>
          <div className="widget-body">
            <form className="form" onSubmit={submitAliases}>
              <div className="field">
                <label htmlFor="s-aliases">Aliases (JSON object)</label>
                <textarea
                  id="s-aliases"
                  className="textarea mono"
                  rows={4}
                  value={aliasesText}
                  onChange={(e) => setAliasesText(e.target.value)}
                  placeholder={'{"haiku": "gpt-5-mini", "sonnet": "llama-4-sonnet"}'}
                  spellCheck={false}
                />
              </div>
              {aliasesError !== null && (
                <p role="alert" className="form-note">
                  {aliasesError}
                </p>
              )}
              {saveAlias.error !== null && (
                <p role="alert" className="form-note">
                  {saveAlias.error}
                </p>
              )}
              <button type="submit" className="btn btn-primary btn-sm" disabled={saveAlias.pending}>
                Save alias map
              </button>
            </form>
          </div>
        </div>
        {error !== null ? (
          <div className="w-full">
            <ErrorNote message={error} />
          </div>
        ) : loading ? (
          <div className="w-full">
            <EmptyState title="Loading models" body="Fetching the model list." />
          </div>
        ) : models.length === 0 ? (
          <div className="w-full">
            <EmptyState
              title="No models registered"
              body="Register a model (or run the model sync) so clients can request it — models without a matching routing rule route implicitly to their provider. Every model carries its capability flags and compatibility policy here — including thinking handling for non-Claude targets."
              action={
                providers.length === 0 ? undefined : (
                  <button type="button" className="btn btn-primary btn-sm" onClick={openCreate}>
                    <IconPlus width="14" height="14" />
                    Register the first model
                  </button>
                )
              }
            />
          </div>
        ) : (
          <div className="w-full">
            {/* Provider-grouped collapsible sections (2026-09-22): each
                provider is one <details> (native, no JS state — the
                v1.0.x screenshot lesson favors structural primitives),
                its models render as cards in a grid. Block layout only. */}
            {groups.map((g) => (
              <details key={g.provider.id} className="fieldset" style={{ marginBottom: 8 }} open={groups.length === 1}>
                <summary style={{ cursor: 'pointer', fontWeight: 600, padding: '4px 0' }}>
                  {g.provider.name} ({g.models.length} {g.models.length === 1 ? 'model' : 'models'})
                </summary>
                <div style={{ display: 'grid', gap: 10, gridTemplateColumns: 'repeat(auto-fill, minmax(280px, 1fr))', marginTop: 8 }}>
                  {g.models.length === 0 && (
                    <p className="hint" style={{ margin: 0 }}>
                      No models yet — run a sync or add one.
                    </p>
                  )}
                  {g.models.map((m) => {
                    const caps = capsFromRaw(m.capabilities);
                    const result = testResults[m.id];
                    return (
                      <div key={m.id} className="fieldset" style={{ display: 'flex', flexDirection: 'column', gap: 6, margin: 0, padding: '10px 12px' }}>
                        <div>
                          <span className="primary">{m.name}</span>
                        </div>
                        <div>
                          <span className="mono" style={{ fontSize: 'var(--fs-sm)' }}>{m.id}</span>
                        </div>
                        <div>
                          <span className={m.origin === 'synced' ? 'badge info' : 'badge paused'}>
                            <span className="b-dot" />
                            {m.origin === 'synced' ? 'synced' : 'manual'}
                          </span>
                        </div>
                        <div style={{ display: 'flex', flexDirection: 'column', gap: 2 }}>
                          {CAPABILITY_FLAGS.map((flag) => (
                            <span key={flag} className="hint" style={{ margin: 0, display: 'flex', alignItems: 'center', gap: 4 }}>
                              {caps[flag] === true ? (
                                <svg width="10" height="10" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="3.5" aria-hidden="true">
                                  <path d="m5 13 4 4 10-10" />
                                </svg>
                              ) : (
                                <svg width="10" height="10" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.5" aria-hidden="true" style={{ opacity: 0.4 }}>
                                  <path d="M6 12h12" />
                                </svg>
                              )}
                              {flag}
                            </span>
                          ))}
                        </div>
                        {/* Test result badge: status-only (kit .form-note
                            carries the color; no wall-clock numbers in
                            screenshots). Block layout. */}
                        {result !== undefined && (
                          <p
                            role="status"
                            className={`form-note${result.ok ? '' : ''}`}
                            style={{
                              margin: 0,
                              color: result.ok ? 'var(--status-up)' : undefined,
                              overflowWrap: 'anywhere',
                            }}
                          >
                            {result.ok
                              ? `OK${result.reply !== undefined && result.reply !== '' ? ` — ${result.reply}` : ''}`
                              : result.error ?? 'Test failed'}
                          </p>
                        )}
                        <div style={{ display: 'flex', gap: 6, marginTop: 'auto' }}>
                          <button
                            type="button"
                            className="btn btn-secondary btn-sm"
                            onClick={() => void runTest(m)}
                            disabled={testing !== null}
                            title="One minimal 'Sag OK' round trip against this model's provider"
                          >
                            {testing === m.id ? 'Testing…' : 'Test'}
                          </button>
                          <button type="button" className="btn btn-primary btn-sm" onClick={() => openEdit(m)}>
                            Edit
                          </button>
                          <button
                            type="button"
                            className="btn btn-danger btn-sm"
                            onClick={() => void remove(m)}
                            disabled={drawerPending}
                          >
                            Delete
                          </button>
                        </div>
                      </div>
                    );
                  })}
                </div>
              </details>
            ))}
          </div>
        )}
      </div>

      {(editing !== null || creating) && (
        <Drawer
          title={creating ? 'Add model' : `Edit ${editing?.name}`}
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
              <button type="submit" className="btn btn-primary" form="model-form" disabled={drawerPending}>
                Save
              </button>
            </>
          }
        >
          <form className="form" id="model-form" onSubmit={submit}>
            {drawerError !== null && (
              <p role="alert" className="form-note">
                {drawerError}
              </p>
            )}
            <div className="form-row">
              <div className="field">
                <label htmlFor="m-id">ID</label>
                <input
                  id="m-id"
                  className="input mono"
                  type="text"
                  value={form.id}
                  onChange={(e) => setForm((f) => ({ ...f, id: e.target.value }))}
                  disabled={!creating}
                  required
                  placeholder="e.g. claude-sonnet-5"
                />
              </div>
              <div className="field has-select">
                <label htmlFor="m-provider">Provider</label>
                <select
                  id="m-provider"
                  value={form.provider_id}
                  onChange={(e) => setForm((f) => ({ ...f, provider_id: e.target.value }))}
                  required
                >
                  {providers.map((p) => (
                    <option key={p.id} value={p.id}>
                      {p.name}
                    </option>
                  ))}
                </select>
              </div>
            </div>
            <div className="form-row">
              <div className="field">
                <label htmlFor="m-name">Name</label>
                <input
                  id="m-name"
                  className="input"
                  type="text"
                  value={form.name}
                  onChange={(e) => setForm((f) => ({ ...f, name: e.target.value }))}
                  required
                  placeholder="Model name clients see"
                />
              </div>
              <div className="field">
                <label htmlFor="m-priority">Priority</label>
                <input
                  id="m-priority"
                  className="input num"
                  type="number"
                  min={0}
                  step={1}
                  value={form.priority}
                  onChange={(e) => setForm((f) => ({ ...f, priority: e.target.value }))}
                />
                <span className="hint">Higher wins when several rules match.</span>
              </div>
            </div>

            <fieldset className="fieldset">
              <legend>Capabilities</legend>
              <span className="hint" style={{ display: 'block' }}>
                Manual selection pins each flag — the automatic learning (a real upstream
                rejection writes the observed gap) never overwrites a pinned flag.
              </span>
              {CAPABILITY_FLAGS.map((flag) => (
                <label key={flag} className="check" style={{ display: 'flex', marginBottom: 8 }}>
                  <input
                    type="checkbox"
                    checked={form.capabilities[flag] === true}
                    onChange={(e) =>
                      setForm((f) => ({ ...f, capabilities: { ...f.capabilities, [flag]: e.target.checked } }))
                    }
                  />
                  <span className="cbox">
                    <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="3.5" aria-hidden="true">
                      <path d="m5 13 4 4 10-10" />
                    </svg>
                  </span>
                  {flag}
                </label>
              ))}
            </fieldset>

          </form>
        </Drawer>
      )}
    </>
  );
}